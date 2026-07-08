package api

// Регистрация и spec-dump OPERATOR-домена на huma full-typed (ТИРАЖ-БАТЧ-2a по 5
// эталонам, ADR-054 §Pattern). create/revoke/issue-token — WRITE+AUDIT (вариант B,
// huma-audit-middleware; full-typed huma САМ пишет ответ, StatusRecorder неприменим —
// audit держит hctx.Status() + carrier-payload, иначе рецидив S6). list — read-with-
// typed-query (БЕЗ audit). get — read-with-path (БЕЗ audit). Доменные *Typed-функции
// (handlers/operator.go) извлечены из (w,r); старый (w,r) — тонкая strict-оболочка
// (MCP operator-tools зовут operator.Service напрямую, мимо handler — извлечение их
// не затрагивает).

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaOperatorCreate монтирует POST /v1/operators через huma (WRITE+AUDIT
// вариант B — event operator.created). opH nil → no-op (паттерн opt-in-домена).
// Handler: claims → CreateTyped → audit-payload на huma-ctx (SetHumaAuditPayload) →
// 201 typed output. Доменные problem-ошибки — через humaProblemError.
func registerHumaOperatorCreate(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, operatorCreateOperation(), func(ctx context.Context, in *operatorCreateInput) (*operatorCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		reply, err := opH.CreateTyped(ctx, claims, handlers.OperatorCreateInput{
			AID:         in.Body.AID,
			DisplayName: in.Body.DisplayName,
			Roles:       in.Body.Roles,
		})
		if err != nil {
			return nil, operatorProblem(err)
		}
		// Audit-payload на huma-ctx — ЕДИНЫЙ источник reply.AuditPayload() (тот же
		// метод, что и у revoke/issue-token).
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &operatorCreateOutput{Status: 201, Body: newOperatorCreateReply(reply)}, nil
	})
}

// registerHumaOperatorList монтирует GET /v1/operators через huma (READ-with-typed-
// query, БЕЗ audit). opH nil → no-op. Handler: typed-query → конверт в доменный
// фильтр → ListTyped → typed envelope-output. RBAC operator.list — на группе.
func registerHumaOperatorList(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, operatorListOperation(), func(ctx context.Context, in *operatorListInput) (*operatorListOutput, error) {
		filter := operator.ListFilter{
			AuthMethod:     operator.AuthMethod(in.AuthMethod), // пустой → фильтр не применяется; enum huma уже отбил вне набора (422)
			IncludeRevoked: in.Revoked,
			Q:              in.Q, // pass-through: пусто → без фильтра (parity /v1/runs)
		}
		page, err := opH.ListTyped(ctx, filter, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, operatorProblem(err)
		}
		return &operatorListOutput{Body: newOperatorListBody(page)}, nil
	})
}

// registerHumaOperatorGet монтирует GET /v1/operators/{aid} через huma (READ-with-
// path, БЕЗ audit). opH nil → no-op. Handler: GetTyped(aid) → typed output (404/422
// через problem). RBAC operator.list (read покрыт list-правом) — на группе.
func registerHumaOperatorGet(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, operatorGetOperation(), func(ctx context.Context, in *operatorGetInput) (*operatorGetOutput, error) {
		view, err := opH.GetTyped(ctx, in.AID)
		if err != nil {
			return nil, operatorProblem(err)
		}
		return &operatorGetOutput{Body: newOperator(view)}, nil
	})
}

// registerHumaOperatorRevoke монтирует POST /v1/operators/{aid}/revoke через huma
// (WRITE+AUDIT вариант B — event operator.revoked). opH nil → no-op. Handler:
// claims → RevokeTyped(aid, reason) → audit-payload → пустой 204-output.
func registerHumaOperatorRevoke(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, operatorRevokeOperation(), func(ctx context.Context, in *operatorRevokeInput) (*operatorNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		reply, err := opH.RevokeTyped(ctx, claims, in.AID, in.Body.Reason)
		if err != nil {
			return nil, operatorProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &operatorNoContentOutput{Status: 204}, nil
	})
}

