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
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// ProviderHandler — endpoints CRUD реестра Cloud-Provider-ов (`providers`,
// ADR-017, docs/keeper/cloud.md). Тонкая обёртка над [provider.Service]: тот
// же service зовёт MCP-tool-handler (один источник правды REST↔MCP).
//
// RBAC-проверка — в middleware (router.go); handler маппит доменные ошибки в
// RFC 7807 (*Typed → *problemError). HTTP обслуживает huma full-typed; пакет
// api строит wire-DTO из плоских доменных view-ов (ProviderView).
//
// Секрет-гигиена: `credentials_ref` отдаётся как ПУТЬ (`vault:<path>`); сами
// credentials НЕ резолвятся и НЕ возвращаются.
type ProviderHandler struct {
	svc    *provider.Service
	logger *slog.Logger
}

// NewProviderHandler создаёт handler. svc обязателен (panic при nil —
// единственная точка misconfiguration).
func NewProviderHandler(svc *provider.Service, logger *slog.Logger) *ProviderHandler {
	if svc == nil {
		panic("handlers.NewProviderHandler: provider.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ProviderHandler{svc: svc, logger: logger}
}

// ProviderSpecStub — непустая заглушка для генерации huma-OpenAPI-фрагмента
// (svc nil — handler никогда не исполняется в spec-режиме; parity
// PushProviderSpecStub).
func ProviderSpecStub() *ProviderHandler {
	return &ProviderHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ProviderCreateInput — NATIVE request-форма POST /v1/providers (handler-native).
type ProviderCreateInput struct {
	Name           string
	Type           string
	Region         string
	CredentialsRef string
	// FQDNSuffix — опц. суффикс FQDN VM (self-onboard Вариант T, ADR-017(h)).
	// Пусто/nil → self-onboard недоступен для провайдера.
	FQDNSuffix *string
}

// ProviderView — ПЛОСКАЯ wire-форма Provider-а (Create-201 / Get-200 / list-element).
// created_at — наносекундный time-wire; created_by_aid — опц. указатель (NULL у
// записей, переживших удаление оператора).
type ProviderView struct {
	Name           string
	Type           string
	Region         string
	CredentialsRef string
	FQDNSuffix     *string
	CreatedAt      time.Time
	CreatedByAID   *string
}

// ProviderListPage — доменный paged-результат GET /v1/providers (handler-native).
type ProviderListPage struct {
	Items  []ProviderView
	Offset int
	Limit  int
	Total  int
}

func toProviderView(p *provider.Provider) ProviderView {
	return ProviderView{
		Name:           p.Name,
		Type:           p.Type,
		Region:         p.Region,
		CredentialsRef: p.CredentialsRef,
		FQDNSuffix:     p.FQDNSuffix,
		CreatedAt:      p.CreatedAt.UTC(),
		CreatedByAID:   p.CreatedByAID,
	}
}

// ProviderWriteReply — результат CreateTyped: 201-тело + audit-поля.
// credentials_ref в audit пишется как ПУТЬ (не секрет; vault:<path>).
type ProviderWriteReply struct {
	Body           ProviderView
	Name           string
	Type           string
	Region         string
	CredentialsRef string
	FQDNSuffix     *string
}

// AuditPayload собирает audit-payload create-роута Provider-а.
func (r ProviderWriteReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{
		"name":            r.Name,
		"type":            r.Type,
		"region":          r.Region,
		"credentials_ref": r.CredentialsRef,
	}
	if r.FQDNSuffix != nil {
		p["fqdn_suffix"] = *r.FQDNSuffix
	}
	return p
}

// ProviderDeleteReply — результат DeleteTyped (audit-поля; HTTP-ответ 204).
type ProviderDeleteReply struct {
	Name string
}

// AuditPayload собирает audit-payload delete-роута.
func (r ProviderDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// CreateTyped — доменная функция POST /v1/providers (handler-native): валидация
// полей + svc.Create + sentinel→problem. 409 на дубль name; 422 на битый
// name/type/region/credentials_ref.
func (h *ProviderHandler) CreateTyped(ctx context.Context, claims *keeperjwt.Claims, req ProviderCreateInput) (ProviderWriteReply, error) {
	var zero ProviderWriteReply
	if req.Name == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'name' is required")}
	}
	if !provider.ValidName(req.Name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'name' must match "+provider.NamePattern)}
	}
	if !provider.ValidName(req.Type) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'type' must match "+provider.NamePattern)}
	}
	if req.Region == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'region' is required")}
	}
	if !provider.ValidCredentialsRef(req.CredentialsRef) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'credentials_ref' must start with "+provider.CredentialsRefPrefix+" and carry a path")}
	}
	// fqdn_suffix опционален (self-onboard Вариант T); если задан — валидируем
	// формат до round-trip-а (пустая строка не допускается — используй отсутствие).
	if req.FQDNSuffix != nil {
		if *req.FQDNSuffix == "" || !provider.ValidFQDNSuffix(*req.FQDNSuffix) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'fqdn_suffix' must match "+provider.FQDNSuffixPattern+" (omit the field for none)")}
		}
	}

	p, err := h.svc.Create(ctx, provider.CreateInput{
		Name:           req.Name,
		Type:           req.Type,
		Region:         req.Region,
		CredentialsRef: req.CredentialsRef,
		FQDNSuffix:     req.FQDNSuffix,
		CallerAID:      claims.Subject,
	})
	switch {
	case err == nil:
		return ProviderWriteReply{
			Body:           toProviderView(p),
			Name:           p.Name,
			Type:           p.Type,
			Region:         p.Region,
			CredentialsRef: p.CredentialsRef,
			FQDNSuffix:     p.FQDNSuffix,
		}, nil
	case errors.Is(err, provider.ErrProviderAlreadyExists):
		return zero, &problemError{problem.New(problem.TypeProviderExists, "",
			"provider "+req.Name+" already exists")}
	default:
		h.logger.Error("provider.create: service failed",
			slog.String("name", req.Name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create provider failed")}
	}
}

// GetTyped — доменная функция GET /v1/providers/{name} (read, БЕЗ audit).
func (h *ProviderHandler) GetTyped(ctx context.Context, name string) (ProviderView, error) {
	var zero ProviderView
	if !provider.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+provider.NamePattern)}
	}
	p, err := h.svc.Get(ctx, name)
	switch {
	case err == nil:
		return toProviderView(p), nil
	case errors.Is(err, provider.ErrProviderNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "provider "+name+" not found")}
	default:
		h.logger.Error("provider.get: service failed", slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get provider failed")}
	}
}

