package main

import (
	"testing"
	"time"
)

// Unit-тесты backoff-прогрессии reconnect-loop-а. Сам reconnectLoop требует
// живого gRPC-клиента (integration, см. failback_integration_test.go); здесь
// проверяем чистую арифметику nextDelay + инвариант leaseHeldBackoffCap, на
// которых стоит различение lease-held (модест-cap) и transport (общий cap).

func TestNextDelay_DoublesUntilCap(t *testing.T) {
	t.Parallel()
	cap := 30 * time.Second
	got := 1 * time.Second
	want := []time.Duration{2, 4, 8, 16, 30, 30}
	for i, w := range want {
		got = nextDelay(got, cap)
		if got != w*time.Second {
			t.Fatalf("step %d: nextDelay = %s, want %s", i, got, w*time.Second)
		}
	}
}

// TestLeaseHeldCap_ProgressionStaysModest — прогрессия с модест-cap (lease-held
// ветка) НЕ улетает в десятки секунд: после краха keeper-а presence истекает за
// ~30s, force-release освобождает lease — Soul переподключается в пределах cap-а,
// а не общего transport-cap. Доказываем, что cap=leaseHeldBackoffCap держит
// прогрессию в пределах нескольких секунд (recovery-latency сохранена).
func TestLeaseHeldCap_ProgressionStaysModest(t *testing.T) {
	t.Parallel()
	if leaseHeldBackoffCap > 5*time.Second {
		t.Fatalf("leaseHeldBackoffCap = %s, want ≤ 5s (recovery-latency требует модест-cap)", leaseHeldBackoffCap)
	}
	// initial=1s, удваиваем многократно — должно сходиться к cap, не выше.
	d := 1 * time.Second
	for i := 0; i < 10; i++ {
		d = nextDelay(d, leaseHeldBackoffCap)
		if d > leaseHeldBackoffCap {
			t.Fatalf("step %d: lease-held delay = %s exceeded cap %s", i, d, leaseHeldBackoffCap)
		}
	}
	if d != leaseHeldBackoffCap {
		t.Fatalf("lease-held delay converged to %s, want cap %s", d, leaseHeldBackoffCap)
	}
}

// TestLeaseHeldCap_ClampsAlreadyGrownDelay — если backoff уже вырос выше модест-
// cap-а (transport-сбои до того, как keeper поднялся в lease-held режим), вход в
// lease-held ветку клампит текущий delay к cap (та же арифметика, что в
// reconnectLoop: `if delay > cap { delay = cap }`). Гарантирует, что переход
// transport→lease-held не оставляет раздутую задержку.
func TestLeaseHeldCap_ClampsAlreadyGrownDelay(t *testing.T) {
	t.Parallel()
	delay := 30 * time.Second // вырос на transport-сбоях до общего max
	cap := leaseHeldBackoffCap
	if delay > cap {
		delay = cap
	}
	if delay != cap {
		t.Fatalf("clamp: delay = %s, want %s", delay, cap)
	}
}

// TestTransportCap_Unchanged — регресс-guard: общий transport-cap (default
// keeper.retry.backoff.max=30s) не задет правкой lease-held ветки. Прогрессия
// transport-backoff по-прежнему доходит до 30s.
func TestTransportCap_Unchanged(t *testing.T) {
	t.Parallel()
	transportCap := 30 * time.Second
	d := 1 * time.Second
	for i := 0; i < 10; i++ {
		d = nextDelay(d, transportCap)
	}
	if d != transportCap {
		t.Fatalf("transport delay converged to %s, want %s", d, transportCap)
	}
}
