-- 006_create_takeover_audit.up.sql
--
-- Append-only record of operator takeovers (CARA-89).
--
-- Design notes:
--   - No foreign key to resources: the record has to outlive the Project.
--   - request_id is unique so a retried request cannot record a second decision.
--   - caller holds only what the server observed. A shared token does not
--     identify a person, so this must not be read as individual attribution.

CREATE TABLE IF NOT EXISTS takeover_audit (
    id                    BIGSERIAL   PRIMARY KEY,
    request_id            TEXT        NOT NULL UNIQUE,
    namespace             TEXT        NOT NULL,
    project_name          TEXT        NOT NULL,
    project_uid           TEXT        NOT NULL,
    node_name             TEXT        NOT NULL,
    assignment_generation BIGINT      NOT NULL,
    evidence              TEXT        NOT NULL,
    outcome               TEXT        NOT NULL,
    caller                TEXT        NOT NULL,
    note                  TEXT        NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_takeover_audit_project
    ON takeover_audit (namespace, project_name, created_at);
