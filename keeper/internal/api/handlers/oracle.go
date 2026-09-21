// Operator API handlers for the Oracle registries (Vigil — a Soul-side check, Decree —
// a reactor rule; ADR-030, beacons S3). The same [oracle.Service] backs the MCP tool
// handlers (keeper.oracle.vigil.* / keeper.oracle.decree.*), one source of truth.
//
// T5d-2c (handler-native): the oracle domain is detached from the legacy generator.
// *Typed functions take NATIVE request types (handlers.VigilCreateInput / DecreeCreateInput;
// the huma input in package api binds and validates the body against these fields) and
// return domain results with FLAT wire fields (handlers.VigilView / DecreeView) — NOT a
// legacy-generator Body. The native wire-DTO (the OpenAPI schema) is built by package api
// from these fields (register func huma_oracle.go); oapi-generated types take no part in the
// oracle domain. The (w,r) wrappers are gone: HTTP is served by huma full-typed, MCP calls
// oracle.Service directly (bypassing the handler — Service-direct, not httptest).
//
// Business logic (validation of name/interval/check/subject for Vigil; name/on_beacon/
// incarnation_name/scenario/subject/where-CEL for Decree) lives in [oracle.Service]; the
// handler does path/query validation and maps sentinels to RFC 7807. RBAC is in the
// middleware (router.go).
//
// SECURITY: a Vigil's params / a Decree's action_input are check/scenario configuration,
// not a secret; a vault-ref in action_input travels AS-IS (invariant A of ADR-027), secret
// values do not pass through this path.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/subject"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// reOracleName is the format of the {name} path segment for Vigil / Decree (kebab 1..63,
// oracle.IDPattern). A path segment with no slashes/`..` is traversal-safe.
var reOracleName = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// OracleHandler holds the REST endpoints for the Oracle registries (vigils + decrees).
// Delegates business logic to [oracle.Service]. All dependencies are immutable; safe for
// concurrent use.
type OracleHandler struct {
	svc    *oracle.Service
	logger *slog.Logger
}

