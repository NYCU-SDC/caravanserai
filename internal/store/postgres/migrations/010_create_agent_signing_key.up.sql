-- 010_create_agent_signing_key.up.sql
--
-- The Ed25519 key cara-server signs its requests to Agents with (CARA-139,
-- ADR-0031). Agents fetch the public half and refuse requests on their API
-- that it did not sign.
--
-- Design notes:
--   - One row. The CHECK keeps it that way, and the first server to start
--     inserts it with ON CONFLICT DO NOTHING, so servers starting together
--     agree on one key.
--   - Kept in the database rather than a file so a server restored from a
--     backup, or one taking over as leader, signs with the key Agents
--     already trust.
--   - The private key is stored as is, like Secret values today (CARA-56).
--     Whoever can read this table can call any Agent.

CREATE TABLE IF NOT EXISTS agent_signing_key (
    id          SMALLINT    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    private_key BYTEA       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL
);
