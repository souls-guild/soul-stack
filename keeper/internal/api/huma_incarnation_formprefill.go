package api

// FULL-TYPED форма INCARNATION form-prefill-роута (code-first источник OpenAPI,
// ADR-054 §Pattern). POST /v1/incarnations/{name}/scenarios/{scenario}/form-prefill
// — day-2 pre-fill UI-формы сценария текущими значениями incarnation.state
// (docs/input.md → «Pre-fill из state»). READ-with-body (резолв, не мутация):
// audit НЕ навешан. RBAC incarnation.get + scope-предикат (ADR-047) — на группе.
// Go-типы — единственный источник правды схемы.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// incFormPrefillInput — huma-input POST .../scenarios/{scenario}/form-prefill.
// Name/Scenario — path; Body — ПОИНТЕР (опц. тело: единственное поле ref). Клиент
// state-путь НЕ передаёт (path-whitelist строится из схемы сценария на backend).
type incFormPrefillInput struct {
	Name     string                      `path:"name" doc:"имя инкарнации"`
	Scenario string                      `path:"scenario" doc:"имя сценария"`
	Body     *IncarnationFormPrefillRequest `doc:"опц. тело: версия сервиса (ref)"`
}

// IncarnationFormPrefillRequest — Go-форма тела POST .../form-prefill (code-first
// источник схемы И валидации). Единственное поле — опц. `ref` (версия сервиса:
// схема той же версии, что строила форму; пусто → ServiceVersion инкарнации).
// additionalProperties:false (huma-дефолт) → unknown поле → 400. Имя структуры =
// контрактное имя схемы (huma DefaultSchemaNamer).
type IncarnationFormPrefillRequest struct {
	Ref string `json:"ref,omitempty" doc:"опц. git-ref версии сервиса (схема той же версии, что форма); пусто → версия инкарнации"`
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
// DefaultStatus=200. READ-роут (резолв, не мутация): audit НЕ навешан. Permission
// incarnation.get. Errors: 400 unknown/malformed, 403 RBAC, 404 нет инкарнации/вне
// scope, 422 невалидный name/scenario, 500.
func incFormPrefillOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "incarnationFormPrefill",
		Method:        http.MethodPost,
		Path:          "/{name}/scenarios/{scenario}/form-prefill",
		Summary:       "Pre-fill day-2-формы сценария из incarnation.state",
		Description:   "Текущие значения state под поля схемы сценария с prefill_from_state (docs/input.md). Path-whitelist (клиент путь не задаёт), secret-поля исключены. Вне RBAC-scope → 404. Permission incarnation.get. Read-only, без audit.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// registerHumaIncarnationFormPrefill монтирует POST .../scenarios/{scenario}/
// form-prefill через huma (READ-with-body, БЕЗ audit). scope-предикат (ADR-047,
// action=get) → вне scope 404. incH nil → no-op.
func registerHumaIncarnationFormPrefill(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incFormPrefillOperation(), func(ctx context.Context, in *incFormPrefillInput) (*incFormPrefillOutput, error) {
		claims, _ := apimiddleware.ClaimsFromContext(ctx)
		ref := ""
		if in.Body != nil {
			ref = in.Body.Ref
		}
		res, err := incH.FormPrefillTyped(ctx, in.Name, in.Scenario, ref, incH.GetInScopeFor(claims, "get"))
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
