// Package handlers — HTTP handlers for the Operator API (M0.6b).
//
// M0.7: business logic moved into [operator.Service]; the handlers are a thin
// HTTP wrapper (decode request → service call → encode 4xx/2xx). The same
// service is called by the MCP tool handler (keeper/internal/mcp), which
// guarantees a single source of truth for the three endpoints (PM decision
// M0.7 #6, delegation.md spec).
//
// T5d (handler-native PILOT): the operator domain is fully decoupled from the legacy
// generator. *Typed functions take NATIVE request types (assembled by the huma-input in
// package api) and return domain results with FLAT wire fields — NOT a legacy generator
// Body. The native wire-DTO (the OpenAPI schema) is built by package api from those fields
// (register func huma_operator.go); oapi-generated types play no part in the operator
// domain. The (w,r) wrappers are gone: HTTP is served by huma full-typed, MCP calls
// operator.Service directly (bypassing the handler).
//
// RBAC checks happen in middleware (see api/router.go and
// api/middleware/rbac.go); the handler only maps errors to
// RFC 7807.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// OperatorDB — narrow interface over pgxpool.Pool needed by the handler for the
// non-transactional endpoints.
type OperatorDB = operator.ExecQueryRower

// OperatorPool — extends [OperatorDB] with BeginTx, needed by the Revoke handler for the
// atomic self-lockout check.
type OperatorPool interface {
	OperatorDB
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// JWTIssuer — narrow interface over `*keeper/internal/jwt.Issuer`.
// The narrowing is for unit tests (mock without loading a signing key).
type JWTIssuer interface {
	Issue(aid string, roles []string, ttl time.Duration, bootstrapInitial bool) (string, error)
}

// RBACSource — narrow surface of the rbac service for handler-side helpers.
// Only RolesOf is needed (passed to operator.Service for JWT issuance); the
// lockout probe takes the admin set from the DB (Slice 3), not from an in-memory snapshot.
type RBACSource interface {
	RolesOf(aid string) []string
}

// ProvisioningGate — narrow surface of the provisioning_allowed_methods policy
// (ADR-058 Part B): gates the operator CREATE branch. Implemented by
// *serviceregistry.Holder; declared locally so handlers doesn't pull in
// serviceregistry. nil → gate off (policy not configured / tests, back-compat —
// CreateTyped lets it through).
type ProvisioningGate interface {
	ProvisioningMethodAllowed(method string) bool
}

// OperatorHandler — the three Operator API endpoints. Delegates business logic to
// [operator.Service].
//
// All dependencies are immutable; safe for concurrent use, since it holds no state
// between requests.
type OperatorHandler struct {
	svc    *operator.Service
	logger *slog.Logger

	// gate — the provisioning_allowed_methods policy (ADR-058 Part B), gates CreateTyped
	// (method "user"). nil → gate off (back-compat). Injected via [SetProvisioningGate]
	// late-binding: the Holder in `keeper run` comes up as a separate setup step, the
	// constructor signature does not change.
	gate ProvisioningGate
}

// SetProvisioningGate late-binds the provisioning_allowed_methods policy (ADR-058 Part
// B). nil — remove the gate (back-compat: create by any method). Called from `keeper run`
// after serviceregistry.Holder comes up. Idempotent; no thread-safety needed — called
// before the HTTP server starts.
func (h *OperatorHandler) SetProvisioningGate(gate ProvisioningGate) {
	h.gate = gate
}

// NewOperatorHandler creates a handler. ttlDefault — TTL of JWT tokens. Internally it
// assembles [operator.Service] (one per handler).
//
// The old signature (pool / issuer / rbacSrc / ttlDefault / logger) is kept for binary
// compatibility with the keeper/cmd/keeper wire-up and unit tests
// (handlers/operator_test.go) — the service object is created behind the scenes.
func NewOperatorHandler(pool OperatorPool, issuer JWTIssuer, rbacSrc RBACSource, ttlDefault time.Duration, logger *slog.Logger) *OperatorHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       pool,
		Issuer:     issuer,
		RBAC:       rbacSrc,
		TTLDefault: ttlDefault,
		Logger:     logger,
	})
	if err != nil {
		// Single point of misconfiguration — the caller (NewServer) already
		// validates non-nil deps; the real path should not reach here, but
		// panicking here beats a silent misconfiguration.
		panic(fmt.Sprintf("handlers.NewOperatorHandler: %v", err))
	}
	return &OperatorHandler{svc: svc, logger: logger}
}

