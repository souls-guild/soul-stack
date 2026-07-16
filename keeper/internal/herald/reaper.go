package herald

// Mini-reaper for orphaned delivery jobs (ADR-052(d)): a worker that claimed a job
// (BRPOPLPUSH into the processing LIST) could die before Ack/Requeue, leaving the
// job stuck in processing with an expired lease key. Reaper periodically returns
// such jobs back to pending (at-least-once). This is a lightweight background
// goroutine, one per instance; concurrent reapers across N instances are safe
// because transfer is idempotent by LREM.

import (
	"context"
	"log/slog"
	"time"
)

// DefaultReaperInterval is the scan period for orphaned jobs in the processing
// LIST. It is less frequent than leaseTTL (5m): an orphaned job waits at most
// interval+leaseTTL before return, which is acceptable for notifications.
const DefaultReaperInterval = time.Minute

// RunDeliveryReaper runs the mini-reaper until ctx is cancelled. On every tick it
// calls the backend's RequeueExpired and logs the number of returned jobs. Scan
// errors are best-effort: logged but do not stop the loop; the next tick retries.
func RunDeliveryReaper(ctx context.Context, queue queueBackend, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = DefaultReaperInterval
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := queue.RequeueExpired(ctx, jobIDFromPayload)
			if err != nil {
				logger.Warn("herald: delivery reaper scan failed", slog.Any("error", err))
				continue
			}
			if n > 0 {
				logger.Info("herald: requeued orphaned delivery jobs", slog.Int("count", n))
			}
		}
	}
}
