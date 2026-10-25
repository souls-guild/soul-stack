-- 127_create_operator_jwts.up.sql
--
-- NIM-1045. Every JWT Keeper mints gets a row, and the row's `jwt_id` is the
-- token's `jti`. Until now a token could only be stopped by revoking its whole
-- operator, permanently; with a row per token one token can be revoked and the
-- operator keeps working with the others. That is what makes a lifetime longer
-- than a day acceptable (`auth.jwt.ttl_max`, ADR-014 amendment NIM-1045).
--
-- `kind` says where the token came from: `bootstrap` (`keeper init`), `issued`
-- (`POST /v1/operators`, `issue-token`), `session` (the cookie of an LDAP/OIDC
-- login; the short Bearer `/auth/token` exchanges it for carries the same
-- `jti` and gets no row of its own).
--
-- The `*_ip` / `*_user_agent` columns are what the request that minted or last
-- used the token said about itself. TEXT, not INET: the peer address is not
-- always an IP (a unix socket, a test listener) and a refused insert would
-- refuse the login. `last_used_*` is written at most once per few minutes per
-- token and per Keeper instance — it answers "is this token still in use",
-- not "what did it do"; the per-request record is the log and the audit trail.
--
-- Revocation is read into the RBAC snapshot: the revoked rows whose token may
-- still verify. The partial index serves exactly that read.

CREATE TABLE operator_jwts (
    jwt_id               TEXT        PRIMARY KEY,
    aid                  TEXT        NOT NULL REFERENCES operators (aid),
    kind                 TEXT        NOT NULL,
    label                TEXT,
    created_at           TIMESTAMPTZ NOT NULL,
    expires_at           TIMESTAMPTZ NOT NULL,
    created_by_aid       TEXT        REFERENCES operators (aid),
    created_ip           TEXT,
    created_user_agent   TEXT,
    last_used_at         TIMESTAMPTZ,
    last_used_ip         TEXT,
    last_used_user_agent TEXT,
    revoked_at           TIMESTAMPTZ,
    revoked_by_aid       TEXT        REFERENCES operators (aid),
    CONSTRAINT operator_jwts_id_format CHECK (jwt_id ~ '^[0-9A-HJKMNP-TV-Z]{26}$'),
    CONSTRAINT operator_jwts_kind_valid CHECK (kind IN ('bootstrap', 'issued', 'session')),
    CONSTRAINT operator_jwts_label_len CHECK (label IS NULL OR char_length(label) <= 200),
    CONSTRAINT operator_jwts_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX operator_jwts_aid_idx ON operator_jwts (aid, created_at DESC);
CREATE INDEX operator_jwts_revoked_idx ON operator_jwts (expires_at) WHERE revoked_at IS NOT NULL;
CREATE INDEX operator_jwts_expires_idx ON operator_jwts (expires_at);

COMMENT ON TABLE operator_jwts IS
    'One row per JWT minted by Keeper (ADR-014 amendment NIM-1045). jwt_id = the token jti; revoked_at refuses that token on the next request.';

-- Retention for the Reaper rule `purge_operator_jwts`: rows whose token expired
-- more than max_age ago. A revoked row is kept as long as an unrevoked one — its
-- token is dead either way once it expires. max_age is floored at one hour: the
-- RBAC snapshot keeps a revoked id five minutes past its expiry to cover clock
-- skew between instances, and a row purged inside that window would un-revoke
-- its token on a lagging instance.
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
