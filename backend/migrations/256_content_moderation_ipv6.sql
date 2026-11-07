-- Audit keys use the existing monotonic allocator and draining lifecycle, with
-- ownership independent of account IDs. Never store key credentials here.
SET LOCAL lock_timeout = '5s';
ALTER TABLE yoshub_ovh_ipv6_leases ADD COLUMN owner_moderation_key TEXT;
ALTER TABLE yoshub_ovh_ipv6_leases ADD CONSTRAINT ovh_ipv6_owner_exclusive
    CHECK (owner_account_id IS NULL OR owner_moderation_key IS NULL);
ALTER TABLE yoshub_ovh_ipv6_leases ADD CONSTRAINT ovh_ipv6_moderation_key_hash
    CHECK (owner_moderation_key IS NULL OR owner_moderation_key ~ '^[0-9a-f]{64}$');
CREATE UNIQUE INDEX idx_ovh_ipv6_moderation_owner ON yoshub_ovh_ipv6_leases(owner_moderation_key)
    WHERE state='bound' AND owner_moderation_key IS NOT NULL;

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
    IF lease.owner_moderation_key IS NOT NULL OR lease.state NOT IN ('reserved', 'bound') OR
        (lease.owner_account_id IS NOT NULL AND lease.owner_account_id <> owner) OR
        NOT EXISTS(SELECT 1 FROM proxies WHERE id=NEW.proxy_id AND deleted_at IS NULL AND status='active') THEN
        RAISE EXCEPTION 'OVH IPv6 belongs to another account or audit key, or is permanently retired';
    END IF;
    IF NEW.parent_account_id IS NOT NULL AND lease.owner_account_id IS NULL THEN
        RAISE EXCEPTION 'OVH IPv6 must first be assigned to the parent account';
    END IF;
    UPDATE yoshub_ovh_ipv6_leases SET owner_account_id=owner,state='bound' WHERE proxy_id=NEW.proxy_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION yoshub_ovh_ipv6_proxy_retire() RETURNS TRIGGER AS $$
DECLARE lease yoshub_ovh_ipv6_leases%ROWTYPE;
BEGIN
    IF TG_OP='UPDATE' AND (NEW.deleted_at IS NULL OR OLD.deleted_at IS NOT NULL) THEN RETURN NEW; END IF;
    SELECT * INTO lease FROM yoshub_ovh_ipv6_leases WHERE proxy_id=OLD.id FOR UPDATE;
    IF FOUND THEN
        IF lease.state='bound' AND (lease.owner_moderation_key IS NOT NULL OR
            EXISTS(SELECT 1 FROM accounts WHERE proxy_id=OLD.id AND deleted_at IS NULL)) THEN
            RAISE EXCEPTION 'OVH IPv6 is still bound; delete or change its owner first';
        END IF;
        UPDATE yoshub_ovh_ipv6_leases SET state='retiring',confirmed=FALSE,next_attempt_at=NOW()
            WHERE proxy_id=OLD.id AND state<>'retiring';
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
