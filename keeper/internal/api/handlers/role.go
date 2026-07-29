// Role handlers of the Operator API (RBAC Phase 2, Slice 2a) — the domain layer over
// [rbac.Service]. *Typed functions carry business logic without http.ResponseWriter/
// *http.Request; HTTP is served by huma full-typed (api/huma_role.go), MCP calls
// rbac.Service directly (bypassing the handler).
//
// T5d (handler-native): the role domain is decoupled from the legacy generator. *Typed
// functions accept NATIVE request types (organized by the huma-input in the api package) and
// return domain results with FLAT wire fields — the native wire-DTO (the OpenAPI schema) is
// built by the api package from these fields. The (w,r) wrappers are removed.
//
// Business logic (builtin boundary, self-lockout, name/permission validation) is
// in [rbac.Service]; the handler only maps sentinel errors to RFC 7807. The RBAC
// check is in middleware (see api/router.go), not here.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// RoleHandler — the six RBAC-CRUD endpoints (roles / permissions / membership).
// Delegates business logic to [rbac.Service].
//
// All dependencies immutable; safe for concurrent use — holds no state between
// requests.
type RoleHandler struct {
	svc    *rbac.Service
	logger *slog.Logger
}

// NewRoleHandler creates the handler. svc is required (panic on nil —
// the only misconfiguration point, the caller must pass non-nil).
func NewRoleHandler(svc *rbac.Service, logger *slog.Logger) *RoleHandler {
	if svc == nil {
		panic("handlers.NewRoleHandler: rbac.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &RoleHandler{svc: svc, logger: logger}
}

// RoleCreateInput — NATIVE request form of POST /v1/roles (handler-native T5d).
// Replaces RoleCreateRequest: the huma-input (api package) binds and validates
// the body by its own fields, then calls CreateTyped with this flat model.
// default_scope is optional (ADR-047 S1): nil → a role without a scope restriction
// (bare-perms unrestricted). Description/Permissions — value/slice (empty ones are
// treated as "not set", parity with the legacy decode).
type RoleCreateInput struct {
	Name         string
	Description  string
	Permissions  []string
	DefaultScope *string
	// ParentRole names the role this one derives from (ADR-078); nil creates a
	// plain role. On a derived role DefaultScope is the attenuating DELTA, not an
	// absolute scope.
	ParentRole *string
	// ScopeMode — `track` (default) or `pin` for that delta (ADR-078(k)). Only
	// meaningful with ParentRole set.
	ScopeMode string
}

// RoleView — the FLAT domain projection of a role (GET /v1/roles items[]), handler-
// native T5d. The api package projects it into the native RoleView schema (register-func).
// DefaultScope/Description — a RAW string from the domain (empty = NULL/no value);
// the nullable wire form (omitempty) is held by the native type in api.
//
// The role comes in BOTH forms (ADR-078): as stored (Permissions/DefaultScope —
// the delta on a derived role) and as resolved (Effective*). Publishing the
// resolved form is what keeps inheritance out of the consumer: a UI that walked
// ParentRole itself would be a second implementation of the attenuation rules.
type RoleView struct {
	Name         string
	Description  string
	Builtin      bool
	Permissions  []string
	Operators    []string
	DefaultScope string
	// ParentRole — the role this one derives from; empty = a plain role.
	ParentRole string
	// ScopeMode — `track` / `pin` / empty on a plain role (ADR-078(k)).
	ScopeMode string
	// EffectivePermissions / EffectiveScope — the role resolved against its chain
	// (`own ∩ the parent's effective`, scopes conjoined). Equal to the stored form
	// on a plain role.
	EffectivePermissions []string
	EffectiveScope       string
	// InertPermissions — stored rows the chain no longer covers (ADR-078(l)): a
	// role can list permissions and grant none of them, and the list alone does not
	// show it.
	InertPermissions []string
}

// RoleListPage — the domain list of roles for GET /v1/roles (handler-native T5d). The api
// package projects Items → native RoleListReply (no pagination, role.list returns
// the whole catalog).
type RoleListPage struct {
	Items []RoleView
}

// RoleSpecStub — a non-empty *RoleHandler stub for generating the huma-OpenAPI
// fragment (HumaRoleSpecYAML): on dump the domain handler is not called, but
// huma.Register requires non-nil for its no-op nil check. svc is nil — the handler
// never executes in spec mode (parity [CadenceSpecStub]).
func RoleSpecStub() *RoleHandler {
	return &RoleHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// RoleCreateReply — the result of a successful [RoleHandler.CreateTyped] (handler-native
// T5d). The role.create 201 body is EMPTY (legacy contract: openapi.yaml `POST /v1/roles`
// returns 201 without `content`), so the reply carries not response wire fields but METADATA
// for the audit-payload (role name, permission set, creator AID) — the huma wrapper
// puts them on the huma-ctx via [middleware.SetHumaAuditPayload], and humaAuditMiddleware
// writes the audit event after a successful next (variant B, see api/huma_audit.go).
type RoleCreateReply struct {
	Name         string
	Permissions  []string
	CreatedByAID string
	// ParentRole / DefaultScope — the derivation of the created role (ADR-078):
	// its ceiling and its delta. nil = none.
	ParentRole   *string
	DefaultScope *string
	// ScopeMode — whether that delta tracks the parent or pins it (ADR-078(k)).
	ScopeMode string
}

// AuditPayload assembles the audit-payload of the create route (parity with the legacy
// SetAuditPayload). The SINGLE source for huma variant B.
//
// parent_role and default_scope are ALWAYS present, null included: on a derived role
// the permission list alone does not say what the role grants — the ceiling and the
// delta are half of the answer, and an audit record of an authorization change has to
// carry both (ADR-078).
func (r RoleCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.Name,
		"permissions":    r.Permissions,
		"created_by_aid": r.CreatedByAID,
		"parent_role":    r.ParentRole,
		"default_scope":  r.DefaultScope,
		// Always present, like the two above: on a derived role "what this delta is
		// for" is part of the authorization change, and its absence from the record
		// would leave a reader unable to tell a tracking role from a pinned one.
		"scope_mode": r.ScopeMode,
	}
}

// CreateTyped — domain function for POST /v1/roles (handler-native T5d): business logic
// without http.ResponseWriter/*http.Request. claims and req arrive as arguments; errors —
// *problemError (delivered by the huma wrapper via [AsProblemDetails]), success —
// [RoleCreateReply].
//
// Steps: required name → svc.CreateRole (name/permission/default_scope validation +
// RBAC subset-check + persist) → sentinel→problem. The audit-payload is NOT written here —
// the reply carries it; the huma-audit-middleware does the write. The 201 body is empty.
func (h *RoleHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req RoleCreateInput) (RoleCreateReply, error) {
	var zero RoleCreateReply
	if req.Name == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'name' is required")}
	}

	perms := req.Permissions
	err := h.svc.CreateRole(ctx, rbac.CreateRoleInput{
		Name:         req.Name,
		Description:  req.Description,
		Permissions:  perms,
		CallerAID:    claims.Subject,
		DefaultScope: req.DefaultScope,
		ParentRole:   req.ParentRole,
		ScopeMode:    rbac.ScopeMode(req.ScopeMode),
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrRoleAlreadyExists):
		return zero, &problemError{problem.New(problem.TypeRoleExists, "", "role "+req.Name+" already exists")}
	case errors.Is(err, rbac.ErrRoleNotFound):
		// Only reachable for a named parent_role (ADR-078) — the role being
		// created cannot itself be missing.
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", err.Error())}
	case errors.Is(err, rbac.ErrInvalidRoleName):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot grant a permission you do not hold yourself")}
	case errors.Is(err, rbac.ErrRoleExceedsParent):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "a derived role may not exceed its parent role")}
	case errors.Is(err, rbac.ErrRootRoleNotPermitted):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "a role with no parent grants privilege that follows nothing - derive from a role you hold, or obtain role.create-root")}
	case isInvalidPermission(err) || isInvalidDefaultScope(err) || isInvalidDerivation(err):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("role.create: service failed",
			slog.String("name", req.Name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create role failed")}
	}

	return RoleCreateReply{
		Name:         req.Name,
		Permissions:  perms,
		CreatedByAID: claims.Subject,
		ParentRole:   req.ParentRole,
		DefaultScope: req.DefaultScope,
		ScopeMode:    req.ScopeMode,
	}, nil
}

