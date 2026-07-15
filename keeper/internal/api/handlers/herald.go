package handlers

// Operator API handlers for the Herald (channels) and Tiding (subscription rules)
// registries for run-event notifications (ADR-052, S4). ONE [HeraldHandler] serves
// BOTH resources. The same [herald.Service] is called by the MCP tool handler — one
// source of truth for the ten herald.*/tiding.* endpoints.
//
// T5d-2c (handler-native): the herald+tiding domain is decoupled from the legacy
// generator. *Typed functions take NATIVE request types (handlers.HeraldCreateInput /
// TidingCreateInput / HeraldUpdateInput / TidingUpdateInput; the huma input in package
// api binds and validates the body against these fields) and return domain results with
// FLAT wire fields (handlers.HeraldView / TidingView) — NOT a legacy-generator Body.
// The native wire-DTO (OpenAPI schema) is built by package api from these fields
// (register func huma_herald.go); oapi-generated types don't participate in the herald
// domain. The (w,r) wrappers are removed: HTTP is served by huma full-typed, MCP calls
// herald.Service directly.
//
// RBAC check — in middleware (api/router.go).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// HeraldHandler — CRUD endpoints for the Herald (channels) and Tiding (subscription
// rules) registries. A thin wrapper over [herald.Service]: the same service is called
// by the MCP tool handler — one source of truth.
type HeraldHandler struct {
	svc    *herald.Service
	logger *slog.Logger
}

