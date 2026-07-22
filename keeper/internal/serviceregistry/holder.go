package serviceregistry

import (
	"context"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"
)

// DefaultRefreshInterval — TTL period for re-reading the registry snapshot
// from DB (S2, rbac.DefaultRefreshInterval pattern). Registry CRUD mutations
// are rare, so seconds of staleness are acceptable; Redis pub/sub invalidation
// on top (see [Holder.WatchInvalidations]) cuts the delay to milliseconds.
const DefaultRefreshInterval = 10 * time.Second

// InvalidationSource — subscription surface for cluster-wide registry
// invalidation (S2). Implemented in `keeper run` by an adapter over
// [keeperredis.SubscribeServiceInvalidate]; declared as an interface so
// [Holder.WatchInvalidations] can be tested without Redis (fake source).
//
// Watch blocks until ctx.Done(), calling onInvalidate for every received
// invalidate message (self-origin already filtered by the source). A returned
// error is a fatal subscription problem — the caller (Holder) logs it and
// degrades to plain TTL-poll (fail-soft), symmetric to rbac.InvalidationSource.
type InvalidationSource interface {
	Watch(ctx context.Context, onInvalidate func()) error
}

// SnapshotSource — surface for loading the registry snapshot from DB.
// Implemented by [PoolSource] over ListServices + GetSetting; declared as an
// interface so Holder can be tested without Postgres (fake source).
type SnapshotSource interface {
	Load(ctx context.Context) (*Snapshot, error)
}

// Snapshot — immutable consistent slice of the registry at DB-read time: a
// catalog of Services by name + keeper_settings scalars. Built by [PoolSource]
// and atomically swapped whole in [Holder]; never mutated after publication
// (getters return by value).
type Snapshot struct {
	// services — catalog keyed by PK service_registry.name. Values are held
	// by value (ServiceEntry is a value type), so Resolve safely hands out a
	// copy without risking external mutation of the shared snapshot.
	services map[string]ServiceEntry

	// defaultDestinySource — keeper_settings[default_destiny_source] scalar;
	// "" means the setting is unset (no row in keeper_settings).
	defaultDestinySource string

	// provisioningMethods — provisioning_allowed_methods policy (set of
	// allowed created_via methods for operator CREATION). nil-map = setting
	// unset (no key in keeper_settings) → everything allowed (back-compat).
	// non-nil = policy set, exactly these methods are allowed. Load never
	// publishes a malformed-nonempty key (returns an error instead), so
	// non-nil here is always a nonempty set from the {user,ldap,oidc} domain.
	provisioningMethods map[string]bool
}

// PoolSource — [SnapshotSource] over pgx-pool (the real source in `keeper
// run`). Reads the Service catalog and well-known scalars in one pass.
type PoolSource struct {
	DB ExecQueryRower
}

