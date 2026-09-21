-- 012_create_purge_souls.up.sql
--
-- Reaper rule `purge_souls` (docs/keeper/reaper.md): deletes `souls` records
-- in the given `statuses[]` (default `[disconnected, expired]`)
-- older than `max_age`. Age is measured from `last_seen_at` for
-- Souls that were previously alive; for ones that never connected (last_seen_at IS NULL)
-- - from `registered_at`.
--
-- Parameterizing `statuses[]` is needed because without a status filter
-- `delete` would wipe out live `connected` records (see reaper.md -> the
-- "Required fields" column).
--
-- ON DELETE CASCADE on FK `bootstrap_tokens.sid` and `soul_seeds.sid` (see
-- 008/009) - deleting a Soul automatically cleans up its related tokens and
-- certificate records. This is a deliberate choice: token/seed history
-- dies with the Soul, same as state_history vs incarnation.

CREATE OR REPLACE FUNCTION purge_souls(
    target_statuses text[],
    max_age         interval,
    batch_size      integer DEFAULT 1000
)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT sid
        FROM souls
        WHERE status = ANY(target_statuses)
          AND COALESCE(last_seen_at, registered_at) < NOW() - max_age
        ORDER BY COALESCE(last_seen_at, registered_at)
        LIMIT batch_size
    )
    DELETE FROM souls
    WHERE sid IN (SELECT sid FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_souls(text[], interval, integer) IS
    'Deletes a batch of souls in the given statuses with COALESCE(last_seen_at, registered_at) older than max_age. Returns the number of deleted rows. CASCADEs to bootstrap_tokens/soul_seeds.';
