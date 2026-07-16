// Package pushprovider is a registry of SSH-Provider parameters in Postgres (ADR-032
// amendment 2026-05-26, S7-2).
//
// Push-Provider is per-provider params for env-payload of SSH plugins in
// push-flow (ADR-020 amendment 1, env convention SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS).
// Canonical long-term store instead of keeper.yml::push.providers[] inline (pilot S6/S7-1).
//
// Conceptually—an SSH-Provider variant of Provider (PM-decision S7-1 #1, S7-2 #1):
// same concept (Cloud Provider + SSH Provider) but separate tables
// (providers for cloud, push_providers for SSH), different params schemas,
// and different RBAC permission scopes (provider.* vs push-provider.*).
package pushprovider

import (
	"regexp"
	"time"
)

// NamePattern is the canonical form for PushProvider names: kebab-case,
// starts with a letter, length 1..63. Matches CHECK constraint
// push_providers_name_format in migration 054 and pattern
// ^[a-z][a-z0-9-]{0,62}$ from keeper.yml::push.providers[].name.
//
// Additional restriction vs cloud-Provider (^[a-z0-9-]{1,63}$):
// name must start with a letter because it translates to an env var
// (SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS)—a leading digit or dash
// would break the env-var-name.
const NamePattern = `^[a-z][a-z0-9-]{0,62}$`

var nameRe = regexp.MustCompile(NamePattern)

// ValidName checks whether name matches the canonical form.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// PushProvider is the runtime representation of a push_providers table row.
//
// Params is the opaque form of the provider: keys and values are defined by the
// plugin itself (vault_addr/role/proxy_addr/…). Sensitive keys
// (secret_id/token/password/private_key) MUST be vault-refs
// (vault:<path>)—validation occurs at service layer (Service.validateSensitive),
// not storage.
type PushProvider struct {
	Name         string         `json:"name"`
	Params       map[string]any `json:"params"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	CreatedByAID string         `json:"created_by_aid"`
	UpdatedByAID *string        `json:"updated_by_aid,omitempty"`
}
