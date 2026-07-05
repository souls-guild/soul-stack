package handlers

// Operator API handler-ы реестров Herald (каналы) и Tiding (правила подписки)
// уведомлений о событиях прогонов (ADR-052, S4). ОДИН [HeraldHandler] обслуживает
// ОБА ресурса. Тот же [herald.Service] вызывает MCP-tool-handler — один источник
// правды для десяти herald.*/tiding.*-эндпоинтов.
//
// T5d-2c (handler-native): домен herald+tiding отвязан от legacy-генерата. *Typed-функции
// принимают NATIVE request-типы (handlers.HeraldCreateInput / TidingCreateInput /
// HeraldUpdateInput / TidingUpdateInput; huma-input в пакете api биндит и валидирует
// тело по этим полям) и возвращают доменные result-ы с ПЛОСКИМИ wire-полями
// (handlers.HeraldView / TidingView) — НЕ legacy-генерата-Body. Native wire-DTO (схему
// OpenAPI) строит пакет api из этих полей (register-func huma_herald.go), oapi-
// генерёные типы в herald-домене не участвуют. (w,r)-оболочки сняты: HTTP
// обслуживает huma full-typed, MCP зовёт herald.Service напрямую.
//
// RBAC-проверка — в middleware (api/router.go).

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

// HeraldHandler — endpoints CRUD реестров Herald (каналы) и Tiding (правила
// подписки). Тонкая обёртка над [herald.Service]: тот же service вызывается
// MCP-tool-handler-ом — один источник правды.
type HeraldHandler struct {
	svc    *herald.Service
	logger *slog.Logger
}

