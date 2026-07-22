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

// ProfileHandler — CRUD endpoints for the Cloud Profile registry (`profiles`,
// ADR-017, docs/keeper/cloud.md). A thin wrapper over [profile.Service] (single
// source of truth REST↔MCP). Profile is a VM spec on top of a Provider.
//
// Secret hygiene: VALUE params are NOT put into audit (keys only); a freeform
// VM spec may carry sensitive values.
type ProfileHandler struct {
	svc    *profile.Service
	logger *slog.Logger
}

// NewProfileHandler builds the handler. svc is required (panics on nil).
func NewProfileHandler(svc *profile.Service, logger *slog.Logger) *ProfileHandler {
	if svc == nil {
		panic("handlers.NewProfileHandler: profile.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ProfileHandler{svc: svc, logger: logger}
}

// ProfileSpecStub — a non-empty stub for generating the huma OpenAPI fragment.
func ProfileSpecStub() *ProfileHandler {
	return &ProfileHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ProfileCreateInput — the NATIVE request shape of POST /v1/profiles (handler-native).
// Params — optional pointer (nil → {}); CloudInit — optional userdata.
type ProfileCreateInput struct {
	Name      string
	Provider  string
	Params    *map[string]any
	CloudInit *string
}

// ProfileView — the FLAT wire shape of a Profile (Create-201 / Get-200 / list element).
// params normalized nil→{}; cloud_init / created_by_aid — optional pointers;
// created_at — nanosecond time-wire.
type ProfileView struct {
	Name         string
	Provider     string
	Params       map[string]any
	CloudInit    *string
	CreatedAt    time.Time
	CreatedByAID *string
}

// ProfileListPage — the domain paged result of GET /v1/profiles (handler-native).
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

// ProfileWriteReply — the result of CreateTyped: 201 body + audit fields (name +
// provider + params_keys without values).
type ProfileWriteReply struct {
	Body       ProfileView
	Name       string
	Provider   string
	ParamsKeys []string
}

// AuditPayload builds the audit payload of the create route. VALUE params are NOT written.
func (r ProfileWriteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":        r.Name,
		"provider":    r.Provider,
		"params_keys": r.ParamsKeys,
	}
}

// ProfileDeleteReply — the result of DeleteTyped (audit fields; HTTP response 204).
type ProfileDeleteReply struct {
	Name string
}

// AuditPayload builds the audit payload of the delete route.
func (r ProfileDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// CreateTyped — the domain function POST /v1/profiles (handler-native): validates
// name/provider + svc.Create + sentinel→problem. 409 on duplicate name; 422 on
// a reference to a nonexistent Provider (FK) or a bad name/provider.
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

// GetTyped — the domain function GET /v1/profiles/{name} (read, no audit).
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

// DeleteTyped — the domain function DELETE /v1/profiles/{name}: 404 when absent.
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

// ListTyped — the domain function GET /v1/profiles (read with typed query, no
// audit). providerFilter non-empty → profiles of a single Provider.
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
