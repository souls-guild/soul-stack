-- 024_add_apply_runs_cancel_requested.down.sql

ALTER TABLE apply_runs
    DROP COLUMN IF EXISTS cancel_requested;
