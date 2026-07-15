// Synod handlers of the Operator API (RBAC Synod, ADR-049) — the domain layer over
// [rbac.Service]. The *Typed functions carry business logic without http.ResponseWriter/
// *http.Request; HTTP is served by huma full-typed (api/huma_synod.go), MCP calls
// rbac.Service directly (bypassing the handler).
//
// T5d (handler-native): the synod domain is detached from the legacy generator. *Typed accept NATIVE
// request types (assembled by the huma input in package api) and return domain
// results with FLAT wire fields. The (w,r) wrappers are gone.
//
// Business logic (builtin boundary, self-lockout, least-privilege subset,
// name/aid validation) lives in [rbac.Service]; the handler maps sentinel errors to RFC 7807.
// The RBAC check is in middleware (see api/router.go), not here.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// SynodHandler — the seven Synod-CRUD endpoints (groups / membership / bundle).
// Delegates business logic to [rbac.Service]. All dependencies immutable; safe
// for concurrent use.
type SynodHandler struct {
	svc    *rbac.Service
	logger *slog.Logger
}

// NewSynodHandler creates the handler. svc is required (panic on nil — the caller
// must pass non-nil).
func NewSynodHandler(svc *rbac.Service, logger *slog.Logger) *SynodHandler {
	if svc == nil {
		panic("handlers.NewSynodHandler: rbac.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &SynodHandler{svc: svc, logger: logger}
}

// SynodCreateInput — the NATIVE request form of POST /v1/synods (handler-native T5d).
// name is required; Description — `*string` (nil → no description), parity with the legacy decode.
type SynodCreateInput struct {
	Name        string
	Description *string
}

// SynodUpdateInput — the NATIVE request form of PATCH /v1/synods/{name} (handler-native
// T5d). Description is required (ONLY it is mutated; name (PK) is immutable — from the path).
type SynodUpdateInput struct {
	Description string
}

// SynodView — the FLAT domain projection of a Synod group (GET /v1/synods items[]),
// handler-native T5d. Description — RAW string (empty = no description); the nullable/
// []-vs-null wire shape is held by the native projection in api (newSynodView).
type SynodView struct {
	Name        string
	Description string
	Builtin     bool
	Roles       []string
	Operators   []string
}

// SynodListPage — the domain list of Synod groups for GET /v1/synods (handler-native T5d).
type SynodListPage struct {
	Items []SynodView
}

// SynodSpecStub — a non-empty *SynodHandler stub for generating the huma OpenAPI
// fragment (HumaSynodSpecYAML): on dump the domain handler is never called, but
// huma.Register requires non-nil for its no-op nil check. svc nil — the handler
// never executes in spec mode (parity with [RoleSpecStub]).
func SynodSpecStub() *SynodHandler {
	return &SynodHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// toSynodResponse projects the domain [rbac.SynodView] into the FLAT [SynodView]
// (handler-native T5d): field-by-field passthrough; the nullable/[]-vs-null wire shape
// (Description always "", roles/operators `[]`) is built by the native projection in api.
// Roles/Operators — non-nil slice (`[]`, not `null`).
func toSynodResponse(v rbac.SynodView) SynodView {
	return SynodView{
		Name:        v.Name,
		Description: v.Description,
		Builtin:     v.Builtin,
		Roles:       emptyIfNil(v.Roles),
		Operators:   emptyIfNil(v.Operators),
	}
}

// SynodCreateReply — the result of [SynodHandler.CreateTyped] (FULL-TYPED expansion,
// ADR-054 §Pattern). The synod.create 201 body is EMPTY (legacy contract: openapi.yaml
// `POST /v1/synods` returns 201 with no `content`), so the reply carries not response
// wire fields but METADATA for the audit payload (group name + creator AID). The 204/201
// body is empty.
type SynodCreateReply struct {
	Name         string
	CreatedByAID string
}

// AuditPayload assembles the audit payload for the create route (parity with legacy SetAuditPayload).
// The SINGLE source for both the (w,r) wrapper AND huma variant B.
func (r SynodCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.Name,
		"created_by_aid": r.CreatedByAID,
	}
}

// CreateTyped — the extracted domain function for POST /v1/synods (FULL-TYPED expansion,
// ADR-054 §Pattern (b)): business logic without http.ResponseWriter/*http.Request.
// claims and req arrive as arguments (decode/auth on the calling layer); errors —
// *problemError (delivered by the huma wrapper via [AsProblemDetails] or the
// (w,r) wrapper via [writeProblemError]), success — [SynodCreateReply].
func (h *SynodHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req SynodCreateInput) (SynodCreateReply, error) {
	var zero SynodCreateReply
	if req.Name == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'name' is required")}
	}

	var description string
	if req.Description != nil {
		description = *req.Description
	}
	if len(description) > rbac.SynodDescriptionMaxLen {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'description' exceeds max length")}
	}
	err := h.svc.CreateSynod(ctx, rbac.CreateSynodInput{
		Name:        req.Name,
		Description: description,
		CallerAID:   claims.Subject,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodAlreadyExists):
		return zero, &problemError{problem.New(problem.TypeSynodExists, "", "synod "+req.Name+" already exists")}
	case errors.Is(err, rbac.ErrInvalidSynodName):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("synod.create: service failed",
			slog.String("name", req.Name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create synod failed")}
	}

	return SynodCreateReply{Name: req.Name, CreatedByAID: claims.Subject}, nil
}

// ListTyped — the domain function for GET /v1/synods (handler-native T5d, READ, no audit):
// reads the group catalog and assembles [SynodListPage] (flat SynodView) without http.
// ResponseWriter/*http.Request. A read error → *problemError (500). The items wire shape
// (toSynodResponse + native projection) is built by api.
func (h *SynodHandler) ListTyped(ctx context.Context) (SynodListPage, error) {
	views, err := h.svc.ListSynods(ctx)
	if err != nil {
		h.logger.Error("synod.list: service failed", slog.Any("error", err))
		return SynodListPage{}, &problemError{problem.New(problem.TypeInternalError, "", "list synods failed")}
	}

	items := make([]SynodView, 0, len(views))
	for _, v := range views {
		items = append(items, toSynodResponse(v))
	}
	return SynodListPage{Items: items}, nil
}

// SynodNameReply — the result of write operations whose audit payload carries only the group
// name (delete). The 204 body is empty; the reply is METADATA for audit.
type SynodNameReply struct {
	Name string
}

// DeleteTyped — the extracted domain function for DELETE /v1/synods/{name} (FULL-TYPED
// expansion, ADR-054 §Pattern (b)). name arrives as an argument; errors — *problemError,
// success — [SynodNameReply] (audit payload). The 204 body is empty.
func (h *SynodHandler) DeleteTyped(ctx context.Context, name string) (SynodNameReply, error) {
	var zero SynodNameReply
	err := h.svc.DeleteSynod(ctx, name)
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodNotFound):
		return zero, &problemError{problem.New(problem.TypeSynodNotFound, "", "synod "+name+" not found")}
	case errors.Is(err, rbac.ErrSynodBuiltin):
		return zero, &problemError{problem.New(problem.TypeSynodBuiltin, "", "synod "+name+" is builtin and cannot be deleted")}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "deleting synod "+name+" would lock out the cluster")}
	default:
		h.logger.Error("synod.delete: service failed",
			slog.String("name", name),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete synod failed")}
	}
	return SynodNameReply{Name: name}, nil
}