// Load builds a snapshot: ListServices (full catalog) + GetSetting for each
// well-known scalar. A missing setting row (ErrSettingNotFound) is not an
// error — the scalar just stays empty. Any other DB error is propagated
// (Holder keeps the previous snapshot).
func (s PoolSource) Load(ctx context.Context) (*Snapshot, error) {
	entries, err := ListServices(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	services := make(map[string]ServiceEntry, len(entries))
	for _, e := range entries {
		services[e.Name] = *e
	}

	dds, err := loadSettingValue(ctx, s.DB, SettingDefaultDestinySource)
	if err != nil {
		return nil, err
	}

	// provisioning_allowed_methods: ErrSettingNotFound → policy unset (nil-map,
	// everything allowed, back-compat). Found → parse: malformed/empty
	// (ErrEmptyProvisioningMethods / ErrInvalidProvisioningMethod) is
	// propagated so Holder never publishes a broken snapshot (fatal on
	// NewHolder startup — anti-lockout "don't start with a broken policy";
	// unreachable at runtime since PUT validates BEFORE writing, see
	// ProvisioningPolicyHandler).
	provMethods, err := loadProvisioningMethods(ctx, s.DB)
	if err != nil {
		return nil, err
	}

	return &Snapshot{
		services:             services,
		defaultDestinySource: dds,
		provisioningMethods:  provMethods,
	}, nil
}

// loadProvisioningMethods reads and parses the provisioning_allowed_methods
// policy. ErrSettingNotFound → (nil, nil) (policy unset, everything allowed);
// found → [ParseProvisioningMethods] (malformed/empty → error); other DB
// errors are propagated.
func loadProvisioningMethods(ctx context.Context, db ExecQueryRower) (map[string]bool, error) {
	set, err := GetSetting(ctx, db, SettingProvisioningAllowedMethods)
	if err != nil {
		if isSettingNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseProvisioningMethods(set.Value)
}

// loadSettingValue reads a setting value by key; ErrSettingNotFound →
// ("", nil) (setting simply unset), other errors are propagated.
func loadSettingValue(ctx context.Context, db ExecQueryRower, key string) (string, error) {
	set, err := GetSetting(ctx, db, key)
	if err != nil {
		if isSettingNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return set.Value, nil
}

func isSettingNotFound(err error) bool {
	// GetSetting returns ErrSettingNotFound directly (sentinel), not wrapped.
	return err == ErrSettingNotFound //nolint:errorlint // sentinel from this package, never wrapped
}

// Holder — owner of the current registry [*Snapshot] built from DB (S2).
//
// Refresh strategy (rbac.Holder pattern):
//   - On startup [NewHolder] synchronously builds the first snapshot from DB
//     (fatal on error — the daemon must not come up with an empty/broken
//     registry).
//   - A background goroutine ([Run]) re-reads the snapshot every
//     refreshInterval. A re-read error does not reset the snapshot: Holder
//     keeps the previous one and logs a warning (a DB hiccup must not zero
//     out the Service catalog).
//   - [WatchInvalidations] on top — near-instant rebuild on a Redis signal.
//
// Getters ([Resolve]/[DefaultDestinySource]) are SYNCHRONOUS (no ctx/error):
// they read the current snapshot via atomic.Pointer.Load without locking.
// This matters for the future consumer switch-over (S4): their synchronous
// interface (ServiceRegistry/DestinySource) doesn't change when the cfg
// source is swapped for Holder.
//
// Concurrency: the snapshot lives in an atomic.Pointer; refresh/rebuild swaps
// the whole pointer, readers always see a consistent slice. The snapshot is
// never mutated after publication.
type Holder struct {
	src      SnapshotSource
	interval time.Duration
	logger   *slog.Logger

	cur atomic.Pointer[Snapshot]

	// metrics — keeper_serviceregistry_* descriptor, injected via
	// [SetMetrics] during the daemon's setup phase (after the registry is
	// created, which comes up later than NewHolder). nil before wire-up and
	// in tests/bootstrap — all Observe* calls are then no-ops (method on nil
	// *RegistryMetrics is a no-op).
	//
	// atomic.Pointer rather than a plain field: SetMetrics is called AFTER
	// the Run goroutine has already started (go Holder.Run in
	// setupServiceRegistry, while SetMetrics comes later in
	// setupMetricsRegistry). In that window Run concurrently reads metrics
	// from refresh — a plain field would be a data race (rbac.Holder.metrics
	// pattern).
	metrics atomic.Pointer[RegistryMetrics]
}

// SetMetrics attaches the keeper_serviceregistry_* descriptor. Safe
// concurrently with background readers ([Run]/[WatchInvalidations]) via
// atomic.Pointer. nil receiver — no-op. Called from the daemon's
// `setupMetricsRegistry` after [RegisterRegistryMetrics]; before that,
// metrics is nil (Load returns nil) and nothing is published
// (rbac.Holder.SetMetrics pattern).
func (h *Holder) SetMetrics(m *RegistryMetrics) {
	if h == nil {
		return
	}
	h.metrics.Store(m)
}

// NewHolder builds the initial snapshot from DB. An error on the first load
// is fatal (err returned to the caller, the daemon fails at startup if the
// registry is unavailable/broken).
//
// nil-src is allowed — Holder behaves as an empty snapshot (no Services,
// empty scalars), and background refresh is then a no-op. Used by tests and
// code paths where the pool isn't initialized yet; in `keeper run` src is
// always non-nil.
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
		h.cur.Store(emptySnapshot())
		return h, nil
	}
	snap, err := src.Load(ctx)
	if err != nil {
		return nil, err
	}
	h.cur.Store(snap)
	return h, nil
}

// emptySnapshot — a snapshot with no Services and empty scalars (nil-src /
// before the first load). Map is non-nil so Resolve doesn't panic.
func emptySnapshot() *Snapshot {
	return &Snapshot{services: map[string]ServiceEntry{}}
}

// Run starts the background TTL re-read of the snapshot until ctx is
// cancelled. Blocking — the caller runs it in a separate goroutine. Returns
// immediately on nil-src (nothing to re-read).
//
// A re-read error is logged (warn) and does NOT change the active snapshot —
// a stale window is the lesser evil compared to "DB blipped → Service
// catalog goes empty".
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

// WatchInvalidations subscribes to cluster-wide registry invalidation (S2)
// and runs [Holder.refresh] on every signal (near-instant re-read from DB,
// instead of waiting for the [Run] TTL-poll). Blocking — the caller runs it
// in a separate goroutine, exits on ctx.Done().
//
// The TTL-poll ([Run]) is NOT replaced: pub/sub has no persistence (message
// loss, reconnect) → the next [Run] tick still re-reads the snapshot. This is
// a fail-soft layer on top of the fallback.
//
// nil-src ([Holder] built without a DB source) or a nil invalidator → no-op.
// A subscription error is logged (warn) and does NOT crash the daemon — the
// registry keeps updating via TTL-poll.
func (h *Holder) WatchInvalidations(ctx context.Context, src InvalidationSource) {
	if h.src == nil || src == nil {
		return
	}
	err := src.Watch(ctx, func() {
		h.metrics.Load().ObserveInvalidation()
		h.refresh(ctx)
	})
	if err != nil && ctx.Err() == nil {
		h.logger.Warn("serviceregistry: cluster-wide invalidation subscription ended with error, falling back to TTL-poll",
			slog.Any("error", err),
		)
	}
}

// Refresh forcibly re-reads the snapshot (lazy path / tests). Returns the
// load error; on error the active snapshot is unchanged.
//
// Metrics keeper_serviceregistry_snapshot_*: the whole rebuild (src.Load) is
// timed, failure phase is "load"; on success a timestamp + Service count from
// the built snapshot are recorded (rbac.Holder.Refresh pattern).
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
	m.ObserveRebuildSuccess(time.Since(start), len(snap.services))
	h.cur.Store(snap)
	return nil
}

