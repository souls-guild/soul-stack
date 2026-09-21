package toll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// sortedKeys returns the sorted keys of map[string]float64 — a shared
// helper for deterministic per-coven iteration (without it, map iteration
// would give an unstable coven choice under multiple simultaneous triggers).
func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SortedSetReader — narrow surface for reading the disconnect window from the
// shared sorted-set + trimming old entries. Narrowing it enables a fake for
// Leader unit tests without a live Redis.
type SortedSetReader interface {
	// CountInWindow — ZCOUNT the sorted-set over range [fromUnix, toUnix].
	CountInWindow(ctx context.Context, fromUnix, toUnix int64) (int64, error)
	// TrimBelow — ZREMRANGEBYSCORE [-inf, beforeUnix]. Idempotent.
	TrimBelow(ctx context.Context, beforeUnix int64) error
}

// CovenAwareReader — optional extension of [SortedSetReader] for per-coven
// grouping (ADR-038 amendment, extensions). Implemented only in the
// production adapter keeperRedisTollSortedSetReader; not required in
// unit fakes (the Leader gets nil-counters → per-coven trigger is a no-op).
//
// Returns the disconnect count in the window grouped by coven label
// (extracted from the last `|`-segment of member-value, ADR-038 schema
// `sid|kid|coven|nano`). An empty coven falls into the "" key (the same style
// as the Prometheus counter disconnectsTotal{coven=""}).
type CovenAwareReader interface {
	// CountByCovenInWindow — ZRANGEBYSCORE [fromUnix, toUnix] → group-by coven.
	// Used only when per_coven_thresholds is configured (otherwise it's one
	// extra round-trip per window). Returns map[coven]count.
	CountByCovenInWindow(ctx context.Context, fromUnix, toUnix int64) (map[string]int64, error)
}

// LeaseAcquirer — narrow surface for the Redis lease (Acquire/Renew/Release). In
// production wrapped by [keeperredis.Lease]; Leader unit tests swap in a
// fake. The Acquire method returns "leader? or the reason not" — ErrLeaseTaken
// must be recognized by the caller (the Leader sleeps and retries).
type LeaseAcquirer interface {
	Acquire(ctx context.Context, key, holder string, ttl time.Duration) (Lease, error)
}

// Lease — held lease handle. Renew returns ErrLeaseLost when the lease
// transitions to another holder. Release is always idempotent.
type Lease interface {
	Renew(ctx context.Context) error
	Release(ctx context.Context) error
}

// ErrLeaseTaken / ErrLeaseLost — sentinels re-exported from the keeperredis
// package for the Leader's common API (tests compare via errors.Is without
// importing keeperredis).
var (
	ErrLeaseTaken = errors.New("toll: lease already taken")
	ErrLeaseLost  = errors.New("toll: lease lost (no longer leader)")
)

// Notifier — narrow surface for alerting out on set/clear cluster:degraded
// (ADR-038 amendment, extensions). Implementation — [WebhookNotifier] in
// webhook.go; fakes in leader_test.go. Notify is called best-effort:
// the error is logged inside the implementation, not returned to the caller
// (the Leader must not interrupt its loop because of a webhook flap; symmetric
// with audit.Writer failure-handling).
type Notifier interface {
	// Notify sends a single alert event. The implementation determines the
	// serialization format itself (generic / pagerduty_v2 / slack) — the Leader
	// passes a normalized TollEvent.
	Notify(ctx context.Context, event TollEvent)
}

// TollEvent — normalized event form for the Notifier. A subset of the
// audit payload without operator fields.
type TollEvent struct {
	// Type — "degraded_set" / "degraded_cleared".
	Type string
	// LeaderKID — KID of the instance that raised/cleared the flag.
	LeaderKID string
	// Rate — disconnect_rate / baseline at the moment of the event.
	Rate float64
	// BaselineConnected — `souls.status='connected'` snapshot.
	BaselineConnected int64
	// Threshold — the threshold that rate crossed (top-level or per-coven).
	Threshold float64
	// WindowSeconds — length of the sliding window, in seconds.
	WindowSeconds int
	// CovenName — coven name for a per-coven trigger; empty for global.
	CovenName string
	// Timestamp — moment the trigger fired (UTC).
	Timestamp time.Time
}

