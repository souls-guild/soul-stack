package audit

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// Tap is a passive observer of audit events on top of the primary [Writer]
// (extension point ADR-022(f)). It receives a COPY of an event already written
// to primary, AFTER a successful primary write. A tap implementation must be
// non-blocking and best-effort: its errors do not affect the outcome of
// [Writer.Write] (see [MultiWriter]). The concrete notification tap is
// keeper-side (keeper/internal/herald, ADR-052(c)).
//
// The "event is already in the primary store" semantics guarantee a tap never
// sees or signals outward an unwritten event (ADR-052(c) order: the audit fact
// in PG first, then the notification).
type Tap interface {
	// Observe receives an event guaranteed to be written to primary. event is
	// read-only for the tap (shared pointer; the tap does not mutate). The
	// implementation must not block the caller: push heavy/network work into its
	// own goroutine/queue. The tap swallows its own errors (log/metric) and does
	// not surface them.
	Observe(ctx context.Context, event *Event)
}

// MultiWriter wraps the primary [Writer] and a set of [Tap]s. Contract:
//
//   - the primary write (PG) runs FIRST and its result is the only one affecting
//     the outcome of [Write]. A primary error is returned as is, and the taps
//     are NOT called (must not signal outward an unwritten event).
//   - taps are called strictly AFTER a successful primary write, in the order
//     passed. Any tap error/panic does not fail [Write] (best-effort,
//     ADR-022(f)/ADR-052(c)).
//
// Non-blocking behavior and buffering are the concrete [Tap]'s responsibility
// (see herald.notificationTap: a bounded channel + drop counter). MultiWriter
// only guarantees ordering and error isolation.
type MultiWriter struct {
	primary Writer
	taps    []Tap
	logger  *slog.Logger

	// tapPanics counts panics recovered while calling Tap.Observe. A tap panic
	// is a programmer error but must not bring down the audit write-path; the
	// atomic counter is available to tests/diagnostics.
	tapPanics atomic.Uint64
}

// NewMultiWriter wraps primary in a decorator with taps. nil taps are dropped.
// With an empty tap set it returns primary as is — a decorator with no observers
// is pointless (zero overhead on the write-path). logger is for logging tap
// panics; nil is allowed (panics are counted silently).
func NewMultiWriter(primary Writer, logger *slog.Logger, taps ...Tap) Writer {
	live := make([]Tap, 0, len(taps))
	for _, t := range taps {
		if t != nil {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		return primary
	}
	return &MultiWriter{primary: primary, taps: live, logger: logger}
}

// Write writes the event to primary; on success it hands it to the taps.
func (m *MultiWriter) Write(ctx context.Context, event *Event) error {
	if err := m.primary.Write(ctx, event); err != nil {
		return err
	}
	for _, t := range m.taps {
		m.observe(ctx, t, event)
	}
	return nil
}

// observe calls a tap with a recover barrier: a tap panic is counted and logged
// but does not bring down the write-path.
func (m *MultiWriter) observe(ctx context.Context, t Tap, event *Event) {
	defer func() {
		if r := recover(); r != nil {
			m.tapPanics.Add(1)
			if m.logger != nil {
				m.logger.Error("audit: tap panic recovered",
					slog.String("event_type", string(event.EventType)),
					slog.Any("panic", r))
			}
		}
	}()
	t.Observe(ctx, event)
}
