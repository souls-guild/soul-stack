-- 098_add_apply_run_plan_params.down.sql

ALTER TABLE apply_run_plan
    DROP COLUMN IF EXISTS params;