// EventTypeDegradedSet / EventTypeDegradedCleared — closed-enum values for
// [TollEvent.Type]. Used by both Notifier implementations and tests.
const (
	EventTypeDegradedSet     = "degraded_set"
	EventTypeDegradedCleared = "degraded_cleared"
)

// LeaderConfig — parameters for the Leader loop.
type LeaderConfig struct {
	// KID — instance identifier (lease holder, leader_kid in the audit payload).
	KID string

	// LeaseTTL — TTL of the cluster:toll:leader lease key. Renewed every LeaseTTL/3.
	LeaseTTL time.Duration
	// AcquireRetry — pause between Acquire attempts (when another instance
	// holds the lease). <=0 → defaults to [defaultAcquireRetry] (5s).
	AcquireRetry time.Duration
	// TickInterval — period of the leader's aggregation tick (how often to read
	// the sorted-set + compute rate). <=0 → defaults to [defaultTickInterval] (5s).
	TickInterval time.Duration

	// WindowSize — sliding window over which the rate is computed (ADR-038(d)).
	WindowSize time.Duration
	// Threshold — rate threshold relative to baseline (0.20 = 20%).
	Threshold float64
	// DegradedTTL — TTL of the cluster:degraded Redis key.
	DegradedTTL time.Duration
	// ClearGrace — sustained window of low rate before clearing (asymmetric
	// hysteresis).
	ClearGrace time.Duration
	// BaselineCacheTTL — TTL of the baseline-snapshot cache (refreshed every ttl).
	BaselineCacheTTL time.Duration

	// PerCovenThresholds — optional per-coven threshold overrides (ADR-038
	// amendment, extensions). If non-empty and [LeaderDeps.SortedSet] implements
	// [CovenAwareReader], the leader additionally computes per-coven rates and
	// raises cluster:degraded when ANY per-coven threshold is exceeded.
	// A per-coven trigger stores the coven name in the audit payload (the
	// coven_name field); a global trigger stays without coven_name.
	//
	// The OR semantics (global OR per-coven) is deliberate: the global level
	// keeps reacting to broad drain (multiple covens draining a bit each),
	// per-coven catches local incidents (one DC split off).
	PerCovenThresholds map[string]float64

	// Notifier — optional webhook alert channel (ADR-038 amendment, extensions).
	// nil → degraded set/clear proceeds without alerting out (audit + gauge +
	// metrics as before). Best-effort: a Notify error is logged, but doesn't
	// block Set/Clear (cluster degraded is the primary goal).
	Notifier Notifier
}

// LeaderDeps — wiring for the Leader loop.
type LeaderDeps struct {
	Lease          LeaseAcquirer
	SortedSet      SortedSetReader
	DegradedWriter degradedWriter
	Baseline       BaselineReader
	Audit          audit.Writer
	Metrics        *Metrics
	Logger         *slog.Logger
}

const (
	defaultAcquireRetry = 5 * time.Second
	defaultTickInterval = 5 * time.Second
)

