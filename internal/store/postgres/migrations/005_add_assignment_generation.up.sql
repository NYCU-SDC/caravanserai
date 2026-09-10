-- 005_add_assignment_generation.up.sql
--
-- Backfills status.assignmentGeneration and status.assignmentHistory for
-- existing Projects (CARA-83). Both live inside the status JSONB — assignment
-- generation is server-owned status, not a user manifest field or a table
-- column — so this migration only rewrites existing Project rows; new Projects
-- initialise the fields in the create handler.
--
-- The migration is conservative on purpose. nodeRef alone cannot prove a
-- Project's assignment history:
--
--   * A non-empty nodeRef proves the Project is currently assigned, so it
--     receives a non-zero baseline generation and Known history. The baseline
--     is 1 rather than 0 because 0 is reserved for a NeverAssigned Project;
--     giving an assigned row generation 0 would let a stale generation-0 report
--     appear current.
--
--   * An empty nodeRef is ambiguous: the Project may have been never assigned,
--     or assigned and later released. We cannot tell from a migrated row, so it
--     becomes Unknown rather than NeverAssigned. Unknown is treated as
--     ineligible for immediate deletion or automatic takeover until cleanup or
--     fencing evidence resolves it. Only Projects created after this migration,
--     through the create handler, are trusted as NeverAssigned at generation 0.
--
-- The WHERE clause skips rows that already carry assignmentHistory so the
-- migration is idempotent and never overwrites a value written after rollout.

-- Currently assigned Projects: non-zero baseline generation and Known history.
UPDATE resources
    SET status = COALESCE(status, '{}'::jsonb)
        || '{"assignmentGeneration": 1, "assignmentHistory": "Known"}'::jsonb
    WHERE kind = 'Project'
      AND NOT (COALESCE(status, '{}'::jsonb) ? 'assignmentHistory')
      AND COALESCE(status->>'nodeRef', '') <> '';

-- Unassigned Projects with unprovable history: Unknown, generation left at 0.
UPDATE resources
    SET status = COALESCE(status, '{}'::jsonb)
        || '{"assignmentHistory": "Unknown"}'::jsonb
    WHERE kind = 'Project'
      AND NOT (COALESCE(status, '{}'::jsonb) ? 'assignmentHistory')
      AND COALESCE(status->>'nodeRef', '') = '';
