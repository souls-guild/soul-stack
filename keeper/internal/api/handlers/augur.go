// Operator API handlers for the Augur registry (Omen — external system, Rite — grant;
// ADR-025, augur.md §4). The same [augur.Service] backs the MCP tool handlers
// (keeper.augur.omen.* / keeper.augur.rite.*) — one source of truth.
//
// T5d-2c (handler-native): the augur domain is decoupled from the legacy generator.
// *Typed functions take NATIVE request types (handlers.OmenCreateInput / RiteCreateInput;
// the huma-input in package api binds and validates the body against these fields) and
// return domain results with FLAT wire fields (handlers.OmenView / RiteView), NOT a
// legacy-generator Body. Package api builds the native wire-DTO (the OpenAPI schema) from
// these fields (register func huma_augur.go); oapi-generated types play no part in the
// augur domain. The (w,r) wrappers are gone: HTTP is served by huma full-typed, MCP calls
// augur.Service directly (bypassing the handler — Service-direct, not httptest).
//
// Business logic (name/source_type/auth_ref validation, XOR subject, allow-shape,
// token fields) lives in [augur.Service]; the handler does path/query validation and maps
// sentinels to RFC 7807. RBAC lives in middleware (router.go).
//
// SECURITY: the external system's master credential is NOT stored in the registry (only
// auth_ref — a vault-ref, augur.md §4.1); endpoint / auth_ref / allow carry no secrets and
// are logged in audit. Secret values never pass through this path.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// reOmenName — format of the {name} path segment of an Omen (kebab 1..63, augur.NamePattern).
// A path segment without slashes/`..` is traversal-safe.
var reOmenName = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// AugurHandler — REST endpoints of the Augur registry (omens + rites). Delegates
// business logic to [augur.Service]. All dependencies are immutable; safe for
// concurrent use.
type AugurHandler struct {
	svc    *augur.Service
	logger *slog.Logger
}

