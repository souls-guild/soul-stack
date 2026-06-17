package toll

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeLease — held lease handle для fakeLeaseAcquirer.
type fakeLease struct {
	mu       sync.Mutex
	renewErr error
	released bool
	renews   int
}

func (l *fakeLease) Renew(_ context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renews++
	return l.renewErr
}

func (l *fakeLease) Release(_ context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = true
	return nil
}

// fakeLeaseAcquirer — controllable acquirer. Возвращает заранее заданные
// результаты по order-у; после исчерпания — конечный ответ.
type fakeLeaseAcquirer struct {
	mu        sync.Mutex
	script    []acquireResult
	idx       int
	calls     int
	leasePtrs []*fakeLease
}

type acquireResult struct {
	lease *fakeLease
	err   error
}

func (a *fakeLeaseAcquirer) Acquire(_ context.Context, _, _ string, _ time.Duration) (Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.idx >= len(a.script) {
		// После скрипта — упорно держим last (или ErrLeaseTaken если ничего нет).
		if len(a.script) > 0 {
			r := a.script[len(a.script)-1]
			if r.lease != nil {
				a.leasePtrs = append(a.leasePtrs, r.lease)
				return r.lease, r.err
			}
			return nil, r.err
		}
		return nil, ErrLeaseTaken
	}
	r := a.script[a.idx]
	a.idx++
	if r.lease != nil {
		a.leasePtrs = append(a.leasePtrs, r.lease)
		return r.lease, r.err
	}
	return nil, r.err
}

func (a *fakeLeaseAcquirer) callsCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// fakeSortedSet — controllable [SortedSetReader]: возвращает заданное count
// либо ошибку. Поддерживает мутацию из теста (между тиками меняется rate).
//
// Также реализует [CovenAwareReader] (per-coven trigger, ADR-038 amendment,
// extensions): при заданной covenCounts возвращает map. nil-map → reader
// возвращает (nil, nil) — leader интерпретирует как «нет данных, no trigger».
type fakeSortedSet struct {
	mu          sync.Mutex
	count       int64
	countEr     error
	trims       int
	trimErr     error
	covenCounts map[string]int64
	covenErr    error
}

func (s *fakeSortedSet) CountInWindow(_ context.Context, _, _ int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count, s.countEr
}

func (s *fakeSortedSet) TrimBelow(_ context.Context, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trims++
	return s.trimErr
}

func (s *fakeSortedSet) CountByCovenInWindow(_ context.Context, _, _ int64) (map[string]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.covenErr != nil {
		return nil, s.covenErr
	}
	if s.covenCounts == nil {
		return map[string]int64{}, nil
	}
	out := make(map[string]int64, len(s.covenCounts))
	for k, v := range s.covenCounts {
		out[k] = v
	}
	return out, nil
}

func (s *fakeSortedSet) setCount(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count = n
}

func (s *fakeSortedSet) setCovenCounts(m map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.covenCounts = m
}

// fakeDegradedWriter — controllable degradedWriter.
type fakeDegradedWriter struct {
	mu         sync.Mutex
	setCalls   int
	clearCalls int
	holder     string
	ttl        time.Duration
	setErr     error
	clearErr   error
}

func (w *fakeDegradedWriter) SetDegraded(_ context.Context, holder string, ttl time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.setCalls++
	w.holder = holder
	w.ttl = ttl
	return w.setErr
}

func (w *fakeDegradedWriter) ClearDegraded(_ context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.clearCalls++
	return w.clearErr
}

// fakeBaseline — fixed value baseline reader.
type fakeBaseline struct {
	mu    sync.Mutex
	value int64
	err   error
	calls int
}

func (b *fakeBaseline) BaselineConnected(_ context.Context) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return b.value, b.err
}

func (b *fakeBaseline) callsCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func newTestLeaderDeps() (*fakeLeaseAcquirer, *fakeSortedSet, *fakeDegradedWriter, *fakeBaseline, LeaderDeps) {
	acq := &fakeLeaseAcquirer{}
	ss := &fakeSortedSet{}
	dw := &fakeDegradedWriter{}
	bl := &fakeBaseline{value: 100} // 100 connected souls baseline по дефолту
	deps := LeaderDeps{
		Lease:          acq,
		SortedSet:      ss,
		DegradedWriter: dw,
		Baseline:       bl,
		Metrics:        nil,
		Logger:         newTestLogger(),
	}
	return acq, ss, dw, bl, deps
}

