package beacon

import (
	"context"
	"sync"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeBeacon — a controllable check body: returns the state set via SetState
// and signals each Check through checked. This lets the test synchronize with
// the Vigil goroutine without time.Sleep.
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

// regWith builds a registry with a single fake-beacon under the given address.
func regWith(name string, b Beacon) *Registry {
	return &Registry{beacons: map[string]Beacon{name: b}}
}

func vigil(name, check, interval string) *keeperv1.VigilDef {
	return &keeperv1.VigilDef{Name: name, Check: check, Interval: interval}
}

// waitChecked waits for one Check from fake-beacon (synchronizes with the Vigil goroutine).
func waitChecked(t *testing.T, f *fakeBeacon) {
	t.Helper()
	select {
	case <-f.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("beacon Check не вызван в срок")
	}
}

// expectNoPortent verifies no Portent arrived on the channel within a short window.
func expectNoPortent(t *testing.T, s *Scheduler) {
	t.Helper()
	select {
	case ev := <-s.Portents():
		t.Fatalf("неожиданный Portent: %v", ev.GetBeaconName())
	case <-time.After(100 * time.Millisecond):
	}
}

// expectPortent waits for exactly one Portent and returns it.
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

// TestBaselineNoPortent — the first check establishes a baseline without a Portent.
func TestBaselineNoPortent(t *testing.T) {
	fb := newFakeBeacon("up")
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})

	mt.Tick()
	waitChecked(t, fb)
	expectNoPortent(t, s) // baseline — no Portent
	s.Stop()
}

// TestEdgeTriggeredOnChange — a state change after baseline → exactly one
// Portent with correct fields.
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

	// state change
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

// TestNoChangeNoPortent — state matching last doesn't emit a Portent.
func TestNoChangeNoPortent(t *testing.T) {
	fb := newFakeBeacon("up")
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})

	mt.Tick() // baseline up
	waitChecked(t, fb)
	expectNoPortent(t, s)

	mt.Tick() // still up — no change
	waitChecked(t, fb)
	expectNoPortent(t, s)
	s.Stop()
}

// TestFlapEmitsTwice — up→down→up produces two Portents (each edge).
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

// TestCheckErrorNoBaselineNoPortent — a Check error doesn't move the baseline
// and doesn't emit a Portent; after recovery, the first successful check is a
// new baseline.
func TestCheckErrorNoBaselineNoPortent(t *testing.T) {
	fb := newFakeBeacon("up")
	fb.SetErr(context.DeadlineExceeded)
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})

	mt.Tick() // error → neither baseline nor Portent
	waitChecked(t, fb)
	expectNoPortent(t, s)

	// Recovery: the first successful check is a baseline (no Portent), despite
	// the earlier tick having errored.
	fb.SetErr(nil)
	fb.SetState("down")
	mt.Tick()
	waitChecked(t, fb)
	expectNoPortent(t, s)
	s.Stop()
}

// TestReplaceAllRemovesVigil — a new snapshot without the previous Vigil stops
// it (Check is no longer called).
func TestReplaceAllRemovesVigil(t *testing.T) {
	fb := newFakeBeacon("up")
	s, mt := newTestScheduler(t, regWith("core.beacon.x", fb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})
	mt.Tick()
	waitChecked(t, fb)

	// Empty snapshot — the Vigil is stopped and forgotten.
	s.Apply(ctx, nil)

	// Tick after the stop: no goroutine, Check doesn't fire.
	mt.Tick()
	select {
	case <-fb.checked:
		t.Fatal("Vigil продолжил проверки после удаления из snapshot")
	case <-time.After(150 * time.Millisecond):
	}
	s.Stop()
}

// TestReplaceAllSameDefKeepsBaseline — a repeat snapshot with the same
// definition does NOT restart the Vigil: baseline is preserved, a real
// subsequent change emits a Portent (rather than being suppressed by a fresh baseline).
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

	// The same snapshot again — must not reset the baseline.
	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})

	// State change → Portent (if the baseline had reset, this would be a new
	// baseline with no event).
	fb.SetState("down")
	mt.Tick()
	waitChecked(t, fb)
	if ev := expectPortent(t, s); ev.GetBeaconName() != "v1" {
		t.Fatalf("ожидали Portent v1, получили %q", ev.GetBeaconName())
	}
	s.Stop()
}

// TestUnknownCheckSkipped — a Vigil with an unknown check doesn't start and
// doesn't crash the scheduler (a known Vigil alongside it still works).
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
	waitChecked(t, fb) // good is working
	s.Stop()
}

// TestInvalidIntervalSkipped — a Vigil with an invalid interval doesn't start.
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

// TestNilSchedulerSafe — nil-receiver doesn't panic (test harness without a live scheduler).
func TestNilSchedulerSafe(t *testing.T) {
	var s *Scheduler
	s.Apply(context.Background(), nil)
	s.Stop()
	if s.Portents() != nil {
		t.Fatal("nil-scheduler должен отдавать nil-канал Portents")
	}
}
