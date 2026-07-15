// Sigil handlers for the Operator API (Sigil S4a) — a domain layer over [sigil.Service].
// The same service is called by the MCP tool handler (S4b), which guarantees a single source
// of truth for plugin.allow/revoke/list.
//
// T5d (handler-native): the sigil domain is decoupled from the legacy codegen. *Typed functions
// accept NATIVE input types (organized via huma-input in the api package) and return
// domain result types with FLAT wire fields — the native wire-DTO (OpenAPI schema) is
// built by the api package from these fields. The (w,r) wrappers are gone; HTTP is served by huma
// full-typed (api/huma_sigil.go), MCP calls sigil.Service directly (bypassing the handler).
//
// Business logic (cache-slot read, signing, registry CRUD) lives in [sigil.Service];
// the handler only maps sentinel errors to RFC 7807. RBAC check is in middleware
// (see api/router.go), not here.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

// reSigilSegment — the closed charset for Sigil path segments (namespace / name / ref).
// kebab-case + dots (tags like v1.0.0) + underscore; NO slashes or `..`.
//
// ref as a single path segment (not body / catch-all): a tag-ref (`v1.2.3`)
// fits into a segment without escaping, like SID=FQDN with dots
// (operator-api.md → ID in path). A branch-ref with a slash (`feature/x`) via
// path-DELETE is NOT supported in the MVP (plugins pin to a tag label, not a moving
// branch; variant C: ref is a stable admission label). A slash in ref → 422; a catch-
// all segment is rejected (breaks the {ref}↔chi drift test and allows path traversal).
var reSigilSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// SigilHandler — the three Sigil allow-list endpoints (allow / list / revoke).
// Delegates business logic to [sigil.Service].
//
// All dependencies are immutable; safe for concurrent use — holds no state between
// requests.
type SigilHandler struct {
	svc    *sigil.Service
	logger *slog.Logger
}

// NewSigilHandler creates the handler. svc is required (panics on nil —
// the single misconfiguration point, the caller must pass non-nil).
func NewSigilHandler(svc *sigil.Service, logger *slog.Logger) *SigilHandler {
	if svc == nil {
		panic("handlers.NewSigilHandler: sigil.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &SigilHandler{svc: svc, logger: logger}
}

// SigilSpecStub — a non-empty *SigilHandler stub for generating the huma OpenAPI
// fragment (HumaSigilSpecYAML): the domain handler is not invoked during dump, but
// huma.Register requires non-nil for its no-op nil-check. svc is nil — the handler
// never executes in spec mode (parity with [RoleSpecStub]).
func SigilSpecStub() *SigilHandler {
	return &SigilHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// SigilAllowInput — the NATIVE request shape for POST /v1/plugins/sigils (handler-native
// T5d). Replaces PluginSigilAllowRequest: huma-input (package api) binds/
// validates the body against its own fields, then calls AllowTyped with this flat model.
// Segment format (reSigilSegment) is a domain validation in AllowTyped (422).
type SigilAllowInput struct {
	Namespace string
	Name      string
	Ref       string
}

// SigilAllowView — a FLAT domain projection of the 201 body for POST /v1/plugins/sigils
// (handler-native T5d). Package api projects it into the native PluginSigilAllowReply schema
// (register-func). namespace/name/ref (echoed triple) + sha256 (computed by the Keeper).
type SigilAllowView struct {
	Namespace string
	Name      string
	Ref       string
	SHA256    string
}

// SigilView — a FLAT domain projection of a single allow-list entry (element of
// SigilListPage.Items), handler-native T5d. Package api projects it into the native
// PluginSigilView schema. AllowedAt/RevokedAt are already truncated to seconds (parity with the legacy wire);
// RevokedAt is nil for active entries → the key is omitted by the native type. WITHOUT signature/manifest.
type SigilView struct {
	Namespace    string
	Name         string
	Ref          string
	SHA256       string
	AllowedByAID string
	AllowedAt    time.Time
	RevokedAt    *time.Time
}

// SigilListPage — the domain result of GET /v1/plugins/sigils (handler-native T5d).
// Package api projects Items → native PluginSigilListReply (items non-nil → `[]`).
type SigilListPage struct {
	Items []SigilView
}

// SigilAllowReply — the result of [SigilHandler.AllowTyped] (handler-native). Carries
// the domain projection of the 201 body (SigilAllowView) + the caller AID (for the audit payload).
type SigilAllowReply struct {
	View      SigilAllowView
	CallerAID string
}

// AuditPayload assembles the audit payload for the allow route (parity with the legacy: namespace/name/
// ref/sha256/allowed_by_aid; without signature/manifest).
func (r SigilAllowReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"namespace":      r.View.Namespace,
		"name":           r.View.Name,
		"ref":            r.View.Ref,
		"sha256":         r.View.SHA256,
		"allowed_by_aid": r.CallerAID,
	}
}

// AllowTyped — the domain function for POST /v1/plugins/sigils (handler-native): validates
// the triple + svc.Allow + sentinel→problem. Errors are *problemError; success is
// [SigilAllowReply] (domain projection of the 201 body + audit fields).
func (h *SigilHandler) AllowTyped(ctx context.Context, claims *jwt.Claims, in SigilAllowInput) (SigilAllowReply, error) {
	var zero SigilAllowReply
	if msg, valid := validateSigilTriple(in.Namespace, in.Name, in.Ref); !valid {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", msg)}
	}

	sha256, err := h.svc.Allow(ctx, sigil.AllowInput{
		Namespace: in.Namespace,
		Name:      in.Name,
		Ref:       in.Ref,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, sigil.ErrPluginNotInCache):
		return zero, &problemError{problem.New(problem.TypePluginNotInCache, "",
			"plugin "+in.Namespace+"-"+in.Name+" not found in host cache")}
	case errors.Is(err, sigil.ErrSigilAlreadyActive):
		return zero, &problemError{problem.New(problem.TypeSigilActive, "",
			"an active sigil already exists for "+in.Namespace+"/"+in.Name+"/"+in.Ref)}
	default:
		h.logger.Error("plugin.allow: service failed",
			slog.String("namespace", in.Namespace),
			slog.String("name", in.Name),
			slog.String("ref", in.Ref),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "allow plugin failed")}
	}

	return SigilAllowReply{
		View: SigilAllowView{
			Namespace: in.Namespace,
			Name:      in.Name,
			Ref:       in.Ref,
			SHA256:    sha256,
		},
		CallerAID: claims.Subject,
	}, nil
}

// ListTyped — the domain function for GET /v1/plugins/sigils (handler-native, READ without
// audit): reads the registry of active grants and assembles [SigilListPage] (items non-nil).
// A read error → *problemError (500). date-time → UTC+Truncate(Second) (nanoseconds
// don't leak into the wire); RevokedAt is nil for active entries → the key is omitted by the native type.
func (h *SigilHandler) ListTyped(ctx context.Context) (SigilListPage, error) {
	views, err := h.svc.List(ctx)
	if err != nil {
		h.logger.Error("plugin.list: service failed", slog.Any("error", err))
		return SigilListPage{}, &problemError{problem.New(problem.TypeInternalError, "", "list sigils failed")}
	}

	items := make([]SigilView, 0, len(views))
	for _, v := range views {
		it := SigilView{
			Namespace:    v.Namespace,
			Name:         v.Name,
			Ref:          v.Ref,
			SHA256:       v.SHA256,
			AllowedByAID: v.AllowedByAID,
			AllowedAt:    v.AllowedAt.UTC().Truncate(time.Second),
		}
		if v.RevokedAt != nil {
			t := v.RevokedAt.UTC().Truncate(time.Second)
			it.RevokedAt = &t
		}
		items = append(items, it)
	}
	return SigilListPage{Items: items}, nil
}

// SigilRevokeReply — the extracted result of [SigilHandler.RevokeTyped] (FULL-TYPED).
// Carries audit fields (the HTTP response is an empty 204 body).
type SigilRevokeReply struct {
	Namespace string
	Name      string
	Ref       string
}

// AuditPayload assembles the audit payload for the revoke route (parity with the legacy: namespace/name/
// ref). Shared between (w,r) and huma-B.
func (r SigilRevokeReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"namespace": r.Namespace,
		"name":      r.Name,
		"ref":       r.Ref,
	}
}

