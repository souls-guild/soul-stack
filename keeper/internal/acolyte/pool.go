// Package acolyte is a pool of apply-execution workers on a Keeper instance
// (ADR-027, Acolyte). Replaces the single run-goroutine of the owning instance:
// each Acolyte periodically polls the planned-task queue and atomically claims
// them (Ward) via `FOR UPDATE SKIP LOCKED`. Any instance's pool can pick up any
// task → execution is distributed across the cluster.
//
// The pool is claim-agnostic: it only periodically invokes an injected
// claim-callback (no-op by default), unaware of applyrun/scenario. The actual
// claim/render/dispatch lives in [scenario.ClaimRunner], wired up (setupAcolyte,
// slice 1.4.4) via [Pool.SetClaim].
//
// Lifecycle follows the pattern of keeper/internal/reaper/runner.go and
// keeper/internal/scenario/runner.go: graceful shutdown via [sync.WaitGroup] +
// ctx-cancel. Stop is two-staged (graceful drain of the Acolyte pool, ADR-027
// Phase 2): a drain signal ("stop claiming") first stops the loop, letting
// already in-flight claims finish within grace; those that don't make it are
// aborted by cancelling claim-ctx — their Ward stays in the DB (claimed/running)
// and is picked up by the recovery scan (ADR-027(i)), with no forced
// commit/rollback.
package acolyte

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// defaultPollInterval is the worker's poll-tick period when cfg.PollInterval
// is unset. Acolyte periodically scans planned tasks regardless, so polling is
// a fallback to the Summons signal (ADR-027(a)): even if a pub/sub signal is
// lost, a task is picked up on the next tick. The value is moderate: frequent
// enough that failover after an owner dies happens within seconds, and rare
// enough not to flood PG with empty claim requests on an idle cluster.
const defaultPollInterval = 2 * time.Second

// defaultDrainGrace is the Acolyte pool's graceful-drain window (ADR-027
// Phase 2) when cfg.DrainGrace is unset: from the "stop claiming" signal to
// the hard cancel of claim-ctx for workers still in flight. The value is
// moderate: enough for an already-started claim (render → MarkDispatched →
// SendApply) to finish on a healthy PG/Soul, yet not so large that it stalls
// SIGTERM shutdown on a stuck in-flight claim (it survives the restart — stays
// in the DB, its lease expires, and the recovery scan picks it up, ADR-027(i)).
const defaultDrainGrace = 5 * time.Second

// hardStopTimeout is an extra bound on waiting for workers to exit AFTER
// claim-ctx is cancelled (once grace is exhausted). A ctx-aborted claim
// unwinds quickly; this margin just guards against a false leak warning on a
// slow unwind.
const hardStopTimeout = 5 * time.Second

// ClaimFunc is the DI seam for claim logic. Called by the worker on every
// poll-tick (and on Summons-wake). Defaults to [noopClaim] — the pool idles
// until the caller wires up a real Ward claim. Wire-up (setupAcolyte, slice
// 1.4.4) sets this to [scenario.ClaimRunner.Claim] (ClaimNext→RenderForHost→
// MarkDispatched→SendApply). A returned error is logged by the worker and does
// not stop the loop: one tick failing (e.g. a transient PG outage) doesn't
// bring down the pool.
type ClaimFunc func(ctx context.Context) error

// noopClaim is the default claim-callback until the caller wires up a real
// one via [Pool.SetClaim] (setupAcolyte, slice 1.4.4).
func noopClaim(context.Context) error { return nil }

// SummonsSubscriber is the DI seam for the Redis subscription to the Summons
// signal (ADR-027(a), slice 1.3). Opens a subscription on the `apply:summons`
// topic, calling onSignal for every received signal; returns an io.Closer for
// a graceful subscription stop.
//
// The abstraction (instead of importing keeper/internal/redis directly) keeps
// acolyte independent of the Redis client: the daemon wires it to
// redis.SubscribeSummons at startup (callback = pool.Notify). If no subscriber
// is set (Redis disabled, or Acolytes>0 without cluster mode) the pool runs on
// pure poll-fallback — losing Summons acceleration, not tasks.
type SummonsSubscriber func(ctx context.Context, onSignal func()) (io.Closer, error)