// NewOracleHandler creates the handler. svc is required (panic on nil — the only
// misconfiguration point; the caller must pass non-nil).
func NewOracleHandler(svc *oracle.Service, logger *slog.Logger) *OracleHandler {
	if svc == nil {
		panic("handlers.NewOracleHandler: oracle.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &OracleHandler{svc: svc, logger: logger}
}

// OracleSpecStub is a non-nil *OracleHandler stub for generating the huma-OpenAPI
// fragment (HumaOracleSpecYAML): on dump the domain handler is not called, but
// huma.Register requires non-nil for its nil no-op check. svc nil — the handler
// never executes in spec mode (parity with [AugurSpecStub]).
func OracleSpecStub() *OracleHandler {
	return &OracleHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// --- Vigil ------------------------------------------------------------

// VigilView is the FLAT wire form of a Vigil (create-201 / list-item / get-200),
// handler-native. Subject is the resolved four-dimension selector (NIM-280) — package
// api projects it into the nested wire object. CreatedByAID — *string nullable (nil →
// key omitted). params — byte-passthrough JSONB ([json.RawMessage], ADR-051 category
// D): raw bytes are returned as-is, without unmarshal→map→marshal (a re-marshal would
// reorder keys). created_at/updated_at — UTC + Truncate(Second) (pinned here, as in the
// oracle (w,r) reference).
type VigilView struct {
	ID string
	// Label — display caption (ADR-0085); nil when the column is NULL, and the
	// consumer then shows Name.
	Label        *string
	Subject      subject.Selector
	Interval     string
	Check        string
	Params       json.RawMessage
	Enabled      bool
	CreatedByAID *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func toVigilView(v *oracle.Vigil) VigilView {
	params := v.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	return VigilView{
		ID:           v.ID,
		Label:        v.Label,
		Subject:      v.Subject(),
		Interval:     v.IntervalSpec,
		Check:        v.CheckAddr,
		Params:       params,
		Enabled:      v.Enabled,
		CreatedByAID: v.CreatedByAID,
		CreatedAt:    v.CreatedAt.UTC().Truncate(time.Second),
		UpdatedAt:    v.UpdatedAt.UTC().Truncate(time.Second),
	}
}

// VigilCreateInput is the NATIVE request form of POST /v1/vigils (handler-native).
// Replaces VigilCreateRequest: Subject — exactly one of the four dimensions
// ([subject.Selector]); params — `json.RawMessage` (byte-passthrough JSONB, ADR-051
// category D); enabled — pointer-optional (omitted → true). The subject and the form of
// interval/check/params are validated by the service.
type VigilCreateInput struct {
	ID string
	// Label — optional display caption (ADR-0085): free text, changed afterwards
	// by PUT /v1/vigils/{id}/label.
	Label    *string
	Subject  subject.Selector
	Interval string
	Check    string
	Params   *json.RawMessage
	Enabled  *bool
}

// VigilCreateReply is the extracted result of [OracleHandler.CreateVigilTyped]
// (handler-native). Carries the flat 201 view (View) + check/interval/subject + the caller
// AID (for the audit payload; params is NOT put in audit).
type VigilCreateReply struct {
	View      VigilView
	Check     string
	Interval  string
	Subject   string
	CallerAID string
}

// AuditPayload builds the audit payload for the vigil.create route (legacy parity:
// name/check/interval/subject/created_by_aid; params is NOT included).
func (r VigilCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"id":             r.View.ID,
		"label":          r.View.Label,
		"check":          r.Check,
		"interval":       r.Interval,
		"subject":        r.Subject,
		"created_by_aid": r.CallerAID,
	}
}

// CreateVigilTyped is the domain function for POST /v1/vigils (handler-native):
// svc.CreateVigil + sentinel→problem. params — byte-passthrough JSONB (ADR-051
// category D). Errors are *problemError; success is [VigilCreateReply] (the flat 201 view
// + audit fields).
func (h *OracleHandler) CreateVigilTyped(ctx context.Context, claims *keeperjwt.Claims, req VigilCreateInput) (VigilCreateReply, error) {
	var zero VigilCreateReply
	callerAID := claims.Subject
	v, err := h.svc.CreateVigil(ctx, oracle.CreateVigilInput{
		ID:        req.ID,
		Label:     req.Label,
		Subject:   req.Subject,
		Interval:  req.Interval,
		Check:     req.Check,
		Params:    derefRawMessage(req.Params),
		Enabled:   enabledOrDefault(req.Enabled),
		CallerAID: &callerAID,
	})
	if err != nil {
		return zero, h.vigilError("oracle.vigil.create", req.ID, callerAID, err)
	}
	return VigilCreateReply{
		View:      toVigilView(v),
		Check:     v.CheckAddr,
		Interval:  v.IntervalSpec,
		Subject:   v.Subject().String(),
		CallerAID: callerAID,
	}, nil
}

// VigilListPage is the domain paged result of GET /v1/vigils (handler-native). Flat
// offset/limit/total + a slice of VigilView; package api projects it into the native
// envelope VigilListReply.
type VigilListPage struct {
	Items  []VigilView
	Offset int
	Limit  int
	Total  int
}

// ListVigilsTyped is the domain function for GET /v1/vigils (handler-native, read with
// typed query, no audit). offset/limit arrive already validated (huma-bind int32); the
// range is enforced by CheckPageBounds → 400. A read error → *problemError (500).
func (h *OracleHandler) ListVigilsTyped(ctx context.Context, offset, limit int) (VigilListPage, error) {
	var zero VigilListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	vigils, total, err := h.svc.ListVigils(ctx, offset, limit)
	if err != nil {
		h.logger.Error("oracle.vigil.list: service failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list vigils failed")}
	}

	items := make([]VigilView, 0, len(vigils))
	for _, v := range vigils {
		items = append(items, toVigilView(v))
	}
	return VigilListPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// GetVigilTyped is the domain function for GET /v1/vigils/{id} (handler-native, read with
// path, no audit): path-name validation + svc.GetVigil + sentinel→problem (404/422/500).
// Errors are *problemError; success is [VigilView].
func (h *OracleHandler) GetVigilTyped(ctx context.Context, id string) (VigilView, error) {
	var zero VigilView
	if !reOracleName.MatchString(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'id' must match "+reOracleName.String())}
	}
	v, err := h.svc.GetVigil(ctx, id)
	switch {
	case err == nil:
		return toVigilView(v), nil
	case errors.Is(err, oracle.ErrVigilNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "vigil "+id+" not found")}
	default:
		h.logger.Error("oracle.vigil.get: service failed",
			slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get vigil failed")}
	}
}

// VigilDeleteReply is the extracted result of [OracleHandler.DeleteVigilTyped]
// (handler-native). Carries the audit fields (the HTTP response is an empty 204 body).
type VigilDeleteReply struct {
	ID string
}

// AuditPayload builds the audit payload for the vigil.delete route (legacy parity: name).
func (r VigilDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"id": r.ID}
}

// DeleteVigilTyped is the domain function for DELETE /v1/vigils/{id} (handler-native):
// path-name validation + svc.DeleteVigil + sentinel→problem. Errors are *problemError;
// success is [VigilDeleteReply].
func (h *OracleHandler) DeleteVigilTyped(ctx context.Context, id string) (VigilDeleteReply, error) {
	var zero VigilDeleteReply
	if !reOracleName.MatchString(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'id' must match "+reOracleName.String())}
	}
	err := h.svc.DeleteVigil(ctx, id)
	switch {
	case err == nil:
		return VigilDeleteReply{ID: id}, nil
	case errors.Is(err, oracle.ErrVigilNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "vigil "+id+" not found")}
	default:
		h.logger.Error("oracle.vigil.delete: service failed",
			slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete vigil failed")}
	}
}

// SetVigilLabelTyped — domain function for PUT /v1/vigils/{id}/label
// (WRITE+AUDIT vigil.label_changed). 404 if absent.
//
// The label itself is NOT validated: free text with capitals, spaces and
// punctuation is what the field carries (ADR-0085), so the only 422 this route
// can raise is on the path identifier, which must still be a well-formed name
// because it addresses the row and is what a Decree's on_beacon points at.
func (h *OracleHandler) SetVigilLabelTyped(ctx context.Context, id string, req LabelSetInput) (LabelWriteReply[VigilView], error) {
	var zero LabelWriteReply[VigilView]
	if !reOracleName.MatchString(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'id' must match "+reOracleName.String())}
	}
	v, previous, err := h.svc.SetVigilLabel(ctx, id, req.Label)
	switch {
	case err == nil:
		return LabelWriteReply[VigilView]{Body: toVigilView(v), ID: id, Label: v.Label, Previous: previous}, nil
	case errors.Is(err, oracle.ErrVigilNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "vigil "+id+" not found")}
	default:
		h.logger.Error("oracle.vigil.label-set: service failed",
			slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "set vigil label failed")}
	}
}

