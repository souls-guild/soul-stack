package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// PushRunCanceller — narrow interface to [pushorch.Store] for reaper-purger.
// Narrowed to two methods (ListOrphans/CancelOrphan) needed for the
// `purge_orphan_push_runs` rule. Real implementation is *pushorch.Store; fake in
// unit tests. Names and signatures are the same as in pushorch.Store so wire-up
// in daemon.setupReaper can pass it directly without an adapter.
//
// Placed in reaper package (not pushorch) so reaper doesn't get
// cross import from pushorch (drift vector through closure connection of modules).
type PushRunCanceller interface {
	ListOrphans(ctx context.Context, maxAge time.Duration, batchSize int) ([]string, error)
	CancelOrphan(ctx context.Context, applyID, reason string) (bool, error)
}

// purgeOrphanPushRunsReason — fixed reason recorded in
// push_runs.summary.reason for all orphaned runs. Reaper doesn't have
// context for why exactly the Keeper instance died (KID went down, ctx cancelled, OOM);
// what matters is that the run was in an in-flight status longer than TTL.
const purgeOrphanPushRunsReason = "orphan_purged_by_reaper"

// PurgeOrphanPushRuns — implementation of rule `purge_orphan_push_runs`
// (docs/keeper/reaper.md, registered in Runner.dispatch). Finds in-flight
// push runs (status IN pending/running) older than `maxAge` (Keeper that started
// the run either died or is stuck), transitions each to `cancelled` with
// `orphan_purged: true` mark in summary.
//
// One batch == one LIST + per-row UPDATE. Each UPDATE is guarded by
// status IN (pending,running): single-winner race with real MarkTerminal
// loses (RowsAffected==0, counted as not-purged).
//
// Returns (affected, err): affected — number of actually transitioned
// records. callers (Runner.runDurationRule) will sum this into keeper_reaper_* metrics.
type orphanPurger struct {
	store  PushRunCanceller
	logger *slog.Logger
}

// NewOrphanPushRunsPurger constructs a purger. logger is nil-safe (warnings
// are suppressed).
func NewOrphanPushRunsPurger(store PushRunCanceller, logger *slog.Logger) *orphanPurger {
	return &orphanPurger{store: store, logger: logger}
}

// Run executes one iteration of the rule. Signature is compatible with
// runDurationRule call (Runner.dispatch::case "purge_orphan_push_runs").
func (p *orphanPurger) Run(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	ids, err := p.store.ListOrphans(ctx, maxAge, batchSize)
	if err != nil {
		return 0, fmt.Errorf("reaper.purge_orphan_push_runs: list: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	var affected int64
	for _, id := range ids {
		ok, cerr := p.store.CancelOrphan(ctx, id, purgeOrphanPushRunsReason)
		if cerr != nil {
			// Failure of one per-row UPDATE: continue through the list (best-effort,
			// like in Purger.PurgeApplyRuns batch loop). Log each failure —
			// aggregating errors into one message muddies observability.
			if p.logger != nil {
				p.logger.Warn("reaper: purge_orphan_push_runs cancel failed",
					slog.String("apply_id", id),
					slog.Any("error", cerr))
			}
			continue
		}
		if ok {
			affected++
		}
		// ok=false — single-winner race with real MarkTerminal (orchestrator
		// managed to finalize the record between ListOrphans and CancelOrphan).
		// This is normal, not an error.
	}
	return affected, nil
}
