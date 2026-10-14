-- 124_add_apply_run_plan_waves.down.sql
--
-- After the rollback a serial: step reads as a flat task list again, as before
-- NIM-996.

ALTER TABLE apply_run_plan DROP COLUMN IF EXISTS waves;
