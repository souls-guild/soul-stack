//go:build integration

// Integration-guard-тесты Tempo (ADR-050, S-R4) НА УРОВНЕ middleware + реальный
// Redis: HTTP-запрос проходит через apimiddleware.RateLimit поверх живого
// *TokenBucket. Это e2e-срез (HTTP → middleware → Redis Lua-bucket), проверяющий
// поведенческие инварианты, которые primitive-тесты (ratelimit_integration_test.go)
// и unit-тесты middleware (api/middleware/ratelimit_test.go с fake-limiter-ом) по
// отдельности не закрывают:
//
//   - per-AID когерентность через ОБЩИЙ Redis (burst исчерпан → 429; разные AID
//     не делят bucket) — лимит авторитетен в Redis, не размножается (ADR-050(a));
//   - fail-OPEN при недоступном Redis (limiter.Allow вернул ошибку → passthrough,
//     НЕ 5xx, ADR-050(b));
//   - 429 несёт Retry-After (секунды, округление вверх) + application/problem+json
//     с type=tempo-exceeded (ADR-050(d)).
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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// tempoMWHandler собирает chi/net-http-цепочку «RateLimit(middleware) → next»
// поверх limiter-а с фиксированными rate/burst. next отвечает 202 (как реальный
// async-create Voyage). bucket уникален на тест (изоляция в общем Redis).
func tempoMWHandler(limiter apimiddleware.RateLimiter, bucket string, rate float64, burst int) (http.Handler, *int) {
	nextCalls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusAccepted)
	})
	limits := func() apimiddleware.RateLimitLimits {
		return apimiddleware.RateLimitLimits{Rate: rate, Burst: burst}
	}
	mw := apimiddleware.RateLimit(limiter, bucket, limits, nil, discardLogger())
	return mw(next), &nextCalls
}

// postAs формирует POST-запрос с claims (AID) в context, как после RequireJWT.
func postAs(aid string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/voyages", nil)
	ctx := apimiddleware.WithClaims(req.Context(), &jwt.Claims{Subject: aid})
	return req.WithContext(ctx)
}

// TestIntegration_TempoMW_PerAIDCoherence — через ОБЩИЙ Redis-bucket: исчерпание
// burst одним AID → 429; ДРУГОЙ AID не затронут (независимый bucket).
func TestIntegration_TempoMW_PerAIDCoherence(t *testing.T) {
	tb := newTokenBucketInt(t)
	const rate = 1.0 // медленный refill — в окне теста незаметен
	const burst = 3
	bucket := "voyage_create_" + t.Name()

	h, _ := tempoMWHandler(tb, bucket, rate, burst)

	aidA := uniqueAID(t) + "-a"
	aidB := uniqueAID(t) + "-b"

	// burst запросов AID-A проходят (202).
	for i := 0; i < burst; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, postAs(aidA))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("A #%d: ожидался 202 в пределах burst, got %d", i, rec.Code)
		}
	}

	// burst+1 для AID-A — бакет пуст → 429.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAs(aidA))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("A over burst: ожидался 429, got %d", rec.Code)
	}

	// AID-B — независимый bucket в общем Redis: первый запрос проходит.
	recB := httptest.NewRecorder()
	h.ServeHTTP(recB, postAs(aidB))
	if recB.Code != http.StatusAccepted {
		t.Fatalf("B first: ожидался 202 (другой AID не делит bucket), got %d", recB.Code)
	}
}

// TestIntegration_TempoMW_FailOpenOnRedisDown — Redis недоступен (клиент закрыт
// после конструирования limiter-а) → limiter.Allow возвращает ошибку →
// middleware fail-OPEN passthrough (202, next вызван), НЕ 5xx (ADR-050(b)).
func TestIntegration_TempoMW_FailOpenOnRedisDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tb, err := NewTokenBucket(c)
	if err != nil {
		t.Fatalf("NewTokenBucket: %v", err)
	}
	// Рвём соединение ПОСЛЕ конструирования: следующий Allow упрётся в closed
	// client → ошибка → fail-open. Деттерминированная имитация Redis-down.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	h, nextCalls := tempoMWHandler(tb, "voyage_create_"+t.Name(), 1.0, 1)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAs(uniqueAID(t)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Redis-down → fail-open: ожидался 202 passthrough, got %d (5xx = недопустим, ADR-050(b))", rec.Code)
	}
	if *nextCalls != 1 {
		t.Fatalf("fail-open → next должен вызваться ровно 1 раз, got %d", *nextCalls)
	}
}

// TestIntegration_TempoMW_429Shape — отклонённый запрос несёт Retry-After
// (целые секунды, минимум 1) + application/problem+json с type=tempo-exceeded
// и status 429 в теле (ADR-050(d)).
func TestIntegration_TempoMW_429Shape(t *testing.T) {
	tb := newTokenBucketInt(t)
	const rate = 1.0 // 1 rps → retryAfter до следующего токена ~1с
	const burst = 1
	h, _ := tempoMWHandler(tb, "voyage_create_"+t.Name(), rate, burst)

	aid := uniqueAID(t)

	// Сжигаем единственный токен.
	rec0 := httptest.NewRecorder()
	h.ServeHTTP(rec0, postAs(aid))
	if rec0.Code != http.StatusAccepted {
		t.Fatalf("drain: ожидался 202, got %d", rec0.Code)
	}

	// Второй запрос — 429 с полной формой.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAs(aid))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("ожидался 429, got %d", rec.Code)
	}

	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("отсутствует заголовок Retry-After")
	}
	secs, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("Retry-After должен быть целым числом секунд, got %q", ra)
	}
	if secs < 1 {
		t.Fatalf("Retry-After должен быть >= 1, got %d", secs)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}

	var body problem.Details
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode problem+json: %v", err)
	}
	if body.Type != problem.TypeTempoExceeded {
		t.Fatalf("problem.type = %q, want %q", body.Type, problem.TypeTempoExceeded)
	}
	if body.Status != http.StatusTooManyRequests {
		t.Fatalf("problem.status = %d, want 429", body.Status)
	}
}
