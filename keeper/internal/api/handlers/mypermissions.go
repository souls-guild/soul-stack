// My-permissions-handler Operator API (`GET /v1/me/permissions`) — publishes the
// effective permissions of the CURRENT Archon (from JWT claims), for a permission-aware UI:
// show/hide buttons based on "may I resource.action [in which scope]".
//
// Difference from `GET /v1/permissions` ([PermissionCatalogHandler]): that one returns the
// WHOLE catalog of possible permissions (source rbac.catalog.go), this one — the SUBSET
// actually granted to the current operator (unpacking its roles via
// [rbac.Enforcer.PermissionsOf]).
//
// RBAC — authentication only (valid JWT), no separate permission: the endpoint is
// self-describing "own permissions" (any authenticated caller sees EXACTLY THEIR OWN
// permissions; others are not returned — AID comes from claims, not from query). Symmetric to
// the catalog — read-only, no audit (health/meta / permissions-catalog pattern).
package handlers

import (
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// PermissionsLister — the narrow subset of the rbac surface the handler needs:
// unpacking the effective permissions of an AID. Implemented by [rbac.Enforcer] and
// [rbac.Holder] (via RBACProvider in server.go). A narrow interface (not *rbac.Enforcer)
// lets a handler test substitute a lightweight fake without building a full snapshot
// (the PermissionChecker pattern in middleware).
type PermissionsLister interface {
	PermissionsOf(aid string) []rbac.EffectivePermission
}

// MyPermissionsHandler — `GET /v1/me/permissions`. Holds no state (enforcer is an
// immutable snapshot); safe for concurrent use.
type MyPermissionsHandler struct {
	enforcer PermissionsLister
	logger   *slog.Logger
}

// NewMyPermissionsHandler creates the handler. logger nil → io.Discard.
func NewMyPermissionsHandler(enforcer PermissionsLister, logger *slog.Logger) *MyPermissionsHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &MyPermissionsHandler{enforcer: enforcer, logger: logger}
}

// MyScope — the FLAT scope summary of one permission (handler-native T5d): either
// unrestricted, or the OR-set of boolean scope predicates (NIM-128). Each entry of
// Exprs is the canonical string of one [rbac.ScopeExpr] (e.g.
// `coven in (a, b) AND host matches redis-*`); Exprs is empty when Unrestricted.
// The api package projects an empty Exprs into a nil pointer (omitempty).
type MyScope struct {
	Unrestricted bool
	Exprs        []string
}

// MyPermission — a FLAT domain effective permission (handler-native T5d). Wildcard=true
// — cluster-admin (`*`): resource/action empty, scope not carried. The api package projects
// it into the pointer-optional native shape.
type MyPermission struct {
	Wildcard bool
	Resource string
	Action   string
	Scope    *MyScope
}

// MyPermissions — the FLAT domain body of `GET /v1/me/permissions` (handler-native T5d).
type MyPermissions struct {
	Permissions []MyPermission
}

// GetTyped — the domain function `GET /v1/me/permissions` (handler-native T5d, READ without
// audit): unpacks the effective permissions of an AID without http.ResponseWriter/*http.Request.
// aid arrives as an argument (extraction from claims is on the calling layer). No errors →
// returns only the value (the native projection in api builds the pointer-optional wire).
// The wire shape (permissions non-nil, snake_case scope keys) is preserved — golden
// pins it byte-for-byte.
func (h *MyPermissionsHandler) GetTyped(aid string) MyPermissions {
	eff := h.enforcer.PermissionsOf(aid)
	perms := make([]MyPermission, 0, len(eff))
	for _, p := range eff {
		perms = append(perms, toMyPermission(p))
	}
	return MyPermissions{Permissions: perms}
}

// toMyPermission converts [rbac.EffectivePermission] into the FLAT domain shape.
// Wildcard → marker without scope; otherwise resource.action + scope summary.
func toMyPermission(p rbac.EffectivePermission) MyPermission {
	if p.Wildcard {
		return MyPermission{Wildcard: true}
	}
	exprs := make([]string, 0, len(p.Scope.Exprs))
	for _, e := range p.Scope.Exprs {
		exprs = append(exprs, e.String())
	}
	return MyPermission{
		Resource: p.Resource,
		Action:   p.Action,
		Scope: &MyScope{
			Unrestricted: p.Scope.Unrestricted,
			Exprs:        exprs,
		},
	}
}
