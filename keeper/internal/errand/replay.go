package errand

import (
	"context"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// orphanGraceDuration is how long a running Errand of this keeper instance
// may be overdue before Replay marks it timed_out. Derived from
// server-cap × 5 (ADR-033 server-cap 300s × 5 = 25 min) — well above any
// real user TimeoutSec (300s ceiling), plus slack for time drift and slow
// recovery. This is a per-Replay-call value, not a package constant
// (callers may override via ReplayOptions).
const orphanGraceDuration = 25 * time.Minute

// ReplayOptions holds the parameters for the one-shot recovery scan at
// keeper startup. All fields are optional; the zero value is a sensible default.
type ReplayOptions struct {
	// Grace is how "overdue" a running Errand of this KID must be before
	// Replay marks it timed_out. nil/0 → [orphanGraceDuration]. Symmetric
	// with pushorch.purge_orphan_push_runs (ADR-027(b)).
	Grace time.Duration

	// Reason is the error_message tag on transitioned rows. Optional;
	// defaults to "keeper restart: orphan running errand".
	Reason string
}

// Replay transitions "orphaned" running Errands of the current keeper
// instance to timed_out. Orphans come from process restart: any running row
// with started_by_kid=self and started_at < now-grace will never see its
// ErrandResult (the background goroutine died with the process).
//
// Called once from setupErrandDispatcher after Store.Insert/Reaper deps are
// wired, BEFORE the HTTP listener starts (so reaper-purge_old_errands and
// stale running rows don't race new Dispatch calls).
//
// Returns the number of transitioned rows (for logging).
func (d *Dispatcher) Replay(ctx context.Context, opts ReplayOptions) (int, error) {
	grace := opts.Grace
	if grace <= 0 {
		grace = orphanGraceDuration
	}
	reason := opts.Reason
	if reason == "" {
		reason = "keeper restart: orphan running errand"
	}

	ids, err := d.deps.Store.SweepOrphanRunning(ctx, d.deps.KID, grace, reason)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	d.deps.Logger.Warn("errand: replay swept orphan running errands",
		slog.String("kid", d.deps.KID),
		slog.Int("count", len(ids)),
		slog.Duration("grace", grace))

	// Audit events: one `errand.timed_out` per orphaned row. No archon_aid
	// in the payload (orphan-purge is a keeper-internal path, there's no
	// initiating Archon on this write path; SourceSoulGRPC matches the
	// live-timed_out handler's channel).
	if d.deps.Audit != nil {
		for _, id := range ids {
			payload := map[string]any{
				"errand_id": id,
				"reason":    reason,
				"orphan":    true,
			}
			ev := &audit.Event{
				EventType:     audit.EventTypeErrandTimedOut,
				Source:        audit.SourceSoulGRPC,
				CorrelationID: id,
				Payload:       payload,
			}
			if werr := d.deps.Audit.Write(ctx, ev); werr != nil {
				d.deps.Logger.Warn("errand: audit orphan-timed_out failed",
					slog.String("errand_id", id),
					slog.Any("error", werr))
			}
		}
	}
	return len(ids), nil
}
