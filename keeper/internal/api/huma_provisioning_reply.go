package api

// HUMA-NATIVE wire-DTO of the PROVISIONING-POLICY domain (ADR-058 Part B). The Reply Body
// of GET/PUT is a native Go struct in package api. The handler (handlers/provisioning.go)
// returns the FLAT domain ProvisioningPolicyView; the register func projects it into
// this schema. THE SCHEMA NAME = the contract one (ProvisioningPolicyReply): huma
// DefaultSchemaNamer takes reflect.Type.Name().

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// ProvisioningPolicyReply — the native 200 body of GET and PUT /v1/provisioning-policy.
// allowed_methods — the list of allowed operator-CREATION methods (from {user,ldap,
// oidc}); non-nil `[]` when policy_set=false (policy not set → default "everything
// allowed", empty list). policy_set — whether the policy is set explicitly.
type ProvisioningPolicyReply struct {
	AllowedMethods []string `json:"allowed_methods"`
	PolicySet      bool     `json:"policy_set"`
}

// newProvisioningPolicyReply projects the flat handlers.ProvisioningPolicyView into
// native. allowed_methods — non-nil `[]` (a nil list from the domain → empty array in
// the wire, not null).
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