// registerHumaOperatorIssueToken монтирует POST /v1/operators/{aid}/issue-token через
// huma (WRITE+AUDIT вариант B — event operator.token-issued). opH nil → no-op.
// Handler: claims → IssueTokenTyped(aid) → audit-payload → 200 С ТЕЛОМ (jwt; отличие
// от 204-write-роутов — issue-token возвращает выпущенный токен).
func registerHumaOperatorIssueToken(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, operatorIssueTokenOperation(), func(ctx context.Context, in *operatorIssueTokenInput) (*operatorIssueTokenOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		reply, err := opH.IssueTokenTyped(ctx, claims, in.AID)
		if err != nil {
			return nil, operatorProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &operatorIssueTokenOutput{Status: 200, Body: newIssueTokenReply(reply)}, nil
	})
}

// === проекция доменных result-ов handler-а → native wire-DTO (handler-native
// PILOT: граница api↔handlers строит схему-тело из плоских доменных полей; OpenAPI-
// схему huma выводит из этих native-типов, генерёные типы не участвуют). ===

// newOperatorCreateReply проецирует доменный handlers.OperatorCreateReply (плоские
// поля) в native 201-тело. roles — `*[]string` С omitempty: пустой granted-набор →
// nil (поле опущено, backward-compat запрос-без-roles).
func newOperatorCreateReply(r handlers.OperatorCreateReply) OperatorCreateReply {
	out := OperatorCreateReply{
		AID:          r.AID,
		CreatedAt:    r.CreatedAt,
		CreatedByAID: r.CreatedByAID,
		DisplayName:  r.DisplayName,
		JWT:          r.JWT,
	}
	if len(r.GrantedRoles) > 0 {
		roles := r.GrantedRoles
		out.Roles = &roles
	}
	return out
}

// newIssueTokenReply проецирует доменный handlers.OperatorIssueTokenReply в native
// 200-тело issue-token.
func newIssueTokenReply(r handlers.OperatorIssueTokenReply) IssueTokenReply {
	return IssueTokenReply{AID: r.AID, ExpiresAt: r.ExpiresAt, JWT: r.JWT}
}

// newOperator проецирует доменный handlers.OperatorView в native Operator (get-200 /
// list-element). auth_method — native enum OperatorAuthMethod; metadata — `*map`
// С omitempty (пустая → nil, ключ опущен).
func newOperator(v handlers.OperatorView) Operator {
	out := Operator{
		AID:              v.AID,
		AuthMethod:       OperatorAuthMethod(v.AuthMethod),
		BootstrapInitial: v.BootstrapInitial,
		CreatedAt:        v.CreatedAt,
		CreatedByAID:     v.CreatedByAID,
		CreatedVia:       v.CreatedVia,
		DisplayName:      v.DisplayName,
		RevokedAt:        v.RevokedAt,
	}
	if len(v.Metadata) > 0 {
		m := map[string]interface{}(v.Metadata)
		out.Metadata = &m
	}
	return out
}

// newOperatorListBody проецирует доменный handlers.OperatorListPage в native
// list-envelope PagedResponse[Operator] (items non-nil [], offset/limit/total).
func newOperatorListBody(p handlers.OperatorListPage) sharedapi.PagedResponse[Operator] {
	items := make([]Operator, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newOperator(v))
	}
	return sharedapi.PagedResponse[Operator]{
		Items:  items,
		Offset: p.Offset,
		Limit:  p.Limit,
		Total:  p.Total,
	}
}

// operatorMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity roleMissingClaims).
func operatorMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// operatorProblem доставляет ошибку *Typed-функции через huma как problem+json.
// Доменный *handlers.problemError → humaProblemError; не-problem → 500 (parity
// roleProblem / auditProblem).
func operatorProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaOperatorAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaRoleAPI). Каждый write-роут
// operator (create/revoke/issue-token) монтируется на СВОЕЙ chi-группе с собственным
// event-типом.
func newHumaOperatorAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaOperatorSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma operator-
// роутов как YAML-строку, БЕЗ монтирования на реальный router. Хук для спека-мерж-
// таргета тиража и guard-теста. Делегирует generic [humaDumpSpec] через те же
// register-функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaOperatorSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.OperatorSpecStub()
		registerHumaOperatorCreate(api, stub)
		registerHumaOperatorList(api, stub)
		registerHumaOperatorGet(api, stub)
		registerHumaOperatorRevoke(api, stub)
		registerHumaOperatorIssueToken(api, stub)
		return nil
	})
}
