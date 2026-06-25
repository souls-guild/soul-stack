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
	"github.com/souls-guild/soul-stack/keeper/internal/profile"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// ProfileHandler — endpoints CRUD реестра Cloud-Profile-ей (`profiles`,
// ADR-017, docs/keeper/cloud.md). Тонкая обёртка над [profile.Service] (один
// источник правды REST↔MCP). Profile — VM-spec поверх Provider-а.
//
// Секрет-гигиена: VALUE params в audit НЕ кладутся (только ключи); freeform
// VM-spec может нести чувствительные значения.
type ProfileHandler struct {
	svc    *profile.Service
	logger *slog.Logger
}

// NewProfileHandler создаёт handler. svc обязателен (panic при nil).
func NewProfileHandler(svc *profile.Service, logger *slog.Logger) *ProfileHandler {
	if svc == nil {
		panic("handlers.NewProfileHandler: profile.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ProfileHandler{svc: svc, logger: logger}
}

// ProfileSpecStub — непустая заглушка для генерации huma-OpenAPI-фрагмента.
func ProfileSpecStub() *ProfileHandler {
	return &ProfileHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ProfileCreateInput — NATIVE request-форма POST /v1/profiles (handler-native).
// Params — опц. указатель (nil → {}); CloudInit — опц. userdata.
type ProfileCreateInput struct {
	Name      string
	Provider  string
	Params    *map[string]any
	CloudInit *string
}

// ProfileView — ПЛОСКАЯ wire-форма Profile-а (Create-201 / Get-200 / list-element).
// params нормализован nil→{}; cloud_init / created_by_aid — опц. указатели;
// created_at — наносекундный time-wire.
type ProfileView struct {
	Name         string
	Provider     string
	Params       map[string]any
	CloudInit    *string
	CreatedAt    time.Time
	CreatedByAID *string
}

// ProfileListPage — доменный paged-результат GET /v1/profiles (handler-native).
type ProfileListPage struct {
	Items  []ProfileView
	Offset int
	Limit  int
	Total  int
}

func toProfileView(p *profile.Profile) ProfileView {
	params := p.Params
	if params == nil {
		params = map[string]any{}
	}
	return ProfileView{
		Name:         p.Name,
		Provider:     p.Provider,
		Params:       params,
		CloudInit:    p.CloudInit,
		CreatedAt:    p.CreatedAt.UTC(),
		CreatedByAID: p.CreatedByAID,
	}
}

// ProfileWriteReply — результат CreateTyped: 201-тело + audit-поля (name +
// provider + params_keys без values).
type ProfileWriteReply struct {
	Body       ProfileView
	Name       string
	Provider   string
	ParamsKeys []string
}

// AuditPayload собирает audit-payload create-роута. VALUE params НЕ пишутся.
func (r ProfileWriteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":        r.Name,
		"provider":    r.Provider,
		"params_keys": r.ParamsKeys,
	}
}

// ProfileDeleteReply — результат DeleteTyped (audit-поля; HTTP-ответ 204).
type ProfileDeleteReply struct {
	Name string
}

// AuditPayload собирает audit-payload delete-роута.
func (r ProfileDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// CreateTyped — доменная функция POST /v1/profiles (handler-native): валидация
// name/provider + svc.Create + sentinel→problem. 409 на дубль name; 422 на
// ссылку на несуществующий Provider (FK) или битый name/provider.
func (h *ProfileHandler) CreateTyped(ctx context.Context, claims *keeperjwt.Claims, req ProfileCreateInput) (ProfileWriteReply, error) {
	var zero ProfileWriteReply
	if req.Name == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'name' is required")}
	}
	if !profile.ValidName(req.Name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'name' must match "+profile.NamePattern)}
	}
	if req.Provider == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'provider' is required")}
	}
	if !profile.ValidName(req.Provider) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'provider' must match "+profile.NamePattern)}
	}

	var params map[string]any
	if req.Params != nil {
		params = *req.Params
	}
	p, err := h.svc.Create(ctx, profile.CreateInput{
		Name:      req.Name,
		Provider:  req.Provider,
		Params:    params,
		CloudInit: req.CloudInit,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		return ProfileWriteReply{
			Body:       toProfileView(p),
			Name:       p.Name,
			Provider:   p.Provider,
			ParamsKeys: paramKeysSorted(p.Params),
		}, nil
	case errors.Is(err, profile.ErrProfileAlreadyExists):
		return zero, &problemError{problem.New(problem.TypeProfileExists, "",
			"profile "+req.Name+" already exists")}
	case errors.Is(err, profile.ErrProviderNotFound):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"referenced provider "+req.Provider+" does not exist")}
	default:
		h.logger.Error("profile.create: service failed",
			slog.String("name", req.Name),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create profile failed")}
	}
}

// GetTyped — доменная функция GET /v1/profiles/{name} (read, БЕЗ audit).
func (h *ProfileHandler) GetTyped(ctx context.Context, name string) (ProfileView, error) {
	var zero ProfileView
	if !profile.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+profile.NamePattern)}
	}
	p, err := h.svc.Get(ctx, name)
	switch {
	case err == nil:
		return toProfileView(p), nil
	case errors.Is(err, profile.ErrProfileNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "profile "+name+" not found")}
	default:
		h.logger.Error("profile.get: service failed", slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get profile failed")}
	}
}

// DeleteTyped — доменная функция DELETE /v1/profiles/{name}: 404 на отсутствие.
func (h *ProfileHandler) DeleteTyped(ctx context.Context, name string) (ProfileDeleteReply, error) {
	var zero ProfileDeleteReply
	if !profile.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+profile.NamePattern)}
	}
	err := h.svc.Delete(ctx, name)
	switch {
	case err == nil:
		return ProfileDeleteReply{Name: name}, nil
	case errors.Is(err, profile.ErrProfileNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "profile "+name+" not found")}
	default:
		h.logger.Error("profile.delete: service failed", slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete profile failed")}
	}
}

// ListTyped — доменная функция GET /v1/profiles (read-with-typed-query, БЕЗ
// audit). providerFilter непуст → список профилей одного Provider-а.
func (h *ProfileHandler) ListTyped(ctx context.Context, providerFilter string, offset, limit int) (ProfileListPage, error) {
	var zero ProfileListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	items, total, err := h.svc.List(ctx, providerFilter, offset, limit)
	if err != nil {
		h.logger.Error("profile.list: service failed",
			slog.Int("offset", offset), slog.Int("limit", limit), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list profiles failed")}
	}
	out := make([]ProfileView, 0, len(items))
	for _, p := range items {
		out = append(out, toProfileView(p))
	}
	return ProfileListPage{Items: out, Offset: offset, Limit: limit, Total: total}, nil
}
