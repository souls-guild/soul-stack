-- 078_add_apply_runs_passage.up.sql
--
-- Staged render (Passage, ADR-056, S1): a scenario run is now executed as N>=1
-- ordered Passages (render -> dispatch -> barrier -> register collection). One
-- run host now gets N tasks - one per Passage. The former
-- composite PK `apply_runs (apply_id, sid)` (migration 018) is no longer unique
-- per host.
--
-- APPROACH CHOICE (ADR-056 left I vs II open for S1): Variant I - extend the PK to
-- `(apply_id, sid, passage)`, N rows per host in the same table. Rationale
-- "smaller regression risk":
--   - The barrier (waitBarrier/classify), Ward-claim (ClaimNext/MarkDispatched),
--     recovery reclaim (ReclaimApplyRuns), Soul-reconcile (OrphanDispatched), and
--     RunResult correlation ALL work with per-host(-per-passage) rows of
--     THIS EXACT table. Variant II (a separate apply_run_passages) would have
--     required repointing all these reads/writes at the new table plus inventing a
--     write path for the apply_runs aggregate - a wider surface, not a narrower one.
--   - passage DEFAULT 0: on S1 NOBODY writes passage>0 (stratification is S2/S3),
--     so every existing `WHERE apply_id=$1 AND sid=$2` query hits
--     exactly one row (passage 0) - behavior BIT-FOR-BIT as it is now.
--   - The only FK referencing apply_runs(apply_id, sid) is
--     apply_task_register (migration 022). We repoint it at the triple
--     (apply_id, sid, passage). NB: at THIS migration the PK of apply_task_register is still
--     by task_idx; migration 079 (ADR-056 S1 fix Variant B) later pulls the
--     register key onto the GLOBAL plan_index - task_idx is LOCAL to Passage/host
--     (not unique across Passages or across hosts under per-host where:), it is NOT
--     a cross-Passage key (the original assumption here turned out to be wrong).
--
-- ADDITIVITY: passage NOT NULL DEFAULT 0 - existing rows get 0,
-- the new PK (apply_id, sid, 0) matches the old one in selectivity on the current
-- data. Backward compat (passage is 0 everywhere = as before staged-render) is an
-- S1 invariant.

-- 1. The passage column on apply_runs. NOT NULL DEFAULT 0: backfill existing
--    rows with zero, the new PK component is deterministic for the current write path
--    (Insert/InsertPlanned don't write passage explicitly -> 0).
ALTER TABLE apply_runs
    ADD COLUMN passage INT NOT NULL DEFAULT 0;

-- 2. apply_task_register: symmetrically carries passage (NOT NULL DEFAULT 0).
--    Needed BEFORE repointing the FK - the referencing columns must exist.
--    The PK of apply_task_register (apply_id, sid, task_idx) is NOT
--    changed by this migration; passage is row data + a component of the FK target. NB: task_idx
--    is LOCAL to Passage/host (not cross-cutting) - migration 079 will pull the register PK
--    onto the global plan_index (ADR-056 S1 fix Variant B).
ALTER TABLE apply_task_register
    ADD COLUMN passage INT NOT NULL DEFAULT 0;

-- 3. Repointing the FK apply_task_register -> apply_runs to the triple. The old FK
--    referenced (apply_id, sid); after the PK change on apply_runs that prefix is
--    no longer unique - the FK must include passage. ON DELETE CASCADE is preserved
--    (register data dies together with the Passage row; the cascade from incarnation
--    through apply_runs remains in effect).
ALTER TABLE apply_task_register
    DROP CONSTRAINT apply_task_register_apply_run_fk;

-- 4. Changing the apply_runs PK: (apply_id, sid) -> (apply_id, sid, passage). The
--    PK constraint name in PG defaults to apply_runs_pkey.
ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_pkey;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_pkey PRIMARY KEY (apply_id, sid, passage);

-- 5. Restoring the apply_task_register FK to point at the new triple key.
ALTER TABLE apply_task_register
    ADD CONSTRAINT apply_task_register_apply_run_fk
        FOREIGN KEY (apply_id, sid, passage) REFERENCES apply_runs (apply_id, sid, passage) ON DELETE CASCADE;

COMMENT ON COLUMN apply_runs.passage IS
    'Passage index (0-based) for staged render (ADR-056). 0 = the only Passage (behavior as before staged-render). Part of the PK (apply_id, sid, passage).';

COMMENT ON COLUMN apply_task_register.passage IS
    'Task Passage (ADR-056), component of the FK to apply_runs(apply_id, sid, passage). task_idx is LOCAL to Passage/host (not cross-cutting) - the register key was pulled onto the global plan_index by migration 079.';
