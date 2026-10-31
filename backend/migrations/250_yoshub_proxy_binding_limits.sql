-- Extension-only settings. Native proxy/account import contracts are unchanged.
CREATE TABLE IF NOT EXISTS yoshub_proxy_binding_limits (
    proxy_id BIGINT PRIMARY KEY REFERENCES proxies(id) ON DELETE CASCADE,
    max_accounts INTEGER NOT NULL DEFAULT 1 CHECK (max_accounts BETWEEN 1 AND 100000),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
