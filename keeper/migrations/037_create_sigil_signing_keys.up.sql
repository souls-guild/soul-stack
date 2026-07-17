-- 037_create_sigil_signing_keys.up.sql
--
-- Registry of Sigil signing trust-anchor keys (ADR-026(h), R3 multi-anchor).
-- Before R3, Sigil signing was done by a SINGLE ed25519 key from Vault KV (following the
-- jwt-signing-key pattern, ADR-014). Multi-anchor introduces a set of keys: exactly one
-- `primary` (which Keeper uses to sign new Sigils) and any number of other
-- `active` keys (which Soul still uses to validate previously signed material) - this gives
-- seamless rotation: a new key is introduced as active, becomes primary,
-- the old one finishes out its life as active and then retired (ADR-026(h), replace semantics
-- of SigilTrustAnchors on the Soul side).
--
-- Security invariant: the PRIVATE KEY is NEVER in Postgres. The table holds only
-- the public part (pubkey_pem, SPKI PEM) for distribution to Soul as a trust-anchor, and
-- a vault_ref pointing to the private key in Vault KV (root of trust - Vault, ADR-026(d)).
--
-- key_id - stable key identifier (SHA-256 of SPKI-DER, hex). Does not
-- depend on the PEM string (whitespace/line breaks): the same key always
-- yields the same key_id. UNIQUE - re-introducing the same key is rejected on INSERT.
--
-- status - key lifecycle: active (valid for verify; exactly one of the active keys is
-- primary) → retired (retired; Soul forgets it on the next
-- SigilTrustAnchors). Forward-only: retired never goes back to active
-- (re-introduce = a new INSERT).
--
-- FK to operators(aid) - both ON DELETE SET NULL: the history of introducing/retiring a key
-- survives deletion of the operator (symmetric with audit_log / plugin_sigils.revoked).

CREATE TABLE sigil_signing_keys (
    id                BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    key_id            TEXT        NOT NULL UNIQUE,        -- stable id: SHA-256 of SPKI-DER, hex
    pubkey_pem        TEXT        NOT NULL,               -- ONLY the public part (SPKI PEM); private key NEVER in PG
    vault_ref         TEXT        NOT NULL,               -- reference to the private key in Vault KV
    is_primary        BOOLEAN     NOT NULL DEFAULT false,
    status            TEXT        NOT NULL DEFAULT 'active',  -- active | retired
    introduced_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    introduced_by_aid TEXT,
    retired_at        TIMESTAMPTZ,
    retired_by_aid    TEXT,

    CONSTRAINT sigil_signing_keys_status_enum
        CHECK (status IN ('active', 'retired')),
    CONSTRAINT sigil_signing_keys_introduced_by_fk
        FOREIGN KEY (introduced_by_aid) REFERENCES operators (aid) ON DELETE SET NULL,
    CONSTRAINT sigil_signing_keys_retired_by_fk
        FOREIGN KEY (retired_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Invariant: exactly one primary AMONG ACTIVE keys. Partial unique on
-- (is_primary) where status='active' AND is_primary - allows no more than one
-- row satisfying the predicate (two active-primary → 23505). Precedent
-- for this style - operators_first_archon_idx (003), bootstrap_tokens_active (008).
CREATE UNIQUE INDEX sigil_signing_keys_one_primary
    ON sigil_signing_keys (is_primary)
    WHERE status = 'active' AND is_primary;

COMMENT ON TABLE sigil_signing_keys IS
    'Sigil signing trust-anchor keys (ADR-026(h), multi-anchor). ONLY pubkey_pem + vault_ref; private key stays in Vault. Exactly one primary among active.';