// NewAugurHandler builds the handler. svc is required (panic on nil — the only
// misconfiguration point; caller must pass non-nil).
func NewAugurHandler(svc *augur.Service, logger *slog.Logger) *AugurHandler {
	if svc == nil {
		panic("handlers.NewAugurHandler: augur.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &AugurHandler{svc: svc, logger: logger}
}

// AugurSpecStub — a non-empty *AugurHandler stub for generating the huma OpenAPI
// fragment (HumaAugurSpecYAML): the domain handler is not called during dump, but
// huma.Register requires non-nil for its nil no-op check. svc is nil — the handler
// never executes in spec mode (parity [OperatorSpecStub]).
func AugurSpecStub() *AugurHandler {
	return &AugurHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// --- Omen -------------------------------------------------------------

// OmenView — FLAT wire form of an Omen (create-201 / list-item / get-200),
// handler-native. created_by_aid is nullable (NULL → key omitted). source_type is a
// flat string (package api projects it into the native enum OmenViewSourceType).
// created_at is UTC + Truncate(Second) (pinned here, as in the operator reference).
type OmenView struct {
	Name         string
	SourceType   string
	Endpoint     string
	AuthRef      string
	CreatedByAID *string
	CreatedAt    time.Time
}

func toOmenView(o *augur.Omen) OmenView {
	return OmenView{
		Name:         o.Name,
		SourceType:   string(o.SourceType),
		Endpoint:     o.Endpoint,
		AuthRef:      o.AuthRef,
		CreatedByAID: o.CreatedByAID,
		CreatedAt:    o.CreatedAt.UTC().Truncate(time.Second),
	}
}

// OmenCreateInput — NATIVE request form for POST /v1/augur/omens (handler-native).
// Replaces OmenCreateRequest: the huma-input (package api) binds and validates the
// body against these fields, then calls CreateOmenTyped. The service validates the
// closed source_type set (domain ValidSourceType).
type OmenCreateInput struct {
	Name       string
	SourceType string
	Endpoint   string
	AuthRef    string
}

// OmenCreateReply — extracted result of [AugurHandler.CreateOmenTyped]
// (handler-native). Carries the flat 201 view (View) + caller AID (for audit-payload).
type OmenCreateReply struct {
	View      OmenView
	CallerAID string
}

// AuditPayload builds the audit-payload for the omen.create route (legacy parity:
// name/source_type/endpoint/auth_ref/created_by_aid; no secrets, augur.md §8).
func (r OmenCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.View.Name,
		"source_type":    r.View.SourceType,
		"endpoint":       r.View.Endpoint,
		"auth_ref":       r.View.AuthRef,
		"created_by_aid": r.CallerAID,
	}
}

// CreateOmenTyped — domain function POST /v1/augur/omens (handler-native):
// svc.CreateOmen + sentinel→problem. Errors are *problemError; success is
// [OmenCreateReply] (flat 201 view + audit fields).
func (h *AugurHandler) CreateOmenTyped(ctx context.Context, claims *keeperjwt.Claims, req OmenCreateInput) (OmenCreateReply, error) {
	var zero OmenCreateReply
	callerAID := claims.Subject
	o, err := h.svc.CreateOmen(ctx, augur.CreateOmenInput{
		Name:       req.Name,
		SourceType: req.SourceType,
		Endpoint:   req.Endpoint,
		AuthRef:    req.AuthRef,
		CallerAID:  &callerAID,
	})
	if err != nil {
		return zero, h.omenError("augur.omen.create", req.Name, callerAID, err)
	}
	return OmenCreateReply{View: toOmenView(o), CallerAID: callerAID}, nil
}

// OmenListPage — domain paged result of GET /v1/augur/omens (handler-native).
// Flat offset/limit/total + a slice of OmenView; package api projects it into the
// native envelope OmenListReply.
type OmenListPage struct {
	Items  []OmenView
	Offset int
	Limit  int
	Total  int
}

// ListOmensTyped — domain function GET /v1/augur/omens (handler-native, read with
// typed query, no audit). offset/limit arrive already validated (huma-bind int32);
// CheckPageBounds enforces the range → 400. A read error →
// *problemError (500).
func (h *AugurHandler) ListOmensTyped(ctx context.Context, offset, limit int) (OmenListPage, error) {
	var zero OmenListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	omens, total, err := h.svc.ListOmens(ctx, offset, limit)
	if err != nil {
		h.logger.Error("augur.omen.list: service failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list omens failed")}
	}

	items := make([]OmenView, 0, len(omens))
	for _, o := range omens {
		items = append(items, toOmenView(o))
	}
	return OmenListPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// GetOmenTyped — domain function GET /v1/augur/omens/{name} (handler-native,
// read with path, no audit): path-name validation + svc.GetOmen + sentinel→problem
// (404/422/500). Errors are *problemError; success is [OmenView].
func (h *AugurHandler) GetOmenTyped(ctx context.Context, name string) (OmenView, error) {
	var zero OmenView
	if !reOmenName.MatchString(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'name' must match "+reOmenName.String())}
	}
	o, err := h.svc.GetOmen(ctx, name)
	switch {
	case err == nil:
		return toOmenView(o), nil
	case errors.Is(err, augur.ErrOmenNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "omen "+name+" not found")}
	default:
		h.logger.Error("augur.omen.get: service failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get omen failed")}
	}
}

// OmenDeleteReply — extracted result of [AugurHandler.DeleteOmenTyped]
// (handler-native). Carries audit fields (HTTP response is an empty 204 body).
type OmenDeleteReply struct {
	Name string
}

// AuditPayload builds the audit-payload for the omen.delete route (legacy parity: name).
func (r OmenDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// DeleteOmenTyped — domain function DELETE /v1/augur/omens/{name} (handler-
// native): path-name validation + svc.DeleteOmen + sentinel→problem. Errors are
// *problemError; success is [OmenDeleteReply].
func (h *AugurHandler) DeleteOmenTyped(ctx context.Context, name string) (OmenDeleteReply, error) {
	var zero OmenDeleteReply
	if !reOmenName.MatchString(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'name' must match "+reOmenName.String())}
	}
	err := h.svc.DeleteOmen(ctx, name)
	switch {
	case err == nil:
		return OmenDeleteReply{Name: name}, nil
	case errors.Is(err, augur.ErrOmenNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "omen "+name+" not found")}
	default:
		h.logger.Error("augur.omen.delete: service failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete omen failed")}
	}
}

