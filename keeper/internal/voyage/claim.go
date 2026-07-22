package voyage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/pgutil"
)

// claimReturning — RETURNING list for ClaimNext. Matches the order of
// [selectColumns] (including EXTRACT EPOCH for inter_batch_interval), so the shared
// [scanVoyage] can read the claimed row.
const claimReturning = `
    voyage_id, kind, scenario_name, module, input,
    target_resolved, target_origin,
    batch_size, concurrency, batch_mode, dry_run,
    schedule_at, EXTRACT(EPOCH FROM inter_batch_interval)::float8, on_failure,
    total_batches, current_batch_index, status,
    claimed_by_kid, last_renewed_at, claim_expires_at, attempt,
    started_by_aid, created_at, started_at, finished_at, summary,
    batch_percent, fail_threshold, EXTRACT(EPOCH FROM inter_unit_interval)::float8, require_alive,
    cadence_id
`

// claimNextSQL — atomically claims a single claimable Voyage. Work-queue idiom,
// parity with tide/errandrun.ClaimNext: the inner SELECT FOR UPDATE SKIP LOCKED
// locks a single row and skips rows locked by competitors — two
// VoyageWorkers from different instances will never claim the same Voyage. The outer
// UPDATE transitions claimable → running, sets the owner/lease, sets
// started_at on first claim, and increments attempt (fencing epoch, ADR-027(g)).
//
// Claimable = `pending` OR (`scheduled` AND schedule_at <= NOW()): a deferred
// start (S4) becomes claimable as soon as its time arrives. A scheduled row with
// schedule_at in the future is ignored (waits its turn). After claiming, the status
// is always `running` — no separate branch is needed for the original status.
//
//	$1 kid   — KID of the owning Keeper
//	$2 lease — interval until claim_expires_at (NOW() + $2)
const claimNextSQL = `
UPDATE voyages AS v
SET status           = 'running',
    claimed_by_kid   = $1,
    last_renewed_at  = NOW(),
    claim_expires_at = NOW() + $2::interval,
    started_at       = COALESCE(v.started_at, NOW()),
    attempt          = v.attempt + 1
WHERE v.voyage_id = (
    SELECT c.voyage_id
    FROM voyages AS c
    WHERE c.status = 'pending'
       OR (c.status = 'scheduled' AND c.schedule_at <= NOW())
    ORDER BY c.created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING ` + claimReturning

// ClaimNext atomically claims a single claimable Voyage: (pending OR
// scheduled-and-due) → running, claim fields + attempt+1 + started_at. FIFO
// by created_at. Returns nil with no error if there are no claimable Voyages
// (the caller sleeps PollInterval and retries).
func ClaimNext(ctx context.Context, db ExecQueryRower, kid string, leaseTTL time.Duration) (*Voyage, error) {
	if kid == "" {
		return nil, fmt.Errorf("voyage: empty kid")
	}
	if leaseTTL <= 0 {
		return nil, fmt.Errorf("voyage: non-positive lease %s", leaseTTL)
	}

	row := db.QueryRow(ctx, claimNextSQL, kid, pgutil.Interval(leaseTTL))
	v, err := scanVoyage(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("voyage: claim next: %w", err)
	}
	return v, nil
}

const renewLeaseSQL = `
UPDATE voyages
SET claim_expires_at = NOW() + $3::interval,
    last_renewed_at  = NOW()
WHERE voyage_id      = $1
  AND claimed_by_kid = $2
  AND claim_expires_at > NOW()
RETURNING voyage_id
`

// RenewLease CAS-extends the current lease. UPDATE condition: the row is owned by this
// KID AND the claim hasn't expired yet. 0 rows → the lease is no longer ours ([ErrLeaseLost]):
// it expired → Reaper returned it to pending → another Keeper picked it up. The caller (renewLoop)
// closes the leaseLost channel — executeVoyage does a graceful exit without finalizing.
func RenewLease(ctx context.Context, db ExecQueryRower, id, kid string, leaseTTL time.Duration) error {
	if id == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if kid == "" {
		return fmt.Errorf("voyage: empty kid")
	}
	if leaseTTL <= 0 {
		return fmt.Errorf("voyage: non-positive lease %s", leaseTTL)
	}

	var returned string
	err := db.QueryRow(ctx, renewLeaseSQL, id, kid, pgutil.Interval(leaseTTL)).Scan(&returned)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		return fmt.Errorf("voyage: renew lease: %w", err)
	}
	return nil
}

const updateBatchProgressSQL = `
UPDATE voyages
SET current_batch_index = $4
WHERE voyage_id      = $1
  AND claimed_by_kid = $2
  AND attempt        = $3
`

// UpdateBatchProgress advances a Voyage's current_batch_index to the number of
// COMPLETED batches (Legs) — a UI indicator "Batch N/total". Called
// by the orchestrator after each Leg finishes, with completedBatches = legIdx+1
// (progression 0→1→…→total_batches; terminal == total_batches = 100%).
//
// Ownership guard in WHERE (parity with [RenewLease]/[VerifyOwnership]): only writes to
// OUR OWN claim — claimed_by_kid + attempt epoch. After a Reaper reclaim (attempt++,
// a different owner) the UPDATE won't match — 0 rows, the other owner's current_batch_index
// is left untouched.
//
// Best-effort: 0 rows (lease lost / reclaimed) and I/O errors are returned
// to the caller, but progress is just a UI hint; the source of truth for run progress is
// voyage_targets. The caller logs a warning and continues the Leg loop, without failing the run.
func UpdateBatchProgress(ctx context.Context, db ExecQueryRower, id, kid string, attempt, completedBatches int) error {
	if id == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if kid == "" {
		return fmt.Errorf("voyage: empty kid")
	}
	if _, err := db.Exec(ctx, updateBatchProgressSQL, id, kid, attempt, completedBatches); err != nil {
		return fmt.Errorf("voyage: update batch progress: %w", err)
	}
	return nil
}

const releaseLeaseSQL = `
UPDATE voyages
SET status           = 'pending',
    claimed_by_kid   = NULL,
    claim_expires_at = NULL,
    last_renewed_at  = NULL
WHERE voyage_id      = $1
  AND claimed_by_kid = $2
  AND status         = 'running'
`

// ReleaseLease voluntarily drops the lease on a VoyageWorker graceful shutdown:
// running → pending for immediate re-pickup by another Keeper (without waiting
// for the lease to expire via Reaper).
//
// WHERE is narrowed to status='running' + ownership: if the row is already terminal or
// the lease was lost — the UPDATE is a no-op with RowsAffected=0, the caller treats it as a normal
// exit (parity with errandrun.ReleaseLease).
func ReleaseLease(ctx context.Context, db ExecQueryRower, id, kid string) error {
	if id == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if kid == "" {
		return fmt.Errorf("voyage: empty kid")
	}
	if _, err := db.Exec(ctx, releaseLeaseSQL, id, kid); err != nil {
		return fmt.Errorf("voyage: release lease: %w", err)
	}
	return nil
}