// NewHeraldHandler creates the handler. svc is required (panic on nil — the only
// misconfiguration point, caller must pass non-nil).
func NewHeraldHandler(svc *herald.Service, logger *slog.Logger) *HeraldHandler {
	if svc == nil {
		panic("handlers.NewHeraldHandler: herald.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &HeraldHandler{svc: svc, logger: logger}
}

// HeraldSpecStub is a non-nil *HeraldHandler stub for generating the huma-OpenAPI
// fragment (HumaHeraldSpecYAML): on dump the domain handler is not called, but
// huma.Register requires non-nil for its no-op nil check. svc is nil — the handler
// never executes in spec mode (parity [AugurSpecStub]).
func HeraldSpecStub() *HeraldHandler {
	return &HeraldHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// --- Herald -----------------------------------------------------------

// HeraldView is the FLAT wire form of a Herald channel (create-201 / get-200 /
// update-200), handler-native. config — map without omitempty; secret_ref/
// created_by_aid — *string with omitempty (nil → key omitted); type — flat string
// (package api projects to the native enum HeraldType). created_at/updated_at — UTC
// (nanosecond wire, no Truncate — parity with the legacy `.UTC()`).
type HeraldView struct {
	Name         string
	Type         string
	Config       map[string]any
	SecretRef    *string
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CreatedByAID *string
}

func toHeraldView(h *herald.Herald) HeraldView {
	config := h.Config
	if config == nil {
		config = map[string]any{}
	}
	return HeraldView{
		Name:         h.Name,
		Type:         string(h.Type),
		Config:       config,
		SecretRef:    h.SecretRef,
		Enabled:      h.Enabled,
		CreatedAt:    h.CreatedAt.UTC(),
		UpdatedAt:    h.UpdatedAt.UTC(),
		CreatedByAID: h.CreatedByAID,
	}
}

// HeraldCreateInput is the NATIVE request form of POST /v1/heralds (handler-native).
// Replaces HeraldCreateRequest: channel name + type (enum webhook in MVP) + config
// (per-type) + optional secret_ref (vault-ref) + optional enabled. The service
// validates field formats.
type HeraldCreateInput struct {
	Name      string
	Type      string
	Config    map[string]any
	SecretRef *string
	// Secret — optional plaintext webhook signing secret (dual-mode, ADR-064); XOR with
	// SecretRef. The service materializes it into Vault; plaintext is not persisted.
	Secret  *string
	Enabled *bool
}

// HeraldUpdateInput is the NATIVE request form of PUT /v1/heralds/{name}
// (handler-native, replace semantics). name from path. Replaces HeraldUpdateRequest.
type HeraldUpdateInput struct {
	Type      string
	Config    map[string]any
	SecretRef *string
	// Secret — optional plaintext webhook signing secret (dual-mode, ADR-064); XOR with
	// SecretRef. Overwritten in Vault at the same path (idempotent write).
	Secret  *string
	Enabled *bool
}

// HeraldWriteReply is the extracted result of Herald write routes (CreateHeraldTyped/
// UpdateHeraldTyped). Carries the flat 201/200 view (View) + the *herald.Herald itself
// for the audit payload (heraldAuditPayload).
type HeraldWriteReply struct {
	View   HeraldView
	herald *herald.Herald
}

// AuditPayload assembles the audit payload of a Herald write route (ADR-052(f), parity
// with the legacy heraldAuditPayload).
func (r HeraldWriteReply) AuditPayload() middleware.AuditPayload {
	return heraldAuditPayload(r.herald)
}

// CreateHeraldTyped is the domain function for POST /v1/heralds (handler-native):
// convert native req → domain model + svc.CreateHerald + sentinel→problem. Errors —
// *problemError; success — [HeraldWriteReply] (201 view + audit fields). callerAID —
// claims.Subject.
func (h *HeraldHandler) CreateHeraldTyped(ctx context.Context, claims *keeperjwt.Claims, req HeraldCreateInput) (HeraldWriteReply, error) {
	var zero HeraldWriteReply
	hr := &herald.Herald{
		Name:         req.Name,
		Type:         herald.HeraldType(req.Type),
		Config:       req.Config,
		SecretRef:    req.SecretRef,
		Secret:       req.Secret,
		Enabled:      boolOr(req.Enabled, true),
		CreatedByAID: aidPtr(claims.Subject),
	}
	created, err := h.svc.CreateHerald(ctx, hr)
	if err != nil {
		return zero, h.heraldError(err, req.Name, "create")
	}
	return HeraldWriteReply{View: toHeraldView(created), herald: created}, nil
}

// UpdateHeraldTyped is the domain function for PUT /v1/heralds/{name} (handler-native,
// replace semantics). name from path; convert native req → domain model +
// svc.UpdateHerald + sentinel→problem. Errors — *problemError; success —
// [HeraldWriteReply] (200 view + audit fields).
func (h *HeraldHandler) UpdateHeraldTyped(ctx context.Context, name string, req HeraldUpdateInput) (HeraldWriteReply, error) {
	var zero HeraldWriteReply
	hr := &herald.Herald{
		Name:      name,
		Type:      herald.HeraldType(req.Type),
		Config:    req.Config,
		SecretRef: req.SecretRef,
		Secret:    req.Secret,
		Enabled:   boolOr(req.Enabled, true),
	}
	updated, err := h.svc.UpdateHerald(ctx, hr)
	if err != nil {
		return zero, h.heraldError(err, name, "update")
	}
	return HeraldWriteReply{View: toHeraldView(updated), herald: updated}, nil
}

// HeraldDeleteReply is the extracted result of [HeraldHandler.DeleteHeraldTyped]
// (handler-native). Carries audit fields (HTTP response — empty 204 body).
type HeraldDeleteReply struct {
	Name string
}

// AuditPayload assembles the audit payload of the herald.delete route (parity with legacy: name).
func (r HeraldDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// DeleteHeraldTyped is the domain function for DELETE /v1/heralds/{name}
// (handler-native): validate path name + svc.DeleteHerald + sentinel→problem (cascade
// removes Tidings). Errors — *problemError; success — [HeraldDeleteReply].
func (h *HeraldHandler) DeleteHeraldTyped(ctx context.Context, name string) (HeraldDeleteReply, error) {
	var zero HeraldDeleteReply
	if !herald.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+herald.NamePattern)}
	}
	if err := h.svc.DeleteHerald(ctx, name); err != nil {
		return zero, h.heraldError(err, name, "delete")
	}
	return HeraldDeleteReply{Name: name}, nil
}

// GetHeraldTyped is the domain function for GET /v1/heralds/{name} (handler-native,
// read-with-path, no audit): validate path name + svc.GetHerald + sentinel→problem
// (404/422/500). Errors — *problemError; success — [HeraldView].
func (h *HeraldHandler) GetHeraldTyped(ctx context.Context, name string) (HeraldView, error) {
	var zero HeraldView
	if !herald.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+herald.NamePattern)}
	}
	hr, err := h.svc.GetHerald(ctx, name)
	if err != nil {
		return zero, h.heraldError(err, name, "get")
	}
	return toHeraldView(hr), nil
}

