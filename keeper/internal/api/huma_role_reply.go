package api

// HUMA-NATIVE wire-DTOs of the ROLE domain (handler-native T5d). The Reply/output Body
// of the huma operations are native Go structs in package api, with no legacy
// generator. The handler (handlers/role.go) returns domain results with flat fields;
// the register func (huma_role.go) projects them INTO THESE types directly — there
// are no more legacy-generator → native converters. Key points:
//
//   - THE SCHEMA NAME = the contract one (RoleView): huma DefaultSchemaNamer takes
//     reflect.Type.Name() → a schema under the same name RoleView gave.
//   - The domain's only reply with a body is GET /v1/roles (RoleListReply.Items
//     []RoleView). create/delete/update-permissions/grant/revoke — 201/204 with no
//     body.
//   - default_scope/description — `*string` WITH omitempty (nil → key omitted);
//     operators/permissions — `[]string` WITHOUT omitempty (the handler gives a
//     non-nil empty array → `[]`).
//   - THE wire SHAPE (json tags/omitempty/[]-vs-null, categories A-D of ADR-051) —
//     golden byte-exact pinned by huma_role_reply_test.go.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// NAME OUTPUT-PATTERN (documentation-only, NOT runtime validation): huma does NOT
// validate the response body (empirically 200, not 500). name ← rbac.RoleNamePattern.
// A format for client codegen; the pattern does not affect json.Marshal (golden
// byte-exact intact). RoleView is output-only (role.list — a separate *Request for
// create) → no input-422 risk. operators[] is NOT tagged: it is an AID
// (operator.AIDPattern), outside the name scope of this batch.

// RoleView — a native role-catalog record (element RoleListReply.items). Shape 1:1
// with the former RoleView: builtin (bool), default_scope/description — `*string`
// WITH omitempty (nil → key omitted), operators/permissions — `[]string` WITHOUT
// omitempty (an empty array, not nil).
type RoleView struct {
	Builtin      bool     `json:"builtin"`
	DefaultScope *string  `json:"default_scope,omitempty" doc:"role scope: boolean predicate over coven/service/incarnation/host/trait (e.g. coven in (a, b) AND host matches redis-*); omitted → role without scope"`
	Description  *string  `json:"description,omitempty"`
	Name         string   `json:"name" pattern:"^[a-z][a-z0-9-]*$"` // ← rbac.RoleNamePattern
	Operators    []string `json:"operators"`
	Permissions  []string `json:"permissions"`
}

// === projection of domain handlers.RoleView (flat fields) → native wire-DTO ===

// newRoleView projects the flat domain handlers.RoleView into the native RoleView.
// Description is always emitted (even empty "" — a field without omitempty in the
// former wire), so the pointer is unconditional; an empty DefaultScope (NULL) → nil
// (omitempty omits the key — a role with no scope).
func newRoleView(v handlers.RoleView) RoleView {
	desc := v.Description
	out := RoleView{
		Builtin:     v.Builtin,
		Description: &desc,
		Name:        v.Name,
		Operators:   v.Operators,
		Permissions: v.Permissions,
	}
	if v.DefaultScope != "" {
		out.DefaultScope = &v.DefaultScope
	}
	return out
}

// newRoleListReply projects the domain handlers.RoleListPage into the native
// RoleListReply (items under `items`, with no pagination). Preserves nil-vs-empty
// input 1:1 (nil → null, [] → []) for the catalog's byte-exact wire (category B of
// ADR-051).
func newRoleListReply(p handlers.RoleListPage) RoleListReply {
	if p.Items == nil {
		return RoleListReply{Items: nil}
	}
	items := make([]RoleView, len(p.Items))
	for i := range p.Items {
		items[i] = newRoleView(p.Items[i])
	}
	return RoleListReply{Items: items}
}
