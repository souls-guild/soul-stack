package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// voyagesQuerier is the narrow pgxpool.Pool surface needed by the
// `reclaim_voyages` rule: one Query (UPDATE ... RETURNING) for per-row audit.
// Narrowing allows a fake in unit tests without starting Postgres; a real
// *pgxpool.Pool satisfies it automatically, following the errandsExecer pattern.
type voyagesQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// reclaimVoyagesSQL is the recovery scan for expired Voyage claims (ADR-043 S4,
// docs/keeper/reaper.md -> section reclaim_voyages). It returns a Voyage with
// status `running` and expired `claim_expires_at` back to `pending` for re-claim
// by another Keeper instance; `attempt++` provides a fencing epoch and parity
// with reclaim_apply_runs. `current_batch_index` is reset to 0 so reclaim
// re-executes the run from scratch (idempotent re-apply executor with legs[0]);
// resume-from-batch, continuing at N, is a separate epic.
//
// Reclaim returns to `pending`, NOT the original `scheduled`: by the time a row
// is running, schedule_at has definitely arrived, and the row must be picked up
// immediately.
//
// Uses partial index `voyages_claim_scan_idx` (migration 059,
// `WHERE status = 'running'`). The rule's `lease` parameter is NOT part of the
// predicate: lease is already encoded in `claim_expires_at` during
// voyage.ClaimNext, so the predicate compares `claim_expires_at < NOW()`
// directly, matching reclaim_apply_runs. FOR UPDATE SKIP LOCKED in the nested
// SELECT protects from races with concurrent claim/renew.
//
// CTE `picked` captures the pre-reclaim `last_renewed_at` because audit needs
// the value BEFORE it is reset to NULL, and also protects SKIP LOCKED.
// `UPDATE ... RETURNING` returns the new `attempt`. Final Query output is
// voyage_id + last_renewed_at(before) + attempt(after), the per-row
// `voyage.reclaimed` payload (kind-agnostic, ADR-043 A3): SQL does not inspect
// kind, and the event is shared for scenario/command.
const reclaimVoyagesSQL = `
WITH picked AS (
    SELECT voyage_id, last_renewed_at
    FROM voyages
    WHERE status = 'running' AND claim_expires_at < NOW()
    FOR UPDATE SKIP LOCKED
), updated AS (
    UPDATE voyages v
    SET status           = 'pending',
        claimed_by_kid   = NULL,
        last_renewed_at  = NULL,
        claim_expires_at = NULL,
        attempt          = attempt + 1,
        current_batch_index = 0
    FROM picked p
    WHERE v.voyage_id = p.voyage_id
    RETURNING v.voyage_id, v.attempt
)
SELECT u.voyage_id, p.last_renewed_at, u.attempt
FROM updated u
JOIN picked p ON p.voyage_id = u.voyage_id
`

// VoyageReclaimer implements the `reclaim_voyages` rule (ADR-043 S4,
// docs/keeper/reaper.md). One batch pass is one Query (UPDATE ... RETURNING)
// through partial index `voyages_claim_scan_idx`. Run's signature is compatible
// with Runner's runDurationRule call.
//
// Per-row audit (ADR-043 A3): each reclaimed row emits `voyage.reclaimed`
// (scope `voyage.*`, kind-agnostic because SQL does not inspect kind). audit is
// nil-safe: dev without audit works, and emit happens only when audit != nil.
//
// The `lease` parameter is NOT part of the predicate; see [reclaimVoyagesSQL].
// `batchSize` is not either because UPDATE cuts in one statement. Arguments
// remain in the signature for compatibility with the common duration runner.
type VoyageReclaimer struct {
	pool   voyagesQuerier
	audit  audit.Writer
	logger *slog.Logger
}

// NewVoyageReclaimer constructs a reclaimer. logger is nil-safe, suppressing
// warnings, and audit is nil-safe, skipping emits. The public constructor fixes
// *pgxpool.Pool so callers do not depend on interface expansion, following the
// NewErrandsPurger pattern.
func NewVoyageReclaimer(pool *pgxpool.Pool, audit audit.Writer, logger *slog.Logger) *VoyageReclaimer {
	return &VoyageReclaimer{pool: pool, audit: audit, logger: logger}
}

// newVoyageReclaimerFromQuerier is the internal constructor for unit tests.
func newVoyageReclaimerFromQuerier(pool voyagesQuerier, audit audit.Writer, logger *slog.Logger) *VoyageReclaimer {
	return &VoyageReclaimer{pool: pool, audit: audit, logger: logger}
}

// Run executes one rule iteration: it returns expired running Voyages back to
// pending and emits per-row `voyage.reclaimed`. It returns (affected, err),
// where affected is the number of reclaimed rows that callers add to
// keeper_reaper_* metrics.
//
// The signature is compatible with runDurationRule (`(ctx, duration, batch) ->
// (int64, error)`); lease/batchSize arguments are ignored, see the type comment.
func (r *VoyageReclaimer) Run(ctx context.Context, _ time.Duration, _ int) (int64, error) {
	if r.pool == nil {
		return 0, fmt.Errorf("reaper.reclaim_voyages: pool is nil")
	}
	rows, err := r.pool.Query(ctx, reclaimVoyagesSQL)
	if err != nil {
		return 0, fmt.Errorf("reaper.reclaim_voyages: %w", err)
	}
	defer rows.Close()

	var (
		reclaimed []reclaimedVoyage
		affected  int64
	)
	for rows.Next() {
		var (
			voyageID    string
			lastRenewed *time.Time
			attempt     int
		)
		if scanErr := rows.Scan(&voyageID, &lastRenewed, &attempt); scanErr != nil {
			return affected, fmt.Errorf("reaper.reclaim_voyages: scan: %w", scanErr)
		}
		affected++
		reclaimed = append(reclaimed, reclaimedVoyage{
			voyageID:    voyageID,
			lastRenewed: lastRenewed,
			attempt:     attempt,
		})
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return affected, fmt.Errorf("reaper.reclaim_voyages: rows: %w", rowsErr)
	}
	rows.Close()

	// Per-row audit happens AFTER closing rows so the cursor is not held open
	// during I/O emit. Best-effort: one event error does not cancel reclaim.
	for _, rv := range reclaimed {
		r.emitReclaimed(ctx, rv)
	}
	return affected, nil
}

// reclaimedVoyage is a snapshot of one reclaimed row for per-row audit.
type reclaimedVoyage struct {
	voyageID    string
	lastRenewed *time.Time
	attempt     int
}

// emitReclaimed writes `voyage.reclaimed` (ADR-043 A3, scope `voyage.*`,
// kind-agnostic). source=keeper_internal, archon_aid="" (NULL). It is nil-safe.
func (r *VoyageReclaimer) emitReclaimed(ctx context.Context, rv reclaimedVoyage) {
	if r.audit == nil {
		return
	}
	payload := map[string]any{
		"voyage_id":     rv.voyageID,
		"attempt_after": rv.attempt,
	}
	if rv.lastRenewed != nil {
		payload["last_renewed_at"] = rv.lastRenewed.UTC()
	}
	ev := &audit.Event{
		EventType: audit.EventVoyageReclaimed,
		Source:    audit.SourceKeeperInternal,
		Payload:   payload,
	}
	if err := r.audit.Write(ctx, ev); err != nil && r.logger != nil {
		r.logger.Warn("reaper.reclaim_voyages: audit write failed",
			slog.String("voyage_id", rv.voyageID), slog.Any("error", err))
	}
}