// ListTyped — domain function for GET /v1/roles (handler-native T5d, READ no audit):
// reads the role catalog and assembles [RoleListPage] (flat RoleView) without
// http.ResponseWriter/*http.Request. A catalog read error → *problemError (500);
// the huma wrapper delivers it via [AsProblemDetails]. The items wire form (Description
// always, DefaultScope nil→omitted, []-vs-null) is built by the native projection in api.
//
// callerAID scopes the catalog to what that operator may see (NIM-202) — the
// service filters, so REST and MCP get the same answer for the same caller.
func (h *RoleHandler) ListTyped(ctx context.Context, callerAID string) (RoleListPage, error) {
	views, err := h.svc.ListRoles(ctx, callerAID)
	if err != nil {
		h.logger.Error("role.list: service failed", slog.Any("error", err))
		return RoleListPage{}, &problemError{problem.New(problem.TypeInternalError, "", "list roles failed")}
	}

	items := make([]RoleView, 0, len(views))
	for _, v := range views {
		items = append(items, toRoleView(v))
	}
	return RoleListPage{Items: items}, nil
}

// RoleNameReply — the result of write operations whose audit-payload carries only the role
// name (delete). The 204 body is empty; the reply is METADATA for audit (the huma wrapper puts
// it on the huma-ctx, the middleware writes after success; the (w,r) wrapper via SetAuditPayload).
type RoleNameReply struct {
	Name string
}

