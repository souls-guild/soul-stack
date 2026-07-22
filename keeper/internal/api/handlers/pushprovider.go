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

// PushProviderHandler — CRUD endpoints for push_providers (ADR-032 amendment
// 2026-05-26, S7-2). A thin wrapper over [pushprovider.Service]: the same service
// is called by the MCP tool handler, which guarantees a single source of truth for the five
// push-provider.* endpoints.
//
// The RBAC check happens in middleware (see api/router.go); the handler
// only maps errors to RFC 7807.
//
// T5d-2c-full (handler-native): the domain is detached from the legacy generator. The *Typed functions
// accept/return NATIVE types with flat wire fields (PushProviderCreateInput /
// PushProviderUpdateInput / PushProviderView / PushProviderListPage); the native wire-DTO
// (OpenAPI schema) is built by package api from these fields (register func huma_pushprovider.go).
// HTTP is served by huma full-typed, MCP calls pushprovider.Service directly (bypassing the handler).
type PushProviderHandler struct {
	svc    *pushprovider.Service
	logger *slog.Logger
}

// NewPushProviderHandler creates the handler. svc is required (panic on nil —
// the only misconfiguration point, the caller must pass non-nil).
func NewPushProviderHandler(svc *pushprovider.Service, logger *slog.Logger) *PushProviderHandler {
	if svc == nil {
		panic("handlers.NewPushProviderHandler: pushprovider.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &PushProviderHandler{svc: svc, logger: logger}
}

// PushProviderSpecStub — a non-empty *PushProviderHandler stub for generating the huma
// OpenAPI fragment (HumaPushProviderSpecYAML): on dump the domain handler is never
// called, but huma.Register requires non-nil for its no-op nil check. svc nil —
// the handler never executes in spec mode (parity with [SigilSpecStub]).
func PushProviderSpecStub() *PushProviderHandler {
	return &PushProviderHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// PushProviderCreateInput — the NATIVE request form of POST /v1/push-providers (handler-native).
// Replaces PushProviderCreateRequest: the huma input (package api) binds/validates the body and
// projects it into these fields. Params — an optional pointer (*map), the handler dereferences
// it into pushprovider.CreateInput.
type PushProviderCreateInput struct {
	Name   string
	Params *map[string]any
}

// PushProviderUpdateInput — the NATIVE request form of PUT /v1/push-providers/{name} (handler-
// native). Replaces PushProviderUpdateRequest. Replace semantics: params fully
// replaces the existing set.
type PushProviderUpdateInput struct {
	Params map[string]any
}

// PushProviderView — the FLAT wire form of a Push Provider (Create-201, Get-200, List items,
// Update-200), handler-native (replaces PushProvider). params normalized nil→{};
// updated_by_aid — optional pointer; created_at/updated_at — nanosecond time-wire.
// Package api projects it into native PushProvider (register func huma_pushprovider.go),
// the native type fixes the wire field order.
type PushProviderView struct {
	Name         string
	Params       map[string]any
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CreatedByAID string
	UpdatedByAID *string
}

// PushProviderListPage — the domain paged result of GET /v1/push-providers (handler-native).
// Flat offset/limit/total + a slice of PushProviderView; package api projects it into the native
// envelope PushProviderListReply (register func huma_pushprovider.go).
type PushProviderListPage struct {
	Items  []PushProviderView
	Offset int
	Limit  int
	Total  int
}

// toPushProviderView projects [pushprovider.PushProvider] into the flat view.
// date-time: the former serialization was a bare time.Time field (RFC3339Nano via MarshalJSON),
// so `.UTC()` without Truncate keeps it byte-for-byte. nil params normalize to an empty
// map (parity with the former behavior).
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

// PushProviderWriteReply — the extracted result of the Push Provider write routes
// (CreateTyped/UpdateTyped/DeleteTyped). Carries the body (create/update — 201/200
// PushProviderView; delete — empty) + name/params_keys (for the audit payload; the params
// VALUEs are NOT put into audit — sensitive invariant).
type PushProviderWriteReply struct {
	Body       PushProviderView
	Name       string
	ParamsKeys []string
}

// AuditPayload assembles the audit payload for the Push Provider write routes (legacy parity:
// name + params_keys without values). Source for huma variant B.
func (r PushProviderWriteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"name":        r.Name,
		"params_keys": r.ParamsKeys,
	}
}

// CreateTyped — the domain function for POST /v1/push-providers (handler-native): name validation +
// svc.Create + sentinel→problem. Errors — *problemError; success — [PushProviderWriteReply]
// (201 body + audit fields).
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

// UpdateTyped — the domain function for PUT /v1/push-providers/{name} (handler-native):
// replace semantics (req.Params fully replaces the existing set — read-modify-write
// on the client, NOT presence-tier). path-name validation + svc.Update + sentinel→problem.
// Errors — *problemError; success — [PushProviderWriteReply] (200 body + audit fields).
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

// PushProviderDeleteReply — the extracted result of [PushProviderHandler.DeleteTyped]
// (handler-native). Carries audit fields (the HTTP response is an empty 204 body).
type PushProviderDeleteReply struct {
	Name string
}

// AuditPayload assembles the audit payload for the delete route (legacy parity: name).
func (r PushProviderDeleteReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"name": r.Name}
}

// DeleteTyped — the domain function for DELETE /v1/push-providers/{name} (handler-native):
// path-name validation + svc.Delete + sentinel→problem. Errors — *problemError; success —
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

// ListTyped — the domain function for GET /v1/push-providers (handler-native, read with typed
// query, no audit). namePattern (LIKE prefix) + offset/limit arrive already
// validated (huma binds int32; CheckPageBounds enforces the range → 400). A read
// error → *problemError (500). The items wire shape (toPushProviderView) is preserved.
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

// GetTyped — the domain function for GET /v1/push-providers/{name} (handler-native, read with path,
// no audit): path-name validation + svc.Get + sentinel→problem (404/422/500). Errors —
// *problemError; success — [PushProviderView].
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

// paramKeysSorted returns the sorted list of params keys for the audit
// payload. Like the role.* patterns (`permissions` are written to audit, but
// the Param VALUEs are treated as an "opaque payload", so we record only the keys —
// the fact of mutation without exposing values; the sensitive invariant is enforced by
// service.validateSensitive).
func paramKeysSorted(params map[string]any) []string {
	if len(params) == 0 {
		return []string{}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	// Small sort; re-sorted on every audit write (tens per second —
	// not a hot path). We avoid strings.SortStrings so as not to pull in sort.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
