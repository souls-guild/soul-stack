// Package cadence — Cadence schedule registry in Postgres (table `cadences`,
// migration 066) per ADR-046.
//
// Cadence is a schedule that spawns a regular Voyage run on a timer. It lives
// independently of runs: when due, it spawns a NEW Voyage (Insert into
// `voyages`/`voyage_targets` with back-link `cadence_id`), preserving the
// Voyage invariant "one Voyage = one run" (new rows, not resurrection).
// Relationship is "parent Cadence → child Voyages" (one-to-many). Cadence
// stores the run "recipe" (same field set as VoyageCreateRequest, ADR-043) +
// a repeat rule ([ScheduleKind] interval XOR cron) + an overlap policy
// ([OverlapPolicy]).
//
// Package scope (S1): read/write CRUD against PG + validation. The scheduler
// trigger (Reaper rule spawn_due_cadence, action `spawn`), next_run_at
// recalculation (interval/cron), and overlap_policy execution are S2/S3,
// living elsewhere. API `/v1/cadences` is S4, UI is S5.
//
// Style mirrors `keeper/internal/voyage` (same CRUD pattern: insertSQL/
// selectColumns/scan, sentinel errors, enum constants + Valid checks).
package cadence

import (
	"encoding/json"
	"errors"
	"time"
)

// ScheduleKind is the repeat rule variant. Closed enum, matches CHECK
// `cadences_schedule_kind_valid` from migration 066.
type ScheduleKind string

const (
	// ScheduleKindInterval repeats every N seconds. Requires non-empty
	// IntervalSeconds; CronExpr must be empty (CHECK
	// `cadences_schedule_consistency`).
	ScheduleKindInterval ScheduleKind = "interval"
	// ScheduleKindCron repeats on a standard 5-field cron expression.
	// Requires non-empty CronExpr; IntervalSeconds must be empty. Cron
	// parsing and next_run_at recalculation are S2 (S1 stores the string
	// as-is).
	ScheduleKindCron ScheduleKind = "cron"
)

// OverlapPolicy is the behavior when the next spawn is due but the
// previously spawned Voyage is not yet terminal. Closed enum, matches CHECK
// `cadences_overlap_policy_valid` from migration 066. Policy execution is S3.
type OverlapPolicy string

const (
	// OverlapPolicySkip skips the spawn (next_run_at is still
	// recalculated); guards against pile-up when the period is shorter
	// than the run duration.
	OverlapPolicySkip OverlapPolicy = "skip"
	// OverlapPolicyQueue defers the spawn until the previous child goes
	// terminal (strict sequencing without dropping runs).
	OverlapPolicyQueue OverlapPolicy = "queue"
	// OverlapPolicyParallel spawns regardless of previous state
	// (overlapping runs are allowed; limits live at the Voyage/Acolyte
	// layer).
	OverlapPolicyParallel OverlapPolicy = "parallel"
)

// Kind is the spawned Voyage run mode. Matches voyage.Kind values / CHECK
// `cadences_kind_valid` from migration 066. Duplicated here (not imported
// from voyage) so cadence validation doesn't depend on the voyage package:
// S1 validates the recipe locally, spawning (voyage.Insert) is the S2
// scheduler's job.
type Kind string

const (
	// KindScenario spawns a Voyage applying a named scenario to a set of
	// incarnations. Requires non-empty ScenarioName.
	KindScenario Kind = "scenario"
	// KindCommand spawns a Voyage running a whitelisted module across a
	// set of hosts. Requires non-empty Module.
	KindCommand Kind = "command"
)

// BatchMode is the recipe's batching mode (parity voyage.BatchMode). Closed
// enum, matches CHECK `cadences_batch_mode_valid` from migration 066. NULL ⇒
// barrier (resolved by the orchestrator at spawn time, S2).
type BatchMode string

const (
	// BatchModeBarrier runs sequential Legs with a barrier between batches.
	BatchModeBarrier BatchMode = "barrier"
	// BatchModeWindow is a full sliding window (width = concurrency).
	BatchModeWindow BatchMode = "window"
)

// OnFailure is the transition policy on failure (parity voyage.OnFailure).
// Matches CHECK `cadences_on_failure_valid` from migration 066.
type OnFailure string

