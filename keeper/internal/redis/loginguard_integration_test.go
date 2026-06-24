//go:build integration

// Integration-тест LoginGuard anti-bruteforce-примитива (ADR-058(g), HIGH-3) на
// реальном redis:7 через testcontainers-go. Реальный Redis обязателен (Lua
// TIME/INCR/PEXPIRE/SET PX): miniredis не эмулирует это корректно.
//
// Контейнер и integrationAddr поднимает общий TestMain (integration_test.go).
//
// Запуск:
//
//	cd keeper && TESTCONTAINERS_RYUK_DISABLED=true \
//	    SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/redis/...

package redis

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func newLoginGuardInt(t *testing.T) *LoginGuard {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := NewClient(ctx, Config{Addr: integrationAddr})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	g, err := NewLoginGuard(c)
	if err != nil {
		t.Fatalf("NewLoginGuard: %v", err)
	}
	return g
}

func uniquePrincipal(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("p-%s", t.Name())
}

// TestIntegration_LoginGuard_LockoutAfterThreshold — порог неудач достигается на
// threshold-й RecordFailure → Locked=true; до порога Locked=false.
func TestIntegration_LoginGuard_LockoutAfterThreshold(t *testing.T) {
	g := newLoginGuardInt(t)
	ctx := context.Background()
	principal := uniquePrincipal(t)
	const threshold = 3
	const window = time.Minute
	const lockout = time.Minute

	// До порога — не заблокирован.
	for i := 1; i < threshold; i++ {
		lockedNow, err := g.RecordFailure(ctx, "ip", principal, threshold, window, lockout)
		if err != nil {
			t.Fatalf("RecordFailure %d: %v", i, err)
		}
		if lockedNow {
			t.Fatalf("locked too early at failure %d (threshold %d)", i, threshold)
		}
		if locked, _, _ := g.Locked(ctx, "ip", principal); locked {
			t.Fatalf("Locked=true before threshold (failure %d)", i)
		}
	}

	// threshold-я неудача → блокировка.
	lockedNow, err := g.RecordFailure(ctx, "ip", principal, threshold, window, lockout)
	if err != nil {
		t.Fatalf("RecordFailure threshold: %v", err)
	}
	if !lockedNow {
		t.Fatalf("expected lockedNow=true at threshold %d", threshold)
	}
	locked, retryAfter, err := g.Locked(ctx, "ip", principal)
	if err != nil {
		t.Fatalf("Locked: %v", err)
	}
	if !locked {
		t.Fatalf("Locked=false after reaching threshold")
	}
	if retryAfter <= 0 || retryAfter > lockout {
		t.Errorf("retryAfter = %v, want (0, %v]", retryAfter, lockout)
	}
}

// TestIntegration_LoginGuard_NotLockedFresh — неизвестный принципал не заблокирован.
func TestIntegration_LoginGuard_NotLockedFresh(t *testing.T) {
	g := newLoginGuardInt(t)
	locked, _, err := g.Locked(context.Background(), "user", uniquePrincipal(t))
	if err != nil {
		t.Fatalf("Locked: %v", err)
	}
	if locked {
		t.Errorf("fresh principal must not be locked")
	}
}

// TestIntegration_LoginGuard_ThrottleBurst — burst попыток проходит, дальше — 429
// (allowed=false с retryAfter). Параллель TokenBucket, отдельный key-prefix.
func TestIntegration_LoginGuard_ThrottleBurst(t *testing.T) {
	g := newLoginGuardInt(t)
	ctx := context.Background()
	principal := uniquePrincipal(t)
	const rate = 1.0
	const burst = 4

	for i := 0; i < burst; i++ {
		allowed, _, err := g.Allow(ctx, "ip", principal, rate, burst)
		if err != nil {
			t.Fatalf("Allow %d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("attempt %d within burst must be allowed", i)
		}
	}
	allowed, retryAfter, err := g.Allow(ctx, "ip", principal, rate, burst)
	if err != nil {
		t.Fatalf("Allow over-burst: %v", err)
	}
	if allowed {
		t.Fatalf("attempt over burst must be throttled")
	}
	if retryAfter <= 0 {
		t.Errorf("throttled attempt must report positive retryAfter")
	}
}
