-- 093_create_purge_old_certs.up.sql
--
-- Reaper rule `purge_old_certs` (R4, cert-rotation Option 1): deletes a batch
-- of `warrant` rows in the given `statuses[]` (default `[superseded, expired,
-- failed]`) with `issued_at` older than `max_age`. Retention for the growing
-- history of service cert rotations (the warrant registry bloats with superseded
-- rows).
--
-- Active/rotating are NOT deleted (the statuses filter excludes them): active is
-- live material, rotating is a cert in the process of rotation (the Reaper rule
-- rotate_due_certs has claimed it via CAS). Age is counted from `issued_at` (how
-- long the row has existed), symmetric with purge_old_seeds (013).
--
-- Parity with purge_old_seeds: same signature (statuses[], max_age, batch_size),
-- same batch pattern (LIMIT + DELETE ... WHERE cert_id IN).

CREATE OR REPLACE FUNCTION purge_old_certs(
    target_statuses text[],
    max_age         interval,
    batch_size      integer DEFAULT 1000
)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH old AS (
        SELECT cert_id
        FROM warrant
        WHERE status = ANY(target_statuses)
          AND issued_at < NOW() - max_age
        ORDER BY issued_at
        LIMIT batch_size
    )
    DELETE FROM warrant
    WHERE cert_id IN (SELECT cert_id FROM old);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_old_certs(text[], interval, integer) IS
    'Deletes a batch of warrant rows in the given statuses (superseded/expired/failed) with issued_at older than max_age. Returns the number of deleted rows.';
