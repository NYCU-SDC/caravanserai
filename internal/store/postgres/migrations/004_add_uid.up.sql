-- 004_add_uid.up.sql
--
-- Adds metadata.uid: an immutable, server-generated identity assigned once when
-- a resource is first created (CARA-82).
--
-- Unlike (kind, namespace, name), which can be reused after deletion, uid is
-- never reused. Runtime ownership (Docker container/network labels) is fenced
-- on uid so a leftover container from a previous lifetime of the same name
-- cannot be mistaken for the current resource.
--
-- Rollout order matters: add the column nullable, backfill every existing row
-- with a fresh value, then enforce NOT NULL and global uniqueness. Doing it in
-- one step would fail against a table that already holds rows.

-- 1. Add nullable so existing rows are not rejected.
ALTER TABLE resources
    ADD COLUMN IF NOT EXISTS uid TEXT;

-- 2. Backfill existing rows. gen_random_uuid() is built into PostgreSQL 13+.
--    Each row gets its own value; there is no correct way to reconstruct the
--    original identity of a pre-UID resource, so a fresh UID is assigned.
UPDATE resources
    SET uid = gen_random_uuid()::text
    WHERE uid IS NULL;

-- 3. Enforce the invariants now that every row has a value.
ALTER TABLE resources
    ALTER COLUMN uid SET NOT NULL;

ALTER TABLE resources
    ADD CONSTRAINT resources_uid_key UNIQUE (uid);
