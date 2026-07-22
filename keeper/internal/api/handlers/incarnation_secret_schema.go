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
//     (see the limitation in collectStateSchemaSecrets);
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
		collectStateSchemaSecrets(art.Manifest.StateSchema, "", set)
	}
	collectCreateInputSecrets(h.loader, art, set)
	if len(set) == 0 {
		return nil
	}
	return set
}

// collectStateSchemaSecrets recursively walks the flat JSON-schema state_schema and
// marks in set the dot/idx paths of fields with `secret: true`. Structure:
//   - properties: map<field, schema> → recurse with path = join(path, field);
//   - items: schema → recurse with path = path+"[]" (array element);
//   - additionalProperties: schema → recurse with path = path (WITHOUT a `.*` segment).
//
// ★ Limitation of the read-path schema layer for a secret leaf under additionalProperties
// (TODO as a separate slice): when `secret: true` sits on the ap node ITSELF (the value of
// any map key is secret), the schema layer does NOT cover it. [audit.SecretPathSet.IsSecret]
// checks the requested path as-is or via normalizeIdx (concrete indices → `[]`) — but NEVER
// substitutes a `.*` segment; an entry like `path.*` in the set would match no real request
// (dead). So marking an ap secret leaf is pointless — it degrades to the vault+regex masking
// layer (MaskSecrets). We DO recurse (an ap may hold nested `properties` with concrete names —
// the schema layer covers their paths), but skip the secret ON the ap node itself.
//
// ★ Related gap (a NESTED secret under ap keyed by a DYNAMIC key, seal-review nit, NOT fixed —
// the fix = dynamic-key matching in IsSecret, a separate slice): recursion into ap does NOT add
// a segment for the arbitrary key, so
// `users:{additionalProperties:{properties:{password:{secret}}}}` collects the path
// `users.password`. The real payload (incarnation.state) carries a CONCRETE map key —
// `{users:{alice:{password:…}}}` → maskMapLayered builds the cell path `users.alice.password`.
// [audit.SecretPathSet.IsSecret] checks `users.alice.password` AND normalizeIdx(the same) — but
// normalizeIdx generalizes ONLY slice indices (`[N]`→`[]`), it leaves the map key `alice` alone
// → no form matches the collected `users.password`. The schema layer does NOT mask such a secret;
// it degrades to vault+regex (the `password` key is caught by the sensitive-by-name regex last
// resort — an alarm fallback, not schema). The current behavior is pinned by the test
// TestCollectStateSchemaSecrets_AdditionalPropertiesNestedSecret_DynamicKeyGap.
func collectStateSchemaSecrets(schema map[string]any, path string, set audit.SecretPathSet) {
	if schema == nil {
		return
	}
	if isSecretNode(schema) && path != "" {
		set[path] = true
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		for field, sub := range props {
			if subm, ok := sub.(map[string]any); ok {
				collectStateSchemaSecrets(subm, joinSchemaPath(path, field), set)
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		collectStateSchemaSecrets(items, path+"[]", set)
	}
	if ap, ok := schema["additionalProperties"].(map[string]any); ok {
		// Do not mark secret ON the ap node itself (see the ★ limitation in the doc comment:
		// the path `map_field` would mark the WHOLE map as secret — over-mask on the read path;
		// and IsSecret never requests `map_field.*` → the entry is dead. Degradation to
		// vault+regex is intentional). We recurse, but WITHOUT the secret flag of the ap node
		// itself: the schema layer covers nested concrete `properties` inside ap by exact path,
		// while we clear the secret ON the ap node so isSecretNode does not mark path.
		recurse := ap
		if isSecretNode(ap) {
			recurse = mapWithoutSecret(ap)
		}
		collectStateSchemaSecrets(recurse, path, set)
	}
}

// isSecretNode reports whether the JSON-schema node carries `secret: true`.
func isSecretNode(schema map[string]any) bool {
	b, _ := schema["secret"].(bool)
	return b
}

// mapWithoutSecret is a shallow copy of a schema node without the `secret` key, so
// recursion over additionalProperties does not mark the ap node itself as a secret leaf
// (its path = the map name, marking it would over-mask the whole map). Nested
// `properties`/`items` copies are untouched — recursion over them follows their exact paths.
func mapWithoutSecret(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		if k == "secret" {
			continue
		}
		out[k] = v
	}
	return out
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
	scn, _, sdiags, perr := artifact.LoadScenarioManifestResolved(art, "scenario/create/main.yml", data)
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

// joinSchemaPath concatenates a state_schema dot path (BIT-FOR-BIT like audit.joinPath /
// render.joinKey: empty path → field without a leading dot).
func joinSchemaPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}
