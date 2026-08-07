-- 115_plugin_sigils_source_identity.down.sql
--
-- Restores the pre-NIM-377 SHAPE so a rolled-back keeper can start, and nothing
-- else. The data does not come back and cannot be recomputed: `namespace` and
-- `name` were read from a manifest.yaml that artifacts no longer carry, and the
-- rows that held them were already emptied by the up-migration because their
-- v1-DST signatures do not verify against the v2 block.
--
-- A rollback across this migration therefore lands on an EMPTY allow-list, which
-- is the fail-closed direction: no plugin is granted until an operator approves
-- one again with the rolled-back binary. Pair the rollback with re-issuing the
-- grants, not with an expectation that the old ones reappear.
--
-- The rows are emptied here too. Going back, `alias` and `source` disappear while
-- `namespace`/`name` come back NOT NULL with no value to fill them, and a v2
-- signature would not verify against the v1 block the rolled-back code builds --
-- so any surviving row would be an unverifiable grant that still reads as active.

DELETE FROM plugin_sigils;

DROP INDEX IF EXISTS plugin_sigils_active_alias_idx;
DROP INDEX IF EXISTS plugin_sigils_active_idx;

ALTER TABLE plugin_sigils
    DROP CONSTRAINT IF EXISTS plugin_sigils_alias_format;

ALTER TABLE plugin_sigils
    RENAME COLUMN schema TO manifest_raw;

ALTER TABLE plugin_sigils
    ALTER COLUMN manifest_raw DROP NOT NULL;

ALTER TABLE plugin_sigils
    DROP COLUMN alias,
    DROP COLUMN source,
    ADD COLUMN namespace TEXT  NOT NULL,
    ADD COLUMN name      TEXT  NOT NULL,
    ADD COLUMN manifest  JSONB NOT NULL;

CREATE UNIQUE INDEX plugin_sigils_active_idx
    ON plugin_sigils (namespace, name, ref)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE plugin_sigils IS
    'Keeper-signed allow-list of approved plugins (ADR-026). Key (namespace, name, ref) -> sha256 + Keeper signature. Replaces TOFU.';
