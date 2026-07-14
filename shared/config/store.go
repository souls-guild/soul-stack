package config

// Store[T] is a snapshot of a typed config (`KeeperConfig` or `SoulConfig`)
// with atomic-swap semantics per [ADR-021](docs/architecture.md) (b) and (c):
//
//   - (b) hot-reload on SIGHUP: see [shared/config/sighup.go];
//   - (c) validation pipeline + atomic swap: on any error diagnostic `Reload`
//     does NOT swap, and the previous snapshot stays current.
//
// Read contract:
//
//   - `Get()` returns `*T`. The pointer is immutable; the caller MUST NOT
//     mutate the returned struct's fields. To mutate and then persist to disk,
//     use `Document()` + the package's `Patch*`/`Save*` free functions.
//   - Concurrent `Get()` calls are safe: the value lives in
//     `atomic.Pointer[T]`, read-side wait-free.
//
// Reload contract:
//
//   - `Reload(ctx, source)` reads the file (`ReadFile`), parses via the
//     matching `Load*FromBytes`, and with no error diagnostics atomically
//     swaps the snapshot and `Document` pointers. Source is a `ReloadSource`
//     (see `ReloadSourceSignal`/`API`/`MCP`); recorded in the audit payload.
//   - On I/O-fatal: `ReloadResult.Swapped=false`, `Phase = diag.PhaseParse`,
//     one `io_error` entry in `Diagnostics`.
//   - On validation-error: `Swapped=false`, `Phase` = the first error phase
//     from the diagnostics, `Diagnostics` holds all collected entries.
//   - On success: `Swapped=true`, `Phase=""`, `Diagnostics` = warnings (if any).
//
// CorrelationID:
//
//   - 26-char ULID (Crockford base32, sortable timestamp prefix). Format per
//     [ADR-022(c)](docs/architecture.md); one implementation —
//     `shared/audit.NewULID()` — to avoid drift between `config.reload_*`
//     events and the rest of the audit pipeline.
//   - Each `Reload` generates its own ID, passed to the audit pipeline as the
//     correlation key between the reload request and its result.
//
// ChangedPaths:
//
//   - Empty slice in M0.3; source↔source diff computation deferred to M0.3.5.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// tracer for the in-process config hot-reload span (ADR-024). Uses the global
// TracerProvider set up by [obs.SetupOTel] in cmd/keeper and cmd/soul; when OTel
// is disabled the provider is no-op — the span is free and the code does not
// branch (symmetric with keeper/render, keeper/scenario).
var tracer = otel.Tracer("shared/config")

// storeKind is the internal discriminator for picking the Load function on
// Reload. External packages don't need it: the concrete store type is set via
// the `LoadKeeperStore` / `LoadSoulStore` constructors.
type storeKind int

const (
	storeKindKeeper storeKind = iota + 1
	storeKindSoul
)

// ReloadSource is the closed enum of reload initiators. Type alias for
// [audit.Source] (ADR-022(b)) — the single source of truth for the audit
// pipeline's source enum; hot-reload callers get the same constants as any
// other write-path initiator.
//
// Casting an arbitrary string (`ReloadSource("hax0r")`) is technically possible
// — the invariant is validated by the write-path initiator before the audit
// INSERT (see [audit.Source.Valid]).
type ReloadSource = audit.Source

const (
	ReloadSourceSignal = audit.SourceSignal
	ReloadSourceAPI    = audit.SourceAPI
	ReloadSourceMCP    = audit.SourceMCP
)

