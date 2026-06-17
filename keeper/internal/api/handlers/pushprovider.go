package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// PushProviderHandler — endpoints CRUD push_providers (ADR-032 amendment
// 2026-05-26, S7-2). Тонкая обёртка над [pushprovider.Service]: тот же service
// вызывается MCP-tool-handler-ом, что гарантирует один источник правды для пяти
// push-provider.*-эндпоинтов.
//
// RBAC-проверка делается в middleware (см. api/router.go); handler
// выполняет только маппинг ошибок в RFC 7807.
//
// T5d-2c-full (handler-native): домен отвязан от legacy-генерата. *Typed-функции
// принимают/возвращают NATIVE типы с плоскими wire-полями (PushProviderCreateInput /
// PushProviderUpdateInput / PushProviderView / PushProviderListPage); native wire-DTO
// (схему OpenAPI) строит пакет api из этих полей (register-func huma_pushprovider.go).
// HTTP обслуживает huma full-typed, MCP зовёт pushprovider.Service напрямую (мимо handler).
type PushProviderHandler struct {
	svc    *pushprovider.Service
	logger *slog.Logger
}

// NewPushProviderHandler создаёт handler. svc обязателен (panic при nil —
// единственная точка misconfiguration, caller обязан передать non-nil).
func NewPushProviderHandler(svc *pushprovider.Service, logger *slog.Logger) *PushProviderHandler {
	if svc == nil {
		panic("handlers.NewPushProviderHandler: pushprovider.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &PushProviderHandler{svc: svc, logger: logger}
}

// PushProviderSpecStub — непустой *PushProviderHandler-заглушка для генерации huma-
// OpenAPI-фрагмента (HumaPushProviderSpecYAML): при dump доменный handler не
// вызывается, но huma.Register требует non-nil для no-op-проверки на nil. svc nil —
// handler никогда не исполняется в spec-режиме (parity [SigilSpecStub]).
func PushProviderSpecStub() *PushProviderHandler {
	return &PushProviderHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// PushProviderCreateInput — NATIVE request-форма POST /v1/push-providers (handler-native).
// Заменяет PushProviderCreateRequest: huma-input (пакет api) биндит/валидирует тело и
// проецирует его в эти поля. Params — опциональный указатель (*map), handler разыменовывает
// его в pushprovider.CreateInput.
type PushProviderCreateInput struct {
	Name   string
	Params *map[string]any
}

// PushProviderUpdateInput — NATIVE request-форма PUT /v1/push-providers/{name} (handler-
// native). Заменяет PushProviderUpdateRequest. Replace-семантика: params полностью
// заменяет существующий набор.
type PushProviderUpdateInput struct {
	Params map[string]any
}

// PushProviderView — ПЛОСКАЯ wire-форма Push-Provider-а (Create-201, Get-200, List-items,
// Update-200), handler-native (заменяет PushProvider). params нормализован nil→{};
// updated_by_aid — опц. указатель; created_at/updated_at — наносекундный time-wire.
// Пакет api проецирует её в native PushProvider (register-func huma_pushprovider.go),
// порядок полей wire фиксирует native-тип.
type PushProviderView struct {
	Name         string
	Params       map[string]any
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CreatedByAID string
	UpdatedByAID *string
}

// PushProviderListPage — доменный paged-результат GET /v1/push-providers (handler-native).
// Плоские offset/limit/total + срез PushProviderView; пакет api проецирует его в native-
// envelope PushProviderListReply (register-func huma_pushprovider.go).
type PushProviderListPage struct {
	Items  []PushProviderView
	Offset int
	Limit  int
	Total  int
}

// toPushProviderView проецирует [pushprovider.PushProvider] в плоский view.
// date-time: прежняя сериализация — голый time.Time-field (RFC3339Nano через MarshalJSON),
// поэтому `.UTC()` БЕЗ Truncate сохраняет байт-в-байт. nil-params нормализуются в пустую
// карту (паритет с прежним поведением).
func toPushProviderView(p *pushprovider.PushProvider) PushProviderView {
	params := p.Params
	if params == nil {
		params = map[string]any{}
	}
	return PushProviderView{
		Name:         p.Name,
		Params:       params,
		CreatedAt:    p.CreatedAt.UTC(),
		UpdatedAt:    p.UpdatedAt.UTC(),
		CreatedByAID: p.CreatedByAID,
		UpdatedByAID: p.UpdatedByAID,
	}
}

// PushProviderWriteReply — извлечённый результат write-роутов Push-Provider-а
// (CreateTyped/UpdateTyped/DeleteTyped). Несёт тело (для create/update — 201/200
// PushProviderView; для delete — пустое) + name/params_keys (для audit-payload; VALUE
// params в audit НЕ кладутся — sensitive-инвариант).
type PushProviderWriteReply struct {
	Body       PushProviderView
	Name       string
	ParamsKeys []string
}

// AuditPayload собирает audit-payload write-роутов Push-Provider-а (parity легаси:
// name + params_keys без values). Источник для huma-варианта B.
func (r PushProviderWriteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":        r.Name,
		"params_keys": r.ParamsKeys,
	}
}

// CreateTyped — доменная функция POST /v1/push-providers (handler-native): валидация name +
// svc.Create + sentinel→problem. Ошибки — *problemError; успех — [PushProviderWriteReply]
// (201-тело + audit-поля).
func (h *PushProviderHandler) CreateTyped(ctx context.Context, claims *keeperjwt.Claims, req PushProviderCreateInput) (PushProviderWriteReply, error) {
	var zero PushProviderWriteReply
	if req.Name == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'name' is required")}
	}
	if !pushprovider.ValidName(req.Name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'name' must match "+pushprovider.NamePattern)}
	}

	var params map[string]any
	if req.Params != nil {
		params = *req.Params
	}
	p, err := h.svc.Create(ctx, pushprovider.CreateInput{
		Name:      req.Name,
		Params:    params,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		return PushProviderWriteReply{Body: toPushProviderView(p), Name: p.Name, ParamsKeys: paramKeysSorted(p.Params)}, nil
	case errors.Is(err, pushprovider.ErrPushProviderAlreadyExists):
		return zero, &problemError{problem.New(problem.TypePushProviderExists, "",
			"push provider "+req.Name+" already exists")}
	case errors.Is(err, pushprovider.ErrSensitiveNotVaultRef):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("push-provider.create: service failed",
			slog.String("name", req.Name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create push provider failed")}
	}
}

// UpdateTyped — доменная функция PUT /v1/push-providers/{name} (handler-native):
// replace-семантика (req.Params полностью заменяет существующий набор — read-modify-write
// на клиенте, НЕ presence-tier). Валидация path-name + svc.Update + sentinel→problem.
// Ошибки — *problemError; успех — [PushProviderWriteReply] (200-тело + audit-поля).
func (h *PushProviderHandler) UpdateTyped(ctx context.Context, claims *keeperjwt.Claims, name string, req PushProviderUpdateInput) (PushProviderWriteReply, error) {
	var zero PushProviderWriteReply
	if !pushprovider.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+pushprovider.NamePattern)}
	}
	p, err := h.svc.Update(ctx, pushprovider.UpdateInput{
		Name:      name,
		Params:    req.Params,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		return PushProviderWriteReply{Body: toPushProviderView(p), Name: p.Name, ParamsKeys: paramKeysSorted(p.Params)}, nil
	case errors.Is(err, pushprovider.ErrPushProviderNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "push provider "+name+" not found")}
	case errors.Is(err, pushprovider.ErrSensitiveNotVaultRef):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("push-provider.update: service failed",
			slog.String("name", name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "update push provider failed")}
	}
}

// PushProviderDeleteReply — извлечённый результат [PushProviderHandler.DeleteTyped]
// (handler-native). Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type PushProviderDeleteReply struct {
	Name string
}