// DeleteTyped — the extracted domain function DELETE /v1/roles/{name} (FULL-TYPED
// rollout of ADR-054 §Pattern (b)): business logic without http.ResponseWriter/
// *http.Request. name arrives as an argument (path extraction is on the calling layer);
// errors — *problemError, success — [RoleNameReply] (audit-payload). The 204 body is empty.
//
// callerAID decides whether this operator may administer the role at all
// (NIM-214) — the service refuses one whose rights the caller could not grant.
func (h *RoleHandler) DeleteTyped(ctx context.Context, name, callerAID string) (RoleNameReply, error) {
	var zero RoleNameReply
	err := h.svc.DeleteRole(ctx, name, callerAID)
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", "role "+name+" not found")}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot administer a role whose permissions you do not hold yourself")}
	case errors.Is(err, rbac.ErrRoleBuiltin):
		return zero, &problemError{problem.New(problem.TypeRoleBuiltin, "", "role "+name+" is builtin and cannot be deleted")}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "deleting role "+name+" would lock out the cluster")}
	case errors.Is(err, rbac.ErrRoleHasChildren):
		return zero, &problemError{problem.New(problem.TypeRoleHasChildren, "", "role "+name+" is a parent of derived roles — re-parent or delete them first")}
	default:
		h.logger.Error("role.delete: service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete role failed")}
	}
	return RoleNameReply{Name: name}, nil
}

// UpdatePermissionsInput — the parameters of [RoleHandler.UpdatePermissionsTyped]
// (FULL-TYPED rollout of ADR-054 §Pattern). SetDefaultScope carries the presence flag
// of the default_scope key (omitted vs explicit null → different PATCH semantics): true →
// replace scope with the DefaultScope value (nil clears it); false → leave scope untouched.
// Presence computation is on the calling layer (the huma convert by raw body, the (w,r)
// wrapper by jsonHasKey).
type UpdatePermissionsInput struct {
	Name            string
	Permissions     []string
	SetDefaultScope bool
	DefaultScope    *string
	// SetParentRole / ParentRole — re-rooting, with the same PATCH-presence
	// semantics as the scope pair (ADR-078): omitted → the role's derivation is
	// untouched; present → replaced (null makes the role plain again).
	SetParentRole bool
	ParentRole    *string
	// SetScopeMode / ScopeMode — the delta's intent, PATCH-presence again
	// (ADR-078(k)). Sending `pin` RE-PINS onto the parent's scope as of now.
	SetScopeMode bool
	ScopeMode    string
	// ConfirmCascade — the caller has seen what this change does to the roles
	// derived from this one and means to do it (NIM-199). Without it a mutation
	// that moves a child's rights is refused with the report.
	ConfirmCascade bool
}

// RolePermissionsReply — the result of [RoleHandler.UpdatePermissionsTyped]:
// METADATA for the audit-payload (role name + new permission set, plus whichever of
// the derivation fields the request actually sent). The 204 body is empty.
type RolePermissionsReply struct {
	Name        string
	Permissions []string

	// SetParentRole / ParentRole / SetDefaultScope / DefaultScope / SetScopeMode /
	// ScopeMode mirror the request's PATCH presence, so the audit record says what
	// the mutation CHANGED rather than restating fields it left alone.
	SetParentRole   bool
	ParentRole      *string
	SetDefaultScope bool
	DefaultScope    *string
	SetScopeMode    bool
	ScopeMode       string

	// ConfirmCascade — recorded whenever it was sent, because it is the operator
	// accepting a change to roles OTHER than the one named in the record. An audit
	// trail that only showed the edited role would not explain why a dozen derived
	// roles moved at the same instant.
	ConfirmCascade bool
}

