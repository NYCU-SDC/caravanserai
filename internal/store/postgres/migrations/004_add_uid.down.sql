-- 004_add_uid.down.sql
--
-- Reverses 004_add_uid.up.sql.

ALTER TABLE resources DROP CONSTRAINT IF EXISTS resources_uid_key;

ALTER TABLE resources DROP COLUMN IF EXISTS uid;
