-- 045_add_apply_runs_no_match_status.down.sql
--
-- Rolls back the `no_match` terminal from the `apply_runs.status` enum (FINDING-01 option (b)).
-- Before narrowing the CHECK, we convert existing no_match rows to `success` - this is
-- exactly the pre-reform behavior for a non-target host (before FINDING-01, Acolyte closed
-- such a no-op host as success), otherwise ADD CONSTRAINT would fail on them. This way
-- down doesn't lose rows and doesn't fail (pattern from 040/044: a preliminary UPDATE
-- before drop+recreate).
--
-- Returns the CHECK to the 044 form (planned/claimed/running/dispatched/success/failed/
-- cancelled/orphaned) and narrows purge_apply_runs back (without no_match).

UPDATE apply_runs SET status = 'success' WHERE status = 'no_match';

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled', 'orphaned'));

CREATE OR REPLACE FUNCTION purge_apply_runs(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT apply_id, sid FROM apply_runs
        WHERE status IN ('success', 'failed', 'cancelled', 'orphaned')
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
    'Deletes a batch of finished apply_runs (success/failed/cancelled/orphaned) with finished_at older than max_age. Does not touch running/dispatched. Returns the number of deleted rows.';
