// Package leaderloop — generic Redis-lease leader-loop for background
// singleton tasks of the Keeper cluster (ADR-006(d)).
//
// This is a generic leadership scaffold extracted from the Reaper runner:
// acquire Redis lease → renewal goroutine → periodic tick-callback on the
// holder → re-acquire on lease loss. The same loop is reused by HA tasks
// (Reaper, Conductor cadence scheduler) that differ only in tick logic.
//
// Algorithm (lease semantics — exact copy of the original Reaper runner):
//
//   - A: periodic tick via `time.Ticker` with interval from [Config.IntervalFn]
//     (hot-reload: interval is re-read on every tick).
//   - B: Redis lease is held only while [Loop.Run] is running. On graceful
//     shutdown (ctx.Done) the lease is released via Release.
//   - C: TTL is renewed by a separate goroutine every `lock_ttl/3`. A long
//     tick doesn't let the key expire — Renew is not blocked by the
//     tick-callback.
//   - D: lease loss (ErrLeaseLost) signals the tick-loop to stop; the loop
//     then returns to the acquire phase (re-acquire leadership).
//   - E: ctx.Done — Release, exit with nil.
//
// Lease primitive (Acquire / Renew / Release / CAS-fencing) lives in
// [keeper/internal/redis]; this package only orchestrates its lifecycle.
package leaderloop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// defaultAcquireBackoff is the pause between Acquire attempts on leadership
// conflict when [Config.AcquireBackoff] is unset. Matches the historical
// Reaper default: large enough to not flood Redis, small enough that
// failover on leader death happens within a few seconds.
const defaultAcquireBackoff = 5 * time.Second

// Config — leader-loop parameters. All fields except [Config.AcquireBackoff]
// and [Config.OnLeaseChange] are required: missing ones are a caller
// programming error, [New] returns an error.
type Config struct {
	// LeaseKey — Redis leadership key. One singleton loop = one unique key
	// in the cluster (e.g. "reaper:leader").
	LeaseKey string

	// Holder — instance identifier (KID), written into the lease key for
	// human-readable logs and distinguishing leadership changes.
	Holder string

	// Redis — client used to acquire the lease.
	Redis *redis.Client

	// Logger — slog logger. Structured fields: `key`, `holder`.
	Logger *slog.Logger

	// IntervalFn — interval between ticks. Called on every tick, which gives
	// hot-reload: returns a new value → the next tick is scheduled with it.
	IntervalFn func() time.Duration

	// LockTTLFn — TTL of the Redis lease key. Called on every acquire
	// iteration (hot-reload between re-acquires). Renew interval is derived
	// as `lock_ttl/3`.
	LockTTLFn func() time.Duration

	// Tick — callback executed by the lease holder on every interval (plus
	// once right after acquire, without waiting for the first tick). Runs
	// synchronously in the loop goroutine: the next tick won't start until
	// the current one returns.
	Tick func(ctx context.Context)

	// AcquireBackoff — pause between Acquire attempts on leadership conflict.
	// Zero value → [defaultAcquireBackoff].
	AcquireBackoff time.Duration

	// OnLeaseChange — optional callback for leadership status changes: true
	// on lease acquire, false on exit from the tick-loop (ctx.Done or
	// lease-loss). Nil is fine (no-op). Used for the lease_held metric.
	OnLeaseChange func(held bool)
}

// Loop — root structure of the leader-loop. One instance per background
// task. Created via [New], started via [Loop.Run].
type Loop struct {
	cfg            Config
	acquireBackoff time.Duration
}

// New validates the config and returns a Loop. Missing required fields
// return an error: a caller wire-up bug, not a runtime condition.
func New(cfg Config) (*Loop, error) {
	if cfg.LeaseKey == "" {
		return nil, errors.New("leaderloop.New: LeaseKey is required")
	}
	if cfg.Holder == "" {
		return nil, errors.New("leaderloop.New: Holder is required")
	}
	if cfg.Redis == nil {
		return nil, errors.New("leaderloop.New: Redis is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("leaderloop.New: Logger is required")
	}
	if cfg.IntervalFn == nil {
		return nil, errors.New("leaderloop.New: IntervalFn is required")
	}
	if cfg.LockTTLFn == nil {
		return nil, errors.New("leaderloop.New: LockTTLFn is required")
	}
	if cfg.Tick == nil {
		return nil, errors.New("leaderloop.New: Tick is required")
	}
	backoff := cfg.AcquireBackoff
	if backoff <= 0 {
		backoff = defaultAcquireBackoff
	}
	return &Loop{cfg: cfg, acquireBackoff: backoff}, nil
}

// Run drives the loop until ctx is cancelled. Returns nil on graceful stop
// (ctx.Done) and a wrapped error on fatal acquire-phase conditions.
//
// Algorithm:
//
//  1. Every `acquireBackoff` try to acquire the lease, until it succeeds or ctx.Done.
//  2. On success, start the renewal goroutine (Renew every `lock_ttl/3`).
//  3. Tick-loop via `time.Ticker(interval)`; first Tick fires right at acquire.
//  4. On ErrLeaseLost from renewal — gracefully exit the tick-loop, do
//     Release only if we exited NOT via lease-loss (on loss the key is
//     already not ours), go back to step 1 (re-acquire).
//  5. On ctx.Done — Release, exit.
func (l *Loop) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		lease, lockTTL, err := l.acquireWithBackoff(ctx)
		if err != nil {
			// acquireWithBackoff returns a non-nil error only on ctx
			// cancellation or a programming error in the Acquire call (the
			// latter are caught by New).
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("leaderloop.Run: acquire: %w", err)
		}

		l.cfg.Logger.Info("leaderloop: acquired lease",
			slog.String("key", lease.Key()),
			slog.String("holder", lease.Holder()),
			slog.Duration("lock_ttl", lockTTL),
		)
		l.setLeaseHeld(true)

		renewEvery := lockTTL / 3
		// Guards against panic in time.NewTicker on an absurdly short
		// lockTTL (<3ms). Cheap, closes off a class of bugs.
		if renewEvery < time.Millisecond {
			renewEvery = time.Millisecond
		}
		lostCh := l.startRenewal(ctx, lease, renewEvery)
		viaLost := l.tickLoop(ctx, lostCh)
		// Any exit from tickLoop means we're no longer leader. Reset the
		// status immediately, without waiting for the Release call.
		l.setLeaseHeld(false)

		// Cleanup. The renewal goroutine closes lostCh itself (on ctx.Done
		// or ErrLeaseLost). Release only if we exited NOT via lostCh: on
		// ErrLeaseLost the key is already not ours — Release would be a
		// wasted round-trip with a guaranteed CAS-0.
		if !viaLost {
			// WithoutCancel: keep trace baggage, don't inherit the cancel
			// from the teardown path (Release must go through even with a
			// cancelled parent ctx).
			relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			if relErr := lease.Release(relCtx); relErr != nil {
				l.cfg.Logger.Warn("leaderloop: lease release failed",
					slog.String("key", lease.Key()),
					slog.Any("error", relErr),
				)
			}
			relCancel()
		}

		if ctx.Err() != nil {
			return nil
		}
		l.cfg.Logger.Info("leaderloop: lease lost, re-acquire pending",
			slog.String("key", lease.Key()),
		)
	}
}

