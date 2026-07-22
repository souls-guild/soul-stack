// Operator API handlers for rotating Sigil trust-anchor signing keys (ADR-026(h),
// R3-S7) — a thin HTTP wrapper over [sigil.KeyService]. The same service is called
// by the MCP tool handler (keeper.sigil.key.*), a single source of truth.
//
// Business logic (key-gen, Vault write, registry CRUD, publish anchors-changed)
// lives in [sigil.KeyService]; the handler decodes the request → service call →
// maps sentinels to RFC 7807 and encodes 2xx. RBAC — in middleware (router.go).
//
// SECURITY: the private key is NEVER in the response (KeyService does not return it)
// and never in the logs (the handler logs only key_id / by_aid).
//
// T5d (handler-native): the sigil-key domain is decoupled from the legacy
// generator. The *Typed functions return domain results with FLAT wire fields —
// package api builds the native wire-DTO (OpenAPI schema) from those fields. The
// (w,r) wrappers are gone; HTTP is served by huma full-typed
// (api/huma_sigil_key.go), MCP calls sigil.KeyService directly (bypassing the
// handler).
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

// reSigilKeyID — the key_id format: exactly 64 lowercase hex characters
// (SHA-256(SPKI), hex). Matches the convention of migration 037 /
// [sigil.keyIDFromPublic]. A path segment with no slashes/`..` — safe from
// traversal.
var reSigilKeyID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SigilKeyHandler — the four signing-key rotation endpoints (introduce / list /
// set-primary / retire). Delegates to [sigil.KeyService].
type SigilKeyHandler struct {
	svc    *sigil.KeyService
	logger *slog.Logger
}

// NewSigilKeyHandler creates the handler. svc is required (panic on nil — the only
// misconfiguration point; the caller must pass non-nil).
func NewSigilKeyHandler(svc *sigil.KeyService, logger *slog.Logger) *SigilKeyHandler {
	if svc == nil {
		panic("handlers.NewSigilKeyHandler: sigil.KeyService is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &SigilKeyHandler{svc: svc, logger: logger}
}

// SigilKeySpecStub — a non-nil *SigilKeyHandler stub for generating the huma
// OpenAPI fragment (HumaSigilKeySpecYAML): on dump the domain handler is not
// called, but huma.Register requires non-nil for its nil no-op check. svc nil — the
// handler never executes in spec mode (parity with [RoleSpecStub]).
func SigilKeySpecStub() *SigilKeyHandler {
	return &SigilKeyHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// SigilKeyIntroduceView — a FLAT domain projection of the 201 body of POST
// /v1/sigil/keys (handler-native T5d). Package api projects it into the native
// SigilKeyIntroduceReply schema. Status — a plain domain string (active/retired);
// the native type in api holds the enum form. No private key (KeyService does not
// return it).
type SigilKeyIntroduceView struct {
	KeyID        string
	PubkeyPEM    string
	IsPrimary    bool
	Status       string
	IntroducedAt time.Time
}

// SigilKeyView — a FLAT domain projection of an active key (element
// SigilKeyListPage.Items), handler-native T5d. Package api projects it into the
// native SigilKeyView schema. No vault_ref; Status — a plain domain string;
// IntroducedAt already truncated to seconds (parity with the legacy wire).
type SigilKeyView struct {
	KeyID        string
	IsPrimary    bool
	Status       string
	IntroducedAt time.Time
}

// SigilKeyListPage — the domain result of GET /v1/sigil/keys (handler-native T5d).
// Package api projects Items → native SigilKeyListReply (items non-nil → `[]`).
type SigilKeyListPage struct {
	Items []SigilKeyView
}

// SigilKeyIntroduceReply — the result of [SigilKeyHandler.IntroduceTyped]
// (handler-native). Carries the domain projection of the 201 body
// (SigilKeyIntroduceView, no private key) + the caller AID.
type SigilKeyIntroduceReply struct {
	View      SigilKeyIntroduceView
	CallerAID string
}

// AuditPayload assembles the audit payload of the introduce route (parity with
// legacy: key_id + is_primary + introduced_by_aid; no private key).
func (r SigilKeyIntroduceReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"key_id":            r.View.KeyID,
		"is_primary":        r.View.IsPrimary,
		"introduced_by_aid": r.CallerAID,
	}
}

// IntroduceTyped — the domain function of POST /v1/sigil/keys (handler-native):
// svc.Introduce (key-gen + Vault write + register) + sentinel→problem. Errors —
// *problemError; success — [SigilKeyIntroduceReply] (the domain projection of the
// 201 body + audit fields). SECURITY: the private key never leaves KeyService.
func (h *SigilKeyHandler) IntroduceTyped(ctx context.Context, claims *jwt.Claims, makePrimary bool) (SigilKeyIntroduceReply, error) {
	var zero SigilKeyIntroduceReply
	res, err := h.svc.Introduce(ctx, makePrimary, claims.Subject)
	switch {
	case err == nil:
	case errors.Is(err, sigil.ErrConcurrentPrimary):
		return zero, &problemError{problem.New(problem.TypeSigilKeyConcurrentChange, "",
			"concurrent primary-key change; retry")}
	default:
		// SECURITY: the error may contain wrapped Vault/PG details — to the log
		// (not the response), and with no private key (KeyService does not put it in
		// err).
		h.logger.Error("sigil.key.introduce: service failed",
			slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "introduce signing key failed")}
	}

	return SigilKeyIntroduceReply{
		View: SigilKeyIntroduceView{
			KeyID:        res.KeyID,
			PubkeyPEM:    res.PubkeyPEM,
			IsPrimary:    res.IsPrimary,
			Status:       res.Status,
			IntroducedAt: res.IntroducedAt,
		},
		CallerAID: claims.Subject,
	}, nil
}

// ListTyped — the domain function of GET /v1/sigil/keys (handler-native, READ
// without audit): active keys (primary first) → [SigilKeyListPage] (items
// non-nil). A read error → *problemError (500). vault_ref omitted; introduced_at →
// UTC+Truncate(Second).
func (h *SigilKeyHandler) ListTyped(ctx context.Context) (SigilKeyListPage, error) {
	keys, err := h.svc.List(ctx)
	if err != nil {
		h.logger.Error("sigil.key.list: service failed", slog.Any("error", err))
		return SigilKeyListPage{}, &problemError{problem.New(problem.TypeInternalError, "", "list signing keys failed")}
	}
	items := make([]SigilKeyView, 0, len(keys))
	for _, k := range keys {
		items = append(items, SigilKeyView{
			KeyID:        k.KeyID,
			IsPrimary:    k.IsPrimary,
			Status:       k.Status,
			IntroducedAt: k.IntroducedAt.UTC().Truncate(time.Second),
		})
	}
	return SigilKeyListPage{Items: items}, nil
}

// SigilKeySetPrimaryReply — the extracted result of [SigilKeyHandler.SetPrimaryTyped]
// (FULL-TYPED). Carries the audit fields (the HTTP response is an empty 204 body).
type SigilKeySetPrimaryReply struct {
	KeyID     string
	CallerAID string
}

// AuditPayload assembles the audit payload of the set-primary route (parity with
// legacy: key_id + set_by_aid). Shared by (w,r) and huma-B.
func (r SigilKeySetPrimaryReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"key_id":     r.KeyID,
		"set_by_aid": r.CallerAID,
	}
}

// SetPrimaryTyped — the extracted domain function of POST
// /v1/sigil/keys/{key_id}/primary (FULL-TYPED ADR-054 §Pattern (b)): key_id
// validation + svc.SetPrimary + sentinel→problem. Errors — *problemError; success —
// [SigilKeySetPrimaryReply].
func (h *SigilKeyHandler) SetPrimaryTyped(ctx context.Context, claims *jwt.Claims, keyID string) (SigilKeySetPrimaryReply, error) {
	var zero SigilKeySetPrimaryReply
	if !reSigilKeyID.MatchString(keyID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'key_id' must match "+reSigilKeyID.String())}
	}

	err := h.svc.SetPrimary(ctx, keyID, claims.Subject)
	switch {
	case err == nil:
	case errors.Is(err, sigil.ErrKeyNotFound):
		return zero, &problemError{problem.New(problem.TypeSigilKeyNotFound, "",
			"no signing key with key_id="+keyID)}
	case errors.Is(err, sigil.ErrKeyRetired):
		return zero, &problemError{problem.New(problem.TypeSigilKeyConcurrentChange, "",
			"signing key "+keyID+" is retired; cannot become primary")}
	case errors.Is(err, sigil.ErrConcurrentPrimary):
		return zero, &problemError{problem.New(problem.TypeSigilKeyConcurrentChange, "",
			"concurrent primary-key change; retry")}
	default:
		h.logger.Error("sigil.key.set-primary: service failed",
			slog.String("key_id", keyID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "set primary signing key failed")}
	}

	return SigilKeySetPrimaryReply{KeyID: keyID, CallerAID: claims.Subject}, nil
}

// SigilKeyRetireReply — the extracted result of [SigilKeyHandler.RetireTyped]
// (FULL-TYPED). Carries the audit fields (the HTTP response is an empty 204 body).
type SigilKeyRetireReply struct {
	KeyID     string
	CallerAID string
}

// AuditPayload assembles the audit payload of the retire route (parity with legacy:
// key_id + retired_by_aid). Shared by (w,r) and huma-B.
func (r SigilKeyRetireReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"key_id":         r.KeyID,
		"retired_by_aid": r.CallerAID,
	}
}

