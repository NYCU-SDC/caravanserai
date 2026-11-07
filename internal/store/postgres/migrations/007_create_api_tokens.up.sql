-- 007_create_api_tokens.up.sql
--
-- Bearer tokens that identify API callers (CARA-137, ADR-0031).
--
-- Design notes:
--   - Only a SHA-256 hash of each token is stored. Tokens are 32 random bytes,
--     so an unsalted hash is enough to make a leaked row useless.
--   - Revoking sets revoked_at instead of deleting, so the list shows who held
--     a token and when it stopped working.

CREATE TABLE IF NOT EXISTS api_tokens (
    id          BIGSERIAL   PRIMARY KEY,
    kind        TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    token_hash  TEXT        NOT NULL UNIQUE,
    created_by  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ
);
