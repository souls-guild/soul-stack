// Package auth — federated authentication of operators (Archon) on top of
// external identity providers (LDAP / OAuth2-OIDC). ADR-058 (accepted,
// stage 1 LDAP + stage 2 OIDC implemented and completed end-to-end).
//
// Model (ADR-058): the external IdP authenticates the human Archon, Keeper
// VALIDATES the result, MAPS the external identity onto the operators(aid)
// registry + RBAC roles, and issues an INTERNAL JWT via the existing
// jwt.Issuer (ADR-014). The rest of the system (auth middleware, RBAC, MCP,
// OpenAPI) stays JWT-based and is unchanged.
//
// Package contents: Authenticator/Mapper contracts + ExternalIdentity/MappedOperator
// (this file), the DBMapper implementation mapping onto operators+roles
// (mapper.go), concrete authenticators — subpackages ldap/ and oidc/ (network
// calls, go-ldap/go-oidc/oauth2). Endpoint layer — keeper/internal/api/huma_{auth,oidc}.go.
package auth

import (
	"context"
	"errors"
)

// ExternalIdentity — result of a successful external authentication (LDAP
// bind or OIDC id_token), before mapping onto operators(aid). A plain
// snapshot of what the external IdP returned, with no project decisions
// applied yet (AID/roles are assigned by Mapper).
//
// Subject — stable identifier at the IdP (OIDC `sub` or LDAP user DN).
// AID — derived project operator identifier (operators.aid), extracted by the
// Authenticator from the configured attribute/claim (LDAP `aid_attr`, default
// `uid`; OIDC `aid_claim`). Kept separate from Subject because Subject is the
// IdP's raw identifier (user DN), while AID is what the operator is known as
// in the registry and in JWT.sub. Mapper takes AID from here.
// Email / Username — optional human-readable fields.
// Groups — external group membership (source for role mapping).
// Claims — raw additional claims/attributes (for extensible mapping).
type ExternalIdentity struct {
	Subject  string
	AID      string
	Email    string
	Username string
	Groups   []string
	Claims   map[string]any
}

// MappedOperator — an external identity mapped onto the project's
// authorization subject: an operators-registry AID + RBAC roles. This is what
// jwt.Issuer issues the internal token from (claims sub=AID, roles=Roles per
// ADR-014).
//
// Provisioned=true means Mapper created a NEW operators row (auto-provision,
// ADR-058(g) decision #1); false means the operator already existed.
type MappedOperator struct {
	AID         string
	Roles       []string
	Provisioned bool
}

// Authenticator — common contract for a federated authentication method.
// Implementations: ldap.Authenticator (bind) and oidc.Authenticator (code
// flow). Returns ONLY the fact of "who this is at the external IdP" —
// mapping to AID and issuing the JWT happen higher up the stack (Mapper +
// jwt.Issuer), so the authentication method itself stays unaware of the
// operators registry and RBAC.
type Authenticator interface {
	// Method — the auth_method value (operator.AuthMethod) for audit / the
	// operators row: "ldap" | "oidc" (ADR-058(a)).
	Method() string
}

// Mapper maps an external identity onto operators(aid) + roles (ADR-058(d)).
// Encapsulates the provisioning decision (auto-provision vs pre-register,
// #1) and the role-source decision (external groups vs registry, #2) — both
// await a user decision, so only the contract lives here.
type Mapper interface {
	// Map converts ExternalIdentity into MappedOperator, or returns an error
	// (operator revoked / not found under pre-register / no role mapping,
	// etc).
	Map(ctx context.Context, ext ExternalIdentity) (MappedOperator, error)
}

// Sentinel errors for federated authentication. Their public HTTP detail is
// classified separately (like jwt.ClassifyVerifyErr, ADR-014): the cause
// must not leak outward (anti-oracle); refined at implementation time.
var (
	// ErrAuthFailed — external authentication did not succeed (bad
	// credentials, invalid id_token, IdP rejected).
	ErrAuthFailed = errors.New("auth: external authentication failed")
	// ErrOperatorRevoked — the external identity maps to a revoked operator;
	// federated login for a revoked operator is forbidden (ADR-058(d)
	// revocation invariant).
	ErrOperatorRevoked = errors.New("auth: operator revoked")
	// ErrOperatorNotProvisioned — pre-register mode, operator was not
	// registered in advance.
	ErrOperatorNotProvisioned = errors.New("auth: operator not pre-registered")
	// ErrNoRoleMapping — the external identity has no role that maps.
	ErrNoRoleMapping = errors.New("auth: no role mapping for external identity")
	// ErrProvisioningDisabled — the provisioning_allowed_methods policy
	// forbids CREATING an operator via this method (ADR-058 Part B).
	// Returned by Mapper from the provision branch (no new operator is
	// created), ONLY on creation — an existing operator can still log in
	// regardless of policy. Not a user auth failure but a policy outcome:
	// the endpoint maps it to a meaningful 403 rather than a sanitized 401
	// (anti-oracle doesn't apply to policy — the fact "method disabled" is
	// not a leak of someone else's credentials).
	ErrProvisioningDisabled = errors.New("auth: operator provisioning is disabled for this method by policy")
)
