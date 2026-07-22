-- 030_add_plugin_sigils_manifest_raw.down.sql
--
-- Rollback of the M1-storage manifest_raw column. The column is additive and nullable,
-- so the rollback is a simple DROP COLUMN: the byte-exact canon for verify is lost (after down
-- S6-verify can only rely on the JSONB projection, which is NOT canon), but the schema
-- reverts to the form of 029.

ALTER TABLE plugin_sigils
    DROP COLUMN IF EXISTS manifest_raw;
