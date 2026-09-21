// Package watchman provides isolation detection of a Keeper instance and
// active closure (shedding) of its local EventStream streams (soul-shedding S2,
// ADR-002 HA cluster Keeper).
//
// Problem: when a Keeper instance is isolated (lost PG/Redis) or unhealthy,
// its already-established long-lived EventStream streams to Souls do not
// close themselves. `/readyz` only prevents NEW connections (LB checks
// readiness), but existing gRPC bidi-streams are independent of HTTP health —
// Souls remain stuck on an unhealthy instance and do not failover to a healthy Keeper.
//
// Watchman is a background goroutine on the instance: periodically probes the same
// dependencies as `/readyz` (PG + Redis), and upon PERSISTENT isolation actively
// closes ALL local streams (hard-close, no drain): cancels per-stream
// ctx for each ([StreamCloser.CloseAll]) → EventStream handler returns →
// gRPC sends EOF to Soul → Soul via reconnect-loop/failback-list moves to a healthy
// Keeper. Upon dependency recovery, resumes normal operation
// (new streams are accepted by listener, lease-renewal / Acolyte recover on their own).
//
// Centralization: the decision "I am isolated" is made ONLY here, not duplicated
// in each per-stream renewal-loop (after CloseAll, their renewal goroutines die
// with the streams). Single source of truth about isolation.
//
// Debounce/flap-guard: isolation is declared only after [Config.FailThreshold]
// consecutive probe failures, not on the first one. A single network spike
// must not shed all streams at once (thundering-herd reconnect across the
// cluster). One successful probe resets the counter. If the instance is already
// in "isolated" state and dependencies return — Watchman logs recovery and
// resets the counter; does not repeat CloseAll while isolated (streams are already
// closed; new streams should not appear on an isolated instance).
package watchman

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	// DefaultInterval is the period of Watchman probe ticks. 5s balances
	// reaction speed to isolation (= Interval × FailThreshold ≈ 15s until shedding)
	// and load on PG/Redis pings. Same order as DefaultConclaveRenewInterval.
	DefaultInterval = 5 * time.Second

	// DefaultFailThreshold is the number of consecutive probe failures before
	// declaring isolation. 3 debounces single spikes: transient outage
	// (one or two ticks) is survived without shedding, persistent loss (>=3 ticks)
	// triggers it. Lower value = aggressive shedding on noisy network;
	// higher value = slow reaction to actual isolation.
	DefaultFailThreshold = 3

	// defaultProbeTimeout is the hard timeout for a single probe call. Without it,
	// a hung PG/Redis ping would block probe tick longer than Interval. 2s
	// matches health.perCheckTimeout (same order as `/readyz` uses).
	defaultProbeTimeout = 2 * time.Second
)

// ErrNoProbeDeps indicates the constructor received no dependencies to probe.
// Watchman without dependencies is pointless: it has nothing to ping, cannot
// detect isolation. Caller (daemon) must provide at least one Pinger.
var ErrNoProbeDeps = errors.New("watchman: at least one health probe is required")

// HealthProbe is a narrow interface for checking instance dependency availability.
// Probe returns nil if all dependencies are healthy, non-nil if at least one
// is unreachable (isolation indicator). Default implementation ([NewDepsProbe])
// composes the same `health.Pinger`s as `/readyz` (PG + Redis), with
// per-check timeout. Narrow interface keeps Watchman testable with fakes
// and separates probe logic from lifecycle.
type HealthProbe interface {
	Probe(ctx context.Context) error
}

// StreamCloser is a narrow interface for forcing closure of all local
// EventStream streams. Implemented by [keepergrpc.StreamManager] (CloseAll cancels
// per-stream ctx for each registered stream). Single-method interface isolates
// Watchman from full stream registry and allows fakes in unit tests.
type StreamCloser interface {
	// CloseAll cancels ctx for all local streams and returns their count
	// (for logging/metrics). Idempotent (context.CancelFunc is safe to retry).
	CloseAll() int
}

// Metrics provides observability for Watchman (optional). Nil disables all
// accounting (unit tests / dev builds without observability); Watchman checks
// `w.metrics != nil` before each call.
type Metrics interface {
	// SetIsolated sets gauge keeper_watchman_isolated (1 = instance declared
	// isolated, 0 = healthy).
	SetIsolated(isolated bool)
	// AddStreamsShed adds n to counter keeper_watchman_streams_shed_total
	// (total streams closed by shedding since startup).
	AddStreamsShed(n int)
}

// Config holds Watchman parameters.
type Config struct {
	// Interval is the period of probe ticks. <=0 defaults to [DefaultInterval].
	Interval time.Duration
	// FailThreshold is the number of consecutive probe failures before shedding.
	// <=0 defaults to [DefaultFailThreshold].
	FailThreshold int
	// ProbeTimeout is the timeout for a single probe call. <=0 defaults to
	// [defaultProbeTimeout].
	ProbeTimeout time.Duration
}

