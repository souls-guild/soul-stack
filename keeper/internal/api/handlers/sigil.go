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
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// reSigilRef — the closed charset for a `ref` label. kebab-case + dots (tags like
// v1.0.0) + underscore; NO slashes or `..`.
//
// A branch-ref with a slash (`feature/x`) is NOT supported in the MVP: plugins pin to
// a stable tag label, not a moving branch (variant C). A slash → 422.
var reSigilRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// maxSourceLen bounds the `source` field. A git remote is a URL, not a path segment,
// so it gets a length bound rather than a charset: the scheme allow-list that decides
// whether a source may be reached at all lives in the resolver
// ([plugingit.validateGitScheme]), and duplicating half of it here would be a second
// opinion that can disagree with the one that actually gates egress.
const maxSourceLen = 2048

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

// SigilAllowInput — the NATIVE request shape for POST /v1/plugins/sigils
// (handler-native T5d). huma-input (package api) binds/validates the body against its
// own fields, then calls AllowTyped with this flat model. Field format is a domain
// validation in AllowTyped (422).
//
// Alias is the registration being created; Source and Ref are what the operator
// asserts about the artifact in that slot, and are the only identity the signature
// will be over.
type SigilAllowInput struct {
	Alias  string
	Source string
	Ref    string
}

// SigilArtifactView — a FLAT domain projection of one approved artifact of a release
// (NIM-793): the platform it serves, where it is published under the grant's source,
// and its digest.
type SigilArtifactView struct {
	OS     string
	Arch   string
	Path   string
	SHA256 string
}

// SigilAllowView — a FLAT domain projection of the 201 body for POST /v1/plugins/sigils
// (handler-native T5d). Package api projects it into the native PluginSigilAllowReply
// schema (register-func). alias/source/ref (echoed) + kind and the artifact list the
// Keeper resolved and signed.
//
// The reply is a release and not a hash. `plugin.allow` confirms a release: an operator
// who approved linux/amd64 and linux/arm64 in one gesture is told which two files that
// was, because the alternative — one digest — would be the reply for a request nobody
// made.
type SigilAllowView struct {
	Alias     string
	Source    string
	Ref       string
	Kind      string
	Artifacts []SigilArtifactView
}

// SigilView — a FLAT domain projection of a single allow-list entry (element of
// SigilListPage.Items), handler-native T5d. Package api projects it into the native
// PluginSigilView schema. AllowedAt/RevokedAt are already truncated to seconds (parity with the legacy wire);
// RevokedAt is nil for active entries → the key is omitted by the native type. WITHOUT signature/manifest.
type SigilView struct {
	Alias        string
	Source       string
	Ref          string
	Kind         string
	Artifacts    []SigilArtifactView
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

// AuditPayload assembles the audit payload for the allow route: alias/source/ref/
// kind/sha256/allowed_by_aid, without the signature or the schema (crypto material and
// a large document; neither belongs in an audit row).
//
// Source is in the payload deliberately: it is what the approval was ON, so an audit
// trail that recorded only the alias would record only the name the operator chose for
// what they approved, not what they approved.
//
// The digests ride under `artifact_sha256` as a LIST, and the old scalar `sha256` key
// is GONE rather than reused. What was approved is a release, so the key would have had
// to change meaning from "the digest" to "one of the digests" or "all of them joined" —
// and a key that keeps its name while changing what it holds is the failure mode an
// audit trail cannot afford: every existing reader of `payload->>'sha256'` would keep
// parsing and start being wrong. A key that disappeared is a reader that breaks
// loudly.
func (r SigilAllowReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"alias":           r.View.Alias,
		"source":          r.View.Source,
		"ref":             r.View.Ref,
		"kind":            r.View.Kind,
		"artifact_sha256": auditDigests(r.View.Artifacts),
		"allowed_by_aid":  r.CallerAID,
	}
}

// auditDigests lists a release's digests for the audit row, in the order the view
// carries them — the SLOT's order, which is canonical whenever the descriptor on disk
// is (MarshalRelease guarantees that), but is not the same statement as the order the
// signature was computed over. Every one of them: recording
// a single digest would make the row a true statement about part of the approval and a
// silent omission about the rest.
func auditDigests(artifacts []SigilArtifactView) []string {
	out := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, a.SHA256)
	}
	return out
}

// artifactViewsOf projects the service's artifact rows into the flat wire shape.
func artifactViewsOf(artifacts []sharedhost.SigilArtifact) []SigilArtifactView {
	out := make([]SigilArtifactView, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, SigilArtifactView{OS: a.OS, Arch: a.Arch, Path: a.Path, SHA256: a.SHA256})
	}
	return out
}

