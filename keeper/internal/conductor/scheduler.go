// Package conductor implements Conductor, the leader-elected executor of
// Cadence schedules (ADR-048). The conductor sets the execution rhythm: on
// each tick, the leader spawns due Cadences into child Voyages.
//
// Conductor is a keeper-side singleton subsystem (not a separate binary,
// same as Reaper). It sits on the generic [leaderloop.Loop] with its own
// Redis lease [LeaseKey] = "conductor:leader" — independent of the reaper
// lease: the Conductor leader and the Reaper leader can be different
// instances (ADR-048 §1). Its own tick interval (cadence_scheduler.interval,
// ~15–30s) is independent of reaper.interval (1h): the Cadence scheduling
// domain and the Reaper cleanup domain have different natural rhythms.
//
// Spawn semantics (due selection FOR UPDATE SKIP LOCKED, three
// overlap_policy values, next_run_at recalculation, single executor in one
// PG tx) live in [Spawner]; the concrete implementation [CadenceSpawner]
// moved here from reaper (C3, ADR-048 §3, verbatim carry-over of ADR-046 §4
// logic). The interface is kept for testability (fakeSpawner in scheduler
// tests). C4 removed the reaper executor — Conductor is the sole spawn
// executor; there's no double/zero spawn (switchover safety: FOR UPDATE SKIP
// LOCKED + advancing next_run_at in one tx cover any transient during
// executor handover).
package conductor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/leaderloop"
	"github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// LeaseKey is the Redis leadership key for Conductor (ADR-048 §1). Separate
// from "reaper:leader" — independent leadership for the cluster's two
// background subsystems.
const LeaseKey = "conductor:leader"

// defaultBatch caps the number of due Cadences spawned per tick when
// [Config.BatchFn] is unset. Anti-avalanche for long cluster downtime:
// accumulated due schedules don't flood the souls in one tick, the rest are
// picked up on subsequent ticks. Matches the historical reaper default for
// spawn_due_cadence.
const defaultBatch = 100

// Spawner is the narrow surface of the due-cadence spawn executor invoked by
// Conductor's tick callback. Signature: `(ctx, duration, batchSize) →
// (spawnedCount, error)`. The duration argument is NOT used in spawn (the
// predicate is next_run_at <= NOW() directly) and is passed as zero;
// batchSize caps the number of schedules per tick.
//
// Production wire-up supplies [*CadenceSpawner] (moved into this package,
// C3). The interface is kept for testability: scheduler tests substitute
// fakeSpawner.
type Spawner interface {
	Run(ctx context.Context, _ time.Duration, batchSize int) (int64, error)
}

// Config holds the Conductor scheduler's parameters. All fields except
// [Config.BatchFn], [Config.AcquireBackoff] and [Config.OnLeaseChange] are
// required: a missing one is a caller (wire-up) programmer error, [New]
// returns an error.
type Config struct {
	// Holder is the instance identifier (KID) for the lease key and logs.
	Holder string

	// Redis is the client used to acquire the lease.
	Redis *redis.Client

	// Logger is the slog logger.
	Logger *slog.Logger

	// Spawner is the due-cadence spawn executor (see [Spawner]).
	Spawner Spawner

	// IntervalFn is the interval between ticks (hot-reload: re-read on every
	// tick). Conductor-specific, independent of reaper.interval (ADR-048 §2).
	IntervalFn func() time.Duration

	// LockTTLFn is the TTL of the Redis lease key (hot-reload between
	// re-acquires).
	LockTTLFn func() time.Duration

	// BatchFn caps the number of due Cadences per tick (hot-reload). Nil or a
	// non-positive result → [defaultBatch].
	BatchFn func() int

	// AcquireBackoff is the pause between lease-acquire attempts. Zero →
	// leaderloop's default.
	AcquireBackoff time.Duration

	// OnLeaseChange is an optional callback for leadership status changes
	// (true on acquire, false on exiting the tick loop). Nil is allowed.
	// Production wire-up supplies [ConductorMetrics.SetLeaseHeld] (lease
	// Gauge, C5).
	OnLeaseChange func(held bool)

	// Metrics are the Prometheus collectors for the spawn tick (executions /
	// spawned / errors / duration). Nil is allowed —
	// [ConductorMetrics.ObserveSpawn] no-ops on a nil receiver (scheduler
	// unit tests without the obs stack).
	Metrics *ConductorMetrics
}

// Scheduler is Conductor's root struct. One instance per keeper process.
// Created via [New], started via [Scheduler.Run].
type Scheduler struct {
	cfg Config
}

// New validates the config and returns a Scheduler. Missing required fields
// return an error: a caller (wire-up) programmer error, not a runtime
// condition.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Holder == "" {
		return nil, errors.New("conductor.New: Holder is required")
	}
	if cfg.Redis == nil {
		return nil, errors.New("conductor.New: Redis is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("conductor.New: Logger is required")
	}
	if cfg.Spawner == nil {
		return nil, errors.New("conductor.New: Spawner is required")
	}
	if cfg.IntervalFn == nil {
		return nil, errors.New("conductor.New: IntervalFn is required")
	}
	if cfg.LockTTLFn == nil {
		return nil, errors.New("conductor.New: LockTTLFn is required")
	}
	return &Scheduler{cfg: cfg}, nil
}

// Run drives the leader loop until ctx is canceled. Leadership, renewal,
// re-acquire and graceful shutdown are delegated to the generic
// [leaderloop.Loop]; Conductor is a thin consumer: the tick callback calls
// [Spawner.Run] over fresh hot-reload snapshots of interval/lock_ttl/batch.
//
// Returns nil on graceful stop (ctx.Done) and a wrapped error on fatal
// acquire-phase conditions.
func (s *Scheduler) Run(ctx context.Context) error {
	loop, err := leaderloop.New(leaderloop.Config{
		LeaseKey:       LeaseKey,
		Holder:         s.cfg.Holder,
		Redis:          s.cfg.Redis,
		Logger:         s.cfg.Logger,
		AcquireBackoff: s.cfg.AcquireBackoff,
		IntervalFn:     s.cfg.IntervalFn,
		LockTTLFn:      s.cfg.LockTTLFn,
		Tick:           s.tick,
		OnLeaseChange:  s.cfg.OnLeaseChange,
	})
	if err != nil {
		// Required fields are validated by New → this shouldn't fail here.
		// Propagated in case the contracts drift out of sync.
		return err
	}
	return loop.Run(ctx)
}

// tick is the tick callback for leaderloop: spawns due cadences via
// [Spawner]. A spawn error doesn't bring down the loop (best-effort
// background rule, parity with Reaper): logged as a warning, the next tick
// retries (the rows stay due — next_run_at isn't advanced on a spawn-tx
// error, see ADR-046 §4).
func (s *Scheduler) tick(ctx context.Context) {
	batch := defaultBatch
	if s.cfg.BatchFn != nil {
		if b := s.cfg.BatchFn(); b > 0 {
			batch = b
		}
	}
	// The duration argument isn't used in spawn (the predicate is next_run_at <= NOW()).
	start := time.Now()
	spawned, err := s.cfg.Spawner.Run(ctx, 0, batch)
	s.cfg.Metrics.ObserveSpawn(spawned, err, time.Since(start))
	if err != nil {
		s.cfg.Logger.Warn("conductor: spawn due cadence failed",
			slog.Any("error", err))
		return
	}
	if spawned > 0 {
		s.cfg.Logger.Info("conductor: spawned voyages from due cadences",
			slog.Int64("count", spawned))
	}
}
