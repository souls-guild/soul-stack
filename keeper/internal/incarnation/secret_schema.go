package incarnation

import (
	"strings"

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
// equal. The read path (`GET /v1/incarnations/{id}`) had the only walk, private
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

// StateSchemaSecretFields — the TOP-LEVEL `state` field names whose value LIVES in
// state, for the render seal ([ADR-010] §7.4, NIM-826): the address
// `${ incarnation.state.<field> }` reads is spelled by cel.FieldAddr over
// cel.IncarnationStateAddrRoot, and CEL can only address the top segment of a
// nested path. So `tls.key` contributes `tls`, and every read under
// `incarnation.state.tls` is sealed — the subtree rule the rest of the seal
// already follows.
//
// ★ [sealAddressable], not every declared secret, and the rule is about the
// ADDRESS rather than the marker: a declaration is taken when the address the seal
// can spell is no WIDER than the declaration itself. That is the one thing
// distinguishing this from [StateSchemaSecrets], which answers the masking question
// and can afford every path because it addresses each one exactly.
//
//   - `secret: true` at any depth — always taken. The value is genuinely in the
//     record ([ADR-010] §7.4), so a CEL read takes home plaintext; folding
//     `tls.key` to `tls` over-seals a subtree and that is the accepted direction,
//     because the alternative is a credential in a durable column.
//   - `type: secret` — taken only where the fold is the identity. Two positions are
//     legal ([config.CollectSecretFields]): a top-level scalar, and a property of a
//     top-level array's items. The second folds to the whole collection —
//     `redis_users` in the example service, an ACL inventory that is public and read
//     by every day-2 scenario — so taking it masks live diagnostics out of every run
//     plan the service writes, for a value that is not supposed to be there. The
//     first folds to itself: exact, no collateral, so it is taken.
//
// ⚠ `topSchemaSegment(path) == path` is a PROXY for "the fold costs nothing", and the
// two are not the same test: a top-level array whose ELEMENT is the declaration
// (`tokens: {items: {type: secret}}`) collects `tokens[]`, folds to `tokens`, and
// carries no collateral either — the proxy declines it. They coincide on every shape
// that can reach here, because that one is refused at load
// (`secret_field_unsupported_location`). Widen the test, not the proxy, if that ever
// relaxes.
//
// ⚠ "Not supposed to be there" is the honest strength of that claim, and it is why
// the exact-address case is taken rather than dropped. [config.StripDeclaredSecrets]
// runs on the state MERGE ([stateop.Merge], its only caller) and not on the upgrade
// write, which applies the migration chain and writes the record directly — so a
// migration that moves a plaintext onto a declared-secret path leaves it readable
// until the next merge. Where the address costs nothing, cover it.
//
// nil when the schema yields no address at all, so a caller can hand the result
// straight to the seal: an empty address set is a detector that only catches vault().
func StateSchemaSecretFields(art *artifact.ServiceArtifact) map[string]bool {
	if art == nil || art.Manifest == nil {
		return nil
	}
	set := audit.SecretPathSet{}
	collectStateSchemaSecrets(art.Manifest.StateSchema, "", set, sealAddressable)
	if len(set) == 0 {
		return nil
	}
	out := make(map[string]bool, len(set))
	for path := range set {
		out[topSchemaSegment(path)] = true
	}
	return out
}

// topSchemaSegment — the first segment of a dot/idx path the collector writes:
// `db_password` → itself, `tls.key` → `tls`, `redis_users[].password` →
// `redis_users`. Both separators, because a secret under an array element carries
// the `[]` marker directly on the field name.
func topSchemaSegment(path string) string {
	if i := strings.IndexAny(path, ".["); i >= 0 {
		return path[:i]
	}
	return path
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
	collectStateSchemaSecrets(schema, path, set, bothMarkers)
}

// stateSecretMarkers — which of the two orthogonal declarations ([ADR-0083] §1) a
// walk is asking about. One recursion answers both, because two recursions over one
// schema is how the read path and the destroy path came to disagree (NIM-531).
type stateSecretMarkers int

const (
	// bothMarkers — every declared secret, whichever marker declares it. The
	// MASKING question: what must never be printed if it is there at all. Every
	// path is addressed exactly, so nothing is over-covered by saying yes.
	bothMarkers stateSecretMarkers = iota
	// sealAddressable — the SEAL question, which a mask does not have to ask:
	// every `secret: true`, plus a `type: secret` whose path the seal can address
	// WITHOUT widening it. See the ★ on [StateSchemaSecretFields] — a CEL read
	// names only the top segment, so a declaration deeper down costs whatever else
	// lives under that segment.
	sealAddressable
)

func collectStateSchemaSecrets(schema config.InputSchemaMap, path string, set audit.SecretPathSet, want stateSecretMarkers) {
	for field, sub := range schema {
		collectStateSchemaSecretsNode(sub, joinSchemaPath(path, field), set, false, want)
	}
}

// collectStateSchemaSecretsNode walks one schema node. apNode says the node is an
// `additional_properties` value, which changes ONE thing: its `secret: true` flag is
// ignored. Its path is the MAP's, so honouring the flag would mark the whole map
// secret — an over-mask on the read path, and a dead entry besides, since IsSecret
// never requests `map_field.*` (see the ★ limitation in the doc comment above; the
// degradation to vault+regex is intentional).
//
// `type: secret` on such a node is still marked FOR A MASKING WALK
// ([bothMarkers]), exactly as before the schema became typed: the older code
// stripped the `secret` KEY and left the type, so the path was entered. That branch
// is unreachable through a loaded manifest — [config.CollectSecretFields] refuses a
// declared secret in that position — and the mask is belt and braces either way,
// which is precisely why it should not quietly narrow. A [sealAddressable] walk
// takes it when the map is top level, where the ap node's path (the MAP's) is
// already its own top segment; under a nested map it folds and is declined, like
// any other `type: secret` one level down.
//
// ⚠ apNode does NOT drop `secret: true` for a [sealAddressable] walk — see the ★ on
// [isSecretNode]. That flag answers a question about the MASK's addressing, and the
// seal addresses this case exactly.
//
// Everything nested inside is still walked, because a concrete `properties` name under
// it does have an exact path.
func collectStateSchemaSecretsNode(s *config.InputSchema, path string, set audit.SecretPathSet, apNode bool, want stateSecretMarkers) {
	if s == nil {
		return
	}
	if path != "" && isSecretNode(s, path, apNode, want) {
		set[path] = true
	}
	collectStateSchemaSecrets(s.Properties, path, set, want)
	collectStateSchemaSecretsNode(s.Items, path+"[]", set, false, want)
	if ap, ok := s.AdditionalProperties.(*config.InputSchema); ok {
		collectStateSchemaSecretsNode(ap, path, set, true, want)
	}
}

// isSecretNode reports whether the schema node is a secret leaf: the older
// `secret: true` marker ([ADR-010] §7.4 — the value LIVES in state and is masked on the
// way out), or `type: secret` ([ADR-0083] §1 — the value lives in Vault and never in
// state at all). ignoreFlag drops the first of the two (see the caller).
//
// For a masking caller the second is belt and braces rather than the mechanism:
// nothing writes a declared secret into state, so the path is normally empty and the
// mask is inert. It is there so that a value arriving by some other route — an old
// snapshot, a migration, a bug — is masked instead of printed.
//
// That argument holds because the mask addresses the exact path, and it is why want
// takes the PATH as well as the node: a [sealAddressable] caller can only address the
// top segment, so it declines a `type: secret` where covering one leaf would mean
// covering a whole collection ([StateSchemaSecretFields]).
//
// ★ ignoreFlag is the MASK's concession and not the seal's, which is why want
// overrides it. The mask drops `secret: true` on an ap node because its own path
// vocabulary cannot say "any key": `map_field.*` is a path [audit.SecretPathSet.IsSecret]
// never asks for, and `map_field` would mask the concrete siblings too. The seal's unit
// IS the top segment, so "every value of this map is secret" is exactly what it can
// express — and dropping it there would leave a map of state-resident plaintext with no
// address at all, which is this ticket's own leak in a container. Inheriting the
// concession unexamined is how the first version of this walk went wrong in the other
// direction; the two callers ask different questions and neither answer is the other's.
func isSecretNode(s *config.InputSchema, path string, ignoreFlag bool, want stateSecretMarkers) bool {
	if s.Secret && (!ignoreFlag || want == sealAddressable) {
		return true
	}
	if s.Type != config.SecretTypeName {
		return false
	}
	return want == bothMarkers || topSchemaSegment(path) == path
}

// joinSchemaPath concatenates a state_schema dot path (BIT-FOR-BIT like audit.joinPath /
// render.joinKey: empty path → field without a leading dot).
func joinSchemaPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}
