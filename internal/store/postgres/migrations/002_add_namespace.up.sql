-- 002_add_namespace.up.sql
--
-- Adds metadata.namespace and metadata.resourceVersion to ObjectMeta.
--
-- 1.0 locks every write to the "default" namespace (enforced in
-- api/v1/validation.go's ValidateNamespace) — this migration only lays the
-- DB groundwork so the primary key never needs to change again once
-- namespaces open up post-1.0.

ALTER TABLE resources
    ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT 'default';

ALTER TABLE resources
    ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE resources DROP CONSTRAINT IF EXISTS resources_pkey;

ALTER TABLE resources ADD PRIMARY KEY (kind, namespace, name);