// ReloadCallback is an opt-in subscriber to successful [Store.Reload] swaps.
// Called **only** when `ReloadResult.Swapped=true`; on validation-fail /
// I/O-fatal subscribers get no notification (the old snapshot stays current,
// nothing to react to).
//
// Argument contract:
//
//   - `old` — pointer to the snapshot BEFORE the swap. May be `nil` when the
//     initial load failed on a validation-error and the first successful Reload
//     swapped `nil → *T`.
//   - `new` — pointer to the snapshot AFTER the swap. Always non-nil (the
//     callback runs only when Swapped=true).
//
// Both pointers are snapshot values with [Store] atomic-swap semantics: they
// **must not be mutated** (see the [Store.Get] doc comment).
//
// Each subscriber runs in a **separate goroutine** per callback (so one
// subscriber can't block others). **Call order is not guaranteed** —
// subscribers must not depend on it. A panic inside a callback is caught by
// `recover` + slog.Error and does not take down other callbacks.
type ReloadCallback[T any] func(old, new *T)

// Store is a typed config snapshot with atomic-swap semantics. Parameterized by
// the concrete config type (`KeeperConfig`/`SoulConfig`); the internal `kind`
// decides which `Load*FromBytes` to call on `Reload`.
type Store[T any] struct {
	snapshot atomic.Pointer[T]

	mu   sync.Mutex // guards doc/path/opts during Reload
	doc  *Document
	path string
	kind storeKind
	opts ValidateOptions

	// auditWriter is an optional [audit.Writer]. If non-nil, each Reload emits a
	// `config.reload_succeeded`/`config.reload_failed` event via
	// [audit.Writer.Write] (best-effort; a write error is logged via slog.Warn
	// but does not block the [ReloadResult] return). nil = backward compat:
	// Reload works without audit emission (the `LoadKeeperStore`/`LoadSoulStore`
	// constructors without the WithAudit suffix).
	auditWriter audit.Writer

	// subsMu guards the subscribers slice. A separate mutex from `mu` so the
	// notify phase (after the swap) can read the list without blocking the
	// reload pipeline, and vice versa: an OnReload call from within a callback
	// (nested subscribe/unsubscribe) does not deadlock on the reload mutex.
	//
	// RWMutex: notify reads the slice under RLock (allows concurrent OnReload),
	// subscribe/unsubscribe under Lock.
	subsMu sync.RWMutex

	// subscribers is the slice of optional reload callbacks, each registered via
	// [Store.OnReload]. Order is irrelevant (callbacks run in separate
	// goroutines).
	//
	// Stored type `*subscription[T]` is a pointer wrapper so unsubscribe can
	// identify an entry by address without comparing functions (not comparable
	// in Go).
	subscribers []*subscription[T]
}

// subscription is an internal subscriber record. Holds the callback and its
// pointer identity, which unsubscribe uses for O(n) lookup and removal. `cb` is
// immutable for the subscription's lifetime; the pointer itself is a stable
// identity, making unsubscribe idempotent.
type subscription[T any] struct {
	cb ReloadCallback[T]
}

// ReloadResult is the payload of a single reload. Meant to be formatted by the
// caller into a `config.reload_succeeded` or `config.reload_failed` audit event
// (event names and dual-write are a separate slice, M0.4).
type ReloadResult struct {
	// Swapped reports whether the atomic swap happened. false means the snapshot
	// is unchanged (I/O fatal or validation-error).
	Swapped bool

	// Source — who initiated the reload. See `ReloadSourceSignal` /
	// `ReloadSourceAPI` / `ReloadSourceMCP`.
	Source ReloadSource

	// Phase — phase of the first error diagnostic if the reload failed; empty
	// for a successful reload or when there were no error-level diagnostics.
	Phase diag.Phase

	// Diagnostics — all diagnostics from this reload (error + warning + hint).
	Diagnostics []diag.Diagnostic

	// ChangedPaths — YAML paths of changed fields in goccy format
	// ("$.auth.jwt.signing_key_ref"). Empty in M0.3 (diff computation deferred
	// to M0.3.5). Declared now so consumers (audit pipeline M0.4, Operator API)
	// can be developed in parallel.
	ChangedPaths []string

	// CorrelationID — 26-char ULID (Crockford base32), unique per reload. Format
	// per [ADR-022(c)] and matches `audit_log.correlation_id` for
	// `config.reload_*` events.
	CorrelationID string

	// Timestamp — when the reload finished (the swap/no-swap decision point).
	Timestamp time.Time
}

