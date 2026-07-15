package api

// HUMA-NATIVE wire-DTO of the SYNOD domain (handler-native T5d). The Reply/output Body of the huma
// operations is a native Go struct in package api, no legacy generator. The handler (handlers/synod.go)
// returns domain results with flat fields; the register func (huma_synod.go)
// projects them INTO THESE types directly — there are no more legacy-generator → native converters.
//
//   - THE SCHEMA NAME = the contract one (SynodListReply / SynodView): huma DefaultSchemaNamer
//     takes reflect.Type.Name() → a schema under the same name the legacy generator produced.
//   - The only domain reply with a body is GET /v1/synods (SynodListReply.Items []SynodView).
//     create/update/delete/add/remove-operator/grant/revoke-role — 201/204 with no body.
//   - description — `*string` WITH omitempty (nil → key omitted); operators/roles — `[]string`
//     without omitempty (the handler yields a non-nil empty array → `[]`).
//   - The wire SHAPE (categories A-D ADR-051) is pinned byte-exact by huma_synod_reply_test.go.
//
// OUTPUT-PATTERN (documentation-only, NOT runtime validation): huma does NOT validate
// the response body (empirically 200, not 500). operators[] — per-element AID (group
// member) ← operator.AIDPattern; huma puts the pattern in items[]. A format for client
// codegen; the pattern does not affect json.Marshal (golden byte-exact intact).
//
// OUTPUT-PATTERN FOR NAMES (batch 5): name + roles[] (per-element) ← rbac.RoleNamePattern
// (the synod name shares reRoleName with role-name per the synod.go decision; roles[] — role names).
// huma puts a per-element pattern in items[]. SynodView is output-only (synod.list — a separate
// *Request on create) → no input-422 risk.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// SynodListReply — the native 200 body of GET /v1/synods (items under `items`, no
// offset/limit/total). items — native SynodView. Shape 1:1 with the former SynodListReply.
type SynodListReply struct {
	Items []SynodView `json:"items"`
}

// SynodView — the native projection of a Synod group (element of SynodListReply.items). Shape 1:1 with
// the former SynodView: builtin (bool), description — `*string` WITH omitempty (nil → key
// omitted), operators/roles — `[]string` without omitempty (empty array, not nil).
type SynodView struct {
	Builtin     bool     `json:"builtin"`
	Description *string  `json:"description,omitempty"`
	Name        string   `json:"name" pattern:"^[a-z][a-z0-9-]*$"`                  // ← rbac.RoleNamePattern (reRoleName)
	Operators   []string `json:"operators" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern (per-element)
	Roles       []string `json:"roles" pattern:"^[a-z][a-z0-9-]*$"`                 // ← rbac.RoleNamePattern (per-element)
}

// === projection of domain handlers.SynodView (flat fields) → native wire-DTO ===

// newSynodView projects the flat domain handlers.SynodView into native SynodView.
// Description is always emitted (even empty "" — a field without omitempty in the former wire),
// so the pointer is unconditional.
func newSynodView(v handlers.SynodView) SynodView {
	desc := v.Description
	return SynodView{
		Builtin:     v.Builtin,
		Description: &desc,
		Name:        v.Name,
		Operators:   v.Operators,
		Roles:       v.Roles,
	}
}

// newSynodListReply projects the domain handlers.SynodListPage into native SynodListReply.
// Preserves nil-vs-empty input 1:1 (nil → null, [] → []) for the byte-exact catalog wire
// (category B ADR-051).
func newSynodListReply(p handlers.SynodListPage) SynodListReply {
	if p.Items == nil {
		return SynodListReply{Items: nil}
	}
	items := make([]SynodView, len(p.Items))
	for i := range p.Items {
		items[i] = newSynodView(p.Items[i])
	}
	return SynodListReply{Items: items}
}
