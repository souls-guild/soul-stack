// Package serviceregistry — Postgres registry of Services and Keeper's
// cluster-wide key-value settings (migrations 034 service_registry / 035
// keeper_settings). Moves the `services[]` catalog and top-level scalars out
// of static keeper.yml into tables managed via OpenAPI/MCP, symmetric to
// RBAC storage (ADR-028).
//
// Slice S1: types + raw-SQL CRUD ([repository.go]) + validating service
// wrapper ([service.go]). Holder snapshot + cluster-wide invalidation (S2),
// OpenAPI/MCP transport facade (S3), and config hard-cut / consumer
// switchover (S4) are separate slices — not here.
package serviceregistry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Sentinel errors of the CRUD layer. The transport side (separate slice S3)
// maps them to HTTP codes:
//   - ErrAlreadyExists  → 409 (UNIQUE on PK service_registry.name);
//   - ErrNotFound       → 404 (no row for the PK);
//   - ErrInvalidName    → 422 (name doesn't match the format);
//   - ErrInvalidGit     → 422 (git is empty);
//   - ErrInvalidRef     → 422 (ref is empty);
//   - ErrInvalidRefresh → 422 (refresh doesn't parse as a duration);
//   - ErrOperatorNotFound → 404 (FK violation on created_by_aid/updated_by_aid:
//     the referenced operator doesn't exist).
var (
	ErrAlreadyExists    = errors.New("serviceregistry: service name already exists")
	ErrNotFound         = errors.New("serviceregistry: service name not found")
	ErrInvalidName      = errors.New("serviceregistry: invalid service name")
	ErrInvalidGit       = errors.New("serviceregistry: git is empty")
	ErrInvalidRef       = errors.New("serviceregistry: ref is empty")
	ErrInvalidRefresh   = errors.New("serviceregistry: invalid refresh duration")
	ErrOperatorNotFound = errors.New("serviceregistry: referenced operator (AID) not found")

	// ErrSettingNotFound — no row in keeper_settings for the key (GetSetting).
	ErrSettingNotFound = errors.New("serviceregistry: setting key not found")
	// ErrInvalidSettingKey — key doesn't match keeper_settings_key_format.
	ErrInvalidSettingKey = errors.New("serviceregistry: invalid setting key")

	// ErrEmptyProvisioningMethods — the provisioning_allowed_methods key is SET,
	// but the parsed set is empty (value empty / whitespace-only / commas-only).
	// This is a config ERROR (anti-lockout): an empty policy would block operator
	// CREATION via all methods (user/ldap/oidc) — an operator must not silently
	// lock themselves out. The "everything allowed" default is signaled by the
	// key's ABSENCE, not by an empty value.
	ErrInvalidProvisioningMethod = errors.New("serviceregistry: provisioning method must be one of {user,ldap,oidc}")
	ErrEmptyProvisioningMethods  = errors.New("serviceregistry: provisioning_allowed_methods is set but empty (anti-lockout)")
)

// NamePattern — canonical Service name form: matches CHECK
// service_registry_name_format in migration 034 (like rbac.reRoleName).
// Duplicated in Go for application-level validation before the round trip
// (better error, no wasted DB round trip on a malformed name).
const NamePattern = `^[a-z][a-z0-9-]*$`

// SettingKeyPattern — keeper_settings key form: matches CHECK
// keeper_settings_key_format in migration 035 (snake_case).
const SettingKeyPattern = `^[a-z][a-z0-9_]*$`

var (
	nameRe       = regexp.MustCompile(NamePattern)
	settingKeyRe = regexp.MustCompile(SettingKeyPattern)
)

// ValidName checks a Service name against the canonical form.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ValidSettingKey checks a keeper_settings key against the canonical form.
func ValidSettingKey(key string) bool { return settingKeyRe.MatchString(key) }

// Well-known keeper_settings keys. Semantics and the set live here, not in
// the schema (the table is untyped — see migration 035). default_module_source
// is NOT introduced: it has no consumer in keeper code (a dead field from the
// old config).
const (
	// SettingDefaultDestinySource — default git source for Destiny.
	SettingDefaultDestinySource = "default_destiny_source"

	// SettingProvisioningAllowedMethods — CSV from the domain {user,ldap,oidc}:
	// the list of allowed operator-CREATION methods (created_via methods). Gates
	// only the creation branch (POST /v1/operators → "user"; federated
	// auto-provision → "ldap"/"oidc"); bootstrap/system are NEVER gated. Key
	// ABSENT = everything allowed (back-compat); SET-but-empty = config error
	// ([ErrEmptyProvisioningMethods], anti-lockout). Parsing semantics —
	// [ParseProvisioningMethods].
	SettingProvisioningAllowedMethods = "provisioning_allowed_methods"
)

// provisioningMethodDomain — the closed set of created_via methods that can be
// specified in the provisioning_allowed_methods policy. bootstrap/system are
// NOT included: they aren't gated (bootstrap of the first Archon via
// `keeper init`, system = internal records) — specifying them in the policy
// is an error.
var provisioningMethodDomain = map[string]struct{}{
	"user": {},
	"ldap": {},
	"oidc": {},
}

// ParseProvisioningMethods parses the CSV value of the
// provisioning_allowed_methods policy into a set of allowed methods. Semantics:
//   - split on ',', trim whitespace, drop empty elements, lowercase;
//   - each element is validated against [provisioningMethodDomain] ({user,ldap,
//     oidc}); invalid → [ErrInvalidProvisioningMethod];
//   - empty result (csv="" / commas-and-whitespace only) → [ErrEmptyProvisioningMethods].
//
// "Key absent" is handled ABOVE (PoolSource.Load: ErrSettingNotFound → policy
// not set); only a set value is passed in here.
func ParseProvisioningMethods(csv string) (map[string]bool, error) {
	out := make(map[string]bool)
	for _, part := range strings.Split(csv, ",") {
		m := strings.ToLower(strings.TrimSpace(part))
		if m == "" {
			continue
		}
		if _, ok := provisioningMethodDomain[m]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrInvalidProvisioningMethod, m)
		}
		out[m] = true
	}
	if len(out) == 0 {
		return nil, ErrEmptyProvisioningMethods
	}
	return out, nil
}

// ServiceEntry — runtime representation of a service_registry row. Carries
// the Service's git coordinates (Name/Git/Ref, ADR-007) plus audit metadata;
// the registry replaces the removed `keeper.yml::services[]` (ADR-029).
//
// Refresh — auto-refresh duration string ("5m"); nil = no auto-refresh (NULL
// in the DB). CreatedByAID / UpdatedByAID — AID of the author/last editor
// operator; nil = NULL (seed / no initiating Archon / before the first update).
type ServiceEntry struct {
	Name         string    `json:"name"`
	Git          string    `json:"git"`
	Ref          string    `json:"ref"`
	Refresh      *string   `json:"refresh,omitempty"`
	CreatedByAID *string   `json:"created_by_aid,omitempty"`
	UpdatedByAID *string   `json:"updated_by_aid,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Setting — runtime representation of a keeper_settings row.
type Setting struct {
	Key          string    `json:"key"`
	Value        string    `json:"value"`
	UpdatedByAID *string   `json:"updated_by_aid,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}
