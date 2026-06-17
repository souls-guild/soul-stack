package toll

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestLeader_UpdateConfig_RejectsInvalid — UpdateConfig валидирует те же
// диапазоны, что [NewLeader]: невалидный newCfg → error БЕЗ swap-а.
func TestLeader_UpdateConfig_RejectsInvalid(t *testing.T) {
	t.Parallel()
	_, _, _, _, deps := newTestLeaderDeps()
	l, err := NewLeader(newTestLeaderCfg(), deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	cases := []LeaderConfig{
		{WindowSize: 0, Threshold: 0.2, DegradedTTL: time.Second, ClearGrace: time.Second},
		{WindowSize: time.Second, Threshold: 0, DegradedTTL: time.Second, ClearGrace: time.Second},
		{WindowSize: time.Second, Threshold: 2, DegradedTTL: time.Second, ClearGrace: time.Second},
		{WindowSize: time.Second, Threshold: 0.2, DegradedTTL: 0, ClearGrace: time.Second},
		{WindowSize: time.Second, Threshold: 0.2, DegradedTTL: time.Second, ClearGrace: 0},
	}
	for i, c := range cases {
		if err := l.UpdateConfig(c); err == nil {
			t.Fatalf("case %d: ожидался error на невалидном newCfg %+v", i, c)
		}
	}
}

// TestLeader_UpdateConfig_SwapsThresholds — повышаем threshold так, чтобы
// прежний rate перестал быть «превышением». Степ-down: до UpdateConfig leader
// SetDegraded зовёт, после — должен прекратить (новый порог выше rate-а).
func TestLeader_UpdateConfig_SwapsThresholds(t *testing.T) {
	t.Parallel()
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(15) // 0.15 — выше 0.10, ниже 0.50

	cfg := newTestLeaderCfg()
	cfg.Threshold = 0.10
	cfg.TickInterval = 20 * time.Millisecond
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	// На середине пути — поднимаем порог до 0.50; новые тики не должны
	// продолжать SetDegraded (rate=0.15 < новый threshold=0.50).
	go func() {
		time.Sleep(50 * time.Millisecond)
		newCfg := cfg
		newCfg.Threshold = 0.50
		if err := l.UpdateConfig(newCfg); err != nil {
			t.Errorf("UpdateConfig: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	dw.mu.Lock()
	setCallsBefore := dw.setCalls
	dw.mu.Unlock()
	if setCallsBefore == 0 {
		t.Fatal("ожидался ≥1 SetDegraded до UpdateConfig (rate=0.15 > 0.10)")
	}
	// Если UpdateConfig корректно поднял threshold, переход в grace-фазу
	// произойдёт; ClearDegraded после grace 50ms.
	dw.mu.Lock()
	clearCalls := dw.clearCalls
	dw.mu.Unlock()
	if clearCalls == 0 {
		t.Fatal("ожидался ≥1 ClearDegraded после UpdateConfig (новый threshold выше rate-а)")
	}
}

// TestLeader_UpdateConfig_PerCovenThresholdsUpdated — добавляем новый
// per-coven threshold через UpdateConfig; на следующем тике leader должен
// триггерить по нему.
func TestLeader_UpdateConfig_PerCovenThresholdsUpdated(t *testing.T) {
	t.Parallel()
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	// Global rate 5/100=0.05 — ниже 0.20, не сработает.
	ss.setCount(5)
	// Per-coven: production-eu выше будущего порога 0.10.
	ss.setCovenCounts(map[string]int64{"production-eu": 15})

	notifier := &recordingNotifier{}
	cfg := newTestLeaderCfg()
	cfg.TickInterval = 20 * time.Millisecond
	cfg.Notifier = notifier
	// Сразу — без per-coven thresholds (триггера не будет).
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	go func() {
		time.Sleep(40 * time.Millisecond)
		newCfg := cfg
		newCfg.PerCovenThresholds = map[string]float64{"production-eu": 0.10}
		if err := l.UpdateConfig(newCfg); err != nil {
			t.Errorf("UpdateConfig: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	dw.mu.Lock()
	setCalls := dw.setCalls
	dw.mu.Unlock()
	if setCalls == 0 {
		t.Fatalf("ожидался ≥1 SetDegraded после добавления per-coven threshold через UpdateConfig")
	}
	events := notifier.snapshot()
	if len(events) == 0 {
		t.Fatal("ожидался ≥1 Notify после UpdateConfig")
	}
	if events[0].CovenName != "production-eu" {
		t.Fatalf("ожидался coven_name=production-eu, got %q", events[0].CovenName)
	}
}

// TestLeader_UpdateConfig_NotifierRecycled — подменяем Notifier через
// UpdateConfig; следующий trigger должен идти к новому, не к старому.
func TestLeader_UpdateConfig_NotifierRecycled(t *testing.T) {
	t.Parallel()
	acq, ss, _, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(30) // > threshold, постоянный trigger

	oldNotifier := &recordingNotifier{}
	newNotifier := &recordingNotifier{}
	cfg := newTestLeaderCfg()
	cfg.TickInterval = 20 * time.Millisecond
	cfg.Notifier = oldNotifier
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	// Дадим первому тику дойти до oldNotifier; затем подменим (degraded_set
	// уже выставлен — повторные tick-и Notify не зовут до cleared).
	// Поэтому проверяем на cleared-event: после reset-а rate-а cleared
	// должен прилететь в newNotifier.
	go func() {
		time.Sleep(40 * time.Millisecond)
		// Сначала дать первый trigger на oldNotifier (он будет в snapshot-е).
		newCfg := cfg
		newCfg.Notifier = newNotifier
		if err := l.UpdateConfig(newCfg); err != nil {
			t.Errorf("UpdateConfig: %v", err)
		}
		// Затем опускаем rate ниже threshold — cleared прилетит к newNotifier.
		time.Sleep(20 * time.Millisecond)
		ss.setCount(5)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	// ClearGrace в leader-конфиге уже из newTestLeaderCfg (50ms), хватит.
	l.Run(ctx)

	oldEvents := oldNotifier.snapshot()
	newEvents := newNotifier.snapshot()

	// Старый должен был получить хотя бы degraded_set (первый tick до swap-а).
	gotSetOnOld := false
	for _, e := range oldEvents {
		if e.Type == EventTypeDegradedSet {
			gotSetOnOld = true
		}
	}
	if !gotSetOnOld {
		t.Fatalf("ожидался degraded_set на oldNotifier (до swap-а), got %+v", oldEvents)
	}
	// Новый должен получить degraded_cleared (после swap-а и drop rate-а).
	gotClearedOnNew := false
	for _, e := range newEvents {
		if e.Type == EventTypeDegradedCleared {
			gotClearedOnNew = true
		}
	}
	if !gotClearedOnNew {
		t.Fatalf("ожидался degraded_cleared на newNotifier (после UpdateConfig), got %+v", newEvents)
	}
	// И на старом cleared НЕ должен прилететь (swap произошёл до drop-а).
	for _, e := range oldEvents {
		if e.Type == EventTypeDegradedCleared {
			t.Fatalf("на oldNotifier не должен был прилететь cleared после swap-а: %+v", oldEvents)
		}
	}
}

// TestLeader_UpdateConfig_DisableNotifier — nil-notifier в UpdateConfig
// отключает alert-канал; cleared-event через grace не должен пропагироваться.
func TestLeader_UpdateConfig_DisableNotifier(t *testing.T) {
	t.Parallel()
	acq, ss, _, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(30)

	notifier := &recordingNotifier{}
	cfg := newTestLeaderCfg()
	cfg.TickInterval = 20 * time.Millisecond
	cfg.ClearGrace = 50 * time.Millisecond
	cfg.Notifier = notifier
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	go func() {
		// Подменяем notifier на nil после первого тика.
		time.Sleep(40 * time.Millisecond)
		newCfg := cfg
		newCfg.Notifier = nil
		if err := l.UpdateConfig(newCfg); err != nil {
			t.Errorf("UpdateConfig: %v", err)
		}
		// Drop rate — должен пойти cleared-flow, но notifier=nil → ничего
		// не записывается.
		time.Sleep(20 * time.Millisecond)
		ss.setCount(5)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	events := notifier.snapshot()
	// Только set, без cleared (cleared прилетел бы, если бы notifier остался).
	for _, e := range events {
		if e.Type == EventTypeDegradedCleared {
			t.Fatalf("ожидался 0 cleared-event после nil-Notifier, got %+v", events)
		}
	}
}

// TestLeader_UpdateConfig_ConcurrentWithTick — race-detector гарантия: tick
// и UpdateConfig запускаются параллельно, читают/пишут cfg-поля.
func TestLeader_UpdateConfig_ConcurrentWithTick(t *testing.T) {
	t.Parallel()
	acq, ss, _, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(10)

	cfg := newTestLeaderCfg()
	cfg.TickInterval = 5 * time.Millisecond
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				newCfg := cfg
				newCfg.Threshold = 0.10 + 0.01*float64(time.Now().UnixNano()%10)
				newCfg.PerCovenThresholds = map[string]float64{
					"a": 0.10,
					"b": 0.20,
				}
				if err := l.UpdateConfig(newCfg); err != nil {
					t.Errorf("UpdateConfig: %v", err)
					return
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	l.Run(ctx)
	close(stop)
	wg.Wait()
}

// TestLeader_UpdateConfig_BeforeRun — UpdateConfig работает и до запуска
// Run (start-up-time apply): значения должны быть подхвачены первым тиком.
func TestLeader_UpdateConfig_BeforeRun(t *testing.T) {
	t.Parallel()
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(15) // 0.15

	cfg := newTestLeaderCfg()
	cfg.Threshold = 0.50 // изначально rate ниже
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	// До Run опускаем порог до 0.10 → 0.15 > 0.10 → должен сработать.
	newCfg := cfg
	newCfg.Threshold = 0.10
	if err := l.UpdateConfig(newCfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	dw.mu.Lock()
	setCalls := dw.setCalls
	dw.mu.Unlock()
	if setCalls == 0 {
		t.Fatal("ожидался ≥1 SetDegraded — UpdateConfig до Run должен примениться к первому тику")
	}
}

// TestLeader_CurrentNotifier_ReturnsActive — sanity-check helper-а для
// daemon-applyTollReload.
func TestLeader_CurrentNotifier_ReturnsActive(t *testing.T) {
	t.Parallel()
	_, _, _, _, deps := newTestLeaderDeps()
	cfg := newTestLeaderCfg()
	n := &recordingNotifier{}
	cfg.Notifier = n
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	got := l.CurrentNotifier()
	if got != Notifier(n) {
		t.Fatalf("CurrentNotifier returned %v, want %v", got, n)
	}
	// nil-cfg path.
	cfg2 := newTestLeaderCfg()
	l2, err := NewLeader(cfg2, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}
	if l2.CurrentNotifier() != nil {
		t.Fatal("ожидался nil CurrentNotifier при отсутствии notifier-а в cfg")
	}
	// После UpdateConfig nil-notifier — снова nil.
	if err := l.UpdateConfig(LeaderConfig{
		WindowSize: cfg.WindowSize, Threshold: cfg.Threshold,
		DegradedTTL: cfg.DegradedTTL, ClearGrace: cfg.ClearGrace,
		Notifier: nil,
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if l.CurrentNotifier() != nil {
		t.Fatal("ожидался nil CurrentNotifier после UpdateConfig с nil-notifier")
	}
}
