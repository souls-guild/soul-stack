package audit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingWriter — primary-writer-заглушка: считает записи и опц. возвращает
// заданную ошибку.
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

// recordingTap — tap-заглушка: считает Observe-вызовы.
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
		t.Fatalf("без tap-ов NewMultiWriter должен вернуть primary как есть, получил %T", w)
	}
	// nil-tap-ы тоже отбрасываются.
	w = NewMultiWriter(primary, nil, nil, nil)
	if w != Writer(primary) {
		t.Fatalf("nil-tap-ы должны отбрасываться, получил %T", w)
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
		t.Fatalf("primary должен записать 1 событие, записал %d", primary.count())
	}
	if tap1.observed.Load() != 1 || tap2.observed.Load() != 1 {
		t.Fatalf("оба tap-а должны получить событие: tap1=%d tap2=%d", tap1.observed.Load(), tap2.observed.Load())
	}
}

func TestMultiWriter_PrimaryFail_TapNotCalled(t *testing.T) {
	wantErr := errors.New("pg down")
	primary := &recordingWriter{err: wantErr}
	tap := &recordingTap{}
	w := NewMultiWriter(primary, nil, tap)

	err := w.Write(context.Background(), &Event{EventType: "scenario_run.failed"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("ошибка primary должна пробрасываться как есть, получил %v", err)
	}
	if tap.observed.Load() != 0 {
		t.Fatalf("при сбое primary tap НЕ должен вызываться, вызван %d раз", tap.observed.Load())
	}
}

// failingTap паникует в Observe — проверяем, что Write не падает.
type panicTap struct{}

func (panicTap) Observe(_ context.Context, _ *Event) { panic("boom") }

func TestMultiWriter_TapPanic_DoesNotFailWrite(t *testing.T) {
	primary := &recordingWriter{}
	after := &recordingTap{}
	w := NewMultiWriter(primary, nil, panicTap{}, after)

	if err := w.Write(context.Background(), &Event{EventType: "command_run.completed"}); err != nil {
		t.Fatalf("паника tap-а не должна фейлить Write, получил %v", err)
	}
	if primary.count() != 1 {
		t.Fatalf("primary-запись должна состояться несмотря на панику tap-а")
	}
	// tap после паникующего тоже должен быть вызван (паника изолирована).
	if after.observed.Load() != 1 {
		t.Fatalf("tap после паникующего должен быть вызван, вызван %d раз", after.observed.Load())
	}
	mw, ok := w.(*MultiWriter)
	if !ok {
		t.Fatalf("ожидался *MultiWriter, получил %T", w)
	}
	if mw.tapPanics.Load() != 1 {
		t.Fatalf("счётчик паник должен быть 1, получил %d", mw.tapPanics.Load())
	}
}

// slowTap блокируется в Observe на сигнале release — проверяем, что MultiWriter
// сам не вводит блокировку (slow-tap — ответственность реализации tap-а; здесь
// убеждаемся, что Write завершается после возврата Observe и порядок tap→Write
// корректен). Реальная неблокируемость buffered-tap-а — в herald/tap_test.go.
func TestMultiWriter_TapInvokedAfterPrimary(t *testing.T) {
	primary := &recordingWriter{}
	order := make(chan string, 2)
	primaryOrderTap := tapFunc(func(_ context.Context, _ *Event) {
		order <- "tap"
	})
	// Оборачиваем primary, чтобы зафиксировать порядок.
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
		t.Fatal("Write завис")
	}
	if got := <-order; got != "primary" {
		t.Fatalf("primary должен записаться ПЕРВЫМ, первым был %q", got)
	}
	if got := <-order; got != "tap" {
		t.Fatalf("tap должен вызваться ВТОРЫМ, вторым был %q", got)
	}
}

// tapFunc / writerFunc — функциональные адаптеры для тестов.
type tapFunc func(context.Context, *Event)

func (f tapFunc) Observe(ctx context.Context, ev *Event) { f(ctx, ev) }

type writerFunc func(context.Context, *Event) error

func (f writerFunc) Write(ctx context.Context, ev *Event) error { return f(ctx, ev) }
