-- 107_apply_runs_notices.down.sql
--
-- Rollback of the run-notices column (NIM-237). The notices of past runs are
-- lost with it: they are a record of what those runs reported, not state that
-- can be recomputed from anywhere else.

ALTER TABLE apply_runs
    DROP COLUMN IF EXISTS notices;
