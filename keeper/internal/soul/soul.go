// Package soul provides registry types for Soul agents (`souls`) under ADR-002 / ADR-012
// and docs/soul/identity.md.
//
// M2.1.a: types + CRUD (Insert / SelectBySID / UpdateStatus / List).
// gRPC Bootstrap-handler, EventStream, SoulSeed certificate issuance are
// next slices (M2.1.b, M2.2+).
package soul

import (
	"regexp"
	"time"
)

// Transport is the command delivery method to Soul (ADR-002).
//
// `agent` - pull model, Soul daemon holds a long-lived gRPC stream.
// `ssh` - push model, Keeper connects to host via SSH (keeper.push); such
// hosts have no bootstrap_token / soul_seed.
//
// Matches CHECK `souls_transport_valid` in 007_create_souls.up.sql.
type Transport string

const (
	TransportAgent Transport = "agent"
	TransportSSH   Transport = "ssh"
)

// Status is Soul state in the registry. Narrow MVP enum (docs/soul/identity.md):
//
//   - `pending` - operator issued bootstrap token, Soul not yet connected.
//   - `connected` - stream alive, Keeper holds lease in Redis.
//   - `disconnected` - stream closed, lease expired (Soul may return).
//   - `revoked` - operator revoked, new connections rejected at mTLS level.
//   - `expired` - Reaper moved `pending` after bootstrap token TTL.
//   - `destroyed` - host physically deleted via `core.cloud.provisioned destroyed`
//     (ADR-017 cascade). Terminal state: no outgoing transitions; intentionally NOT
//     included in default set `purge_souls.statuses` (forensic > GC).
//
// Matches CHECK `souls_status_valid` in 007_create_souls.up.sql
// (extended by migration 016 - added `destroyed`).
type Status string

const (
	StatusPending      Status = "pending"
	StatusConnected    Status = "connected"
	StatusDisconnected Status = "disconnected"
	StatusRevoked      Status = "revoked"
	StatusExpired      Status = "expired"
	StatusDestroyed    Status = "destroyed"
)

// SIDPattern is the canonical form of SID (= host FQDN).
//
// Lowercase letters/digits, dots, hyphens; starts with alnum; 1..254
// characters. Duplicates CHECK `souls_sid_format` from migration - needed for
// validation before round-trip (better error messages, no extra PG query).
//
// Strict RFC-1035 label length limit of 63 characters is not
// enforced here (FQDN with label > 63 does not exist in practice, but PG-CHECK
// enforces total string length).
const SIDPattern = `^[a-z0-9][a-z0-9.-]{0,253}$`

var sidRe = regexp.MustCompile(SIDPattern)

// ValidSID checks SID conformance to canonical form.
func ValidSID(sid string) bool { return sidRe.MatchString(sid) }

// ReservedSIDs are synthetic apply_runs.sid that do not address Souls: keeper-side
// target (render.KeeperTargetSID) and run-sentinel for abort before dispatch
// (render.RunSentinelSID). Registering a Soul with such sid would cause PK
// collision in apply_runs(apply_id, sid, passage) and indistinguishability from
// synthetic entries in UI (NIM-36).
// Literals duplicate render constants (soul is a leaf package, render does not import it);
// drift is caught by reserved_sid_test.go.
var ReservedSIDs = map[string]struct{}{
	"keeper":  {}, // = render.KeeperTargetSID
	"__run__": {}, // = render.RunSentinelSID (underscore fails ValidSID anyway)
}

// IsReservedSID checks if sid is reserved by the system (see ReservedSIDs); Soul with such
// name cannot be registered (bootstrap rejects it).
func IsReservedSID(sid string) bool {
	_, ok := ReservedSIDs[sid]
	return ok
}

// CovenPattern is the canonical form of Coven tag: single-level kebab-case
// (ADR-008 - stable logical tags). Matches form with service/scenario name
// and with `reCovenName` in shared/config (validation of scenario `on:` tags):
// starts with letter, segments separated by hyphens, no trailing/double
// hyphens. Length limit 1..63 - RFC-1035-compatible threshold for label.
const CovenPattern = `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`

var covenRe = regexp.MustCompile(CovenPattern)

const covenMaxLen = 63

// ValidCoven checks a single Coven tag (kebab-case, 1..63 characters).
func ValidCoven(label string) bool {
	if len(label) == 0 || len(label) > covenMaxLen {
		return false
	}
	return covenRe.MatchString(label)
}

// Soul is the runtime representation of a row in the `souls` registry.
//
// JSON tags are for future Operator API (M0.7+). SQL NULL semantics
// map to pointers: LastSeenAt = nil for never-connected
// Soul, CreatedByAID = nil for seed import from cli-bootstrap.
type Soul struct {
	SID       string    `json:"sid"`
	Transport Transport `json:"transport"`
	Status    Status    `json:"status"`
	Coven     []string  `json:"coven"`
	// Traits are operator-set key-value tags (ADR-060): key → (scalar | list).
	// Separate axis alongside flat Coven; source is operator (like Coven),
	// NOT Soul-reported. jsonb column `souls.traits` (migration 087); nil/empty
	// map = no tags (read/target pilot, write path - next slice).
	Traits        map[string]any `json:"traits,omitempty"`
	RegisteredAt  time.Time      `json:"registered_at"`
	LastSeenAt    *time.Time     `json:"last_seen_at,omitempty"`
	LastSeenByKID *string        `json:"last_seen_by_kid,omitempty"`
	CreatedByAID  *string        `json:"created_by_aid,omitempty"`
	RequestedAt   *time.Time     `json:"requested_at,omitempty"`
	Note          string         `json:"note,omitempty"`
}

// ValidStatus / ValidTransport are exported closed-enum checks
// for list-filter in handler layer (symmetrically with [incarnation.ValidStatus]).
// Delegate to private validators that duplicate SQL-CHECK.
func ValidStatus(s Status) bool       { return validStatus(s) }
func ValidTransport(t Transport) bool { return validTransport(t) }

// validStatus / validTransport are closed enum checks that duplicate
// SQL-CHECK for rejection before round-trip.
func validStatus(s Status) bool {
	switch s {
	case StatusPending, StatusConnected, StatusDisconnected, StatusRevoked, StatusExpired, StatusDestroyed:
		return true
	}
	return false
}

func validTransport(t Transport) bool {
	switch t {
	case TransportAgent, TransportSSH:
		return true
	}
	return false
}
