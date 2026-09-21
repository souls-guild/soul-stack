package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// TestRunAnchorsReloadTicker_FiresPeriodically -- the ticker calls reload
// repeatedly at the interval (TTL-fallback self-healing for a missed
// `sigil:anchors-changed`, ADR-026(h) R3 known-gap). Short interval, no
// real Vault/PG/Outbound-deps (reload is passed as a parameter).
func TestRunAnchorsReloadTicker_FiresPeriodically(t *testing.T) {
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runAnchorsReloadTicker(ctx, time.Millisecond, func(context.Context) {
			calls.Add(1)
		})
	}()

	// Wait for at least 3 ticks, then stop.
	deadline := time.After(2 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("ticker did not fire 3 reload calls in 2s: got %d", calls.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ticker goroutine did not finish after cancel -- shutdown leak")
	}
}

// TestRunAnchorsReloadTicker_SelfHealsMissedSignal -- models a missed pub/sub
// signal: the "signal" never arrives (callback not invoked externally), but
// the TTL tick re-reads the set on its own at the interval. Proves that a
// lagging node self-heals without a restart (the key property of the
// known-gap fix).
func TestRunAnchorsReloadTicker_SelfHealsMissedSignal(t *testing.T) {
	reloaded := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runAnchorsReloadTicker(ctx, 10*time.Millisecond, func(context.Context) {
		select {
		case reloaded <- struct{}{}:
		default:
		}
	})

	// No external signals sent -- only the tick. Must fire on its own.
	select {
	case <-reloaded:
	case <-time.After(2 * time.Second):
		t.Fatal("missed signal did not self-heal via TTL tick within the window")
	}
}

// TestRunAnchorsReloadTicker_ShutdownBeforeFirstTick -- cancel before the
// first tick: goroutine exits immediately, reload is never called.
func TestRunAnchorsReloadTicker_ShutdownBeforeFirstTick(t *testing.T) {
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	done := make(chan struct{})
	go func() {
		defer close(done)
		runAnchorsReloadTicker(ctx, time.Hour, func(context.Context) {
			calls.Add(1)
		})
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutine did not exit on an already-cancelled ctx")
	}
	if calls.Load() != 0 {
		t.Errorf("reload called %d times on cancelled ctx, want 0", calls.Load())
	}
}

// TestRunAnchorsReloadTicker_NonPositiveInterval -- interval <= 0 never starts
// a ticker (busy-loop guard); the function returns immediately, reload is
// never called.
func TestRunAnchorsReloadTicker_NonPositiveInterval(t *testing.T) {
	var calls atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAnchorsReloadTicker(context.Background(), 0, func(context.Context) {
			calls.Add(1)
		})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("interval=0: function did not return immediately (guard did not fire)")
	}
	if calls.Load() != 0 {
		t.Errorf("interval=0: reload called %d times, want 0", calls.Load())
	}
}

// TestSigilAnchorsReloadInterval_Default -- empty field -> default 30s.
func TestSigilAnchorsReloadInterval_Default(t *testing.T) {
	cfg := &config.KeeperConfig{}
	if got := sigilAnchorsReloadInterval(cfg); got != config.DefaultSigilAnchorsReloadInterval {
		t.Errorf("empty field: interval = %v, want %v", got, config.DefaultSigilAnchorsReloadInterval)
	}
}

// TestSigilAnchorsReloadInterval_Explicit -- a valid explicit value is taken
// as-is; invalid/non-positive -> default (fail-safe resolver).
func TestSigilAnchorsReloadInterval_Explicit(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"valid", "5s", 5 * time.Second},
		{"days-convention", "1d", 24 * time.Hour},
		{"garbage", "nonsense", config.DefaultSigilAnchorsReloadInterval},
		{"zero", "0s", config.DefaultSigilAnchorsReloadInterval},
		{"negative", "-1s", config.DefaultSigilAnchorsReloadInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.KeeperConfig{SigilAnchorsReloadInterval: tc.raw}
			if got := sigilAnchorsReloadInterval(cfg); got != tc.want {
				t.Errorf("interval(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
