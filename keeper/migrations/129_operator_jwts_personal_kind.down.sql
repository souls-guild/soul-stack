-- 129_operator_jwts_personal_kind.down.sql
--
-- Personal rows become `issued` instead of being deleted: a deleted revoked row
-- would drop its id from the RBAC snapshot's revoked projection, and the token
-- it stopped would verify again until it expires. The purge function returns to
-- its migration-127 body.

UPDATE operator_jwts SET kind = 'issued' WHERE kind = 'personal';
ALTER TABLE operator_jwts DROP CONSTRAINT operator_jwts_kind_valid;
ALTER TABLE operator_jwts ADD CONSTRAINT operator_jwts_kind_valid
    CHECK (kind IN ('bootstrap', 'issued', 'session'));

CREATE OR REPLACE FUNCTION purge_operator_jwts(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT jwt_id
        FROM operator_jwts
        WHERE expires_at < NOW() - GREATEST(max_age, INTERVAL '1 hour')
        ORDER BY expires_at
        LIMIT batch_size
    )
    DELETE FROM operator_jwts
    WHERE jwt_id IN (SELECT jwt_id FROM expired);

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_operator_jwts(interval, integer) IS
    'Deletes a batch of operator_jwts rows whose token expired more than max_age (at least one hour) ago. Returns the number of deleted rows. The Reaper loop calls it until 0 is returned (drain pattern).';
