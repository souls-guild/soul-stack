-- 045_add_apply_runs_no_match_status.up.sql
--
-- FINDING-01 option (b): introduces a terminal status `no_match` in the enum
-- `apply_runs.status`. Semantics -- the host is out of scope for the run:
-- the Acolyte path (acolytes>0) writes a planned task for EVERY roster host
-- of the incarnation BEFORE the per-host resolution of `on:`/`where:`
-- (resolved later, at claim time). A host that ends up with 0 tasks after
-- `on:`/`where:` is closed by Acolyte as `no_match` (NOT `success`) --
-- apply_runs no longer over-reports "success where nothing was applied".
-- The barrier classifies `no_match` as TERMINAL and NOT-a-failure (benign,
-- like success): a run where in-scope hosts succeeded and out-of-scope
-- hosts are no_match still drives the incarnation to `ready` (NOT
-- error_locked).
--
-- Extending the CHECK via drop+recreate -- the pattern from 025/036/040/044
-- (status is a CHECK constraint, not a PG enum, so it's reversible).
-- Additive: previous values are preserved, this lands ON TOP of 044
-- (orphaned) without breaking it.
--
-- Set of statuses after this migration:
--   planned / claimed / running / dispatched / success / failed / cancelled /
--   orphaned / no_match.
--
-- The partial index `apply_runs_claim_scan_idx` (025, WHERE status IN
-- ('planned','claimed','running')) is left UNTOUCHED: no_match rows are
-- terminal, they don't need this index. The Reaper rule `purge_apply_runs`
-- is extended below -- no_match is also a finished-terminal (carries
-- finished_at), subject to retention purge, otherwise out-of-scope-host
-- rows would pile up forever.

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled', 'orphaned', 'no_match'));

-- purge_apply_runs (021/044): adds `no_match` to the set of finished terminals
-- subject to retention purge (no_match carries finished_at, like success/failed/
-- cancelled/orphaned). Without this, out-of-scope-host rows would pile up forever.
CREATE OR REPLACE FUNCTION purge_apply_runs(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT apply_id, sid FROM apply_runs
        WHERE status IN ('success', 'failed', 'cancelled', 'orphaned', 'no_match')
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
    'Deletes a batch of finished apply_runs (success/failed/cancelled/orphaned/no_match) with finished_at older than max_age. Does not touch running/dispatched. Returns the number of deleted rows.';
