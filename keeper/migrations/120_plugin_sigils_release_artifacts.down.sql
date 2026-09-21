-- 120_plugin_sigils_release_artifacts.down.sql
--
-- Restores the single-digest shape so a rolled-back Keeper can start.
--
-- ⚠ LOSSY, and in the only way that matters here: the table is emptied again.
-- A v3 signature is over a list, so no grant written by the new code verifies
-- against the v2 block the old code builds -- and a release of two platforms
-- has no single-digest form at all. Carrying the first row over would leave a
-- row that looks like an approval, cannot be verified, and quietly names one
-- platform's bytes as if they were the approval.
--
-- Re-issue approvals with `keeper.plugin.allow` on whichever side of the
-- rollback you end up. That is the same instruction migration 115 gave, for the
-- same reason.

DELETE FROM plugin_sigils;

ALTER TABLE plugin_sigils
    DROP CONSTRAINT plugin_sigils_artifacts_shape,
    DROP CONSTRAINT plugin_sigils_kind_allowed;

ALTER TABLE plugin_sigils
    DROP COLUMN artifacts,
    DROP COLUMN kind,
    ADD COLUMN sha256 TEXT NOT NULL;

ALTER TABLE plugin_sigils
    ADD CONSTRAINT plugin_sigils_sha256_format
        -- SHA-256 hex = 64 lower-hex chars.
        CHECK (sha256 ~ '^[0-9a-f]{64}$');

COMMENT ON TABLE plugin_sigils IS
    'Keeper-signed allow-list of approved plugin artifacts (ADR-026, re-keyed by NIM-377). Trust key (source, ref) -> sha256 + schema + Keeper signature; alias is the operator registration and the runtime lookup key, never signed.';

COMMENT ON COLUMN plugin_sigils.commit_sha IS NULL;
