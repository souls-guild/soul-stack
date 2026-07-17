-- 078_add_apply_runs_passage.down.sql
--
-- Rollback of the staged-render Passage schema (ADR-056, S1): revert to the
-- composite PK `apply_runs (apply_id, sid)` and the FK apply_task_register on
-- the pair.
--
-- PRECONDITION for a correct rollback: passage is 0 everywhere (N=1, not a
-- single multi-passage run). If the table has rows with passage>0 (S2/S3
-- stratification has already written them), the DROP of the old PK /
-- restoring the PK on (apply_id, sid) will hit duplicates - which is
-- correct: rolling back after a rolled-out staged-render is impossible
-- without data loss, the migration is forward-only in essence (like
-- ADR-019). On S1 (passage 0 everywhere) the rollback is clean.

-- 1. Drop the triple FK before restoring the paired PK.
ALTER TABLE apply_task_register
    DROP CONSTRAINT apply_task_register_apply_run_fk;

-- 2. Restore PK apply_runs to (apply_id, sid).
ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_pkey;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_pkey PRIMARY KEY (apply_id, sid);

-- 3. Restore FK apply_task_register on the pair (apply_id, sid).
ALTER TABLE apply_task_register
    ADD CONSTRAINT apply_task_register_apply_run_fk
        FOREIGN KEY (apply_id, sid) REFERENCES apply_runs (apply_id, sid) ON DELETE CASCADE;

-- 4. Drop the passage columns.
ALTER TABLE apply_task_register
    DROP COLUMN passage;

ALTER TABLE apply_runs
    DROP COLUMN passage;
