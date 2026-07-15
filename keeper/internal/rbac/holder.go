package rbac

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultRefreshInterval is the TTL reload period for the RBAC snapshot from
// the DB (ADR-028(d), Phase 1 = B1 TTL poll). Role/membership mutations are
// rare and the staleness window is small — seconds are acceptable. Redis
// pub/sub invalidation (B2) is Phase 3, see [Holder.WatchInvalidations].
const DefaultRefreshInterval = 10 * time.Second

// InvalidationSource is the subscription surface for cluster-wide RBAC
// invalidation (ADR-028(d), B2). Implemented in `keeper run` by an adapter
// over [keeperredis.SubscribeRBACInvalidate]; declared as an interface so
// [Holder.WatchInvalidations] can be tested without Redis (fake source).
//
// Watch blocks until ctx.Done(), calling onInvalidate for every received
// invalidate message (self-origin is already filtered out by the source).
// A returned error is a fatal subscription problem; the caller (Holder) logs
// it and degrades to plain TTL polling (fail-soft).
type InvalidationSource interface {
	Watch(ctx context.Context, onInvalidate func()) error
}

// SnapshotSource is the loading surface for the RBAC snapshot from the DB.
// Implemented by a function over [LoadSnapshot] + pool; declared as an
// interface so Holder can be tested without Postgres (fake source).
type SnapshotSource interface {
	Load(ctx context.Context) (*Snapshot, error)
}

// PoolSource is a SnapshotSource backed by a pgx pool (the real source in
// `keeper run`).
type PoolSource struct {
	DB ExecQueryRower
}

// Load reads the snapshot with three SELECTs via [LoadSnapshot].
func (s PoolSource) Load(ctx context.Context) (*Snapshot, error) {
	return LoadSnapshot(ctx, s.DB)
}

// Holder owns the current [*Enforcer], built from a DB snapshot (ADR-028(d)).
//
// Refresh strategy (Phase 1 = B1, TTL poll):
//   - On startup, [NewHolder] synchronously builds the first Enforcer from
//     the DB (fatal on error — the daemon shouldn't come up with an
//     empty/broken RBAC).
//   - A background goroutine ([Run]) reloads the snapshot every
//     refreshInterval. A reload error doesn't clear the snapshot: Holder
//     keeps the previous Enforcer and logs a warning (a DB blip shouldn't
//     turn everyone into default-deny).
//
// Redis pub/sub invalidation (B2) is Phase 3, NOT implemented here.
//
// Concurrency: the current Enforcer is stored under a Mutex; Check and
// refresh are serialized over a short critical section (pointer swap). Usage
// profile is admin-API (tens of RPS max), not the data path.
type Holder struct {
	src      SnapshotSource
	interval time.Duration
	logger   *slog.Logger

	mu  sync.Mutex
	cur *Enforcer

	// metrics is the keeper_rbac_* descriptor, injected via [SetMetrics]
	// during the daemon's setup phase (after the registry is created, which
	// comes up after NewHolder). nil until wire-up and in tests/bootstrap —
	// all Observe* calls are no-ops (a method on nil *RBACMetrics is a no-op,
	// so a nil Load is safe).
	//
	// atomic.Pointer rather than a plain field: SetMetrics is called during
	// the daemon's setup phase, already AFTER the Run goroutine starts
	// (`go Holder.Run` in setupRBAC, while SetMetrics comes later in
	// setupMetricsRegistry). In that window, Run concurrently reads metrics
	// from Refresh/ObserveInvalidation — a plain field would be a data race.
	// Check stays hot here via Load, not a mutex (cheaper, non-blocking).
	metrics atomic.Pointer[RBACMetrics]
}

// SetMetrics attaches the keeper_rbac_* descriptor. Safe to call
// concurrently with background readers ([Run]/[WatchInvalidations]) via
// atomic.Pointer. nil receiver is a no-op (symmetric with Holder's other
// nil-safe wrappers). Called from the daemon's `setupMetricsRegistry` after
// [RegisterRBACMetrics]; before that call, metrics are nil (Load returns
// nil) and aren't published.
func (h *Holder) SetMetrics(m *RBACMetrics) {
	if h == nil {
		return
	}
	h.metrics.Store(m)
}

// NewHolder builds the initial Enforcer from a DB snapshot. An initial-load
// error is fatal (err is returned to the caller; the daemon fails at startup
// if the RBAC schema is unreachable/broken).
//
// nil src is allowed — Holder behaves like an empty snapshot (default deny),
// and the background refresh becomes a no-op. Used by tests and code paths
// where the pool isn't initialized yet; in practice src is always non-nil in
// `keeper run`.
//
// interval <= 0 → [DefaultRefreshInterval]. logger nil → slog.Default().
func NewHolder(ctx context.Context, src SnapshotSource, interval time.Duration, logger *slog.Logger) (*Holder, error) {
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	h := &Holder{
		src:      src,
		interval: interval,
		logger:   logger,
	}
	if src == nil {
		enf, err := NewEnforcerFromSnapshot(nil)
		if err != nil {
			return nil, err
		}
		h.cur = enf
		return h, nil
	}
	snap, err := src.Load(ctx)
	if err != nil {
		return nil, err
	}
	enf, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		return nil, err
	}
	h.cur = enf
	return h, nil
}

// Run starts the background TTL reload of the snapshot until ctx is
// cancelled (Phase 1, B1). Blocking — the caller runs it in its own
// goroutine. With nil src, it returns immediately (nothing to reload).
//
// A reload error is logged (warn) and does NOT change the active Enforcer —
// a staleness window is the lesser evil compared to "DB blip → entire
// cluster goes default-deny".
func (h *Holder) Run(ctx context.Context) {
	if h.src == nil {
		return
	}
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.refresh(ctx)
		}
	}
}

