-- One compact row per live account. Retention of request logs must not reset
-- lifetime totals. Account deletion removes the row, including concurrent late
-- usage writes. No copy of request bodies, credentials or per-request history.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS account_lifetime_stats (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    requests BIGINT NOT NULL DEFAULT 0,
    tokens BIGINT NOT NULL DEFAULT 0,
    cost NUMERIC(30,12) NOT NULL DEFAULT 0,
    standard_cost NUMERIC(30,12) NOT NULL DEFAULT 0,
    user_cost NUMERIC(30,12) NOT NULL DEFAULT 0
);

-- Serialize the one-time baseline with usage INSERT/UPDATE and account deletion.
-- Only live accounts are backfilled, using the existing account_id index.
LOCK TABLE accounts, usage_logs IN SHARE ROW EXCLUSIVE MODE;
INSERT INTO account_lifetime_stats (account_id, requests, tokens, cost, standard_cost, user_cost)
SELECT a.id, totals.*
FROM accounts a
CROSS JOIN LATERAL (
    SELECT COUNT(*),
           COALESCE(SUM(input_tokens::bigint + output_tokens + cache_creation_tokens + cache_read_tokens), 0),
           COALESCE(SUM(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)), 0),
           COALESCE(SUM(total_cost), 0), COALESCE(SUM(actual_cost), 0)
    FROM usage_logs WHERE account_id = a.id
) totals
WHERE a.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM account_lifetime_stats s WHERE s.account_id = a.id)
ON CONFLICT (account_id) DO NOTHING;

CREATE OR REPLACE FUNCTION accumulate_account_lifetime_stats_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE account_key BIGINT;
BEGIN
    -- SHARE conflicts with soft deletion; KEY SHARE alone would allow a late
    -- writer to recreate totals after deleted_at was set.
    FOR account_key IN
        SELECT a.id FROM accounts a
        JOIN (SELECT DISTINCT account_id FROM lifetime_inserted) n ON n.account_id = a.id
        WHERE a.deleted_at IS NULL ORDER BY a.id FOR SHARE OF a
    LOOP
        INSERT INTO account_lifetime_stats (account_id, requests, tokens, cost, standard_cost, user_cost)
        SELECT account_key, COUNT(*),
               COALESCE(SUM(input_tokens::bigint + output_tokens + cache_creation_tokens + cache_read_tokens), 0),
               COALESCE(SUM(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)), 0),
               COALESCE(SUM(total_cost), 0), COALESCE(SUM(actual_cost), 0)
        FROM lifetime_inserted WHERE account_id = account_key
        ON CONFLICT (account_id) DO UPDATE SET
            requests = account_lifetime_stats.requests + EXCLUDED.requests,
            tokens = account_lifetime_stats.tokens + EXCLUDED.tokens,
            cost = account_lifetime_stats.cost + EXCLUDED.cost,
            standard_cost = account_lifetime_stats.standard_cost + EXCLUDED.standard_cost,
            user_cost = account_lifetime_stats.user_cost + EXCLUDED.user_cost;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION accumulate_account_lifetime_stats_update()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE account_key BIGINT;
BEGIN
    FOR account_key IN
        SELECT a.id FROM accounts a
        JOIN (SELECT account_id FROM lifetime_old UNION SELECT account_id FROM lifetime_new) n ON n.account_id = a.id
        WHERE a.deleted_at IS NULL ORDER BY a.id FOR SHARE OF a
    LOOP
        INSERT INTO account_lifetime_stats (account_id, requests, tokens, cost, standard_cost, user_cost)
        SELECT account_key, SUM(delta), SUM(tokens), SUM(cost), SUM(standard_cost), SUM(user_cost)
        FROM (
            SELECT -1 AS delta, -(input_tokens::bigint + output_tokens + cache_creation_tokens + cache_read_tokens) AS tokens,
                   -COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1) AS cost,
                   -total_cost AS standard_cost, -actual_cost AS user_cost
            FROM lifetime_old WHERE account_id = account_key
            UNION ALL
            SELECT 1, input_tokens::bigint + output_tokens + cache_creation_tokens + cache_read_tokens,
                   COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1), total_cost, actual_cost
            FROM lifetime_new WHERE account_id = account_key
        ) changes
        ON CONFLICT (account_id) DO UPDATE SET
            requests = account_lifetime_stats.requests + EXCLUDED.requests,
            tokens = account_lifetime_stats.tokens + EXCLUDED.tokens,
            cost = account_lifetime_stats.cost + EXCLUDED.cost,
            standard_cost = account_lifetime_stats.standard_cost + EXCLUDED.standard_cost,
            user_cost = account_lifetime_stats.user_cost + EXCLUDED.user_cost;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION remove_deleted_account_lifetime_stats()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM account_lifetime_stats WHERE account_id = NEW.id;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS usage_logs_lifetime_insert ON usage_logs;
CREATE TRIGGER usage_logs_lifetime_insert AFTER INSERT ON usage_logs
REFERENCING NEW TABLE AS lifetime_inserted FOR EACH STATEMENT
EXECUTE FUNCTION accumulate_account_lifetime_stats_insert();

DROP TRIGGER IF EXISTS usage_logs_lifetime_update ON usage_logs;
CREATE TRIGGER usage_logs_lifetime_update AFTER UPDATE ON usage_logs
REFERENCING OLD TABLE AS lifetime_old NEW TABLE AS lifetime_new FOR EACH STATEMENT
EXECUTE FUNCTION accumulate_account_lifetime_stats_update();

DROP TRIGGER IF EXISTS accounts_lifetime_cleanup ON accounts;
CREATE TRIGGER accounts_lifetime_cleanup AFTER UPDATE OF deleted_at ON accounts
FOR EACH ROW WHEN (NEW.deleted_at IS NOT NULL AND OLD.deleted_at IS NULL)
EXECUTE FUNCTION remove_deleted_account_lifetime_stats();
