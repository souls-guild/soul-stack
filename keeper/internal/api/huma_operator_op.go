package api

// FULL-TYPED shape of the OPERATOR domain (code-first source of OpenAPI, ADR-054 §Pattern).
// ROLLOUT-BATCH-2a (operator fully on huma per the 5 references): create (write-middleware-
// audit), list (read-with-typed-query), get (read-with-path), revoke + issue-token
// (write-middleware-audit). Go types are the single source of truth: huma builds from
// them BOTH the OpenAPI fragment's JSON Schema AND input validation/typed-bind AND typed-output.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// === POST /v1/operators (create) — WRITE+AUDIT operator.created ===

// operatorCreateInput — huma input for POST /v1/operators (FULL-TYPED). Body —
// a typed body: huma decodes and validates it against the schema from the huma tags of
// OperatorCreateRequest. Conversion to the domain model happens in registerHumaOperatorCreate.
type operatorCreateInput struct {
	Body OperatorCreateRequest
}

// OperatorCreateRequest — native Go shape of the POST /v1/operators body (code-first
// source of schema AND validation, handler-native PILOT). AID of the new Archon + optional
// display_name + optional roles[] for atomic create+grant. The register func projects it into
// the domain handlers.OperatorCreateInput.
//
// huma tags: `required:"true"` — mandatory (missing → 422); display_name/roles —
// optional. additionalProperties:false (huma default) → unknown field → 400
// (error-override). AID format (operator.ValidAID) and role existence are domain
// validation in CreateTyped (422/404), not the huma schema. The struct name = the contractual
// schema name in OpenAPI (huma's DefaultSchemaNamer takes reflect.Type.Name()) — aligned with the
// committed hand-written spec (docs/keeper/openapi.yaml → OperatorCreateRequest).
type OperatorCreateRequest struct {
	AID         string   `json:"aid" required:"true" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID нового Архонта (naming-rules.md)"`
	DisplayName string   `json:"display_name,omitempty" doc:"человекочитаемое имя для UI/аудита"`
	Roles       []string `json:"roles,omitempty" doc:"опц. список ролей для atomic create+grant (онбординг одним вызовом); ошибка роли → rollback"`
}

// operatorCreateOutput — huma output for POST /v1/operators (FULL-TYPED). Status=201;
// Body — native 201 body (OperatorCreateReply: aid/display_name/created_at/jwt/
// created_by_aid + optional roles). JWT SENSITIVE (secret-masking on the way out to logs/OTel).
type operatorCreateOutput struct {
	Status int `json:"-"`
	Body   OperatorCreateReply
}

// operatorCreateOperation — metadata for POST /v1/operators. Path = "/" relative to
// the chi group /v1/operators. DefaultStatus=201. Permission operator.create + audit
// operator.created. Errors: 400 unknown/malformed, 403 RBAC, 404 role-grant-target,
// 409 aid-exists, 422 aid/role-name validation, 500.
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

// === GET /v1/operators (list) — READ with typed query (no audit) ===

// operatorListInput — huma input for GET /v1/operators (FULL-TYPED typed-query). Each
// field carries `query:"<name>"` → huma binds from url.Values and validates against the schema.
//
// Bind-phase semantics (parity with legacy List → ListTyped):
//   - AuthMethod carries `enum:"jwt,mtls,combined,ldap,oidc"` — a value outside the set → 422
//     (schema-validate enum-mismatch, NOT parse) → error-override classifies it as
//     TypeValidationFailed (KEY contract: enum→422, as an audit source). Empty →
//     no filter applied. enum set = domain operator.AuthMethod{JWT,MTLS,Combined,
//     LDAP,OIDC}; ldap/oidc — federated authentication (ADR-058), only-add to the former;
//   - Revoked — bool: huma parseInto on a bad value yields "invalid boolean …" → 400
//     (hasQueryParseError). Omitted → false (active only, parity with legacy);
//   - Q — free substring search (ILIKE) over display_name/aid, case-insensitive;
//     empty → no filter applied (parity with /v1/runs q);
//   - Offset/Limit — int32 (NOT Go int: huma emits int64 for int, the committed spec
//     carries int32) with `default` (offset 0, limit 50, matches shared/api.ParsePage).
//     bad-int → 400 (parseInto). The range BOUNDS (offset≥0, limit∈[1,1000]) are NOT
//     expressed via huma minimum/maximum tags DELIBERATELY (huma would reject out-of-range
//     with 422, whereas legacy ParsePage returns 400) — enforced by the DOMAIN ListTyped via
//     CheckPageBounds with the same message → 400.
type operatorListInput struct {
	AuthMethod string `query:"auth_method" enum:"jwt,mtls,combined,ldap,oidc" doc:"фильтр по форме credential; значение вне enum → 422"`
	Revoked    bool   `query:"revoked" doc:"включать ревокнутых (false — только активные); bad-value → 400"`
	Q          string `query:"q" doc:"свободный поиск по display_name/aid (substring, регистронезависимо); пусто → без фильтра"`
	Offset     int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
	Limit      int32  `query:"limit" default:"50" doc:"размер страницы 1..1000 (совпадает с shared/api.ParsePage; out-of-range → 400)"`
}

