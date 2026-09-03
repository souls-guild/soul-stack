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
	ID string
	// Label — optional display caption (ADR-0085): free text, changed afterwards
	// by PUT /v1/providers/{id}/label. nil/blank → NULL, and the consumer shows
	// Name.
	Label          *string
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
	ID string
	// Label — display caption (ADR-0085); nil when the column is NULL, and the
	// consumer then shows Name.
	Label          *string
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
		ID:             p.ID,
		Label:          p.Label,
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
	ID             string
	Label          *string
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
		"id":              r.ID,
		"type":            r.Type,
		"region":          r.Region,
		"credentials_ref": r.CredentialsRef,
		"label":           r.Label,
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
	ID string
}

// AuditPayload assembles the audit payload of the delete route.
func (r ProviderDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"id": r.ID}
}

// CreateTyped — domain function for POST /v1/providers (handler-native): validates
// the fields + svc.Create + sentinel→problem. 409 on a duplicate name; 422 on a malformed
// name/type/region/credentials_ref.
func (h *ProviderHandler) CreateTyped(ctx context.Context, claims *keeperjwt.Claims, req ProviderCreateInput) (ProviderWriteReply, error) {
	var zero ProviderWriteReply
	if req.ID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'id' is required")}
	}
	if !provider.ValidID(req.ID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'id' must match "+provider.IDPattern)}
	}
	if !provider.ValidID(req.Type) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'type' must match "+provider.IDPattern)}
	}
	if req.Region == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'region' is required")}
	}
	// credentials_ref / credentials — dual-mode XOR (ADR-064); format/XOR/plaintext-
	// disabled is validated by the service (provider.IsValidationError → 422 below).
	//
	// fqdn_suffix is optional (self-onboard Variant T); if set, validate the
	// format before the round-trip (an empty string is not allowed — omit the field instead).
	if req.FQDNSuffix != nil {
		if *req.FQDNSuffix == "" || !provider.ValidFQDNSuffix(*req.FQDNSuffix) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'fqdn_suffix' must match "+provider.FQDNSuffixPattern+" (omit the field for none)")}
		}
	}

	p, err := h.svc.Create(ctx, provider.CreateInput{
		ID:             req.ID,
		Label:          req.Label,
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
			ID:             p.ID,
			Label:          p.Label,
			Type:           p.Type,
			Region:         p.Region,
			CredentialsRef: p.CredentialsRef,
			FQDNSuffix:     p.FQDNSuffix,
			SecretWritten:  p.SecretWritten,
		}, nil
	case errors.Is(err, provider.ErrProviderAlreadyExists):
		return zero, &problemError{problem.New(problem.TypeProviderExists, "",
			"provider "+req.ID+" already exists")}
	case provider.IsValidationError(err):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", provider.PublicMessage(err))}
	default:
		h.logger.Error("provider.create: service failed",
			slog.String("id", req.ID),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create provider failed")}
	}
}

// GetTyped — domain function for GET /v1/providers/{id} (read, no audit).
func (h *ProviderHandler) GetTyped(ctx context.Context, id string) (ProviderView, error) {
	var zero ProviderView
	if !provider.ValidID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must match "+provider.IDPattern)}
	}
	p, err := h.svc.Get(ctx, id)
	switch {
	case err == nil:
		return toProviderView(p), nil
	case errors.Is(err, provider.ErrProviderNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "provider "+id+" not found")}
	default:
		h.logger.Error("provider.get: service failed", slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get provider failed")}
	}
}

// DeleteTyped — domain function for DELETE /v1/providers/{id}: 404 if absent,
// 409 on dependent Profiles (FK RESTRICT).
func (h *ProviderHandler) DeleteTyped(ctx context.Context, id string) (ProviderDeleteReply, error) {
	var zero ProviderDeleteReply
	if !provider.ValidID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must match "+provider.IDPattern)}
	}
	err := h.svc.Delete(ctx, id)
	switch {
	case err == nil:
		return ProviderDeleteReply{ID: id}, nil
	case errors.Is(err, provider.ErrProviderNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "provider "+id+" not found")}
	case errors.Is(err, provider.ErrProviderHasProfiles):
		return zero, &problemError{problem.New(problem.TypeProviderHasProfiles, "",
			"provider "+id+" has dependent profiles; delete them first")}
	default:
		h.logger.Error("provider.delete: service failed", slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete provider failed")}
	}
}

// SetLabelTyped — domain function for PUT /v1/providers/{id}/label
// (WRITE+AUDIT provider.label_changed). 404 if absent.
//
// The label itself is NOT validated: free text with capitals, spaces and
// punctuation is what the field carries (ADR-0085), so the only 422 this route
// can raise is on the path identifier, which must still be a well-formed name
// because it addresses the row.
func (h *ProviderHandler) SetLabelTyped(ctx context.Context, id string, req LabelSetInput) (LabelWriteReply[ProviderView], error) {
	var zero LabelWriteReply[ProviderView]
	if !provider.ValidID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must match "+provider.IDPattern)}
	}
	p, previous, err := h.svc.SetLabel(ctx, id, req.Label)
	switch {
	case err == nil:
		return LabelWriteReply[ProviderView]{Body: toProviderView(p), ID: id, Label: p.Label, Previous: previous}, nil
	case errors.Is(err, provider.ErrProviderNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "provider "+id+" not found")}
	default:
		h.logger.Error("provider.label-set: service failed", slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "set provider label failed")}
	}
}

// ListTyped — domain function for GET /v1/providers (read-with-typed-query, no audit).
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
