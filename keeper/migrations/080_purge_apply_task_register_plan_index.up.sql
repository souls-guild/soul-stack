-- 080_purge_apply_task_register_plan_index.up.sql
--
-- Forward fix for the Reaper rule `purge_apply_task_register` (023) under staged-render
-- (ADR-056 §S1 fix Variant B, migrations 078/079): the DELETE join switches from
-- `task_idx` to the stably-unique `plan_index`.
--
-- THE PROBLEM (batch overshoot): migration 023 deleted register rows by joining on
-- `(apply_id, sid, task_idx)`. After 079, task_idx is NO LONGER unique within
-- (apply_id, sid) under staged-render (N>1 Passage): the passage0 probe and the passage1
-- action share local idx=0. The `expired` CTE picks batch_size rows, but the
-- final DELETE join on the non-unique task_idx wipes out ALL rows sharing
-- that task_idx within the host -- one selected row deletes N physical rows.
-- `LIMIT batch_size` on the CTE then stops accurately bounding the transaction
-- size (overshoot up to N x batch_size). With N=1 (old data, passage always 0,
-- plan_index==task_idx) the bug didn't show up.
--
-- THE FIX: join on `(apply_id, sid, plan_index)` -- the stably-unique key
-- of a register row (PK of apply_task_register after 079). One selected row deletes
-- exactly one row, batch_size is accurate again. The CTE projection carries plan_index
-- instead of task_idx.
--
-- WHY A NEW MIGRATION INSTEAD OF EDITING 023 IN PLACE: 023 has already been applied to
-- existing databases (beta.1). golang-migrate does not re-apply already-applied
-- migrations (it only stores the version, not a body checksum) -> editing 023's body wouldn't
-- reach already-deployed databases. A `CREATE OR REPLACE FUNCTION` in a separate forward migration
-- re-executes everywhere and replaces the function body. Depends on plan_index (079) --
-- ordering 079 < 080 is correct.

CREATE OR REPLACE FUNCTION purge_apply_task_register(grace_period interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT atr.apply_id, atr.sid, atr.plan_index
        FROM apply_task_register atr
        JOIN apply_runs ar
          ON ar.apply_id = atr.apply_id AND ar.sid = atr.sid AND ar.passage = atr.passage
        WHERE ar.status IN ('success', 'failed', 'cancelled')
          AND ar.finished_at IS NOT NULL
          AND ar.finished_at < NOW() - grace_period
        ORDER BY ar.finished_at
        LIMIT batch_size
    )
    DELETE FROM apply_task_register t
    USING expired e
    WHERE t.apply_id = e.apply_id AND t.sid = e.sid AND t.plan_index = e.plan_index;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_apply_task_register(interval, integer) IS
    'Deletes a batch of apply_task_register rows for runs in a terminal status (success/failed/cancelled) with finished_at older than grace_period. Deletion key is (apply_id, sid, plan_index) (stably unique after 079, ADR-056). Does not touch the register of an active (running) run. Returns the number of deleted rows.';
