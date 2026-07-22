-- 038_add_plugin_sigils_commit_sha.up.sql
--
-- A1-S3: an audit marker for the origin of the plugin binary (ADR-026(g),
-- docs/keeper/plugins.md -> Integrity model). On a git-verified resolve (Variant A,
-- F-fetch), Keeper resolves `source`+`ref` into a 40-hex `commit_sha` and checks out
-- exactly that commit; `commit_sha` records WHICH git commit the
-- allowed binary was extracted from.
--
-- OUTSIDE THE SIGNATURE. The Sigil's signed block is (namespace, name, ref,
-- binary_sha256, manifest_sha256), DST `soul-stack/sigil/v1` (ADR-026(b)/(c)).
-- `commit_sha` is NOT part of this block and carries NO integrity authority: authority
-- remains with sha256 + the Keeper's signature (invariant (b) is not weakened). This is purely
-- a keeper-side audit field ("origin/readability"), not trust - so it does NOT
-- participate in the verify DTO (shared/pluginhost.SigilRecord) and is NOT carried in
-- the PluginSigil broadcast transport.
--
-- The column is nullable (additive, forward-only ADR-007):
--   - old registry rows (predating this migration) carry no origin -> NULL;
--   - Variant C of allowances (operator-asserted ref, without git-verify) -> NULL = legacy
--     operator-asserted ("binary allowed by a manual marker, git commit unknown").
-- Populated from ResolvedSlot.CommitSHA on a git-verified-allow - slice S4
-- (only the column here, the allow path is untouched). There are no production records.

ALTER TABLE plugin_sigils
    ADD COLUMN commit_sha TEXT;

COMMENT ON COLUMN plugin_sigils.commit_sha IS
    'Git commit from which the allowed binary was resolved (an audit marker of origin, ADR-026(g)). Outside the signed block: integrity authority is sha256 + Keeper signature, not commit_sha. Keeper-audit-only (not in the SigilRecord verify DTO, not in the PluginSigil transport). NULL = legacy operator-asserted (Variant C) or a row predating migration 038.';
