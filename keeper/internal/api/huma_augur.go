package api

// Registration and spec-dump of the AUGUR domain (omens + rites) on huma full-typed
// (handler-native T5d-2c, ADR-054 §Pattern). omen create/delete + rite
// create/delete — WRITE+AUDIT (variant B, huma-audit-middleware; events
// omen.created/omen.revoked/rite.created/rite.revoked); omen list/get + rite list —
// read (no audit). The domain *Typed functions (handlers/augur.go) take NATIVE
// request types and return domain results with flat wire fields; the register
// funcs project them into native wire-DTO (huma_augur_reply.go) DIRECTLY — the legacy generator is
// not involved. MCP augur-tools call augur.Service directly (bypassing the handler).

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

// registerHumaOmenCreate mounts POST /v1/augur/omens via huma (WRITE+AUDIT
// variant B — event omen.created). augurH nil → no-op. Handler: claims →
// CreateOmenTyped → audit payload on the huma ctx (SetHumaAuditPayload) → 201 typed output.
func registerHumaOmenCreate(humaAPI huma.API, augurH *handlers.AugurHandler) {
	if augurH == nil {
		return
	}
	huma.Register(humaAPI, omenCreateOperation(), func(ctx context.Context, in *omenCreateInput) (*omenCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, augurMissingClaims()
		}
		reply, err := augurH.CreateOmenTyped(ctx, claims, handlers.OmenCreateInput{
			Name:       in.Body.Name,
			SourceType: in.Body.SourceType,
			Endpoint:   in.Body.Endpoint,
			AuthRef:    in.Body.AuthRef,
		})
		if err != nil {
			return nil, augurProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &omenCreateOutput{Status: 201, Body: newOmenView(reply.View)}, nil
	})
}

// registerHumaOmenList mounts GET /v1/augur/omens via huma (READ with typed
// query, no audit). augurH nil → no-op. Handler: typed-query → ListOmensTyped →
// typed envelope output. RBAC omen.list — on the group.
func registerHumaOmenList(humaAPI huma.API, augurH *handlers.AugurHandler) {
	if augurH == nil {
		return
	}
	huma.Register(humaAPI, omenListOperation(), func(ctx context.Context, in *omenListInput) (*omenListOutput, error) {
		page, err := augurH.ListOmensTyped(ctx, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, augurProblem(err)
		}
		return &omenListOutput{Body: newOmenListReply(page)}, nil
	})
}

// registerHumaOmenGet mounts GET /v1/augur/omens/{name} via huma (READ with
// path, no audit). augurH nil → no-op. Handler: GetOmenTyped(name) → typed output
// (404/422 via problem). RBAC omen.list (read is covered by the list permission) — on the group.
func registerHumaOmenGet(humaAPI huma.API, augurH *handlers.AugurHandler) {
	if augurH == nil {
		return
	}
	huma.Register(humaAPI, omenGetOperation(), func(ctx context.Context, in *omenGetInput) (*omenGetOutput, error) {
		view, err := augurH.GetOmenTyped(ctx, in.Name)
		if err != nil {
			return nil, augurProblem(err)
		}
		return &omenGetOutput{Body: newOmenView(view)}, nil
	})
}

