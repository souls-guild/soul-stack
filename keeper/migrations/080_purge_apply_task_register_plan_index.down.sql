-- 080_purge_apply_task_register_plan_index.down.sql
--
-- Rollback of forward-fix 080: restores the body of purge_apply_task_register to the
-- 023 form (DELETE-join over task_idx). Down 080 runs BEFORE down 079 (rollback order
-- is reversed), and 079.down drops the plan_index column - so the function must stop
-- referencing plan_index, otherwise the next purge would fail on a nonexistent column.
-- The restored form is identical to 023 (N=1: task_idx is unique, behavior is correct).

CREATE OR REPLACE FUNCTION purge_apply_task_register(grace_period interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT atr.apply_id, atr.sid, atr.task_idx
        FROM apply_task_register atr
        JOIN apply_runs ar
          ON ar.apply_id = atr.apply_id AND ar.sid = atr.sid
        WHERE ar.status IN ('success', 'failed', 'cancelled')
          AND ar.finished_at IS NOT NULL
          AND ar.finished_at < NOW() - grace_period
        ORDER BY ar.finished_at
        LIMIT batch_size
    )
    DELETE FROM apply_task_register t
    USING expired e
    WHERE t.apply_id = e.apply_id AND t.sid = e.sid AND t.task_idx = e.task_idx;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_apply_task_register(interval, integer) IS
    'Deletes a batch of apply_task_register rows for runs in a terminal status (success/failed/cancelled) with finished_at older than grace_period. Does not touch the register of an active (running) run. Returns the number of deleted rows.';