// AuditPayload собирает audit-payload delete-роута (parity легаси: name).
func (r PushProviderDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// DeleteTyped — доменная функция DELETE /v1/push-providers/{name} (handler-native):
// валидация path-name + svc.Delete + sentinel→problem. Ошибки — *problemError; успех —
// [PushProviderDeleteReply].
func (h *PushProviderHandler) DeleteTyped(ctx context.Context, name string) (PushProviderDeleteReply, error) {
	var zero PushProviderDeleteReply
	if !pushprovider.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+pushprovider.NamePattern)}
	}
	err := h.svc.Delete(ctx, name)
	switch {
	case err == nil:
		return PushProviderDeleteReply{Name: name}, nil
	case errors.Is(err, pushprovider.ErrPushProviderNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "push provider "+name+" not found")}
	default:
		h.logger.Error("push-provider.delete: service failed",
			slog.String("name", name),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete push provider failed")}
	}
}

// ListTyped — доменная функция GET /v1/push-providers (handler-native, read-with-typed-
// query, БЕЗ audit). namePattern (LIKE-prefix) + offset/limit приходят уже
// провалидированными (huma-bind int32; диапазон enforce-ит CheckPageBounds → 400). Ошибка
// чтения → *problemError (500). Wire-форма items (toPushProviderView) сохранена.
func (h *PushProviderHandler) ListTyped(ctx context.Context, namePattern string, offset, limit int) (PushProviderListPage, error) {
	var zero PushProviderListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	items, total, err := h.svc.List(ctx, pushprovider.ListFilter{NamePattern: namePattern}, offset, limit)
	if err != nil {
		h.logger.Error("push-provider.list: service failed",
			slog.Int("offset", offset),
			slog.Int("limit", limit),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list push providers failed")}
	}
	out := make([]PushProviderView, 0, len(items))
	for _, p := range items {
		out = append(out, toPushProviderView(p))
	}
	return PushProviderListPage{
		Items:  out,
		Offset: offset,
		Limit:  limit,
		Total:  total,
	}, nil
}

// GetTyped — доменная функция GET /v1/push-providers/{name} (handler-native, read-with-path,
// БЕЗ audit): валидация path-name + svc.Get + sentinel→problem (404/422/500). Ошибки —
// *problemError; успех — [PushProviderView].
func (h *PushProviderHandler) GetTyped(ctx context.Context, name string) (PushProviderView, error) {
	var zero PushProviderView
	if !pushprovider.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+pushprovider.NamePattern)}
	}
	p, err := h.svc.Get(ctx, name)
	switch {
	case err == nil:
		return toPushProviderView(p), nil
	case errors.Is(err, pushprovider.ErrPushProviderNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "push provider "+name+" not found")}
	default:
		h.logger.Error("push-provider.get: service failed",
			slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get push provider failed")}
	}
}

// paramKeysSorted возвращает отсортированный список ключей params для audit-
// payload. Симметрично patterns role.* (`permissions` пишутся в audit, но
// VALUE Param-ов считаются «opaque payload», поэтому фиксируем только ключи —
// факт мутации без раскрытия значений; sensitive-инвариант обеспечивает
// service.validateSensitive).
func paramKeysSorted(params map[string]any) []string {
	if len(params) == 0 {
		return []string{}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	// Малый сорт; повторно сортируется на каждой записи аудита (десятки/секунду —
	// не hot path). strings.SortStrings не делается, чтобы не тянуть sort.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