// Service returns the inner [operator.Service]. Used by the MCP server wire-up in
// keeper/cmd/keeper to reuse the same instance (single source of truth, see
// delegation.md PM decision #6).
func (h *OperatorHandler) Service() *operator.Service { return h.svc }

// OperatorSpecStub — a non-empty *OperatorHandler stub for generating the huma-OpenAPI
// fragment (HumaOperatorSpecYAML): on dump the domain handler is not called, but
// huma.Register requires non-nil for its no-op nil check. svc nil — the handler never
// executes in spec mode (parity with [RoleSpecStub]).
func OperatorSpecStub() *OperatorHandler {
	return &OperatorHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// maxDisplayNameLen — upper bound on an Archon's `display_name`. An empty
// display_name is legitimate (the service substitutes the AID); an overly long one is
// junk/DoS in UI lists. 200 chars — ample for "First Last (team)".
const maxDisplayNameLen = 200

// OperatorCreateInput — NATIVE request form of POST /v1/operators (handler-native
// PILOT T5d). Replaces OperatorCreateRequest: the huma-input (package api) binds and
// validates the body against these fields, then calls CreateTyped. Roles — a flat
// []string (huma omitempty: empty/omitted → nil → "no roles", legacy parity).
type OperatorCreateInput struct {
	AID         string
	DisplayName string
	Roles       []string
}

// OperatorCreateReply — extracted result of [OperatorHandler.CreateTyped]
// (handler-native PILOT). Carries the FLAT wire fields of the 201 body (api builds the
// native OperatorCreateReply schema from them) + audit-payload fields (set by the
// middleware: huma variant B). GrantedRoles serves both: the roles wire field (omitempty)
// and the audit payload.
type OperatorCreateReply struct {
	AID          string
	DisplayName  string
	AuthMethod   string
	CreatedAt    time.Time
	CreatedByAID string
	JWT          string
	GrantedRoles []string
}

// CreateTyped — domain function for POST /v1/operators (handler-native PILOT):
// business logic without http.ResponseWriter/*http.Request. claims and req arrive as
// arguments; errors — *problemError (delivered by the huma wrapper via
// [AsProblemDetails]), success — [OperatorCreateReply] (flat wire fields + audit).
func (h *OperatorHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req OperatorCreateInput) (OperatorCreateReply, error) {
	var zero OperatorCreateReply
	if req.AID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'aid' is required")}
	}
	if !operator.ValidAID(req.AID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'aid' must match "+operator.AIDPattern)}
	}
	// display_name is optional (empty → the service substitutes the AID); we bound only
	// the upper length, to keep junk out of the registry / UI.
	if len(req.DisplayName) > maxDisplayNameLen {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			fmt.Sprintf("field 'display_name' must be at most %d characters", maxDisplayNameLen))}
	}

	// provisioning_allowed_methods policy gate (ADR-058 Part B): creating an operator via
	// the Operator API is the "user" method (created_via=user). gate==nil → let it through
	// (policy not configured, back-compat). The bootstrap path (`keeper init`) does NOT
	// come here — it does not call CreateTyped.
	if h.gate != nil && !h.gate.ProvisioningMethodAllowed("user") {
		return zero, &problemError{problem.New(problem.TypeProvisioningMethodDisabled, "",
			"operator provisioning via 'user' method is disabled by policy")}
	}

	res, err := h.svc.Create(ctx, operator.CreateInput{
		AID:         req.AID,
		DisplayName: req.DisplayName,
		CallerAID:   claims.Subject,
		Roles:       req.Roles,
	})
	if err != nil {
		switch {
		case errors.Is(err, operator.ErrOperatorAlreadyExists):
			return zero, &problemError{problem.New(problem.TypeOperatorExists, "",
				"operator with this AID already exists")}
		// roles[]: a non-existent role — validation-failed (422) naming which role
		// was not found. The atomic create+grant already rolled the tx back —
		// the operator is NOT created.
		case errors.Is(err, rbac.ErrRoleNotFound):
			return zero, &problemError{problem.New(problem.TypeRoleNotFound, "", err.Error())}
		// FK violation on role-grant aid → the operator does not exist. On the
		// create+grant path this would mean an INSERT/grant desync in one tx —
		// impossible by construction, but we guard with an explicit 404 mapping.
		case errors.Is(err, rbac.ErrOperatorNotFound):
			return zero, &problemError{problem.New(problem.TypeNotFound, "", err.Error())}
		// invalid role name from pre-validation — validation-failed.
		case strings.Contains(err.Error(), "invalid role name"):
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		}
		h.logger.Error("operator.create: service failed",
			slog.String("aid", req.AID),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create operator failed")}
	}

	return OperatorCreateReply{
		AID:          res.AID,
		DisplayName:  res.DisplayName,
		AuthMethod:   string(res.AuthMethod),
		CreatedAt:    res.CreatedAt.UTC().Truncate(time.Second),
		CreatedByAID: res.CreatedByAID,
		JWT:          res.JWT,
		GrantedRoles: res.GrantedRoles,
	}, nil
}

