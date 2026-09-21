-- 097_create_purge_apply_run_plan.up.sql
--
-- Reaper rule purge_apply_run_plan (docs/keeper/reaper.md): deletes a batch
-- of run-task plan rows (apply_run_plan, migration 096) whose run has finished and is
-- old enough. Mirrors purge_apply_task_register (023), but keyed on apply_id rather than
-- (apply_id, sid): the plan is host-invariant, it has no sid.
--
-- Why a separate rule (unlike apply_task_register, where FK ON DELETE
-- CASCADE cleans up automatically): apply_run_plan has NO FK (apply_id isn't a PK in any
-- table), so plan rows are NOT carried away automatically by purge_apply_runs -
-- without this rule they would grow without bound (orphaned once apply_runs is purged).
--
-- Deletion criteria:
--   - created_at < NOW() - grace_period - the plan is older than the retention window. Age is measured from
--     created_at (written ONCE at dispatch, never shifted - there's no retry-upsert per-host
--     like there is for register). Grace is aligned with the apply-history retention (30d):
--     the plan is needed by the /tasks endpoint for as long as the run's apply_runs live.
--     The floor on created_at also closes the race where "the plan is written at render time, but
--     this run's apply_runs rows haven't been inserted by dispatch yet" - a fresh plan
--     is left untouched under the grace window.
--   - NOT EXISTS a non-terminal apply_run for the run - the plan of an ACTIVE (running/
--     planned/...) run is NEVER deleted, regardless of age. An orphan
--     (apply_runs already purged by purge_apply_runs) has no non-terminal rows ->
--     gets deleted once it's past the created_at floor.
--
-- The CTE with LIMIT batch_size bounds the transaction size, as in the other
-- Reaper rules.

CREATE OR REPLACE FUNCTION purge_apply_run_plan(grace_period interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT p.apply_id, p.plan_index
        FROM apply_run_plan p
        WHERE p.created_at < NOW() - grace_period
          AND NOT EXISTS (
              SELECT 1
              FROM apply_runs ar
              WHERE ar.apply_id = p.apply_id
                AND ar.status NOT IN ('success', 'failed', 'cancelled')
          )
        ORDER BY p.created_at
        LIMIT batch_size
    )
    DELETE FROM apply_run_plan t
    USING expired e
    WHERE t.apply_id = e.apply_id AND t.plan_index = e.plan_index;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_apply_run_plan(interval, integer) IS
    'Deletes a batch of apply_run_plan rows for runs older than grace_period (by created_at) with NO non-terminal apply_runs. Leaves the plan of an active run untouched. Returns the number of rows deleted.';
