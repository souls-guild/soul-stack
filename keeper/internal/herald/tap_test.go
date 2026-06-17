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

// blockingQueue блокирует первый Enqueue до release — имитирует медленный
// consumer (downstream доставки/PG-кэш не успевает), заставляя буфер tap-а
// переполниться.
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
	// Правило, которое сматчит ВСЕ scenario_run-события (consumer на каждом
	// зовёт Enqueue, который заблокирован → consumer застрянет на первом).
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

	// Первое событие — consumer его заберёт и застрянет на blockingQueue.Enqueue.
	tap.Observe(context.Background(), event)
	select {
	case <-bq.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer не вошёл в Enqueue — tap не разбирает буфер")
	}

	// Теперь consumer заблокирован. Заполняем буфер (buf событий) + переполняем
	// (ещё N — они должны дропнуться).
	const overflow = 5
	for i := 0; i < buf+overflow; i++ {
		tap.Observe(context.Background(), event)
	}

	drops := counterValue(t, metrics.tapDropped)
	if drops < float64(overflow) {
		t.Fatalf("ожидалось >= %d дропов при переполнении буфера, получили %v", overflow, drops)
	}
}

func TestTap_Observe_NeverBlocks(t *testing.T) {
	// Даже при наглухо заблокированном consumer Observe обязан вернуться
	// мгновенно (неблокирующий select-with-default).
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
		// Много Observe подряд — ни один не должен заблокироваться.
		for i := 0; i < 1000; i++ {
			tap.Observe(context.Background(), event)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Observe заблокировался при полном буфере и застрявшем consumer-е")
	}
}

func TestTap_Close_Idempotent(t *testing.T) {
	d := NewDispatcher(DispatcherConfig{Source: &staticSource{}, Queue: &fakeQueue{}})
	tap := NewNotificationTap(d, nil, 4)
	tap.Close()
	tap.Close() // повторный Close не должен паниковать / висеть
}

// TestTap_Close_Concurrent — guard на MAJOR из review S2: конкурентный Close не
// должен паниковать «close of closed channel». 8 горутин с общим start-барьером
// вызывают Close одновременно; sync.Once гарантирует единственное закрытие
// done-канала. Прогон под -race ловит и гонку, и double-close. Стресс-итерации
// повышают шанс воспроизвести узкое окно между select и close (старый баг).
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
				<-start // общий барьер: все стартуют как можно ближе друг к другу
				tap.Close()
			}()
		}
		close(start)
		wg.Wait() // не должно ни паниковать, ни зависнуть
	}
}

// TestTap_Observe_During_Close — guard на MAJOR из re-review S2: Observe ∥ Close
// не должен паниковать send-after-close. Раньше ch закрывался в Close, а
// select-with-default в Observe ловил только полный буфер, не закрытый канал —
// гонка приводила к панике «send on closed channel». Теперь ch не закрывается
// (shutdown через done), и эта паника невозможна by construction. Стресс: на
// каждой итерации несколько Observe-горутин крутятся параллельно с Close под
// -race; ноль паник = инвариант держится.
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
		close(start) // отпускаем Observe-горутины и Close максимально близко по времени
		wg.Wait()    // ни одна Observe не должна паниковать на send-after-close
	}
}

// TestTap_Observe_After_Close — guard: Observe после завершившегося Close —
// тихий no-op, без паники. done уже закрыт → первая done-ветка select-а
// возвращает управление, в ch ничего не пишется.
func TestTap_Observe_After_Close(t *testing.T) {
	d := NewDispatcher(DispatcherConfig{Source: &staticSource{}, Queue: &fakeQueue{}})
	tap := NewNotificationTap(d, nil, 4)
	tap.Close()

	event := &audit.Event{EventType: audit.EventScenarioRunCompleted}
	for i := 0; i < 100; i++ {
		tap.Observe(context.Background(), event) // не должно паниковать
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

	// Ждём, пока consumer разберёт буфер и поставит job.
	deadline := time.After(2 * time.Second)
	for {
		if len(q.snapshot()) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("tap не доставил событие в dispatcher")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := q.snapshot()[0].CorrelationID; got != "vy_42" {
		t.Fatalf("job несёт неверный CorrelationID: %q", got)
	}
}
