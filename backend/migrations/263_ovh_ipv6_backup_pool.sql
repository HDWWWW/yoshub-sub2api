-- The two host agents have independent counters, credentials and private ports.
CREATE TABLE IF NOT EXISTS yoshub_ovh_ipv6_backup_settings (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO yoshub_ovh_ipv6_backup_settings(id) VALUES (1) ON CONFLICT DO NOTHING;

-- Rule 0 is the administrator's manual picker. Each refill group has its own turn.
CREATE TABLE IF NOT EXISTS yoshub_ovh_ipv6_pool_rotation (
    rule_id BIGINT PRIMARY KEY CHECK (rule_id >= 0),
    next_pool TEXT NOT NULL CHECK (next_pool IN ('primary', 'backup'))
);

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
-- An in-progress lease write takes precedence over starting the new slot.
LOCK TABLE yoshub_ovh_ipv6_leases IN SHARE ROW EXCLUSIVE MODE NOWAIT;
CREATE OR REPLACE TRIGGER record_ipv6_retirement AFTER DELETE ON yoshub_ovh_ipv6_leases
    FOR EACH ROW EXECUTE FUNCTION yoshub_record_ipv6_retirement();