// SynodUpdateReply — the result of [SynodHandler.UpdateTyped]: METADATA for the audit
// payload (group name + new description). The 204 body is empty.
type SynodUpdateReply struct {
	Name        string
	Description string
}

// AuditPayload assembles the audit payload for the update route. The SINGLE source for
// both the (w,r) wrapper AND huma variant B (parity with [SynodCreateReply.AuditPayload]).
func (r SynodUpdateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":        r.Name,
		"description": r.Description,
	}
}

// UpdateTyped — the extracted domain function for PATCH /v1/synods/{name} (FULL-TYPED
// expansion, ADR-054 §Pattern (b)): description validation + replace, without
// http.ResponseWriter/*http.Request. claims/name/req arrive as arguments; errors
// — *problemError, success — [SynodUpdateReply] (audit payload).
func (h *SynodHandler) UpdateTyped(ctx context.Context, claims *jwt.Claims, name string, req SynodUpdateInput) (SynodUpdateReply, error) {
	var zero SynodUpdateReply
	if req.Description == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'description' is required")}
	}
	if len(req.Description) > rbac.SynodDescriptionMaxLen {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'description' exceeds max length")}
	}

	err := h.svc.UpdateSynodDescription(ctx, name, req.Description)
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodNotFound):
		return zero, &problemError{problem.New(problem.TypeSynodNotFound, "", "synod "+name+" not found")}
	default:
		h.logger.Error("synod.update: service failed",
			slog.String("name", name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "update synod failed")}
	}

	return SynodUpdateReply{Name: name, Description: req.Description}, nil
}

