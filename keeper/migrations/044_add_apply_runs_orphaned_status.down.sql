-- 044_add_apply_runs_orphaned_status.down.sql
--
-- Rollback of the `orphaned` terminal from the `apply_runs.status` enum (Soul-reconcile,
-- ADR-027(g), S6). Before narrowing the CHECK, we convert existing orphaned rows to
-- `failed` - the closest terminal-fail match in semantics for the pre-reform schema
-- (RunResult never arrived, the host did not finish the run successfully), otherwise
-- ADD CONSTRAINT would fail on them. This way down does not lose rows and does not fail
-- (pattern from 040: a preliminary UPDATE before drop+recreate).
--
-- Returns the CHECK to the 040 form (planned/claimed/running/dispatched/success/failed/
-- cancelled) and narrows purge_apply_runs back (without orphaned).

UPDATE apply_runs SET status = 'failed' WHERE status = 'orphaned';

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled'));

CREATE OR REPLACE FUNCTION purge_apply_runs(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT apply_id, sid FROM apply_runs
        WHERE status IN ('success', 'failed', 'cancelled')
          AND finished_at IS NOT NULL
          AND finished_at < NOW() - max_age
        ORDER BY finished_at
        LIMIT batch_size
    )
    DELETE FROM apply_runs ar
    USING expired e
    WHERE ar.apply_id = e.apply_id AND ar.sid = e.sid;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_apply_runs(interval, integer) IS
    'Deletes a batch of finished apply_runs (success/failed/cancelled) with finished_at older than max_age. Does not touch running. Returns the number of deleted rows.';
