// Package applyrun — registry of apply runs in Postgres (table `apply_runs`,
// migration 018) for the M2.x scenario-runner.
//
// Purpose — correlate `apply_id` with an incarnation/scenario: when Keeper
// receives a `RunResult` from a Soul, the proto alone doesn't say which
// incarnation the run belongs to. scenario-runner writes a row when it
// dispatches `ApplyRequest` ([Insert]); the RunResult handler looks it up by
// `(apply_id, sid)` ([SelectIncarnationByApplyID]) and commits state to the
// right incarnation.
//
// apply_id model A (PM decision): one `apply_id` per scenario, a distinct
// `sid` per fan-out host → composite PK `(apply_id, sid)`.
package applyrun

import "time"

// Status — apply_runs row status. Closed enum (PM decision 1), matches the
// `apply_runs_status_valid` CHECK constraint from migration 018.
type Status string

const (
	// StatusPlanned — task not yet claimed by an Acolyte via Ward-claim
	// (work-queue entry, ADR-027). scenario-runner writes it on dispatch;
	// Acolyte claims planned tasks via [ClaimNext]. In [ValidStatus] since
	// Phase 1.
	StatusPlanned Status = "planned"
	// StatusClaimed — Ward claimed by an Acolyte ([ClaimNext]); the task is
	// resolved/rendered just-in-time before moving to `dispatched` via
	// [MarkDispatched] (ADR-027 amend). In [ValidStatus] since Phase 1.
	StatusClaimed Status = "claimed"
	// StatusRunning — run started (row inserted on dispatch), no terminal
	// RunResult yet. Vestigial after the GATE-1 recovery redesign (ADR-027
	// amend): the Acolyte flow no longer writes it (claimed →
	// [StatusDispatched] → terminal), but the value stays valid for
	// wire-compat (old/ad-hoc rows) — see [ValidStatus].
	StatusRunning Status = "running"
	// StatusDispatched — task handed off to a Soul (lifecycle phase, ADR-027
	// amend S2). claimed → dispatched is marked ATOMICALLY BEFORE SendApply
	// ([MarkDispatched]) — a deliver-once intent marker. Once a row is
	// dispatched, recovery-reclaim does NOT touch it (reclaim is scoped to
	// `status='claimed'`, S4): after handoff the Soul owns the run, so a
	// re-claim would mean a double apply. The terminal state arrives on a
	// dispatched row via RunResult.
	StatusDispatched Status = "dispatched"
	// StatusSuccess — RunResult with status=SUCCESS.
	StatusSuccess Status = "success"
	// StatusFailed — RunResult with status=FAILED / ERROR_LOCKED.
	StatusFailed Status = "failed"
	// StatusCancelled — RunResult with status=CANCELLED.
	StatusCancelled Status = "cancelled"
	// StatusOrphaned — Soul-reconcile terminal (ADR-027(g), S6): a row stayed
	// in `dispatched` after "both Keeper and Soul died post-handoff", and on
	// reconnect the Soul did NOT declare this apply_id in [WardRoster] (no
	// in-flight run physically exists — e.g. the Soul process was restarted).
	// No RunResult will ever arrive for it — [OrphanDispatched] terminalizes
	// it to `orphaned`. The barrier classifies orphaned as a terminal
	// non-success (incarnation → error_locked). Added by migration 044.
	StatusOrphaned Status = "orphaned"
	// StatusNoMatch — "host got nothing to do" terminal (FINDING-01, option
	// (b)). The Acolyte path writes a planned task for EVERY roster host of an
	// incarnation BEFORE per-host `on:`/`where:` resolution (resolution
	// happens later, at claim time). A host left with 0 tasks after
	// `on:`/`where:` closes as `no_match` (NOT `success`): apply_runs no
	// longer over-reports "success" where nothing was actually applied. The
	// barrier classifies no_match as a TERMINAL non-failure (benign, like
	// success): a run with targeted successes + non-targeted no_match drives
	// the incarnation to ready (NOT error_locked). Carries finished_at —
	// subject to retention-purge like other terminals. Added by migration 045.
	StatusNoMatch Status = "no_match"
)

