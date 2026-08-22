package incarnation

import (
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The declarative secret layer for an incarnation's `state` ([ADR-010] §7.4,
// layer 1): which dot-paths inside `state` the SERVICE AUTHOR declared secret in
// the manifest's `state_schema` (`properties.<field>.secret: true`).
//
// This lives here, in the package every state-touching operation already imports,
// because more than one surface needs the same answer and the copies do not stay
// equal. The read path (`GET /v1/incarnations/{name}`) had the only walk, private
// to the handlers package; the force-destroy capture could not reach it and
// therefore masked `state` with the vault+regex layers alone, so a key a service
// declared `secret: true` — and nothing else gave away — was masked on the read
// path and printed on the destroy path, into the DELETE reply, the MCP result, a
// WARN line and durable `incarnation_archive.status_details` (NIM-531).
//
// Both callers now walk the same code. Adding a third means calling it, not
// copying it.

// StateSchemaSecrets is the [audit.SecretSchema] for one service artifact's
// `state`. nil when there is no artifact, no manifest, or nothing declared
// secret — callers degrade to [audit.MaskSecrets] (vault+regex) on nil, so an
// unreadable artifact costs the schema layer and never the operation.
//
// EXACTLY a nil interface for an empty set, not a non-nil interface wrapping a
// nil map: callers test `schema == nil` to tell "no schema" from "empty schema",
// and a wrapped nil would engage the schema layer to answer false every time.
func StateSchemaSecrets(art *artifact.ServiceArtifact) audit.SecretSchema {
	if art == nil || art.Manifest == nil {
		return nil
	}
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(art.Manifest.StateSchema, "", set)
	if len(set) == 0 {
		return nil
	}
	return set
}

// CollectStateSchemaSecrets recursively walks the flat JSON-schema state_schema and
// marks in set the dot/idx paths of fields with `secret: true`. Structure:
//   - properties: map<field, schema> → recurse with path = join(path, field);
//   - items: schema → recurse with path = path+"[]" (array element);
//   - additionalProperties: schema → recurse with path = path (WITHOUT a `.*` segment).
//
// Additive into set, so a caller that combines sources (the read path adds the
// create-scenario `input.<name>` secrets to the same set) passes one set through.
//
// ★ Limitation of the schema layer for a secret leaf under additionalProperties
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
func CollectStateSchemaSecrets(schema map[string]any, path string, set audit.SecretPathSet) {
	if schema == nil {
		return
	}
	if isSecretNode(schema) && path != "" {
		set[path] = true
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		for field, sub := range props {
			if subm, ok := sub.(map[string]any); ok {
				CollectStateSchemaSecrets(subm, joinSchemaPath(path, field), set)
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		CollectStateSchemaSecrets(items, path+"[]", set)
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
		CollectStateSchemaSecrets(recurse, path, set)
	}
}

// isSecretNode reports whether the JSON-schema node is a secret leaf: the older
// `secret: true` marker ([ADR-010] §7.4 — the value LIVES in state and is masked on the
// way out), or `type: secret` ([ADR-0083] §1 — the value lives in Vault and never in
// state at all).
//
// The second is belt and braces rather than the mechanism: nothing writes a declared
// secret into state, so the path is normally empty and the mask is inert. It is here so
// that a value arriving there by some other route — an old snapshot, a migration, a bug
// — is masked instead of printed.
func isSecretNode(schema map[string]any) bool {
	if b, _ := schema["secret"].(bool); b {
		return true
	}
	t, _ := schema["type"].(string)
	return t == config.SecretTypeName
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

// joinSchemaPath concatenates a state_schema dot path (BIT-FOR-BIT like audit.joinPath /
// render.joinKey: empty path → field without a leading dot).
func joinSchemaPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}
