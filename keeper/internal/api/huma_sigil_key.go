package api

// Registration and spec-dump of the SIGIL-KEY domain (/v1/sigil/keys) on huma full-typed
// (ROLLOUT BATCH 2a after the role reference, ADR-054 §Pattern). introduce/set-primary/retire —
// WRITE+AUDIT (variant B, huma-audit-middleware; events sigil.key-introduced /
// sigil.key-primary-set / sigil.key-retired). list — read-bare (no audit). Domain
// *Typed functions (handlers/sigil_key.go) extracted from (w,r); the old (w,r) — a thin
// strict wrapper (MCP sigil-key-tools call sigil.KeyService directly, bypassing the handler).

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaSigilKeyIntroduce mounts POST /v1/sigil/keys through huma (WRITE+AUDIT
// variant B — event sigil.key-introduced). sigilKeyH nil → no-op. Handler: claims →
// IntroduceTyped → audit-payload on the huma ctx → 201 typed output. Private key NOT in the response.
func registerHumaSigilKeyIntroduce(humaAPI huma.API, sigilKeyH *handlers.SigilKeyHandler) {
	if sigilKeyH == nil {
		return
	}
	huma.Register(humaAPI, sigilKeyIntroduceOperation(), func(ctx context.Context, in *sigilKeyIntroduceInput) (*sigilKeyIntroduceOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, sigilKeyMissingClaims()
		}
		makePrimary := in.Body.MakePrimary != nil && *in.Body.MakePrimary
		reply, err := sigilKeyH.IntroduceTyped(ctx, claims, makePrimary)
		if err != nil {
			return nil, sigilKeyProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		// Projection of the domain reply.View (SigilKeyIntroduceView, Status — plain string)
		// into native SigilKeyIntroduceReply (Status — native enum). Json tags match →
		// wire bytes identical to the legacy writeJSON (golden pins it).
		return &sigilKeyIntroduceOutput{Status: 201, Body: newSigilKeyIntroduceReply(reply.View)}, nil
	})
}

// registerHumaSigilKeyList mounts GET /v1/sigil/keys through huma (READ-bare, no
// audit). sigilKeyH nil → no-op. Handler: ListTyped → typed output. RBAC
// sigil.key-list — on the group.
func registerHumaSigilKeyList(humaAPI huma.API, sigilKeyH *handlers.SigilKeyHandler) {
	if sigilKeyH == nil {
		return
	}
	huma.Register(humaAPI, sigilKeyListOperation(), func(ctx context.Context, _ *sigilKeyListInput) (*sigilKeyListOutput, error) {
		reply, err := sigilKeyH.ListTyped(ctx)
		if err != nil {
			return nil, sigilKeyProblem(err)
		}
		return &sigilKeyListOutput{Body: newSigilKeyListReply(reply)}, nil
	})
}

// registerHumaSigilKeySetPrimary mounts POST /v1/sigil/keys/{key_id}/primary through
// huma (WRITE+AUDIT variant B — event sigil.key-primary-set). sigilKeyH nil → no-op.
// Handler: claims → SetPrimaryTyped(key_id) → audit-payload → empty 204 output.
func registerHumaSigilKeySetPrimary(humaAPI huma.API, sigilKeyH *handlers.SigilKeyHandler) {
	if sigilKeyH == nil {
		return
	}
	huma.Register(humaAPI, sigilKeySetPrimaryOperation(), func(ctx context.Context, in *sigilKeySetPrimaryInput) (*sigilKeyNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, sigilKeyMissingClaims()
		}
		reply, err := sigilKeyH.SetPrimaryTyped(ctx, claims, in.KeyID)
		if err != nil {
			return nil, sigilKeyProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &sigilKeyNoContentOutput{Status: 204}, nil
	})
}

// registerHumaSigilKeyRetire mounts DELETE /v1/sigil/keys/{key_id} through huma
// (WRITE+AUDIT variant B — event sigil.key-retired). sigilKeyH nil → no-op. Handler:
// claims → RetireTyped(key_id) → audit-payload → empty 204 output.
func registerHumaSigilKeyRetire(humaAPI huma.API, sigilKeyH *handlers.SigilKeyHandler) {
	if sigilKeyH == nil {
		return
	}
	huma.Register(humaAPI, sigilKeyRetireOperation(), func(ctx context.Context, in *sigilKeyRetireInput) (*sigilKeyNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, sigilKeyMissingClaims()
		}
		reply, err := sigilKeyH.RetireTyped(ctx, claims, in.KeyID)
		if err != nil {
			return nil, sigilKeyProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &sigilKeyNoContentOutput{Status: 204}, nil
	})
}

// sigilKeyMissingClaims — defensive reply when claims are missing (unreachable:
// RequireJWT sets claims before huma). problem+json (parity roleMissingClaims).
func sigilKeyMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// sigilKeyProblem delivers a *Typed-function error through huma as problem+json.
// Domain *handlers.problemError → humaProblemError; non-problem → 500 (parity
// roleProblem).
func sigilKeyProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaSigilKeyAPI builds a huma.API over a chi group with huma-audit-middleware
// (variant B) for the given event type (parity newHumaRoleAPI). Each sigil-key write
// route (introduce/set-primary/retire) is mounted on ITS OWN chi group with
// its own event type.
func newHumaSigilKeyAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaSigilKeySpecYAML assembles the OpenAPI fragment of ALL sigil-key routes migrated to huma
// as a YAML string, without mounting on a real router. Hook for the rollout spec-merge
// target and guard test. Delegates to the generic [humaDumpSpec] through the same
// register functions (single register path). Returns a 3.1.0 spec (huma default).
func HumaSigilKeySpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.SigilKeySpecStub()
		registerHumaSigilKeyIntroduce(api, stub)
		registerHumaSigilKeyList(api, stub)
		registerHumaSigilKeySetPrimary(api, stub)
		registerHumaSigilKeyRetire(api, stub)
		return nil
	})
}
