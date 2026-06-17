-- 051_create_push_runs.down.sql

DROP INDEX IF EXISTS push_runs_started_by_kid_idx;
DROP INDEX IF EXISTS push_runs_status_idx;
DROP TABLE IF EXISTS push_runs;
