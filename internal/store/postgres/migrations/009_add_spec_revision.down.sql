-- 009_add_spec_revision.down.sql
ALTER TABLE resources DROP COLUMN IF EXISTS spec_revision;
