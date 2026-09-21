-- 086_seed_archon_system.down.sql
--
-- Rollback of 086: deletes the system operator. The created_via='system' filter
-- guards against accidentally deleting a same-named row of a different nature.
--
-- WARNING: FK push_providers.created_by_aid → operators(aid) may hold on to
-- the row if auto-import has already imported providers under this AID. In a clean
-- down chain, migration order tears down the dependent tables first; prod down is
-- never run (forward-only), so we leave the DELETE as-is.

DELETE FROM operators WHERE aid = 'archon-system' AND created_via = 'system';