// Leader — background goroutine that runs an aggregation tick while holding
// the Redis lease (single-leader invariant, ADR-038).
//
// [Leader.Run] lifecycle:
//  1. Try Acquire `cluster:toll:leader`. Conflict → sleep AcquireRetry, retry.
//  2. After acquire — parallel renew-loop (Renew every LeaseTTL/3).
//  3. Aggregation loop: every TickInterval — ZCOUNT over the window, baseline, rate,
//     set/clear `cluster:degraded` with asymmetric hysteresis.
//  4. On ErrLeaseLost (renew) → exit the aggregation loop, attempt to re-acquire
//     (could be split-brain / leader flap).
//  5. On ctx.Done() → release the lease, exit.
type Leader struct {
	// cfgMu guards the hot-reload-able cfg fields (see [Leader.UpdateConfig]).
	// Tick reads values under RLock; UpdateConfig swaps them under Lock. The
	// KID/LeaseTTL/AcquireRetry/TickInterval/BaselineCacheTTL fields are
	// startup-only (the renew-loop / leader-election sleep timers already hold
	// captured duration copies), they are NOT applied on hot-reload: restart-required
	// fields per [ADR-021(e)], symmetric with the `logging.file` policy.
	cfgMu sync.RWMutex
	cfg   LeaderConfig
	deps  LeaderDeps

	// Asymmetric hysteresis state (leader-loop goroutine only, no mutex;
	// the only writer is that same goroutine).
	degradedSet     bool
	belowSince      time.Time // moment since which rate ≤ threshold continuously
	lastClearedRate float64
	lastBaseline    int64
}

// NewLeader validates Config/Deps and assembles a Leader. The caller (daemon)
// runs [Leader.Run] in a separate goroutine.
func NewLeader(cfg LeaderConfig, deps LeaderDeps) (*Leader, error) {
	if cfg.KID == "" {
		return nil, errors.New("toll.NewLeader: empty KID")
	}
	if cfg.LeaseTTL <= 0 {
		return nil, errors.New("toll.NewLeader: LeaseTTL must be > 0")
	}
	if cfg.WindowSize <= 0 {
		return nil, errors.New("toll.NewLeader: WindowSize must be > 0")
	}
	if cfg.Threshold <= 0 || cfg.Threshold > 1 {
		return nil, fmt.Errorf("toll.NewLeader: Threshold must be in (0, 1], got %v", cfg.Threshold)
	}
	if cfg.DegradedTTL <= 0 {
		return nil, errors.New("toll.NewLeader: DegradedTTL must be > 0")
	}
	if cfg.ClearGrace <= 0 {
		return nil, errors.New("toll.NewLeader: ClearGrace must be > 0")
	}
	if deps.Lease == nil {
		return nil, errors.New("toll.NewLeader: nil Lease")
	}
	if deps.SortedSet == nil {
		return nil, errors.New("toll.NewLeader: nil SortedSet")
	}
	if deps.DegradedWriter == nil {
		return nil, errors.New("toll.NewLeader: nil DegradedWriter")
	}
	if deps.Baseline == nil {
		return nil, errors.New("toll.NewLeader: nil Baseline")
	}
	if deps.Logger == nil {
		return nil, errors.New("toll.NewLeader: nil Logger")
	}
	if cfg.AcquireRetry <= 0 {
		cfg.AcquireRetry = defaultAcquireRetry
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = defaultTickInterval
	}
	if cfg.BaselineCacheTTL <= 0 {
		cfg.BaselineCacheTTL = cfg.WindowSize
	}
	return &Leader{cfg: cfg, deps: deps}, nil
}

