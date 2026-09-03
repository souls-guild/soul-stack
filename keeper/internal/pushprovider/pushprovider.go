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

// IDPattern is the canonical form for PushProvider ids: kebab-case,
// starts with a letter, length 1..63. Matches CHECK constraint
// push_providers_id_format in migration 118 (push_providers_name_format before
// it) and pattern ^[a-z][a-z0-9-]{0,62}$ from keeper.yml::push.providers[].
//
// Additional restriction vs cloud-Provider (^[a-z0-9-]{1,63}$):
// the id must start with a letter because it translates to an env var
// (SOUL_SSH_<UPPER_SNAKE(id)>_PARAMS)—a leading digit or dash
// would break the env-var-name.
//
// The form is UNCHANGED by the `name` -> `id` rename ([ADR-0085], NIM-729): the
// identifier moved spelling, not grammar, and this registry's letter-first rule
// is a local mechanical constraint that survives it.
const IDPattern = `^[a-z][a-z0-9-]{0,62}$`

var idRe = regexp.MustCompile(IDPattern)

// ValidID checks whether id matches the canonical form.
func ValidID(id string) bool { return idRe.MatchString(id) }

// PushProvider is the runtime representation of a push_providers table row.
//
// Params is the opaque form of the provider: keys and values are defined by the
// plugin itself (vault_addr/role/proxy_addr/…). Sensitive keys
// (secret_id/token/password/private_key) MUST be vault-refs
// (vault:<path>)—validation occurs at service layer (Service.validateSensitive),
// not storage.
type PushProvider struct {
	// ID is the immutable identifier ([ADR-0085]): set once at creation, the
	// PRIMARY KEY of `push_providers`, and the source of the env-var name
	// SOUL_SSH_<UPPER_SNAKE(id)>_PARAMS. There is no rename operation.
	ID string `json:"id"`
	// Label is the display caption ([ADR-0085]): free text, mutable via
	// SetLabel, not unique, optional. nil means the column is NULL and a
	// consumer shows ID instead. It participates in nothing derived — in
	// particular NOT the env-var name SOUL_SSH_<UPPER_SNAKE(id)>_PARAMS, which
	// is built from ID alone. That is also why ID keeps the letter-first
	// rule and Label needs no rule at all.
	//
	// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md
	Label        *string        `json:"label,omitempty"`
	Params       map[string]any `json:"params"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	CreatedByAID string         `json:"created_by_aid"`
	UpdatedByAID *string        `json:"updated_by_aid,omitempty"`
}
