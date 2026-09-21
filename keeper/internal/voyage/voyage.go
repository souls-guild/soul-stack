// Package voyage — registry of Voyage runs in Postgres (tables `voyages` +
// `voyage_targets`, migration 059) per ADR-043.
//
// Voyage — unified batch run, absorbing Tide (kind=scenario) +
// ErrandRun (kind=command). Discriminator [Kind]:
//
//   - [KindScenario] — apply named scenario to incarnation set
//     (absorbs Tide + classic scenario-run); batch (Leg) = N incarnations,
//     per-incarnation state-commit (B1, ADR-043 §3).
//   - [KindCommand] — execute whitelisted module on host set (absorbs
//     ErrandRun); batch (Leg) = N hosts, `incarnation.state` untouched.
//
// Batch unit — Leg (one «segment»), identified by
// [VoyageTarget.BatchIndex]. Per-target progress stored in `voyage_targets`.
//
// Failover-resilient via PG-based claim+lease (parity Tide / ErrandRun,
// ADR-027(d)): pending → claimed_by_kid + claim_expires_at → running; stale
// claim returned by Reaper-rule back to pending for reclaim by another
// Keeper instance (rollout — post-S1).
//
// Package scope: read/write CRUD + claim/lease/finalize. Orchestrator
// (VoyageWorker) lives separately in `keeper/internal/voyageorch/`. At S1 worker —
// NOOP execution (config-gated OFF by default): real scenario/command
// run wired in S2/S3.
//
// TODO(post-S1): claim+lease helpers duplicate tide/errandrun (architect-
// decision γ 2026-05-27 to extract to shared `claimlease/` deferred until review
// to avoid premature abstraction). Current copy-pattern — explicit place for
// future refactoring.
package voyage

import (
	"encoding/json"
	"errors"
	"time"
)

// Kind — Voyage run mode. Closed enum, matches CHECK `voyages_kind_valid`
// of migration 059.
type Kind string

const (
	// KindScenario — apply named scenario to incarnation set (absorbs
	// Tide + classic scenario-run). Requires non-empty ScenarioName (CHECK
	// `voyages_kind_payload_consistency`).
	KindScenario Kind = "scenario"
	// KindCommand — execute whitelisted module on host set (absorbs
	// ErrandRun). Requires non-empty Module.
	KindCommand Kind = "command"
)

// Status — row status of `voyages`. Closed enum, matches CHECK
// `voyages_status_valid` of migration 059.
type Status string

const (
	// StatusScheduled — deferred start (schedule_at in future, S4). Worker does not
	// pick up scheduled until schedule_at arrives; not used at S1
	// (scheduler — S4), but value reserved in CHECK.
	StatusScheduled Status = "scheduled"
	// StatusPending — Voyage created, awaiting VoyageWorker pickup. Pickup
	// queue: ClaimNext per FIFO `created_at` (FOR UPDATE SKIP LOCKED).
	StatusPending Status = "pending"
	// StatusRunning — Voyage picked up by VoyageWorker (claim CAS-UPDATE
	// pending→running), claim fields NOT NULL (CHECK
	// `voyages_running_claim_consistency`). renewal-goroutine holds lease.
	StatusRunning Status = "running"
	// StatusSucceeded — all Legs / targets completed successfully. Terminal.
	StatusSucceeded Status = "succeeded"
	// StatusFailed — run failed entirely. Terminal.
	StatusFailed Status = "failed"
	// StatusPartialFailed — some targets succeeded, some failed/cancelled. Terminal.
	StatusPartialFailed Status = "partial_failed"
	// StatusCancelled — Voyage cancelled by operator. Terminal.
	StatusCancelled Status = "cancelled"
)

// BatchMode — run batching mode (ADR-043 amendment 2026-06-01). Closed enum,
// matches CHECK `voyages_batch_mode_valid` of migration 064. NULL column
// treated as [BatchModeBarrier] (forward-compat: runs without field work
// as before).
type BatchMode string

const (
	// BatchModeBarrier — run split into sequential Legs (batch batch_size units),
	// barrier between Legs (await all units of Leg terminal). concurrency =
	// parallelism WITHIN Leg. Default behavior (NULL column ⇒ barrier).
	BatchModeBarrier BatchMode = "barrier"
	// BatchModeWindow — full sliding window: pool of workers pulls units from
	// shared run queue, one returns → next starts, constantly maintains
	// concurrency active. No barriers between batches; batch_size unused (window
	// width = concurrency). batch_index = 0 for all units.
	BatchModeWindow BatchMode = "window"
)

