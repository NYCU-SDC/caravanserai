-- 001_create_resources.up.sql
--
-- Single-table resource store for Caravanserai.
--
-- Design notes:
--   - All resource kinds (Node, Project, ...) share this one table.
--     A new kind never requires a schema migration.
--   - spec and status are stored as separate JSONB columns so the controller
--     can update status without touching spec (avoids read-modify-write on
--     the spec side).
--   - labels is a first-class JSONB column to enable future label-selector
--     queries via GIN index without parsing the full spec blob.
--   - phase is a promoted TEXT column for cheap O(1) filtered list queries
--     (e.g. ListProjectsByPhase) without a GIN index scan.

CREATE TABLE IF NOT EXISTS resources (
    kind        TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    phase       TEXT        NOT NULL DEFAULT '',
    spec        JSONB       NOT NULL DEFAULT '{}',
    status      JSONB       NOT NULL DEFAULT '{}',
    labels      JSONB       NOT NULL DEFAULT '{}',
    annotations JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (kind, name)
);

-- Fast filtered list by kind + phase (used by ListProjectsByPhase, ListReadyNodeNames).
CREATE INDEX IF NOT EXISTS idx_resources_kind_phase
    ON resources (kind, phase);

-- GIN index on labels for future label-selector support.
CREATE INDEX IF NOT EXISTS idx_resources_labels
    ON resources USING GIN (labels);