// RevokeTyped — the extracted domain function for DELETE /v1/plugins/sigils/{namespace}/
// {name}/{ref} (FULL-TYPED ADR-054 §Pattern (b)): validates the triple of path segments +
// svc.Revoke + sentinel→problem. Errors are *problemError; success is [SigilRevokeReply].
func (h *SigilHandler) RevokeTyped(ctx context.Context, claims *jwt.Claims, namespace, name, ref string) (SigilRevokeReply, error) {
	var zero SigilRevokeReply
	if msg, valid := validateSigilTriple(namespace, name, ref); !valid {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", msg)}
	}

	err := h.svc.Revoke(ctx, namespace, name, ref, claims.Subject)
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, sigil.ErrSigilNotFound):
		return zero, &problemError{problem.New(problem.TypeSigilNotFound, "",
			"no active sigil for "+namespace+"/"+name+"/"+ref)}
	default:
		h.logger.Error("plugin.revoke: service failed",
			slog.String("namespace", namespace),
			slog.String("name", name),
			slog.String("ref", ref),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "revoke plugin failed")}
	}

	return SigilRevokeReply{Namespace: namespace, Name: name, Ref: ref}, nil
}

// validateSigilTriple checks the (namespace, name, ref) triple against
// [reSigilSegment]. Returns (human-readable msg, false) at the first
// invalid part, ("", true) if all are valid.
func validateSigilTriple(namespace, name, ref string) (string, bool) {
	switch {
	case namespace == "":
		return "field 'namespace' is required", false
	case !reSigilSegment.MatchString(namespace):
		return "field 'namespace' must match " + reSigilSegment.String(), false
	case name == "":
		return "field 'name' is required", false
	case !reSigilSegment.MatchString(name):
		return "field 'name' must match " + reSigilSegment.String(), false
	case ref == "":
		return "field 'ref' is required", false
	case !reSigilSegment.MatchString(ref):
		return "field 'ref' must match " + reSigilSegment.String() + " (branch-refs with '/' are not supported via path in MVP)", false
	}
	return "", true
}