// Config holds the pool's parameters. Populated from keeper.yml at startup
// (setupAcolyte: Workers ← `acolytes`, PollInterval ← `acolyte_poll_interval`,
// DrainGrace ← `acolyte_drain_grace`).
type Config struct {
	// Workers is the pool's worker count (`keeper.acolytes`). The caller only
	// starts a Pool when Workers > 0 (feature flag, see setupAcolyte); the
	// constructor requires a positive value and errors otherwise.
	Workers int

	// PollInterval is the worker's poll-tick period
	// (`keeper.acolyte_poll_interval`). Zero-value → [defaultPollInterval].
	PollInterval time.Duration

	// DrainGrace is the graceful-drain window in [Pool.Shutdown]
	// (`keeper.acolyte_drain_grace`): from the "stop claiming" signal to the
	// hard cancel of claim-ctx for workers still in flight. Zero-value →
	// [defaultDrainGrace].
	DrainGrace time.Duration
}

// Deps holds the pool's external dependencies. Logger is required; Claim is
// injected via the [Pool.SetClaim] setter after construction (slice 1.4),
// defaulting to no-op — the pool builds and starts independent of the claim
// logic's readiness.
//
// Summons is an optional subscriber to the Redis signal for planned tasks
// (slice 1.3). nil → the pool runs on poll-fallback without Summons
// acceleration (Redis disabled).
type Deps struct {
	Logger  *slog.Logger
	Summons SummonsSubscriber
}

// Pool is a pool of N Acolyte workers. One instance per keeper process.
type Pool struct {
	cfg    Config
	logger *slog.Logger

	// claim performs the Ward claim on each tick. Guarded by mu: SetClaim may
	// be called before Start, but is read by workers on every tick; mu removes
	// the race.
	mu    sync.Mutex
	claim ClaimFunc

	// wake is a buffered "planned tasks appeared" channel (Summons-wake,
	// ADR-027(a)). [Pool.Notify] posts to it non-blocking; a worker wakes on it
	// ahead of the next poll-tick. The Redis pub/sub feeding this channel is
	// slice 1.3: the subscription on `apply:summons` is opened in [Pool.Start]
	// and calls [Pool.Notify].
	wake chan struct{}

	// summons is the DI subscriber for the Summons signal; nil → poll-fallback
	// only. summonsCloser is the handle of the active subscription (closed in
	// Shutdown).
	summons       SummonsSubscriber
	summonsCloser io.Closer

	// drain is the "stop claiming" signal (Acolyte pool graceful drain,
	// ADR-027 Phase 2). Closed once by [Pool.beginDrain] on Shutdown; a worker
	// that sees it in select exits the loop WITHOUT starting a new tick. An
	// already in-flight tick is NOT aborted by this — it runs to completion
	// (or until claimCtx is cancelled once grace expires).
	drain     chan struct{}
	drainOnce sync.Once

	// claimCtx/claimCancel is the ctx the claim-callback runs under (NOT the
	// worker-lifecycle ctx). Separates the two stop stages: drain stops the
	// loop (no new claims), while claimCancel aborts an already-running claim
	// only once grace expires. An aborted claim leaves its Ward in the DB as-is
	// (claimed/running) — the lease expires, recovery picks up the task
	// (ADR-027(i)), fencing rules out double execution (ADR-027(g)).
	// Initialized in Start.
	claimCtx    context.Context
	claimCancel context.CancelFunc

	// inflight is the number of workers currently executing the
	// claim-callback. At the moment grace expires in Shutdown, its value is
	// the number of in-flight claims about to be aborted by claimCtx (used for
	// the drain-outcome log).
	inflight atomic.Int64

	wg sync.WaitGroup
}

