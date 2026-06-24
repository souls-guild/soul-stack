package api

// FULL-TYPED форма OPERATOR-домена (code-first источник OpenAPI, ADR-054 §Pattern).
// ТИРАЖ-БАТЧ-2a (operator целиком на huma по 5 эталонам): create (write-middleware-
// audit), list (read-with-typed-query), get (read-with-path), revoke + issue-token
// (write-middleware-audit). Go-типы — единственный источник правды: huma строит из
// них И JSON Schema OpenAPI-фрагмента, И валидацию/typed-bind входа, И typed-output.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// === POST /v1/operators (create) — WRITE+AUDIT operator.created ===

// operatorCreateInput — huma-input POST /v1/operators (FULL-TYPED). Body —
// типизированное тело: huma декодит и валидирует по схеме из huma-тегов
// OperatorCreateRequest. Конверт в доменную модель — в registerHumaOperatorCreate.
type operatorCreateInput struct {
	Body OperatorCreateRequest
}

// OperatorCreateRequest — native Go-форма тела POST /v1/operators (code-first
// источник схемы И валидации, handler-native PILOT). AID нового Архонта + опц.
// display_name + опц. roles[] для atomic create+grant. Register-func проецирует в
// доменный handlers.OperatorCreateInput.
//
// huma-теги: `required:"true"` — обязательное (missing → 422); display_name/roles —
// опциональные. additionalProperties:false (huma-дефолт) → unknown-поле → 400
// (error-override). Формат AID (operator.ValidAID) и существование ролей — доменная
// валидация в CreateTyped (422/404), не huma-схема. Имя структуры = контрактное имя
// схемы в OpenAPI (huma DefaultSchemaNamer берёт reflect.Type.Name()) — выровнено под
// committed-рукопись (docs/keeper/openapi.yaml → OperatorCreateRequest).
type OperatorCreateRequest struct {
	AID         string   `json:"aid" required:"true" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID нового Архонта (naming-rules.md)"`
	DisplayName string   `json:"display_name,omitempty" doc:"человекочитаемое имя для UI/аудита"`
	Roles       []string `json:"roles,omitempty" doc:"опц. список ролей для atomic create+grant (онбординг одним вызовом); ошибка роли → rollback"`
}

// operatorCreateOutput — huma-output POST /v1/operators (FULL-TYPED). Status=201;
// Body — native 201-тело (OperatorCreateReply: aid/display_name/created_at/jwt/
// created_by_aid + опц. roles). JWT SENSITIVE (secret-masking на выходе логов/OTel).
type operatorCreateOutput struct {
	Status int `json:"-"`
	Body   OperatorCreateReply
}

// operatorCreateOperation — метаданные POST /v1/operators. Path = "/" относительно
// chi-группы /v1/operators. DefaultStatus=201. Permission operator.create + audit
// operator.created. Errors: 400 unknown/malformed, 403 RBAC, 404 role-grant-target,
// 409 aid-exists, 422 валидация aid/role-name, 500.
func operatorCreateOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "createOperator",
		Method:        http.MethodPost,
		Path:          "/",
		Summary:       "Создать Архонта",
		Description:   "Создаёт оператора (Archon) + опц. atomic-grant ролей (ADR-013/014). Permission operator.create. 409 — AID занят. Возвращает JWT один раз.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/operators (list) — READ-with-typed-query (БЕЗ audit) ===

