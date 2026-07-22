-- 009_create_soul_seeds.up.sql
--
-- Registry of issued SoulSeed certificates, per docs/soul/identity.md.
-- One SID can have many seeds (rotation history); exactly one active
-- (`status='active'`) at a time is guaranteed by a partial unique index.
--
-- The `status` enum is extended with the `orphaned` value in migration 017
-- (ADR-017 cascade from `core.cloud.provisioned destroyed`: host removed,
-- but revoked semantics must not be overwritten).
--
-- The DB stores no PEM, no private key, no separate public key - only the
-- fingerprint (SHA-256 of the certificate's public key, hex). The primary defense is
-- the CA private key in Vault PKI.
--
-- Push hosts (`transport: ssh`) don't use soul_seeds (no mTLS).
--
-- FK to souls(sid) - ON DELETE CASCADE (seed history dies with the Soul).

CREATE TABLE soul_seeds (
    seed_id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    sid                TEXT        NOT NULL,
    fingerprint        TEXT        NOT NULL,
    serial_number      TEXT        NOT NULL,
    issued_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at         TIMESTAMPTZ NOT NULL,
    issued_by_kid      TEXT,
    status             TEXT        NOT NULL DEFAULT 'active',
    revocation_reason  TEXT,

    CONSTRAINT soul_seeds_sid_fk
        FOREIGN KEY (sid) REFERENCES souls (sid) ON DELETE CASCADE,
    CONSTRAINT soul_seeds_status_valid
        CHECK (status IN ('active', 'superseded', 'expired', 'revoked')),
    CONSTRAINT soul_seeds_fingerprint_format
        -- SHA-256 hex = 64 lower-hex chars.
        CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    CONSTRAINT soul_seeds_expires_after_issued
        CHECK (expires_at > issued_at)
);

-- Invariant: exactly one active seed per sid at a time.
CREATE UNIQUE INDEX soul_seeds_active_by_sid_idx
    ON soul_seeds (sid)
    WHERE status = 'active';

-- mTLS handshake: certificate lookup by fingerprint (for CRL status
-- checks). UNIQUE - the fingerprint must be globally unique (a collision
-- would be a crypto catastrophe; the constraint enforces the invariant explicitly).
CREATE UNIQUE INDEX soul_seeds_fingerprint_idx
    ON soul_seeds (fingerprint);

-- Serial number unique (a CA must never issue two certificates with the same
-- serial - a Vault PKI invariant; we keep the constraint explicit for defense-in-depth).
CREATE UNIQUE INDEX soul_seeds_serial_number_idx
    ON soul_seeds (serial_number);

-- Reaper: superseded/expired seeds older than max_age -> DELETE.
CREATE INDEX soul_seeds_status_idx
    ON soul_seeds (status);

-- Soul-side rotation requests a new seed at `expires_at - 24h`; Reaper
-- moves active -> expired once expires_at is reached.
CREATE INDEX soul_seeds_expires_at_idx
    ON soul_seeds (expires_at);

COMMENT ON TABLE soul_seeds IS
    'History of issued SoulSeed certificates (docs/soul/identity.md). One active per sid.';
