-- 008_create_node_join_tokens.up.sql
--
-- One-time tokens that enrol an Agent as a Node (CARA-138, ADR-0031).
--
-- Design notes:
--   - Only a SHA-256 hash is stored, as for api_tokens.
--   - used_at is set in the same transaction that issues the Node token, so a
--     join token can be redeemed once.
--   - Rows are kept after use, expiry or revocation, as a record of who
--     enrolled which Node.

CREATE TABLE IF NOT EXISTS node_join_tokens (
    id          BIGSERIAL   PRIMARY KEY,
    node_name   TEXT        NOT NULL,
    token_hash  TEXT        NOT NULL UNIQUE,
    created_by  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS node_join_tokens_node_name ON node_join_tokens (node_name);
