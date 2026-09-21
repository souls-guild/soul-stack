-- 021_create_purge_apply_runs.up.sql
--
-- Reaper rule `purge_apply_runs` (docs/keeper/reaper.md): deletes a batch
-- of finished apply runs from the `apply_runs` registry (migration 018) older than
-- `max_age`. Apply-history retention (default 30d, counted from
-- `finished_at`).
--
-- ONLY finished records are deleted (`success` / `failed` / `cancelled` with
-- `finished_at IS NOT NULL`). `running` records are NEVER purged - these are
-- hanging/in-progress runs; their triage and completion are a separate
-- mechanism of the scenario runner, not the Reaper.
--
-- Age is counted from `finished_at`: a run's history is measured by the time
-- elapsed since completion, not since start (a long run that finished
-- recently should not be deleted before a short run that finished long ago).
--
-- composite PK `(apply_id, sid)` (018) → DELETE ... USING expired on both
-- keys; a CTE with LIMIT batch_size bounds the transaction size, as in
-- the other Reaper rules.

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
