-- Only live bindings and unfinished cleanup are retained. The monotonic
-- address counter lives independently on the egress host, outside DB restores.
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS yoshub_ovh_ipv6_settings (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO yoshub_ovh_ipv6_settings(id) VALUES (1) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS yoshub_ovh_ipv6_leases (
    proxy_id BIGINT PRIMARY KEY,
    lease_id TEXT NOT NULL UNIQUE,
    address INET NOT NULL UNIQUE,
    owner_account_id BIGINT,
    state TEXT NOT NULL DEFAULT 'bound' CHECK (state IN ('bound', 'retiring')),
    confirmed BOOLEAN NOT NULL DEFAULT FALSE,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ovh_ipv6_pending ON yoshub_ovh_ipv6_leases(next_attempt_at)
    WHERE NOT confirmed OR state = 'retiring';
CREATE UNIQUE INDEX IF NOT EXISTS idx_ovh_ipv6_proxy_identity ON proxies(username)
    WHERE username LIKE 'ovh6\_%' ESCAPE '\';

-- Covers normal, batch, scoped-key and import paths, including old application
-- slots during a blue/green rollout. A credential shadow belongs to its parent.
CREATE OR REPLACE FUNCTION yoshub_ovh_ipv6_claim() RETURNS TRIGGER AS $$
DECLARE lease yoshub_ovh_ipv6_leases%ROWTYPE; owner BIGINT;
BEGIN
    IF NEW.proxy_id IS NULL OR NEW.deleted_at IS NOT NULL THEN RETURN NEW; END IF;
    IF TG_OP = 'UPDATE' AND NEW.proxy_id IS NOT DISTINCT FROM OLD.proxy_id
        AND NEW.parent_account_id IS NOT DISTINCT FROM OLD.parent_account_id
        AND OLD.deleted_at IS NULL THEN RETURN NEW; END IF;
    SELECT * INTO lease FROM yoshub_ovh_ipv6_leases WHERE proxy_id=NEW.proxy_id FOR UPDATE;
    IF NOT FOUND THEN RETURN NEW; END IF;
    owner := COALESCE(NEW.parent_account_id, NEW.id);
    IF lease.state <> 'bound' OR (lease.owner_account_id IS NOT NULL AND lease.owner_account_id <> owner) THEN
        RAISE EXCEPTION 'OVH IPv6 belongs to another account or is permanently retired';
    END IF;
    IF NEW.parent_account_id IS NOT NULL AND lease.owner_account_id IS NULL THEN
        RAISE EXCEPTION 'OVH IPv6 must first be assigned to the parent account';
    END IF;
    UPDATE yoshub_ovh_ipv6_leases SET owner_account_id=owner WHERE proxy_id=NEW.proxy_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS yoshub_ovh_ipv6_claim ON accounts;
CREATE TRIGGER yoshub_ovh_ipv6_claim BEFORE INSERT OR UPDATE ON accounts
    FOR EACH ROW EXECUTE FUNCTION yoshub_ovh_ipv6_claim();

CREATE OR REPLACE FUNCTION yoshub_ovh_ipv6_retire() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.proxy_id IS NULL OR OLD.parent_account_id IS NOT NULL THEN RETURN NULL; END IF;
    IF TG_OP='UPDATE' AND NEW.proxy_id IS NOT DISTINCT FROM OLD.proxy_id AND NEW.deleted_at IS NULL THEN RETURN NULL; END IF;
    UPDATE yoshub_ovh_ipv6_leases SET state='retiring', confirmed=FALSE, next_attempt_at=NOW()
        WHERE proxy_id=OLD.proxy_id AND owner_account_id=OLD.id AND state='bound';
    IF FOUND THEN
        IF TG_OP='UPDATE' AND NEW.deleted_at IS NULL THEN
            UPDATE accounts SET proxy_id=NEW.proxy_id,updated_at=NOW()
                WHERE parent_account_id=OLD.id AND proxy_id=OLD.proxy_id AND deleted_at IS NULL;
        END IF;
        -- Hide immediately. Keep the row only while established streams drain.
        UPDATE proxies SET status='inactive', deleted_at=NOW(), updated_at=NOW() WHERE id=OLD.proxy_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS yoshub_ovh_ipv6_retire ON accounts;
CREATE TRIGGER yoshub_ovh_ipv6_retire AFTER UPDATE OR DELETE ON accounts
    FOR EACH ROW EXECUTE FUNCTION yoshub_ovh_ipv6_retire();

CREATE OR REPLACE FUNCTION yoshub_ovh_ipv6_proxy_guard() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.backup_proxy_id IS NOT NULL AND EXISTS(
        SELECT 1 FROM yoshub_ovh_ipv6_leases WHERE proxy_id=NEW.backup_proxy_id
    ) THEN
        RAISE EXCEPTION 'Managed OVH IPv6 cannot be shared as a backup proxy';
    END IF;
    IF TG_OP = 'INSERT' THEN RETURN NEW; END IF;
    IF EXISTS(SELECT 1 FROM yoshub_ovh_ipv6_leases WHERE proxy_id=OLD.id) AND
        (NEW.protocol,NEW.host,NEW.port,NEW.username,NEW.password,NEW.fallback_mode,NEW.backup_proxy_id,NEW.expires_at)
        IS DISTINCT FROM (OLD.protocol,OLD.host,OLD.port,OLD.username,OLD.password,OLD.fallback_mode,OLD.backup_proxy_id,OLD.expires_at) THEN
        RAISE EXCEPTION 'Managed OVH IPv6 routing cannot be edited; change the account proxy instead';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS yoshub_ovh_ipv6_proxy_guard ON proxies;
CREATE TRIGGER yoshub_ovh_ipv6_proxy_guard BEFORE INSERT OR UPDATE ON proxies
    FOR EACH ROW EXECUTE FUNCTION yoshub_ovh_ipv6_proxy_guard();
