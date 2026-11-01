-- 137_create_apply_run_plan_host_times.down.sql
--
-- After the rollback the run view shows no task times again, as before NIM-1170.

DROP TABLE IF EXISTS apply_run_plan_host_times;