// ResolveBatchMode returns effective mode: NULL/empty → barrier
// (forward-compat). Used by orchestrator when branching executor.
func ResolveBatchMode(m *BatchMode) BatchMode {
	if m == nil || *m == "" {
		return BatchModeBarrier
	}
	return *m
}

// ValidBatchMode reports whether mode is in [BatchMode] enum.
func ValidBatchMode(m BatchMode) bool {
	switch m {
	case BatchModeBarrier, BatchModeWindow:
		return true
	}
	return false
}

// ResolveFailThreshold returns effective threshold for number of failures at which
// run stops (ADR-043 amendment 2026-06-01). Semantics generalize existing abort-gate:
//   - fail_threshold set → its value (N>1 — intermediate tolerance);
//   - not set, OnFailure=abort → 1 (first failure → stop, backcompat);
//   - not set, OnFailure=continue/nil → 0 (no threshold, run to end).
//
// Return 0 means «no threshold». Used by orchestrator: reached
// failCount >= threshold (threshold>0) → cease spawning new units.
func ResolveFailThreshold(threshold *int, policy *OnFailure) int {
	if threshold != nil && *threshold > 0 {
		return *threshold
	}
	if policy != nil && *policy == OnFailureAbort {
		return 1
	}
	return 0
}

// ResolveRequireAlive returns effective presence-filter value: NULL →
// false (no filter, forward-compat), ADR-043 amendment.
func ResolveRequireAlive(b *bool) bool {
	return b != nil && *b
}

// OnFailure — policy for transitioning to next Leg on failure. Closed set,
// matches CHECK `voyages_on_failure_valid`.
type OnFailure string

const (
	// OnFailureContinue — target failures counted, remaining Legs continue;
	// terminal — partial_failed (if any failure) or succeeded.
	OnFailureContinue OnFailure = "continue"
	// OnFailureAbort — failure → cessation of transition to next Leg → terminal.
	OnFailureAbort OnFailure = "abort"
)

// TargetKind — type of run unit in `voyage_targets`. Closed enum, matches
// CHECK `voyage_targets_target_kind_valid`.
type TargetKind string

const (
	// TargetKindIncarnation — unit for kind=scenario (one incarnation =
	// full scenario-run, back-link ApplyID to apply_runs).
	TargetKindIncarnation TargetKind = "incarnation"
	// TargetKindSID — unit for kind=command (one host, back-link ErrandID
	// to errands).
	TargetKindSID TargetKind = "sid"
)

// TargetStatus — row status of `voyage_targets`. Closed enum, matches CHECK
// `voyage_targets_status_valid`.
type TargetStatus string

const (
	// TargetStatusAwaiting — target in queue of its Leg, not yet started.
	TargetStatusAwaiting TargetStatus = "awaiting"
	// TargetStatusRunning — spawned child run for target (apply_run /
	// errand), awaiting its terminal.
	TargetStatusRunning TargetStatus = "running"
	// TargetStatusSucceeded — child run of target completed successfully.
	TargetStatusSucceeded TargetStatus = "succeeded"
	// TargetStatusFailed — child run of target failed.
	TargetStatusFailed TargetStatus = "failed"
	// TargetStatusCancelled — target cancelled (cancel-all or on_failure: abort).
	TargetStatusCancelled TargetStatus = "cancelled"
	// TargetStatusNoMatch — target yielded no match on resolve
	// (parity apply_runs `no_match`): target incarnation/host outside actual
	// scope at execution time.
	TargetStatusNoMatch TargetStatus = "no_match"
)

