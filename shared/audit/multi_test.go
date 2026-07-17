package audit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingWriter is a primary-writer stub: counts writes and optionally returns a
// preset error.
type recordingWriter struct {
	mu     sync.Mutex
	events []*Event
	err    error
}

func (w *recordingWriter) Write(_ context.Context, ev *Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.events = append(w.events, ev)
	return nil
}

func (w *recordingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.events)
}

// recordingTap is a tap stub: counts Observe calls.
type recordingTap struct {
	observed atomic.Int64
}

func (t *recordingTap) Observe(_ context.Context, _ *Event) {
	t.observed.Add(1)
}

func TestMultiWriter_NoTaps_ReturnsPrimary(t *testing.T) {
	primary := &recordingWriter{}
	w := NewMultiWriter(primary, nil)
	if w != Writer(primary) {
		t.Fatalf("without taps, NewMultiWriter should return primary as-is, got %T", w)
	}
	// nil taps are dropped too.
	w = NewMultiWriter(primary, nil, nil, nil)
	if w != Writer(primary) {
		t.Fatalf("nil taps should be dropped, got %T", w)
	}
}

func TestMultiWriter_PrimarySuccess_CallsTaps(t *testing.T) {
	primary := &recordingWriter{}
	tap1, tap2 := &recordingTap{}, &recordingTap{}
	w := NewMultiWriter(primary, nil, tap1, tap2)

	if err := w.Write(context.Background(), &Event{EventType: "scenario_run.completed"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if primary.count() != 1 {
		t.Fatalf("primary should write 1 event, wrote %d", primary.count())
	}
	if tap1.observed.Load() != 1 || tap2.observed.Load() != 1 {
		t.Fatalf("both taps should receive the event: tap1=%d tap2=%d", tap1.observed.Load(), tap2.observed.Load())
	}
}

func TestMultiWriter_PrimaryFail_TapNotCalled(t *testing.T) {
	wantErr := errors.New("pg down")
	primary := &recordingWriter{err: wantErr}
	tap := &recordingTap{}
	w := NewMultiWriter(primary, nil, tap)

	err := w.Write(context.Background(), &Event{EventType: "scenario_run.failed"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("primary error should propagate as-is, got %v", err)
	}
	if tap.observed.Load() != 0 {
		t.Fatalf("on primary failure the tap must NOT be called, called %d times", tap.observed.Load())
	}
}

// panicTap panics in Observe — we verify that Write does not fail.
type panicTap struct{}

func (panicTap) Observe(_ context.Context, _ *Event) { panic("boom") }

func TestMultiWriter_TapPanic_DoesNotFailWrite(t *testing.T) {
	primary := &recordingWriter{}
	after := &recordingTap{}
	w := NewMultiWriter(primary, nil, panicTap{}, after)

	if err := w.Write(context.Background(), &Event{EventType: "command_run.completed"}); err != nil {
		t.Fatalf("a tap panic must not fail Write, got %v", err)
	}
	if primary.count() != 1 {
		t.Fatalf("the primary write must succeed despite the tap panic")
	}
	// the tap after the panicking one must still be called (the panic is isolated).
	if after.observed.Load() != 1 {
		t.Fatalf("the tap after the panicking one must be called, called %d times", after.observed.Load())
	}
	mw, ok := w.(*MultiWriter)
	if !ok {
		t.Fatalf("expected *MultiWriter, got %T", w)
	}
	if mw.tapPanics.Load() != 1 {
		t.Fatalf("panic counter should be 1, got %d", mw.tapPanics.Load())
	}
}

// slowTap blocks in Observe on a release signal — we verify that MultiWriter itself
// introduces no blocking (a slow tap is the tap implementation's responsibility; here
// we confirm Write completes after Observe returns and the tap→Write order is
// correct). The actual non-blocking of a buffered tap is in herald/tap_test.go.
func TestMultiWriter_TapInvokedAfterPrimary(t *testing.T) {
	primary := &recordingWriter{}
	order := make(chan string, 2)
	primaryOrderTap := tapFunc(func(_ context.Context, _ *Event) {
		order <- "tap"
	})
	// Wrap primary to record ordering.
	wrapped := writerFunc(func(ctx context.Context, ev *Event) error {
		order <- "primary"
		return primary.Write(ctx, ev)
	})
	w := NewMultiWriter(wrapped, nil, primaryOrderTap)

	done := make(chan struct{})
	go func() {
		_ = w.Write(context.Background(), &Event{EventType: "voyage.reclaimed"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write hung")
	}
	if got := <-order; got != "primary" {
		t.Fatalf("primary should write FIRST, first was %q", got)
	}
	if got := <-order; got != "tap" {
		t.Fatalf("tap should be called SECOND, second was %q", got)
	}
}

// tapFunc / writerFunc — functional adapters for tests.
type tapFunc func(context.Context, *Event)

func (f tapFunc) Observe(ctx context.Context, ev *Event) { f(ctx, ev) }

type writerFunc func(context.Context, *Event) error

func (f writerFunc) Write(ctx context.Context, ev *Event) error { return f(ctx, ev) }
