-- 038_add_plugin_sigils_commit_sha.down.sql
--
-- Rollback of the A1-S3 audit column commit_sha. The column is additive and nullable, outside
-- the signature and outside the verify/broadcast path, so the rollback is a simple DROP COLUMN:
-- only the audit provenance marker is lost, the schema reverts to the form of 037.

ALTER TABLE plugin_sigils
    DROP COLUMN IF EXISTS commit_sha;
