-- 043_mark_disconnected_lease_aware.up.sql
--
-- Lease-aware BIDIRECTIONAL reconcile of the `souls.status` snapshot (Reaper
-- rule `mark_disconnected`, docs/keeper/reaper.md -> ADR-006(a) deferred
-- slice). `souls.status` is a lazy "last known" snapshot for the Operator
-- API, NOT the presence source (Redis SID-lease decides online/offline). The
-- Reaper reconciles the snapshot to the lease's reality in the BACKGROUND, in
-- both directions:
--
--   * connected -> disconnected: stale `last_seen_at` AND no live SID-lease;
--   * disconnected -> connected: the SID-lease is alive (the Soul is really
--     online; a reconnect of an already-onboarded Soul does not touch the
--     Bootstrap RPC, and the eventstream does not write presence to PG on
--     the hot path - only the Reaper fixes the snapshot).
--
-- Without the reverse direction, the snapshot would latch into `disconnected`
-- forever after the first "drop+sweep": a reconnect raises the lease, but
-- nothing moved the row - the Operator API returned a contradiction
-- (status=disconnected + a fresh last_seen_at).
--
-- Before this migration, the rule marked `disconnected` purely by PG
-- `last_seen_at` (migration 014): a live stream kept `last_seen_at` fresh via
-- a throttled flush from the EventStream handler. But an idle Soul that only
-- sends a soulprint once per refresh_interval could get a stale
-- `last_seen_at` within stale_after and be falsely marked `disconnected` on a
-- LIVE stream.
--
-- The fix is a two-phase lease-aware rule in Go (keeper/internal/reaper/purger.go):
--   1) select PG candidates for both directions (select_disconnect_candidates /
--      select_reconnect_candidates);
--   2) check each one against the Redis SID-lease (Go side, the Purger has
--      Redis access): no lease -> disconnect, live lease -> reconnect;
--   3) apply (mark_disconnected_sids / mark_connected_sids).
--
-- The old `mark_disconnected(interval, integer)` (migration 014) is NOT
-- removed: it remains a fallback for when Redis is not configured
-- (single-instance dev / unit mode without coordination) - in that case the
-- rule is one-directional pure SQL, and there is no latch by construction
-- (stale `last_seen_at` <=> no stream on a single instance).

-- select_disconnect_candidates - candidates for disconnect: connected souls
-- with `last_seen_at` older than stale_after. Returns only SIDs (the Go side
-- checks each against Redis and marks the survivors). ORDER BY last_seen_at +
-- LIMIT - the same drain pattern as the other rules (oldest first, batch
-- cap).
--
-- last_seen_at IS NULL (never connected) does not match the predicate:
-- NULL < NOW() - stale_after = NULL -> false (SQL three-valued logic),
-- symmetric with the original mark_disconnected.
CREATE OR REPLACE FUNCTION select_disconnect_candidates(stale_after interval, batch_size integer DEFAULT 1000)
RETURNS SETOF text AS $$
    SELECT sid
    FROM souls
    WHERE status = 'connected'
      AND last_seen_at < NOW() - stale_after
    ORDER BY last_seen_at
    LIMIT batch_size;
$$ LANGUAGE sql STABLE;

COMMENT ON FUNCTION select_disconnect_candidates(interval, integer) IS
    'Returns SIDs of connected souls with a stale last_seen_at (mark_disconnected candidates). The Go side filters by a live Redis lease/heartbeat and marks the survivors via mark_disconnected_sids.';

-- mark_disconnected_sids - mark connected -> disconnected for exactly the
-- listed SIDs. Applied after Go-side candidate filtering: the list contains
-- ONLY the ones that are really stale (no live Redis lease/heartbeat). The
-- repeated `status = 'connected'` guard in WHERE protects against a race: the
-- SID could have changed status between the select phase and the mark phase
-- (Bootstrap/teardown on another instance). An empty array -> 0 rows (no-op).
CREATE OR REPLACE FUNCTION mark_disconnected_sids(target_sids text[])
RETURNS BIGINT AS $$
DECLARE
    updated_count BIGINT;
BEGIN
    UPDATE souls
       SET status = 'disconnected'
     WHERE sid = ANY(target_sids)
       AND status = 'connected';

    GET DIAGNOSTICS updated_count = ROW_COUNT;
    RETURN updated_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION mark_disconnected_sids(text[]) IS
    'Transitions the listed connected souls to disconnected. Applied after Go-side filtering of select_disconnect_candidates candidates by a live Redis lease/heartbeat. Returns the number of updated rows.';

-- select_reconnect_candidates - the reverse direction of the reconcile:
-- candidates to return from `disconnected` to `connected`. Unlike the
-- disconnect direction, WITHOUT a predicate on `last_seen_at`: online status
-- is determined by a live SID-lease, not the freshness of the PG snapshot (an
-- idle Soul on a live stream holds the lease, but `last_seen_at` may have
-- gone stale - filtering by time here would wrongly keep it from returning to
-- connected). Returns only SIDs (the Go side checks each against Redis and
-- marks the ones whose lease is ALIVE). ORDER BY last_seen_at NULLS FIRST +
-- LIMIT - the same drain pattern (oldest/never-seen first, batch cap).
CREATE OR REPLACE FUNCTION select_reconnect_candidates(batch_size integer DEFAULT 1000)
RETURNS SETOF text AS $$
    SELECT sid
    FROM souls
    WHERE status = 'disconnected'
    ORDER BY last_seen_at NULLS FIRST
    LIMIT batch_size;
$$ LANGUAGE sql STABLE;

COMMENT ON FUNCTION select_reconnect_candidates(integer) IS
    'Returns SIDs of disconnected souls (any last_seen_at) - candidates for returning to connected. The Go side filters by a LIVE Redis lease and marks the survivors via mark_connected_sids. The reverse direction of the mark_disconnected reconcile.';

-- mark_connected_sids - mark disconnected -> connected for exactly the
-- listed SIDs. Applied after Go-side candidate filtering: the list contains
-- ONLY the ones that are really online (with a live Redis SID-lease). The
-- repeated `status = 'disconnected'` guard in WHERE protects against a race:
-- the SID could have changed status between the select phase and the mark
-- phase (revoke/teardown/cloud-destroy on another instance) - connected must
-- not overwrite revoked/destroyed. An empty array -> 0 rows (no-op).
CREATE OR REPLACE FUNCTION mark_connected_sids(target_sids text[])
RETURNS BIGINT AS $$
DECLARE
    updated_count BIGINT;
BEGIN
    UPDATE souls
       SET status = 'connected'
     WHERE sid = ANY(target_sids)
       AND status = 'disconnected';

    GET DIAGNOSTICS updated_count = ROW_COUNT;
    RETURN updated_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION mark_connected_sids(text[]) IS
    'Transitions the listed disconnected souls back to connected. Applied after Go-side filtering of select_reconnect_candidates candidates by a live Redis lease. The status=disconnected guard protects revoked/destroyed from being overwritten. Returns the number of updated rows.';
