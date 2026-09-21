-- 008_create_bootstrap_tokens.up.sql
--
-- Registry of one-time SoulSeed tokens per docs/soul/onboarding.md.
-- One SID has one active (`used_at IS NULL`) token at a time
-- (partial unique index). After use the record stays in the table
-- for audit purposes; the Reaper later picks it up via the `purge_used_tokens`
-- rule with `max_age: 90d` from `used_at`.
--
-- The plain token itself is not stored in the DB - only `token_hash` (SHA-256, hex,
-- no salt - the token is itself high-entropy). The plain value lives only in the
-- bootstrap API response to the operator and in a file on the Soul's host.
--
-- For push hosts (`transport: ssh`) no records are created in bootstrap_tokens.
--
-- FK to souls(sid) - ON DELETE CASCADE: deleting a Soul deletes its related
-- tokens (token history dies with the Soul, same as state_history vs
-- incarnation).

CREATE TABLE bootstrap_tokens (
    token_id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    sid             TEXT        NOT NULL,
    token_hash      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    used_at         TIMESTAMPTZ,
    used_by_kid     TEXT,
    created_by_aid  TEXT,

    CONSTRAINT bootstrap_tokens_sid_fk
        FOREIGN KEY (sid) REFERENCES souls (sid) ON DELETE CASCADE,
    CONSTRAINT bootstrap_tokens_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL,
    CONSTRAINT bootstrap_tokens_expires_after_created
        CHECK (expires_at > created_at),
    CONSTRAINT bootstrap_tokens_token_hash_format
        -- SHA-256 hex = 64 lower-hex chars.
        CHECK (token_hash ~ '^[0-9a-f]{64}$')
);

-- Invariant: one sid has one active (unused) token.
CREATE UNIQUE INDEX bootstrap_tokens_active_by_sid_idx
    ON bootstrap_tokens (sid)
    WHERE used_at IS NULL;

-- Lookup by token_hash on presentation (UNIQUE - guarantee that one
-- hash is not attached to more than one SID; high-entropy rules out collision
-- de facto, but the constraint enforces the invariant explicitly).
CREATE UNIQUE INDEX bootstrap_tokens_token_hash_idx
    ON bootstrap_tokens (token_hash);

-- Reaper: used tokens older than max_age → DELETE.
CREATE INDEX bootstrap_tokens_used_at_idx
    ON bootstrap_tokens (used_at)
    WHERE used_at IS NOT NULL;

-- Reaper: pending tokens older than expires_at → DELETE + souls.status = expired.
CREATE INDEX bootstrap_tokens_expires_at_idx
    ON bootstrap_tokens (expires_at)
    WHERE used_at IS NULL;

COMMENT ON TABLE bootstrap_tokens IS
    'One-time SoulSeed tokens (docs/soul/onboarding.md). Active = used_at IS NULL.';
