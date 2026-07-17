-- 023_create_purge_apply_task_register.up.sql
--
-- Reaper rule `purge_apply_task_register` (docs/keeper/reaper.md): deletes a
-- batch of register rows (`apply_task_register`, migration 022) for runs
-- whose `apply_runs` is already in a TERMINAL status (`success` / `failed` /
-- `cancelled`) and finished (`finished_at`) older than `grace_period`.
--
-- Why a separate rule, when the FK `ON DELETE CASCADE` already cleans up the
-- register together with the apply_runs row (rule `purge_apply_runs`, default 30d)?
--   - `apply_task_register.register_data` is plaintext JSONB of probe task
--     results, potentially containing secrets (`register: X` + command output).
--     This is TRANSIENT run-state: the scenario-runner reads it exactly once
--     after the cross-host barrier to render `state_changes.sets`, and after
--     that it is no longer needed. Keeping it for the full 30d apply-history
--     retention is an unnecessary plaintext-storage window. The rule reaps
--     register more aggressively (default 1h after terminal), leaving
--     apply_run itself intact for history/triage.
--
-- Criterion "terminal status + grace" (and NOT a plain TTL on created_at):
--   - the terminal filter guarantees that the register of an ACTIVE
--     (`running`) run is NEVER deleted, regardless of its duration. A TTL on
--     created_at would reap the register of a long-running run whose
--     created_at is already in the past, corrupting the state_changes render
--     after the barrier.
--   - grace from `finished_at` is a buffer for the cross-Keeper-routing edge
--     case (ADR-002 stateless): RunResult arrived and apply_run became
--     terminal, but the run-goroutine on another instance is still reading
--     the register. The default grace is deliberately larger than the
--     "barrier → register read" time.
--
-- Age is measured from `apply_runs.finished_at` (not from
-- `apply_task_register.created_at`): "run finished N ago" is the correct
-- measure of the register's readiness for deletion. The register row's
-- created_at shifts on every retry-upsert of the same task and does not
-- reflect run completion.
--
-- DELETE ... USING joins on the composite key `(apply_id, sid)` against
-- apply_runs; the CTE with LIMIT batch_size caps the transaction size, as in
-- the other Reaper rules.

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