// omenError maps [augur.Service] sentinels (Omen create) to *problemError:
//   - ErrValidation       → validation-failed (422).
//   - ErrOmenAlreadyExists → omen-already-exists (409).
//
// For unknown errors — internal-error (500) + generic detail (raw err.Error()
// is not surfaced to the client; diagnostics go to the logs). Delivered by the huma
// wrapper via AsProblemDetails.
func (h *AugurHandler) omenError(op, name, callerAID string, err error) error {
	switch {
	case errors.Is(err, augur.ErrValidation):
		return &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	case errors.Is(err, augur.ErrOmenAlreadyExists):
		return &problemError{problem.New(problem.TypeOmenExists, "", "omen "+name+" already exists")}
	default:
		h.logger.Error(op+": service failed",
			slog.String("name", name), slog.String("by_aid", callerAID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", op+" failed")}
	}
}

// --- Rite -------------------------------------------------------------

// RiteView — FLAT wire form of a Rite (create-201 / list-item), handler-native.
// allow is byte-passthrough JSONB ([json.RawMessage], ADR-051 category D): the domain's
// raw bytes travel as-is, without unmarshal→map→marshal (re-marshal would reorder the
// keys — PG JSONB canonicalization ≠ Go `map`-marshal lexicographic order).
// coven/sid/token_*/created_by_aid are nullable (nil → key omitted). created_at is
// UTC + Truncate(Second).
type RiteView struct {
	ID           int64
	Omen         string
	Coven        *string
	SID          *string
	Allow        json.RawMessage
	Delegate     bool
	TokenTTL     *string
	TokenNumUses *int
	CreatedByAID *string
	CreatedAt    time.Time
}

func toRiteView(r *augur.Rite) RiteView {
	return RiteView{
		ID:           r.ID,
		Omen:         r.Omen,
		Coven:        r.Coven,
		SID:          r.SID,
		Allow:        r.Allow,
		Delegate:     r.Delegate,
		TokenTTL:     r.TokenTTL,
		TokenNumUses: r.TokenNumUses,
		CreatedByAID: r.CreatedByAID,
		CreatedAt:    r.CreatedAt.UTC().Truncate(time.Second),
	}
}

// RiteCreateInput — NATIVE request form for POST /v1/augur/rites (handler-native).
// Replaces RiteCreateRequest: subject is XOR coven/sid; allow is
// `json.RawMessage` (byte-passthrough JSONB, ADR-051 category D); delegate is
// pointer-optional (omitted → false). The service validates XOR subject / allow-shape /
// token fields.
type RiteCreateInput struct {
	Omen         string
	Coven        *string
	SID          *string
	Allow        json.RawMessage
	Delegate     *bool
	TokenTTL     *string
	TokenNumUses *int
}

// RiteCreateReply — extracted result of [AugurHandler.CreateRiteTyped]
// (handler-native). Carries the flat 201 view (View) + subject and caller AID (for
// audit-payload; the allow-list is NOT put into audit, augur.md §8).
type RiteCreateReply struct {
	View      RiteView
	Subject   string
	CallerAID string
}

// AuditPayload builds the audit-payload for the rite.create route (legacy parity:
// id/omen/subject/delegate/created_by_aid; allow is NOT included).
func (r RiteCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"id":             r.View.ID,
		"omen":           r.View.Omen,
		"subject":        r.Subject,
		"delegate":       r.View.Delegate,
		"created_by_aid": r.CallerAID,
	}
}

// CreateRiteTyped — domain function POST /v1/augur/rites (handler-native):
// svc.CreateRite + sentinel→problem. allow is byte-passthrough JSONB (ADR-051
// category D), passed straight to the service validator. Errors are *problemError;
// success is [RiteCreateReply] (flat 201 view + audit fields).
func (h *AugurHandler) CreateRiteTyped(ctx context.Context, claims *keeperjwt.Claims, req RiteCreateInput) (RiteCreateReply, error) {
	var zero RiteCreateReply
	callerAID := claims.Subject
	rite, err := h.svc.CreateRite(ctx, augur.CreateRiteInput{
		Omen:         req.Omen,
		Coven:        req.Coven,
		SID:          req.SID,
		Allow:        req.Allow,
		Delegate:     req.Delegate != nil && *req.Delegate,
		TokenTTL:     req.TokenTTL,
		TokenNumUses: req.TokenNumUses,
		CallerAID:    &callerAID,
	})
	if err != nil {
		return zero, h.riteError("augur.rite.create", callerAID, err)
	}
	return RiteCreateReply{View: toRiteView(rite), Subject: riteSubject(rite), CallerAID: callerAID}, nil
}

// RiteListResult — domain result of GET /v1/augur/rites?omen=<name> (handler-
// native, items-only, no pagination). Package api projects it into native RiteListReply.
type RiteListResult struct {
	Items []RiteView
}

// ListRitesTyped — domain function GET /v1/augur/rites?omen=<name> (handler-
// native, read with typed query, no audit). The by-omen filter is REQUIRED in MVP
// (augur.md §6): empty/malformed omen → 422 (domain regex validation). A read
// error → *problemError (500).
func (h *AugurHandler) ListRitesTyped(ctx context.Context, omen string) (RiteListResult, error) {
	var zero RiteListResult
	if !reOmenName.MatchString(omen) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"query param 'omen' is required and must match "+reOmenName.String())}
	}

	rites, err := h.svc.ListRitesByOmen(ctx, omen)
	if err != nil {
		h.logger.Error("augur.rite.list: service failed",
			slog.String("omen", omen), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list rites failed")}
	}

	items := make([]RiteView, 0, len(rites))
	for _, rt := range rites {
		items = append(items, toRiteView(rt))
	}
	return RiteListResult{Items: items}, nil
}