// ValidStatus — checks a status allowed by the CRUD layer ([Insert] /
// [UpdateStatus]). Matches the full set of the apply_runs_status_valid CHECK
// after migration 045 ([StatusNoMatch], FINDING-01 option (b); earlier 044 —
// [StatusOrphaned], Soul-reconcile ADR-027(g)). [StatusPlanned] /
// [StatusClaimed] were added in Phase 1 (claim logic, ADR-027):
// scenario-runner writes `planned` on dispatch, Acolyte claims into
// `claimed` ([ClaimNext]). [StatusDispatched] was added by the GATE-1
// recovery redesign (ADR-027 amend, S2): Acolyte moves claimed → dispatched
// ([MarkDispatched]) BEFORE SendApply. [StatusRunning] stays valid
// (vestigial — the Acolyte flow no longer writes it, but wire-compat is
// preserved).
func ValidStatus(s Status) bool {
	switch s {
	case StatusPlanned, StatusClaimed, StatusRunning, StatusDispatched, StatusSuccess, StatusFailed, StatusCancelled, StatusOrphaned, StatusNoMatch:
		return true
	}
	return false
}

// ApplyRun — runtime representation of an `apply_runs` registry row.
//
// TaskIdx / ErrorSummary / FinishedAt / StartedByAID — nullable columns
// (PM decision 2/3): TaskIdx is unknown at dispatch time; FinishedAt/
// ErrorSummary are filled by the terminal RunResult; StartedByAID is `NULL`
// for runs initiated by a Soul without an Archon identity.
//
// ClaimByKID / ClaimAt / ClaimExpiresAt / Attempt — Ward-claim columns
// (ADR-027, migration 025). Phase 0: added to the struct to stay in sync
// with the schema (a full row representation), but nothing writes or reads
// them yet — the CRUD layer (Insert/SelectByApplyID) doesn't map them. The
// claim logic that fills these fields is Phase 1.
//
// Recipe — render instructions for the Acolyte's just-in-time task render at
// claim time (ADR-027(c)(f), nullable recipe column, migration 029). nil for
// rows from the old Insert(running) path — a recipe is only carried by a
// planned task awaiting claim. Parsed from/to jsonb via [UnmarshalRecipe] /
// [MarshalRecipe] at the CRUD layer boundary (symmetric with the nullable
// TaskIdx / StartedByAID pointers). Writing the recipe on dispatch is Phase
// 1.4.2, reading it at claim is Phase 1.4.3.
type ApplyRun struct {
	ApplyID         string `json:"apply_id"`
	SID             string `json:"sid"`
	IncarnationName string `json:"incarnation_name"`
	Scenario        string `json:"scenario"`
	TaskIdx         *int   `json:"task_idx,omitempty"`
	Status          Status `json:"status"`

	// Passage — Passage index for staged-render (ADR-056, S3): a run's single
	// host maps to N apply_runs rows (one per Passage), forming the PK
	// (apply_id, sid, passage) since migration 078. Zero-value 0 = single
	// Passage — an N=1 run (no register dependencies) writes a single
	// passage=0 row, behavior BIT-FOR-BIT identical to pre-staged-render.
	// Insert writes this field explicitly.
	Passage int `json:"passage"`

	ErrorSummary *string    `json:"error_summary,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	StartedByAID *string    `json:"started_by_aid,omitempty"`

	ClaimByKID     *string    `json:"claim_by_kid,omitempty"`
	ClaimAt        *time.Time `json:"claim_at,omitempty"`
	ClaimExpiresAt *time.Time `json:"claim_expires_at,omitempty"`
	Attempt        int        `json:"attempt"`

	Recipe *Recipe `json:"recipe,omitempty"`
}

// ActiveApply — one entry of the set of apply runs a Soul is tracking, from
// WardRoster (Soul-reconcile, ADR-027(g), S6). Domain counterpart of the
// keeperv1.ActiveApply proto message; the gRPC handler maps proto → domain so
// the CRUD layer (this package) stays independent of proto generation (like
// the rest of applyrun). [OrphanDispatched] only reads ApplyID; Attempt is
// kept for a future epoch mismatch check (in MVP, presence of apply_id in
// the set protects the row from orphaning regardless of attempt).
type ActiveApply struct {
	ApplyID string
	Attempt int32
}