// AuditPayload assembles the audit payload of the create route (parity with legacy
// SetAuditPayload). The SINGLE source for huma variant B (uniform with OperatorRevokeReply/
// OperatorIssueTokenReply.AuditPayload()).
func (r OperatorCreateReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{
		"aid":            r.AID,
		"display_name":   r.DisplayName,
		"auth_method":    r.AuthMethod,
		"created_by_aid": r.CreatedByAID,
	}
	if len(r.GrantedRoles) > 0 {
		p["roles"] = r.GrantedRoles
	}
	return p
}

// OperatorRevokeReply — extracted result of [OperatorHandler.RevokeTyped]
// (handler-native PILOT). Carries audit fields (the HTTP response is an empty 204 body).
type OperatorRevokeReply struct {
	AID    string
	Reason string
}

// AuditPayload assembles the audit payload of the revoke route (parity with legacy: aid +
// optional reason). Source for huma variant B.
func (r OperatorRevokeReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{"aid": r.AID}
	if r.Reason != "" {
		p["reason"] = r.Reason
	}
	return p
}

// RevokeTyped — domain function for POST /v1/operators/{aid}/revoke (handler-native
// PILOT): path-AID validation + svc.Revoke + sentinel→problem. Errors — *problemError;
// success — [OperatorRevokeReply] (audit fields).
func (h *OperatorHandler) RevokeTyped(ctx context.Context, claims *jwt.Claims, targetAID, reason string) (OperatorRevokeReply, error) {
	var zero OperatorRevokeReply
	if !operator.ValidAID(targetAID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'aid' must match "+operator.AIDPattern)}
	}

	err := h.svc.Revoke(ctx, operator.RevokeInput{
		AID:       targetAID,
		Reason:    reason,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, operator.ErrWouldLockOutCluster):
		return zero, &problemError{problem.New(problem.TypeWouldLockOutCluster, "",
			"target is the last active cluster-admin; revoking would lock out the cluster")}
	case errors.Is(err, operator.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "",
			"operator "+targetAID+" not found")}
	case errors.Is(err, operator.ErrOperatorAlreadyRevoked):
		return zero, &problemError{problem.New(problem.TypeOperatorRevoked, "",
			"operator "+targetAID+" is already revoked")}
	default:
		h.logger.Error("operator.revoke: service failed",
			slog.String("aid", targetAID),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "revoke failed")}
	}

	return OperatorRevokeReply{AID: targetAID, Reason: reason}, nil
}

// OperatorIssueTokenReply — extracted result of [OperatorHandler.IssueTokenTyped]
// (handler-native PILOT). Carries the FLAT wire fields of the 200 body (api builds the
// native IssueTokenReply schema) + audit fields.
type OperatorIssueTokenReply struct {
	AID       string
	JWT       string
	ExpiresAt time.Time
}

// AuditPayload assembles the audit payload of the issue-token route (parity with legacy:
// aid + expires_at RFC3339). WITHOUT the JWT itself (SENSITIVE). Source for huma variant B.
func (r OperatorIssueTokenReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"aid":        r.AID,
		"expires_at": r.ExpiresAt.Format(time.RFC3339),
	}
}