// HeraldListPage is the domain paged result of GET /v1/heralds (handler-native).
// Package api projects it into the native envelope HeraldListReply.
type HeraldListPage struct {
	Items  []HeraldView
	Offset int
	Limit  int
	Total  int
}

// ListHeraldsTyped is the domain function for GET /v1/heralds (handler-native,
// read-with-typed-query, no audit). offset/limit arrive pre-validated (huma-bind
// int32); CheckPageBounds enforces the range → 400 (parity ParsePage). Read error →
// *problemError (500).
func (h *HeraldHandler) ListHeraldsTyped(ctx context.Context, offset, limit int) (HeraldListPage, error) {
	var zero HeraldListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	items, total, err := h.svc.ListHeralds(ctx, offset, limit)
	if err != nil {
		h.logger.Error("herald.list: service failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list heralds failed")}
	}
	out := make([]HeraldView, 0, len(items))
	for _, hr := range items {
		out = append(out, toHeraldView(hr))
	}
	return HeraldListPage{Items: out, Offset: offset, Limit: limit, Total: total}, nil
}

// heraldError maps sentinel errors of the herald layer to *problemError. exists→409,
// not-found→404, validation (bad config/secret_ref/type)→422, other→500 (raw err is
// not propagated to the client, only logged).
func (h *HeraldHandler) heraldError(err error, name, op string) error {
	switch {
	case errors.Is(err, herald.ErrHeraldExists):
		return &problemError{problem.New(problem.TypeHeraldExists, "", "herald "+name+" already exists")}
	case errors.Is(err, herald.ErrHeraldNotFound):
		return &problemError{problem.New(problem.TypeNotFound, "", "herald "+name+" not found")}
	case herald.IsValidationError(err):
		return &problemError{problem.New(problem.TypeValidationFailed, "", herald.PublicMessage(err))}
	default:
		h.logger.Error("herald."+op+": service failed",
			slog.String("name", name), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", op+" herald failed")}
	}
}

// heraldAuditPayload — audit fields for Herald CRUD (ADR-052(f), naming-rules):
// name/type/url (for webhook — not a secret)/secret_ref (vault-ref, not a secret)/
// created_by_aid. The url value is taken from config (for webhook, config.url).
func heraldAuditPayload(h *herald.Herald) middleware.AuditPayload {
	p := middleware.AuditPayload{
		"name":    h.Name,
		"type":    string(h.Type),
		"enabled": h.Enabled,
	}
	if url, ok := h.Config["url"].(string); ok {
		p["url"] = url
	}
	if h.SecretRef != nil {
		p["secret_ref"] = *h.SecretRef
	}
	// plaintext_ingested — marker that keeper wrote the secret (ADR-064 audit event):
	// the operator passed the secret by value, keeper wrote it into Vault. No plaintext.
	// A key without a sensitive fragment → not masked (unlike secret_ref).
	if h.SecretWritten {
		p["plaintext_ingested"] = true
	}
	if h.CreatedByAID != nil {
		p["created_by_aid"] = *h.CreatedByAID
	}
	return p
}

func aidPtr(aid string) *string {
	if aid == "" {
		return nil
	}
	return &aid
}

// --- Tiding -----------------------------------------------------------

// TidingView is the FLAT wire form of a Tiding rule (create-201 / get-200 /
// update-200), handler-native. event_types — []string without omitempty; annotations —
// *map with omitempty; cadence/created_by_aid/ephemeral/incarnation/projection/task/
// voyage_id — optional pointers with omitempty. created_at/updated_at — UTC (nanosecond wire).
type TidingView struct {
	Name         string
	Herald       string
	EventTypes   []string
	OnlyFailures bool
	OnlyChanges  bool
	Incarnation  *string
	Cadence      *string
	Task         *string
	Ephemeral    *bool
	VoyageID     *string
	Annotations  *map[string]any
	Projection   *[]string
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CreatedByAID *string
}

func toTidingView(t *herald.Tiding) TidingView {
	eventTypes := t.EventTypes
	if eventTypes == nil {
		eventTypes = []string{}
	}
	ephemeral := t.Ephemeral
	return TidingView{
		Name:         t.Name,
		Herald:       t.Herald,
		EventTypes:   eventTypes,
		OnlyFailures: t.OnlyFailures,
		OnlyChanges:  t.OnlyChanges,
		Incarnation:  t.Incarnation,
		Cadence:      t.Cadence,
		Task:         t.Task,
		Ephemeral:    &ephemeral,
		VoyageID:     t.VoyageID,
		Annotations:  annotationsPtr(t.Annotations),
		Projection:   projectionPtr(t.Projection),
		Enabled:      t.Enabled,
		CreatedAt:    t.CreatedAt.UTC(),
		UpdatedAt:    t.UpdatedAt.UTC(),
		CreatedByAID: t.CreatedByAID,
	}
}

// TidingCreateInput is the NATIVE request form of POST /v1/tidings (handler-native).
// Replaces TidingCreateRequest: rule name + herald (FK) + event_types (run-scope) +
// optional filters/selectors + annotations/projection. ephemeral/voyage_id are absent —
// server-side (ADR-052(g)). The service validates field formats.
type TidingCreateInput struct {
	Name         string
	Herald       string
	EventTypes   []string
	OnlyFailures *bool
	OnlyChanges  *bool
	Incarnation  *string
	Cadence      *string
	Task         *string
	Annotations  *map[string]any
	Projection   *[]string
	Enabled      *bool
}

// TidingUpdateInput is the NATIVE request form of PUT /v1/tidings/{name}
// (handler-native, replace semantics: omit==clear for optional fields — lesson N4).
// name from path. Replaces TidingUpdateRequest.
type TidingUpdateInput struct {
	Herald       string
	EventTypes   []string
	OnlyFailures *bool
	OnlyChanges  *bool
	Incarnation  *string
	Cadence      *string
	Task         *string
	Annotations  *map[string]any
	Projection   *[]string
	Enabled      *bool
}

// TidingWriteReply is the extracted result of Tiding write routes (CreateTidingTyped/
// UpdateTidingTyped). Carries the flat 201/200 view (View) + the *herald.Tiding itself
// for the audit payload (tidingAuditPayload).
type TidingWriteReply struct {
	View   TidingView
	tiding *herald.Tiding
}

// AuditPayload assembles the audit payload of a Tiding write route (ADR-052(f), parity
// with the legacy tidingAuditPayload).
func (r TidingWriteReply) AuditPayload() middleware.AuditPayload {
	return tidingAuditPayload(r.tiding)
}

// CreateTidingTyped is the domain function for POST /v1/tidings (handler-native).
// Convert native req → domain model + svc.CreateTiding + sentinel→problem. Errors —
// *problemError; success — [TidingWriteReply] (201 view + audit fields).
func (h *HeraldHandler) CreateTidingTyped(ctx context.Context, claims *keeperjwt.Claims, req TidingCreateInput) (TidingWriteReply, error) {
	var zero TidingWriteReply
	tg := &herald.Tiding{
		Name:         req.Name,
		Herald:       req.Herald,
		EventTypes:   req.EventTypes,
		OnlyFailures: boolOr(req.OnlyFailures, false),
		OnlyChanges:  boolOr(req.OnlyChanges, false),
		Incarnation:  req.Incarnation,
		Cadence:      req.Cadence,
		Task:         req.Task,
		Annotations:  derefAnnotations(req.Annotations),
		Projection:   derefProjection(req.Projection),
		Enabled:      boolOr(req.Enabled, true),
		CreatedByAID: aidPtr(claims.Subject),
	}
	created, err := h.svc.CreateTiding(ctx, tg)
	if err != nil {
		return zero, h.tidingError(err, req.Name, "create")
	}
	return TidingWriteReply{View: toTidingView(created), tiding: created}, nil
}

// UpdateTidingTyped is the domain function for PUT /v1/tidings/{name} (handler-native,
// replace semantics). name from path. PUT-replace: omit==clear (lesson N4) — req.Task=
// nil/incarnation/cadence/annotations/projection are cleared, the FE sends the whole
// rule. svc.UpdateTiding + sentinel→problem. Errors — *problemError; success —
// [TidingWriteReply] (200 view + audit fields).
func (h *HeraldHandler) UpdateTidingTyped(ctx context.Context, name string, req TidingUpdateInput) (TidingWriteReply, error) {
	var zero TidingWriteReply
	tg := &herald.Tiding{
		Name:         name,
		Herald:       req.Herald,
		EventTypes:   req.EventTypes,
		OnlyFailures: boolOr(req.OnlyFailures, false),
		OnlyChanges:  boolOr(req.OnlyChanges, false),
		Incarnation:  req.Incarnation,
		Cadence:      req.Cadence,
		Task:         req.Task,
		Annotations:  derefAnnotations(req.Annotations),
		Projection:   derefProjection(req.Projection),
		Enabled:      boolOr(req.Enabled, true),
	}
	updated, err := h.svc.UpdateTiding(ctx, tg)
	if err != nil {
		return zero, h.tidingError(err, name, "update")
	}
	return TidingWriteReply{View: toTidingView(updated), tiding: updated}, nil
}

// TidingDeleteReply is the extracted result of [HeraldHandler.DeleteTidingTyped]
// (handler-native). Carries audit fields (HTTP response — empty 204 body).
type TidingDeleteReply struct {
	Name string
}

// AuditPayload assembles the audit payload of the tiding.delete route (parity with legacy: name).
func (r TidingDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// DeleteTidingTyped is the domain function for DELETE /v1/tidings/{name}
// (handler-native): validate path name + svc.DeleteTiding + sentinel→problem. Errors —
// *problemError; success — [TidingDeleteReply].
func (h *HeraldHandler) DeleteTidingTyped(ctx context.Context, name string) (TidingDeleteReply, error) {
	var zero TidingDeleteReply
	if !herald.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+herald.NamePattern)}
	}
	if err := h.svc.DeleteTiding(ctx, name); err != nil {
		return zero, h.tidingError(err, name, "delete")
	}
	return TidingDeleteReply{Name: name}, nil
}

