-- 111_state_history_run.down.sql
--
-- Drops the four columns. Reversible in SCHEMA; the data in them is lost, and
-- there is no source to recompute it from — a run's input and its outcome were
-- never derived from anything else in the database.
--
-- The consequence of rolling back is worth stating rather than discovering: a
-- keeper on pre-NIM-408 code resolves a rerun's input the old way (create path
-- from `incarnation.spec.input`, day-2 from `apply_runs.recipe`), which works
-- only while that column still exists and the recipe has not been purged. If the
-- rollback crosses the migration that drops `incarnation.spec`, the create path
-- has nowhere left to read from and every rerun-last of a create failure answers
-- ErrRerunInputUnavailable.

ALTER TABLE state_history
    DROP COLUMN IF EXISTS error_summary,
    DROP COLUMN IF EXISTS finished_at,
    DROP COLUMN IF EXISTS run_status,
    DROP COLUMN IF EXISTS run;