func newTestLeaderCfg() LeaderConfig {
	return LeaderConfig{
		KID:              "kid-test",
		LeaseTTL:         100 * time.Millisecond,
		AcquireRetry:     10 * time.Millisecond,
		TickInterval:     20 * time.Millisecond,
		WindowSize:       60 * time.Second,
		Threshold:        0.20,
		DegradedTTL:      60 * time.Second,
		ClearGrace:       50 * time.Millisecond,
		BaselineCacheTTL: 60 * time.Second,
	}
}

func TestLeader_NewLeader_RejectsInvalid(t *testing.T) {
	_, _, _, _, deps := newTestLeaderDeps()
	if _, err := NewLeader(LeaderConfig{}, deps); err == nil {
		t.Fatal("expected error for empty KID")
	}
	if _, err := NewLeader(LeaderConfig{KID: "k", LeaseTTL: 0, WindowSize: time.Second, Threshold: 0.1, DegradedTTL: time.Second, ClearGrace: time.Second}, deps); err == nil {
		t.Fatal("expected error for LeaseTTL=0")
	}
	if _, err := NewLeader(LeaderConfig{KID: "k", LeaseTTL: time.Second, WindowSize: time.Second, Threshold: 2, DegradedTTL: time.Second, ClearGrace: time.Second}, deps); err == nil {
		t.Fatal("expected error for Threshold > 1")
	}
}

