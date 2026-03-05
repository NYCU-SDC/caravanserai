-- 001_create_resources.down.sql

DROP INDEX IF EXISTS idx_resources_labels;
DROP INDEX IF EXISTS idx_resources_kind_phase;
DROP TABLE IF EXISTS resources;
