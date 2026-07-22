package auditmulti

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// mockWriter — test [audit.Writer] with configurable delay and returned
// error. Tracks call count via atomic.
type mockWriter struct {
	mu      sync.Mutex
	calls   int32
	err     error
	delay   time.Duration
	events  []*audit.Event
	onWrite func() // optional hook, called from Write before it returns
}

func (m *mockWriter) Write(_ context.Context, ev *audit.Event) error {
	atomic.AddInt32(&m.calls, 1)
	m.mu.Lock()
	m.events = append(m.events, ev)
	m.mu.Unlock()
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	if m.onWrite != nil {
		m.onWrite()
	}
	return m.err
}

func (m *mockWriter) Calls() int32 {
	return atomic.LoadInt32(&m.calls)
}

func newEvent() *audit.Event {
	return &audit.Event{
		AuditID:   "01HXYZABCDEFGHJKMNPQRSTVWX",
		EventType: audit.EventConfigReloadSucceeded,
		Source:    audit.SourceSignal,
		Payload:   map[string]any{"path": "/etc/keeper.yml"},
	}
}

// captureLogger returns (logger, buf) — slog with in-memory output for
// asserting on warning messages.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	handler := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(handler), buf
}

func TestMulti_BothSuccess(t *testing.T) {
	prim := &mockWriter{}
	sec := &mockWriter{}
	w := New(prim, []audit.Writer{sec})
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	if err := w.Write(context.Background(), newEvent()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if prim.Calls() != 1 {
		t.Errorf("primary calls = %d, want 1", prim.Calls())
	}
	if sec.Calls() != 1 {
		t.Errorf("secondary calls = %d, want 1 (after drain)", sec.Calls())
	}
}

func TestMulti_PrimarySuccessSecondaryFail(t *testing.T) {
	prim := &mockWriter{}
	sec := &mockWriter{err: errors.New("otel down")}
	logger, buf := captureLogger()
	w := New(prim, []audit.Writer{sec}, WithLogger(logger))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	if err := w.Write(context.Background(), newEvent()); err != nil {
		t.Fatalf("Write returned %v, want nil (secondary-fail must not propagate)", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if prim.Calls() != 1 {
		t.Errorf("primary calls = %d, want 1", prim.Calls())
	}
	if sec.Calls() != 1 {
		t.Errorf("secondary calls = %d, want 1", sec.Calls())
	}
	logs := buf.String()
	if !strings.Contains(logs, "audit secondary write failed") {
		t.Errorf("expected warning in logs, got: %q", logs)
	}
	if !strings.Contains(logs, "otel down") {
		t.Errorf("expected error message in logs, got: %q", logs)
	}
}

func TestMulti_PrimaryFailSecondarySuccess(t *testing.T) {
	prim := &mockWriter{err: errors.New("pg down")}
	sec := &mockWriter{}
	w := New(prim, []audit.Writer{sec})
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	err := w.Write(context.Background(), newEvent())
	if err == nil || !strings.Contains(err.Error(), "pg down") {
		t.Fatalf("Write err = %v, want primary error", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if prim.Calls() != 1 {
		t.Errorf("primary calls = %d, want 1", prim.Calls())
	}
	// PM decision: secondary is NOT started on primary failure.
	if sec.Calls() != 0 {
		t.Errorf("secondary calls = %d, want 0 (primary-fail must skip secondary)", sec.Calls())
	}
}

func TestMulti_PrimaryFailSecondaryFail(t *testing.T) {
	prim := &mockWriter{err: errors.New("pg down")}
	sec := &mockWriter{err: errors.New("otel down")}
	w := New(prim, []audit.Writer{sec})
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	err := w.Write(context.Background(), newEvent())
	if err == nil || !strings.Contains(err.Error(), "pg down") {
		t.Fatalf("Write err = %v, want primary error", err)
	}
	if sec.Calls() != 0 {
		t.Errorf("secondary calls = %d, want 0", sec.Calls())
	}
}

func TestMulti_ShutdownDrain(t *testing.T) {
	prim := &mockWriter{}
	// Secondary with a delay: Close must wait for it to finish.
	sec := &mockWriter{delay: 50 * time.Millisecond}
	w := New(prim, []audit.Writer{sec}, WithShutdownDrain(500*time.Millisecond))

	if err := w.Write(context.Background(), newEvent()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Close right after Write — secondary goroutine is still delaying.
	start := time.Now()
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)
	if sec.Calls() != 1 {
		t.Errorf("secondary calls = %d, want 1 (drain must wait for inflight)", sec.Calls())
	}
	if elapsed < 40*time.Millisecond {
		t.Errorf("Close returned too fast (%v); did not wait for secondary", elapsed)
	}
}

func TestMulti_ShutdownDrainTimeout(t *testing.T) {
	prim := &mockWriter{}
	// Secondary delay longer than drain — timeout.
	sec := &mockWriter{delay: 200 * time.Millisecond}
	logger, buf := captureLogger()
	w := New(prim, []audit.Writer{sec},
		WithShutdownDrain(30*time.Millisecond),
		WithLogger(logger),
	)

	if err := w.Write(context.Background(), newEvent()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err := w.Close(context.Background())
	if err == nil || !strings.Contains(err.Error(), "drain timeout") {
		t.Fatalf("Close err = %v, want drain timeout", err)
	}
	if !strings.Contains(buf.String(), "shutdown drain timed out") {
		t.Errorf("expected timeout warning in logs, got: %q", buf.String())
	}
	// Wait for the secondary to finish so the goroutine doesn't leak.
	time.Sleep(250 * time.Millisecond)
}

func TestMulti_NoSecondariesPassThrough(t *testing.T) {
	prim := &mockWriter{}
	w := New(prim, nil)
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	if err := w.Write(context.Background(), newEvent()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if prim.Calls() != 1 {
		t.Errorf("primary calls = %d, want 1", prim.Calls())
	}
}

func TestMulti_DeepCopyPayloadForSecondary(t *testing.T) {
	// Secondary receives a copy of the event; mutating it in the
	// secondary must not affect the caller's original payload.
	prim := &mockWriter{}
	done := make(chan struct{})
	sec := &mockWriter{}
	sec.onWrite = func() {
		// mutate the last received event
		sec.mu.Lock()
		ev := sec.events[len(sec.events)-1]
		ev.Payload["mutated_by_secondary"] = true
		sec.mu.Unlock()
		close(done)
	}

	w := New(prim, []audit.Writer{sec})
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	origEv := newEvent()
	if err := w.Write(context.Background(), origEv); err != nil {
		t.Fatalf("Write: %v", err)
	}
	<-done

	if _, ok := origEv.Payload["mutated_by_secondary"]; ok {
		t.Errorf("original payload mutated by secondary; deep-copy contract broken")
	}
}

func TestMulti_NilSecondary_PanicsInNew(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New did not panic on nil secondary")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value type = %T, want string; value = %v", r, r)
		}
		if !strings.Contains(msg, "secondaries[1] is nil") {
			t.Errorf("panic message = %q, want substring 'secondaries[1] is nil'", msg)
		}
	}()
	_ = New(&mockWriter{}, []audit.Writer{&mockWriter{}, nil})
}

func TestMulti_NilPrimary_PanicsInNew(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New did not panic on nil primary")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value type = %T, want string; value = %v", r, r)
		}
		if !strings.Contains(msg, "primary writer is nil") {
			t.Errorf("panic message = %q, want substring 'primary writer is nil'", msg)
		}
	}()
	_ = New(nil, nil)
}

// TestMulti_ConcurrentCloseAndWrite_NoRace — the race detector must stay
// clean under a Close vs Write race. Without a mutex around
// `select <-closed / wg.Add`, this test would likely fail under `-race`
// with "WaitGroup is reused before previous Wait has returned" or via a
// child Write after Close has returned.
func TestMulti_ConcurrentCloseAndWrite_NoRace(t *testing.T) {
	prim := &mockWriter{}
	sec := &mockWriter{}
	w := New(prim, []audit.Writer{sec}, WithShutdownDrain(2*time.Second))

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = w.Write(context.Background(), newEvent())
		}()
	}
	// Close in the middle of a burst of Writes — hits the race window
	// between `select <-closed` and `wg.Add(1)` without a mutex.
	closeErr := w.Close(context.Background())
	wg.Wait()
	// drain 2s > mockWriter delay (0) → there should be no timeout.
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	// After Close, an additional Write should go to primary only.
	if err := w.Write(context.Background(), newEvent()); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}
	// sec.Calls() could be anywhere from 0 to n inclusive — all values
	// are fine; we only check the invariant that calls never exceed what
	// primary saw before Close (n + 1 post-Close).
	if got := sec.Calls(); got > int32(n) {
		t.Errorf("secondary calls = %d, want ≤ %d (post-Close write must skip secondary)", got, n)
	}
}

