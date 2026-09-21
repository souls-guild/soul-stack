package api

// FULL-TYPED form of the PROVISIONING-POLICY domain (runtime policy for operator
// CREATION methods, ADR-058 Part B; code-first OpenAPI source). GET — read (no
// audit, permission provisioning.read); PUT — WRITE+AUDIT (provisioning.policy_changed,
// permission provisioning.update). The Go types are the single source of schema +
// validation + typed output.

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// === GET /v1/provisioning-policy (read) — READ (no audit) ===

// provisioningPolicyGetInput — huma input GET /v1/provisioning-policy. No parameters.
type provisioningPolicyGetInput struct{}

// provisioningPolicyGetOutput — huma output GET. Body — native 200 body
// (ProvisioningPolicyReply: allowed_methods + policy_set).
type provisioningPolicyGetOutput struct {
	Body ProvisioningPolicyReply
}

// provisioningPolicyGetOperation — metadata of GET /v1/provisioning-policy.
// Path = "/" relative to the chi group /v1/provisioning-policy. DefaultStatus=200.
// READ route: audit not wired. Permission provisioning.read — on the group. Errors:
// 403 RBAC, 500.
func provisioningPolicyGetOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getProvisioningPolicy",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Policy of operator creation methods",
		Description:   "Current list of allowed operator CREATION methods (provisioning_allowed_methods, ADR-058 Part B). policy_set=false -> policy is unset (default: all methods allowed). Permission provisioning.read. Read-only, no audit.",
		Tags:          []string{"provisioning"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === PUT /v1/provisioning-policy (update) — WRITE+AUDIT provisioning.policy_changed ===

// provisioningPolicyPutInput — huma input PUT /v1/provisioning-policy. Body —
// typed body (new list of allowed methods, replace semantics).
type provisioningPolicyPutInput struct {
	Body ProvisioningPolicyUpdateRequest
}

// ProvisioningPolicyUpdateRequest — Go form of the PUT /v1/provisioning-policy body.
// allowed_methods — list of allowed operator CREATION methods (enum
// {user,ldap,oidc}); minItems:1 — anti-lockout (can't forbid ALL methods and
// lock out operator creation). Domain validation (domain/dedup) — in PutTyped.
// Struct name = contract schema name in OpenAPI.
type ProvisioningPolicyUpdateRequest struct {
	AllowedMethods []string `json:"allowed_methods" required:"true" minItems:"1" enum:"user,ldap,oidc" doc:"allowed operator creation methods (anti-lockout: non-empty list from {user,ldap,oidc})"`
}

// provisioningPolicyPutOutput — huma output PUT. Status=200 WITH BODY (native
// ProvisioningPolicyReply — the updated policy).
type provisioningPolicyPutOutput struct {
	Status int `json:"-"`
	Body   ProvisioningPolicyReply
}

// provisioningPolicyPutOperation — metadata of PUT /v1/provisioning-policy.
// DefaultStatus=200. Permission provisioning.update + audit provisioning.policy_changed.
// Errors: 400 unknown/malformed, 403 RBAC, 404 caller-not-found (FK), 422 empty/
// invalid method (anti-lockout), 500.
func provisioningPolicyPutOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "updateProvisioningPolicy",
		Method:        http.MethodPut,
		Path:          "/",
		Summary:       "Change the operator creation methods policy",
		Description:   "Replace semantics for the list of allowed operator CREATION methods (provisioning_allowed_methods, ADR-058 Part B). Permission provisioning.update. 422 - empty list (anti-lockout) or method outside {user,ldap,oidc}. Gates ONLY operator creation; existing operators log in independent of the policy.",
		Tags:          []string{"provisioning"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}