// operatorListInput — huma-input GET /v1/operators (FULL-TYPED typed-query). Каждое
// поле несёт `query:"<name>"` → huma биндит из url.Values и валидирует по схеме.
//
// Семантика bind-фазы (parity легаси List → ListTyped):
//   - AuthMethod несёт `enum:"jwt,mtls,combined,ldap,oidc"` — значение вне набора → 422
//     (schema-validate enum-mismatch, НЕ parse) → error-override классифицирует как
//     TypeValidationFailed (КЛЮЧЕВОЙ контракт: enum→422, как audit source). Пустой →
//     фильтр не применяется. enum-набор = доменный operator.AuthMethod{JWT,MTLS,Combined,
//     LDAP,OIDC}; ldap/oidc — федеративная аутентификация (ADR-058), only-add к прежнему;
//   - Revoked — bool: huma parseInto на bad-value даёт "invalid boolean …" → 400
//     (hasQueryParseError). Опущен → false (только активные, parity легаси);
//   - Offset/Limit — int32 (НЕ Go-int: huma на int эмитит int64, committed-спека
//     несёт int32) с `default` (offset 0, limit 50, совпадает с shared/api.ParsePage).
//     bad-int → 400 (parseInto). ГРАНИЦЫ диапазона (offset≥0, limit∈[1,1000]) НЕ
//     выражены huma-тегами minimum/maximum СОЗНАТЕЛЬНО (huma отбивал бы out-of-range
//     на 422, а легаси ParsePage даёт 400) — enforce-ит ДОМЕННАЯ ListTyped через
//     CheckPageBounds тем же сообщением → 400.
type operatorListInput struct {
	AuthMethod string `query:"auth_method" enum:"jwt,mtls,combined,ldap,oidc" doc:"фильтр по форме credential; значение вне enum → 422"`
	Revoked    bool   `query:"revoked" doc:"включать ревокнутых (false — только активные); bad-value → 400"`
	Offset     int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit      int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// operatorListOutput — huma-output GET /v1/operators (FULL-TYPED). Body — typed
// 200-envelope (sharedapi.PagedResponse[Operator] с NATIVE element: items/offset/
// limit/total). Wire-форма (items non-nil [], created_by_aid/revoked_at nullable,
// created_at секундной точности) зафиксирована golden-JSON byte-exact-тестом.
//
// known-gap: envelope-поля Offset/Limit/Total — Go int → huma эмитит int64 в
// huma-схему, тогда как committed-спека несёт их как int32. На served-wire не
// влияет (huma-фрагмент не мержится в served OpenAPI), синхронизируется при
// мерж-батче huma-фрагментов (подъём заголовка спеки до 3.1). Query-вход
// (Offset/Limit в operatorListInput) — int32 СОЗНАТЕЛЬНО. Это общий для всех
// list-доменов envelope (sharedapi.PagedResponse), не operator-локальный.
// Alias PagedResponse[Operator] → named-схема OperatorListReply в
// registerOperatorEnvelopes (huma_operator_envelope.go).
type operatorListOutput struct {
	Body sharedapi.PagedResponse[Operator]
}

// operatorListOperation — метаданные GET /v1/operators. Path = "/" относительно
// chi-группы /v1/operators. DefaultStatus=200. READ-роут: audit НЕ навешан.
// Errors: 400 (bad typed-query bind / out-of-range pagination), 422 (bad
// auth_method enum), 500 (БД).
func operatorListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listOperators",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список Архонтов (paged + фильтры)",
		Description:   "Реестр операторов с фильтрами (auth_method enum, revoked) и пагинацией. Permission operator.list. Read-only, без audit.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/operators/{aid} (get) — READ-with-path (БЕЗ audit) ===

// operatorGetInput — huma-input GET /v1/operators/{aid}. AID — path-параметр
// (huma извлекает по `path:"aid"`). Формат AID (operator.ValidAID) — доменная
// валидация в GetTyped (422), не huma-схема.
type operatorGetInput struct {
	AID string `path:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID Архонта"`
}

// operatorGetOutput — huma-output GET /v1/operators/{aid} (FULL-TYPED). Body —
// native 200-тело (Operator). Wire-форма (nullable created_by_aid/revoked_at,
// bootstrap_initial derived, metadata omitempty) зафиксирована golden-тестом.
type operatorGetOutput struct {
	Body Operator
}

// operatorGetOperation — метаданные GET /v1/operators/{aid}. DefaultStatus=200.
// READ-роут: audit НЕ навешан. Permission operator.list (read покрыт list-правом,
// см. router.go). Errors: 404 (AID не найден), 422 (bad path-AID), 500.
func operatorGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getOperator",
		Method:        http.MethodGet,
		Path:          "/{aid}",
		Summary:       "Карточка Архонта",
		Description:   "Метаданные одного оператора по AID. Permission operator.list (read покрыт list-правом). Read-only, без audit.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/operators/{aid}/revoke (revoke) — WRITE+AUDIT operator.revoked ===

// operatorRevokeInput — huma-input POST /v1/operators/{aid}/revoke. AID — path;
// Body — typed тело (опц. reason). aid в теле (echo для MCP) НЕ читается huma-путём.
type operatorRevokeInput struct {
	AID  string `path:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID Архонта для отзыва"`
	Body OperatorRevokeRequest
}

// OperatorRevokeRequest — Go-форма тела POST /v1/operators/{aid}/revoke.
// Reason — опц. свободный текст для audit-trail. aid из тела не несём (huma path-{aid}
// — авторитет; echo-поле тела было только для MCP). Имя структуры = контрактное имя
// схемы в OpenAPI (committed-рукопись → OperatorRevokeRequest).
type OperatorRevokeRequest struct {
	Reason string `json:"reason,omitempty" doc:"свободный текст причины для audit-trail (optional)"`
}

// operatorNoContentOutput — huma-output 204-write-роута revoke. БЕЗ Body
// (легаси-контракт: 204 No Content). huma на output без Body → SetStatus(204) →
// пустое тело (wire-идентично прежнему WriteHeader(204)).
type operatorNoContentOutput struct {
	Status int `json:"-"`
}

// operatorRevokeOperation — POST /v1/operators/{aid}/revoke. DefaultStatus=204.
// Permission operator.revoke + audit operator.revoked. Errors: 403 RBAC, 404
// AID-не-найден, 409 already-revoked/last-admin-lockout, 422 bad path-AID, 500.
func operatorRevokeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "revokeOperator",
		Method:        http.MethodPost,
		Path:          "/{aid}/revoke",
		Summary:       "Отозвать Архонта",
		Description:   "Ставит operators.revoked_at (ADR-014). Permission operator.revoke. 409 — последний cluster-admin (self-lockout-защита) либо уже revoked.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === POST /v1/operators/{aid}/issue-token (issue) — WRITE+AUDIT operator.token-issued ===

// operatorIssueTokenInput — huma-input POST /v1/operators/{aid}/issue-token. AID —
// path-параметр. Body нет (выпуск нового JWT не несёт тела).
type operatorIssueTokenInput struct {
	AID string `path:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID Архонта, для которого выпускается новый JWT"`
}

// operatorIssueTokenOutput — huma-output POST /v1/operators/{aid}/issue-token
// (FULL-TYPED). Status=200; Body — native 200-тело (IssueTokenReply: aid/jwt/
// expires_at). JWT SENSITIVE. Отличие от прочих write-роутов: 200 С ТЕЛОМ (не 204).
type operatorIssueTokenOutput struct {
	Status int `json:"-"`
	Body   IssueTokenReply
}

// operatorIssueTokenOperation — POST /v1/operators/{aid}/issue-token. DefaultStatus=200.
// Permission operator.issue-token + audit operator.token-issued. Errors: 403 RBAC,
// 404 AID-не-найден, 409 revoked, 422 bad path-AID, 500.
func operatorIssueTokenOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "issueOperatorToken",
		Method:        http.MethodPost,
		Path:          "/{aid}/issue-token",
		Summary:       "Выпустить новый JWT Архонту",
		Description:   "Выпускает свежий JWT существующему оператору (ADR-014). Permission operator.issue-token. 409 — оператор revoked. Возвращает JWT один раз.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