// GetTidingTyped is the domain function for GET /v1/tidings/{name} (handler-native,
// read-with-path, no audit): validate path name + svc.GetTiding + sentinel→problem
// (404/422/500). Errors — *problemError; success — [TidingView].
func (h *HeraldHandler) GetTidingTyped(ctx context.Context, name string) (TidingView, error) {
	var zero TidingView
	if !herald.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+herald.NamePattern)}
	}
	tg, err := h.svc.GetTiding(ctx, name)
	if err != nil {
		return zero, h.tidingError(err, name, "get")
	}
	return toTidingView(tg), nil
}

// TidingListPage is the domain paged result of GET /v1/tidings (handler-native).
// Package api projects it into the native envelope TidingListReply.
type TidingListPage struct {
	Items  []TidingView
	Offset int
	Limit  int
	Total  int
}

// ListTidingsTyped is the domain function for GET /v1/tidings (handler-native,
// read-with-typed-query, no audit). includeEphemeral — typed bool (huma-bind, bad bool
// → 400 at the bind phase). offset/limit pre-validated by huma-bind; CheckPageBounds
// enforces the range → 400 (parity ParsePage). Read error → *problemError (500).
func (h *HeraldHandler) ListTidingsTyped(ctx context.Context, includeEphemeral bool, offset, limit int) (TidingListPage, error) {
	var zero TidingListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	items, total, err := h.svc.ListTidings(ctx, includeEphemeral, offset, limit)
	if err != nil {
		h.logger.Error("tiding.list: service failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list tidings failed")}
	}
	out := make([]TidingView, 0, len(items))
	for _, tg := range items {
		out = append(out, toTidingView(tg))
	}
	return TidingListPage{Items: out, Offset: offset, Limit: limit, Total: total}, nil
}