const (
	// OnFailureContinue records the failure; the remaining Legs continue.
	OnFailureContinue OnFailure = "continue"
	// OnFailureAbort stops advancing to the next Leg on failure.
	OnFailureAbort OnFailure = "abort"
)

// Cadence is the runtime representation of a `cadences` row. Full
// projection: run recipe + repeat rule + overlap policy + computed timings.
//
// Nullable recipe fields are pointers/raw bytes (resolved by the
// orchestrator at spawn time, not by CRUD). IntervalSeconds/CronExpr are
// mutually exclusive per ScheduleKind (CHECK
// `cadences_schedule_consistency`); ScenarioName/Module are mutually
// exclusive per Kind (CHECK `cadences_kind_payload_consistency`).
type Cadence struct {
	ID      string
	Name    string
	Enabled bool

	// Repeat rule.
	ScheduleKind    ScheduleKind
	IntervalSeconds *int    // for ScheduleKindInterval; nil for cron.
	CronExpr        *string // for ScheduleKindCron; nil for interval.
	OverlapPolicy   OverlapPolicy

	// Run recipe (same field set as VoyageCreateRequest, ADR-043).
	Kind          Kind
	ScenarioName  *string
	Module        *string
	Target        json.RawMessage // selection from the creator's RBAC scope (NOT NULL)
	Input         []byte          // jsonb (caller serializes input once)
	BatchMode     *BatchMode      // nil ⇒ barrier (resolved at spawn time).
	BatchSize     *int            // XOR with BatchPercent (handler invariant).
	BatchPercent  *int            // % of scope; nil ⇒ BatchSize is set / whole run is one Leg.
	Concurrency   *int
	FailThreshold *int // absolute failure count threshold → stop; nil ⇒ no threshold.
	// FailThresholdPercent is the failure threshold as a percent of spawn
	// scope (XOR with FailThreshold, handler invariant). Stored as a
	// column (unlike Voyage, where max_failures="N%" resolves to an
	// absolute at create-scope) because Cadence scope is unknown at
	// creation — it resolves at spawn time against len(resolved) in
	// BuildVoyage, symmetric with BatchPercent. nil ⇒ FailThreshold is
	// set / no threshold.
	FailThresholdPercent *int
	InterBatchInterval   *time.Duration // pause between Legs (barrier).
	InterUnitInterval    *time.Duration // per-unit pause in window.
	RequireAlive         *bool          // liveness presence filter at scope resolution; nil ⇒ false.
	OnFailure            *OnFailure

	// Computed timings (scheduler is S2; S1 CRUD reads/writes as-is).
	NextRunAt *time.Time
	LastRunAt *time.Time

	CreatedByAID string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Sentinel errors for the CRUD layer (parity voyage).
//   - ErrCadenceNotFound — no row for the requested id.
//   - ErrCadenceExists   — UNIQUE on PK (id): repeat Insert of the same ULID
//     (caller programming error).
var (
	ErrCadenceNotFound = errors.New("cadence: not found")
	ErrCadenceExists   = errors.New("cadence: id already exists")
)

// ValidScheduleKind reports whether the kind is a member of the
// [ScheduleKind] enum.
func ValidScheduleKind(k ScheduleKind) bool {
	switch k {
	case ScheduleKindInterval, ScheduleKindCron:
		return true
	}
	return false
}

// ValidOverlapPolicy reports whether the policy is a member of the
// [OverlapPolicy] enum.
func ValidOverlapPolicy(p OverlapPolicy) bool {
	switch p {
	case OverlapPolicySkip, OverlapPolicyQueue, OverlapPolicyParallel:
		return true
	}
	return false
}

// ValidKind reports whether kind is a member of the [Kind] enum.
func ValidKind(k Kind) bool {
	switch k {
	case KindScenario, KindCommand:
		return true
	}
	return false
}

// ValidBatchMode reports whether the mode is a member of the [BatchMode]
// enum.
func ValidBatchMode(m BatchMode) bool {
	switch m {
	case BatchModeBarrier, BatchModeWindow:
		return true
	}
	return false
}

// ValidOnFailure is the matching check for [OnFailure].
func ValidOnFailure(p OnFailure) bool {
	switch p {
	case OnFailureContinue, OnFailureAbort:
		return true
	}
	return false
}
