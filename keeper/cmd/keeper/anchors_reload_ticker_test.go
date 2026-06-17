package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// TestRunAnchorsReloadTicker_FiresPeriodically — тикер вызывает reload
// многократно по интервалу (TTL-fallback самоисцеления пропущенного
// `sigil:anchors-changed`, ADR-026(h) R3 known-gap). Коротким интервалом, без
// реальных Vault/PG/Outbound-deps (reload вынесен параметром).
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

	// Ждём минимум 3 тика, затем останавливаем.
	deadline := time.After(2 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("тикер не дал 3 вызова reload за 2s: got %d", calls.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine тикера не завершилась после cancel — shutdown leak")
	}
}

// TestRunAnchorsReloadTicker_SelfHealsMissedSignal — модель пропущенного pub/sub-
// сигнала: «сигнал» НЕ приходит (callback не вызывается извне), но TTL-тик сам
// перечитывает набор за интервал. Доказывает, что отставшая нода самоисцеляется
// без рестарта (ключевое свойство fix-а known-gap).
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

	// Никаких внешних сигналов не шлём — только тик. Должен сработать сам.
	select {
	case <-reloaded:
	case <-time.After(2 * time.Second):
		t.Fatal("пропущенный сигнал не самоисцелился TTL-тиком за окно")
	}
}

// TestRunAnchorsReloadTicker_ShutdownBeforeFirstTick — cancel до первого тика:
// goroutine выходит сразу, reload не вызывается.
func TestRunAnchorsReloadTicker_ShutdownBeforeFirstTick(t *testing.T) {
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // уже отменён

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
		t.Fatal("goroutine не вышла на уже отменённом ctx")
	}
	if calls.Load() != 0 {
		t.Errorf("reload вызван %d раз на отменённом ctx, want 0", calls.Load())
	}
}

// TestRunAnchorsReloadTicker_NonPositiveInterval — interval <= 0 не поднимает
// тик (guard от busy-loop); функция возвращается сразу, reload не вызывается.
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
		t.Fatal("interval=0: функция не вернулась сразу (guard не сработал)")
	}
	if calls.Load() != 0 {
		t.Errorf("interval=0: reload вызван %d раз, want 0", calls.Load())
	}
}

// TestSigilAnchorsReloadInterval_Default — пустое поле → дефолт 30s.
func TestSigilAnchorsReloadInterval_Default(t *testing.T) {
	cfg := &config.KeeperConfig{}
	if got := sigilAnchorsReloadInterval(cfg); got != config.DefaultSigilAnchorsReloadInterval {
		t.Errorf("пустое поле: interval = %v, want %v", got, config.DefaultSigilAnchorsReloadInterval)
	}
}

// TestSigilAnchorsReloadInterval_Explicit — заданное валидное значение берётся
// как есть; некорректное/непозитивное → дефолт (резолвер fail-safe).
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
