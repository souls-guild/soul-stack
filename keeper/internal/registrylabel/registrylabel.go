// Package registrylabel is the one place the platform states what a registry
// entity's `label` is, and the one place that canonicalises an operator-supplied
// value before it reaches a row ([ADR-0085], NIM-728).
//
// A registry entity carries two fields instead of one:
//
//   - `name` — the identifier. Lower-case kebab, immutable, the primary key, a
//     URL path segment, a Vault path segment, a CEL root and an RBAC scope value.
//   - `label` — this. Free text, capitals allowed, mutable at any time, not
//     unique, not required, display-only.
//
// # THE INVARIANT
//
// `label` participates in NOTHING derived. Not a Vault path. Not an RBAC scope.
// Not a snapshot directory. Not `incarnation.<...>` in CEL. Not a selector. Not
// a resolver. Not an FK. Not a URL.
//
// Only under that invariant is "I changed the label and nothing moved" a
// guarantee rather than a hope. A `label` that reached one derived surface would
// make every label edit a potential rename of something, and the sharpest
// instance costs a secret: `SecretField.VaultPath` substitutes its segments
// verbatim, `ValidVaultPathSegment` allows capitals and folds no case, and
// `secretwrite`'s `Put` REPLACES rather than merges — so a derived path that
// moved leaves every password already issued under the old one unreachable,
// with no error raised anywhere.
//
// # Where the invariant is guarded
//
// The guard is behavioural and lives next to each derivation, because each
// derivation is reached through unexported code that only its own package can
// call. Every one of these drives the real derivation with a row whose `Label`
// is BOTH different from the identifier AND itself a well-formed identifier —
// so a naive substitution produces a perfectly valid address, and only the
// assertion catches it:
//
//   - Vault path, `<mount>/<domain>/<entity>/<field>` (Herald, Provider) —
//     keeper/internal/herald/label_invariant_guard_test.go,
//     keeper/internal/provider/label_invariant_guard_test.go
//   - Vault path, `<mount>/<service>/<incarnation>/<state-field>[/<key>]` —
//     keeper/internal/api/handlers/label_invariant_guard_test.go, at the reveal
//     route, which is the one place that derivation is assembled from a LOADED
//     registry row. Its other consumer, `core.state.<verb>`, is reached only
//     through plain strings on the run context, so no caption is in scope along
//     it — a structural accident worth keeping rather than a guarantee.
//   - CEL root `incarnation.<...>` — ALL THREE environments [ADR-0085] names:
//     keeper/internal/render/label_invariant_guard_test.go covers the scenario /
//     destiny root and the flow-control context built from it;
//     keeper/internal/servicevars/label_invariant_guard_test.go covers the
//     [ADR-0082] service-vars environment, which builds its own map and would
//     otherwise have been missed.
//   - snapshot directory `<cacheRoot>/<service>/<sha1>` — the address at
//     keeper/internal/scenario/label_invariant_guard_test.go (the one place a
//     registry row becomes an artifact ref) and the layout itself at
//     keeper/internal/artifact/label_invariant_guard_test.go
//   - RBAC scope values (`incarnation=`, `service=`) —
//     keeper/internal/api/handlers/label_invariant_guard_test.go
//
// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md
package registrylabel

import "strings"

// Normalize canonicalises an operator-supplied label for storage.
//
// Surrounding whitespace is trimmed, and a value that is empty or all-whitespace
// after trimming becomes nil — which stores SQL NULL. That collapse is the point:
// "no label" and "a label the operator blanked out" are the same state, and the
// consumer's fallback (show the identifier) then has exactly one trigger to test
// for instead of two. A caller passing nil gets nil back.
//
// Nothing else is done to the value. No case folding, no length bound, no form
// check: capitals, spaces and punctuation are what the field exists to carry, and
// the narrow grammar belongs to the identifier beside it.
func Normalize(in *string) *string {
	if in == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*in)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// Display resolves what a consumer shows for an entity: the label when it
// carries one, the identifier otherwise ([ADR-0085] — "empty/absent → the
// consumer shows the identifier").
//
// **Nothing in keeper calls this, and that is expected rather than an
// oversight.** Keeper renders no captions: every REST and MCP surface returns
// the nullable field as it stands, so the consumer can style the two cases
// differently rather than receive one pre-flattened string. The renderers are
// the web UI (a separate repository) and `soulctl` (a separate Go module, which
// cannot import an `internal` package at all).
//
// It exists anyway because the fallback is a RULE, not a formatting preference —
// blank and absent collapse to the same thing, and they collapse the same way
// here as they do on the write path through [Normalize]. Written once and
// tested, rather than restated in prose and re-derived by each reader.
//
// It must never be called to build an address: the identifier alone does that,
// and the whole point of the split is that this function's output is not stable.
func Display(label *string, id string) string {
	if n := Normalize(label); n != nil {
		return *n
	}
	return id
}
