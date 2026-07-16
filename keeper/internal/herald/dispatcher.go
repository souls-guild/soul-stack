package herald

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// DefaultRuleCacheTTL is TTL of enabled-Tiding rules snapshot in dispatcher cache.
// Dispatcher does NOT go to PG on each event (hot path of audit write-path):
// keeps rule snapshot, updating it no more than once per TTL (ADR-052(c) —
// "dispatcher matches against enabled rules").
//
// 15s is MVP compromise: Tiding edits rare (CRUD-API appears only in S4),
// 15-second lag of new rule application acceptable. Inline invalidation by
// CRUD hook in same process — [Dispatcher.InvalidateRules] (S4 calls it from
// CRUD handlers). Cross-keeper invalidation (other Keeper created Tiding) —
// open question S4 (Redis pub/sub, pattern RBAC/service-registry
// invalidation); in S2 doesn't block: TTL guarantees convergence in ≤15s.
const DefaultRuleCacheTTL = 15 * time.Second

// RuleSource is source of enabled Tiding rules for dispatcher. In production —
// adapter over CRUD layer (SELECT ... WHERE enabled=true, partial index
// tidings_enabled_idx). Narrow interface for unit-testable matching without
// PG (like ExecQueryRower in CRUD).
type RuleSource interface {
	// EnabledTidings returns current snapshot of ENABLED Tiding rules.
	EnabledTidings(ctx context.Context) ([]*Tiding, error)
}

// Dispatcher matches run audit event against enabled Tiding rules and
// enqueues [DeliveryJob] to [DeliveryQueue] on each match (ADR-052(c), S2 — without
// delivery). Rule snapshot cached with TTL ([DefaultRuleCacheTTL]) — hot
// match path doesn't go to PG.
//
// Thread-safe: Dispatch called from tap goroutine (single), but cache under
// RWMutex for parallel InvalidateRules from CRUD handlers (S4).
type Dispatcher struct {
	source RuleSource
	queue  DeliveryQueue
	logger *slog.Logger
	ttl    time.Duration
	clock  func() time.Time

	mu        sync.RWMutex
	cached    []*Tiding
	cachedAt  time.Time
	cacheInit bool

	metrics *DispatcherMetrics
}

// DispatcherConfig are [Dispatcher] construction parameters.
type DispatcherConfig struct {
	Source RuleSource
	Queue  DeliveryQueue
	Logger *slog.Logger
	// TTL of rule snapshot; <= 0 → [DefaultRuleCacheTTL].
	TTL time.Duration
}

// NewDispatcher constructs Dispatcher. Source and Queue are required.
func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultRuleCacheTTL
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Dispatcher{
		source: cfg.Source,
		queue:  cfg.Queue,
		logger: logger,
		ttl:    ttl,
		clock:  time.Now,
	}
}

// SetQueue does late-binding substitution of delivery queue. Init-order of keeper:
// tap + dispatcher built in setupAudit (with fallback LogDeliveryQueue), Redis
// starts later (setupRedis); real [RedisDeliveryQueue] passed here after — with same
// late-binding technique as SetMetrics. Under cache write-lock (same mu) — Dispatch
// reads queue without separate sync, so substitution serialized with rules cache.
// nil-receiver / nil-queue → no-op.
func (d *Dispatcher) SetQueue(q DeliveryQueue) {
	if d == nil || q == nil {
		return
	}
	d.mu.Lock()
	d.queue = q
	d.mu.Unlock()
}

// SetMetrics does late-binding injection of dispatcher metrics. Init-order of keeper:
// audit-writer (with tap) built before metrics-registry, so metrics
// passed after (pattern vault.SetMetrics / rbacHolder.SetMetrics).
// nil-receiver / nil-metrics — no-op.
func (d *Dispatcher) SetMetrics(m *DispatcherMetrics) {
	if d == nil {
		return
	}
	d.metrics = m
}

// InvalidateRules flushes rule cache — next Dispatch will reread
// snapshot from source. Called by CRUD handlers of Tiding/Herald (S4) after
// create/update/delete for immediate change application in this process.
func (d *Dispatcher) InvalidateRules() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.cacheInit = false
	d.cached = nil
	d.mu.Unlock()
}