// UpdateConfig atomically swaps the hot-reload-able fields of the Leader config
// ([ADR-021](docs/architecture.md) hot-reload). Safe to call concurrently
// with the tick-loop: the tick reads a snapshot of its fields under RLock, the
// next snapshot will see the already-updated values.
//
// Updated fields: Threshold, WindowSize, DegradedTTL, ClearGrace, PerCovenThresholds,
// Notifier. KID/LeaseTTL/AcquireRetry/TickInterval/BaselineCacheTTL are
// restart-required (captured in the renew-loop / leader-election sleep timers),
// the reload config-validation compares them to the current values and silently ignores them.
//
// Notifier is swapped by pointer: the caller (daemon) assembles a new
// [WebhookNotifier] on a `toll.webhook.*` mutation (vault-resolve, URL, etc.) and
// passes it here. The old notifier is GC'd naturally (no resources require
// an explicit Close: http.Client only holds an idle-conn pool).
//
// Field validation mirrors [NewLeader]: an invalid value returns an error
// WITHOUT applying the swap (the old snapshot remains current — a parallel to
// [Store.Reload] semantics).
func (l *Leader) UpdateConfig(newCfg LeaderConfig) error {
	if newCfg.WindowSize <= 0 {
		return errors.New("toll.Leader.UpdateConfig: WindowSize must be > 0")
	}
	if newCfg.Threshold <= 0 || newCfg.Threshold > 1 {
		return fmt.Errorf("toll.Leader.UpdateConfig: Threshold must be in (0, 1], got %v", newCfg.Threshold)
	}
	if newCfg.DegradedTTL <= 0 {
		return errors.New("toll.Leader.UpdateConfig: DegradedTTL must be > 0")
	}
	if newCfg.ClearGrace <= 0 {
		return errors.New("toll.Leader.UpdateConfig: ClearGrace must be > 0")
	}

	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	l.cfg.WindowSize = newCfg.WindowSize
	l.cfg.Threshold = newCfg.Threshold
	l.cfg.DegradedTTL = newCfg.DegradedTTL
	l.cfg.ClearGrace = newCfg.ClearGrace
	l.cfg.PerCovenThresholds = newCfg.PerCovenThresholds
	l.cfg.Notifier = newCfg.Notifier
	return nil
}

// CurrentNotifier returns the current [Notifier] under RLock. Needed by the
// caller (daemon-applyTollReload), which skips the webhook recycle when there's
// no diff and passes the same notifier back into [Leader.UpdateConfig].
func (l *Leader) CurrentNotifier() Notifier {
	l.cfgMu.RLock()
	defer l.cfgMu.RUnlock()
	return l.cfg.Notifier
}

// Run — the blocking leader loop. Exits on ctx.Done(). The caller (daemon)
// gates cleanup with a LIFO stack: cancel ctx → join goroutine.
func (l *Leader) Run(ctx context.Context) {
	baseline := newCachedBaseline(l.deps.Baseline, l.cfg.BaselineCacheTTL)
	for {
		if ctx.Err() != nil {
			return
		}
		lease, err := l.deps.Lease.Acquire(ctx, LeaseKey, l.cfg.KID, l.cfg.LeaseTTL)
		if err != nil {
			if errors.Is(err, ErrLeaseTaken) {
				l.deps.Logger.Debug("toll: lease held by another keeper — sleeping",
					slog.Duration("retry_in", l.cfg.AcquireRetry))
			} else {
				l.deps.Logger.Warn("toll: lease acquire failed — sleeping",
					slog.Any("error", err),
					slog.Duration("retry_in", l.cfg.AcquireRetry))
			}
			if !sleepCtx(ctx, l.cfg.AcquireRetry) {
				return
			}
			continue
		}

		l.deps.Logger.Info("toll: leader-election won",
			slog.String("kid", l.cfg.KID),
			slog.Duration("lease_ttl", l.cfg.LeaseTTL))
		l.deps.Metrics.SetLeaderActive(true)

		l.runAsLeader(ctx, lease, baseline)
		l.deps.Metrics.SetLeaderActive(false)
		// On any exit from runAsLeader: reset the cluster_degraded gauge
		// to 0 (this instance is no longer leader, nothing to set). The actual flag
		// in Redis either still stands (TTL will expire it) or was cleared (our ClearDegraded below).
		l.deps.Metrics.SetClusterDegraded(false)
	}
}

