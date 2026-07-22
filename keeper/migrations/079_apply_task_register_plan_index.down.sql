-- 079_apply_task_register_plan_index.down.sql
--
-- Rollback of keying apply_task_register by plan_index (ADR-056 S1 fix Variant B):
-- revert to PK (apply_id, sid, task_idx).
--
-- PRECONDITION for a correct rollback: no staged run (N>1 Passage) with
-- a task_idx collision between Passages in live data. If such rows exist (probe
-- passage0 + action passage1 share task_idx), restoring PK (apply_id, sid,
-- task_idx) will hit a duplicate - this is correct: a rollback after a rolled-out staged
-- register is impossible without data loss (forward-only in essence, like ADR-019). At
-- N=1 (plan_index==task_idx) the rollback is clean.

-- 1. Restore PK to (apply_id, sid, task_idx).
ALTER TABLE apply_task_register
    DROP CONSTRAINT apply_task_register_pkey;

ALTER TABLE apply_task_register
    ADD CONSTRAINT apply_task_register_pkey PRIMARY KEY (apply_id, sid, task_idx);

-- 2. Drop the plan_index column.
ALTER TABLE apply_task_register
    DROP COLUMN plan_index;