func TestLeader_AcquireRetryThenSucceed(t *testing.T) {
	acq, _, _, _, deps := newTestLeaderDeps()
	acq.script = []acquireResult{
		{err: ErrLeaseTaken},
		{err: ErrLeaseTaken},
		{lease: &fakeLease{}},
	}
	l, err := NewLeader(newTestLeaderCfg(), deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	l.Run(ctx)
	if acq.callsCount() < 3 {
		t.Fatalf("ожидался ≥3 acquire (2 conflict + 1 success), got %d", acq.callsCount())
	}
}

func TestLeader_SetDegradedWhenRateExceedsThreshold(t *testing.T) {
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(25) // 25/100 = 0.25 > 0.20

	cfg := newTestLeaderCfg()
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	dw.mu.Lock()
	calls := dw.setCalls
	clearCalls := dw.clearCalls
	dw.mu.Unlock()
	if calls == 0 {
		t.Fatal("ожидался ≥1 SetDegraded при rate > threshold")
	}
	if clearCalls > 0 {
		t.Fatalf("ожидался 0 ClearDegraded, got %d", clearCalls)
	}
}

func TestLeader_NoActionWhenBelowThreshold(t *testing.T) {
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(10) // 10/100 = 0.10 < 0.20

	l, err := NewLeader(newTestLeaderCfg(), deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	dw.mu.Lock()
	calls := dw.setCalls
	clearCalls := dw.clearCalls
	dw.mu.Unlock()
	if calls != 0 {
		t.Fatalf("ожидался 0 SetDegraded при rate < threshold, got %d", calls)
	}
	if clearCalls != 0 {
		t.Fatalf("ожидался 0 ClearDegraded (никогда не было set), got %d", clearCalls)
	}
}

func TestLeader_ClearDegraded_RequiresGrace(t *testing.T) {
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(25) // > threshold → degraded set

	cfg := newTestLeaderCfg()
	cfg.TickInterval = 20 * time.Millisecond
	cfg.ClearGrace = 100 * time.Millisecond
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	// Run-сценарий: первые 50ms — high rate; затем — drop; ждём grace; затем — clear.
	go func() {
		time.Sleep(50 * time.Millisecond)
		ss.setCount(5) // rate=0.05 < threshold
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	dw.mu.Lock()
	setCalls := dw.setCalls
	clearCalls := dw.clearCalls
	dw.mu.Unlock()
	if setCalls == 0 {
		t.Fatal("ожидался ≥1 SetDegraded (high rate в первые 50ms)")
	}
	if clearCalls == 0 {
		t.Fatal("ожидался ≥1 ClearDegraded после grace и устойчивого low rate")
	}
}

func TestLeader_BaselineZero_NoDegraded(t *testing.T) {
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 0 // пустой кластер
	ss.setCount(100)

	l, err := NewLeader(newTestLeaderCfg(), deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	dw.mu.Lock()
	calls := dw.setCalls
	dw.mu.Unlock()
	if calls != 0 {
		t.Fatalf("ожидался 0 SetDegraded при baseline=0, got %d", calls)
	}
}

func TestLeader_LeaseLost_StepDownAndReAcquire(t *testing.T) {
	acq, _, _, _, deps := newTestLeaderDeps()
	lostLease := &fakeLease{renewErr: ErrLeaseLost}
	acq.script = []acquireResult{
		{lease: lostLease},
		{err: ErrLeaseTaken},
		{lease: &fakeLease{}},
	}

	cfg := newTestLeaderCfg()
	cfg.LeaseTTL = 30 * time.Millisecond // быстрые renew-ы
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	if acq.callsCount() < 2 {
		t.Fatalf("ожидался ≥2 Acquire после ErrLeaseLost, got %d", acq.callsCount())
	}
	if !lostLease.released {
		t.Fatal("ErrLeaseLost-lease должен быть Release-нут на step-down")
	}
}

func TestLeader_SortedSetError_SkipsTick(t *testing.T) {
	acq, ss, dw, _, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	ss.countEr = errors.New("redis down")

	l, err := NewLeader(newTestLeaderCfg(), deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	dw.mu.Lock()
	setCalls := dw.setCalls
	clearCalls := dw.clearCalls
	dw.mu.Unlock()
	if setCalls != 0 || clearCalls != 0 {
		t.Fatalf("на ZCOUNT-ошибке тик должен пропускаться, got set=%d clear=%d", setCalls, clearCalls)
	}
}

// recordingNotifier — fake [Notifier], собирает все вызовы Notify.
type recordingNotifier struct {
	mu     sync.Mutex
	events []TollEvent
}

func (n *recordingNotifier) Notify(_ context.Context, ev TollEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, ev)
}

func (n *recordingNotifier) snapshot() []TollEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]TollEvent, len(n.events))
	copy(out, n.events)
	return out
}

func TestLeader_PerCovenTrigger_SetsDegradedWithCovenName(t *testing.T) {
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	// Global rate 5/100 = 0.05 < 0.20 → global не сработает.
	ss.setCount(5)
	// Per-coven: production-eu = 15/100 = 0.15 > 0.10 → должно сработать.
	ss.setCovenCounts(map[string]int64{
		"production-us": 3,
		"production-eu": 15,
	})

	notifier := &recordingNotifier{}
	cfg := newTestLeaderCfg()
	cfg.PerCovenThresholds = map[string]float64{
		"production-eu": 0.10,
		"production-us": 0.10,
	}
	cfg.Notifier = notifier

	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	dw.mu.Lock()
	setCalls := dw.setCalls
	dw.mu.Unlock()
	if setCalls == 0 {
		t.Fatalf("ожидался ≥1 SetDegraded из-за per-coven trigger")
	}

	events := notifier.snapshot()
	if len(events) == 0 {
		t.Fatalf("ожидался ≥1 Notify-вызов")
	}
	first := events[0]
	if first.Type != EventTypeDegradedSet {
		t.Fatalf("ожидался EventTypeDegradedSet, got %q", first.Type)
	}
	if first.CovenName != "production-eu" {
		t.Fatalf("ожидался coven_name=production-eu, got %q", first.CovenName)
	}
	if first.Threshold != 0.10 {
		t.Fatalf("ожидался threshold=0.10 (per-coven), got %v", first.Threshold)
	}
}

func TestLeader_PerCovenTrigger_GlobalTrumpsCoven(t *testing.T) {
	// При global-trigger-е coven_name должен быть пустым (cluster-wide отток).
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(30) // 30/100 = 0.30 > 0.20 (global сработает)
	ss.setCovenCounts(map[string]int64{
		"production-eu": 25, // 25/100 = 0.25 > 0.10 (per-coven тоже сработал)
	})

	notifier := &recordingNotifier{}
	cfg := newTestLeaderCfg()
	cfg.PerCovenThresholds = map[string]float64{"production-eu": 0.10}
	cfg.Notifier = notifier

	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	_ = dw
	events := notifier.snapshot()
	if len(events) == 0 {
		t.Fatalf("ожидался ≥1 Notify")
	}
	if events[0].CovenName != "" {
		t.Fatalf("при global-trigger-е coven_name должен быть пустым, got %q", events[0].CovenName)
	}
	if events[0].Threshold != 0.20 {
		t.Fatalf("при global-trigger-е threshold должен быть global, got %v", events[0].Threshold)
	}
}

func TestLeader_PerCovenTrigger_NoTrigger_NoNotify(t *testing.T) {
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(5)
	ss.setCovenCounts(map[string]int64{"production-eu": 5}) // 0.05 < 0.10

	notifier := &recordingNotifier{}
	cfg := newTestLeaderCfg()
	cfg.PerCovenThresholds = map[string]float64{"production-eu": 0.10}
	cfg.Notifier = notifier

	l, _ := NewLeader(cfg, deps)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	if len(notifier.snapshot()) != 0 {
		t.Fatalf("ожидался 0 Notify (rate ниже всех порогов)")
	}
	dw.mu.Lock()
	defer dw.mu.Unlock()
	if dw.setCalls != 0 {
		t.Fatalf("ожидался 0 SetDegraded, got %d", dw.setCalls)
	}
}

func TestLeader_Notifier_OnClearedAfterGrace(t *testing.T) {
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(25) // > threshold → degraded set

	notifier := &recordingNotifier{}
	cfg := newTestLeaderCfg()
	cfg.TickInterval = 20 * time.Millisecond
	cfg.ClearGrace = 80 * time.Millisecond
	cfg.Notifier = notifier
	l, _ := NewLeader(cfg, deps)

	go func() {
		time.Sleep(40 * time.Millisecond)
		ss.setCount(5)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	events := notifier.snapshot()
	if len(events) < 2 {
		t.Fatalf("ожидалось ≥2 Notify (set + cleared), got %d", len(events))
	}
	if events[0].Type != EventTypeDegradedSet {
		t.Fatalf("первый event должен быть set, got %q", events[0].Type)
	}
	gotCleared := false
	for _, e := range events {
		if e.Type == EventTypeDegradedCleared {
			gotCleared = true
			break
		}
	}
	if !gotCleared {
		t.Fatalf("ожидался cleared-event после grace, не нашлось в %+v", events)
	}
	_ = dw
}

func TestLeader_NotifierPanic_DoesNotCrashLoop(t *testing.T) {
	acq, ss, _, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(30)

	cfg := newTestLeaderCfg()
	cfg.Notifier = panickyNotifier{}

	l, _ := NewLeader(cfg, deps)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Не должен паниковать наружу: recover внутри l.notify.
	l.Run(ctx)
}

type panickyNotifier struct{}

func (panickyNotifier) Notify(context.Context, TollEvent) {
	panic("simulated webhook crash")
}

func TestLeader_CachedBaseline_AvoidsRefetch(t *testing.T) {
	// Direct test cachedBaseline без Leader-loop-а.
	bl := &fakeBaseline{value: 100}
	c := newCachedBaseline(bl, time.Second)
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 5; i++ {
		v, err := c.get(ctx, now)
		if err != nil || v != 100 {
			t.Fatalf("get #%d: v=%d err=%v", i, v, err)
		}
	}
	if got := bl.callsCount(); got != 1 {
		t.Fatalf("ожидался 1 fetch (cached), got %d", got)
	}
	// После TTL — refresh.
	v, err := c.get(ctx, now.Add(2*time.Second))
	if err != nil || v != 100 {
		t.Fatalf("post-TTL get: v=%d err=%v", v, err)
	}
	if got := bl.callsCount(); got != 2 {
		t.Fatalf("ожидался 2 fetch после TTL, got %d", got)
	}
}