// runAsLeader — internal loop while holding the lease. Exits on:
//   - ctx.Done() (graceful shutdown) → Release + return;
//   - ErrLeaseLost from renew (split-brain) → Release + return (the outer Run
//     will attempt to re-acquire).
func (l *Leader) runAsLeader(ctx context.Context, lease Lease, baseline *cachedBaseline) {
	defer func() {
		// Detached release-ctx (as in keeperredis.eventstream): the stream-ctx
		// may already be cancelled (graceful shutdown), but the lease still needs to be released.
		relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := lease.Release(relCtx); err != nil {
			l.deps.Logger.Warn("toll: lease release failed",
				slog.Any("error", err))
		}
	}()

	// Renew goroutine: periodically calls Renew. On ErrLeaseLost it signals the
	// main loop by cancelling renewCtx → the inner select will exit.
	renewCtx, cancelRenew := context.WithCancel(ctx)
	defer cancelRenew()
	renewDone := make(chan error, 1)
	go func() {
		every := l.cfg.LeaseTTL / 3
		if every <= 0 {
			every = l.cfg.LeaseTTL
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-renewCtx.Done():
				renewDone <- nil
				return
			case <-t.C:
				if err := lease.Renew(renewCtx); err != nil {
					if errors.Is(err, ErrLeaseLost) {
						renewDone <- ErrLeaseLost
						return
					}
					l.deps.Logger.Warn("toll: lease renew failed",
						slog.Any("error", err))
				}
			}
		}
	}()

	tick := time.NewTicker(l.cfg.TickInterval)
	defer tick.Stop()

	// First tick immediately: the operator shouldn't have to wait TickInterval
	// after leader election (important during failover).
	l.aggregationTick(ctx, baseline)

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-renewDone:
			if errors.Is(err, ErrLeaseLost) {
				l.deps.Logger.Warn("toll: lease lost — stepping down")
			}
			return
		case <-tick.C:
			l.aggregationTick(ctx, baseline)
		}
	}
}

// tickSnapshot — snapshot of the hot-reload-able cfg fields at tick time. Taken
// once at the start of [Leader.aggregationTick]; the tick works with local
// copies, without blocking [Leader.UpdateConfig] during Redis/audit calls.
type tickSnapshot struct {
	windowSize         time.Duration
	threshold          float64
	degradedTTL        time.Duration
	clearGrace         time.Duration
	perCovenThresholds map[string]float64
	notifier           Notifier
	kid                string
}

// snapshotCfg returns a snapshot of the hot-reload-able fields under RLock. The
// PerCovenThresholds map is NOT shallow-copied: UpdateConfig swaps the reference
// atomically (an old Leader tick sees the old map, a new one sees the new one),
// no in-place map mutation occurs.
func (l *Leader) snapshotCfg() tickSnapshot {
	l.cfgMu.RLock()
	defer l.cfgMu.RUnlock()
	return tickSnapshot{
		windowSize:         l.cfg.WindowSize,
		threshold:          l.cfg.Threshold,
		degradedTTL:        l.cfg.DegradedTTL,
		clearGrace:         l.cfg.ClearGrace,
		perCovenThresholds: l.cfg.PerCovenThresholds,
		notifier:           l.cfg.Notifier,
		kid:                l.cfg.KID,
	}
}