// DeleteTyped — доменная функция DELETE /v1/providers/{name}: 404 на отсутствие,
// 409 при зависимых Profile-ях (FK RESTRICT).
func (h *ProviderHandler) DeleteTyped(ctx context.Context, name string) (ProviderDeleteReply, error) {
	var zero ProviderDeleteReply
	if !provider.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+provider.NamePattern)}
	}
	err := h.svc.Delete(ctx, name)
	switch {
	case err == nil:
		return ProviderDeleteReply{Name: name}, nil
	case errors.Is(err, provider.ErrProviderNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "provider "+name+" not found")}
	case errors.Is(err, provider.ErrProviderHasProfiles):
		return zero, &problemError{problem.New(problem.TypeProviderHasProfiles, "",
			"provider "+name+" has dependent profiles; delete them first")}
	default:
		h.logger.Error("provider.delete: service failed", slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete provider failed")}
	}
}

// ListTyped — доменная функция GET /v1/providers (read-with-typed-query, БЕЗ audit).
func (h *ProviderHandler) ListTyped(ctx context.Context, offset, limit int) (ProviderListPage, error) {
	var zero ProviderListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	items, total, err := h.svc.List(ctx, offset, limit)
	if err != nil {
		h.logger.Error("provider.list: service failed",
			slog.Int("offset", offset), slog.Int("limit", limit), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list providers failed")}
	}
	out := make([]ProviderView, 0, len(items))
	for _, p := range items {
		out = append(out, toProviderView(p))
	}
	return ProviderListPage{Items: out, Offset: offset, Limit: limit, Total: total}, nil
}
