-- 010_create_expire_pending_seeds.up.sql
--
-- Reaper rule `expire_pending_seeds` (docs/keeper/reaper.md).
-- Semantics - DELETE: a bootstrap token with `used_at IS NULL` and an expired
-- `expires_at` can no longer be used (the check on the Burn side
-- rejects expired tokens), so there's no point keeping it any longer.
--
-- NOTE: at the time Reaper.b was implemented, a PM decision was made
-- to reinterpret the rule as DELETE (rather than UPDATE-with-status):
-- the `bootstrap_tokens` table has no `status` column, and a separate
-- "expired token, but still stored" semantics doesn't exist in the MVP. The audit of
-- token creation lives in `audit_log` under its own retention (ADR-022).
--
-- The `max_age` parameter is currently unused: the criterion is `expires_at < NOW()`,
-- the TTL threshold was fixed at token creation. The parameter is kept in the signature
-- for symmetry with the other rules and the hot-reload convention (if in the
-- future an operator wants to introduce a grace period of "don't delete expired ones
-- immediately, keep them N hours" - that would become `expires_at < NOW() - max_age`).

CREATE OR REPLACE FUNCTION expire_pending_seeds(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT token_id
        FROM bootstrap_tokens
        WHERE used_at IS NULL
          AND expires_at < NOW() - max_age
        ORDER BY expires_at
        LIMIT batch_size
    )
    DELETE FROM bootstrap_tokens
    WHERE token_id IN (SELECT token_id FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION expire_pending_seeds(interval, integer) IS
    'Deletes a batch of unused bootstrap_tokens with an expired expires_at (older than max_age beyond expiry). Returns the number of deleted rows. The Reaper loop calls this repeatedly until it returns 0 (drain pattern).';