// TestMulti_ContextDetach_SecondaryGetsDetachedContext — the caller
// cancels ctx right after Write; the secondary must receive a
// non-cancelled ctx.
func TestMulti_ContextDetach_SecondaryGetsDetachedContext(t *testing.T) {
	prim := &mockWriter{}
	gotCtx := make(chan context.Context, 1)
	// ctxCapturingWriter captures ctx and returns right away (defined below).
	sec := &ctxCapturingWriter{ch: gotCtx}

	w := New(prim, []audit.Writer{sec})
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Write(ctx, newEvent()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	cancel() // caller ctx cancelled right after Write — typical HTTP handler.

	select {
	case received := <-gotCtx:
		if err := received.Err(); err != nil {
			t.Errorf("secondary ctx.Err() = %v, want nil (caller-cancel must not propagate)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("secondary did not receive event within 2s")
	}
}

type ctxCapturingWriter struct {
	ch chan context.Context
}

func (c *ctxCapturingWriter) Write(ctx context.Context, _ *audit.Event) error {
	// Small delay so the caller has time to cancel before this Write runs.
	time.Sleep(20 * time.Millisecond)
	c.ch <- ctx
	return nil
}

func TestMulti_NilEvent_NoOp(t *testing.T) {
	prim := &mockWriter{}
	sec := &mockWriter{}
	w := New(prim, []audit.Writer{sec})
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	if err := w.Write(context.Background(), nil); err != nil {
		t.Fatalf("Write(nil) = %v, want nil", err)
	}
	if prim.Calls() != 0 {
		t.Errorf("primary calls on nil event = %d, want 0", prim.Calls())
	}
	if sec.Calls() != 0 {
		t.Errorf("secondary calls on nil event = %d, want 0", sec.Calls())
	}
}

func TestMulti_CloseIdempotent(t *testing.T) {
	prim := &mockWriter{}
	sec := &mockWriter{}
	w := New(prim, []audit.Writer{sec})

	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// After Close — Write still works against primary, but secondary is
	// not started.
	if err := w.Write(context.Background(), newEvent()); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}
	if prim.Calls() != 1 {
		t.Errorf("primary calls = %d, want 1 (Write after Close goes to primary)", prim.Calls())
	}
	if sec.Calls() != 0 {
		t.Errorf("secondary calls = %d, want 0 (closed → no fan-out)", sec.Calls())
	}
}
