package beacon

import (
	"context"
	"sync"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeBeacon — управляемое тело проверки: возвращает state, выставленный
// SetState, и сигналит каждый Check через checked. Так тест синхронизируется с
// Vigil-горутиной без time.Sleep.
type fakeBeacon struct {
	mu      sync.Mutex
	state   State
	data    *structpb.Struct
	err     error
	checked chan struct{}
}

func newFakeBeacon(initial State) *fakeBeacon {
	return &fakeBeacon{state: initial, checked: make(chan struct{}, 8)}
}

func (f *fakeBeacon) Check(_ context.Context, _ *structpb.Struct) (State, *structpb.Struct, error) {
	f.mu.Lock()
	st, data, err := f.state, f.data, f.err
	f.mu.Unlock()
	f.checked <- struct{}{}
	return st, data, err
}

func (f *fakeBeacon) SetState(st State) {
	f.mu.Lock()
	f.state = st
	f.mu.Unlock()
}

func (f *fakeBeacon) SetErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// regWith собирает реестр с одним fake-beacon под заданным адресом.
func regWith(name string, b Beacon) *Registry {
	return &Registry{beacons: map[string]Beacon{name: b}}
}

func vigil(name, check, interval string) *keeperv1.VigilDef {
	return &keeperv1.VigilDef{Name: name, Check: check, Interval: interval}
}

// waitChecked ждёт один Check от fake-beacon (синхронизация с Vigil-горутиной).
func waitChecked(t *testing.T, f *fakeBeacon) {
	t.Helper()
	select {
	case <-f.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("beacon Check не вызван в срок")
	}
}

// expectNoPortent проверяет, что в канал не пришёл Portent за короткое окно.
func expectNoPortent(t *testing.T, s *Scheduler) {
	t.Helper()
	select {
	case ev := <-s.Portents():
		t.Fatalf("неожиданный Portent: %v", ev.GetBeaconName())
	case <-time.After(100 * time.Millisecond):
	}
}

// expectPortent ждёт ровно один Portent и возвращает его.
func expectPortent(t *testing.T, s *Scheduler) *keeperv1.PortentEvent {
	t.Helper()
	select {
	case ev := <-s.Portents():
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("ожидали Portent, не пришёл")
		return nil
	}
}

func newTestScheduler(t *testing.T, reg *Registry) (*Scheduler, *ManualTicker) {
	t.Helper()
	s := NewScheduler(SchedulerConfig{Registry: reg, SID: "host.example"})
	mt := NewManualTicker()
	s.SetTicker(func(time.Duration) Ticker { return mt })
	return s, mt
}

// TestBaselineNoPortent — первая проверка устанавливает baseline без Portent.
func TestBaselineNoPortent(t *testing.T) {
	fb := newFakeBeacon("up")
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})

	mt.Tick()
	waitChecked(t, fb)
	expectNoPortent(t, s) // baseline — без Portent
	s.Stop()
}

// TestEdgeTriggeredOnChange — смена state после baseline → ровно один Portent с
// корректными полями.
func TestEdgeTriggeredOnChange(t *testing.T) {
	fb := newFakeBeacon("up")
	now := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	s.SetNow(func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("svc-vigil", "core.beacon.x", "1s")})

	// baseline
	mt.Tick()
	waitChecked(t, fb)
	expectNoPortent(t, s)

	// смена состояния
	fb.SetState("down")
	mt.Tick()
	waitChecked(t, fb)

	ev := expectPortent(t, s)
	if ev.GetBeaconName() != "svc-vigil" {
		t.Errorf("beacon_name = %q, want svc-vigil", ev.GetBeaconName())
	}
	if ev.GetSid() != "host.example" {
		t.Errorf("sid = %q, want host.example", ev.GetSid())
	}
	if !ev.GetCollectedAt().AsTime().Equal(now) {
		t.Errorf("collected_at = %v, want %v", ev.GetCollectedAt().AsTime(), now)
	}
	s.Stop()
}

// TestNoChangeNoPortent — совпадение state с last не эмитит Portent.
func TestNoChangeNoPortent(t *testing.T) {
	fb := newFakeBeacon("up")
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})

	mt.Tick() // baseline up
	waitChecked(t, fb)
	expectNoPortent(t, s)

	mt.Tick() // снова up — нет смены
	waitChecked(t, fb)
	expectNoPortent(t, s)
	s.Stop()
}

