// Package auditgate applies the `audit.enabled` / `audit.otel_export` toggles of
// ADR-022(i) to the audit write-path.
//
// The gate is a decorator over [audit.Writer] rather than a check spread across
// the initiators: every audit write in Keeper goes through one interface, and
// the single place where that interface is assembled (`setupAudit` in
// `cmd/keeper`) is therefore the only place the toggle has to exist. Adding a
// new initiator cannot forget to honor it.
//
// It lives in `keeper/internal` and not in `shared/audit` because it reads the
// live `config.Store` snapshot, and `shared/config` already imports
// `shared/audit` — the reverse edge would be an import cycle. That is the same
// boundary ADR-011 draws for `auditpg` / `auditotel` / `auditmulti`.
//
// # The always-write set
//
// A gate that swallowed everything would swallow the record of its own closing:
// `Store.reload` swaps the snapshot BEFORE it emits `config.reload_succeeded`
// (shared/config/store.go), so by the time the reload that disabled audit is
// journaled, the gate is already shut. Turning audit off would then leave no
// trace of who turned it off or when — "work without leaving traces", which is
// precisely what the flag must not buy. Hence [AlwaysWrite]: the reload pair and
// `audit.disabled` are written regardless of the toggle.
//
// # Why the transition marker lives here and not in an OnReload subscriber
//
// `config.Store.notify` runs each subscriber in its own goroutine, so a marker
// written from an `OnReload` callback races the very events it must precede — it
// can land after an arbitrary number of writes have already been dropped. The
// gate instead notices the flip on the write path itself: the first event that
// would be suppressed is preceded, in-line and before the drop, by
// `audit.disabled`. The trail therefore never contains an unexplained gap, which
// is the property that matters — not the wall-clock instant of the marker.
package auditgate

import (
	"context"
	"log/slog"
	"sync"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// AlwaysWrite lists the event types written even with `audit.enabled: false`
// (ADR-022(i)). Each is a record ABOUT the gate rather than an ordinary event:
// the two reload outcomes (which is how the toggle is flipped at all) and the
// `audit.disabled` marker. `audit.enabled` is deliberately absent — the gate is
// already open when re-enabling is journaled.
var AlwaysWrite = map[audit.EventType]struct{}{
	audit.EventConfigReloadSucceeded: {},
	audit.EventConfigReloadFailed:    {},
	audit.EventAuditDisabled:         {},
}

// Config assembles the master gate. Next and Enabled are required.
type Config struct {
	// Next is the writer being gated.
	Next audit.Writer

	// Enabled is called per event, so the toggle stays hot-reloadable
	// (ADR-022(i)): it must read the current `config.Store` snapshot rather
	// than a value captured at startup.
	Enabled func() bool

	// KID identifies the instance in the transition marker's payload. A gate is
	// per process, and the toggle is per `keeper.yml`, so a flip is a fact about
	// this Keeper and not about the cluster.
	KID string

	// Logger reports a failed transition marker. Optional.
	Logger *slog.Logger
}

// gate is an [audit.Writer] that consults enabled before delegating.
type gate struct {
	next    audit.Writer
	enabled func() bool
	bypass  map[audit.EventType]struct{}

	kid    string
	logger *slog.Logger

	// marks distinguishes the master gate (journals its flips) from the
	// secondary one (nothing to explain — see NewSecondary).
	marks bool

	// mu guards last, the toggle state as of the previous write. Only the
	// transition itself is serialized; the marker is written outside the lock.
	mu   sync.Mutex
	last bool
}

// New wraps cfg.Next in the master `audit.enabled` gate. Events in [AlwaysWrite]
// are delegated even while the toggle is off, and a flip of the toggle is
// journaled by the gate itself (see the package doc).
func New(cfg Config) audit.Writer {
	return &gate{
		next:    cfg.Next,
		enabled: cfg.Enabled,
		bypass:  AlwaysWrite,
		kid:     cfg.KID,
		logger:  cfg.Logger,
		marks:   true,
		last:    cfg.Enabled(),
	}
}

// NewSecondary wraps a best-effort secondary writer (OTel dual-write) in the
// `audit.otel_export` gate. No bypass and no transition marker: a suppressed
// span costs nothing — Postgres remains the source of truth (ADR-022(f)), so
// there is no gap in the trail to explain.
func NewSecondary(next audit.Writer, enabled func() bool) audit.Writer {
	return &gate{next: next, enabled: enabled}
}

// Write delegates when the toggle is on or the event type is on the bypass list;
// otherwise it drops the event and reports success. Dropping is the configured
// outcome, not a failure — returning an error would fail the initiator's own
// operation (an HTTP request, a Reaper rule) over a deliberate setting.
func (g *gate) Write(ctx context.Context, event *audit.Event) error {
	on := g.enabled()
	g.noteTransition(ctx, on)

	if event != nil {
		if _, ok := g.bypass[event.EventType]; ok {
			return g.next.Write(ctx, event)
		}
	}
	if !on {
		return nil
	}
	return g.next.Write(ctx, event)
}

// noteTransition journals a flip of the toggle, in-line and before the caller's
// event is dropped. The marker goes straight to next: it is a record about the
// gate and must not be subject to it.
func (g *gate) noteTransition(ctx context.Context, on bool) {
	if !g.marks {
		return
	}

	g.mu.Lock()
	if g.last == on {
		g.mu.Unlock()
		return
	}
	// Flip the state under the lock so a concurrent writer cannot emit a second
	// marker for the same transition; the write itself is outside it.
	g.last = on
	g.mu.Unlock()

	eventType := audit.EventAuditEnabled
	if !on {
		eventType = audit.EventAuditDisabled
	}
	err := g.next.Write(ctx, &audit.Event{
		EventType: eventType,
		Source:    audit.SourceKeeperInternal,
		Payload:   map[string]any{"kid": g.kid},
	})
	if err != nil && g.logger != nil {
		// Loud: a lost `audit.disabled` is an unexplained blind spot in the trail.
		g.logger.Error("audit toggle marker write failed",
			slog.String("event_type", string(eventType)),
			slog.Any("error", err))
	}
}
