-- 101_add_apply_runs_input.down.sql

ALTER TABLE apply_runs
    DROP COLUMN IF EXISTS input;