// NewPool validates cfg/deps and returns a pool. Invalid parameters return an
// error: that's a caller programming error (setupAcolyte), not a runtime
// condition. Claim defaults to no-op; the real claim is wired up via
// [Pool.SetClaim].
func NewPool(cfg Config, deps Deps) (*Pool, error) {
	if cfg.Workers <= 0 {
		return nil, errors.New("acolyte.NewPool: Workers must be > 0")
	}
	if deps.Logger == nil {
		return nil, errors.New("acolyte.NewPool: Logger is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.DrainGrace <= 0 {
		cfg.DrainGrace = defaultDrainGrace
	}
	return &Pool{
		cfg:     cfg,
		logger:  deps.Logger,
		claim:   noopClaim,
		summons: deps.Summons,
		// Buffer of 1: coalesce a burst of Summons signals into one wake —
		// the worker only needs a single "wake up and check the queue".
		wake:  make(chan struct{}, 1),
		drain: make(chan struct{}),
	}, nil
}

// SetClaim injects the claim-callback (slice 1.4 — applyrun.ClaimNext). Safe
// to call before Start; a nil callback is ignored (stays no-op).
func (p *Pool) SetClaim(fn ClaimFunc) {
	if fn == nil {
		return
	}
	p.mu.Lock()
	p.claim = fn
	p.mu.Unlock()
}

// Notify wakes workers: "planned tasks appeared" (Summons-wake). Non-blocking
// — an extra Notify coalesces with an already-pending signal. Fed by the
// Redis pub/sub subscriber (slice 1.3); for now, a call site for future
// wire-up.
func (p *Pool) Notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Start launches cfg.Workers workers and returns immediately (async). Workers
// keep running until ctx is cancelled; wait for completion via
// [Pool.Shutdown].
//
// If a Summons subscriber is set (Deps.Summons), Start opens a subscription
// on `apply:summons` with callback = [Pool.Notify]: an incoming signal wakes
// workers ahead of the poll-tick. Subscription failure is best-effort — logged
// as a warning, and the pool keeps running on poll-fallback (losing Summons
// acceleration doesn't lose tasks). The subscription lives under the passed
// ctx and is closed in [Pool.Shutdown].
func (p *Pool) Start(ctx context.Context) {
	// claimCtx is the claim-callback's execution ctx, derived from the
	// Start-ctx. The worker lifecycle runs under the original ctx; claimCtx is
	// cancelled separately (by Shutdown once grace expires), so graceful-drain
	// can let an in-flight claim finish BEFORE the hard cancel, instead of
	// cutting it off together with the loop.
	p.claimCtx, p.claimCancel = context.WithCancel(ctx)

	if p.summons != nil {
		closer, err := p.summons(ctx, p.Notify)
		if err != nil {
			p.logger.Warn("acolyte: summons subscribe failed — poll-fallback only",
				slog.Any("error", err))
		} else {
			p.summonsCloser = closer
		}
	}

	for i := 0; i < p.cfg.Workers; i++ {
		p.wg.Add(1)
		go p.worker(ctx, i)
	}
	p.logger.Info("acolyte: pool started",
		slog.Int("workers", p.cfg.Workers),
		slog.Duration("poll_interval", p.cfg.PollInterval),
		slog.Duration("drain_grace", p.cfg.DrainGrace),
		slog.Bool("summons", p.summonsCloser != nil),
	)
}

// beginDrain closes the drain channel ("stop claiming" signal) exactly once
// and wakes workers blocked waiting on a poll-tick, so they see drain right
// away and exit the loop. Idempotent (sync.Once): a repeated Shutdown / race
// won't panic on a double close.
func (p *Pool) beginDrain() {
	p.drainOnce.Do(func() {
		close(p.drain)
		// Wake workers blocked in select on a poll-tick: on the next pass
		// they'll see drain closed and exit. Notify is non-blocking.
		p.Notify()
	})
}

// Shutdown performs a graceful drain of the Acolyte pool (ADR-027 Phase 2):
//
//  1. Closes the Summons subscription (new wake signals would be pointless).
//  2. beginDrain — workers stop entering NEW claim ticks; an already
//     in-flight tick is NOT aborted.
//  3. Waits for workers to exit within grace (cfg.DrainGrace, or the passed
//     ctx's deadline if earlier). An in-flight tick that finishes within
//     grace completes normally (claimed→dispatched or terminal — like any
//     claim).
//  4. Once grace expires — claimCancel: aborts the claim-callback of workers
//     still in flight via ctx. A claim aborted BEFORE reaching dispatched
//     leaves its Ward in the DB AS-IS (claimed, attempt/lease untouched) — no
//     forced commit/rollback: the lease expires, the recovery scan picks up
//     the task (ADR-027(i)), fencing rules out double execution (ADR-027(g)).
//     Then waits a bit more, within [hardStopTimeout].
//
// Returns ctx.Err() if the passed ctx expired before normal completion (drain
// didn't fit the window). Logs the outcome: how many in-flight claims were
// aborted by grace.
func (p *Pool) Shutdown(ctx context.Context) error {
	if p.summonsCloser != nil {
		if err := p.summonsCloser.Close(); err != nil {
			p.logger.Warn("acolyte: summons subscription close error", slog.Any("error", err))
		}
		p.summonsCloser = nil
	}

	p.beginDrain()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	// Grace window: min of cfg.DrainGrace and the passed ctx's deadline.
	grace := time.NewTimer(p.cfg.DrainGrace)
	defer grace.Stop()

	select {
	case <-done:
		// All workers exited within grace — in-flight finished normally.
		p.logger.Info("acolyte: pool drained gracefully")
		return nil
	case <-grace.C:
		// grace exhausted — abort the remaining in-flight claim via claimCtx.
	case <-ctx.Done():
		// The passed ctx (daemon's 15s shutdown timeout) expired before grace
		// — also proceed to the hard cancel.
	}

	interrupted := p.inflight.Load()
	if p.claimCancel != nil {
		p.claimCancel()
	}
	if interrupted > 0 {
		p.logger.Warn("acolyte: drain grace exceeded — in-flight claims aborted by ctx (Ward kept in DB for recovery)",
			slog.Int64("inflight_aborted", interrupted),
			slog.Duration("grace", p.cfg.DrainGrace),
		)
	}

	select {
	case <-done:
		return ctx.Err()
	case <-time.After(hardStopTimeout):
		p.logger.Warn("acolyte: workers did not stop within hard-stop timeout — leak suspected",
			slog.Duration("timeout", hardStopTimeout),
		)
		return ctx.Err()
	}
}

// worker is one Acolyte's poll-tick loop. On every tick (or Summons-wake) it
// invokes the claim-callback. Exits on ctx.Done (hard cancel) OR on drain
// (stop claiming).
//
// The stop sequence is two-staged: drain stops the loop (no new ticks start),
// but an already running [Pool.tick] runs to completion — only claimCtx,
// cancelled by Shutdown once grace expires, can abort it. That's why drain is
// checked both in the main select (don't enter a new tick) and separately
// right before the tick itself (canStartTick): drain may have fired between
// the wake/poll wakeup and the claim starting.
func (p *Pool) worker(ctx context.Context, id int) {
	defer p.wg.Done()

	t := time.NewTicker(p.cfg.PollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.drain:
			// Drain: don't start new claims, exit the loop.
			return
		case <-t.C:
			if p.canStartTick() {
				p.tick(p.claimCtx, id)
			}
		case <-p.wake:
			// Summons-wake (beginDrain also triggers Notify): check drain
			// before starting a tick — on a drain-wake we don't start a tick,
			// we head for the exit.
			if p.canStartTick() {
				p.tick(p.claimCtx, id)
			}
		}
	}
}

// canStartTick reports whether a new claim tick may start: false if drain has
// already fired. Closes the race of "woke up on wake, but drain landed
// between select and tick".
func (p *Pool) canStartTick() bool {
	select {
	case <-p.drain:
		return false
	default:
		return true
	}
}

// tick runs one claim pass under claimCtx. A claim-callback error is logged
// and does NOT stop the worker (best-effort: one tick failing doesn't bring
// down the pool). The call is wrapped by an inflight increment/decrement: at
// the moment grace expires in Shutdown, its value is the number of in-flight
// claims about to be aborted by claimCtx.
func (p *Pool) tick(ctx context.Context, id int) {
	p.mu.Lock()
	claim := p.claim
	p.mu.Unlock()

	p.inflight.Add(1)
	err := claim(ctx)
	p.inflight.Add(-1)

	if err != nil {
		// ctx.Canceled is expected on a hard drain-grace cancel (claimCtx
		// cancelled): the in-flight claim was aborted normally, the Ward stays
		// in the DB for recovery — don't log noise.
		if errors.Is(err, context.Canceled) {
			return
		}
		p.logger.Warn("acolyte: claim tick failed",
			slog.Int("worker", id),
			slog.Any("error", err),
		)
	}
}
