package handlers

import (
	"context"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// secret schema for read-path masking ([ADR-010] §7.4, declarative layer 1).
// incarnation_view masks spec/state/history via [audit.MaskSecretsWithSchema] along
// the paths declared secret:true in the service schema:
//
//   - state — from the flat state_schema manifest (`properties.<field>.secret: true`,
//     recursively through properties/items, and through additionalProperties-NESTED
//     properties); path is `<field>`, an array element is `<field>[].<...>`.
//     ★ secret ON the additionalProperties node ITSELF (the value of an arbitrary map key)
//     is NOT covered by the schema layer (TODO as a separate slice) — degrades to vault+regex
//     (see the limitation in [incarnation.CollectStateSchemaSecrets], which owns the walk —
//     the force-destroy capture needs the same answer, NIM-531);
//   - spec — from the input_schema of scenario `create` (spec carries the operator input
//     under the `input` key); path is `input.<name>` (config.InputSchema.Secret).
//
// Assembly is best-effort: materializing the service snapshot (git) happens NOT on every
// read route, only in [IncarnationHandler.GetTyped]/History (single-incarnation detail
// view). A load/parse error → nil schema → degradation to [audit.MaskSecrets]
// (vault+regex), GET does not fail (observability, not contract).

// secretSchemaForIncarnation materializes the incarnation's service snapshot and builds
// a combined [audit.SecretPathSet] for state (state_schema) + spec (create-scenario
// input_schema). nil → schema unavailable (loader/services nil, load error) — the caller
// degrades to MaskSecrets.
//
// ctx is threaded from the read handler (cancel/timeout of the snapshot materialization).
// The return is an [audit.SecretSchema] interface: for an empty set it returns EXACTLY a
// nil interface (not SecretPathSet(nil)), so the caller's `schema == nil` check tells "no
// schema" from "empty schema" — otherwise a non-nil interface wrapping a nil map would
// engage the schema layer for nothing.
func (h *IncarnationHandler) secretSchemaForIncarnation(ctx context.Context, inc *incarnation.Incarnation) audit.SecretSchema {
	if h.loader == nil || h.services == nil || inc == nil {
		return nil
	}
	ref, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil
	}
	// Pin ref to the incarnation's version (the read path checks the snapshot the
	// incarnation was created/migrated against).
	if inc.ServiceVersion != "" {
		ref.Ref = inc.ServiceVersion
	}
	art, err := h.loader.Load(ctx, ref)
	if err != nil || art == nil {
		return nil
	}

	set := audit.SecretPathSet{}
	if art.Manifest != nil {
		incarnation.CollectStateSchemaSecrets(art.Manifest.StateSchema, "", set)
	}
	collectCreateInputSecrets(h.loader, art, set)
	if len(set) == 0 {
		return nil
	}
	return set
}

// collectCreateInputSecrets reads the snapshot's scenario `create`/main.yml, parses its
// input schema (config.InputSchemaMap) and marks `input.<name>` for secret:true params
// (spec carries the operator input under the `input` key). Best-effort: no create-scenario /
// parse failed → adds nothing.
func collectCreateInputSecrets(loader ServiceSnapshotLoader, art *artifact.ServiceArtifact, set audit.SecretPathSet) {
	data, err := loader.ReadFile(art, "scenario/create/main.yml")
	if err != nil || len(data) == 0 {
		return
	}
	scn, _, sdiags, perr := artifact.LoadScenarioManifestResolved(art, "scenario/create/main.yml", data, nil)
	if perr != nil || scn == nil {
		return
	}
	// fail-closed ONLY on a covenant-merge error (symmetric to scenario.run/validate_input):
	// when the covenant merge fails the merged scn.Input is PARTIAL (covenant fields may not
	// have merged in) — building the secret mask from it is unsafe, or a secret field from the
	// covenant would silently go unmasked. Other schema errors of the create scenario (e.g.
	// `tasks is required` on an incomplete fixture) do NOT truncate input — best-effort secret
	// schema from whatever parsed (GET is not a contract validator, see the package doc comment).
	// Degradation to vault+regex (the caller gets a nil contribution to set) stays only for
	// covenant partiality.
	if hasCovenantMergeError(sdiags) {
		return
	}
	for name, s := range scn.Input {
		if s != nil && s.Secret {
			set["input."+name] = true
		}
	}
}

// covenantMergeErrorCodes are the diagnostic codes of config.ResolveScenarioCovenant/
// MergeCovenant meaning the covenant merge did NOT happen → the merged input is partial
// (covenant fields did not merge in). The secret schema fail-closes ONLY on these (a
// covenant secret field would otherwise leak unmasked). Other scenario schema errors do
// not truncate input. Source of the codes — shared/config/covenant_resolve.go and
// covenant.go (keep in sync: add any new covenant-merge code here too).
var covenantMergeErrorCodes = map[string]bool{
	"covenant_extends_invalid":          true,
	"covenant_extends_target_not_found": true,
	"state_changes_form_mismatch":       true,
	"section_key_conflict":              true,
	"covenant_merge_failed":             true,
	"covenant_unexpected_key":           true,
}

// hasCovenantMergeError reports whether diags contain an error-level covenant-merge code
// (see covenantMergeErrorCodes). True → the merged input is partial, the secret schema
// must not be built from it.
func hasCovenantMergeError(diags []diag.Diagnostic) bool {
	for _, d := range diags {
		if d.Level == diag.LevelError && covenantMergeErrorCodes[d.Code] {
			return true
		}
	}
	return false
}