// tidingError maps sentinel errors to *problemError. tiding-exists→409,
// tiding-not-found→404, herald-not-found (FK)→404, validation→422, other→500.
// ErrHeraldNotFound is checked BEFORE ErrTidingNotFound: an FK violation on a missing
// herald has a distinct meaning (a rule is created for a nonexistent channel).
func (h *HeraldHandler) tidingError(err error, name, op string) error {
	switch {
	case errors.Is(err, herald.ErrTidingExists):
		return &problemError{problem.New(problem.TypeTidingExists, "", "tiding "+name+" already exists")}
	case errors.Is(err, herald.ErrHeraldNotFound):
		return &problemError{problem.New(problem.TypeNotFound, "", "referenced herald not found")}
	case errors.Is(err, herald.ErrTidingNotFound):
		return &problemError{problem.New(problem.TypeNotFound, "", "tiding "+name+" not found")}
	case herald.IsValidationError(err):
		return &problemError{problem.New(problem.TypeValidationFailed, "", herald.PublicMessage(err))}
	default:
		h.logger.Error("tiding."+op+": service failed",
			slog.String("name", name), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", op+" tiding failed")}
	}
}

// tidingAuditPayload — audit fields for Tiding CRUD (ADR-052(f), naming-rules): all
// values are public (area-glob lists / names, not secrets). omitempty selectors are
// written only when set.
func tidingAuditPayload(t *herald.Tiding) middleware.AuditPayload {
	p := middleware.AuditPayload{
		"name":          t.Name,
		"herald":        t.Herald,
		"event_types":   t.EventTypes,
		"only_failures": t.OnlyFailures,
		"only_changes":  t.OnlyChanges,
		"enabled":       t.Enabled,
	}
	if t.Incarnation != nil {
		p["incarnation"] = *t.Incarnation
	}
	if t.Cadence != nil {
		p["cadence"] = *t.Cadence
	}
	if t.Task != nil {
		p["task"] = *t.Task
	}
	if t.CreatedByAID != nil {
		p["created_by_aid"] = *t.CreatedByAID
	}
	return p
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// derefAnnotations dereferences the request form of annotations (*map, top-level
// object-ness guaranteed by the type) into a domain map. nil/empty pointer → nil
// (= no static fields; PUT-replace treats this as a clear, domain marshalAnnotations →
// `{}`).
func derefAnnotations(a *map[string]any) map[string]any {
	if a == nil {
		return nil
	}
	return *a
}

// derefProjection dereferences the request form of projection (*[]string) into a
// domain slice. nil pointer → nil (PUT-replace = clear, domain projectionArg → empty
// TEXT[]). The path syntax itself is validated by the domain (ValidateProjection).
func derefProjection(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// annotationsPtr is the inverse conversion for the reply. nil → nil (omitempty: the
// field won't appear in JSON, symmetric with "no static fields").
func annotationsPtr(a map[string]any) *map[string]any {
	if a == nil {
		return nil
	}
	return &a
}

// projectionPtr is the inverse conversion for the reply. nil/empty → nil (omitempty:
// full payload form, the field won't appear in JSON).
func projectionPtr(p []string) *[]string {
	if len(p) == 0 {
		return nil
	}
	return &p
}