// SetDecreeLabelTyped — domain function for PUT /v1/decrees/{id}/label
// (WRITE+AUDIT decree.label_changed). 404 if absent. Same contract as
// [OracleHandler.SetVigilLabelTyped]; the reactor's cooldown and circuit state
// are keyed on the name and do not move.
func (h *OracleHandler) SetDecreeLabelTyped(ctx context.Context, id string, req LabelSetInput) (LabelWriteReply[DecreeView], error) {
	var zero LabelWriteReply[DecreeView]
	if !reOracleName.MatchString(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'id' must match "+reOracleName.String())}
	}
	d, previous, err := h.svc.SetDecreeLabel(ctx, id, req.Label)
	switch {
	case err == nil:
		return LabelWriteReply[DecreeView]{Body: toDecreeView(d), ID: id, Label: d.Label, Previous: previous}, nil
	case errors.Is(err, oracle.ErrDecreeNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "decree "+id+" not found")}
	default:
		h.logger.Error("oracle.decree.label-set: service failed",
			slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "set decree label failed")}
	}
}

// vigilError maps [oracle.Service] sentinels (Vigil create) to *problemError:
//   - ErrValidation         → validation-failed (422).
//   - ErrVigilAlreadyExists  → vigil-already-exists (409).
func (h *OracleHandler) vigilError(op, id, callerAID string, err error) error {
	switch {
	case errors.Is(err, oracle.ErrValidation):
		return &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	case errors.Is(err, oracle.ErrVigilAlreadyExists):
		return &problemError{problem.New(problem.TypeVigilExists, "", "vigil "+id+" already exists")}
	default:
		h.logger.Error(op+": service failed",
			slog.String("id", id), slog.String("by_aid", callerAID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", op+" failed")}
	}
}

// --- Decree -----------------------------------------------------------

// DecreeView is the FLAT wire form of a Decree (create-201 / list-item / get-200),
// handler-native. Subject is WHO may fire the rule (the four-dimension selector,
// NIM-280); IncarnationName is the opposite end — WHAT the reaction acts on. Where/
// CreatedByAID — *string nullable (nil → key omitted). action_input — byte-passthrough
// JSONB ([json.RawMessage], ADR-051 category D): raw bytes are returned as-is.
// created_at/updated_at — UTC + Truncate(Second).
type DecreeView struct {
	ID string
	// Label — display caption (ADR-0085); nil when the column is NULL, and the
	// consumer then shows Name.
	Label           *string
	OnBeacon        string
	Where           *string
	Subject         subject.Selector
	IncarnationName string
	ActionScenario  string
	ActionInput     json.RawMessage
	Cooldown        string
	Enabled         bool
	CreatedByAID    *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func toDecreeView(d *oracle.Decree) DecreeView {
	input := d.ActionInput
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	return DecreeView{
		ID:              d.ID,
		Label:           d.Label,
		OnBeacon:        d.OnBeacon,
		Where:           d.WhereCEL,
		Subject:         d.Subject(),
		IncarnationName: d.IncarnationName,
		ActionScenario:  d.ActionScenario,
		ActionInput:     input,
		Cooldown:        d.Cooldown,
		Enabled:         d.Enabled,
		CreatedByAID:    d.CreatedByAID,
		CreatedAt:       d.CreatedAt.UTC().Truncate(time.Second),
		UpdatedAt:       d.UpdatedAt.UTC().Truncate(time.Second),
	}
}

// DecreeCreateInput is the NATIVE request form of POST /v1/decrees (handler-native).
// Replaces DecreeCreateRequest: Subject — exactly one of the four dimensions, WHO may
// fire the rule ([subject.Selector]); IncarnationName — the TARGET the reaction acts on;
// action_input — `json.RawMessage` (byte-passthrough JSONB, ADR-051 category D);
// cooldown/enabled — pointer-optional (enabled omitted → true). The subject / where-CEL /
// cooldown are validated by the service.
type DecreeCreateInput struct {
	ID string
	// Label — optional display caption (ADR-0085): free text, changed afterwards
	// by PUT /v1/decrees/{id}/label.
	Label           *string
	OnBeacon        string
	Subject         subject.Selector
	IncarnationName string
	ActionScenario  string
	ActionInput     *json.RawMessage
	Where           *string
	Cooldown        *string
	Enabled         *bool
}

// DecreeCreateReply is the extracted result of [OracleHandler.CreateDecreeTyped]
// (handler-native). Carries the flat 201 view (View) + subject and caller AID (for the
// audit payload; where-CEL and action_input are NOT put in audit — action_input may carry
// a vault-ref in transit).
type DecreeCreateReply struct {
	View      DecreeView
	Subject   string
	CallerAID string
}

// AuditPayload builds the audit payload for the decree.create route (legacy parity:
// name/on_beacon/incarnation/action_scenario/subject/created_by_aid; where-CEL and
// action_input are NOT included).
func (r DecreeCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"id":              r.View.ID,
		"label":           r.View.Label,
		"on_beacon":       r.View.OnBeacon,
		"incarnation":     r.View.IncarnationName,
		"action_scenario": r.View.ActionScenario,
		"subject":         r.Subject,
		"created_by_aid":  r.CallerAID,
	}
}

// CreateDecreeTyped is the domain function for POST /v1/decrees (handler-native):
// svc.CreateDecree + sentinel→problem. action_input — byte-passthrough JSONB (ADR-051
// category D), passed to the service directly. Errors are *problemError; success is
// [DecreeCreateReply] (the flat 201 view + audit fields).
func (h *OracleHandler) CreateDecreeTyped(ctx context.Context, claims *keeperjwt.Claims, req DecreeCreateInput) (DecreeCreateReply, error) {
	var zero DecreeCreateReply
	callerAID := claims.Subject
	d, err := h.svc.CreateDecree(ctx, oracle.CreateDecreeInput{
		ID:              req.ID,
		Label:           req.Label,
		OnBeacon:        req.OnBeacon,
		WhereCEL:        req.Where,
		Subject:         req.Subject,
		IncarnationName: req.IncarnationName,
		ActionScenario:  req.ActionScenario,
		ActionInput:     derefRawMessage(req.ActionInput),
		Cooldown:        derefString(req.Cooldown),
		Enabled:         enabledOrDefault(req.Enabled),
		CallerAID:       &callerAID,
	})
	if err != nil {
		return zero, h.decreeError("oracle.decree.create", req.ID, callerAID, err)
	}
	return DecreeCreateReply{View: toDecreeView(d), Subject: d.Subject().String(), CallerAID: callerAID}, nil
}

// DecreeListPage is the domain paged result of GET /v1/decrees (handler-native). Package
// api projects it into the native envelope DecreeListReply.
type DecreeListPage struct {
	Items  []DecreeView
	Offset int
	Limit  int
	Total  int
}

// ListDecreesTyped is the domain function for GET /v1/decrees (handler-native, read with
// typed query, no audit). offset/limit arrive already validated (huma-bind int32); the
// range is enforced by CheckPageBounds → 400. A read error → *problemError (500).
func (h *OracleHandler) ListDecreesTyped(ctx context.Context, offset, limit int) (DecreeListPage, error) {
	var zero DecreeListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	decrees, total, err := h.svc.ListDecrees(ctx, offset, limit)
	if err != nil {
		h.logger.Error("oracle.decree.list: service failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list decrees failed")}
	}

	items := make([]DecreeView, 0, len(decrees))
	for _, d := range decrees {
		items = append(items, toDecreeView(d))
	}
	return DecreeListPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// GetDecreeTyped is the domain function for GET /v1/decrees/{id} (handler-native, read
// with path, no audit): path-name validation + svc.GetDecree + sentinel→problem
// (404/422/500). Errors are *problemError; success is [DecreeView].
func (h *OracleHandler) GetDecreeTyped(ctx context.Context, id string) (DecreeView, error) {
	var zero DecreeView
	if !reOracleName.MatchString(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'id' must match "+reOracleName.String())}
	}
	d, err := h.svc.GetDecree(ctx, id)
	switch {
	case err == nil:
		return toDecreeView(d), nil
	case errors.Is(err, oracle.ErrDecreeNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "decree "+id+" not found")}
	default:
		h.logger.Error("oracle.decree.get: service failed",
			slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get decree failed")}
	}
}

// DecreeDeleteReply is the extracted result of [OracleHandler.DeleteDecreeTyped]
// (handler-native). Carries the audit fields (the HTTP response is an empty 204 body).
type DecreeDeleteReply struct {
	ID string
}

// AuditPayload builds the audit payload for the decree.delete route (legacy parity: name).
func (r DecreeDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"id": r.ID}
}

// DeleteDecreeTyped is the domain function for DELETE /v1/decrees/{id} (handler-native):
// path-name validation + svc.DeleteDecree + sentinel→problem (the cascade clears
// cooldown-state). Errors are *problemError; success is [DecreeDeleteReply].
func (h *OracleHandler) DeleteDecreeTyped(ctx context.Context, id string) (DecreeDeleteReply, error) {
	var zero DecreeDeleteReply
	if !reOracleName.MatchString(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'id' must match "+reOracleName.String())}
	}
	err := h.svc.DeleteDecree(ctx, id)
	switch {
	case err == nil:
		return DecreeDeleteReply{ID: id}, nil
	case errors.Is(err, oracle.ErrDecreeNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "decree "+id+" not found")}
	default:
		h.logger.Error("oracle.decree.delete: service failed",
			slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete decree failed")}
	}
}

// decreeError maps [oracle.Service] sentinels (Decree create) to *problemError:
//   - ErrValidation          → validation-failed (422).
//   - ErrDecreeAlreadyExists   → decree-already-exists (409).
func (h *OracleHandler) decreeError(op, id, callerAID string, err error) error {
	switch {
	case errors.Is(err, oracle.ErrValidation):
		return &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	case errors.Is(err, oracle.ErrDecreeAlreadyExists):
		return &problemError{problem.New(problem.TypeDecreeExists, "", "decree "+id+" already exists")}
	default:
		h.logger.Error(op+": service failed",
			slog.String("id", id), slog.String("by_aid", callerAID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", op+" failed")}
	}
}

// enabledOrDefault: an omitted `enabled` → true (an active check/rule, symmetric to
// DEFAULT true in migration 041).
func enabledOrDefault(p *bool) bool {
	if p == nil {
		return true
	}
	return *p
}

// derefRawMessage dereferences an optional JSONB field from the request body (huma yields
// `*json.RawMessage` for omitempty params/action_input); nil → nil ([json.RawMessage]).
// Raw bytes are NOT copied or reordered (ADR-051 category D, byte-passthrough); an empty
// JSONB is normalized to `{}` at the reply boundary (toVigilView/toDecreeView).
func derefRawMessage(p *json.RawMessage) json.RawMessage {
	if p == nil {
		return nil
	}
	return *p
}
