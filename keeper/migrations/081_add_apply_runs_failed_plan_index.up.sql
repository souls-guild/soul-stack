-- 081_add_apply_runs_failed_plan_index.up.sql
--
-- Fixes the failure channel for staged-render (ADR-056 §S1 fix Variant B,
-- the last instance of the global-vs-local-task_idx class): apply_runs now
-- carries the GLOBAL cross-plan plan_index of the failed task, not just its
-- LOCAL position in ApplyRequest.tasks[] (the task_idx column).
--
-- ROOT CAUSE. recordTaskFailure (grpc/events_taskevent.go) wrote the LOCAL
-- TaskEvent.task_idx into apply_runs.task_idx (the task's position in
-- ApplyRequest.tasks[] for its Passage). buildHostReport (scenario/checkdrift.go,
-- failure branch) and failureReason (scenario/dispatch.go, no_log suppression)
-- resolved the failed task's name/metadata via taskMeta[task_idx] /
-- noLogByIndex[task_idx], where taskMeta/noLogByIndex are built from
-- RenderedTask.Index (the GLOBAL index across the whole plan). On
-- staged-render (N>1 Passage) or with per-host where: the local task_idx !=
-- the global Index -> the module/action of the failed task got mislabeled
-- with a neighboring task. The same defect already closed in the register
-- channel by migration 079; the failure channel is its last instance.
--
-- VARIANT B (symmetric with 079): Soul already echoes TaskEvent.plan_index
-- (the global cross-plan index, = RenderedTask.Index) on ALL branches.
-- apply_runs gets a separate nullable column failed_plan_index for this
-- global index; task_idx remains informational-local (for triage), same as
-- apply_task_register.task_idx after 079. Correlators (drift-report failure
-- + barrier no_log) switch over to failed_plan_index.
--
-- ADDITIVITY / backward compat. failed_plan_index is nullable (like
-- task_idx):
--   - N=1 run (a single Passage, no per-host filtering) -- local idx ==
--     global idx, failed_plan_index == task_idx of the failed task.
--     Correlation via the new field matches the old (task_idx) by result.
--     BACKFILL on existing rows: failed_plan_index := task_idx (where
--     task_idx is set) -- for N=1 this is the EXACT value of the global
--     index.
--   - An old Soul without plan_index in TaskEvent (plan_index=0): a
--     non-staged passage=0 run doesn't produce collisions (local==global),
--     staged (N>1) is gated on the "passage" capability (S5) -- such a Soul
--     isn't allowed into a staged scenario before dispatch.

-- 1. Column failed_plan_index: the global cross-plan index of the failed
--    task across the whole run plan (all Passages). NULL until the first
--    failed task (like task_idx); filled in by recordTaskFailure
--    first-failure-wins (COALESCE).
ALTER TABLE apply_runs
    ADD COLUMN failed_plan_index INT;

-- 2. Backfill: on existing rows with an already-recorded failed task, the
--    global index matches the local task_idx (passage is 0 everywhere in
--    the current data, local==global). Makes old rows consistent with the
--    new correlation.
UPDATE apply_runs
    SET failed_plan_index = task_idx
    WHERE task_idx IS NOT NULL;

COMMENT ON COLUMN apply_runs.failed_plan_index IS
    'Global cross-plan plan_index of the first failed task of the host across the whole run plan (all Passages, ADR-056 §S1 fix Variant B). Correlation key with RenderedTask.Index for the module/action of the failed task (drift-report) and no_log suppression (barrier). task_idx remains the local position within the ApplyRequest of its Passage (informational) -- correlation on it was non-unique under staged/per-host-where (latent bug).';

COMMENT ON COLUMN apply_runs.task_idx IS
    'Local position of the first failed task in ApplyRequest.tasks[] of its Passage (informational, echoes TaskEvent.task_idx). NOT the correlation key with the global plan -- see failed_plan_index.';
