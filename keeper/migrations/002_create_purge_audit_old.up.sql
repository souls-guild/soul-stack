-- 002_create_purge_audit_old.up.sql
--
-- ADR-022(d): retention for audit_log via the Reaper rule
-- `purge_audit_old` (see docs/keeper/reaper.md). The PL/pgSQL function
-- deletes one batch of expired rows and returns the number of deleted
-- rows; the Reaper loop (M0.6) calls it in a loop until it returns 0
-- (drain pattern). Batch DELETE reduces lock contention compared to a
-- whole-table DELETE.
--
-- `max_age` comes from the config (`keeper.yml -> reaper.rules.purge_audit_old.max_age`,
-- aliased to `audit.retention_days`); parameterizing on the function side
-- instead of a literal in SQL lets the caller (reaper-runner) freely rotate
-- the value without re-creating the function.

CREATE OR REPLACE FUNCTION purge_audit_old(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT audit_id
        FROM audit_log
        WHERE created_at < NOW() - max_age
        ORDER BY created_at
        LIMIT batch_size
    )
    DELETE FROM audit_log
    WHERE audit_id IN (SELECT audit_id FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_audit_old(interval, integer) IS
    'Deletes a batch of expired audit_log rows (created_at < NOW() - max_age). Returns the number of deleted rows. The Reaper loop calls it in a loop until it returns 0 (drain pattern).';