// Dispatch matches event against enabled rules and enqueues DeliveryJob on
// each match. Best-effort: errors from rule source / enqueue
// logged but not propagated (tap shouldn't affect audit write-path).
//
// Non-run-scope event (any keeper-event outside run scopes) filtered
// cheaply before rule load: matchEventType won't fire, but area check
// saves extra rule traversal for CRUD/lifecycle noise.
func (d *Dispatcher) Dispatch(ctx context.Context, event *audit.Event) {
	if d == nil || event == nil {
		return
	}
	// Loop-guard (ADR-052(d)): own delivery terminals `herald.*` themselves
	// go through audit-writer → tap → here. Filter them before rule load —
	// "notification about notification" shouldn't become loop (guard over
	// CRUD validation, see isHeraldOwnEvent).
	if isHeraldOwnEvent(event.EventType) {
		return
	}
	rules, err := d.rules(ctx)
	if err != nil {
		d.logger.Warn("herald: dispatch skipped, rules load failed",
			slog.String("event_type", string(event.EventType)),
			slog.Any("error", err))
		d.metrics.observeError()
		return
	}

	// Snapshot queue under RLock; late-binding SetQueue (setupRedis) may substitute
	// it concurrently with Dispatch (tap-consumer goroutine).
	d.mu.RLock()
	queue := d.queue
	d.mu.RUnlock()

	// occurred_at of match moment: event.CreatedAt is usually zero; write-path
	// initiators rely on PG `DEFAULT NOW()` (auditpg.Write writes time into
	// DB row, but NOT back to *event), so by tap observation time field is
	// zero. Use match time (d.clock, job enqueue moment, nearest observable time
	// to audit INSERT), and use CreatedAt only when initiator set it explicitly
	// (rare case). Otherwise occurred_at in webhook body would be 0001-01-01
	// (Herald live-smoke bug).
	occurredAt := occurredAt(event, d.clock())

	matched := 0
	for _, t := range rules {
		if !matchTiding(t, event) {
			continue
		}
		matched++
		job := &DeliveryJob{
			ID:            audit.NewULID(),
			Herald:        t.Herald,
			Tiding:        t.Name,
			EventType:     event.EventType,
			CorrelationID: event.CorrelationID,
			OccurredAt:    occurredAt,
			PayloadCopy:   copyPayload(event.Payload),
			// Annotations/Projection are copied into job from Tiding, but dispatcher
			// does NOT apply them (ADR-052(h): merge/projection is off-path in worker
			// when building webhookPayload, N3). This only transfers fields; worker (N3)
			// reads them.
			Annotations: t.Annotations,
			Projection:  t.Projection,
		}
		if err := queue.Enqueue(ctx, job); err != nil {
			d.logger.Warn("herald: enqueue delivery job failed",
				slog.String("tiding", t.Name),
				slog.String("herald", t.Herald),
				slog.Any("error", err))
			d.metrics.observeError()
			continue
		}
	}
	d.metrics.observeDispatch(matched)
}

// rules returns snapshot of enabled rules, rereading from source on cold
// cache or expired TTL. Under RWMutex: fast path (read-lock) on warm cache.
func (d *Dispatcher) rules(ctx context.Context) ([]*Tiding, error) {
	now := d.clock()

	d.mu.RLock()
	if d.cacheInit && now.Sub(d.cachedAt) < d.ttl {
		rules := d.cached
		d.mu.RUnlock()
		return rules, nil
	}
	d.mu.RUnlock()

	d.mu.Lock()
	defer d.mu.Unlock()
	// Recheck under write-lock: another call may have refreshed cache while
	// we waited for lock (single-flight without separate group; refresh is cheap).
	if d.cacheInit && now.Sub(d.cachedAt) < d.ttl {
		return d.cached, nil
	}
	rules, err := d.source.EnabledTidings(ctx)
	if err != nil {
		// Do not touch cache: on source failure keep previous snapshot (if any),
		// but return error so Dispatch logs it and skips event.
		// Previous snapshot remains valid for following events until convergence.
		return nil, err
	}
	d.cached = rules
	d.cachedAt = now
	d.cacheInit = true
	return rules, nil
}

// matchTiding is true if event passes ALL conditions of Tiding rule
// (ADR-052(c)): at least one event_type pattern covers event type AND
// (only_failures implies failed event) AND (only_changes implies event carries changes)
// AND incarnation/cadence/task selectors (if set) match. task selector
// (ADR-052 section l) matches only incarnation.run_completed with requested address in
// changed_tasks (see matchTask).
//
// Disabled rules do not reach here; source returns only enabled
// (tidings_enabled_idx). Empty EventTypes impossible (CHECK + validation).
func matchTiding(t *Tiding, event *audit.Event) bool {
	if t == nil {
		return false
	}
	if !matchAnyEventType(t.EventTypes, event.EventType) {
		return false
	}
	if t.OnlyFailures && !isFailureEvent(event.EventType) {
		return false
	}
	if t.OnlyChanges && !hasChanges(event.EventType, event.Payload) {
		return false
	}
	// Ephemeral rule (ADR-052(g)) is narrowed to ITS run: VoyageID selector
	// matches only events of that Voyage. Permanent rules (VoyageID nil)
	// pass as before; matchVoyage returns true.
	if !matchVoyage(t.VoyageID, event.CorrelationID, event.Payload) {
		return false
	}
	if !matchIncarnation(t.Incarnation, event.EventType, event.Payload) {
		return false
	}
	if !matchCadence(t.Cadence, event.EventType, event.Payload) {
		return false
	}
	if !matchTask(t.Task, event.EventType, event.Payload) {
		return false
	}
	return true
}

func matchAnyEventType(patterns []string, et audit.EventType) bool {
	for _, p := range patterns {
		if matchEventType(p, et) {
			return true
		}
	}
	return false
}

// occurredAt chooses occurred_at for DeliveryJob: explicit event.CreatedAt if
// initiator set it, otherwise fallback to now (match moment). Fallback reason:
// auditpg.Write leaves event.CreatedAt zero when relying on PG
// `DEFAULT NOW()`, and tap observes same pointer after INSERT (see
// call in Dispatch). Returned time is UTC.
func occurredAt(event *audit.Event, now time.Time) time.Time {
	if !event.CreatedAt.IsZero() {
		return event.CreatedAt.UTC()
	}
	return now.UTC()
}

// copyPayload copies ONLY top level of payload map: new map with same
// values by reference. Nested map/slice values are NOT isolated; deep mutation
// (for example, payload["summary"].(map)["x"]=...) is visible through both copy
// and original. Deliberate trade-off: payload here is NOT masked yet (raw
// in-process *event, tap sees it before masking), and this is only read-only snapshot for
// delivery; secret masking happens later on delivery in worker.buildPayload
// (MaskSecrets). Deep copying on hot write-path is not justified; copy
// protects only from replacing/adding top-level keys on shared pointer.
// nil -> nil.
func copyPayload(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}
