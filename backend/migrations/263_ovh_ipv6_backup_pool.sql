-- The two host agents have independent counters, credentials and private ports.
SET LOCAL lock_timeout = '5s';
ALTER TABLE yoshub_ovh_ipv6_settings ADD COLUMN IF NOT EXISTS backup_enabled BOOLEAN NOT NULL DEFAULT FALSE;

-- A proxy username is local to a listening endpoint. Build the replacement
-- index before dropping the original so uniqueness holds throughout migration.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ovh_ipv6_proxy_endpoint_identity
    ON proxies (host, port, username) WHERE username LIKE 'ovh6\_%' ESCAPE '\';
DROP INDEX IF EXISTS idx_ovh_ipv6_proxy_identity;

CREATE TABLE IF NOT EXISTS yoshub_ovh_ipv6_retirements (
    id BIGSERIAL PRIMARY KEY,
    address INET NOT NULL UNIQUE,
    pool TEXT NOT NULL CHECK (pool IN ('primary','backup')),
    retired_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE OR REPLACE FUNCTION yoshub_record_ipv6_retirement() RETURNS TRIGGER AS $retirement$
BEGIN
    IF OLD.state = 'retiring' THEN
        INSERT INTO yoshub_ovh_ipv6_retirements(address, pool)
        VALUES(OLD.address, CASE WHEN OLD.lease_id LIKE 'backup:%' THEN 'backup' ELSE 'primary' END)
        ON CONFLICT(address) DO NOTHING;
    END IF;
    RETURN OLD;
END;
$retirement$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS record_ipv6_retirement ON yoshub_ovh_ipv6_leases;
CREATE TRIGGER record_ipv6_retirement AFTER DELETE ON yoshub_ovh_ipv6_leases
    FOR EACH ROW EXECUTE FUNCTION yoshub_record_ipv6_retirement();

ALTER TABLE yoshub_ovh_ipv6_settings ADD COLUMN IF NOT EXISTS manual_next_pool TEXT NOT NULL DEFAULT 'primary'
    CHECK (manual_next_pool IN ('primary','backup'));