// registerHumaOmenDelete mounts DELETE /v1/augur/omens/{name} via huma
// (WRITE+AUDIT variant B — event omen.revoked). augurH nil → no-op. Handler:
// DeleteOmenTyped(name) → audit payload → empty 204 output.
func registerHumaOmenDelete(humaAPI huma.API, augurH *handlers.AugurHandler) {
	if augurH == nil {
		return
	}
	huma.Register(humaAPI, omenDeleteOperation(), func(ctx context.Context, in *omenDeleteInput) (*augurNoContentOutput, error) {
		reply, err := augurH.DeleteOmenTyped(ctx, in.Name)
		if err != nil {
			return nil, augurProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &augurNoContentOutput{Status: 204}, nil
	})
}

// registerHumaRiteCreate mounts POST /v1/augur/rites via huma (WRITE+AUDIT
// variant B — event rite.created). augurH nil → no-op. Handler: claims →
// CreateRiteTyped → audit payload → 201 typed output.
func registerHumaRiteCreate(humaAPI huma.API, augurH *handlers.AugurHandler) {
	if augurH == nil {
		return
	}
	huma.Register(humaAPI, riteCreateOperation(), func(ctx context.Context, in *riteCreateInput) (*riteCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, augurMissingClaims()
		}
		reply, err := augurH.CreateRiteTyped(ctx, claims, handlers.RiteCreateInput{
			Omen:         in.Body.Omen,
			Coven:        in.Body.Coven,
			SID:          in.Body.SID,
			Allow:        in.Body.Allow,
			Delegate:     in.Body.Delegate,
			TokenTTL:     in.Body.TokenTTL,
			TokenNumUses: in.Body.TokenNumUses,
		})
		if err != nil {
			return nil, augurProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &riteCreateOutput{Status: 201, Body: newRiteView(reply.View)}, nil
	})
}

// registerHumaRiteList mounts GET /v1/augur/rites?omen=<name> via huma (READ
// with typed query, no audit). augurH nil → no-op. Handler: omen-query →
// ListRitesTyped (omen required, empty/malformed → 422) → typed output. RBAC
// rite.list — on the group.
func registerHumaRiteList(humaAPI huma.API, augurH *handlers.AugurHandler) {
	if augurH == nil {
		return
	}
	huma.Register(humaAPI, riteListOperation(), func(ctx context.Context, in *riteListInput) (*riteListOutput, error) {
		res, err := augurH.ListRitesTyped(ctx, in.Omen)
		if err != nil {
			return nil, augurProblem(err)
		}
		return &riteListOutput{Body: newRiteListReply(res)}, nil
	})
}

// registerHumaRiteDelete mounts DELETE /v1/augur/rites/{id} via huma
// (WRITE+AUDIT variant B — event rite.revoked). augurH nil → no-op. Handler:
// DeleteRiteTyped(id) → audit payload → empty 204 output.
func registerHumaRiteDelete(humaAPI huma.API, augurH *handlers.AugurHandler) {
	if augurH == nil {
		return
	}
	huma.Register(humaAPI, riteDeleteOperation(), func(ctx context.Context, in *riteDeleteInput) (*augurNoContentOutput, error) {
		reply, err := augurH.DeleteRiteTyped(ctx, in.ID)
		if err != nil {
			return nil, augurProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &augurNoContentOutput{Status: 204}, nil
	})
}

// === projection of the handler's domain results → native wire-DTO (handler-native:
// the api↔handlers boundary builds the schema body from flat domain fields; huma derives
// the OpenAPI schema from these native types, generated types are not involved). ===

// newOmenView projects the domain handlers.OmenView (flat fields) into the native
// OmenView (create-201 / get-200 / element list). source_type — native enum
// OmenViewSourceType (inline string-enum); created_by_aid — *string omitempty.
func newOmenView(v handlers.OmenView) OmenView {
	return OmenView{
		AuthRef:      v.AuthRef,
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Endpoint:     v.Endpoint,
		Name:         v.Name,
		SourceType:   OmenViewSourceType(v.SourceType),
	}
}

// newOmenListReply projects the domain handlers.OmenListPage into the native envelope
// OmenListReply (items non-nil [], offset/limit/total int32).
func newOmenListReply(p handlers.OmenListPage) OmenListReply {
	items := make([]OmenView, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newOmenView(v))
	}
	return OmenListReply{
		Items:  items,
		Offset: int32(p.Offset),
		Limit:  int32(p.Limit),
		Total:  int32(p.Total),
	}
}

// newRiteView projects the domain handlers.RiteView into the native RiteView (create-201 /
// element list). allow — byte-passthrough JSONB (as-is); coven/sid/token_*/
// created_by_aid — *-optional omitempty.
func newRiteView(v handlers.RiteView) RiteView {
	return RiteView{
		Allow:        v.Allow,
		Coven:        v.Coven,
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Delegate:     v.Delegate,
		ID:           v.ID,
		Omen:         v.Omen,
		SID:          v.SID,
		TokenNumUses: v.TokenNumUses,
		TokenTTL:     v.TokenTTL,
	}
}

// newRiteListReply projects the domain handlers.RiteListResult into the native body
// RiteListReply (items non-nil [], list-by-omen without pagination).
func newRiteListReply(res handlers.RiteListResult) RiteListReply {
	items := make([]RiteView, 0, len(res.Items))
	for _, v := range res.Items {
		items = append(items, newRiteView(v))
	}
	return RiteListReply{Items: items}
}

// augurMissingClaims — a defensive response when claims are absent from ctx (unreachable:
// RequireJWT puts claims in before huma). problem+json (parity operatorMissingClaims).
func augurMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// augurProblem delivers a *Typed-function error through huma as problem+json.
// A domain *handlers.problemError → humaProblemError; non-problem → 500 (parity
// operatorProblem).
func augurProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaAugurAPI builds a huma.API over the chi group with huma-audit-middleware
// (variant B) for the given event type (parity newHumaOperatorAPI). Each write
// route of augur (omen create/delete, rite create/delete) is mounted on its OWN chi
// group with its own event type.
func newHumaAugurAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaAugurSpecYAML assembles the OpenAPI fragment of ALL migrated-to-huma augur routes
// as a YAML string, without mounting on a real router. A hook for the rollout spec-merge
// target and the guard test. Delegates to generic [humaDumpSpec] via the same register
// functions (single register path). Returns a 3.1.0 spec (huma default).
func HumaAugurSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.AugurSpecStub()
		registerHumaOmenCreate(api, stub)
		registerHumaOmenList(api, stub)
		registerHumaOmenGet(api, stub)
		registerHumaOmenDelete(api, stub)
		registerHumaRiteCreate(api, stub)
		registerHumaRiteList(api, stub)
		registerHumaRiteDelete(api, stub)
		return nil
	})
}
