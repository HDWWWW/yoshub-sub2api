-- Nullable for historic rows and rolling deployments. Lists omit the payload;
-- administrators fetch one record on demand. Policy evidence is not truncated.
ALTER TABLE content_moderation_logs
    ADD COLUMN IF NOT EXISTS input_evidence JSONB;

-- Budget counts UTF-8 serialized log content, including all evidence, not
-- PostgreSQL indexes, WAL or free pages. Enforced atomically across instances.
ALTER TABLE content_moderation_logs
    ADD COLUMN IF NOT EXISTS storage_bytes BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS content_moderation_storage (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    used_bytes BIGINT NOT NULL CHECK (used_bytes >= 0),
    max_bytes BIGINT NOT NULL CHECK (max_bytes > 0 AND max_bytes <= 200000000000)
);

UPDATE content_moderation_logs l
SET storage_bytes = octet_length(convert_to((to_jsonb(l) - 'storage_bytes')::text, 'UTF8'))
WHERE storage_bytes = 0;

INSERT INTO content_moderation_storage (id, used_bytes, max_bytes)
SELECT 1, COALESCE(SUM(storage_bytes), 0), 200000000000 FROM content_moderation_logs
ON CONFLICT (id) DO UPDATE SET used_bytes = EXCLUDED.used_bytes;

CREATE OR REPLACE FUNCTION content_moderation_storage_lock() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(257, 1);
    RETURN NULL;
END;
$$;

-- Bounded, primary-key-ordered batches; never scan all payloads while writing.
CREATE OR REPLACE FUNCTION content_moderation_storage_trim() RETURNS void LANGUAGE plpgsql AS $$
DECLARE excess BIGINT; removed BIGINT;
BEGIN
    LOOP
        SELECT used_bytes - max_bytes INTO excess FROM content_moderation_storage WHERE id = 1;
        EXIT WHEN excess <= 0;
        WITH oldest AS (
            SELECT id, storage_bytes FROM content_moderation_logs ORDER BY id LIMIT 256
        ), candidates AS (
            SELECT id, COALESCE(SUM(storage_bytes) OVER (
                ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING
            ), 0) AS freed_before FROM oldest
        )
        DELETE FROM content_moderation_logs WHERE id IN (
            SELECT id FROM candidates WHERE freed_before < excess
        );
        GET DIAGNOSTICS removed = ROW_COUNT;
        IF removed = 0 THEN RAISE EXCEPTION 'moderation storage accounting inconsistent'; END IF;
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION content_moderation_storage_size() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.storage_bytes := octet_length(convert_to((to_jsonb(NEW) - 'storage_bytes')::text, 'UTF8'));
    IF NEW.storage_bytes > (SELECT max_bytes FROM content_moderation_storage WHERE id = 1) THEN
        RAISE EXCEPTION 'single moderation log exceeds configured storage capacity';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION content_moderation_storage_account() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE added BIGINT := 0; removed BIGINT := 0;
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        UPDATE content_moderation_storage SET used_bytes = 0 WHERE id = 1;
        RETURN NULL;
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        SELECT COALESCE(SUM(storage_bytes), 0) INTO added FROM moderation_new_rows;
    END IF;
    IF TG_OP IN ('DELETE', 'UPDATE') THEN
        SELECT COALESCE(SUM(storage_bytes), 0) INTO removed FROM moderation_old_rows;
    END IF;
    UPDATE content_moderation_storage SET used_bytes = used_bytes + added - removed WHERE id = 1;
    IF TG_OP <> 'DELETE' THEN PERFORM content_moderation_storage_trim(); END IF;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE TRIGGER moderation_storage_lock
    BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON content_moderation_logs
    FOR EACH STATEMENT EXECUTE FUNCTION content_moderation_storage_lock();
CREATE OR REPLACE TRIGGER moderation_storage_size
    BEFORE INSERT OR UPDATE ON content_moderation_logs
    FOR EACH ROW EXECUTE FUNCTION content_moderation_storage_size();
CREATE OR REPLACE TRIGGER moderation_storage_insert
    AFTER INSERT ON content_moderation_logs REFERENCING NEW TABLE AS moderation_new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION content_moderation_storage_account();
CREATE OR REPLACE TRIGGER moderation_storage_update
    AFTER UPDATE ON content_moderation_logs REFERENCING OLD TABLE AS moderation_old_rows NEW TABLE AS moderation_new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION content_moderation_storage_account();
CREATE OR REPLACE TRIGGER moderation_storage_delete
    AFTER DELETE ON content_moderation_logs REFERENCING OLD TABLE AS moderation_old_rows
    FOR EACH STATEMENT EXECUTE FUNCTION content_moderation_storage_account();
CREATE OR REPLACE TRIGGER moderation_storage_truncate
    AFTER TRUNCATE ON content_moderation_logs
    FOR EACH STATEMENT EXECUTE FUNCTION content_moderation_storage_account();

-- A retention-setting update and any resulting eviction commit together.
CREATE OR REPLACE FUNCTION content_moderation_storage_config() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE limit_gb BIGINT;
BEGIN
    IF NEW.key <> 'content_moderation_config' THEN RETURN NEW; END IF;
    BEGIN
        limit_gb := (NEW.value::jsonb ->> 'log_storage_limit_gb')::bigint;
    EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range THEN
        limit_gb := 200;
    END;
    IF limit_gb IS NULL OR limit_gb < 1 OR limit_gb > 200 THEN limit_gb := 200; END IF;
    PERFORM pg_advisory_xact_lock(257, 1);
    UPDATE content_moderation_storage SET max_bytes = limit_gb * 1000000000 WHERE id = 1;
    PERFORM content_moderation_storage_trim();
    RETURN NEW;
END;
$$;
CREATE OR REPLACE TRIGGER moderation_storage_config
    AFTER INSERT OR UPDATE OF value ON settings
    FOR EACH ROW EXECUTE FUNCTION content_moderation_storage_config();

-- Apply an existing saved limit and account for pre-upgrade records immediately.
UPDATE settings SET value = value WHERE key = 'content_moderation_config';
SELECT content_moderation_storage_trim();
