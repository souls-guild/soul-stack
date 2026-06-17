// Operator API handler-ы реестра Augur (Omen — внешняя система, Rite — grant;
// ADR-025, augur.md §4). Тот же [augur.Service] вызывает MCP-tool-handler
// (keeper.augur.omen.* / keeper.augur.rite.*), один источник правды.
//
// T5d-2c (handler-native): домен augur отвязан от legacy-генерата. *Typed-функции
// принимают NATIVE request-типы (handlers.OmenCreateInput / RiteCreateInput;
// huma-input в пакете api биндит и валидирует тело по этим полям) и возвращают
// доменные result-ы с ПЛОСКИМИ wire-полями (handlers.OmenView / RiteView) — НЕ
// legacy-генерата-Body. Native wire-DTO (схему OpenAPI) строит пакет api из этих полей
// (register-func huma_augur.go), oapi-генерёные типы в augur-домене не участвуют.
// (w,r)-оболочки сняты: HTTP обслуживает huma full-typed, MCP зовёт
// augur.Service напрямую (мимо handler — Service-direct, не httptest).
//
// Бизнес-логика (валидация name/source_type/auth_ref, XOR-субъект, allow-shape,
// token-поля) — в [augur.Service]; handler делает path/query-валидацию и маппит
// sentinel-ы в RFC 7807. RBAC — в middleware (router.go).
//
// БЕЗОПАСНОСТЬ: master-credential внешней системы в реестре НЕ хранится (только
// auth_ref — vault-ref, augur.md §4.1); endpoint / auth_ref / allow секретов не
// несут и логируются в audit. Значения секретов через этот path не проходят.
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

// reOmenName — формат path-сегмента {name} Omen-а (kebab 1..63, augur.NamePattern).
// Path-сегмент без слешей/`..` — безопасен от traversal.
var reOmenName = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// AugurHandler — REST-эндпоинты реестра Augur (omens + rites). Делегирует
// бизнес-логику в [augur.Service]. Все зависимости immutable; safe for
// concurrent use.
type AugurHandler struct {
	svc    *augur.Service
	logger *slog.Logger
}

