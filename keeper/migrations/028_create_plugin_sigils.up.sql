-- 028_create_plugin_sigils.up.sql
--
-- Sigil registry - a Keeper-signed allow-list of approved plugin binaries
-- (ADR-026, docs/keeper/plugins.md -> Integrity model). Replaces TOFU
-- semantics ("the host decides on its own whether to trust") with an
-- "authoritative list maintained by the Keeper": a record appears only
-- when an Archon EXPLICITLY allows a plugin via OpenAPI/MCP.
--
-- Plugin identity is a triple (namespace, name, ref):
--   - namespace - plugin type: cloud / ssh / mod;
--   - name      - binary name (soul-cloud-hetzner and so on);
--   - ref       - version. In MVP (ADR-026(g), Variant C) this is an
--                 operator-asserted LABEL (typically a git-tag per ADR-007),
--                 NOT a git-verified ref: on allow, the Keeper reads the
--                 current binary from the single-slot cache by
--                 (namespace, name) - ref does not participate in the read -
--                 and does NOT verify that the binary was built from this
--                 ref. Integrity authority is sha256 + signature below, not
--                 ref. A git-verified ref is post-MVP (requires a
--                 ref-aware cache layout).
-- Uniqueness - partial unique over active records: at most one ACTIVE
-- (revoked_at IS NULL) record per (namespace, name, ref). History of
-- revoked approvals is preserved, so re-allow after revoke is a clean
-- INSERT of a new record (the audit trail of sha256/signature/allowed_by
-- is never overwritten by an UPDATE).
-- Style precedent - bootstrap_tokens_active_by_sid_idx (migration 008).
-- The verify-path lookup goes by this triple and is covered by the
-- active index.
--
-- sha256 - fingerprint of the approved binary (hex, lowercase, 64
-- characters). Together with manifest it makes up the signable block of
-- the Sigil (ADR-026(b)/(c)): the signature covers the manifest with the
-- digest stitched in, so declared side_effects / capabilities cannot be
-- forged.
--
-- signature - the Keeper's signature over the signable block. Type BYTEA:
-- an ed25519/ECDSA signature is raw binary bytes of fixed (ed25519, 64
-- bytes) or near-fixed length; BYTEA stores them without the overhead of
-- base64 encoding and without risking encoding desync on the verify path.
-- The detailed format of the signable block is slice S3 (only the column
-- lives here).
--
-- Approval lifecycle: allowed (allowed_by_aid / allowed_at) -> optionally
-- revoked (revoked_at / revoked_by_aid). Revocation is soft (the record
-- stays for audit purposes, NOT NULL revoked_at = revoked).
--
-- FK to operators(aid):
--   - allowed_by_aid (NOT NULL) - no ON DELETE: default NO ACTION
--     (effectively RESTRICT in this case). An operator holding an active
--     approval cannot be deleted - otherwise the author of the trust
--     record would be lost (a security invariant; SET NULL is impossible
--     because of NOT NULL).
--   - revoked_by_aid (NULL)     - ON DELETE SET NULL: revocation history
--     survives operator deletion (symmetric with audit_log / bootstrap_tokens).

CREATE TABLE plugin_sigils (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    namespace       TEXT        NOT NULL,
    name            TEXT        NOT NULL,
    ref             TEXT        NOT NULL,
    sha256          TEXT        NOT NULL,
    signature       BYTEA       NOT NULL,
    manifest        JSONB       NOT NULL,
    allowed_by_aid  TEXT        NOT NULL,
    allowed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at      TIMESTAMPTZ,
    revoked_by_aid  TEXT,

    CONSTRAINT plugin_sigils_sha256_format
        -- SHA-256 hex = 64 lower-hex chars.
        CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT plugin_sigils_allowed_by_aid_fk
        FOREIGN KEY (allowed_by_aid) REFERENCES operators (aid),
    CONSTRAINT plugin_sigils_revoked_by_aid_fk
        FOREIGN KEY (revoked_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Feed of a specific operator's Sigils (audit / triage of "what has this Archon allowed").
CREATE INDEX plugin_sigils_allowed_by_aid_idx
    ON plugin_sigils (allowed_by_aid);

-- Invariant: at most one active (non-revoked) record per
-- (namespace, name, ref). This same index covers the scan of active
-- approvals - the typical verify-/list-path query. Precedent -
-- bootstrap_tokens (migration 008).
CREATE UNIQUE INDEX plugin_sigils_active_idx
    ON plugin_sigils (namespace, name, ref)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE plugin_sigils IS
    'Keeper-signed allow-list of approved plugins (ADR-026). Key (namespace, name, ref) -> sha256 + Keeper signature. Replaces TOFU.';
