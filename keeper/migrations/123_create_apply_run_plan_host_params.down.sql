-- 123_create_apply_run_plan_host_params.down.sql
--
-- After the rollback a per-host task shows `<per-host>` again, as before NIM-985.

DROP TABLE IF EXISTS apply_run_plan_host_params;
