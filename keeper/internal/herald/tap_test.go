package herald

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// blockingQueue blocks the first Enqueue until release. It simulates a slow
// consumer (delivery downstream / PG cache cannot keep up), forcing the tap buffer
// to overflow.
type blockingQueue struct {
	release chan struct{}
	once    sync.Once
	entered chan struct{}
}

func newBlockingQueue() *blockingQueue {
	return &blockingQueue{release: make(chan struct{}), entered: make(chan struct{}, 1)}
}

func (q *blockingQueue) Enqueue(_ context.Context, _ *DeliveryJob) error {
	q.once.Do(func() { q.entered <- struct{}{} })
	<-q.release
	return nil
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("collect counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestTap_BufferFull_DropsAndCounts(t *testing.T) {
	// Rule that matches all scenario_run events. The consumer calls Enqueue for
	// each, and Enqueue is blocked, so the consumer gets stuck on the first one.
	src := &staticSource{rules: []*Tiding{
		{Name: "a", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: true},
	}}
	bq := newBlockingQueue()
	d := NewDispatcher(DispatcherConfig{Source: src, Queue: bq})

	const buf = 2
	tap := NewNotificationTap(d, nil, buf)
	defer func() {
		close(bq.release)
		tap.Close()
	}()

	metrics := RegisterDispatcherMetrics(obs.NewRegistry())
	tap.SetMetrics(metrics)

	event := &audit.Event{EventType: audit.EventScenarioRunCompleted, Payload: map[string]any{
		"summary": map[string]any{"succeeded": 1},
	}}

	// First event: consumer takes it and gets stuck in blockingQueue.Enqueue.
	tap.Observe(context.Background(), event)
	select {
	case <-bq.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer did not enter Enqueue: tap is not consuming the buffer")
	}

	// Now the consumer is blocked. Fill the buffer (buf events) and overflow it
	// with N more events that should be dropped.
	const overflow = 5
	for i := 0; i < buf+overflow; i++ {
		tap.Observe(context.Background(), event)
	}

	drops := counterValue(t, metrics.tapDropped)
	if drops < float64(overflow) {
		t.Fatalf("expected >= %d drops on buffer overflow, got %v", overflow, drops)
	}
}

func TestTap_Observe_NeverBlocks(t *testing.T) {
	// Even with a fully blocked consumer, Observe must return immediately
	// (non-blocking select-with-default).
	bq := newBlockingQueue()
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: []*Tiding{
			{Name: "a", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: true},
		}},
		Queue: bq,
	})
	tap := NewNotificationTap(d, nil, 1)
	defer func() {
		close(bq.release)
		tap.Close()
	}()

	event := &audit.Event{EventType: audit.EventScenarioRunCompleted}
	done := make(chan struct{})
	go func() {
		// Many consecutive Observe calls; none must block.
		for i := 0; i < 1000; i++ {
			tap.Observe(context.Background(), event)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Observe blocked with full buffer and stuck consumer")
	}
}

func TestTap_Close_Idempotent(t *testing.T) {
	d := NewDispatcher(DispatcherConfig{Source: &staticSource{}, Queue: &fakeQueue{}})
	tap := NewNotificationTap(d, nil, 4)
	tap.Close()
	tap.Close() // Repeated Close must not panic or hang.
}

// TestTap_Close_Concurrent guards a MAJOR from review S2: concurrent Close must
// not panic with "close of closed channel". 8 goroutines with a shared start
// barrier call Close at the same time; sync.Once guarantees one close of the done
// channel. Running with -race catches both race and double-close. Stress
// iterations increase the chance of reproducing the narrow window between select
// and close from the old bug.
func TestTap_Close_Concurrent(t *testing.T) {
	const iterations = 2000
	for it := 0; it < iterations; it++ {
		d := NewDispatcher(DispatcherConfig{Source: &staticSource{}, Queue: &fakeQueue{}})
		tap := NewNotificationTap(d, nil, 4)

		const closers = 8
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(closers)
		for i := 0; i < closers; i++ {
			go func() {
				defer wg.Done()
				<-start // Shared barrier: all start as close together as possible.
				tap.Close()
			}()
		}
		close(start)
		wg.Wait() // Must neither panic nor hang.
	}
}

// TestTap_Observe_During_Close guards a MAJOR from re-review S2: Observe and Close
// must not panic with send-after-close. Previously ch was closed in Close, and
// select-with-default in Observe handled only a full buffer, not a closed channel;
// the race caused "send on closed channel". Now ch is not closed (shutdown through
// done), so this panic is impossible by construction. Stress: on every iteration
// several Observe goroutines run in parallel with Close under -race; zero panics
// means the invariant holds.
func TestTap_Observe_During_Close(t *testing.T) {
	const iterations = 500
	for it := 0; it < iterations; it++ {
		d := NewDispatcher(DispatcherConfig{Source: &staticSource{}, Queue: &fakeQueue{}})
		tap := NewNotificationTap(d, nil, 4)

		event := &audit.Event{EventType: audit.EventScenarioRunCompleted}
		const observers = 4
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(observers + 1)
		for i := 0; i < observers; i++ {
			go func() {
				defer wg.Done()
				<-start
				for j := 0; j < 50; j++ {
					tap.Observe(context.Background(), event)
				}
			}()
		}
		go func() {
			defer wg.Done()
			<-start
			tap.Close()
		}()
		close(start) // Release Observe goroutines and Close as close together as possible.
		wg.Wait()    // No Observe may panic on send-after-close.
	}
}

// TestTap_Observe_After_Close guards that Observe after completed Close is a quiet
// no-op without panic. done is already closed, so the first done branch of select
// returns control and writes nothing to ch.
func TestTap_Observe_After_Close(t *testing.T) {
	d := NewDispatcher(DispatcherConfig{Source: &staticSource{}, Queue: &fakeQueue{}})
	tap := NewNotificationTap(d, nil, 4)
	tap.Close()

	event := &audit.Event{EventType: audit.EventScenarioRunCompleted}
	for i := 0; i < 100; i++ {
		tap.Observe(context.Background(), event) // Must not panic.
	}
}

func TestTap_DeliversToDispatcher(t *testing.T) {
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: []*Tiding{
			{Name: "a", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: true},
		}},
		Queue: q,
	})
	tap := NewNotificationTap(d, nil, 16)
	defer tap.Close()

	tap.Observe(context.Background(), &audit.Event{
		EventType:     audit.EventScenarioRunCompleted,
		CorrelationID: "vy_42",
		Payload:       map[string]any{"summary": map[string]any{"succeeded": 1}},
	})

	// Wait until consumer drains the buffer and enqueues a job.
	deadline := time.After(2 * time.Second)
	for {
		if len(q.snapshot()) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("tap did not deliver event to dispatcher")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := q.snapshot()[0].CorrelationID; got != "vy_42" {
		t.Fatalf("job carries wrong CorrelationID: %q", got)
	}
}
