-- 089_add_incarnation_created_scenario.down.sql
--
-- Reversible rollback of the multiple create-scenarios mechanism (Variant A): drop
-- the incarnation.created_scenario column. rerun-create logic reverts to the
-- previous hardcode `create` (state_history-based), which is correct for runtimes
-- without multiple create scenarios.

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS created_scenario;
