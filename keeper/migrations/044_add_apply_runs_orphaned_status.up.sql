-- 044_add_apply_runs_orphaned_status.up.sql
--
-- Soul-reconcile (ADR-027(g), S6): introduces the terminal status `orphaned` into the
-- `apply_runs.status` enum. Semantics -- the row stayed in `dispatched` after "Keeper and
-- Soul both died after dispatch", and Soul on reconnect did NOT declare this apply_id in
-- WardRoster (there's physically nothing in flight -- e.g. the Soul process restarted).
-- A RunResult for that row will never arrive; Keeper terminates it as `orphaned`
-- (applyrun.OrphanDispatched), the barrier classifies it as a failure (incarnation ->
-- error_locked). Without this status the row would get stuck in `dispatched` forever:
-- reclaim is scoped down to `claimed` (S4), and we deliberately don't do a Reaper dispatched-timeout.
--
-- Extending the CHECK via drop+recreate -- the 025/036/040 pattern (status is a CHECK
-- constraint, not a PG enum, so it's reversible). Additive: previous values are preserved.
--
-- Set of statuses after the migration:
--   planned / claimed / running / dispatched / success / failed / cancelled / orphaned.
--
-- We do NOT touch the partial index `apply_runs_claim_scan_idx` (025, WHERE status IN
-- ('planned','claimed','running')): orphaned rows are terminal, they don't
-- need this index (correlateRunResult/OrphanDispatched look up by exact
-- keys). The Reaper rule `purge_apply_runs` is extended separately (see below in this
-- same migration) -- orphaned is also a finished terminal, subject to retention purge.

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled', 'orphaned'));

-- purge_apply_runs (021): adds `orphaned` to the set of finished terminals
-- subject to retention purge (orphaned carries finished_at, like success/failed/
-- cancelled). Without this, orphaned rows would pile up forever.
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