// operatorListOutput — huma output for GET /v1/operators (FULL-TYPED). Body — typed
// 200-envelope (sharedapi.PagedResponse[Operator] with a NATIVE element: items/offset/
// limit/total). The wire shape (items non-nil [], created_by_aid/revoked_at nullable,
// created_at at second precision) is pinned by a golden-JSON byte-exact test.
//
// known gap: envelope fields Offset/Limit/Total — Go int → huma emits int64 in the
// huma schema, whereas the committed spec carries them as int32. Does not affect the
// served wire (the huma fragment is not merged into the served OpenAPI); synchronized during
// the huma-fragment merge batch (raising the spec header to 3.1). The query input
// (Offset/Limit in operatorListInput) is int32 DELIBERATELY. This is the envelope shared by all
// list domains (sharedapi.PagedResponse), not operator-local.
// Alias PagedResponse[Operator] → named schema OperatorListReply in
// registerOperatorEnvelopes (huma_operator_envelope.go).
type operatorListOutput struct {
	Body sharedapi.PagedResponse[Operator]
}

// operatorListOperation — metadata for GET /v1/operators. Path = "/" relative to
// the chi group /v1/operators. DefaultStatus=200. READ route: audit not wired.
// Errors: 400 (bad typed-query bind / out-of-range pagination), 422 (bad
// auth_method enum), 500 (DB).
func operatorListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listOperators",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Список Архонтов (paged + фильтры)",
		Description:   "Реестр операторов с фильтрами (auth_method enum, revoked, q — substring-поиск по display_name/aid, регистронезависимо) и пагинацией. Permission operator.list. Read-only, без audit.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/operators/{aid} (get) — READ with path (no audit) ===

// operatorGetInput — huma input for GET /v1/operators/{aid}. AID — path parameter
// (huma extracts it via `path:"aid"`). AID format (operator.ValidAID) — domain
// validation in GetTyped (422), not the huma schema.
type operatorGetInput struct {
	AID string `path:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID Архонта"`
}

// operatorGetOutput — huma output for GET /v1/operators/{aid} (FULL-TYPED). Body —
// native 200 body (Operator). The wire shape (nullable created_by_aid/revoked_at,
// bootstrap_initial derived, metadata omitempty) is pinned by a golden test.
type operatorGetOutput struct {
	Body Operator
}

// operatorGetOperation — metadata for GET /v1/operators/{aid}. DefaultStatus=200.
// READ route: audit not wired. Permission operator.list (read is covered by the list right,
// see router.go). Errors: 404 (AID not found), 422 (bad path-AID), 500.
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

// operatorRevokeInput — huma input for POST /v1/operators/{aid}/revoke. AID — path;
// Body — typed body (optional reason). aid in the body (echo for MCP) is NOT read by huma.
type operatorRevokeInput struct {
	AID  string `path:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID Архонта для отзыва"`
	Body OperatorRevokeRequest
}

// OperatorRevokeRequest — Go shape of the POST /v1/operators/{aid}/revoke body.
// Reason — optional free text for the audit trail. We do not carry aid from the body (huma path-{aid}
// is authoritative; the body echo field was only for MCP). The struct name = the contractual
// schema name in OpenAPI (committed hand-written spec → OperatorRevokeRequest).
type OperatorRevokeRequest struct {
	Reason string `json:"reason,omitempty" doc:"свободный текст причины для audit-trail (optional)"`
}

// operatorNoContentOutput — huma output for the 204 write route revoke. No Body
// (legacy contract: 204 No Content). huma on an output without Body → SetStatus(204) →
// empty body (wire-identical to the former WriteHeader(204)).
type operatorNoContentOutput struct {
	Status int `json:"-"`
}

// operatorRevokeOperation — POST /v1/operators/{aid}/revoke. DefaultStatus=204.
// Permission operator.revoke + audit operator.revoked. Errors: 403 RBAC, 404
// AID-not-found, 409 already-revoked/last-admin-lockout, 422 bad path-AID, 500.
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

// operatorIssueTokenInput — huma input for POST /v1/operators/{aid}/issue-token. AID —
// path parameter. No Body (issuing a new JWT carries no body).
type operatorIssueTokenInput struct {
	AID string `path:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID Архонта, для которого выпускается новый JWT"`
}

// operatorIssueTokenOutput — huma output for POST /v1/operators/{aid}/issue-token
// (FULL-TYPED). Status=200; Body — native 200 body (IssueTokenReply: aid/jwt/
// expires_at). JWT SENSITIVE. Unlike other write routes: 200 WITH BODY (not 204).
type operatorIssueTokenOutput struct {
	Status int `json:"-"`
	Body   IssueTokenReply
}

// operatorIssueTokenOperation — POST /v1/operators/{aid}/issue-token. DefaultStatus=200.
// Permission operator.issue-token + audit operator.token-issued. Errors: 403 RBAC,
// 404 AID-not-found, 409 revoked, 422 bad path-AID, 500.
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
