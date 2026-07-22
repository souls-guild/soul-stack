-- 007_create_souls.up.sql
--
-- Registry of Soul agents under ADR-002 / ADR-012 + docs/soul/identity.md.
-- PK is `sid` (= host FQDN), `transport` is an enum (`agent` | `ssh`),
-- `status` is a narrow MVP enum (`pending` | `connected` | `disconnected` |
-- `revoked` | `expired`). Extended with the `destroyed` value in migration 016
-- (ADR-017 cascade from `core.cloud.provisioned destroyed`).
--
-- Real pull/push flow:
--   * `pending` - operator issued a bootstrap token, Soul hasn't connected yet.
--   * `connected` - stream is alive, Keeper holds a lease in Redis.
--   * `disconnected` - stream closed, lease expired.
--   * `revoked` - operator revoked it, new connections are rejected at the mTLS level.
--   * `expired` - Reaper moved pending -> expired after the bootstrap token TTL.
--   * `destroyed` - added by migration 016: terminal state after cloud-destroy.
--
-- FK `created_by_aid` -> operators(aid) (ADR-014). ON DELETE SET NULL -
-- Soul history matters more than referential integrity (revoking an operator
-- must not wipe out the Souls registry).
--
-- `coven` is `text[]` (multiple stable labels, ADR-008).
-- `last_seen_at` in PG is a flush from Redis (the live value lives in the Redis cache).

CREATE TABLE souls (
    sid                TEXT        PRIMARY KEY,
    transport          TEXT        NOT NULL DEFAULT 'agent',
    status             TEXT        NOT NULL DEFAULT 'pending',
    coven              TEXT[]      NOT NULL DEFAULT ARRAY[]::TEXT[],
    registered_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at       TIMESTAMPTZ,
    last_seen_by_kid   TEXT,
    created_by_aid     TEXT,
    requested_at       TIMESTAMPTZ,
    note               TEXT,

    CONSTRAINT souls_sid_format
        CHECK (sid ~ '^[a-z0-9][a-z0-9.-]{0,253}$'),
    CONSTRAINT souls_transport_valid
        CHECK (transport IN ('agent', 'ssh')),
    CONSTRAINT souls_status_valid
        CHECK (status IN ('pending', 'connected', 'disconnected', 'revoked', 'expired')),
    CONSTRAINT souls_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Typical Reaper / Operator API query - "all pending older than X" or
-- "all connected for the health overview".
CREATE INDEX souls_status_idx
    ON souls (status);

-- Supports targeting by coven labels (ADR-008, scenario `on:`).
-- GIN index on text[] - the standard path for `coven && ARRAY['db','prod']`.
CREATE INDEX souls_coven_idx
    ON souls USING GIN (coven);

-- For Reaper: pending Souls older than the bootstrap token TTL -> expired.
CREATE INDEX souls_pending_requested_at_idx
    ON souls (requested_at)
    WHERE status = 'pending';

COMMENT ON TABLE souls IS
    'Registry of Soul agents (ADR-002 / ADR-012). PK = sid (FQDN), coven - text[] labels.';
