-- 003_create_preauth_keys.up.sql
--
-- Server-side mapping between a Headscale pre-auth key and the Cara Node the
-- key was issued for (CARA-68, design CARA-50 §3–§4).
--
-- Design notes:
--   - This is server-managed join-key state, not a user-facing resource kind,
--     so it lives in its own table rather than the shared "resources" table.
--   - key_hash is the hex SHA-256 of the full pre-auth key and is the primary
--     lookup key. The full key is never stored; key_prefix keeps only the
--     first few characters for operator-facing audit/correlation.
--   - state moves active -> used on first successful heartbeat; expired/revoked
--     are reserved for later lifecycle work.
--   - used_by_ip / used_at record which overlay identity consumed the key.

CREATE TABLE IF NOT EXISTS preauth_keys (
    key_hash       TEXT        PRIMARY KEY,
    key_prefix     TEXT        NOT NULL DEFAULT '',
    cara_node_name TEXT        NOT NULL,
    expiration     TIMESTAMPTZ,
    state          TEXT        NOT NULL DEFAULT 'active',
    used_by_ip     TEXT        NOT NULL DEFAULT '',
    used_at        TIMESTAMPTZ,
    issued_by      TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Look up all keys issued for a given Cara Node (audit, future GC sweeps).
CREATE INDEX IF NOT EXISTS idx_preauth_keys_node
    ON preauth_keys (cara_node_name);
