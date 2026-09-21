-- 023_create_purge_apply_task_register.down.sql

DROP FUNCTION IF EXISTS purge_apply_task_register(interval, integer);