// aggregationTick — one pass of the aggregation loop: ZCOUNT over the window, baseline,
// rate, set/clear cluster:degraded.
func (l *Leader) aggregationTick(ctx context.Context, baseline *cachedBaseline) {
	snap := l.snapshotCfg()
	now := time.Now()
	from := now.Add(-snap.windowSize).Unix()
	to := now.Unix()

	count, err := l.deps.SortedSet.CountInWindow(ctx, from, to)
	if err != nil {
		l.deps.Logger.Warn("toll: ZCOUNT failed — skipping tick",
			slog.Any("error", err))
		return
	}

	base, baseErr := baseline.get(ctx, now)
	if baseErr != nil {
		if base == 0 {
			// No stale value — skip the tick (a false-positive is worse than
			// losing one tick).
			l.deps.Logger.Warn("toll: baseline fetch failed and no stale — skipping tick",
				slog.Any("error", baseErr))
			return
		}
		// Stale-fallback: continue, but log it.
		l.deps.Logger.Warn("toll: baseline fetch failed — using stale",
			slog.Any("error", baseErr),
			slog.Int64("stale_baseline", base))
	}

	// Trim — better to do AFTER ZCOUNT (if ZCOUNT fails during a flap, the window is
	// still untouched, the next tick will retry). Idempotent.
	if err := l.deps.SortedSet.TrimBelow(ctx, from); err != nil {
		l.deps.Logger.Debug("toll: ZREMRANGEBYSCORE failed (non-fatal)",
			slog.Any("error", err))
	}

	// Protection against division by zero: baseline=0 → don't evaluate (a fresh
	// cluster with no registered Souls, or all of them pending). We treat ratio=0
	// as "not exceeded" (no degraded flags).
	var rate float64
	if base > 0 {
		rate = float64(count) / float64(base)
	}
	l.lastClearedRate = rate
	l.lastBaseline = base

	// Per-coven trigger is checked when configuration is present AND the reader
	// is able to group by coven. If global already exceeded the threshold,
	// the per-coven analysis isn't strictly necessary (cluster:degraded will be
	// set anyway), but we still do it to refine coven_name in the audit/webhook
	// payload (a per-coven trigger gives the operator a more precise diagnosis).
	triggeredCoven, triggeredThr := l.maybePerCovenTrigger(ctx, snap.perCovenThresholds, from, to, base)

	exceededGlobal := rate > snap.threshold
	exceededAny := exceededGlobal || triggeredCoven != ""

	switch {
	case exceededAny:
		// Threshold exceeded — set/refresh degraded. Reset belowSince (the grace
		// window stops accumulating).
		l.belowSince = time.Time{}
		if err := l.deps.DegradedWriter.SetDegraded(ctx, snap.kid, snap.degradedTTL); err != nil {
			l.deps.Logger.Warn("toll: SET cluster:degraded failed",
				slog.Any("error", err))
			return
		}
		l.deps.Metrics.SetClusterDegraded(true)
		if !l.degradedSet {
			l.degradedSet = true
			// Pick the "causal" threshold and coven for diag/audit/webhook:
			// a global trigger outweighs per-coven, so that on a double
			// breach the logs show the global rate. The per-coven trigger
			// is surfaced only if global didn't fire (a local incident).
			triggerThreshold := snap.threshold
			triggerCoven := ""
			if !exceededGlobal {
				triggerThreshold = triggeredThr
				triggerCoven = triggeredCoven
			}
			l.deps.Logger.Error("toll: cluster degraded — write-API blocked",
				slog.Float64("rate", rate),
				slog.Float64("threshold", triggerThreshold),
				slog.String("coven", triggerCoven),
				slog.Int64("disconnects", count),
				slog.Int64("baseline_connected", base),
				slog.Duration("window", snap.windowSize))
			auditDegradedSet(ctx, l.deps.Audit, l.deps.Logger,
				snap.kid, rate, base, triggerThreshold, int(snap.windowSize.Seconds()), triggerCoven)
			l.notifyWith(ctx, snap.notifier, TollEvent{
				Type:              EventTypeDegradedSet,
				LeaderKID:         snap.kid,
				Rate:              rate,
				BaselineConnected: base,
				Threshold:         triggerThreshold,
				WindowSeconds:     int(snap.windowSize.Seconds()),
				CovenName:         triggerCoven,
				Timestamp:         now.UTC(),
			})
		}
	default:
		// rate ≤ threshold: if degraded was set, accumulate the grace window.
		if !l.degradedSet {
			// Nothing to clear; keep belowSince inactive.
			return
		}
		if l.belowSince.IsZero() {
			l.belowSince = now
			l.deps.Logger.Info("toll: rate dropped below threshold — grace window started",
				slog.Float64("rate", rate),
				slog.Float64("threshold", snap.threshold),
				slog.Duration("grace", snap.clearGrace))
			return
		}
		if now.Sub(l.belowSince) < snap.clearGrace {
			// Grace hasn't expired yet — don't clear (but the degraded TTL in
			// Redis will still expire naturally via DegradedTTL — this is a
			// by-design fail-safe, the leader isn't required to hold it explicitly).
			return
		}
		// Grace period elapsed — clearing.
		if err := l.deps.DegradedWriter.ClearDegraded(ctx); err != nil {
			l.deps.Logger.Warn("toll: DEL cluster:degraded failed",
				slog.Any("error", err))
			return
		}
		l.deps.Metrics.SetClusterDegraded(false)
		l.degradedSet = false
		l.belowSince = time.Time{}
		l.deps.Logger.Info("toll: cluster degraded cleared after grace",
			slog.Float64("rate", rate),
			slog.Int64("baseline_connected", base),
			slog.Duration("grace", snap.clearGrace))
		auditDegradedCleared(ctx, l.deps.Audit, l.deps.Logger,
			snap.kid, rate, base, int(snap.clearGrace.Seconds()))
		l.notifyWith(ctx, snap.notifier, TollEvent{
			Type:              EventTypeDegradedCleared,
			LeaderKID:         snap.kid,
			Rate:              rate,
			BaselineConnected: base,
			Threshold:         snap.threshold,
			WindowSeconds:     int(snap.windowSize.Seconds()),
			Timestamp:         now.UTC(),
		})
	}
}

