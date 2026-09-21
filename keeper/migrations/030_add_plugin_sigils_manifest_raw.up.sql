-- 030_add_plugin_sigils_manifest_raw.up.sql
--
-- M1-storage fix: persist the byte-exact RAW bytes of manifest.yaml that
-- Keeper signs with the Sigil (ADR-026, docs/keeper/plugins.md -> Integrity
-- model). Before this column, Allow signed the raw slot.ManifestBytes, but
-- stored only the JSONB projection (manifestYAMLToJSON) in the registry,
-- discarding the raw bytes - so S6-sender / S6b-verify could not obtain
-- EXACTLY the signed bytes for PluginSigil.manifest (broadcast).
--
-- manifest_raw is the CANON for verify/broadcast: the same bytes that went
-- through NormalizeManifestBytes at signing time (S3<->S6 invariant) travel
-- into PluginSigil.manifest as-is. The manifest column (jsonb, migration 028)
-- is a DERIVED projection for query/audit (searching by side_effects /
-- capabilities, displaying in the UI), NOT canon: Normalize("{}") !=
-- Normalize(""), and the JSONB round-trip does not preserve the bytes.
--
-- The column is nullable (additive, forward-only per ADR-007): existing
-- registry rows carry no raw data -> NULL. The new Allow path requires a
-- non-NULL manifest_raw - that is a code invariant (an insert guard: empty
-- ManifestRaw is a caller bug, a root-of-trust issue), not a schema one; at
-- the DDL level the column stays nullable so migrating old rows succeeds
-- without errors.

ALTER TABLE plugin_sigils
    ADD COLUMN manifest_raw BYTEA;

COMMENT ON COLUMN plugin_sigils.manifest_raw IS
    'Byte-exact RAW bytes of manifest.yaml signed by Keeper (CANON for S6-verify/broadcast, travels into PluginSigil.manifest). The manifest column (jsonb) is a derived query/audit projection, NOT canon. NULL for rows created before migration 030.';
