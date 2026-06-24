package api

// HUMA-NATIVE wire-DTO PROVISIONING-POLICY-домена (ADR-058 Часть B). Reply Body
// GET/PUT — native Go-struct в пакете api. Handler (handlers/provisioning.go)
// возвращает доменную ПЛОСКУЮ ProvisioningPolicyView; register-func проецирует её в
// эту схему. ИМЯ СХЕМЫ = контрактное (ProvisioningPolicyReply): huma
// DefaultSchemaNamer берёт reflect.Type.Name().

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// ProvisioningPolicyReply — native 200-тело GET и PUT /v1/provisioning-policy.
// allowed_methods — список разрешённых методов СОЗДАНИЯ оператора (из {user,ldap,
// oidc}); non-nil `[]` при policy_set=false (политика не задана → дефолт «всё
// разрешено», пустой список). policy_set — задана ли политика явно.
type ProvisioningPolicyReply struct {
	AllowedMethods []string `json:"allowed_methods"`
	PolicySet      bool     `json:"policy_set"`
}

// newProvisioningPolicyReply проецирует плоскую handlers.ProvisioningPolicyView в
// native. allowed_methods — non-nil `[]` (nil-список из домена → пустой массив в
// wire, не null).
func newProvisioningPolicyReply(v handlers.ProvisioningPolicyView) ProvisioningPolicyReply {
	methods := v.AllowedMethods
	if methods == nil {
		methods = []string{}
	}
	return ProvisioningPolicyReply{
		AllowedMethods: methods,
		PolicySet:      v.PolicySet,
	}
}
