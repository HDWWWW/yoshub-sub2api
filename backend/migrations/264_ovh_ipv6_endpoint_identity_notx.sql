-- Agents issue usernames independently. Keep the legacy unique index until
-- the endpoint-scoped index has finished building while the old slot serves.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_ovh_ipv6_proxy_endpoint_identity
    ON proxies (host, port, username) WHERE username LIKE 'ovh6\_%' ESCAPE '\';

DROP INDEX CONCURRENTLY IF EXISTS idx_ovh_ipv6_proxy_identity;
