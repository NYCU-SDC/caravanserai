-- 002_add_namespace.down.sql

ALTER TABLE resources DROP CONSTRAINT IF EXISTS resources_pkey;

ALTER TABLE resources ADD PRIMARY KEY (kind, name);

ALTER TABLE resources DROP COLUMN IF EXISTS resource_version;

ALTER TABLE resources DROP COLUMN IF EXISTS namespace;
