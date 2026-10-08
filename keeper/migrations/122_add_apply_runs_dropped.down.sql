-- 122_add_apply_runs_dropped.down.sql
--
-- A run that ended `partial_failed` reads `failed` again after the rollback:
-- its dropped hosts' rows are failed rows.

ALTER TABLE apply_runs DROP COLUMN IF EXISTS dropped;
