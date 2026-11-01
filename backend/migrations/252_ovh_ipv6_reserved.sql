-- A reserved lease is a deliberately created, never-bound pool entry. Once
-- claimed it can only become retiring; it must never return to reserved.
SET LOCAL lock_timeout = '5s';
ALTER TABLE yoshub_ovh_ipv6_leases DROP CONSTRAINT yoshub_ovh_ipv6_leases_state_check;
ALTER TABLE yoshub_ovh_ipv6_leases ADD CONSTRAINT yoshub_ovh_ipv6_leases_state_check
    CHECK (state IN ('reserved', 'bound', 'retiring'));

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
    IF lease.state NOT IN ('reserved', 'bound') OR
        (lease.owner_account_id IS NOT NULL AND lease.owner_account_id <> owner) OR
        NOT EXISTS(SELECT 1 FROM proxies WHERE id=NEW.proxy_id AND deleted_at IS NULL AND status='active') THEN
        RAISE EXCEPTION 'OVH IPv6 belongs to another account or is permanently retired';
    END IF;
    IF NEW.parent_account_id IS NOT NULL AND lease.owner_account_id IS NULL THEN
        RAISE EXCEPTION 'OVH IPv6 must first be assigned to the parent account';
    END IF;
    UPDATE yoshub_ovh_ipv6_leases SET owner_account_id=owner,state='bound' WHERE proxy_id=NEW.proxy_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Account-triggered retirement already changes the lease to retiring before
-- hiding its proxy. Direct deletion of an unused reservation follows the same
-- durable cleanup path, including old slots and hard DELETE maintenance paths.
CREATE OR REPLACE FUNCTION yoshub_ovh_ipv6_proxy_retire() RETURNS TRIGGER AS $$
DECLARE lease yoshub_ovh_ipv6_leases%ROWTYPE;
BEGIN
    IF TG_OP='UPDATE' AND (NEW.deleted_at IS NULL OR OLD.deleted_at IS NOT NULL) THEN RETURN NEW; END IF;
    SELECT * INTO lease FROM yoshub_ovh_ipv6_leases WHERE proxy_id=OLD.id FOR UPDATE;
    IF FOUND THEN
        IF lease.state='bound' AND EXISTS(SELECT 1 FROM accounts WHERE proxy_id=OLD.id AND deleted_at IS NULL) THEN
            RAISE EXCEPTION 'OVH IPv6 is still bound; delete or change the account first';
        END IF;
        UPDATE yoshub_ovh_ipv6_leases SET state='retiring',confirmed=FALSE,next_attempt_at=NOW()
            WHERE proxy_id=OLD.id AND state<>'retiring';
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER yoshub_ovh_ipv6_proxy_retire BEFORE UPDATE OF deleted_at OR DELETE ON proxies
    FOR EACH ROW EXECUTE FUNCTION yoshub_ovh_ipv6_proxy_retire();