// RetireTyped — the extracted domain function of DELETE /v1/sigil/keys/{key_id}
// (FULL-TYPED ADR-054 §Pattern (b)): key_id validation + svc.Retire + sentinel→
// problem (last-active/primary → 409). Errors — *problemError; success —
// [SigilKeyRetireReply].
func (h *SigilKeyHandler) RetireTyped(ctx context.Context, claims *jwt.Claims, keyID string) (SigilKeyRetireReply, error) {
	var zero SigilKeyRetireReply
	if !reSigilKeyID.MatchString(keyID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path param 'key_id' must match "+reSigilKeyID.String())}
	}

	err := h.svc.Retire(ctx, keyID, claims.Subject)
	switch {
	case err == nil:
	case errors.Is(err, sigil.ErrKeyNotFound):
		return zero, &problemError{problem.New(problem.TypeSigilKeyNotFound, "",
			"no active signing key with key_id="+keyID)}
	case errors.Is(err, sigil.ErrLastActiveKey):
		return zero, &problemError{problem.New(problem.TypeSigilKeyLastActive, "",
			"cannot retire the last active signing key")}
	case errors.Is(err, sigil.ErrRetirePrimary):
		return zero, &problemError{problem.New(problem.TypeSigilKeyPrimary, "",
			"cannot retire the primary key; set another key primary first")}
	default:
		h.logger.Error("sigil.key.retire: service failed",
			slog.String("key_id", keyID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "retire signing key failed")}
	}

	return SigilKeyRetireReply{KeyID: keyID, CallerAID: claims.Subject}, nil
}