// LoadKeeperStore reads `keeper.yml` and wraps the result in a Store.
//
// Returns a Store even on validation-errors: the first snapshot may be `nil`
// (`Get()` then returns a zero-value pointer), but the caller sees the
// diagnostics and decides whether to abort startup. On I/O-fatal no Store is
// created — returns (nil, diags, err).
func LoadKeeperStore(path string, opts ValidateOptions) (*Store[KeeperConfig], []diag.Diagnostic, error) {
	cfg, doc, diags, err := LoadKeeper(path, opts)
	if err != nil {
		return nil, diags, err
	}
	s := &Store[KeeperConfig]{
		doc:  doc,
		path: path,
		kind: storeKindKeeper,
		opts: opts,
	}
	if cfg != nil && !diag.HasErrors(diags) {
		s.snapshot.Store(cfg)
	}
	return s, diags, nil
}

// LoadSoulStore is the same for `soul.yml`. See `LoadKeeperStore`.
func LoadSoulStore(path string, opts ValidateOptions) (*Store[SoulConfig], []diag.Diagnostic, error) {
	cfg, doc, diags, err := LoadSoul(path, opts)
	if err != nil {
		return nil, diags, err
	}
	s := &Store[SoulConfig]{
		doc:  doc,
		path: path,
		kind: storeKindSoul,
		opts: opts,
	}
	if cfg != nil && !diag.HasErrors(diags) {
		s.snapshot.Store(cfg)
	}
	return s, diags, nil
}

// LoadKeeperStoreWithAudit is [LoadKeeperStore] plus an injected [audit.Writer].
// Each [Store.Reload] then emits a `config.reload_succeeded` or
// `config.reload_failed` event (see the file contract +
// [ADR-022(j)](docs/architecture.md) for the payload structure).
//
// w may be nil — behavior is then identical to [LoadKeeperStore]. Handy for
// constructors whose Writer isn't initialized yet (e.g. before the Postgres
// pool comes up in the bootstrap phase): the caller passes nil now and
// reinitializes the Store later.
//
// The audit write is best-effort: an [audit.Writer.Write] error is logged via
// `slog.Warn` but does not block the [ReloadResult] return or change `Swapped`.
// The atomic snapshot swap does not depend on audit emission succeeding (audit
// is observability, not correctness).
func LoadKeeperStoreWithAudit(path string, opts ValidateOptions, w audit.Writer) (*Store[KeeperConfig], []diag.Diagnostic, error) {
	s, diags, err := LoadKeeperStore(path, opts)
	if s != nil {
		s.auditWriter = w
	}
	return s, diags, err
}

// LoadSoulStoreWithAudit is the same for `soul.yml`. See [LoadKeeperStoreWithAudit].
func LoadSoulStoreWithAudit(path string, opts ValidateOptions, w audit.Writer) (*Store[SoulConfig], []diag.Diagnostic, error) {
	s, diags, err := LoadSoulStore(path, opts)
	if s != nil {
		s.auditWriter = w
	}
	return s, diags, err
}

// SetAuditWriter injects an [audit.Writer] into an already-created Store — the
// late-binding variant of [LoadKeeperStoreWithAudit] / [LoadSoulStoreWithAudit].
//
// Needed by the `keeper`/`soul` binary init order, where the Store is created
// BEFORE the audit writer comes up (Vault → pool → migrations → writer, a
// deliberate sequence). The caller builds the Store via `LoadKeeperStore` (no
// audit) and, once the writer is up, calls `SetAuditWriter(w)` — from then on
// each [Store.Reload] emits `config.reload_succeeded`/`config.reload_failed`
// (see [Store.emitAudit], ADR-022(j)).
//
// w may be nil (audit emission then off — back-compat). Safe to call before
// [WatchSIGHUP] starts; concurrent use with Reload is not expected (called once
// in the init phase) — the field is guarded by `mu` on write, and the read in
// `emitAudit` goes under the same `mu` via a snapshot.
func (s *Store[T]) SetAuditWriter(w audit.Writer) {
	s.mu.Lock()
	s.auditWriter = w
	s.mu.Unlock()
}

