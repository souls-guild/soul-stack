// Package operator — types of Archon registry (operators) per ADR-014.
//
// M0.5a: Go-struct only + helper methods + AID-regex for use outside
// SQL CHECK. CRUD (Insert / SelectByAID / Revoke / List) and self-lockout
// invariant (ADR-013 + rbac.md) added in M0.5c with bootstrap logic
// and Operator API in M0.6.
package operator

import (
	"regexp"
	"time"
)

// AuthMethod — form of Archon credential.
//
// MVP per ADR-014: AuthMethodJWT only. AuthMethodMTLS and
// AuthMethodCombined reserved as enum extensions post-MVP
// (via `auth_method` column, without breaking schema changes).
type AuthMethod string

const (
	AuthMethodJWT      AuthMethod = "jwt"
	AuthMethodMTLS     AuthMethod = "mtls"
	AuthMethodCombined AuthMethod = "combined"

	// Federated authentication (ADR-058, accepted). Only-add enum extension
	// (like mTLS/combined post-MVP in ADR-014): captures how externally
	// operator arrived. Internal JWT after issuance is identical. SQL CHECK
	// `auth_method_valid` extended by migration 083 (only-add, forward-only).
	AuthMethodLDAP AuthMethod = "ldap" // ADR-058 stage 1
	AuthMethodOIDC AuthMethod = "oidc" // ADR-058 stage 2
)

// CreatedVia — source of operator creation (ADR-058(d)). Differs from
// [AuthMethod]: auth_method answers "how operator logs in", created_via —
// "where it came from in registry". Bootstrap Archon created via
// `keeper init` (created_via=bootstrap), but logs in via jwt; federated operator
// created via auto-provision (created_via=ldap/oidc); `archon-system` —
// system anchor for FK attribution of system-initiated inserts.
//
// Type — string alias (not separate enum type), because value stored in
// common field Operator.CreatedVia alongside other string columns and doesn't
// require methods; domain validated in [Insert] and SQL CHECK `created_via_valid`
// (migration 084).
type CreatedVia = string

const (
	CreatedViaBootstrap CreatedVia = "bootstrap"
	CreatedViaUser      CreatedVia = "user"
	CreatedViaLDAP      CreatedVia = "ldap"
	CreatedViaOIDC      CreatedVia = "oidc"
	CreatedViaSystem    CreatedVia = "system"
)

// AIDPattern — form of Archon ID (ADR-014 amendment 2026-05-29): first
// character is lowercase ASCII letter or digit, then 1..127 characters from
// `[a-z0-9._@-]`. Total length of AID is 2..128 characters. Prefix
// `archon-` no longer required.
//
// Charset intentionally narrow and safe: no `/`/`\` (path-traversal),
// only ASCII lowercase (no unicode lookalikes and case), no
// control/quote characters (no injections). `@` and `.` allowed for
// email-like external names (LDAP/Keycloak auto-provision).
//
// Duplicates SQL CHECK `aid_format` (migration 058) — needed for application
// validation on API handler side before request reaches DB
// (better error messages, no unnecessary round-trip).
const AIDPattern = `^[a-z0-9][a-z0-9._@-]{1,127}$`

var aidRe = regexp.MustCompile(AIDPattern)

// ValidAID checks AID compliance with canonical form.
func ValidAID(aid string) bool { return aidRe.MatchString(aid) }

// Operator — runtime representation of operators registry row
// (ADR-014, docs/keeper/storage.md → operators table).
//
// JSON tags — for future Operator API (M0.6). SQL NULL semantics
// mapped to pointers: CreatedByAID = nil for first bootstrap Archon
// and for other rows without parent (archon-system, federated — ADR-058(d)
// legalized NULL for non-bootstrap), RevokedAt = nil for active.
// "This is the first Archon" determined via CreatedVia, not via nil parent.
type Operator struct {
	AID          string         `json:"aid"`
	DisplayName  string         `json:"display_name"`
	AuthMethod   AuthMethod     `json:"auth_method"`
	CreatedAt    time.Time      `json:"created_at"`
	CreatedByAID *string        `json:"created_by_aid,omitempty"`
	CreatedVia   CreatedVia     `json:"created_via"`
	RevokedAt    *time.Time     `json:"revoked_at,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// IsRevoked — Operator has RevokedAt set (non-nil). Active JWTs
// of existing sessions per ADR-014(d) continue to work until `exp`;
// IsRevoked check needed on write-path operations and for UI.
func (o *Operator) IsRevoked() bool { return o.RevokedAt != nil }

// IsBootstrap — Operator created via `keeper init` (created_via='bootstrap').
// Useful for audit / RBAC checks "cannot delete bootstrap Archon
// if it is the last with *-permission" (self-lockout, ADR-014 + rbac.md).
//
// ADR-058(d): indicator moved from `CreatedByAID == nil` to
// `CreatedVia == CreatedViaBootstrap`. After legalizing NULL in created_by_aid
// for non-bootstrap rows (archon-system, federated operators), check on
// created_by_aid would give false-positive bootstrap flag — only
// authority "this is the first Archon" is now created_via.
func (o *Operator) IsBootstrap() bool { return o.CreatedVia == CreatedViaBootstrap }
