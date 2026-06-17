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

// mockWriter — тестовый [audit.Writer] с конфигурируемой задержкой и
// возвращаемой ошибкой. Захватывает количество вызовов через atomic.
type mockWriter struct {
	mu      sync.Mutex
	calls   int32
	err     error
	delay   time.Duration
	events  []*audit.Event
	onWrite func() // optional hook, вызывается из Write до возврата
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

// captureLogger возвращает (logger, buf) — slog с in-memory выводом для
// проверки warning-сообщений.
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
	// PM-decision: secondary НЕ запускается при primary-fail.
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
	// Secondary с delay-ем: Close должен дождаться завершения.
	sec := &mockWriter{delay: 50 * time.Millisecond}
	w := New(prim, []audit.Writer{sec}, WithShutdownDrain(500*time.Millisecond))

	if err := w.Write(context.Background(), newEvent()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Close сразу после Write — secondary goroutine ещё в delay-е.
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
	// Secondary с delay-ем больше, чем drain — таймаут.
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
	// Дождёмся завершения secondary, чтобы goroutine не утекла.
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
	// Secondary получает копию event-а; мутация в secondary не должна
	// влиять на исходный payload caller-а.
	prim := &mockWriter{}
	done := make(chan struct{})
	sec := &mockWriter{}
	sec.onWrite = func() {
		// мутируем последний полученный event
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

// TestMulti_ConcurrentCloseAndWrite_NoRace — race detector должен оставаться
// чистым при гонке Close vs Write. Без mutex-а вокруг
// `select <-closed / wg.Add` тест с большой вероятностью провалится в
// `-race` через «WaitGroup is reused before previous Wait has returned»
// или через дочерний Write после возврата Close.
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
	// Close посредине вала Write-ов — попадает в race-window между
	// `select <-closed` и `wg.Add(1)` без mutex-а.
	closeErr := w.Close(context.Background())
	wg.Wait()
	// drain 2s > задержки в mockWriter (0) → таймаута быть не должно.
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	// После Close дополнительный Write должен пойти только в primary.
	if err := w.Write(context.Background(), newEvent()); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}
	// sec.Calls() мог быть от 0 до n включительно — все варианты ok,
	// проверяем только инвариант: вызовов не больше, чем primary видел
	// до Close (n + 1 post-Close).
	if got := sec.Calls(); got > int32(n) {
		t.Errorf("secondary calls = %d, want ≤ %d (post-Close write must skip secondary)", got, n)
	}
}

// TestMulti_ContextDetach_SecondaryGetsDetachedContext — caller отменяет
// ctx сразу после Write; secondary должен получить не-cancelled ctx.
func TestMulti_ContextDetach_SecondaryGetsDetachedContext(t *testing.T) {
	prim := &mockWriter{}
	gotCtx := make(chan context.Context, 1)
	// kontextCapturingWriter инлайн — захватывает ctx и сразу возвращает.
	sec := &ctxCapturingWriter{ch: gotCtx}

	w := New(prim, []audit.Writer{sec})
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Write(ctx, newEvent()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	cancel() // caller-ctx отменён сразу после Write — типовой HTTP-handler.

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
	// Небольшая задержка, чтобы caller успел сделать cancel ДО Write-а.
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
	// После Close — Write всё ещё работает в primary, но secondary не
	// запускается.
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
