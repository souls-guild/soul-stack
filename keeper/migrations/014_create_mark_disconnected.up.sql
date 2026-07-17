-- 014_create_mark_disconnected.up.sql
--
-- Reaper rule `mark_disconnected` (docs/keeper/reaper.md): UPDATE-only
-- rule (action `set_status`), transitions `souls.status='connected'` ->
-- `disconnected` if `last_seen_at` is older than `stale_after` (default 90s).
--
-- Unlike the other rules, there is no filter by `target_statuses[]` here
-- - only the source `connected`. The target status is normalized by the
-- document (`target_status: disconnected`) and hardcoded here - no
-- extension needed, since anything that is not connected -> disconnected
-- makes no sense (revoked / expired are terminal states we don't move).
--
-- Corresponds to docs/keeper/reaper.md: "last_seen_at older than N + no
-- live stream -> disconnected". This function is the fallback mode
-- (single-instance dev / unit tests without Redis): "no live stream" is
-- checked implicitly - the live stream updates `last_seen_at` via flush
-- from Redis (ADR-006), so a stale `last_seen_at` is equivalent to no live
-- stream on a single instance.
--
-- The lease-aware mode (migration 043, select_disconnect_candidates +
-- mark_disconnected_sids) reworked the rule into two phases cross-checked
-- against the Redis SID lease - closing the deferred slice ADR-006(a).
-- This function is kept as a fallback and is NOT removed.
--
-- last_seen_at IS NULL means never connected (just transitioned
-- pending -> connected without a heartbeat yet). Not matched by the rule,
-- because NULL < NOW() - stale_after = NULL (SQL three-valued logic),
-- predicate is `false`.

CREATE OR REPLACE FUNCTION mark_disconnected(stale_after interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    updated_count BIGINT;
BEGIN
    WITH stale AS (
        SELECT sid
        FROM souls
        WHERE status = 'connected'
          AND last_seen_at < NOW() - stale_after
        ORDER BY last_seen_at
        LIMIT batch_size
    )
    UPDATE souls
       SET status = 'disconnected'
     WHERE sid IN (SELECT sid FROM stale);

    GET DIAGNOSTICS updated_count = ROW_COUNT;
    RETURN updated_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION mark_disconnected(interval, integer) IS
    'Transitions a batch of souls connected -> disconnected if last_seen_at is older than stale_after. Returns the number of updated rows.';
