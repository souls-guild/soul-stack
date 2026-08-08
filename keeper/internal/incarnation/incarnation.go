// Package incarnation — runtime instance of a Service in Postgres under ADR-009.
//
// M0.6c-1: types + CRUD (Create / SelectByName / SelectAll / HistorySelectByName).
// Scenario-execution / migrate-executor are the next slices (M0.6c-2/3),
// blocked on Soul gRPC infrastructure (M2.x).
package incarnation

import (
	"regexp"
	"time"
)

// Status — incarnation status. MVP enum: four base values + DESTROYING
// (S-D1, teardown phase via scenario `destroy`) + DESTROY_FAILED (S-D2a,
// terminal for a failed teardown) + DRIFT (informational, ADR-031(d)).
// PROVISIONING is post-MVP, will appear once that phase is implemented.
//
// Matches CHECK-constraint incarnation_status_valid (005 + 031 + 036 + 047).
type Status string

const (
	StatusReady           Status = "ready"
	StatusApplying        Status = "applying"
	StatusErrorLocked     Status = "error_locked"
	StatusMigrationFailed Status = "migration_failed"

	// StatusDestroying — the operator initiated destroy: teardown is running
	// (scenario `destroy`, S-D2b), followed by DELETE of the row (S-D3). Not
	// terminal for the row itself — on success the row is deleted, on failure
	// teardown moves to destroy_failed (S-D2b). Other operations (run / upgrade /
	// repeat destroy) are rejected from this status.
	StatusDestroying Status = "destroying"

	// StatusDestroyFailed — teardown (scenario `destroy`) failed on the hosts: the
	// instance is NOT deleted, state stays last known-good (teardown works with
	// hosts, not jsonb-state). Terminal, requires operator intervention — from it
	// (in S-D2b/S-D3) the operator can retry destroy, force-remove, or unlock to
	// ready. The transition into this status is set by the teardown outcome
	// (S-D2b/S-D3); S-D2a only introduces the value itself. Rejected for a normal
	// run by the fail-closed allow-list in scenario.lockRun (run.go).
	StatusDestroyFailed Status = "destroy_failed"

	// StatusDrift — the incarnation's DB state is ahead of what the hosts are
	// running (ADR-031(d)). Informational, NOT blocking: remediation = a normal
	// apply from `drift` → `ready` (the allow-list in scenario.lockRun accepts
	// drift as a starting status, symmetric to ready).
	//
	// The single writer is the legacy upgrade branch ([PrepareUpgrade], ADR-031
	// amendment 2026-06-27 / ADR-019 amendment): a state-schema migration moved
	// the pin and the state in one tx while the hosts stayed on the old
	// rollout. The Scry check that was the OTHER writer left with NIM-446;
	// upgrade-drift is what the status now means. Cleared by a successful apply
	// (commitSuccess → ready).
	StatusDrift Status = "drift"
)

// NamePattern — canonical form of an incarnation name: kebab-case, starts
// with a letter/digit, length 1..63. Same as CHECK incarnation_name_format
// in the migration.
const NamePattern = `^[a-z0-9][a-z0-9-]{0,62}$`

// ReasonMaxLen — upper bound on the free-text confirmation reason for
// unlock / rerun-last. Single source for the huma maxLength tag and the
// runtime validator (UnlockTyped / RerunLastTyped 422 on
// `len(reason) > ReasonMaxLen`). The lower bound (non-empty) is a separate
// check `reason == ""`.
const ReasonMaxLen = 500

var nameRe = regexp.MustCompile(NamePattern)

// ValidName reports whether name matches the canonical form.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Incarnation — runtime representation of an `incarnation` registry row.
//
// jsonb fields (`State` / `StatusDetails`) are `map[string]any` for
// freeform data; typing for a concrete service / scenario lives in their
// manifests, not in this layer.
type Incarnation struct {
	Name               string         `json:"name"`
	Service            string         `json:"service"`
	ServiceVersion     string         `json:"service_version"`
	StateSchemaVersion int            `json:"state_schema_version"`
	State              map[string]any `json:"state"`
	Status             Status         `json:"status"`
	StatusDetails      map[string]any `json:"status_details,omitempty"`
	CreatedByAID       *string        `json:"created_by_aid,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`

	// Covens — declared environment tags of the incarnation (ADR-008 amendment
	// a, column incarnation.covens). Source for the RBAC coven-scope of
	// incarnation operations: effective scope = Covens ∪ {Name} (the name is
	// the root Coven label). Empty array for an incarnation without env tags
	// (DEFAULT '{}').
	Covens []string `json:"covens"`

	// CreatedScenario — name of the starting scenario that created the
	// incarnation (multiple create-scenarios mechanism, Option A; column
	// incarnation.created_scenario NULLABLE, migrations 089+090). Runtime fact:
	// the operator picks it in POST /v1/incarnations (field `create_scenario`).
	//
	// nil = bare incarnation (NULL in the DB): a service with no create
	// scenarios is created as StatusReady WITHOUT a run (migration 090 dropped
	// NOT NULL/DEFAULT). A non-nil pointer is the bootstrap scenario name.
	// Pointer (not string) so "bare" is never confused with an empty string and
	// never silently normalized in `create`.
	CreatedScenario *string `json:"created_scenario,omitempty"`

	// Traits — operator-set key-value labels of the incarnation (ADR-060
	// amend, R1, column incarnation.traits jsonb). Source of truth for traits:
	// set by the operator in incarnation.spec at create, projected
	// MATERIALIZED into member-host souls.traits via a sync hook (incarnation
	// create + host bind via core.soul.registered). Value is polymorphic
	// (scalar | list). Empty map for an incarnation without traits (DEFAULT
	// '{}').
	Traits map[string]any `json:"traits"`

	// TraitsRaw — the SAME column, unparsed: the exact jsonb bytes Postgres
	// returned. Scope evaluation reads THIS, never [Incarnation.Traits] — see
	// [rbac.TraitValues] for why (NIM-521). Decoding through `map[string]any`
	// destroys the number token the SQL half of the same boundary compares
	// against, and no float formatting brings it back, so the two halves of
	// the incarnation scope answered differently for the same row.
	//
	// Set ONLY where an Incarnation is assembled from a query. A value built
	// in Go (the create path) leaves it nil, and a trait condition then fails
	// closed — correct: such a value has no traits in the registry yet.
	//
	// Not serialized: this is the display field's own bytes, and an API
	// response carries `traits` once, decoded.
	TraitsRaw []byte `json:"-"`

	// ApplyingApplyID — apply_id of the currently running run (ADR-068 §A1,
	// column applying_apply_id, ADR-027 m-S1). Non-null exactly while a run is
	// in progress (written in lockRun, cleared on terminal); nil = no run in
	// progress. Read source for the incarnation→live-run link used by SSE
	// subscription (the UI doesn't guess via /v1/runs).
	ApplyingApplyID *string `json:"applying_apply_id,omitempty"`
}

// HistoryEntry — a `state_history` record (snapshot per change, ADR-009 / ADR-019).
type HistoryEntry struct {
	HistoryID    string         `json:"history_id"`
	Scenario     string         `json:"scenario"`
	StateBefore  map[string]any `json:"state_before"`
	StateAfter   map[string]any `json:"state_after"`
	ChangedByAID *string        `json:"changed_by_aid,omitempty"`
	ApplyID      string         `json:"apply_id"`
	At           time.Time      `json:"at"`
}