// AuditPayload assembles the audit-payload of the update route. name/permissions
// always; parent_role/default_scope only when the request carried them — an absent
// key means "untouched", a present null means "cleared", and the two are different
// authorization changes (ADR-078: re-rooting a role rewrites its ceiling).
func (r RolePermissionsReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{
		"name":        r.Name,
		"permissions": r.Permissions,
	}
	if r.SetParentRole {
		p["parent_role"] = r.ParentRole
	}
	if r.SetDefaultScope {
		p["default_scope"] = r.DefaultScope
	}
	if r.SetScopeMode {
		p["scope_mode"] = r.ScopeMode
	}
	if r.ConfirmCascade {
		p["confirm_cascade"] = true
	}
	return p
}

// UpdatePermissionsTyped — the extracted domain function PATCH /v1/roles/{name}/
// permissions (FULL-TYPED rollout of ADR-054 §Pattern (b)): replace semantics for
// permissions + optional default_scope replacement, without http.ResponseWriter/*http.Request.
// claims/in arrive as arguments (decode/presence-detect/auth on the calling layer);
// errors — *problemError, success — [RolePermissionsReply] (audit-payload).
func (h *RoleHandler) UpdatePermissionsTyped(ctx context.Context, claims *jwt.Claims, in UpdatePermissionsInput) (RolePermissionsReply, error) {
	var zero RolePermissionsReply
	err := h.svc.UpdateRolePermissions(ctx, rbac.UpdateRolePermissionsInput{
		Name:            in.Name,
		Permissions:     in.Permissions,
		CallerAID:       claims.Subject,
		SetDefaultScope: in.SetDefaultScope,
		DefaultScope:    in.DefaultScope,
		SetParentRole:   in.SetParentRole,
		ParentRole:      in.ParentRole,
		SetScopeMode:    in.SetScopeMode,
		ScopeMode:       rbac.ScopeMode(in.ScopeMode),
		ConfirmCascade:  in.ConfirmCascade,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", "role "+in.Name+" not found")}
	case errors.Is(err, rbac.ErrRoleBuiltin):
		return zero, &problemError{problem.New(problem.TypeRoleBuiltin, "", "role "+in.Name+" is builtin and cannot be updated")}
	case errors.Is(err, rbac.ErrRoleCascadeNeedsConfirm):
		// The report travels in `detail`: it names every derived role that moves
		// and how many operators hold them, which is what the caller has to be
		// shown before they can meaningfully resend with confirm_cascade.
		return zero, &problemError{problem.New(problem.TypeRoleCascade, "",
			"updating role "+in.Name+" changes roles derived from it — "+cascadeDetail(err)+
				"; resend with confirm_cascade=true to proceed")}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "updating role "+in.Name+" would lock out the cluster")}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot grant a permission you do not hold yourself")}
	case errors.Is(err, rbac.ErrRoleExceedsParent):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "a derived role may not exceed its parent role")}
	case errors.Is(err, rbac.ErrRootRoleNotPermitted):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "a role with no parent grants privilege that follows nothing - derive from a role you hold, or obtain role.create-root")}
	case isInvalidPermission(err) || isInvalidDefaultScope(err) || isInvalidDerivation(err):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("role.update: service failed",
			slog.String("name", in.Name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "update role permissions failed")}
	}
	return RolePermissionsReply{
		Name:            in.Name,
		Permissions:     in.Permissions,
		SetParentRole:   in.SetParentRole,
		ParentRole:      in.ParentRole,
		SetDefaultScope: in.SetDefaultScope,
		DefaultScope:    in.DefaultScope,
		SetScopeMode:    in.SetScopeMode,
		ScopeMode:       in.ScopeMode,
		ConfirmCascade:  in.ConfirmCascade,
	}, nil
}

// cascadeDetail renders the blast-radius report carried by the error, falling
// back to the sentinel's own text if the error travelled without one.
func cascadeDetail(err error) string {
	var ce *rbac.CascadeError
	if errors.As(err, &ce) {
		return ce.Cascade.String()
	}
	return err.Error()
}

// RoleOperatorReply — the result of grant/revoke-operator: METADATA for the audit-payload
// (role name + AID; grant additionally carries GrantedByAID). The 204 body is empty.
type RoleOperatorReply struct {
	Name         string
	AID          string
	GrantedByAID string
}