// WatchInvalidations subscribes to cluster-wide RBAC invalidation (B2 = B1 +
// pub/sub, ADR-028(d)) and calls [Holder.refresh] on every signal
// (near-instant snapshot reload from the DB, instead of waiting for [Run]'s
// TTL poll). Blocking — the caller runs it in its own goroutine; it returns
// on ctx.Done().
//
// TTL polling ([Run]) is NOT replaced: pub/sub has no persistence (a
// dropped message, a reconnect) → the next [Run] tick still reloads the
// snapshot. This is a fail-soft layer on top of that fallback.
//
// nil src ([Holder] built without a DB source) or a nil invalidator → no-op
// (nothing to reload / nowhere to subscribe). A subscribe error is logged
// (warn) and does NOT bring down the daemon — RBAC keeps updating via TTL
// polling.
func (h *Holder) WatchInvalidations(ctx context.Context, src InvalidationSource) {
	if h.src == nil || src == nil {
		return
	}
	err := src.Watch(ctx, func() {
		h.metrics.Load().ObserveInvalidation()
		h.refresh(ctx)
	})
	if err != nil && ctx.Err() == nil {
		h.logger.Warn("rbac: подписка на cluster-инвалидацию завершилась с ошибкой, остаётся TTL-poll",
			slog.Any("error", err),
		)
	}
}

// Refresh forcibly reloads the snapshot (lazy path / tests). Returns a
// load/parse error; the active Enforcer is unchanged on error.
//
// Metrics keeper_rbac_snapshot_*: the whole rebuild (Load +
// NewEnforcerFromSnapshot) is timed, the failure phase is distinguished
// explicitly (load/parse), and on success the timestamp + role/operator
// counts from the built enforcer are recorded.
func (h *Holder) Refresh(ctx context.Context) error {
	if h.src == nil {
		return nil
	}
	m := h.metrics.Load()
	start := time.Now()
	snap, err := h.src.Load(ctx)
	if err != nil {
		m.ObserveRebuildError(time.Since(start), rebuildErrorLoad)
		return err
	}
	enf, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		m.ObserveRebuildError(time.Since(start), rebuildErrorParse)
		return err
	}
	m.ObserveRebuildSuccess(time.Since(start), enf.RoleCount(), enf.OperatorCount())
	h.mu.Lock()
	h.cur = enf
	h.mu.Unlock()
	return nil
}

// refresh is the internal best-effort reload for the background goroutine:
// logs the error, doesn't propagate it.
func (h *Holder) refresh(ctx context.Context) {
	if err := h.Refresh(ctx); err != nil {
		h.logger.Warn("rbac: TTL-refresh снимка из БД не удался, оставлен прежний enforcer",
			slog.Any("error", err),
		)
	}
}

// current returns the current Enforcer under the Mutex.
func (h *Holder) current() *Enforcer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur
}

// Check delegates to the current enforcer. See [Enforcer.Check]. This is the
// sole permission-check point in `keeper run` (api-middleware and MCP get
// Holder as a PermissionChecker), so keeper_rbac_checks_total is incremented
// here — the hot path is unchanged, just one nil-safe counter Inc added.
func (h *Holder) Check(aid, resource, action string, context map[string]string) error {
	err := h.current().Check(aid, resource, action, context)
	h.metrics.Load().ObserveCheck(err)
	return err
}

// HasWildcard reports whether AID has at least one `*` permission through
// any role in the current snapshot. See [Enforcer.HasWildcard].
func (h *Holder) HasWildcard(aid string) bool {
	return h.current().HasWildcard(aid)
}

// ClusterAdmins is the list of AIDs with an active wildcard permission in
// the current snapshot. See [Enforcer.ClusterAdmins].
func (h *Holder) ClusterAdmins() []string {
	return h.current().ClusterAdmins()
}

// RolesOf returns AID's role names in the current snapshot. See
// [Enforcer.RolesOf].
func (h *Holder) RolesOf(aid string) []string {
	return h.current().RolesOf(aid)
}

// CovenScope is AID's coven scope for (resource, action) in the current
// snapshot. See [Enforcer.CovenScope].
func (h *Holder) CovenScope(aid, resource, action string) ([]string, bool) {
	return h.current().CovenScope(aid, resource, action)
}

// ResolvePurview is AID's scope boundary (Purview by dimension) for
// (resource, action) in the current snapshot. See [Enforcer.ResolvePurview].
// Needed for scoped visibility on `GET /v1/souls` (ADR-047 S3b,
// keeper/internal/soulpurview).
func (h *Holder) ResolvePurview(aid, resource, action string) Purview {
	return h.current().ResolvePurview(aid, resource, action)
}

// HoldsAction is the existence gate for read endpoints (ADR-047 §d amendment
// 2026-06-04): does AID hold the action in any scope, in the current
// snapshot. See [Enforcer.HoldsAction]. Needed by the [RequireAction]
// middleware (G1/G2).
func (h *Holder) HoldsAction(aid, resource, action string) bool {
	return h.current().HoldsAction(aid, resource, action)
}

// PermissionsOf returns AID's effective rights in the current snapshot
// (self-describing `GET /v1/me/permissions`). See [Enforcer.PermissionsOf].
func (h *Holder) PermissionsOf(aid string) []EffectivePermission {
	return h.current().PermissionsOf(aid)
}