// maybePerCovenTrigger — optional per-coven analysis (ADR-038 amendment, extensions).
// Returns (coven, threshold) for the first found coven whose rate exceeded
// the configured per-coven threshold; ("", 0) — no trigger / per-coven isn't
// configured / SortedSet doesn't support grouping.
//
// No errors surface outward: per-coven trigger is an addition, it must not break
// the global loop on a Redis flap during ZRANGEBYSCORE.
//
// perCovenThresholds is passed via the tick-snapshot (see [tickSnapshot]): reading
// `l.cfg.PerCovenThresholds` directly here would race with [Leader.UpdateConfig].
func (l *Leader) maybePerCovenTrigger(ctx context.Context, perCovenThresholds map[string]float64, from, to int64, base int64) (string, float64) {
	if len(perCovenThresholds) == 0 || base <= 0 {
		return "", 0
	}
	reader, ok := l.deps.SortedSet.(CovenAwareReader)
	if !ok {
		return "", 0
	}
	counts, err := reader.CountByCovenInWindow(ctx, from, to)
	if err != nil {
		l.deps.Logger.Debug("toll: per-coven CountByCoven failed (non-fatal)",
			slog.Any("error", err))
		return "", 0
	}
	// Deterministic order when multiple coven triggers happen simultaneously
	// (several exceed at once): iterate in sorted config-key order.
	// Without sorting, the choice would depend on map iteration, which is
	// unstable for tests and logs. We iterate over the thresholds (a small
	// set) and check against counts.
	for _, coven := range sortedKeys(perCovenThresholds) {
		thr := perCovenThresholds[coven]
		c, present := counts[coven]
		if !present {
			continue
		}
		rate := float64(c) / float64(base)
		if rate > thr {
			return coven, thr
		}
	}
	return "", 0
}

// notifyWith — best-effort wrapper: nil-safe + recovers from a panic in the
// implementation (a webhook-side bug must not crash the leader loop). notifier
// is passed via the tick-snapshot to avoid a race with [Leader.UpdateConfig].
func (l *Leader) notifyWith(ctx context.Context, n Notifier, ev TollEvent) {
	if n == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			l.deps.Logger.Warn("toll: notifier panic recovered",
				slog.Any("panic", r),
				slog.String("event_type", ev.Type))
		}
	}()
	n.Notify(ctx, ev)
}

// sleepCtx sleeps for the given duration, interruptible via ctx.Done(). Returns
// true if the sleep ran to completion; false if ctx was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
