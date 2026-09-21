package toll

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestLeader_UpdateConfig_RejectsInvalid — UpdateConfig validates the same
// ranges as [NewLeader]: invalid newCfg → error without swap.
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
			t.Fatalf("case %d: expected error on invalid newCfg %+v", i, c)
		}
	}
}

// TestLeader_UpdateConfig_SwapsThresholds — raise threshold so the former
// rate ceases to be an "exceedance". Step-down: before UpdateConfig leader
// calls SetDegraded, after — should stop (new threshold above rate).
func TestLeader_UpdateConfig_SwapsThresholds(t *testing.T) {
	t.Parallel()
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(15) // 0.15 — above 0.10, below 0.50

	cfg := newTestLeaderCfg()
	cfg.Threshold = 0.10
	cfg.TickInterval = 20 * time.Millisecond
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	// Midway — raise threshold to 0.50; new ticks should not
	// continue SetDegraded (rate=0.15 < new threshold=0.50).
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
		t.Fatal("expected ≥1 SetDegraded before UpdateConfig (rate=0.15 > 0.10)")
	}
	// If UpdateConfig correctly raised threshold, transition to grace-phase
	// will occur; ClearDegraded after grace 50ms.
	dw.mu.Lock()
	clearCalls := dw.clearCalls
	dw.mu.Unlock()
	if clearCalls == 0 {
		t.Fatal("expected ≥1 ClearDegraded after UpdateConfig (new threshold above rate)")
	}
}

// TestLeader_UpdateConfig_PerCovenThresholdsUpdated — add a new
// per-coven threshold via UpdateConfig; on the next tick leader should
// trigger on it.
func TestLeader_UpdateConfig_PerCovenThresholdsUpdated(t *testing.T) {
	t.Parallel()
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	// Global rate 5/100=0.05 — below 0.20, won't trigger.
	ss.setCount(5)
	// Per-coven: production-eu above future threshold 0.10.
	ss.setCovenCounts(map[string]int64{"production-eu": 15})

	notifier := &recordingNotifier{}
	cfg := newTestLeaderCfg()
	cfg.TickInterval = 20 * time.Millisecond
	cfg.Notifier = notifier
	// Initially — without per-coven thresholds (no trigger).
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
		t.Fatalf("expected ≥1 SetDegraded after adding per-coven threshold via UpdateConfig")
	}
	events := notifier.snapshot()
	if len(events) == 0 {
		t.Fatal("expected ≥1 Notify after UpdateConfig")
	}
	if events[0].CovenName != "production-eu" {
		t.Fatalf("expected coven_name=production-eu, got %q", events[0].CovenName)
	}
}

// TestLeader_UpdateConfig_NotifierRecycled — swap Notifier via
// UpdateConfig; next trigger should go to the new one, not the old.
func TestLeader_UpdateConfig_NotifierRecycled(t *testing.T) {
	t.Parallel()
	acq, ss, _, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(30) // > threshold, constant trigger

	oldNotifier := &recordingNotifier{}
	newNotifier := &recordingNotifier{}
	cfg := newTestLeaderCfg()
	cfg.TickInterval = 20 * time.Millisecond
	cfg.Notifier = oldNotifier
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	// Let first tick reach oldNotifier; then swap (degraded_set
	// already set — subsequent ticks don't call Notify until cleared).
	// So we check for cleared-event: after rate reset, cleared
	// should arrive at newNotifier.
	go func() {
		time.Sleep(40 * time.Millisecond)
		// First let the first trigger reach oldNotifier (it will be in snapshot).
		newCfg := cfg
		newCfg.Notifier = newNotifier
		if err := l.UpdateConfig(newCfg); err != nil {
			t.Errorf("UpdateConfig: %v", err)
		}
		// Then drop rate below threshold — cleared will arrive at newNotifier.
		time.Sleep(20 * time.Millisecond)
		ss.setCount(5)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	// ClearGrace in leader config already from newTestLeaderCfg (50ms), sufficient.
	l.Run(ctx)

	oldEvents := oldNotifier.snapshot()
	newEvents := newNotifier.snapshot()

	// Old should have received at least degraded_set (first tick before swap).
	gotSetOnOld := false
	for _, e := range oldEvents {
		if e.Type == EventTypeDegradedSet {
			gotSetOnOld = true
		}
	}
	if !gotSetOnOld {
		t.Fatalf("expected degraded_set on oldNotifier (before swap), got %+v", oldEvents)
	}
	// New should have received degraded_cleared (after swap and rate drop).
	gotClearedOnNew := false
	for _, e := range newEvents {
		if e.Type == EventTypeDegradedCleared {
			gotClearedOnNew = true
		}
	}
	if !gotClearedOnNew {
		t.Fatalf("expected degraded_cleared on newNotifier (after UpdateConfig), got %+v", newEvents)
	}
	// And on old, cleared should NOT arrive (swap happened before drop).
	for _, e := range oldEvents {
		if e.Type == EventTypeDegradedCleared {
			t.Fatalf("cleared should not have arrived on oldNotifier after swap: %+v", oldEvents)
		}
	}
}

// TestLeader_UpdateConfig_DisableNotifier — nil-notifier in UpdateConfig
// disables alert channel; cleared-event after grace should not propagate.
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
		// Swap notifier to nil after first tick.
		time.Sleep(40 * time.Millisecond)
		newCfg := cfg
		newCfg.Notifier = nil
		if err := l.UpdateConfig(newCfg); err != nil {
			t.Errorf("UpdateConfig: %v", err)
		}
		// Drop rate — cleared-flow should start, but notifier=nil → nothing
		// is recorded.
		time.Sleep(20 * time.Millisecond)
		ss.setCount(5)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	l.Run(ctx)

	events := notifier.snapshot()
	// Only set, no cleared (cleared would arrive if notifier remained).
	for _, e := range events {
		if e.Type == EventTypeDegradedCleared {
			t.Fatalf("expected 0 cleared-event after nil-Notifier, got %+v", events)
		}
	}
}

// TestLeader_UpdateConfig_ConcurrentWithTick — race-detector guarantee: tick
// and UpdateConfig run concurrently, read/write cfg fields.
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

// TestLeader_UpdateConfig_BeforeRun — UpdateConfig works before
// Run starts (start-up-time apply): values should be picked up by first tick.
func TestLeader_UpdateConfig_BeforeRun(t *testing.T) {
	t.Parallel()
	acq, ss, dw, bl, deps := newTestLeaderDeps()
	acq.script = []acquireResult{{lease: &fakeLease{}}}
	bl.value = 100
	ss.setCount(15) // 0.15

	cfg := newTestLeaderCfg()
	cfg.Threshold = 0.50 // initially rate below
	l, err := NewLeader(cfg, deps)
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	// Before Run, drop threshold to 0.10 → 0.15 > 0.10 → should trigger.
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
		t.Fatal("expected ≥1 SetDegraded — UpdateConfig before Run should apply to first tick")
	}
}

// TestLeader_CurrentNotifier_ReturnsActive — sanity-check helper for
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
		t.Fatal("expected nil CurrentNotifier when notifier absent in cfg")
	}
	// After UpdateConfig with nil-notifier — nil again.
	if err := l.UpdateConfig(LeaderConfig{
		WindowSize: cfg.WindowSize, Threshold: cfg.Threshold,
		DegradedTTL: cfg.DegradedTTL, ClearGrace: cfg.ClearGrace,
		Notifier: nil,
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if l.CurrentNotifier() != nil {
		t.Fatal("expected nil CurrentNotifier after UpdateConfig with nil-notifier")
	}
}