// Watchman implements isolation detection and stream shedding. One instance
// per Keeper instance, started by daemon after EventStream listener (needs
// StreamManager) and Redis/PG (needs probe dependencies).
//
// consecutiveFails and isolated track debounce machine state. No mutex: accessed
// ONLY from probe loop (single [Run] goroutine); tests call [Watchman.tick]
// similarly from single goroutine.
type Watchman struct {
	probe   HealthProbe
	closer  StreamCloser
	cfg     Config
	logger  *slog.Logger
	metrics Metrics
	probeTO time.Duration

	consecutiveFails int
	isolated         bool
}

// New constructs a Watchman. probe / closer / logger are required; metrics
// is optional (nil disables observability). Empty Config fields default.
func New(probe HealthProbe, closer StreamCloser, cfg Config, metrics Metrics, logger *slog.Logger) (*Watchman, error) {
	if probe == nil {
		return nil, errors.New("watchman: HealthProbe is required")
	}
	if closer == nil {
		return nil, errors.New("watchman: StreamCloser is required")
	}
	if logger == nil {
		return nil, errors.New("watchman: logger is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = DefaultFailThreshold
	}
	probeTO := cfg.ProbeTimeout
	if probeTO <= 0 {
		probeTO = defaultProbeTimeout
	}
	return &Watchman{
		probe:   probe,
		closer:  closer,
		cfg:     cfg,
		logger:  logger,
		metrics: metrics,
		probeTO: probeTO,
	}, nil
}

// Run is the blocking probe loop. Exits on ctx.Done() (graceful shutdown).
// Caller (daemon) runs it in a goroutine and stops via ctx cancellation plus
// LIFO cleanup stack (like conclave/reaper).
//
// Each tick: probe with timeout. Failure → increment consecutive failures;
// reach FailThreshold (not already isolated) → shedding (CloseAll). Success →
// if there were failures or isolation, log recovery and reset counter and flag.
// No repeated CloseAll while isolated (streams already closed; new streams should
// not appear on isolated instance — listener needs healthy PG/Redis for
// lease/seed-auth).
func (w *Watchman) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := w.runProbe(ctx)
			// Cancellation of probe-ctx due to shutdown (ctx.Done between ticks)
			// is not counted as dependency failure: on next iteration the loop
			// will exit at ctx.Done() above.
			if err != nil && ctx.Err() != nil {
				return
			}
			w.tick(err)
		}
	}
}

// tick is one debounce machine iteration: accounts for probe result (nil = healthy),
// decides shedding/recovery. Extracted from [Run] (which only runs ticker)
// for declarative testing of debounce/isolation/recovery without timers.
// Executes in same goroutine as Run (state is unguarded).
func (w *Watchman) tick(err error) {
	if err != nil {
		w.consecutiveFails++
		w.logger.Warn("watchman: probe failed",
			slog.Int("consecutive_fails", w.consecutiveFails),
			slog.Int("threshold", w.cfg.FailThreshold),
			slog.Bool("isolated", w.isolated),
			slog.Any("error", err),
		)
		if w.consecutiveFails >= w.cfg.FailThreshold && !w.isolated {
			w.isolated = true
			w.setIsolated(true)
			n := w.closer.CloseAll()
			w.addStreamsShed(n)
			w.logger.Error("watchman: instance isolated — shedding all local EventStream streams",
				slog.Int("consecutive_fails", w.consecutiveFails),
				slog.Int("streams_shed", n),
			)
		}
		return
	}
	// Successful probe.
	if w.isolated {
		w.logger.Info("watchman: dependencies recovered — resuming normal operation",
			slog.Int("prior_consecutive_fails", w.consecutiveFails),
		)
		w.isolated = false
		w.setIsolated(false)
	} else if w.consecutiveFails > 0 {
		// Spike survived without declaring isolation (debounce worked).
		w.logger.Info("watchman: probe recovered before isolation threshold",
			slog.Int("prior_consecutive_fails", w.consecutiveFails),
		)
	}
	w.consecutiveFails = 0
}

// runProbe invokes probe with per-tick timeout.
func (w *Watchman) runProbe(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, w.probeTO)
	defer cancel()
	return w.probe.Probe(pctx)
}

func (w *Watchman) setIsolated(isolated bool) {
	if w.metrics != nil {
		w.metrics.SetIsolated(isolated)
	}
}

func (w *Watchman) addStreamsShed(n int) {
	if w.metrics != nil && n > 0 {
		w.metrics.AddStreamsShed(n)
	}
}
