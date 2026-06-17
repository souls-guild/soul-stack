package middleware

// Tempo rate-limit middleware (ADR-050) — per-AID ограничитель частоты
// обращений оператора к resolver-тяжёлым write-эндпоинтам.
//
// Навешивается ПОСЛЕ [RequireJWT]: AID берётся из claims в context
// ([ClaimsFromContext], `claims.Subject`). До handler-а (до запуска
// резолверов) — берётся один токен из per-AID-bucket-а в Redis; при
// исчерпании — 429 + Retry-After + application/problem+json
// ([problem.TypeTempoExceeded]), без вызова next.
//
// Образец — toll.DegradedMiddleware (keeper/internal/toll/middleware.go):
// та же форма фабрики, тот же nil → passthrough, тот же fail-OPEN при
// Redis-ошибке. Отличие: Tempo — per-AID 429 по частоте, Toll — cluster-wide
// 503 по здоровью. Навеска (точечно на `POST /v1/voyages`) и конфиг
// (`tempo:`-блок, rate/burst, hot-reload) — wire-up в keeper/cmd/keeper +
// keeper/internal/api (S-R3/S-R4).

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// RateLimiter — поверхность Tempo-token-bucket-а, нужная middleware.
// Реализуется *redis.TokenBucket (keeper/internal/redis/ratelimit.go).
//
// Интерфейс (а не конкретный тип) — чтобы middleware жил в `api/middleware`
// без import-зависимости на `internal/redis` и чтобы unit-тесты подменяли
// limiter fake-ом (как DegradedReader у Toll). allowed/retryAfter/err — по
// контракту TokenBucket.Allow.
type RateLimiter interface {
	Allow(ctx context.Context, aid, bucket string, rate float64, burst int) (allowed bool, retryAfter time.Duration, err error)
}

// RateLimitMetrics — узкая metrics-поверхность Tempo (ADR-050(g)).
// Реализуется keeper/internal/api/server-метриками; nil-safe (nil hook → emit
// no-op). Лейбл endpoint = bucket-имя; AID-лейбла НЕТ (кардинальность).
type RateLimitMetrics interface {
	IncTempoAllowed(endpoint string)
	IncTempoRejected(endpoint string)
}

// RateLimitLimits — текущие rate/burst bucket-а. Возвращается провайдером,
// который читает живой config.Store snapshot (ADR-021 hot-reload): новый лимит
// применяется со следующего запроса без рестарта, текущие бакеты в Redis
// доживают по своему PEXPIRE.
type RateLimitLimits struct {
	Rate  float64
	Burst int
}

// RateLimit возвращает chi/net-http-совместимый middleware Tempo-лимита для
// конкретного логического bucket-а (`bucket`, напр. "voyage_create").
//
// rate/burst читаются НЕ при сборке, а на каждом запросе через `limits`-провайдер
// (hot-reload, ADR-050(f)/ADR-021): провайдер отдаёт живой config.Store snapshot.
// Невалидные (≤0) значения провайдера трактуются как fail-OPEN passthrough —
// limiter.Allow всё равно отверг бы их ошибкой, и на сбое конфига блокировать
// нельзя (доступность > перестраховка, как при Redis-флапе).
//
// limiter=nil → middleware no-op (passthrough). Симметрично toll-middleware
// при reader=nil: dev/single-instance без Redis не блокирует ничего, и тесты
// роутера, которым Tempo не нужен, не получают сюрпризов.
//
// metrics=nil → emit no-op (unit-тесты без obs.Registry).
//
// Логика:
//  1. limiter=nil → passthrough (решается на этапе фабрики, без overhead на
//     запрос).
//  2. Нет claims в context (middleware прицеплен без RequireJWT-цепочки) →
//     fail-OPEN passthrough + warn. Это ошибка wire-up-а, но блокировать
//     по «нет AID» нельзя — bucket per-AID, без AID лимит бессмыслен.
//  3. limiter.Allow вернул ошибку (Redis-сбой / скрипт упал) → fail-OPEN
//     passthrough + debug (ADR-050(b): доступность > перестраховка, как Toll).
//  4. allowed=false → 429 + Retry-After + problem+json, next НЕ вызывается,
//     keeper_tempo_rejected_total{endpoint} +1.
//  5. allowed=true → passthrough, keeper_tempo_allowed_total{endpoint} +1.
func RateLimit(limiter RateLimiter, bucket string, limits func() RateLimitLimits, metrics RateLimitMetrics, logger *slog.Logger) func(http.Handler) http.Handler {
	if limiter == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFromContext(r.Context())
			if !ok || claims.Subject == "" {
				// Wire-up без RequireJWT перед Tempo — программная ошибка
				// конфигурации сервера. Fail-OPEN: лимит per-AID без AID
				// невычислим, блокировать нельзя.
				logger.Warn("tempo: no AID in context — middleware навешан без RequireJWT? fail-open",
					slog.String("path", r.URL.Path),
				)
				next.ServeHTTP(w, r)
				return
			}

			lim := limits()
			if lim.Rate <= 0 || lim.Burst <= 0 {
				// Битый/нулевой конфиг (hot-reload подсунул невалид) → fail-OPEN.
				logger.Debug("tempo: non-positive rate/burst from config — fail-open",
					slog.Float64("rate", lim.Rate),
					slog.Int("burst", lim.Burst),
					slog.String("bucket", bucket),
				)
				next.ServeHTTP(w, r)
				return
			}

			allowed, retryAfter, err := limiter.Allow(r.Context(), claims.Subject, bucket, lim.Rate, lim.Burst)
			if err != nil {
				// Fail-OPEN: Redis-флап — частое явление, доступность важнее
				// перестраховки (ADR-050(b)).
				logger.Debug("tempo: limiter check failed — fail-open",
					slog.Any("error", err),
					slog.String("aid", claims.Subject),
					slog.String("bucket", bucket),
					slog.String("path", r.URL.Path),
				)
				next.ServeHTTP(w, r)
				return
			}
			if allowed {
				if metrics != nil {
					metrics.IncTempoAllowed(bucket)
				}
				next.ServeHTTP(w, r)
				return
			}
			if metrics != nil {
				metrics.IncTempoRejected(bucket)
			}
			writeTempo429(w, r, retryAfter)
		})
	}
}

// writeTempo429 пишет 429 + Retry-After + RFC 7807 problem+json
// ([problem.TypeTempoExceeded]). Retry-After — секунды до пополнения хотя бы
// одного токена, округление ВВЕРХ (раньше срока ретрай снова упрётся в пустой
// бакет). Минимум 1с: заголовок Retry-After в секундах, 0 ввёл бы клиента в
// busy-retry.
func writeTempo429(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	secs := int(retryAfter / time.Second)
	if retryAfter%time.Second > 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	problem.Write(w, problem.New(
		problem.TypeTempoExceeded,
		r.URL.Path,
		"rate limit exceeded for this operator; retry after the Retry-After interval",
	))
}
