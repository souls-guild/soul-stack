-- 011_create_purge_used_tokens.up.sql
--
-- Reaper rule `purge_used_tokens` (docs/keeper/reaper.md):
-- deletes a batch of used bootstrap tokens older than max_age by `used_at`.
-- A used token (`used_at IS NOT NULL`) no longer serves a protective
-- function; long-term audit of creation/use is stored in
-- `audit_log` under its own retention (ADR-022).
--
-- Default `max_age` in the config is 90d (docs/keeper/reaper.md).

CREATE OR REPLACE FUNCTION purge_used_tokens(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT token_id
        FROM bootstrap_tokens
        WHERE used_at IS NOT NULL
          AND used_at < NOW() - max_age
        ORDER BY used_at
        LIMIT batch_size
    )
    DELETE FROM bootstrap_tokens
    WHERE token_id IN (SELECT token_id FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_used_tokens(interval, integer) IS
    'Deletes a batch of used bootstrap_tokens with used_at older than max_age. Returns the number of deleted rows. The Reaper loop calls it in a cycle until 0 is returned (drain pattern).';