// IssueTokenTyped — domain function for POST /v1/operators/{aid}/issue-token
// (handler-native PILOT): path-AID validation + svc.IssueToken + sentinel→problem.
// Errors — *problemError; success — [OperatorIssueTokenReply] (wire fields + audit).
func (h *OperatorHandler) IssueTokenTyped(ctx context.Context, claims *jwt.Claims, targetAID string) (OperatorIssueTokenReply, error) {
	var zero OperatorIssueTokenReply
	if !operator.ValidAID(targetAID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'aid' must match "+operator.AIDPattern)}
	}
	res, err := h.svc.IssueToken(ctx, operator.IssueTokenInput{
		AID:       targetAID,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		// fall through.
	case errors.Is(err, operator.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "",
			"operator "+targetAID+" not found")}
	case errors.Is(err, operator.ErrOperatorAlreadyRevoked):
		return zero, &problemError{problem.New(problem.TypeOperatorRevoked, "",
			"operator "+targetAID+" is revoked")}
	default:
		h.logger.Error("operator.issue-token: service failed",
			slog.String("aid", targetAID),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue JWT failed")}
	}

	return OperatorIssueTokenReply{
		AID:       res.AID,
		JWT:       res.JWT,
		ExpiresAt: res.ExpiresAt.UTC().Truncate(time.Second),
	}, nil
}

// OperatorView — FLAT wire form of an Operator (list-item / get-200), handler-native
// PILOT (replaces the Operator alias). Nullable fields mirror NULL in the DB:
// created_by_aid (NULL for bootstrap/system/federated — ADR-058(d) legalized NULL for
// non-bootstrap rows), revoked_at (NULL for an active one). bootstrap_initial — a derived
// flag `op.IsBootstrap()` (created_via='bootstrap') for the UI: there is no separate DB
// column; uniqueness of the first Archon is guaranteed by the partial unique index
// `WHERE created_via='bootstrap'` (ADR-013/ADR-014 amendment 2026-06-23, migration 085).
// The flag was moved off the former `created_by_aid IS NULL` — otherwise federated/system
// operators with a NULL parent gave a false bootstrap flag. Package api projects
// OperatorView → the native Operator schema (register func); the wire form (UTC +
// Truncate(Second) on date-time) is pinned here.
type OperatorView struct {
	AID              string
	AuthMethod       string
	BootstrapInitial bool
	CreatedAt        time.Time
	CreatedByAID     *string
	CreatedVia       string
	DisplayName      string
	Metadata         map[string]any
	RevokedAt        *time.Time
}

func toOperatorView(op *operator.Operator) OperatorView {
	out := OperatorView{
		AID:              op.AID,
		DisplayName:      op.DisplayName,
		AuthMethod:       string(op.AuthMethod),
		CreatedAt:        op.CreatedAt.UTC().Truncate(time.Second),
		CreatedByAID:     op.CreatedByAID,
		CreatedVia:       op.CreatedVia,
		BootstrapInitial: op.IsBootstrap(),
		Metadata:         op.Metadata,
	}
	if op.RevokedAt != nil {
		t := op.RevokedAt.UTC().Truncate(time.Second)
		out.RevokedAt = &t
	}
	return out
}

// OperatorListPage — domain paged result of GET /v1/operators (handler-native PILOT).
// Flat offset/limit/total + a slice of OperatorView; package api projects it into the
// native envelope (PagedResponse[api.Operator] → the OperatorListReply schema).
type OperatorListPage struct {
	Items  []OperatorView
	Offset int
	Limit  int
	Total  int
}

// ListTyped — domain function for GET /v1/operators (handler-native PILOT, read with
// typed query, no audit). filter/offset/limit arrive already validated (huma-bind:
// auth_method enum→422, revoked bool→400, pagination int32; the offset/limit range is
// enforced by this layer via CheckPageBounds). A read error → *problemError (500).
func (h *OperatorHandler) ListTyped(ctx context.Context, filter operator.ListFilter, offset, limit int) (OperatorListPage, error) {
	var zero OperatorListPage

	// Pagination range (offset≥0, limit∈[1,1000]) — the SINGLE source of bounds
	// sharedapi.CheckPageBounds (same as ParsePage). Out-of-range → 400
	// TypeMalformedRequest (contract invariant: a huma typed-int carries NO schema
	// minimum/maximum, otherwise it would return 422 — a wire change vs legacy/strict 400).
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	ops, total, err := h.svc.List(ctx, filter, offset, limit)
	if err != nil {
		h.logger.Error("operator.list: service failed",
			slog.Int("offset", offset),
			slog.Int("limit", limit),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list operators failed")}
	}

	items := make([]OperatorView, 0, len(ops))
	for _, op := range ops {
		items = append(items, toOperatorView(op))
	}
	return OperatorListPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// GetTyped — domain function for GET /v1/operators/{aid} (handler-native PILOT,
// READ variant without audit): path-AID validation + svc.Get + sentinel→problem
// (404/500). Errors — *problemError; success — [OperatorView] (200 body).
func (h *OperatorHandler) GetTyped(ctx context.Context, targetAID string) (OperatorView, error) {
	var zero OperatorView
	if !operator.ValidAID(targetAID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'aid' must match "+operator.AIDPattern)}
	}
	op, err := h.svc.Get(ctx, targetAID)
	switch {
	case err == nil:
		return toOperatorView(op), nil
	case errors.Is(err, operator.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "",
			"operator "+targetAID+" not found")}
	default:
		h.logger.Error("operator.get: service failed",
			slog.String("aid", targetAID),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get operator failed")}
	}
}
