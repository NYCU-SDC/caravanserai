-- 005_add_assignment_generation.down.sql
--
-- Reverses 005_add_assignment_generation.up.sql by removing the two
-- server-owned assignment keys from every Project status. The generation
-- counter cannot be reconstructed once dropped, which is acceptable: a
-- re-applied up migration re-derives a conservative baseline from nodeRef.

UPDATE resources
    SET status = (status - 'assignmentGeneration') - 'assignmentHistory'
    WHERE kind = 'Project'
      AND status IS NOT NULL;
