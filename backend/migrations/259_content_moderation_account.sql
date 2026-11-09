-- Preserve the responding pool account even if it is renamed or deleted later.
ALTER TABLE content_moderation_logs
    ADD COLUMN IF NOT EXISTS account_id BIGINT,
    ADD COLUMN IF NOT EXISTS account_name TEXT NOT NULL DEFAULT '';

-- Historical policy records can recover the account through their exact request.
-- Both lookups use existing request_id indexes. Never infer it from the gateway user.
WITH recovered AS (
    SELECT l.id, COALESCE(o.account_id, u.account_id) AS account_id
    FROM content_moderation_logs l
    LEFT JOIN LATERAL (
        SELECT e.account_id FROM ops_error_logs e
        WHERE e.request_id = l.request_id AND e.api_key_id = l.api_key_id
          AND e.error_type = l.action AND e.account_id IS NOT NULL
        ORDER BY e.id DESC LIMIT 1
    ) o ON TRUE
    LEFT JOIN LATERAL (
        SELECT x.account_id FROM usage_logs x
        WHERE x.request_id = l.request_id AND x.api_key_id = l.api_key_id
          AND x.request_type = 4 AND x.account_id IS NOT NULL
        ORDER BY x.id DESC LIMIT 1
    ) u ON o.account_id IS NULL
    WHERE l.account_id IS NULL AND l.request_id <> ''
      AND l.action IN ('cyber_policy', 'bio_policy')
)
UPDATE content_moderation_logs l
SET account_id = r.account_id, account_name = COALESCE(a.name, '')
FROM recovered r LEFT JOIN accounts a ON a.id = r.account_id
WHERE l.id = r.id AND r.account_id IS NOT NULL;