// SynodOperatorReply — the result of add/remove-operator: METADATA for the audit payload
// (group name + AID; add additionally carries AddedByAID). The 204 body is empty.
type SynodOperatorReply struct {
	Name       string
	AID        string
	AddedByAID string
}

// AddOperatorAuditPayload assembles the audit payload for the add-operator route (carries
// added_by_aid). The SINGLE source for both the (w,r) wrapper AND huma variant B. Separate
// from [RemoveOperatorAuditPayload]: one reply type serves both routes, but their
// payload sets differ (remove does not carry added_by_aid).
func (r SynodOperatorReply) AddOperatorAuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":         r.Name,
		"aid":          r.AID,
		"added_by_aid": r.AddedByAID,
	}
}

// RemoveOperatorAuditPayload assembles the audit payload for the remove-operator route (no
// added_by_aid). The SINGLE source for both the (w,r) wrapper AND huma variant B.
func (r SynodOperatorReply) RemoveOperatorAuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name": r.Name,
		"aid":  r.AID,
	}
}

// AddOperatorTyped — the extracted domain function for POST /v1/synods/{name}/operators
// (FULL-TYPED expansion, ADR-054 §Pattern (b)): AID validation (required + format) +
// binding the member to the group, without http.ResponseWriter/*http.Request. AddedByAID — from
// claims. claims/name/aid arrive as arguments; errors — *problemError, success —
// [SynodOperatorReply]. Idempotent (a repeat is a no-op in the service).
func (h *SynodHandler) AddOperatorTyped(ctx context.Context, claims *jwt.Claims, name, aid string) (SynodOperatorReply, error) {
	var zero SynodOperatorReply
	if aid == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'aid' is required")}
	}
	if !operator.ValidAID(aid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'aid' must match "+operator.AIDPattern)}
	}

	err := h.svc.AddOperator(ctx, rbac.AddOperatorInput{
		SynodName: name,
		AID:       aid,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodNotFound):
		return zero, &problemError{problem.New(problem.TypeSynodNotFound, "", "synod "+name+" not found")}
	case errors.Is(err, rbac.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "operator "+aid+" not found")}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot add an operator to a synod bundling permissions you do not hold yourself")}
	default:
		h.logger.Error("synod.add-operator: service failed",
			slog.String("name", name),
			slog.String("aid", aid),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "add operator failed")}
	}

	return SynodOperatorReply{Name: name, AID: aid, AddedByAID: claims.Subject}, nil
}

// RemoveOperatorTyped — the extracted domain function for DELETE /v1/synods/{name}/
// operators/{aid} (FULL-TYPED expansion, ADR-054 §Pattern (b)): path-AID validation +
// removing the membership row, without http.ResponseWriter/*http.Request. name/aid
// arrive as arguments; errors — *problemError, success — [SynodOperatorReply]
// (audit payload; AddedByAID empty — remove does not carry it).
func (h *SynodHandler) RemoveOperatorTyped(ctx context.Context, name, aid string) (SynodOperatorReply, error) {
	var zero SynodOperatorReply
	if !operator.ValidAID(aid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'aid' must match "+operator.AIDPattern)}
	}

	err := h.svc.RemoveOperator(ctx, rbac.RemoveOperatorInput{
		SynodName: name,
		AID:       aid,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "operator "+aid+" is not a member of synod "+name)}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "removing operator "+aid+" from synod "+name+" would lock out the cluster")}
	default:
		h.logger.Error("synod.remove-operator: service failed",
			slog.String("name", name),
			slog.String("aid", aid),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "remove operator failed")}
	}

	return SynodOperatorReply{Name: name, AID: aid}, nil
}