// acquireWithBackoff tries to acquire the lease, retrying on ErrLeaseTaken
// with a constant `acquireBackoff` pause. On ctx cancellation returns
// (nil, 0, ctx.Err()).
//
// Also returns the effective `lock_ttl` (via LockTTLFn) — the caller uses it
// for the renew interval. Read right here so hot-reloaded TTL is picked up
// between re-acquire iterations.
func (l *Loop) acquireWithBackoff(ctx context.Context) (*redis.Lease, time.Duration, error) {
	for {
		lockTTL := l.cfg.LockTTLFn()

		lease, err := redis.Acquire(ctx, l.cfg.Redis, l.cfg.LeaseKey, l.cfg.Holder, lockTTL)
		if err == nil {
			return lease, lockTTL, nil
		}
		if errors.Is(err, redis.ErrLeaseTaken) {
			// Normal scenario: wait out the backoff, try again.
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(l.acquireBackoff):
				continue
			}
		}
		// Network/validation error from Acquire — log it and wait too.
		// Redis unavailability shouldn't crash the process: the background
		// task runs best-effort.
		l.cfg.Logger.Warn("leaderloop: acquire failed, will retry",
			slog.String("key", l.cfg.LeaseKey),
			slog.Duration("backoff", l.acquireBackoff),
			slog.Any("error", err),
		)
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(l.acquireBackoff):
		}
	}
}

// startRenewal starts a goroutine that renews the key's TTL every
// `renewEvery`. On ErrLeaseLost it closes the returned channel — the
// tick-loop reads it to exit gracefully. On ctx.Done it also closes it (no
// distinction: tick must stop regardless of the reason).
func (l *Loop) startRenewal(ctx context.Context, lease *redis.Lease, renewEvery time.Duration) <-chan struct{} {
	lost := make(chan struct{})
	go func() {
		defer close(lost)
		t := time.NewTicker(renewEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := lease.Renew(ctx); err != nil {
					if errors.Is(err, redis.ErrLeaseLost) {
						l.cfg.Logger.Warn("leaderloop: lease lost during renewal",
							slog.String("key", lease.Key()),
						)
						return
					}
					// Network error from Renew — keep going (the next tick
					// will retry). If Redis stays down long enough, the key
					// will expire and Renew will return ErrLeaseLost on one
					// of the following ticks.
					l.cfg.Logger.Warn("leaderloop: renew failed",
						slog.String("key", lease.Key()),
						slog.Any("error", err),
					)
				}
			}
		}
	}()
	return lost
}

// tickLoop drives the main Ticker until ctx is cancelled or lostCh closes
// (lease lost). The first Tick fires right at acquire (without waiting for
// the first tick).
//
// Returns true if we exited via lostCh (lease-loss), false on ctx.Done.
// The caller uses this to decide whether to call Release.
func (l *Loop) tickLoop(ctx context.Context, lostCh <-chan struct{}) bool {
	// Interval hot-reload: re-read IntervalFn on every tick and recreate the
	// Ticker if it changed. Reset is not called when the interval is stable.
	interval := l.cfg.IntervalFn()
	t := time.NewTicker(interval)
	defer t.Stop()

	// First run happens right at acquire (smoke-visibility of activity +
	// "sweep up whatever piled up right after a restart/failover").
	l.cfg.Tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return false
		case <-lostCh:
			return true
		case <-t.C:
			newInterval := l.cfg.IntervalFn()
			if newInterval != interval {
				interval = newInterval
				t.Reset(interval)
				l.cfg.Logger.Info("leaderloop: interval updated",
					slog.Duration("interval", interval),
				)
			}
			l.cfg.Tick(ctx)
		}
	}
}

// setLeaseHeld is a nil-safe call to OnLeaseChange.
func (l *Loop) setLeaseHeld(held bool) {
	if l.cfg.OnLeaseChange != nil {
		l.cfg.OnLeaseChange(held)
	}
}
