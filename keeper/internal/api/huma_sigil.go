package api

// Registration and spec dump of the SIGIL domain (plugins/sigils) on huma full-typed (ROLLOUT-
// BATCH-2a per the role references, ADR-054 §Pattern). allow/revoke — WRITE+AUDIT (variant B,
// huma-audit-middleware; event types plugin.allowed / plugin.revoked — permission
// domain `plugin`, NOT `sigil`). list — read-bare (no audit). The domain *Typed functions
// (handlers/sigil.go) are extracted from (w,r); the old (w,r) is a thin strict wrapper (MCP
// sigil-tools call sigil.Service directly, bypassing the handler — the extraction doesn't affect them).

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

// registerHumaSigilAllow mounts POST /v1/plugins/sigils via huma (WRITE+AUDIT
// variant B — event plugin.allowed). sigilH nil → no-op. Handler: claims →
// AllowTyped → audit payload on the huma ctx → 201 typed output.
func registerHumaSigilAllow(humaAPI huma.API, sigilH *handlers.SigilHandler) {
	if sigilH == nil {
		return
	}
	huma.Register(humaAPI, sigilAllowOperation(), func(ctx context.Context, in *sigilAllowInput) (*sigilAllowOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, sigilMissingClaims()
		}
		reply, err := sigilH.AllowTyped(ctx, claims, handlers.SigilAllowInput{
			Namespace: in.Body.Namespace,
			Name:      in.Body.Name,
			Ref:       in.Body.Ref,
		})
		if err != nil {
			return nil, sigilProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &sigilAllowOutput{Status: 201, Body: newPluginSigilAllowReply(reply.View)}, nil
	})
}

// registerHumaSigilList mounts GET /v1/plugins/sigils via huma (READ-bare, no
// audit). sigilH nil → no-op. Handler: ListTyped → typed output. RBAC plugin.list —
// on the group.
func registerHumaSigilList(humaAPI huma.API, sigilH *handlers.SigilHandler) {
	if sigilH == nil {
		return
	}
	huma.Register(humaAPI, sigilListOperation(), func(ctx context.Context, _ *sigilListInput) (*sigilListOutput, error) {
		reply, err := sigilH.ListTyped(ctx)
		if err != nil {
			return nil, sigilProblem(err)
		}
		return &sigilListOutput{Body: newPluginSigilListReply(reply)}, nil
	})
}

// registerHumaSigilRevoke mounts DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}
// via huma (WRITE+AUDIT variant B — event plugin.revoked). sigilH nil → no-op.
// Handler: claims → RevokeTyped(triple) → audit payload → empty 204 output.
func registerHumaSigilRevoke(humaAPI huma.API, sigilH *handlers.SigilHandler) {
	if sigilH == nil {
		return
	}
	huma.Register(humaAPI, sigilRevokeOperation(), func(ctx context.Context, in *sigilRevokeInput) (*sigilNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, sigilMissingClaims()
		}
		reply, err := sigilH.RevokeTyped(ctx, claims, in.Namespace, in.Name, in.Ref)
		if err != nil {
			return nil, sigilProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &sigilNoContentOutput{Status: 204}, nil
	})
}

// sigilMissingClaims — a defensive response when claims are absent in ctx (unreachable:
// RequireJWT places claims before huma). problem+json (parity roleMissingClaims).
func sigilMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// sigilProblem delivers a *Typed function error via huma as problem+json.
// A domain *handlers.problemError → humaProblemError; non-problem → 500 (parity
// roleProblem).
func sigilProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaSigilAPI assembles a huma.API over a chi group with huma-audit-middleware
// (variant B) for the given event type (parity newHumaRoleAPI). Each sigil write route
// (allow/revoke) is mounted on its OWN chi group with its own event type.
func newHumaSigilAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaSigilSpecYAML assembles the OpenAPI fragment of ALL sigil routes migrated to huma
// as a YAML string, WITHOUT mounting on a real router. A hook for the rollout spec-merge target
// and the guard test. Delegates to generic [humaDumpSpec] through the same register
// functions (a single register path). Returns a 3.1.0 spec (huma default).
func HumaSigilSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.SigilSpecStub()
		registerHumaSigilAllow(api, stub)
		registerHumaSigilList(api, stub)
		registerHumaSigilRevoke(api, stub)
		return nil
	})
}