// AllowTyped — the domain function for POST /v1/plugins/sigils (handler-native): validates
// the triple + svc.Allow + sentinel→problem. Errors are *problemError; success is
// [SigilAllowReply] (domain projection of the 201 body + audit fields).
func (h *SigilHandler) AllowTyped(ctx context.Context, claims *jwt.Claims, in SigilAllowInput) (SigilAllowReply, error) {
	var zero SigilAllowReply
	if msg, valid := validateAllowInput(in); !valid {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", msg)}
	}

	approved, err := h.svc.Allow(ctx, sigil.AllowInput{
		Alias:     in.Alias,
		Source:    in.Source,
		Ref:       in.Ref,
		CallerAID: claims.Subject,
	})
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, sigil.ErrAliasReserved), errors.Is(err, sigil.ErrSourceMismatch):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	case errors.Is(err, sigil.ErrPluginNotInCache):
		return zero, &problemError{problem.New(problem.TypePluginNotInCache, "",
			"no plugin artifact registered under alias "+in.Alias+" in the host cache")}
	// The two conflicts share a status and differ in what the operator does next.
	// A bare "already exists" would leave them guessing which of the two keys they
	// hit, and the fixes are not interchangeable.
	case errors.Is(err, sigil.ErrAliasAlreadyRegistered):
		return zero, &problemError{problem.New(problem.TypeSigilActive, "",
			"alias "+in.Alias+" is already registered by an active sigil; "+
				"revoke it (DELETE /v1/plugins/sigils/"+in.Alias+") or pick another alias")}
	case errors.Is(err, sigil.ErrSigilAlreadyActive):
		return zero, &problemError{problem.New(problem.TypeSigilActive, "",
			"the artifact at "+in.Source+" ref "+in.Ref+" is already approved under another alias; "+
				"an artifact identity carries at most one active approval, so revoke the existing grant first "+
				"(GET /v1/plugins/sigils shows which alias holds it). Renaming an alias is revoke-then-approve, "+
				"and the plugin is unapproved in between")}
	default:
		h.logger.Error("plugin.allow: service failed",
			slog.String("alias", in.Alias),
			slog.String("source", in.Source),
			slog.String("ref", in.Ref),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "allow plugin failed")}
	}

	return SigilAllowReply{
		View: SigilAllowView{
			Alias:     in.Alias,
			Source:    in.Source,
			Ref:       in.Ref,
			Kind:      approved.Kind,
			Artifacts: artifactViewsOf(approved.Artifacts),
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
			Alias:        v.Alias,
			Source:       v.Source,
			Ref:          v.Ref,
			Kind:         v.Kind,
			Artifacts:    artifactViewsOf(v.Artifacts),
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
	Alias string
}

// AuditPayload assembles the audit payload for the revoke route.
func (r SigilRevokeReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"alias": r.Alias,
	}
}

// RevokeTyped — the extracted domain function for DELETE /v1/plugins/sigils/{alias}
// (FULL-TYPED ADR-054 §Pattern (b)): validates the path segment + svc.Revoke +
// sentinel→problem. Errors are *problemError; success is [SigilRevokeReply].
//
// The alias is the whole path. It is the operator's gesture — "un-register this" — and
// it identifies exactly one live grant (plugin_sigils_active_alias_idx). The signed
// identity (source, ref) cannot serve here: a git remote is a URL, not a path segment.
func (h *SigilHandler) RevokeTyped(ctx context.Context, claims *jwt.Claims, alias string) (SigilRevokeReply, error) {
	var zero SigilRevokeReply
	if err := sigil.ValidateAlias(alias); err != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	}

	err := h.svc.Revoke(ctx, alias, claims.Subject)
	switch {
	case err == nil:
		// fall through to reply.
	case errors.Is(err, sigil.ErrSigilNotFound):
		return zero, &problemError{problem.New(problem.TypeSigilNotFound, "",
			"no active sigil for alias "+alias)}
	default:
		h.logger.Error("plugin.revoke: service failed",
			slog.String("alias", alias),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err),
		)
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "revoke plugin failed")}
	}

	return SigilRevokeReply{Alias: alias}, nil
}

// validateAllowInput checks an allow request field by field, returning
// (human-readable msg, false) at the first invalid one.
//
// The alias goes through [sigil.ValidateAlias] — the same call the service makes, so
// the transport cannot accept a name the domain would refuse, or refuse one it would
// accept. That includes the reserved list: an operator naming a plugin `core` is told
// here, in the 422, rather than by an address that silently shadows the engine.
func validateAllowInput(in SigilAllowInput) (string, bool) {
	if err := sigil.ValidateAlias(in.Alias); err != nil {
		return err.Error(), false
	}
	switch {
	case in.Source == "":
		return "field 'source' is required", false
	case len(in.Source) > maxSourceLen:
		return fmt.Sprintf("field 'source' must be at most %d characters", maxSourceLen), false
	case in.Ref == "":
		return "field 'ref' is required", false
	case !reSigilRef.MatchString(in.Ref):
		return "field 'ref' must match " + reSigilRef.String() + " (branch-refs with '/' are not supported in MVP)", false
	}
	return "", true
}