// Voyage — runtime representation of `voyages` row. Full projection (including
// claim columns and summary): unified read model for CRUD calls.
//
// Nullable fields — pointers/raw bytes. ScenarioName/Module mutually exclusive per
// Kind (CHECK `voyages_kind_payload_consistency`). BatchSize/Concurrency
// nullable — absence = «entire run in one Leg / default degree»
// (resolved by orchestrator, not CRUD).
type Voyage struct {
	VoyageID           string
	Kind               Kind
	ScenarioName       *string
	Module             *string
	Input              []byte // jsonb (caller serializes input once)
	TargetResolved     json.RawMessage
	TargetOrigin       json.RawMessage // optional declarative origin (NULL permitted)
	BatchSize          *int
	BatchPercent       *int // % of scope (XOR with BatchSize), ADR-043 amendment. nil ⇒ BatchSize set / entire run in one Leg.
	Concurrency        *int
	BatchMode          *BatchMode // nil ⇒ barrier (forward-compat), ADR-043 amendment.
	DryRun             bool
	ScheduleAt         *time.Time
	InterBatchInterval *time.Duration
	InterUnitInterval  *time.Duration // per-unit pause in window (parity InterBatchInterval), ADR-043 amendment.
	FailThreshold      *int           // threshold of absolute number of failures → stop. nil ⇒ no threshold, ADR-043 amendment.
	RequireAlive       *bool          // presence-filter of alive on scope resolve. nil ⇒ false, ADR-043 amendment.
	OnFailure          *OnFailure
	CadenceID          *string // back-link to spawning Cadence (ADR-046 §2). nil ⇒ manual run; populated ⇒ spawn from Cadence (S2).
	TotalBatches       int
	CurrentBatchIndex  int
	Status             Status
	ClaimedByKID       *string
	LastRenewedAt      *time.Time
	ClaimExpiresAt     *time.Time
	Attempt            int
	StartedByAID       string
	CreatedAt          time.Time
	StartedAt          *time.Time
	FinishedAt         *time.Time
	Summary            *Summary
}

// VoyageTarget — runtime representation of `voyage_targets` row (run unit,
// Leg split). ApplyID (kind=scenario) and ErrandID (kind=command) — back-links
// to child run, populated by orchestrator on spawn (S2/S3).
type VoyageTarget struct {
	VoyageID   string
	TargetKind TargetKind
	TargetID   string
	BatchIndex int
	Status     TargetStatus
	ApplyID    *string
	ErrandID   *string
	FinishedAt *time.Time
}

// Summary — aggregated run summary, folded into jsonb column `summary`
// on Voyage finalization.
type Summary struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	NoMatch   int `json:"no_match,omitempty"`
}

// Sentinel errors of CRUD layer.
//   - ErrVoyageNotFound  — no row for requested voyage_id.
//   - ErrLeaseLost       — CAS-renewal / finalize returned 0 rows: lease no longer
//     ours (another Keeper picked up via Reaper-reclaim+claim). VoyageWorker,
//     receiving ErrLeaseLost, immediately ceases work.
//   - ErrInvalidStatus   — attempt to transition to incompatible status
//     (caller programming error; CRUD does not write terminal to terminal).
//   - ErrVoyageExists     — UNIQUE on PK (voyage_id) — duplicate Insert of one
//     ULID (caller programming error).
var (
	ErrVoyageNotFound = errors.New("voyage: not found")
	ErrLeaseLost      = errors.New("voyage: lease lost (CAS returned 0 rows)")
	ErrInvalidStatus  = errors.New("voyage: invalid status transition")
	ErrVoyageExists   = errors.New("voyage: voyage_id already exists")
)

// ValidKind reports whether kind is in [Kind] enum.
func ValidKind(k Kind) bool {
	switch k {
	case KindScenario, KindCommand:
		return true
	}
	return false
}

// ValidStatus reports whether status is in [Status] enum.
func ValidStatus(s Status) bool {
	switch s {
	case StatusScheduled, StatusPending, StatusRunning,
		StatusSucceeded, StatusFailed, StatusPartialFailed, StatusCancelled:
		return true
	}
	return false
}

// ValidOnFailure — paired check for OnFailure.
func ValidOnFailure(p OnFailure) bool {
	switch p {
	case OnFailureContinue, OnFailureAbort:
		return true
	}
	return false
}

// ValidTargetKind — check for TargetKind.
func ValidTargetKind(k TargetKind) bool {
	switch k {
	case TargetKindIncarnation, TargetKindSID:
		return true
	}
	return false
}

// IsTerminal reports whether status is terminal (finalized).
// finished_at for such rows always NOT NULL.
func IsTerminal(s Status) bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusPartialFailed, StatusCancelled:
		return true
	}
	return false
}

// marshalSummary serializes Summary to jsonb bytes. nil → nil (NULL in DB).
func marshalSummary(s *Summary) ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	return json.Marshal(s)
}

// unmarshalSummary parses jsonb bytes to Summary. Empty/nil input → nil.
func unmarshalSummary(raw []byte) (*Summary, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
