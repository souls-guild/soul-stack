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
	ID string
	// Label — optional display caption (ADR-0085): free text, changed afterwards
	// by PUT /v1/profiles/{id}/label. nil/blank → NULL, and the consumer shows
	// Name.
	Label     *string
	Provider  string
	Params    *map[string]any
	CloudInit *string
}

// ProfileView — the FLAT wire shape of a Profile (Create-201 / Get-200 / list element).
// params normalized nil→{}; cloud_init / created_by_aid — optional pointers;
// created_at — nanosecond time-wire.
type ProfileView struct {
	ID string
	// Label — display caption (ADR-0085); nil when the column is NULL, and the
	// consumer then shows Name.
	Label        *string
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
		ID:           p.ID,
		Label:        p.Label,
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
	ID         string
	Label      *string
	Provider   string
	ParamsKeys []string
}

// AuditPayload builds the audit payload of the create route. VALUE params are NOT written.
func (r ProfileWriteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"id":          r.ID,
		"label":       r.Label,
		"provider":    r.Provider,
		"params_keys": r.ParamsKeys,
	}
}

// ProfileDeleteReply — the result of DeleteTyped (audit fields; HTTP response 204).
type ProfileDeleteReply struct {
	ID string
}

// AuditPayload builds the audit payload of the delete route.
func (r ProfileDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"id": r.ID}
}

// CreateTyped — the domain function POST /v1/profiles (handler-native): validates
// name/provider + svc.Create + sentinel→problem. 409 on duplicate name; 422 on
// a reference to a nonexistent Provider (FK) or a bad name/provider.
func (h *ProfileHandler) CreateTyped(ctx context.Context, claims *keeperjwt.Claims, req ProfileCreateInput) (ProfileWriteReply, error) {
	var zero ProfileWriteReply
	if req.ID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'id' is required")}
	}
	if !profile.ValidID(req.ID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'id' must match "+profile.IDPattern)}
	}
	if req.Provider == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'provider' is required")}
	}
	if !profile.ValidID(req.Provider) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'provider' must match "+profile.IDPattern)}
	}

	var params map[string]any
	if req.Params != nil {
		params = *req.Params
	}
	p, err := h.svc.Create(ctx, profile.CreateInput{
		ID:        req.ID,
		Label:     req.Label,
		Provider:  req.Provider,
		Params:    params,
		CloudInit: req.CloudInit,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		return ProfileWriteReply{
			Body:       toProfileView(p),
			ID:         p.ID,
			Label:      p.Label,
			Provider:   p.Provider,
			ParamsKeys: paramKeysSorted(p.Params),
		}, nil
	case errors.Is(err, profile.ErrProfileAlreadyExists):
		return zero, &problemError{problem.New(problem.TypeProfileExists, "",
			"profile "+req.ID+" already exists")}
	case errors.Is(err, profile.ErrProviderNotFound):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"referenced provider "+req.Provider+" does not exist")}
	default:
		h.logger.Error("profile.create: service failed",
			slog.String("id", req.ID),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create profile failed")}
	}
}

// GetTyped — the domain function GET /v1/profiles/{id} (read, no audit).
func (h *ProfileHandler) GetTyped(ctx context.Context, id string) (ProfileView, error) {
	var zero ProfileView
	if !profile.ValidID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must match "+profile.IDPattern)}
	}
	p, err := h.svc.Get(ctx, id)
	switch {
	case err == nil:
		return toProfileView(p), nil
	case errors.Is(err, profile.ErrProfileNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "profile "+id+" not found")}
	default:
		h.logger.Error("profile.get: service failed", slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get profile failed")}
	}
}

// DeleteTyped — the domain function DELETE /v1/profiles/{id}: 404 when absent.
func (h *ProfileHandler) DeleteTyped(ctx context.Context, id string) (ProfileDeleteReply, error) {
	var zero ProfileDeleteReply
	if !profile.ValidID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must match "+profile.IDPattern)}
	}
	err := h.svc.Delete(ctx, id)
	switch {
	case err == nil:
		return ProfileDeleteReply{ID: id}, nil
	case errors.Is(err, profile.ErrProfileNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "profile "+id+" not found")}
	default:
		h.logger.Error("profile.delete: service failed", slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "delete profile failed")}
	}
}

// SetLabelTyped — domain function for PUT /v1/profiles/{id}/label
// (WRITE+AUDIT profile.label_changed). 404 if absent.
//
// The label itself is NOT validated: free text with capitals, spaces and
// punctuation is what the field carries (ADR-0085), so the only 422 this route
// can raise is on the path identifier, which must still be a well-formed name
// because it addresses the row.
func (h *ProfileHandler) SetLabelTyped(ctx context.Context, id string, req LabelSetInput) (LabelWriteReply[ProfileView], error) {
	var zero LabelWriteReply[ProfileView]
	if !profile.ValidID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must match "+profile.IDPattern)}
	}
	p, previous, err := h.svc.SetLabel(ctx, id, req.Label)
	switch {
	case err == nil:
		return LabelWriteReply[ProfileView]{Body: toProfileView(p), ID: id, Label: p.Label, Previous: previous}, nil
	case errors.Is(err, profile.ErrProfileNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "profile "+id+" not found")}
	default:
		h.logger.Error("profile.label-set: service failed", slog.String("id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "set profile label failed")}
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
