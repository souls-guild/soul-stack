-- 132_add_apply_runs_handled.down.sql
--
-- A run that ended `success` by handling its failures reads `failed` (or
-- `partial_failed` beside a drop) again after the rollback: its handled rows
-- are failed rows.

ALTER TABLE apply_runs DROP COLUMN IF EXISTS handled;
