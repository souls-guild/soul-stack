package toll

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Watcher — per-Keeper-instance observer of gRPC EventStream disconnect events
// (ADR-038(b)). NOT a goroutine: passive object whose methods are called
// by EventStream handlers on receive-loop exit. Holds no state
// (`StartedAt` for warmup-window — only field); one instance per keeper
// process shared among all streams.
//
// Filtering (ADR-038(c)):
//  1. Warmup-immunity: first [Config.WarmupDelay] after instance start
//     disconnects NOT published (cluster cold-start protection). Metric
//     [Metrics.IncWarmupSkipped] still grows — operator sees the fact.
//  2. Graceful-shutdown: if caller specifies gracefulShutdown=true (context
//     cancelled on instance shutdown), disconnect rejected. Metric
//     [Metrics.IncGracefulSkipped].
//  3. Live disconnect: after filters — ZADD to common Redis sorted-set
//     ([Publisher]) + [Metrics.IncDisconnect].
//
// Per-coven counter incremented ALWAYS (including rejected ones) so
// counter is observation rate source, not filtered by leader.
// Note: ADR-038(g) describes counter as «non-graceful disconnects»;
// counter kept AFTER filtering (graceful + warmup rejected, see NotifyDisconnect).
type Watcher struct {
	kid       string
	publisher Publisher
	logger    *slog.Logger
	metrics   *Metrics
	startedAt time.Time
	warmup    time.Duration
}

// Config — Watcher parameters.
type Config struct {
	// KID — keeper instance identifier (written to sorted-set member-value
	// for logging / diagnostics of leader-aggregator).
	KID string
	// WarmupDelay — immunity window after instance start. <=0 → default (60s),
	// so fake-watcher in tests doesn't depend on config resolution.
	WarmupDelay time.Duration
}

// defaultWarmupDelay — reserve if WarmupDelay<=0 (unit-tests may
// pass 0 and explicitly reset StartedAt). 60s matches ADR-038.
const defaultWarmupDelay = 60 * time.Second

// NewWatcher builds Watcher. publisher / logger required; metrics
// optional (nil → counters disabled, nil-safe methods [Metrics]).
//
// startedAt = NOW: warmup counted from construction, not from first
// disconnect. For unit-tests there is [Watcher.setStartedAt] (test-helper).
func NewWatcher(cfg Config, publisher Publisher, metrics *Metrics, logger *slog.Logger) (*Watcher, error) {
	if cfg.KID == "" {
		return nil, errors.New("toll.NewWatcher: empty KID")
	}
	if publisher == nil {
		return nil, errors.New("toll.NewWatcher: nil publisher")
	}
	if logger == nil {
		return nil, errors.New("toll.NewWatcher: nil logger")
	}
	warmup := cfg.WarmupDelay
	if warmup <= 0 {
		warmup = defaultWarmupDelay
	}
	return &Watcher{
		kid:       cfg.KID,
		publisher: publisher,
		logger:    logger,
		metrics:   metrics,
		startedAt: time.Now(),
		warmup:    warmup,
	}, nil
}

// NotifyDisconnect — hook for gRPC EventStream cleanup. Caller passes
// SID of disconnected Soul, its covens (if known; empty string
// allowed) and gracefulShutdown flag (true → closure initiated by the instance
// itself, e.g. Watchman-shedding or graceful keeper-shutdown).
//
// Method non-blocking: on any problem (Publisher-error, ctx.Done) logs
// debug and continues — disconnect flow of EventStream handler should not
// depend on Toll infrastructure liveness. Publisher-error not fatal
// (Redis temporarily down — Leader will ignore empty window anyway and
// not set false-positive).
func (w *Watcher) NotifyDisconnect(ctx context.Context, sid, coven string, gracefulShutdown bool) {
	if w == nil {
		return
	}
	// Warmup-immunity: first WarmupDelay disconnects not published.
	if time.Since(w.startedAt) < w.warmup {
		w.metrics.IncWarmupSkipped()
		w.logger.Debug("toll: disconnect skipped (warmup immunity)",
			slog.String("sid", sid),
			slog.Duration("since_start", time.Since(w.startedAt)),
		)
		return
	}
	// Graceful-shutdown: don't count planned closure as churn.
	if gracefulShutdown {
		w.metrics.IncGracefulSkipped()
		w.logger.Debug("toll: disconnect skipped (graceful shutdown)",
			slog.String("sid", sid),
		)
		return
	}
	// Post-filter: counter grows + publish to common sorted-set.
	w.metrics.IncDisconnect(coven)
	if err := w.publisher.PublishDisconnect(ctx, sid, w.kid, coven, time.Now()); err != nil {
		// Not fatal: Leader on next tick will ignore empty window. Debug-level
		// log because Redis flaps can be frequent, and the disconnect is already
		// reflected in counter.
		w.logger.Debug("toll: publish disconnect failed",
			slog.String("sid", sid),
			slog.Any("error", err),
		)
	}
}

// setStartedAt — test-helper for unit-tests: allows shifting startedAt
// to the past (warmup expired) or to the future (warmup still active) without sleeps
// and without dependence on real clock.
func (w *Watcher) setStartedAt(t time.Time) { w.startedAt = t }