// GrantOperatorTyped — the extracted domain function POST /v1/roles/{name}/operators
// (FULL-TYPED rollout of ADR-054 §Pattern (b)): AID validation (required + format) +
// binding an operator to the role, without http.ResponseWriter/*http.Request. CallerAID
// (granted_by_aid) — from claims. claims/name/aid arrive as arguments; errors —
// *problemError, success — [RoleOperatorReply] (audit-payload). Idempotent (a repeat is a
// no-op in the service).
func (h *RoleHandler) GrantOperatorTyped(ctx context.Context, claims *jwt.Claims, name, aid string) (RoleOperatorReply, error) {
	var zero RoleOperatorReply
	if aid == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'aid' is required")}
	}
	if !operator.ValidAID(aid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'aid' must match "+operator.AIDPattern)}
	}

	callerAID := claims.Subject
	err := h.svc.GrantOperator(ctx, rbac.GrantOperatorInput{
		RoleName:  name,
		AID:       aid,
		CallerAID: &callerAID,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", "role "+name+" not found")}
	case errors.Is(err, rbac.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "operator "+aid+" not found")}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot grant a role holding a permission you do not hold yourself")}
	default:
		h.logger.Error("role.grant-operator: service failed",
			slog.String("name", name),
			slog.String("aid", aid),
			slog.String("by_aid", callerAID),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "grant operator failed")}
	}
	return RoleOperatorReply{Name: name, AID: aid, GrantedByAID: callerAID}, nil
}

// RevokeOperatorTyped — the extracted domain function DELETE /v1/roles/{name}/
// operators/{aid} (FULL-TYPED rollout of ADR-054 §Pattern (b)): path-AID validation +
// removing the membership row, without http.ResponseWriter/*http.Request. name/aid
// arrive as arguments; errors — *problemError, success — [RoleOperatorReply]
// (audit-payload; GrantedByAID empty — revoke does not carry it).
func (h *RoleHandler) RevokeOperatorTyped(ctx context.Context, name, aid, callerAID string) (RoleOperatorReply, error) {
	var zero RoleOperatorReply
	if !operator.ValidAID(aid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'aid' must match "+operator.AIDPattern)}
	}

	err := h.svc.RevokeOperator(ctx, rbac.RevokeOperatorInput{
		RoleName:  name,
		AID:       aid,
		CallerAID: callerAID,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrRoleOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "operator "+aid+" is not a member of role "+name)}
	case errors.Is(err, rbac.ErrRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", "role "+name+" not found")}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "revoking operator "+aid+" from role "+name+" would lock out the cluster")}
	default:
		h.logger.Error("role.revoke-operator: service failed",
			slog.String("name", name),
			slog.String("aid", aid),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "revoke operator failed")}
	}
	return RoleOperatorReply{Name: name, AID: aid}, nil
}

// isInvalidPermission — true if err is an [rbac.ParsePermission] error
// (a malformed permission in CreateRole / UpdateRolePermissions). It has no
// sentinel (the text carries the diagnostic), so we recognize it by the wrapped
// "invalid permission" from the service. Maps to 422.
func isInvalidPermission(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid permission")
}

// isInvalidDefaultScope — true if err is an [rbac.ParseDefaultScope] error
// (a malformed default_scope in CreateRole / UpdateRolePermissions). No sentinel
// (the ParseDefaultScope text carries the diagnostic), we recognize it by the wrapped
// "invalid default_scope". Maps to 422.
func isInvalidDefaultScope(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid default_scope")
}

// isInvalidDerivation reports a parent-graph rule violation (ADR-078(f)): a chain
// that closes on itself, one past the depth cap, or a scope conjunction too
// complex to normalize. All three are malformed input rather than a missing
// right — 422, not 403.
func isInvalidDerivation(err error) bool {
	return errors.Is(err, rbac.ErrRoleParentCycle) ||
		errors.Is(err, rbac.ErrRoleChainTooDeep) ||
		errors.Is(err, rbac.ErrRoleScopeTooComplex)
}

// toRoleView converts [rbac.RoleView] into the FLAT domain [RoleView] (handler-
// native T5d): field-by-field passthrough; the nullable/omitempty wire form
// (Description always, DefaultScope ""→omitted, []-vs-null) is built by the native projection
// in api (newRoleView). Permissions/Operators — a non-nil slice (`[]`, not `null`).
func toRoleView(v rbac.RoleView) RoleView {
	return RoleView{
		Name:                 v.Name,
		Description:          v.Description,
		Builtin:              v.Builtin,
		Permissions:          emptyIfNil(v.Permissions),
		Operators:            emptyIfNil(v.Operators),
		DefaultScope:         v.DefaultScope,
		ParentRole:           v.ParentRole,
		ScopeMode:            string(v.ScopeMode),
		EffectivePermissions: emptyIfNil(v.EffectivePermissions),
		EffectiveScope:       v.EffectiveScope,
		InertPermissions:     emptyIfNil(v.InertPermissions),
	}
}

// emptyIfNil guarantees a non-nil slice for JSON (`[]` instead of `null`) —
// a role's permissions/operators with no entries serialize as an empty array. A shared
// helper of the role/synod domains (synod.toSynodResponse uses it too).
func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
