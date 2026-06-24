package api

// FULL-TYPED форма PROVISIONING-POLICY-домена (runtime-политика способов СОЗДАНИЯ
// операторов, ADR-058 Часть B; code-first источник OpenAPI). GET — read (БЕЗ
// audit, permission provisioning.read); PUT — WRITE+AUDIT (provisioning.policy_changed,
// permission provisioning.update). Go-типы — единственный источник схемы +
// валидации + typed-output.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === GET /v1/provisioning-policy (read) — READ (БЕЗ audit) ===

// provisioningPolicyGetInput — huma-input GET /v1/provisioning-policy. Параметров нет.
type provisioningPolicyGetInput struct{}

// provisioningPolicyGetOutput — huma-output GET. Body — native 200-тело
// (ProvisioningPolicyReply: allowed_methods + policy_set).
type provisioningPolicyGetOutput struct {
	Body ProvisioningPolicyReply
}

// provisioningPolicyGetOperation — метаданные GET /v1/provisioning-policy.
// Path = "/" относительно chi-группы /v1/provisioning-policy. DefaultStatus=200.
// READ-роут: audit НЕ навешан. Permission provisioning.read — на группе. Errors:
// 403 RBAC, 500.
func provisioningPolicyGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getProvisioningPolicy",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Политика способов создания операторов",
		Description:   "Текущий список разрешённых способов СОЗДАНИЯ оператора (provisioning_allowed_methods, ADR-058 Часть B). policy_set=false → политика не задана (дефолт: все способы разрешены). Permission provisioning.read. Read-only, без audit.",
		Tags:          []string{"provisioning"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === PUT /v1/provisioning-policy (update) — WRITE+AUDIT provisioning.policy_changed ===

// provisioningPolicyPutInput — huma-input PUT /v1/provisioning-policy. Body —
// typed тело (новый список разрешённых методов, replace-семантика).
type provisioningPolicyPutInput struct {
	Body ProvisioningPolicyUpdateRequest
}

// ProvisioningPolicyUpdateRequest — Go-форма тела PUT /v1/provisioning-policy.
// allowed_methods — список разрешённых способов СОЗДАНИЯ оператора (enum
// {user,ldap,oidc}); minItems:1 — anti-lockout (нельзя запретить ВСЕ методы и
// залочить заведение операторов). Доменная валидация (домен/dedup) — в PutTyped.
// Имя структуры = контрактное имя схемы в OpenAPI.
type ProvisioningPolicyUpdateRequest struct {
	AllowedMethods []string `json:"allowed_methods" required:"true" minItems:"1" enum:"user,ldap,oidc" doc:"разрешённые способы создания оператора (anti-lockout: непустой список из {user,ldap,oidc})"`
}

// provisioningPolicyPutOutput — huma-output PUT. Status=200 С ТЕЛОМ (native
// ProvisioningPolicyReply — обновлённая политика).
type provisioningPolicyPutOutput struct {
	Status int `json:"-"`
	Body   ProvisioningPolicyReply
}

// provisioningPolicyPutOperation — метаданные PUT /v1/provisioning-policy.
// DefaultStatus=200. Permission provisioning.update + audit provisioning.policy_changed.
// Errors: 400 unknown/malformed, 403 RBAC, 404 caller-not-found (FK), 422 пустой/
// невалидный метод (anti-lockout), 500.
func provisioningPolicyPutOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateProvisioningPolicy",
		Method:        http.MethodPut,
		Path:          "/",
		Summary:       "Сменить политику способов создания операторов",
		Description:   "Replace-семантика списка разрешённых способов СОЗДАНИЯ оператора (provisioning_allowed_methods, ADR-058 Часть B). Permission provisioning.update. 422 — пустой список (anti-lockout) или метод вне {user,ldap,oidc}. Гейтит ТОЛЬКО создание оператора; существующие логинятся независимо от политики.",
		Tags:          []string{"provisioning"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
