// Package incarnation — runtime instance of a Service in Postgres under ADR-009.
//
// M0.6c-1: types + CRUD (Create / SelectByID / SelectAll / HistorySelectByName).
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

// IDPattern — canonical form of an incarnation id: kebab-case, starts
// with a letter/digit, length 1..63. Same as CHECK incarnation_id_format
// in migration 118 (incarnation_name_format before it).
//
// The form is UNCHANGED by the `name` -> `id` rename ([ADR-0085], NIM-729): the
// identifier moved spelling, not grammar. [ADR-0085] adopts THIS pattern as the
// one grammar for every registry, so here alone the rename and the unification
// coincide -- and even here the constant's VALUE does not move.
const IDPattern = `^[a-z0-9][a-z0-9-]{0,62}$`

// ReasonMaxLen — upper bound on the free-text confirmation reason for
// unlock / rerun-last. Single source for the huma maxLength tag and the
// runtime validator (UnlockTyped / RerunLastTyped 422 on
// `len(reason) > ReasonMaxLen`). The lower bound (non-empty) is a separate
// check `reason == ""`.
const ReasonMaxLen = 500

var idRe = regexp.MustCompile(IDPattern)

// ValidID reports whether id matches the canonical form.
func ValidID(id string) bool { return idRe.MatchString(id) }

// Incarnation — runtime representation of an `incarnation` registry row.
//
// jsonb fields (`State` / `StatusDetails`) are `map[string]any` for
// freeform data; typing for a concrete service / scenario lives in their
// manifests, not in this layer.
type Incarnation struct {
	// ID is the immutable identifier ([ADR-0085]): kebab code word, set once at
	// creation, the PRIMARY KEY of `incarnation`. There is no rename operation,
	// and this is the entity where that matters most -- see Label below for the
	// three derivations it feeds.
	//
	// The CEL root agrees since NIM-730: the map built in
	// keeper/internal/render/dispatch.go is keyed `"id"`, and the retired
	// `incarnation.name` survives only as an alias added at the activation
	// (shared/cel.Vars.incarnationRoot) for the length of the [ADR-0085]
	// compatibility window every service repository needs.
	ID string `json:"id"`
	// Label is the display caption ([ADR-0085]): free text, mutable via
	// SetLabel, not unique, optional. nil means the column is NULL and a
	// consumer shows ID instead.
	//
	// It participates in NOTHING derived, and this is the entity where that
	// matters most. ID is segment 3 of every derived secret path
	// (`<mount>/<service>/<incarnation>/<state-field>[/<key>]`, [ADR-0083] §1),
	// substituted verbatim with no case folding; it is the value of the RBAC
	// `incarnation=` scope dimension; and it is the CEL root `incarnation.id`
	// (NIM-730 moved that spelling; `incarnation.name` is a window alias).
	// Label reaches none of the three — deliberately, and guarded by
	// keeper/internal/render/label_invariant_guard_test.go and
	// keeper/internal/servicevars/label_invariant_guard_test.go (the CEL roots,
	// all three environments) and
	// keeper/internal/api/handlers/label_invariant_guard_test.go (the RBAC scope
	// value, and the derived secret path at the reveal route).
	//
	// In particular it is NOT projected into CEL: `incarnation.label` does not
	// resolve, because the CEL root is built from [render.IncarnationMeta], which
	// carries the identifier and no caption.
	//
	// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md
	// [ADR-0083]: ../../../docs/adr/0083-declared-secret-state-fields.md
	Label              *string        `json:"label,omitempty"`
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