// refresh — internal best-effort re-read for background goroutines: logs
// the error, doesn't propagate it.
func (h *Holder) refresh(ctx context.Context) {
	if err := h.Refresh(ctx); err != nil {
		h.logger.Warn("serviceregistry: snapshot refresh from DB failed, keeping previous snapshot",
			slog.Any("error", err),
		)
	}
}

// current returns the up-to-date snapshot (atomic, without locking).
func (h *Holder) current() *Snapshot {
	return h.cur.Load()
}

// Resolve returns a Service entry by name from the current snapshot. A false
// second result means the Service doesn't exist (not an error: this is a hot
// path, absence is a normal outcome, not a failure). The getter is
// SYNCHRONOUS — that's the S4 consumer contract.
func (h *Holder) Resolve(name string) (ServiceEntry, bool) {
	e, ok := h.current().services[name]
	return e, ok
}

// DefaultDestinySource returns the keeper_settings[default_destiny_source]
// scalar from the current snapshot; "" means the setting is unset. The
// getter is SYNCHRONOUS.
func (h *Holder) DefaultDestinySource() string {
	return h.current().defaultDestinySource
}

// ProvisioningMethodAllowed reports whether the current
// provisioning_allowed_methods policy allows CREATING an operator via
// method. The getter is SYNCHRONOUS (atomic snapshot, no locking). Semantics:
//   - bootstrap/system → ALWAYS true (never gated by policy: bootstrap of
//     the first Archon via `keeper init`, system is internal records);
//   - policy unset (nil-map, no key in keeper_settings) → true (default
//     "everything allowed", back-compat);
//   - policy set → method ∈ set.
//
// nil receiver (gate not configured / tests) → true (back-compat, gate==nil
// is treated by the caller as "let it through").
func (h *Holder) ProvisioningMethodAllowed(method string) bool {
	if method == "bootstrap" || method == "system" {
		return true
	}
	if h == nil {
		return true
	}
	methods := h.current().provisioningMethods
	if methods == nil {
		return true
	}
	return methods[method]
}

// ProvisioningPolicy returns the current policy for the GET endpoint: a
// sorted list of allowed methods and a set flag (whether the policy is
// configured). set=false → policy unset (default "everything allowed"),
// methods=nil. The getter is SYNCHRONOUS.
func (h *Holder) ProvisioningPolicy() (methods []string, set bool) {
	if h == nil {
		return nil, false
	}
	m := h.current().provisioningMethods
	if m == nil {
		return nil, false
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, true
}