// NewAugurHandler создаёт handler. svc обязателен (паника при nil — единственная
// точка misconfiguration; caller обязан передать non-nil).
func NewAugurHandler(svc *augur.Service, logger *slog.Logger) *AugurHandler {
	if svc == nil {
		panic("handlers.NewAugurHandler: augur.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &AugurHandler{svc: svc, logger: logger}
}

// AugurSpecStub — непустой *AugurHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaAugurSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки на nil. svc nil — handler
// никогда не исполняется в spec-режиме (parity [OperatorSpecStub]).
func AugurSpecStub() *AugurHandler {
	return &AugurHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// --- Omen -------------------------------------------------------------

// OmenView — ПЛОСКАЯ wire-форма Omen-а (create-201 / list-item / get-200),
// handler-native. created_by_aid — nullable (NULL → ключ опущен). source_type —
// плоская строка (пакет api проецирует её в native enum OmenViewSourceType).
// created_at — UTC + Truncate(Second) (фиксируется здесь, как в эталоне operator).
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

// OmenCreateInput — NATIVE request-форма POST /v1/augur/omens (handler-native).
// Заменяет OmenCreateRequest: huma-input (пакет api) биндит и валидирует
// тело по этим полям, затем зовёт CreateOmenTyped. Закрытый набор source_type
// валидирует service (доменный ValidSourceType).
type OmenCreateInput struct {
	Name       string
	SourceType string
	Endpoint   string
	AuthRef    string
}

// OmenCreateReply — извлечённый результат [AugurHandler.CreateOmenTyped]
// (handler-native). Несёт плоский 201-вид (View) + caller AID (для audit-payload).
type OmenCreateReply struct {
	View      OmenView
	CallerAID string
}

// AuditPayload собирает audit-payload omen.create-роута (parity легаси:
// name/source_type/endpoint/auth_ref/created_by_aid; без секретов, augur.md §8).
func (r OmenCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":           r.View.Name,
		"source_type":    r.View.SourceType,
		"endpoint":       r.View.Endpoint,
		"auth_ref":       r.View.AuthRef,
		"created_by_aid": r.CallerAID,
	}
}

// CreateOmenTyped — доменная функция POST /v1/augur/omens (handler-native):
// svc.CreateOmen + sentinel→problem. Ошибки — *problemError; успех —
// [OmenCreateReply] (плоский 201-вид + audit-поля).
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

// OmenListPage — доменный paged-результат GET /v1/augur/omens (handler-native).
// Плоские offset/limit/total + срез OmenView; пакет api проецирует в native
// envelope OmenListReply.
type OmenListPage struct {
	Items  []OmenView
	Offset int
	Limit  int
	Total  int
}

// ListOmensTyped — доменная функция GET /v1/augur/omens (handler-native, read-
// with-typed-query, БЕЗ audit). offset/limit приходят уже провалидированными
// (huma-bind int32); диапазон enforce-ит CheckPageBounds → 400. Ошибка чтения →
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

// GetOmenTyped — доменная функция GET /v1/augur/omens/{name} (handler-native,
// read-with-path, БЕЗ audit): валидация path-name + svc.GetOmen + sentinel→problem
// (404/422/500). Ошибки — *problemError; успех — [OmenView].
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

// OmenDeleteReply — извлечённый результат [AugurHandler.DeleteOmenTyped]
// (handler-native). Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type OmenDeleteReply struct {
	Name string
}

// AuditPayload собирает audit-payload omen.delete-роута (parity легаси: name).
func (r OmenDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// DeleteOmenTyped — доменная функция DELETE /v1/augur/omens/{name} (handler-
// native): валидация path-name + svc.DeleteOmen + sentinel→problem. Ошибки —
// *problemError; успех — [OmenDeleteReply].
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

// omenError маппит sentinel-ы [augur.Service] (Omen create) в *problemError:
//   - ErrValidation       → validation-failed (422).
//   - ErrOmenAlreadyExists → omen-already-exists (409).
//
// Для unknown-ошибок — internal-error (500) + generic-detail (raw err.Error()
// не пробрасывается клиенту; диагностика — в логах). Доставляется huma-обёрткой
// через AsProblemDetails.
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

// RiteView — ПЛОСКАЯ wire-форма Rite-а (create-201 / list-item), handler-native.
// allow — byte-passthrough JSONB ([json.RawMessage], ADR-051 категория D): сырые
// байты домена едут as-is, БЕЗ unmarshal→map→marshal (re-marshal переупорядочил бы
// ключи — PG-JSONB-канонизация ≠ Go-`map`-marshal лексикографический порядок).
// coven/sid/token_*/created_by_aid — nullable (nil → ключ опущен). created_at —
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

// RiteCreateInput — NATIVE request-форма POST /v1/augur/rites (handler-native).
// Заменяет RiteCreateRequest: subject — XOR coven/sid; allow —
// `json.RawMessage` (byte-passthrough JSONB, ADR-051 категория D); delegate —
// pointer-optional (опущено → false). XOR-субъект / allow-shape / token-поля
// валидирует service.
type RiteCreateInput struct {
	Omen         string
	Coven        *string
	SID          *string
	Allow        json.RawMessage
	Delegate     *bool
	TokenTTL     *string
	TokenNumUses *int
}

// RiteCreateReply — извлечённый результат [AugurHandler.CreateRiteTyped]
// (handler-native). Несёт плоский 201-вид (View) + субъект и caller AID (для
// audit-payload; allow-list в audit НЕ кладётся, augur.md §8).
type RiteCreateReply struct {
	View      RiteView
	Subject   string
	CallerAID string
}

// AuditPayload собирает audit-payload rite.create-роута (parity легаси:
// id/omen/subject/delegate/created_by_aid; allow НЕ кладётся).
func (r RiteCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"id":             r.View.ID,
		"omen":           r.View.Omen,
		"subject":        r.Subject,
		"delegate":       r.View.Delegate,
		"created_by_aid": r.CallerAID,
	}
}

// CreateRiteTyped — доменная функция POST /v1/augur/rites (handler-native):
// svc.CreateRite + sentinel→problem. allow — byte-passthrough JSONB (ADR-051
// категория D), едет в service-валидатор напрямую. Ошибки — *problemError; успех —
// [RiteCreateReply] (плоский 201-вид + audit-поля).
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

// RiteListResult — доменный результат GET /v1/augur/rites?omen=<name> (handler-
// native, items-only без пагинации). Пакет api проецирует в native RiteListReply.
type RiteListResult struct {
	Items []RiteView
}

// ListRitesTyped — доменная функция GET /v1/augur/rites?omen=<name> (handler-
// native, read-with-typed-query, БЕЗ audit). Фильтр by-omen ОБЯЗАТЕЛЕН в MVP
// (augur.md §6): пустой/битый omen → 422 (доменная regex-валидация). Ошибка
// чтения → *problemError (500).
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

// RiteDeleteReply — извлечённый результат [AugurHandler.DeleteRiteTyped]
// (handler-native). Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type RiteDeleteReply struct {
	ID int64
}

// AuditPayload собирает audit-payload rite.delete-роута (parity легаси: id).
func (r RiteDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"id": r.ID}
}

// DeleteRiteTyped — доменная функция DELETE /v1/augur/rites/{id} (handler-
// native): валидация path-id (положительное число → 422) + svc.DeleteRite +
// sentinel→problem. Ошибки — *problemError; успех — [RiteDeleteReply].
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

// riteError маппит sentinel-ы [augur.Service] (Rite create) в *problemError:
//   - ErrValidation   → validation-failed (422).
//   - ErrOmenNotFound → not-found (404; Omen grant-а не существует).
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

// riteSubject — человекочитаемая форма субъекта Rite-а для audit-payload
// (`coven=<v>` / `sid=<v>`). XOR гарантирован валидацией; при пустом обоих
// (теоретически невозможно после insert) — пустая строка.
func riteSubject(r *augur.Rite) string {
	if r.Coven != nil && *r.Coven != "" {
		return "coven=" + *r.Coven
	}
	if r.SID != nil && *r.SID != "" {
		return "sid=" + *r.SID
	}
	return ""
}