// RiteDeleteReply — extracted result of [AugurHandler.DeleteRiteTyped]
// (handler-native). Carries audit fields (HTTP response is an empty 204 body).
type RiteDeleteReply struct {
	ID int64
}

// AuditPayload builds the audit-payload for the rite.delete route (legacy parity: id).
func (r RiteDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"id": r.ID}
}

// DeleteRiteTyped — domain function DELETE /v1/augur/rites/{id} (handler-
// native): path-id validation (must be a positive integer, else 422) + svc.DeleteRite +
// sentinel→problem. Errors are *problemError; success is [RiteDeleteReply].
func (h *AugurHandler) DeleteRiteTyped(ctx context.Context, rawID string) (RiteDeleteReply, error) {
	var zero RiteDeleteReply
	id, perr := strconv.ParseInt(rawID, 10, 64)
	if perr != nil || id <= 0 {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'id' must be a positive integer")}
	}

	err := h.svc.DeleteRite(ctx, id)
	switch {
	case err == nil:
		return RiteDeleteReply{ID: id}, nil
	case errors.Is(err, augur.ErrRiteNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "rite "+rawID+" not found")}
	default:
		h.logger.Error("augur.rite.delete: service failed",
			slog.Int64("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete rite failed")}
	}
}

// riteError maps [augur.Service] sentinels (Rite create) to *problemError:
//   - ErrValidation   → validation-failed (422).
//   - ErrOmenNotFound → not-found (404; the grant's Omen does not exist).
func (h *AugurHandler) riteError(op, callerAID string, err error) error {
	switch {
	case errors.Is(err, augur.ErrValidation):
		return &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	case errors.Is(err, augur.ErrOmenNotFound):
		return &problemError{problem.New(problem.TypeNotFound, "", "omen of this rite not found")}
	default:
		h.logger.Error(op+": service failed",
			slog.String("by_aid", callerAID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", op+" failed")}
	}
}

// riteSubject — human-readable form of a Rite's subject for the audit-payload
// (`coven=<v>` / `sid=<v>`). XOR is guaranteed by validation; if both are empty
// (theoretically impossible after insert) — empty string.
func riteSubject(r *augur.Rite) string {
	if r.Coven != nil && *r.Coven != "" {
		return "coven=" + *r.Coven
	}
	if r.SID != nil && *r.SID != "" {
		return "sid=" + *r.SID
	}
	return ""
}
