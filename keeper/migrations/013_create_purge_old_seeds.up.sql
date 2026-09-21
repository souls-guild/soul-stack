-- 013_create_purge_old_seeds.up.sql
--
-- Reaper rule `purge_old_seeds` (docs/keeper/reaper.md): deletes a
-- batch of `soul_seeds` records in the given `statuses[]` (default
-- `[superseded, expired, revoked]`) with `issued_at` older than `max_age`.
--
-- Active seeds are NOT deleted by this rule (the statuses filter
-- excludes them, see reaper.md). An active seed can only "die" via
-- rotation (status -> superseded) or revoke (status -> revoked) - both
-- scenarios are governed by the soul-side / Operator API, not the Reaper.
--
-- Age is counted from `issued_at`, not from the transition into the
-- current status. This is a deliberate choice: certificate history is
-- measured by lifetime, not by the remaining term after rotation/revocation.

CREATE OR REPLACE FUNCTION purge_old_seeds(
    target_statuses text[],
    max_age         interval,
    batch_size      integer DEFAULT 1000
)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT seed_id
        FROM soul_seeds
        WHERE status = ANY(target_statuses)
          AND issued_at < NOW() - max_age
        ORDER BY issued_at
        LIMIT batch_size
    )
    DELETE FROM soul_seeds
    WHERE seed_id IN (SELECT seed_id FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_old_seeds(text[], interval, integer) IS
    'Deletes a batch of soul_seeds in the specified statuses with issued_at older than max_age. Returns the number of deleted rows.';
