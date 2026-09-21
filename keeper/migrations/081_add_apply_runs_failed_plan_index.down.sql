-- 081_add_apply_runs_failed_plan_index.down.sql
--
-- Rolls back the failure-channel fix (ADR-056 section S1 fix Variant B): drops the
-- failed_plan_index column. For N=1 (failed_plan_index == task_idx) the rollback is clean -
-- the failed task's data stays in task_idx, correlation reverts to it (as before the
-- fix; for N=1, task_idx == the global index, so behavior is correct). On a
-- rolled-out staged render (N>1), after the rollback failure correlation would again become
-- local (mislabel) - forward-only in practice, like 079/080.
ALTER TABLE apply_runs
    DROP COLUMN IF EXISTS failed_plan_index;
