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

// CollectStateSchemaSecrets recursively walks the state_schema — since [NIM-740] the
// input dialect, a map of state field → schema — and marks in set the dot/idx paths of
// fields with `secret: true`. Structure:
//   - the map itself, and every `properties:` inside it → recurse with
//     path = join(path, field);
//   - items: schema → recurse with path = path+"[]" (array element);
//   - additional_properties: schema → recurse with path = path (WITHOUT a `.*` segment).
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
func CollectStateSchemaSecrets(schema config.InputSchemaMap, path string, set audit.SecretPathSet) {
	for field, sub := range schema {
		collectStateSchemaSecretsNode(sub, joinSchemaPath(path, field), set, false)
	}
}

// collectStateSchemaSecretsNode walks one schema node. apNode says the node is an
// `additional_properties` value, which changes ONE thing: its `secret: true` flag is
// ignored. Its path is the MAP's, so honouring the flag would mark the whole map
// secret — an over-mask on the read path, and a dead entry besides, since IsSecret
// never requests `map_field.*` (see the ★ limitation in the doc comment above; the
// degradation to vault+regex is intentional).
//
// `type: secret` on such a node is still marked, exactly as before the schema became
// typed: the older code stripped the `secret` KEY and left the type, so the path was
// entered. That branch is unreachable through a loaded manifest —
// [config.CollectSecretFields] refuses a declared secret in that position — and the
// mask is belt and braces either way, which is precisely why it should not quietly
// narrow.
//
// Everything nested inside is still walked, because a concrete `properties` name under
// it does have an exact path.
func collectStateSchemaSecretsNode(s *config.InputSchema, path string, set audit.SecretPathSet, apNode bool) {
	if s == nil {
		return
	}
	if path != "" && isSecretNode(s, apNode) {
		set[path] = true
	}
	CollectStateSchemaSecrets(s.Properties, path, set)
	collectStateSchemaSecretsNode(s.Items, path+"[]", set, false)
	if ap, ok := s.AdditionalProperties.(*config.InputSchema); ok {
		collectStateSchemaSecretsNode(ap, path, set, true)
	}
}

// isSecretNode reports whether the schema node is a secret leaf: the older
// `secret: true` marker ([ADR-010] §7.4 — the value LIVES in state and is masked on the
// way out), or `type: secret` ([ADR-0083] §1 — the value lives in Vault and never in
// state at all). ignoreFlag drops the first of the two (see the caller).
//
// The second is belt and braces rather than the mechanism: nothing writes a declared
// secret into state, so the path is normally empty and the mask is inert. It is here so
// that a value arriving there by some other route — an old snapshot, a migration, a bug
// — is masked instead of printed.
func isSecretNode(s *config.InputSchema, ignoreFlag bool) bool {
	return (s.Secret && !ignoreFlag) || s.Type == config.SecretTypeName
}

// joinSchemaPath concatenates a state_schema dot path (BIT-FOR-BIT like audit.joinPath /
// render.joinKey: empty path → field without a leading dot).
func joinSchemaPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}
