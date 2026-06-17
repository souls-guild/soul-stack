package applybus

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestBus() *EventBus {
	return NewBus(slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func TestSubscribePublishReceive(t *testing.T) {
	b := newTestBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := b.Subscribe(ctx, "01J000000000000000000000AB")
	b.Publish(Event{ApplyID: "01J000000000000000000000AB", Kind: KindTaskExecuted})

	select {
	case ev := <-ch:
		if ev.Kind != KindTaskExecuted {
			t.Errorf("Kind = %q, want task.executed", ev.Kind)
		}
		if ev.At.IsZero() {
			t.Error("At is zero — Publish must stamp default")
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered within 1s")
	}
}

func TestPublishNoSubscribers(t *testing.T) {
	b := newTestBus()
	// Не должно паниковать или блокироваться.
	b.Publish(Event{ApplyID: "no-one", Kind: KindApplyStarted})
}

func TestPublishEmptyApplyIDIgnored(t *testing.T) {
	b := newTestBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := b.Subscribe(ctx, "x")
	b.Publish(Event{ApplyID: "", Kind: KindTaskExecuted})

	select {
	case ev := <-ch:
		t.Errorf("unexpected event with empty apply_id delivered: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeEmptyApplyIDReturnsClosed(t *testing.T) {
	b := newTestBus()
	ch := b.Subscribe(context.Background(), "")
	if _, ok := <-ch; ok {
		t.Error("expected closed channel for empty apply_id")
	}
}

func TestSubscribeNilCtxReturnsClosed(t *testing.T) {
	b := newTestBus()
	ch := b.Subscribe(nil, "any")
	if _, ok := <-ch; ok {
		t.Error("expected closed channel for nil ctx")
	}
}

func TestMultipleSubscribersReceiveSameEvent(t *testing.T) {
	b := newTestBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const subs = 5
	chs := make([]<-chan Event, subs)
	for i := 0; i < subs; i++ {
		chs[i] = b.Subscribe(ctx, "apply-X")
	}
	b.Publish(Event{ApplyID: "apply-X", Kind: KindApplyCompleted})

	for i, ch := range chs {
		select {
		case ev := <-ch:
			if ev.Kind != KindApplyCompleted {
				t.Errorf("sub %d kind = %q", i, ev.Kind)
			}
		case <-time.After(time.Second):
			t.Errorf("sub %d did not receive event", i)
		}
	}
}

func TestLateSubscriberMissesEarlierEvents(t *testing.T) {
	b := newTestBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.Publish(Event{ApplyID: "apply-Y", Kind: KindTaskExecuted})
	ch := b.Subscribe(ctx, "apply-Y")

	select {
	case <-ch:
		t.Error("late subscriber should not receive earlier events")
	case <-time.After(50 * time.Millisecond):
	}

	// А вот следующий Publish — получит.
	b.Publish(Event{ApplyID: "apply-Y", Kind: KindApplyCompleted})
	select {
	case ev := <-ch:
		if ev.Kind != KindApplyCompleted {
			t.Errorf("Kind = %q", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("subsequent event not delivered")
	}
}

func TestUnsubscribeOnCtxCancel(t *testing.T) {
	b := newTestBus()
	ctx, cancel := context.WithCancel(context.Background())
	ch := b.Subscribe(ctx, "apply-Z")

	if got := b.Subscribers("apply-Z"); got != 1 {
		t.Fatalf("Subscribers before cancel = %d, want 1", got)
	}
	cancel()

	// Ждём пока goroutine сделает unsubscribe.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if b.Subscribers("apply-Z") == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := b.Subscribers("apply-Z"); got != 0 {
		t.Fatalf("Subscribers after cancel = %d, want 0", got)
	}

	// Канал НЕ закрывается (вариант A done-канала): инвариант — Publish
	// после cancel ничего не доставляет (subscriber снят, done закрыт).
	b.Publish(Event{ApplyID: "apply-Z", Kind: KindTaskExecuted})
	select {
	case ev := <-ch:
		t.Errorf("unexpected delivery after ctx cancel: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBufferOverflowDropsOldest(t *testing.T) {
	b := newTestBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := b.Subscribe(ctx, "apply-overflow")

	// Заливаем SubscriberBufferSize+10 событий без чтения. Должно
	// сохраниться ровно SubscriberBufferSize самых свежих.
	total := SubscriberBufferSize + 10
	for i := 0; i < total; i++ {
		b.Publish(Event{ApplyID: "apply-overflow", Kind: KindTaskExecuted, Payload: i})
	}

	// Вычитываем всё, что есть, без блокировки. Должно получиться
	// SubscriberBufferSize событий, и последнее — i=total-1 (newest сохранён).
	var got []Event
drain:
	for {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-time.After(20 * time.Millisecond):
			break drain
		}
	}
	if len(got) != SubscriberBufferSize {
		t.Fatalf("got %d events, want exactly %d after overflow", len(got), SubscriberBufferSize)
	}
	last := got[len(got)-1]
	if last.Payload.(int) != total-1 {
		t.Errorf("newest event payload = %v, want %d (drop-oldest semantics)", last.Payload, total-1)
	}
}

func TestConcurrentPublishAndSubscribe(t *testing.T) {
	b := newTestBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		applyIDs    = 8
		pubPerID    = 50
		subsPerID   = 3
		readWorkers = applyIDs * subsPerID
	)

	subChans := make([]<-chan Event, 0, readWorkers)
	for i := 0; i < applyIDs; i++ {
		id := applyKey(i)
		for j := 0; j < subsPerID; j++ {
			subChans = append(subChans, b.Subscribe(ctx, id))
		}
	}

	var received atomic.Int64
	var wgR sync.WaitGroup
	wgR.Add(len(subChans))
	for _, ch := range subChans {
		ch := ch
		go func() {
			defer wgR.Done()
			for {
				select {
				case _, ok := <-ch:
					// ch не закрывается (вариант A) — ветка !ok недостижима,
					// оставлена как defensive. Выход — строго по ctx.Done().
					if !ok {
						return
					}
					received.Add(1)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	var wgP sync.WaitGroup
	wgP.Add(applyIDs)
	for i := 0; i < applyIDs; i++ {
		id := applyKey(i)
		go func() {
			defer wgP.Done()
			for k := 0; k < pubPerID; k++ {
				b.Publish(Event{ApplyID: id, Kind: KindTaskExecuted})
			}
		}()
	}
	wgP.Wait()

	// Даём reader-ам время выпить очередь.
	time.Sleep(50 * time.Millisecond)
	cancel()
	wgR.Wait()

	want := int64(applyIDs * subsPerID * pubPerID)
	if received.Load() != want {
		// При drop-oldest under load цифры могут различаться; но 50 событий
		// при буфере 64 — гарантированно без потерь.
		t.Errorf("received = %d, want %d (publish=%d, subs=%d, ids=%d)",
			received.Load(), want, pubPerID, subsPerID, applyIDs)
	}
}

func applyKey(i int) string {
	return "apply-" + string(rune('A'+i))
}

func TestPublishStampsAtIfZero(t *testing.T) {
	b := newTestBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx, "stamp")

	before := time.Now().UTC()
	b.Publish(Event{ApplyID: "stamp", Kind: KindApplyStarted})

	select {
	case ev := <-ch:
		if ev.At.Before(before) {
			t.Errorf("stamped At=%v is before Publish call (=%v)", ev.At, before)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

// TestNewBusWithRedis_NilRedisFallsBackToLocal — redis=nil / kid="" не
// должны включать cluster-mode и не должны менять local-поведение.
func TestNewBusWithRedis_NilRedisFallsBackToLocal(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := []struct {
		name string
		kid  string
	}{
		{"nil redis empty kid", ""},
		{"nil redis non-empty kid", "keeper-X"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBusWithRedis(logger, nil, tc.kid)
			if b.clusterEnabled() {
				t.Errorf("clusterEnabled = true, want false (redis=nil)")
			}
			// Smoke: local publish/subscribe работает.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := b.Subscribe(ctx, "apply-local")
			b.Publish(Event{ApplyID: "apply-local", Kind: KindTaskExecuted})
			select {
			case ev := <-ch:
				if ev.Kind != KindTaskExecuted {
					t.Errorf("Kind = %q", ev.Kind)
				}
			case <-time.After(time.Second):
				t.Fatal("no event")
			}
		})
	}
}

func TestPublishPreservesNonZeroAt(t *testing.T) {
	b := newTestBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx, "preserve")

	fixed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Publish(Event{ApplyID: "preserve", Kind: KindTaskExecuted, At: fixed})

	select {
	case ev := <-ch:
		if !ev.At.Equal(fixed) {
			t.Errorf("At = %v, want preserved %v", ev.At, fixed)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}