// Get returns the current snapshot. The pointer is immutable — the caller must
// not mutate fields. To modify and then persist, use `Document()` + `Patch*` +
// `Save*`.
//
// On a failed initial load (validation-errors on the first file read)
// `Get() == nil` but `Document() != nil` — use Document for Patch/Save and retry
// Reload after fixing the file. After the first successful Reload `Get()` starts
// returning a valid pointer.
//
// Consistency of the `Get()` ↔ `Document()` pair across calls is not guaranteed:
// a Reload swap may happen between them. A caller needing a consistent
// snapshot+doc must rely on only one of the two (e.g. `Document()` and parse the
// needed fields itself).
func (s *Store[T]) Get() *T {
	return s.snapshot.Load()
}

// Document returns the current AST handle (for Patch/Save). Replaced under the
// lock together with the snapshot on a successful Reload. For consistency with
// `Get()`, see the `Get()` doc comment.
func (s *Store[T]) Document() *Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doc
}

// Path returns the file path the Store is bound to. Immutable for the Store's
// lifetime — Reload reads the same path.
func (s *Store[T]) Path() string {
	return s.path
}

// Reload re-reads the file and atomically swaps the snapshot when there are no
// error diagnostics. See the file doc comment for the result-field contract.
//
// `ctx` is reserved for future cancellation of long semantic checks (e.g. vault
// reachability in `AllowNetworkCalls=true` mode); in M0.3 validation is
// synchronous and ctx is not interrupted inside.
//
// `Timestamp` is the swap/no-swap decision point, set right before return on
// each branch.
//
// On `Swapped=true` all subscribers registered via [Store.OnReload] are notified
// (separate goroutines, recover-panic). On `Swapped=false` there is no notify —
// the old snapshot stays in effect. See [ReloadCallback] for the argument
// contract.
func (s *Store[T]) Reload(ctx context.Context, source ReloadSource) ReloadResult {
	// In-process span over the whole hot-reload (parse → validation → semantic →
	// swap) — ADR-024. source matches the reload audit event
	// (config.reload_succeeded/failed); the span is a separate observability
	// channel, not a duplicate of audit. No secrets (config contents, vault
	// values) go into attributes: only source + outcome + file path. When OTel
	// is disabled the tracer is no-op — Start/End are free.
	ctx, span := tracer.Start(ctx, "config.reload",
		trace.WithAttributes(
			attribute.String("source", string(source)),
			attribute.String("path", s.path),
		),
	)
	defer span.End()

	prev := s.snapshot.Load()
	res := s.reload(source)
	s.emitAudit(ctx, res)
	if res.Swapped {
		span.SetAttributes(attribute.String("outcome", "ok"))
		s.notify(prev, s.snapshot.Load())
	} else {
		span.SetAttributes(
			attribute.String("outcome", "failed"),
			attribute.String("phase", string(res.Phase)),
		)
		// First error diagnostic as a span error: the failure reason is visible
		// in the trace without exposing config contents (the diagnostic
		// code+message are the same as in reload-audit validation_errors).
		if d := firstErrorDiag(res.Diagnostics); d != nil {
			span.RecordError(fmt.Errorf("%s: %s", d.Code, d.Message))
		}
		span.SetStatus(codes.Error, "config_reload_failed")
	}
	return res
}

