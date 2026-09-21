-- 079_apply_task_register_plan_index.up.sql
--
-- Fixes a latent staged-register bug (ADR-056 §S1 fix, Variant B): keys
-- apply_task_register by the GLOBAL, plan-wide plan_index instead of the
-- LOCAL task_idx.
--
-- ROOT CAUSE (two defects). The Soul emits TaskEvent.task_idx = the LOCAL
-- position of the task in its Passage's ApplyRequest.tasks[] (the proto
-- RenderedTask did not carry a global index before). On staged renders (N>1
-- Passage), a single Passage's ApplyRequest carries only the subset of that
-- Passage's tasks, further filtered per host by where: - so task_idx:
--   (1) is NOT unique across Passages: a probe task (passage 0, local
--       position 0) and an action (passage 1, local position 0) share
--       task_idx=0. The old PK (apply_id, sid, task_idx) -> ON CONFLICT let
--       the action overwrite the probe register.
--   (2) is NOT unique across hosts within one Passage: a different where:
--       yields a different subset -> the same register task gets a different
--       local idx on host-A and host-B.
-- On top of that, buildRegisterByHost (scenario/state.go) mapped
-- nameByIdx[t.Index] (GLOBAL) against rows.task_idx (LOCAL) - a name mismatch
-- on passage>0.
--
-- VARIANT B (architect design): proto only-add RenderedTask.plan_index +
-- TaskEvent.plan_index - a global, plan-wide task index (across ALL
-- Passages, = RenderedTask.Index on the Keeper). The Soul echoes back the
-- plan_index it received. Register is keyed by it: the global index is
-- unique across both Passages and hosts -> both defects go away. Variant A
-- (passage in the PK) was rejected: the local task_idx is ambiguous even
-- WITHIN a single Passage across hosts (different where:), so
-- (apply_id, sid, passage, task_idx) would still be fragile.
--
-- ADDITIVITY / backward-compat. plan_index NOT NULL DEFAULT 0:
--   - An N=1 run (a single Passage with no register dependencies): local idx
--     == global idx, plan_index == task_idx - the new key matches the old
--     one's selectivity at N=1, behavior is BIT-FOR-BIT identical.
--   - An old Soul without plan_index sends 0; but staged (N>1) is gated on
--     the "passage" capability (S5, Hello.capabilities) -> such a Soul is
--     not admitted to a staged scenario BEFORE dispatch. A non-staged
--     passage=0 run does not produce collisions now either (a single
--     Passage, local==global).

-- 1. plan_index column: a global, plan-wide task index across the whole run
--    plan. NOT NULL DEFAULT 0 - backfills existing rows with zero (on
--    current data passage is 0 everywhere, plan_index==task_idx, PK
--    selectivity unchanged).
ALTER TABLE apply_task_register
    ADD COLUMN plan_index INT NOT NULL DEFAULT 0;

-- 2. Backfill: on existing (N=1) rows the global index matches the local
--    task_idx. Makes old rows consistent with the new PK BEFORE it changes
--    (otherwise they would all collapse to plan_index=0 -> a duplicate PK).
UPDATE apply_task_register
    SET plan_index = task_idx;

-- 3. PK change: (apply_id, sid, task_idx) -> (apply_id, sid, plan_index). The
--    PK constraint's default name in PG is apply_task_register_pkey.
--    task_idx remains a plain data column (the local position in the
--    ApplyRequest, informational for triage); the register correlation and
--    upsert key is now plan_index.
ALTER TABLE apply_task_register
    DROP CONSTRAINT apply_task_register_pkey;

ALTER TABLE apply_task_register
    ADD CONSTRAINT apply_task_register_pkey PRIMARY KEY (apply_id, sid, plan_index);

COMMENT ON COLUMN apply_task_register.plan_index IS
    'Global, plan-wide task index across the whole run plan (all Passages, ADR-056 §S1 fix Variant B). The register correlation key (PK component). task_idx remains the local position in the ApplyRequest of its Passage - correlation on it was non-unique (latent bug).';

COMMENT ON COLUMN apply_task_register.task_idx IS
    'Local position of the task in the ApplyRequest.tasks[] of its Passage (informational). NOT the correlation key - see plan_index.';
