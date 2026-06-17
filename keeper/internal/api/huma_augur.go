package api

// Регистрация и spec-dump AUGUR-домена (omens + rites) на huma full-typed
// (handler-native T5d-2c, ADR-054 §Pattern). omen create/delete + rite
// create/delete — WRITE+AUDIT (вариант B, huma-audit-middleware; события
// omen.created/omen.revoked/rite.created/rite.revoked); omen list/get + rite list —
// read (БЕЗ audit). Доменные *Typed-функции (handlers/augur.go) принимают NATIVE
// request-типы и возвращают доменные result-ы с плоскими wire-полями; register-
// func проецирует их в native wire-DTO (huma_augur_reply.go) НАПРЯМУЮ — legacy-генерата не
// участвует. MCP augur-tools зовут augur.Service напрямую (мимо handler).

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

// registerHumaOmenCreate монтирует POST /v1/augur/omens через huma (WRITE+AUDIT
// вариант B — event omen.created). augurH nil → no-op. Handler: claims →
// CreateOmenTyped → audit-payload на huma-ctx (SetHumaAuditPayload) → 201 typed output.
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

// registerHumaOmenList монтирует GET /v1/augur/omens через huma (READ-with-typed-
// query, БЕЗ audit). augurH nil → no-op. Handler: typed-query → ListOmensTyped →
// typed envelope-output. RBAC omen.list — на группе.
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

// registerHumaOmenGet монтирует GET /v1/augur/omens/{name} через huma (READ-with-
// path, БЕЗ audit). augurH nil → no-op. Handler: GetOmenTyped(name) → typed output
// (404/422 через problem). RBAC omen.list (read покрыт list-правом) — на группе.
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

// registerHumaOmenDelete монтирует DELETE /v1/augur/omens/{name} через huma
// (WRITE+AUDIT вариант B — event omen.revoked). augurH nil → no-op. Handler:
// DeleteOmenTyped(name) → audit-payload → пустой 204-output.
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

// registerHumaRiteCreate монтирует POST /v1/augur/rites через huma (WRITE+AUDIT
// вариант B — event rite.created). augurH nil → no-op. Handler: claims →
// CreateRiteTyped → audit-payload → 201 typed output.
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

// registerHumaRiteList монтирует GET /v1/augur/rites?omen=<name> через huma (READ-
// with-typed-query, БЕЗ audit). augurH nil → no-op. Handler: omen-query →
// ListRitesTyped (обязательный omen, пустой/битый → 422) → typed output. RBAC
// rite.list — на группе.
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

// registerHumaRiteDelete монтирует DELETE /v1/augur/rites/{id} через huma
// (WRITE+AUDIT вариант B — event rite.revoked). augurH nil → no-op. Handler:
// DeleteRiteTyped(id) → audit-payload → пустой 204-output.
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

// === проекция доменных result-ов handler-а → native wire-DTO (handler-native:
// граница api↔handlers строит схему-тело из плоских доменных полей; OpenAPI-схему
// huma выводит из этих native-типов, генерёные типы не участвуют). ===

// newOmenView проецирует доменный handlers.OmenView (плоские поля) в native
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

// newOmenListReply проецирует доменный handlers.OmenListPage в native envelope
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

// newRiteView проецирует доменный handlers.RiteView в native RiteView (create-201 /
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

// newRiteListReply проецирует доменный handlers.RiteListResult в native тело
// RiteListReply (items non-nil [], list-by-omen без пагинации).
func newRiteListReply(res handlers.RiteListResult) RiteListReply {
	items := make([]RiteView, 0, len(res.Items))
	for _, v := range res.Items {
		items = append(items, newRiteView(v))
	}
	return RiteListReply{Items: items}
}

// augurMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity operatorMissingClaims).
func augurMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// augurProblem доставляет ошибку *Typed-функции через huma как problem+json.
// Доменный *handlers.problemError → humaProblemError; не-problem → 500 (parity
// operatorProblem).
func augurProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaAugurAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaOperatorAPI). Каждый write-
// роут augur (omen create/delete, rite create/delete) монтируется на СВОЕЙ chi-
// группе с собственным event-типом.
func newHumaAugurAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaAugurSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma augur-роутов
// как YAML-строку, БЕЗ монтирования на реальный router. Хук для спека-мерж-таргета
// тиража и guard-теста. Делегирует generic [humaDumpSpec] через те же register-
// функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
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