// TestFlapEmitsTwice — up→down→up даёт два Portent (каждый edge).
func TestFlapEmitsTwice(t *testing.T) {
	fb := newFakeBeacon("up")
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})
	mt.Tick() // baseline up
	waitChecked(t, fb)
	expectNoPortent(t, s)

	fb.SetState("down")
	mt.Tick()
	waitChecked(t, fb)
	if expectPortent(t, s) == nil {
		return
	}

	fb.SetState("up")
	mt.Tick()
	waitChecked(t, fb)
	if expectPortent(t, s) == nil {
		return
	}
	s.Stop()
}

// TestCheckErrorNoBaselineNoPortent — ошибка Check не двигает baseline и не
// эмитит Portent; после восстановления первая успешная проверка — снова baseline.
func TestCheckErrorNoBaselineNoPortent(t *testing.T) {
	fb := newFakeBeacon("up")
	fb.SetErr(context.DeadlineExceeded)
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})

	mt.Tick() // ошибка → ни baseline, ни Portent
	waitChecked(t, fb)
	expectNoPortent(t, s)

	// Восстановление: первая успешная проверка — baseline (без Portent), несмотря
	// на то, что был тик с ошибкой.
	fb.SetErr(nil)
	fb.SetState("down")
	mt.Tick()
	waitChecked(t, fb)
	expectNoPortent(t, s)
	s.Stop()
}

// TestReplaceAllRemovesVigil — новый snapshot без прежнего Vigil останавливает
// его (Check больше не вызывается).
func TestReplaceAllRemovesVigil(t *testing.T) {
	fb := newFakeBeacon("up")
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})
	mt.Tick()
	waitChecked(t, fb)

	// Пустой snapshot — Vigil остановлен и забыт.
	s.Apply(ctx, nil)

	// Tick после остановки: горутины нет, Check не приходит.
	mt.Tick()
	select {
	case <-fb.checked:
		t.Fatal("Vigil продолжил проверки после удаления из snapshot")
	case <-time.After(150 * time.Millisecond):
	}
	s.Stop()
}

// TestReplaceAllSameDefKeepsBaseline — повторный snapshot с тем же определением
// НЕ перезапускает Vigil: baseline сохраняется, реальная последующая смена
// эмитит Portent (а не подавляется новым baseline).
func TestReplaceAllSameDefKeepsBaseline(t *testing.T) {
	fb := newFakeBeacon("up")
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	def := vigil("v1", "core.beacon.x", "1s")
	s.Apply(ctx, []*keeperv1.VigilDef{def})
	mt.Tick() // baseline up
	waitChecked(t, fb)
	expectNoPortent(t, s)

	// Тот же snapshot снова — не должен сбросить baseline.
	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})

	// Смена состояния → Portent (если бы baseline сбросился, был бы новый baseline
	// без события).
	fb.SetState("down")
	mt.Tick()
	waitChecked(t, fb)
	if ev := expectPortent(t, s); ev.GetBeaconName() != "v1" {
		t.Fatalf("ожидали Portent v1, получили %q", ev.GetBeaconName())
	}
	s.Stop()
}

// TestUnknownCheckSkipped — Vigil с неизвестным check не запускается и не валит
// scheduler (известный Vigil рядом работает).
func TestUnknownCheckSkipped(t *testing.T) {
	fb := newFakeBeacon("up")
	s, mt := newTestScheduler(t, regWith("core.beacon.known", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{
		vigil("bad", "core.beacon.missing", "1s"),
		vigil("good", "core.beacon.known", "1s"),
	})

	mt.Tick()
	waitChecked(t, fb) // good работает
	s.Stop()
}

// TestInvalidIntervalSkipped — Vigil с невалидным interval не запускается.
func TestInvalidIntervalSkipped(t *testing.T) {
	fb := newFakeBeacon("up")
	s, _ := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "not-a-duration")})

	select {
	case <-fb.checked:
		t.Fatal("Vigil с невалидным interval не должен запускаться")
	case <-time.After(150 * time.Millisecond):
	}
	s.Stop()
}

// TestNilSchedulerSafe — nil-receiver не паникует (тестовая обвязка без контура).
func TestNilSchedulerSafe(t *testing.T) {
	var s *Scheduler
	s.Apply(context.Background(), nil)
	s.Stop()
	if s.Portents() != nil {
		t.Fatal("nil-scheduler должен отдавать nil-канал Portents")
	}
}
