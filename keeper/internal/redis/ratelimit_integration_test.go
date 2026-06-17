//go:build integration

// Integration-тест Tempo token-bucket (ADR-050) на реальном redis:7 через
// testcontainers-go. Реальный Redis обязателен (не miniredis): примитив
// читает время через `redis.call("TIME")`, а miniredis-Lua не эмулирует
// рефилл-во-времени корректно — тесты refill/atomicity потеряли бы смысл.
//
// Контейнер и `integrationAddr` поднимает общий TestMain (integration_test.go).
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

func newTokenBucketInt(t *testing.T) *TokenBucket {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	tb, err := NewTokenBucket(c)
	if err != nil {
		t.Fatalf("NewTokenBucket: %v", err)
	}
	return tb
}

// uniqueAID — уникальный AID на тест, чтобы бакеты тестов не пересекались
// в общем Redis-контейнере.
func uniqueAID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("archon-test-%s", t.Name())
}

// TestIntegration_TokenBucket_BurstThenSustained — burst пропускается целиком,
// дальше (без рефилла) запросы режутся с положительным retryAfter.
func TestIntegration_TokenBucket_BurstThenSustained(t *testing.T) {
	tb := newTokenBucketInt(t)
	ctx := context.Background()

	const rate = 1.0 // 1 токен/сек — рефилл медленный, в окне теста незаметен
	const burst = 5

	aid := uniqueAID(t)

	// Первые burst запросов проходят.
	for i := 0; i < burst; i++ {
		allowed, retry, err := tb.Allow(ctx, aid, "voyage_create", rate, burst)
		if err != nil {
			t.Fatalf("Allow #%d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("Allow #%d: ожидался allow в пределах burst, got deny (retry=%v)", i, retry)
		}
		if retry != 0 {
			t.Fatalf("Allow #%d: на allow retryAfter должен быть 0, got %v", i, retry)
		}
	}

	// burst+1 — бакет пуст, deny + положительный retryAfter.
	allowed, retry, err := tb.Allow(ctx, aid, "voyage_create", rate, burst)
	if err != nil {
		t.Fatalf("Allow over burst: %v", err)
	}
	if allowed {
		t.Fatal("Allow over burst: ожидался deny, got allow")
	}
	if retry <= 0 {
		t.Fatalf("Allow over burst: retryAfter должен быть > 0, got %v", retry)
	}
	// При rate=1 до следующего токена ~1с; допускаем небольшой разброс.
	if retry > 2*time.Second {
		t.Fatalf("Allow over burst: retryAfter неожиданно велик: %v", retry)
	}
}

// TestIntegration_TokenBucket_RefillOverTime — после исчерпания бакет
// восстанавливается во времени: подождав, снова получаем allow.
func TestIntegration_TokenBucket_RefillOverTime(t *testing.T) {
	tb := newTokenBucketInt(t)
	ctx := context.Background()

	const rate = 10.0 // 10 токенов/сек → 1 токен за 100ms
	const burst = 2

	aid := uniqueAID(t)

	// Сжигаем бакет.
	for i := 0; i < burst; i++ {
		if allowed, _, err := tb.Allow(ctx, aid, "voyage_create", rate, burst); err != nil || !allowed {
			t.Fatalf("Allow drain #%d: allowed=%v err=%v", i, allowed, err)
		}
	}
	if allowed, _, err := tb.Allow(ctx, aid, "voyage_create", rate, burst); err != nil || allowed {
		t.Fatalf("Allow after drain: ожидался deny, allowed=%v err=%v", allowed, err)
	}

	// Ждём заведомо больше одного интервала рефилла (1 токен = 100ms при rate=10).
	time.Sleep(300 * time.Millisecond)

	allowed, retry, err := tb.Allow(ctx, aid, "voyage_create", rate, burst)
	if err != nil {
		t.Fatalf("Allow after refill: %v", err)
	}
	if !allowed {
		t.Fatalf("Allow after refill: ожидался allow (бакет пополнился), got deny (retry=%v)", retry)
	}
}

// TestIntegration_TokenBucket_IsolationByKey — разные AID и разные bucket-имена
// не делят один бакет.
func TestIntegration_TokenBucket_IsolationByKey(t *testing.T) {
	tb := newTokenBucketInt(t)
	ctx := context.Background()

	const rate = 1.0
	const burst = 2

	aidA := uniqueAID(t) + "-a"
	aidB := uniqueAID(t) + "-b"

	// Сжигаем бакет AID-A полностью.
	for i := 0; i < burst; i++ {
		if allowed, _, err := tb.Allow(ctx, aidA, "voyage_create", rate, burst); err != nil || !allowed {
			t.Fatalf("drain A #%d: allowed=%v err=%v", i, allowed, err)
		}
	}
	if allowed, _, err := tb.Allow(ctx, aidA, "voyage_create", rate, burst); err != nil || allowed {
		t.Fatalf("A over burst: ожидался deny, allowed=%v err=%v", allowed, err)
	}

	// AID-B — независимый бакет, первый запрос проходит.
	if allowed, _, err := tb.Allow(ctx, aidB, "voyage_create", rate, burst); err != nil || !allowed {
		t.Fatalf("B first: ожидался allow (другой AID), allowed=%v err=%v", allowed, err)
	}

	// Тот же AID-A, но другой bucket — тоже независим.
	if allowed, _, err := tb.Allow(ctx, aidA, "voyage_preview", rate, burst); err != nil || !allowed {
		t.Fatalf("A other-bucket: ожидался allow (другой bucket), allowed=%v err=%v", allowed, err)
	}
}

// TestIntegration_TokenBucket_CreateVsPreviewSeparate — ИНВАРИАНТ ADR-050
// amendment 2026-06-17 через РЕАЛЬНЫЙ Redis: voyage_create и voyage_preview —
// разные ключи `tempo:<aid>:<bucket>` для одного AID, квоту НЕ делят.
// Исчерпание create НЕ влияет на preview, и симметрично.
func TestIntegration_TokenBucket_CreateVsPreviewSeparate(t *testing.T) {
	tb := newTokenBucketInt(t)
	ctx := context.Background()

	const rate = 1.0 // медленный refill — в окне теста незаметен
	const burst = 1
	aid := uniqueAID(t)

	// Сжигаем create-бакет (burst=1): первый allow, второй deny.
	if allowed, _, err := tb.Allow(ctx, aid, "voyage_create", rate, burst); err != nil || !allowed {
		t.Fatalf("create #1: ожидался allow, allowed=%v err=%v", allowed, err)
	}
	if allowed, _, err := tb.Allow(ctx, aid, "voyage_create", rate, burst); err != nil || allowed {
		t.Fatalf("create #2: ожидался deny (create исчерпан), allowed=%v err=%v", allowed, err)
	}

	// preview ТОГО ЖЕ AID нетронут исчерпанием create → проходит.
	if allowed, _, err := tb.Allow(ctx, aid, "voyage_preview", rate, burst); err != nil || !allowed {
		t.Fatalf("preview #1: ожидался allow — preview не делит квоту с create, allowed=%v err=%v", allowed, err)
	}
	// preview исчерпан собственным burst → deny.
	if allowed, _, err := tb.Allow(ctx, aid, "voyage_preview", rate, burst); err != nil || allowed {
		t.Fatalf("preview #2: ожидался deny (собственный preview-бакет исчерпан), allowed=%v err=%v", allowed, err)
	}

	// Симметрия на свежем AID: исчерпание preview не трогает create.
	aid2 := uniqueAID(t) + "-sym"
	if allowed, _, err := tb.Allow(ctx, aid2, "voyage_preview", rate, burst); err != nil || !allowed {
		t.Fatalf("sym preview #1: ожидался allow, allowed=%v err=%v", allowed, err)
	}
	if allowed, _, err := tb.Allow(ctx, aid2, "voyage_preview", rate, burst); err != nil || allowed {
		t.Fatalf("sym preview #2: ожидался deny, allowed=%v err=%v", allowed, err)
	}
	if allowed, _, err := tb.Allow(ctx, aid2, "voyage_create", rate, burst); err != nil || !allowed {
		t.Fatalf("sym create #1: ожидался allow — create не делит квоту с preview, allowed=%v err=%v", allowed, err)
	}
}

// TestIntegration_TokenBucket_RejectsInvalidArgs — невалидные аргументы
// отвергаются ошибкой, не молча.
func TestIntegration_TokenBucket_RejectsInvalidArgs(t *testing.T) {
	tb := newTokenBucketInt(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		aid    string
		bucket string
		rate   float64
		burst  int
	}{
		{"empty_aid", "", "voyage_create", 1, 1},
		{"empty_bucket", "archon-x", "", 1, 1},
		{"zero_rate", "archon-x", "voyage_create", 0, 1},
		{"negative_rate", "archon-x", "voyage_create", -1, 1},
		{"zero_burst", "archon-x", "voyage_create", 1, 0},
		{"negative_burst", "archon-x", "voyage_create", 1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := tb.Allow(ctx, tc.aid, tc.bucket, tc.rate, tc.burst); err == nil {
				t.Errorf("Allow(%+v) вернул nil err; ожидалась валидационная ошибка", tc)
			}
		})
	}
}
