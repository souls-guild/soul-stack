package voyage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/pgutil"
)

// claimableWhere — the claimable predicate: `pending`, or `scheduled` whose
// schedule_at has come (S4). The peek, the claim and the deferral all repeat
// it, so a row another worker changed in between fails the later statement.
const claimableWhere = `(status = 'pending' OR (status = 'scheduled' AND schedule_at <= NOW()))`

// peekClaimableSQL — the next claimable Voyage, FIFO by created_at, together
// with the database clock the predicate is evaluated against. The start gate
// is asked at that instant: a Keeper whose clock runs behind the database would
// otherwise find a row due by NOW() still closed, defer it to the same moment,
// and spin on it.
//
// No lock: the claim that follows is a CAS on the same predicate, so two
// workers that peek the same row cannot both claim it.
const peekClaimableSQL = `SELECT ` + selectColumns + `, NOW()
FROM voyages
WHERE ` + claimableWhere + `
ORDER BY created_at ASC
LIMIT 1
`

// claimByIDSQL — claims the peeked Voyage: claimable → running, sets the
// owner/lease, sets started_at on first claim, and increments attempt (fencing
// epoch, ADR-027(g)). SKIP LOCKED makes a row a competitor is claiming at this
// instant a miss rather than a wait; the predicate re-checked under the lock
// makes a row it already claimed a miss too.
//
//	$1 kid   — KID of the owning Keeper
//	$2 lease — interval until claim_expires_at (NOW() + $2)
//	$3 id    — the peeked voyage_id
const claimByIDSQL = `
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
    WHERE c.voyage_id = $3
      AND (c.status = 'pending' OR (c.status = 'scheduled' AND c.schedule_at <= NOW()))
    FOR UPDATE SKIP LOCKED
)
RETURNING ` + selectColumns

// deferByIDSQL — puts a row the start gate held back to `scheduled` at the
// gate's time. started_at stays NULL and attempt is untouched: the row has not
// started, so the next claim asks the gate again.
const deferByIDSQL = `
UPDATE voyages
SET status      = 'scheduled',
    schedule_at = $2
WHERE voyage_id = $1
  AND started_at IS NULL
  AND ` + claimableWhere

// refuseByIDSQL — fails a row whose targets have no common start, and cancels
// its targets with the reason, in one statement so a crash cannot leave a
// failed Voyage with awaiting targets. The count says whether the row was
// still claimable.
const refuseByIDSQL = `
WITH v AS (
    UPDATE voyages
    SET status      = 'failed',
        finished_at = NOW(),
        summary     = $2
    WHERE voyage_id = $1
      AND started_at IS NULL
      AND ` + claimableWhere + `
    RETURNING voyage_id
), t AS (
    UPDATE voyage_targets AS vt
    SET status        = 'cancelled',
        finished_at   = NOW(),
        error_code    = $3,
        error_message = $4
    FROM v
    WHERE vt.voyage_id = v.voyage_id
      AND vt.status = 'awaiting'
)
SELECT count(*) FROM v
`

// maxClaimHops bounds one [ClaimNext]: every hop is a row the gate held back
// or a race lost to another worker. The caller retries at once when the hops
// did something, so the bound only spreads the work over calls.
const maxClaimHops = 32

// GateOutcome is what the claim did with a row its [StartGate] held back:
// deferred to Verdict.DeferTo, or failed for Verdict.NoStart. Voyage is the row
// as peeked.
type GateOutcome struct {
	Voyage  *Voyage
	Verdict GateVerdict
}

// ClaimNext claims the next claimable Voyage (pending, or scheduled and due),
// FIFO by created_at: → running, claim fields + attempt+1 + started_at.
//
// A scenario Voyage that has not started yet is first put to gate (ADR-0093
// §3) — every one, whoever created it: an operator's run, a Cadence spawn, the
// Reaper's rotation. A held-back row is deferred, or failed when it can never
// start, and the next row is tried; held-back rows come back as outcomes for
// the caller to report. A row that already started is a reclaim after a lost
// lease, and the gate is not asked again: a run that started runs to its end.
//
// Returns a nil Voyage when nothing is claimable. With outcomes but no Voyage,
// the caller should call again at once rather than sleep.
func ClaimNext(ctx context.Context, db ExecQueryRower, kid string, leaseTTL time.Duration, gate StartGate) (*Voyage, []GateOutcome, error) {
	if kid == "" {
		return nil, nil, fmt.Errorf("voyage: empty kid")
	}
	if leaseTTL <= 0 {
		return nil, nil, fmt.Errorf("voyage: non-positive lease %s", leaseTTL)
	}
	if gate == nil {
		return nil, nil, fmt.Errorf("voyage: nil start gate")
	}

	var outcomes []GateOutcome
	for range maxClaimHops {
		var now time.Time
		cand, err := scanVoyage(db.QueryRow(ctx, peekClaimableSQL), &now)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, outcomes, nil
			}
			return nil, outcomes, fmt.Errorf("voyage: peek claimable: %w", err)
		}

		if cand.Kind == KindScenario && cand.StartedAt == nil {
			verdict, err := gate.Decide(ctx, db, cand, now)
			if err != nil {
				return nil, outcomes, err
			}
			held, err := holdBack(ctx, db, cand, verdict)
			if err != nil {
				return nil, outcomes, err
			}
			if held {
				outcomes = append(outcomes, GateOutcome{Voyage: cand, Verdict: verdict})
				continue
			}
			if !verdict.DeferTo.IsZero() || verdict.NoStart {
				continue // lost the row to another worker
			}
		}

		v, err := scanVoyage(db.QueryRow(ctx, claimByIDSQL, kid, pgutil.Interval(leaseTTL), cand.VoyageID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, outcomes, fmt.Errorf("voyage: claim next: %w", err)
		}
		return v, outcomes, nil
	}
	return nil, outcomes, nil
}

// holdBack applies a closed verdict to the row. false with no error means
// either the verdict was open or the row had stopped being claimable.
func holdBack(ctx context.Context, db ExecQueryRower, v *Voyage, verdict GateVerdict) (bool, error) {
	switch {
	case verdict.NoStart:
		total := scopeSize(v.TargetResolved)
		summary, err := marshalSummary(&Summary{Total: total, Cancelled: total})
		if err != nil {
			return false, fmt.Errorf("voyage: marshal summary: %w", err)
		}
		var n int
		if err := db.QueryRow(ctx, refuseByIDSQL, v.VoyageID, summary,
			TargetErrorNoCommonWindow, verdict.Reason).Scan(&n); err != nil {
			return false, fmt.Errorf("voyage: refuse %s: %w", v.VoyageID, err)
		}
		return n == 1, nil
	case !verdict.DeferTo.IsZero():
		tag, err := db.Exec(ctx, deferByIDSQL, v.VoyageID, verdict.DeferTo.UTC())
		if err != nil {
			return false, fmt.Errorf("voyage: defer %s: %w", v.VoyageID, err)
		}
		return tag.RowsAffected() == 1, nil
	}
	return false, nil
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