// NewHeraldHandler создаёт handler. svc обязателен (panic при nil —
// единственная точка misconfiguration, caller обязан передать non-nil).
func NewHeraldHandler(svc *herald.Service, logger *slog.Logger) *HeraldHandler {
	if svc == nil {
		panic("handlers.NewHeraldHandler: herald.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &HeraldHandler{svc: svc, logger: logger}
}

// HeraldSpecStub — непустой *HeraldHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaHeraldSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки на nil. svc nil — handler
// никогда не исполняется в spec-режиме (parity [AugurSpecStub]).
func HeraldSpecStub() *HeraldHandler {
	return &HeraldHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// --- Herald -----------------------------------------------------------

// HeraldView — ПЛОСКАЯ wire-форма Herald-канала (create-201 / get-200 / update-200),
// handler-native. config — map БЕЗ omitempty; secret_ref/created_by_aid — *string С
// omitempty (nil → ключ опущен); type — плоская строка (пакет api проецирует в
// native enum HeraldType). created_at/updated_at — UTC (наносекундный wire,
// БЕЗ Truncate — паритет легаси `.UTC()`).
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

// HeraldCreateInput — NATIVE request-форма POST /v1/heralds (handler-native).
// Заменяет HeraldCreateRequest: имя канала + type (enum webhook в MVP) +
// config (per-type) + опц. secret_ref (vault-ref) + опц. enabled. Формат полей
// валидирует service.
type HeraldCreateInput struct {
	Name      string
	Type      string
	Config    map[string]any
	SecretRef *string
	// Secret — опц. plaintext webhook signing-secret (dual-mode, ADR-064); XOR с
	// SecretRef. Service материализует его в Vault, plaintext не персистится.
	Secret  *string
	Enabled *bool
}

// HeraldUpdateInput — NATIVE request-форма PUT /v1/heralds/{name} (handler-native,
// replace-семантика). name из path. Заменяет HeraldUpdateRequest.
type HeraldUpdateInput struct {
	Type      string
	Config    map[string]any
	SecretRef *string
	// Secret — опц. plaintext webhook signing-secret (dual-mode, ADR-064); XOR с
	// SecretRef. Перезаписывается в Vault по тому же пути (idempotent-write).
	Secret  *string
	Enabled *bool
}

// HeraldWriteReply — извлечённый результат write-роутов Herald-а (CreateHeraldTyped/
// UpdateHeraldTyped). Несёт плоский 201/200-вид (View) + сам *herald.Herald для
// audit-payload (heraldAuditPayload).
type HeraldWriteReply struct {
	View   HeraldView
	herald *herald.Herald
}

// AuditPayload собирает audit-payload Herald write-роута (ADR-052(f), parity легаси
// heraldAuditPayload).
func (r HeraldWriteReply) AuditPayload() middleware.AuditPayload {
	return heraldAuditPayload(r.herald)
}

// CreateHeraldTyped — доменная функция POST /v1/heralds (handler-native): конверт
// native req → доменная модель + svc.CreateHerald + sentinel→problem. Ошибки —
// *problemError; успех — [HeraldWriteReply] (201-вид + audit-поля). callerAID —
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

// UpdateHeraldTyped — доменная функция PUT /v1/heralds/{name} (handler-native,
// replace-семантика). name из path; конверт native req → доменная модель +
// svc.UpdateHerald + sentinel→problem. Ошибки — *problemError; успех —
// [HeraldWriteReply] (200-вид + audit-поля).
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

// HeraldDeleteReply — извлечённый результат [HeraldHandler.DeleteHeraldTyped]
// (handler-native). Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type HeraldDeleteReply struct {
	Name string
}

// AuditPayload собирает audit-payload herald.delete-роута (parity легаси: name).
func (r HeraldDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// DeleteHeraldTyped — доменная функция DELETE /v1/heralds/{name} (handler-native):
// валидация path-name + svc.DeleteHerald + sentinel→problem (каскад сносит
// Tiding-ы). Ошибки — *problemError; успех — [HeraldDeleteReply].
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

// GetHeraldTyped — доменная функция GET /v1/heralds/{name} (handler-native, read-
// with-path, БЕЗ audit): валидация path-name + svc.GetHerald + sentinel→problem
// (404/422/500). Ошибки — *problemError; успех — [HeraldView].
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

// HeraldListPage — доменный paged-результат GET /v1/heralds (handler-native). Пакет
// api проецирует в native envelope HeraldListReply.
type HeraldListPage struct {
	Items  []HeraldView
	Offset int
	Limit  int
	Total  int
}

// ListHeraldsTyped — доменная функция GET /v1/heralds (handler-native, read-with-
// typed-query, БЕЗ audit). offset/limit приходят провалидированными (huma-bind
// int32); диапазон enforce-ит CheckPageBounds → 400 (parity ParsePage). Ошибка
// чтения → *problemError (500).
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

// heraldError маппит sentinel-ошибки herald-слоя в *problemError. exists→409,
// not-found→404, валидация (битый config/secret_ref/type)→422, прочее→500 (raw err
// не пробрасывается клиенту, только в лог).
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

// heraldAuditPayload — audit-поля Herald-CRUD (ADR-052(f), naming-rules):
// name/type/url (для webhook — не секрет)/secret_ref (vault-ref, не секрет)/
// created_by_aid. Значение url достаём из config (для webhook config.url).
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
	// plaintext_ingested — маркер записи секрета keeper-ом (ADR-064 audit-event):
	// оператор передал секрет значением, keeper записал его в Vault. БЕЗ plaintext.
	// Ключ без sensitive-фрагмента → не маскируется (в отличие от secret_ref).
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

// TidingView — ПЛОСКАЯ wire-форма Tiding-правила (create-201 / get-200 / update-200),
// handler-native. event_types — []string БЕЗ omitempty; annotations — *map С
// omitempty; cadence/created_by_aid/ephemeral/incarnation/projection/task/voyage_id —
// опц. указатели С omitempty. created_at/updated_at — UTC (наносекундный wire).
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

// TidingCreateInput — NATIVE request-форма POST /v1/tidings (handler-native).
// Заменяет TidingCreateRequest: имя правила + herald (FK) + event_types
// (run-scope) + опц. фильтры/селекторы + annotations/projection. ephemeral/voyage_id
// отсутствуют — серверные (ADR-052(g)). Формат полей валидирует service.
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

// TidingUpdateInput — NATIVE request-форма PUT /v1/tidings/{name} (handler-native,
// replace-семантика: omit==clear для опц. полей — урок N4). name из path. Заменяет
// TidingUpdateRequest.
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

// TidingWriteReply — извлечённый результат write-роутов Tiding-а (CreateTidingTyped/
// UpdateTidingTyped). Несёт плоский 201/200-вид (View) + сам *herald.Tiding для
// audit-payload (tidingAuditPayload).
type TidingWriteReply struct {
	View   TidingView
	tiding *herald.Tiding
}

// AuditPayload собирает audit-payload Tiding write-роута (ADR-052(f), parity легаси
// tidingAuditPayload).
func (r TidingWriteReply) AuditPayload() middleware.AuditPayload {
	return tidingAuditPayload(r.tiding)
}

// CreateTidingTyped — доменная функция POST /v1/tidings (handler-native). Конверт
// native req → доменная модель + svc.CreateTiding + sentinel→problem. Ошибки —
// *problemError; успех — [TidingWriteReply] (201-вид + audit-поля).
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

// UpdateTidingTyped — доменная функция PUT /v1/tidings/{name} (handler-native,
// replace-семантика). name из path. PUT-replace: omit==clear (урок N4) — req.Task=
// nil/incarnation/cadence/annotations/projection очищаются, FE шлёт правило целиком.
// svc.UpdateTiding + sentinel→problem. Ошибки — *problemError; успех —
// [TidingWriteReply] (200-вид + audit-поля).
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

// TidingDeleteReply — извлечённый результат [HeraldHandler.DeleteTidingTyped]
// (handler-native). Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type TidingDeleteReply struct {
	Name string
}

// AuditPayload собирает audit-payload tiding.delete-роута (parity легаси: name).
func (r TidingDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// DeleteTidingTyped — доменная функция DELETE /v1/tidings/{name} (handler-native):
// валидация path-name + svc.DeleteTiding + sentinel→problem. Ошибки — *problemError;
// успех — [TidingDeleteReply].
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

// GetTidingTyped — доменная функция GET /v1/tidings/{name} (handler-native, read-
// with-path, БЕЗ audit): валидация path-name + svc.GetTiding + sentinel→problem
// (404/422/500). Ошибки — *problemError; успех — [TidingView].
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

// TidingListPage — доменный paged-результат GET /v1/tidings (handler-native). Пакет
// api проецирует в native envelope TidingListReply.
type TidingListPage struct {
	Items  []TidingView
	Offset int
	Limit  int
	Total  int
}

// ListTidingsTyped — доменная функция GET /v1/tidings (handler-native, read-with-
// typed-query, БЕЗ audit). includeEphemeral — typed bool (huma-bind, bad bool → 400
// на bind-фазе). offset/limit провалидированы huma-bind; диапазон enforce-ит
// CheckPageBounds → 400 (parity ParsePage). Ошибка чтения → *problemError (500).
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

// tidingError маппит sentinel-ошибки в *problemError. tiding-exists→409,
// tiding-not-found→404, herald-not-found (FK)→404, валидация→422, прочее→500.
// ErrHeraldNotFound проверяется ДО ErrTidingNotFound: FK-violation на отсутствующий
// herald — отдельный смысл (создаётся правило к несуществующему каналу).
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

// tidingAuditPayload — audit-поля Tiding-CRUD (ADR-052(f), naming-rules): все
// значения публичны (area-glob-списки / имена, не секреты). omitempty-селекторы
// пишутся только если заданы.
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

// derefAnnotations разыменовывает request-форму annotations (*map, top-level
// объектность гарантирована типом) в domain-map. nil/пустой указатель → nil
// (= нет статических полей; PUT-replace трактует это как очистку, domain
// marshalAnnotations → `{}`).
func derefAnnotations(a *map[string]any) map[string]any {
	if a == nil {
		return nil
	}
	return *a
}

// derefProjection разыменовывает request-форму projection (*[]string) в
// domain-slice. nil-указатель → nil (PUT-replace = очистка, domain projectionArg →
// пустой TEXT[]). Сам синтаксис путей валидирует domain (ValidateProjection).
func derefProjection(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// annotationsPtr — обратное преобразование для reply. nil → nil (omitempty: поле не
// появится в JSON, симметрично «нет статических полей»).
func annotationsPtr(a map[string]any) *map[string]any {
	if a == nil {
		return nil
	}
	return &a
}

// projectionPtr — обратное преобразование для reply. nil/пустой → nil (omitempty:
// полная форма payload, поле не появится в JSON).
func projectionPtr(p []string) *[]string {
	if len(p) == 0 {
		return nil
	}
	return &p
}
