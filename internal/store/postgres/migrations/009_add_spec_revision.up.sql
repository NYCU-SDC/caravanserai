-- 009_add_spec_revision.up.sql
--
-- Adds metadata.specRevision (CARA-134, ADR-0026): the version of a
-- Project's spec, 1 at creation and one more each time an update changes the
-- spec's content. resource_version cannot serve: it moves on every status
-- write. Rows of other kinds keep the default and never read it.
ALTER TABLE resources
    ADD COLUMN IF NOT EXISTS spec_revision BIGINT NOT NULL DEFAULT 1;