// OnReload registers a subscriber callback for successful Reload swaps. Returns
// an `unsubscribe` function to cancel the subscription.
//
// Contract:
//
//   - `fn` is called **only** when `ReloadResult.Swapped=true`. On
//     validation-fail / I/O-fatal the subscriber is not notified.
//   - Each call runs in a **separate goroutine** — a slow subscriber blocks
//     neither Reload nor other subscribers.
//   - Call order between subscribers is **undefined**.
//   - A panic inside `fn` is caught by `recover` + slog.Error with an empty
//     `correlation_id` field (notify is not tied to the Reload correlation, but
//     the callback can read fields from the new snapshot itself).
//   - `unsubscribe` is idempotent: a repeat call is a no-op. Calling it from the
//     subscriber's own callback is safe (RWMutex, no deadlock).
//   - `fn == nil` panics (a caller programming error, like
//     `signal.Notify(nil, ...)`).
//
// Snapshot visibility in the callback: at call time `s.snapshot.Load()` is
// guaranteed to return `new` (the swap already happened). A concurrent later
// Reload may swap the snapshot again — the callback still sees **the** snapshot
// that triggered its notify (via the `new` argument).
func (s *Store[T]) OnReload(fn ReloadCallback[T]) func() {
	if fn == nil {
		panic("config.Store.OnReload: callback is nil")
	}
	sub := &subscription[T]{cb: fn}
	s.subsMu.Lock()
	s.subscribers = append(s.subscribers, sub)
	s.subsMu.Unlock()

	return func() {
		s.subsMu.Lock()
		defer s.subsMu.Unlock()
		for i, x := range s.subscribers {
			if x == sub {
				// Shift the tail and nil out the last element so we don't keep
				// unreachable callback references in the backing array (matters
				// for long-lived Stores that cycle through subscribe/unsubscribe
				// series).
				last := len(s.subscribers) - 1
				s.subscribers[i] = s.subscribers[last]
				s.subscribers[last] = nil
				s.subscribers = s.subscribers[:last]
				return
			}
		}
	}
}

// notify delivers the successful-swap notification to all registered
// subscribers. The slice is snapshotted under RLock so that:
//
//   - concurrent OnReload calls (Lock) wait for the snapshot to finish;
//   - unsubscribe from the subscriber's own callback does not deadlock (the
//     callback runs in a separate goroutine, so the Lock in its unsubscribe call
//     does not overlap the already-released RLock).
//
// Each callback runs in its own goroutine with recover-panic.
func (s *Store[T]) notify(old, new *T) {
	s.subsMu.RLock()
	subs := make([]*subscription[T], len(s.subscribers))
	copy(subs, s.subscribers)
	s.subsMu.RUnlock()

	for _, sub := range subs {
		go func(cb ReloadCallback[T]) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("config: ReloadCallback panicked",
						"panic", r,
					)
				}
			}()
			cb(old, new)
		}(sub.cb)
	}
}

// reload is the internal body of Reload without audit emission. Split out so
// emitAudit wraps a single ReloadResult instead of being duplicated on every
// return branch. This body doesn't need ctx (validation is synchronous in M0.3);
// the audit call gets ctx from the Reload wrapper.
func (s *Store[T]) reload(source ReloadSource) ReloadResult {
	res := ReloadResult{
		Source:        source,
		CorrelationID: newCorrelationID(),
	}

	src, err := os.ReadFile(s.path)
	if err != nil {
		res.Phase = diag.PhaseParse
		res.Diagnostics = []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    s.path,
			Code:    "io_error",
			Message: err.Error(),
		}}
		res.Timestamp = time.Now()
		return res
	}

	var (
		newCfgKeeper *KeeperConfig
		newCfgSoul   *SoulConfig
		newDoc       *Document
		diags        []diag.Diagnostic
	)

	switch s.kind {
	case storeKindKeeper:
		newCfgKeeper, newDoc, diags, _ = LoadKeeperFromBytes(s.path, src, s.opts)
	case storeKindSoul:
		newCfgSoul, newDoc, diags, _ = LoadSoulFromBytes(s.path, src, s.opts)
	default:
		// Programming error — constructors must set kind.
		res.Phase = diag.PhaseParse
		res.Diagnostics = []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    s.path,
			Code:    "io_error",
			Message: fmt.Sprintf("config: Store has unknown kind %d", s.kind),
		}}
		res.Timestamp = time.Now()
		return res
	}

	res.Diagnostics = diags

	if diag.HasErrors(diags) {
		res.Phase = firstErrorPhase(diags)
		res.Timestamp = time.Now()
		return res
	}

	s.mu.Lock()
	switch s.kind {
	case storeKindKeeper:
		// Cast via any: the generic `Store[T]` doesn't know that T==KeeperConfig
		// for this kind. The constructor guarantees it.
		s.snapshot.Store(any(newCfgKeeper).(*T))
	case storeKindSoul:
		s.snapshot.Store(any(newCfgSoul).(*T))
	}
	s.doc = newDoc
	s.mu.Unlock()

	res.Swapped = true
	res.Timestamp = time.Now()
	return res
}

