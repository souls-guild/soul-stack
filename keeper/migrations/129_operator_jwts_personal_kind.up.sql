-- 129_operator_jwts_personal_kind.up.sql
--
-- NIM-1052. A fourth `operator_jwts.kind`: `personal` — a token an operator
-- issued to itself through `POST /v1/me/jwts` (ADR-014 amendment NIM-1052).
-- `created_by_aid` of such a row is the owner.
--
-- The Reaper keeps a personal row at least 24 hours after it was created, so the
-- limit on personal tokens issued in 24 hours counts every one of them: with a
-- short `max_age` an issue-then-revoke loop of one-minute tokens would otherwise
-- see its rows purged within the hour and its count drop back.

ALTER TABLE operator_jwts DROP CONSTRAINT operator_jwts_kind_valid;
ALTER TABLE operator_jwts ADD CONSTRAINT operator_jwts_kind_valid
    CHECK (kind IN ('bootstrap', 'issued', 'session', 'personal'));

CREATE OR REPLACE FUNCTION purge_operator_jwts(max_age interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT jwt_id
        FROM operator_jwts
        WHERE expires_at < NOW() - GREATEST(max_age, INTERVAL '1 hour')
          AND (kind <> 'personal' OR created_at < NOW() - INTERVAL '24 hours')
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
    'Deletes a batch of operator_jwts rows whose token expired more than max_age (at least one hour) ago; a personal row also waits until it is 24 hours old. Returns the number of deleted rows. The Reaper loop calls it until 0 is returned (drain pattern).';