// SynodRoleReply — the result of grant/revoke-role: METADATA for the audit payload
// (group name + role; grant additionally carries GrantedByAID). The 204 body is empty.
type SynodRoleReply struct {
	Name         string
	Role         string
	GrantedByAID string
}

// GrantRoleAuditPayload assembles the audit payload for the grant-role route (carries
// granted_by_aid). The SINGLE source for both the (w,r) wrapper AND huma variant B. Separate
// from [RevokeRoleAuditPayload]: one reply type serves both routes, but their
// payload sets differ (revoke does not carry granted_by_aid).
func (r SynodRoleReply) GrantRoleAuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.Name,
		"role":           r.Role,
		"granted_by_aid": r.GrantedByAID,
	}
}

// RevokeRoleAuditPayload assembles the audit payload for the revoke-role route (no
// granted_by_aid). The SINGLE source for both the (w,r) wrapper AND huma variant B.
func (r SynodRoleReply) RevokeRoleAuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name": r.Name,
		"role": r.Role,
	}
}

// GrantRoleTyped — the extracted domain function for POST /v1/synods/{name}/roles
// (FULL-TYPED expansion, ADR-054 §Pattern (b)): role validation (required) +
// adding the role to the bundle, without http.ResponseWriter/*http.Request. GrantedByAID —
// from claims. claims/name/role arrive as arguments; errors — *problemError, success
// — [SynodRoleReply]. Idempotent.
func (h *SynodHandler) GrantRoleTyped(ctx context.Context, claims *jwt.Claims, name, role string) (SynodRoleReply, error) {
	var zero SynodRoleReply
	if role == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'role' is required")}
	}

	callerAID := claims.Subject
	err := h.svc.GrantRole(ctx, rbac.GrantRoleInput{
		SynodName: name,
		RoleName:  role,
		CallerAID: callerAID,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodNotFound):
		return zero, &problemError{problem.New(problem.TypeSynodNotFound, "", "synod "+name+" not found")}
	case errors.Is(err, rbac.ErrRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", "role "+role+" not found")}
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return zero, &problemError{problem.New(problem.TypeForbidden, "", "cannot grant a role bundling a permission you do not hold yourself")}
	default:
		h.logger.Error("synod.grant-role: service failed",
			slog.String("name", name),
			slog.String("role", role),
			slog.String("by_aid", callerAID),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "grant role failed")}
	}

	return SynodRoleReply{Name: name, Role: role, GrantedByAID: callerAID}, nil
}

// RevokeRoleTyped — the extracted domain function for DELETE /v1/synods/{name}/roles/
// {role_name} (FULL-TYPED expansion, ADR-054 §Pattern (b)): removing the role from the bundle,
// without http.ResponseWriter/*http.Request. name/role arrive as arguments; errors —
// *problemError, success — [SynodRoleReply] (audit payload; GrantedByAID empty).
func (h *SynodHandler) RevokeRoleTyped(ctx context.Context, name, role string) (SynodRoleReply, error) {
	var zero SynodRoleReply
	err := h.svc.RevokeRole(ctx, rbac.RevokeRoleInput{
		SynodName: name,
		RoleName:  role,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, rbac.ErrSynodRoleNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "role "+role+" is not bundled in synod "+name)}
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "", "revoking role "+role+" from synod "+name+" would lock out the cluster")}
	default:
		h.logger.Error("synod.revoke-role: service failed",
			slog.String("name", name),
			slog.String("role", role),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "revoke role failed")}
	}

	return SynodRoleReply{Name: name, Role: role}, nil
}
