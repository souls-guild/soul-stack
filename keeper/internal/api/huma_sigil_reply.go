package api

// HUMA-NATIVE reply-DTO for the SIGIL domain (plugins/sigils allow-list;
// Teardown T5b, by the T5a huma_incarnation_reply.go reference). The
// Reply/output Body of huma operations is a native Go struct in package api,
// NOT legacy-generated. Teardown removes oapi/ + the handwritten layer:
// reply-Body must become code-first native.
//
// INVARIANTS (★ wire byte-exact + ★ schema name stable): the shape is
// byte-for-byte identical to the former legacy-generated one (same json
// tags; revoked_at — `*time.Time` WITH omitempty → key omitted when nil,
// category C; allowed_at — nanosecond time-wire value from the handler
// layer). EXPORTED-struct name = the contract name (PluginSigilAllowReply /
// PluginSigilListReply / PluginSigilView) → huma DefaultSchemaNamer yields
// the same schema. PluginSigilListReply is NOT a paged envelope (items[]
// only). Projection of domain handlers.Sigil*-results into these types is
// done by the register-func (huma_sigil.go); the handler hands out flat
// fields (handler-native T5d).

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (form 1:1 with previous legacy generated) ===

// PluginSigilArtifactView — native element artifacts[]: one approved file of a
// release (NIM-793).
//
// os/arch are GOOS/GOARCH spellings and are BOTH EMPTY on a kind=git grant, whose
// single binary declares no platform and therefore answers for every one. path is
// relative to source and is empty for that same entry.
type PluginSigilArtifactView struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256" pattern:"^[0-9a-f]{64}$"` // hex(sha256) binary
}

// PluginSigilAllowReply — native 201-body POST /v1/plugins/sigils: the echoed
// alias/source/ref + the kind and the artifacts the Keeper resolved and signed.
//
// The reply describes a RELEASE (NIM-793). `plugin.allow` confirms a release, so the
// body says which files that covered — the single `sha256` this reply carried until
// then could name only one platform's bytes, which on a multi-platform release is a
// true answer to a question the operator did not ask.
//
// OUTPUT-PATTERN (documentation, NOT runtime-validation): huma does NOT validate
// response-body (empirically 200, not 500). sha256 — machine hex(sha256) binary
// (hex.EncodeToString, lowercase 64 chars); allowed_by_aid ← operator.AIDPattern. ref
// NOT tagged: this is a git-ref (tag/branch per ADR-007), an arbitrary string, NOT a
// hash.
type PluginSigilAllowReply struct {
	Alias     string                    `json:"alias"`
	Ref       string                    `json:"ref"`
	Kind      string                    `json:"kind" enum:"git,artifact"`
	Artifacts []PluginSigilArtifactView `json:"artifacts"`
	Source    string                    `json:"source"`
}

// PluginSigilListReply — native 200-body GET /v1/plugins/sigils (form 1:1 with previous
// PluginSigilListReply). Only items[] — NOT paged-envelope.
type PluginSigilListReply struct {
	Items []PluginSigilView `json:"items"`
}

// === nested reply-DTO ===

// PluginSigilView — native element items[] (form 1:1 with previous PluginSigilView).
// revoked_at — `*time.Time` with omitempty (nil for active → key omitted); allowed_at —
// nanosecond time-wire (value truncates handler-layer to seconds).
type PluginSigilView struct {
	Alias        string     `json:"alias"`
	AllowedAt    time.Time  `json:"allowed_at"`
	AllowedByAID string     `json:"allowed_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Ref          string     `json:"ref"`
	Source       string     `json:"source"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	Kind         string     `json:"kind" enum:"git,artifact"`
	// Artifacts is the whole approved release, not this Keeper's platform: an
	// operator auditing the allow-list has to be able to see every digest that
	// approval covers.
	Artifacts []PluginSigilArtifactView `json:"artifacts"`
}

// === projection of domain handlers.Sigil*-result-s → native wire-DTO ===

// newPluginSigilAllowReply projects flat domain handlers.SigilAllowView to native.
func newPluginSigilAllowReply(v handlers.SigilAllowView) PluginSigilAllowReply {
	return PluginSigilAllowReply{
		Alias:     v.Alias,
		Ref:       v.Ref,
		Kind:      v.Kind,
		Artifacts: newPluginSigilArtifactViews(v.Artifacts),
		Source:    v.Source,
	}
}

// newPluginSigilArtifactViews projects the flat domain artifact rows to native. Always
// non-nil, so an approved release serializes as `[]` and never as `null` — the list is
// never legitimately absent.
func newPluginSigilArtifactViews(artifacts []handlers.SigilArtifactView) []PluginSigilArtifactView {
	out := make([]PluginSigilArtifactView, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, PluginSigilArtifactView{OS: a.OS, Arch: a.Arch, Path: a.Path, SHA256: a.SHA256})
	}
	return out
}

// newPluginSigilView projects flat domain handlers.SigilView to native. RevokedAt
// and AllowedAt handler already truncated to seconds (byte-exact with legacy wire).
func newPluginSigilView(v handlers.SigilView) PluginSigilView {
	return PluginSigilView{
		Alias:        v.Alias,
		AllowedAt:    v.AllowedAt,
		AllowedByAID: v.AllowedByAID,
		Ref:          v.Ref,
		RevokedAt:    v.RevokedAt,
		Kind:         v.Kind,
		Artifacts:    newPluginSigilArtifactViews(v.Artifacts),
		Source:       v.Source,
	}
}

// newPluginSigilListReply projects domain handlers.SigilListPage to native. Items
// preserve nil-vs-empty 1:1 (nil → null, [] → []) for byte-exact — ListTyped returns
// non-nil [] (empty registry → `[]`).
func newPluginSigilListReply(p handlers.SigilListPage) PluginSigilListReply {
	var items []PluginSigilView
	if p.Items != nil {
		items = make([]PluginSigilView, len(p.Items))
		for i := range p.Items {
			items[i] = newPluginSigilView(p.Items[i])
		}
	}
	return PluginSigilListReply{Items: items}
}