// emitAudit publishes one `config.reload_*` event to the [audit.Writer]. Safe
// when `s.auditWriter == nil` (no-op). Contract per ADR-022(j):
//
//   - EventType  = config.reload_succeeded | config.reload_failed.
//   - Source     = [ReloadSource] (alias for [audit.Source]).
//   - Payload    = `{ "path": ..., ?"phase": ..., ?"validation_errors": [...],
//     ?"changed_paths": [...] }` — optional keys are present only on failure
//     (phase/validation_errors) or when data exists (changed_paths). In M0.3
//     changed_paths is always empty (source↔source diff deferred to M0.3.5).
//   - CorrelationID — same as [ReloadResult.CorrelationID] (one request = one
//     event chain).
//   - CreatedAt — same as [ReloadResult.Timestamp].
//
// The audit write is best-effort: an [audit.Writer.Write] error is logged via
// `slog.Warn` but not propagated — observability must not break hot-reload.
func (s *Store[T]) emitAudit(ctx context.Context, res ReloadResult) {
	// Snapshot the writer under `mu`: [SetAuditWriter] may set it late-binding in
	// the init phase. Reading a copy (rather than touching the field again) makes
	// both the nil-check and the subsequent Write race-free.
	s.mu.Lock()
	w := s.auditWriter
	s.mu.Unlock()
	if w == nil {
		return
	}

	et := audit.EventConfigReloadSucceeded
	if !res.Swapped {
		et = audit.EventConfigReloadFailed
	}

	payload := map[string]any{
		"path": s.path,
	}
	if !res.Swapped {
		payload["phase"] = string(res.Phase)
		if ve := audit.FormatDiagnostics(res.Diagnostics); ve != nil {
			payload["validation_errors"] = ve
		}
	}
	if len(res.ChangedPaths) > 0 {
		payload["changed_paths"] = res.ChangedPaths
	}

	ev := &audit.Event{
		AuditID:       audit.NewULID(),
		EventType:     et,
		Source:        res.Source,
		CorrelationID: res.CorrelationID,
		Payload:       payload,
		CreatedAt:     res.Timestamp,
	}

	if err := w.Write(ctx, ev); err != nil {
		slog.Warn("audit write failed for config.reload event",
			"path", s.path,
			"source", string(res.Source),
			"event_type", string(et),
			"correlation_id", res.CorrelationID,
			"error", err,
		)
	}
}

// firstErrorPhase returns the phase of the first error diagnostic, the key for
// the `config.reload_failed.phase` audit event.
func firstErrorPhase(ds []diag.Diagnostic) diag.Phase {
	for i := range ds {
		if ds[i].Level == diag.LevelError {
			return ds[i].Phase
		}
	}
	return ""
}

// newCorrelationID returns a 26-char ULID (see [audit.NewULID]). Delegated to
// `shared/audit` — the project's single source of ULID generation; the format
// matches `audit_id` and `correlation_id` in `audit_log`
// ([ADR-022(c)](docs/architecture.md)).
func newCorrelationID() string {
	return audit.NewULID()
}
