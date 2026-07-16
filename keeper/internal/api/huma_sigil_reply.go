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

// PluginSigilAllowReply — native 201-body POST /v1/plugins/sigils (form 1:1 with previous
// PluginSigilAllowReply). namespace/name/ref + sha256 (calculated Keeper).
//
// OUTPUT-PATTERN (documentation, NOT runtime-validation): huma does NOT validate
// response-body (empirically 200, not 500). sha256 — machine hex(sha256) binary
// (hex.EncodeToString, lowercase 64 chars, pluginhost/slot.go:173); allowed_by_aid ←
// operator.AIDPattern. ref NOT tagged: this is git-ref (tag/branch per ADR-007),
// arbitrary string, NOT hash.
type PluginSigilAllowReply struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Ref       string `json:"ref"`
	SHA256    string `json:"sha256" pattern:"^[0-9a-f]{64}$"` // hex(sha256) binary
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
	AllowedAt    time.Time  `json:"allowed_at"`
	AllowedByAID string     `json:"allowed_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Name         string     `json:"name"`
	Namespace    string     `json:"namespace"`
	Ref          string     `json:"ref"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	SHA256       string     `json:"sha256" pattern:"^[0-9a-f]{64}$"` // hex(sha256) binary
}

// === projection of domain handlers.Sigil*-result-s → native wire-DTO ===

// newPluginSigilAllowReply projects flat domain handlers.SigilAllowView to native.
func newPluginSigilAllowReply(v handlers.SigilAllowView) PluginSigilAllowReply {
	return PluginSigilAllowReply{
		Name:      v.Name,
		Namespace: v.Namespace,
		Ref:       v.Ref,
		SHA256:    v.SHA256,
	}
}

// newPluginSigilView projects flat domain handlers.SigilView to native. RevokedAt
// and AllowedAt handler already truncated to seconds (byte-exact with legacy wire).
func newPluginSigilView(v handlers.SigilView) PluginSigilView {
	return PluginSigilView{
		AllowedAt:    v.AllowedAt,
		AllowedByAID: v.AllowedByAID,
		Name:         v.Name,
		Namespace:    v.Namespace,
		Ref:          v.Ref,
		RevokedAt:    v.RevokedAt,
		SHA256:       v.SHA256,
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
