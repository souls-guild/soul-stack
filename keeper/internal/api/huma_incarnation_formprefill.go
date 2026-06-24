package api

// FULL-TYPED форма INCARNATION form-prefill-роута (code-first источник OpenAPI,
// ADR-054 §Pattern). POST /v1/incarnations/{name}/scenarios/{scenario}/form-prefill
// — day-2 pre-fill UI-формы сценария текущими значениями incarnation.state
// (docs/input.md → «Pre-fill из state»). Резолв (не мутация), тела НЕТ: audit НЕ
// навешан. RBAC incarnation.get + scope-предикат (ADR-047) — на группе.
// Go-типы — единственный источник правды схемы.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// incFormPrefillInput — huma-input POST .../scenarios/{scenario}/form-prefill.
// Name/Scenario — path; тела НЕТ. Клиент НЕ задаёт ни state-путь (path-whitelist
// строится из схемы сценария на backend), ни версию сервиса: схема всегда берётся
// по inc.ServiceVersion (анти version-craft инвариант, см. FormPrefillTyped).
type incFormPrefillInput struct {
	Name     string `path:"name" doc:"имя инкарнации"`
	Scenario string `path:"scenario" doc:"имя сценария"`
}

// IncarnationFormPrefillReply — native 200-тело POST .../form-prefill. Values —
// карта `field → current-value` (только prefill-объявленные не-secret поля с
// покрытым state-путём; остальные опущены). Имя структуры = контрактное имя схемы.
type IncarnationFormPrefillReply struct {
	Values map[string]any `json:"values" doc:"field → текущее значение из incarnation.state (prefill-hint)"`
}

// incFormPrefillOutput — huma-output POST .../form-prefill (FULL-TYPED). Body —
// native 200-тело (IncarnationFormPrefillReply: {values}).
type incFormPrefillOutput struct {
	Body IncarnationFormPrefillReply
}

// incFormPrefillOperation — метаданные POST .../scenarios/{scenario}/form-prefill.
// DefaultStatus=200. READ-роут (резолв, не мутация, тела нет): audit НЕ навешан.
// Permission incarnation.get. Errors: 403 RBAC, 404 нет инкарнации/вне scope,
// 422 невалидный name/scenario, 500.
func incFormPrefillOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "incarnationFormPrefill",
		Method:        http.MethodPost,
		Path:          "/{name}/scenarios/{scenario}/form-prefill",
		Summary:       "Pre-fill day-2-формы сценария из incarnation.state",
		Description:   "Текущие значения state под поля схемы сценария с prefill_from_state (docs/input.md). Path-whitelist (клиент путь не задаёт), secret-поля исключены. Вне RBAC-scope → 404. Permission incarnation.get. Read-only, без audit.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaIncarnationFormPrefill монтирует POST .../scenarios/{scenario}/
// form-prefill через huma (READ, без тела, БЕЗ audit). scope-предикат (ADR-047,
// action=get) → вне scope 404. incH nil → no-op.
func registerHumaIncarnationFormPrefill(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incFormPrefillOperation(), func(ctx context.Context, in *incFormPrefillInput) (*incFormPrefillOutput, error) {
		claims, _ := apimiddleware.ClaimsFromContext(ctx)
		res, err := incH.FormPrefillTyped(ctx, in.Name, in.Scenario, incH.GetInScopeFor(claims, "get"))
		if err != nil {
			return nil, incProblem(err)
		}
		return &incFormPrefillOutput{Body: IncarnationFormPrefillReply{Values: coalesceFormPrefillValues(res.Values)}}, nil
	})
}

// coalesceFormPrefillValues гарантирует non-nil карту в wire (`values: {}` вместо
// `null` при отсутствии prefill-полей) — стабильный контракт для UI-формы.
func coalesceFormPrefillValues(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}
