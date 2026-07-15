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

// ProviderHandler — CRUD endpoints for the Cloud-Provider registry (`providers`,
// ADR-017, docs/keeper/cloud.md). A thin wrapper over [provider.Service]: the same
// service is called by the MCP tool handler (single source of truth REST↔MCP).
//
// RBAC checks — in middleware (router.go); the handler maps domain errors to
// RFC 7807 (*Typed → *problemError). HTTP is served by huma full-typed; package
// api builds the wire-DTO from the flat domain views (ProviderView).
//
// Secret hygiene: `credentials_ref` is returned as a PATH (`vault:<path>`); the
// credentials themselves are NOT resolved and NOT returned.
type ProviderHandler struct {
	svc    *provider.Service
	logger *slog.Logger
}

// NewProviderHandler creates a handler. svc is mandatory (panic on nil —
// the single misconfiguration point).
func NewProviderHandler(svc *provider.Service, logger *slog.Logger) *ProviderHandler {
	if svc == nil {
		panic("handlers.NewProviderHandler: provider.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ProviderHandler{svc: svc, logger: logger}
}

// ProviderSpecStub — a non-empty stub for generating the huma-OpenAPI fragment
// (svc nil — the handler never executes in spec mode; parity with
// PushProviderSpecStub).
func ProviderSpecStub() *ProviderHandler {
	return &ProviderHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ProviderCreateInput — NATIVE request form of POST /v1/providers (handler-native).
type ProviderCreateInput struct {
	Name           string
	Type           string
	Region         string
	CredentialsRef string
	// Credentials — optional plaintext cloud credentials (dual-mode, ADR-064); XOR with
	// CredentialsRef. The service materializes them into Vault; plaintext is not persisted.
	Credentials map[string]any
	// FQDNSuffix — optional VM FQDN suffix (self-onboard Variant T, ADR-017(h)).
	// Empty/nil → self-onboard is unavailable for the provider.
	FQDNSuffix *string
}

// ProviderView — FLAT wire form of a Provider (Create-201 / Get-200 / list element).
// created_at — nanosecond time-wire; created_by_aid — an optional pointer (NULL for
// rows that outlived the operator's deletion).
type ProviderView struct {
	Name           string
	Type           string
	Region         string
	CredentialsRef string
	FQDNSuffix     *string
	CreatedAt      time.Time
	CreatedByAID   *string
}

// ProviderListPage — domain paged result of GET /v1/providers (handler-native).
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

// ProviderWriteReply — result of CreateTyped: 201 body + audit fields.
// credentials_ref is written to audit as a PATH (not a secret; vault:<path>).
type ProviderWriteReply struct {
	Body           ProviderView
	Name           string
	Type           string
	Region         string
	CredentialsRef string
	FQDNSuffix     *string
	// SecretWritten — keeper wrote plaintext credentials to Vault (ADR-064 audit).
	SecretWritten bool
}

// AuditPayload assembles the audit payload of the Provider create route.
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
	// plaintext_ingested — marker that keeper wrote credentials (ADR-064), without plaintext.
	if r.SecretWritten {
		p["plaintext_ingested"] = true
	}
	return p
}

// ProviderDeleteReply — result of DeleteTyped (audit fields; HTTP response 204).
type ProviderDeleteReply struct {
	Name string
}

// AuditPayload assembles the audit payload of the delete route.
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
	// credentials_ref / credentials — dual-mode XOR (ADR-064); формат/XOR/plaintext-
	// disabled валидирует сервис (provider.IsValidationError → 422 ниже).
	//
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
		Credentials:    req.Credentials,
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
			SecretWritten:  p.SecretWritten,
		}, nil
	case errors.Is(err, provider.ErrProviderAlreadyExists):
		return zero, &problemError{problem.New(problem.TypeProviderExists, "",
			"provider "+req.Name+" already exists")}
	case provider.IsValidationError(err):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", provider.PublicMessage(err))}
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
