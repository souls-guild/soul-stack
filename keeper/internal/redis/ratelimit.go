package redis

// TokenBucket — per-AID rate-limiter Tempo (ADR-050) поверх Redis.
//
// Алгоритм — классический token-bucket, состояние которого живёт в Redis-hash
// `tempo:<aid>:<bucket>` (поля `tokens` / `last_refill_ts`). Refill+take —
// атомарно ОДНИМ Lua-скриптом (read-modify-write бакета в одном round-trip),
// что и даёт когерентный лимит поверх stateless-HA-кластера Keeper-а: лимит
// авторитетен в Redis, а не размножается ×N по инстансам (ADR-050(a)).
//
// Образец стиля — соседние [Lease] (lease.go) / SoulLease (soullease.go):
// вся атомарная логика — в Lua-скрипте, Go-обёртка только формирует ключ,
// прокидывает аргументы и интерпретирует результат.
//
// Время бакет читает ИЗ САМОГО Redis-а (`redis.call("TIME")`), а не из Go-часов
// вызывающего: иначе refill зависел бы от рассинхрона часов между N Keeper-
// инстансами, и бакет «дрейфовал» бы при обращениях с разных инстансов. TIME —
// единый источник для всех take-ов одного бакета. NB: `redis.call("TIME")` —
// non-deterministic, поэтому скрипт требует Redis >= 5 (effects-replication:
// в реплику/AOF попадают эффекты HSET/PEXPIRE, а не сам скрипт).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// TokenBucket — handle для take-операций над token-bucket-ами в Redis.
// Stateless относительно конкретного бакета: ключ передаётся в каждый [Allow],
// один TokenBucket обслуживает любое число AID/bucket-комбинаций.
type TokenBucket struct {
	client *Client
}

// NewTokenBucket оборачивает Redis-клиент в Tempo-rate-limiter. Возвращает
// ошибку только на nil-клиенте — это программная ошибка wire-up-а (middleware
// при отсутствии Redis получает nil-limiter и работает passthrough, см.
// api/middleware/ratelimit.go, поэтому до сюда nil доходить не должен).
func NewTokenBucket(c *Client) (*TokenBucket, error) {
	if c == nil {
		return nil, errors.New("redis.NewTokenBucket: nil client")
	}
	return &TokenBucket{client: c}, nil
}

// tokenBucketKey формирует Redis-ключ бакета. Convention `tempo:<aid>:<bucket>`
// зафиксирована каноном (ADR-050(a), naming-rules.md): per-AID, per-логический-
// bucket эндпоинта.
func tokenBucketKey(aid, bucket string) string {
	return "tempo:" + aid + ":" + bucket
}

// allowScript — атомарный refill+take token-bucket-а.
//
// KEYS[1] — ключ бакета (`tempo:<aid>:<bucket>`).
// ARGV[1] — rate (токенов в секунду, float).
// ARGV[2] — burst (capacity бакета, целое > 0).
// ARGV[3] — TTL ключа в миллисекундах (PEXPIRE, скользит на каждом take).
//
// Возврат — массив `{allowed, retry_after_ms}`:
//   - allowed=1, retry_after_ms=0      — токен взят, запрос пропускается;
//   - allowed=0, retry_after_ms=<ms>   — бакет пуст, до пополнения хотя бы
//     одного токена осталось retry_after_ms миллисекунд.
//
// Время — из `redis.call("TIME")` (см. doc-comment пакета). Дробная часть
// токенов сохраняется в hash как float-строка: иначе высокий-rate-low-burst
// сценарий терял бы накопление между take-ами (каждый раз округление в ноль).
//
// Первый запрос к несуществующему бакету трактуется как полный бакет
// (tokens = burst): новый оператор не штрафуется «холодным» бакетом.
var allowScript = redis.NewScript(`
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local ttl_ms = tonumber(ARGV[3])

local t = redis.call("TIME")
-- t[1] = seconds, t[2] = microseconds. now_ms = sec*1000 + usec/1000.
local now_ms = (tonumber(t[1]) * 1000) + (tonumber(t[2]) / 1000)

local data = redis.call("HMGET", KEYS[1], "tokens", "last_refill_ts")
local tokens = tonumber(data[1])
local last_ms = tonumber(data[2])

if tokens == nil or last_ms == nil then
  -- Холодный бакет — стартуем полным.
  tokens = burst
  last_ms = now_ms
end

-- Refill: за прошедшее время накапливаем rate*dt токенов, но не выше burst.
local elapsed_ms = now_ms - last_ms
if elapsed_ms < 0 then
  elapsed_ms = 0
end
tokens = tokens + (elapsed_ms / 1000.0) * rate
if tokens > burst then
  tokens = burst
end

local allowed = 0
local retry_after_ms = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  -- Не хватает (1 - tokens) токенов; при rate токенов/сек до них ждать
  -- (1 - tokens)/rate секунд. rate > 0 гарантируется Go-валидацией.
  retry_after_ms = math.ceil(((1 - tokens) / rate) * 1000)
end

redis.call("HSET", KEYS[1], "tokens", tostring(tokens), "last_refill_ts", tostring(now_ms))
redis.call("PEXPIRE", KEYS[1], ttl_ms)

return {allowed, retry_after_ms}
`)

// bucketTTL — за сколько времени полностью пустой бакет восстанавливается до
// capacity: burst/rate секунд + запас. Дольше держать ключ незачем (бакет
// и так был бы полным), но и обрезать раньше нельзя — иначе активный лимит
// «забывался» бы между всплесками. Возвращается в миллисекундах для PEXPIRE.
func bucketTTL(rate float64, burst int) time.Duration {
	refillSeconds := float64(burst) / rate
	// Удваиваем + 1с страховки от округления — TTL не критичен к точности,
	// важно лишь чтобы ключ пережил окно реального использования бакета.
	ttl := time.Duration((refillSeconds*2)+1) * time.Second
	return ttl
}

// Allow атомарно пытается взять один токен из бакета `tempo:<aid>:<bucket>`.
//
//   - allowed=true             — токен взят, запрос можно пропускать;
//     retryAfter == 0.
//   - allowed=false            — бакет пуст; retryAfter — время до пополнения
//     хотя бы одного токена (для HTTP-заголовка Retry-After).
//
// `rate` — токенов в секунду (> 0), `burst` — ёмкость бакета (> 0). Невалидные
// аргументы — программная ошибка caller-а (конфиг провалидирован выше), здесь
// отвергаются явной ошибкой, а не молча.
//
// Ошибка возвращается только на сетевой/протокольный сбой Redis-а либо на
// невалидные аргументы. Caller (middleware) при Redis-ошибке деградирует
// fail-OPEN (passthrough, ADR-050(b)) — Allow сам решения о fail-open не
// принимает, лишь сигнализирует ошибку.
func (tb *TokenBucket) Allow(ctx context.Context, aid, bucket string, rate float64, burst int) (allowed bool, retryAfter time.Duration, err error) {
	if tb == nil || tb.client == nil {
		return false, 0, errors.New("redis.TokenBucket.Allow: nil bucket/client")
	}
	if aid == "" {
		return false, 0, errors.New("redis.TokenBucket.Allow: empty aid")
	}
	if bucket == "" {
		return false, 0, errors.New("redis.TokenBucket.Allow: empty bucket")
	}
	if rate <= 0 {
		return false, 0, fmt.Errorf("redis.TokenBucket.Allow: rate must be > 0, got %v", rate)
	}
	if burst <= 0 {
		return false, 0, fmt.Errorf("redis.TokenBucket.Allow: burst must be > 0, got %d", burst)
	}

	key := tokenBucketKey(aid, bucket)
	res, runErr := allowScript.Run(ctx, tb.client.underlying(),
		[]string{key},
		rate,
		burst,
		bucketTTL(rate, burst).Milliseconds(),
	).Int64Slice()
	if runErr != nil {
		return false, 0, fmt.Errorf("redis.TokenBucket.Allow %q: %w", key, runErr)
	}
	if len(res) != 2 {
		return false, 0, fmt.Errorf("redis.TokenBucket.Allow %q: unexpected script result len=%d", key, len(res))
	}

	allowed = res[0] == 1
	if !allowed {
		retryAfter = time.Duration(res[1]) * time.Millisecond
	}
	return allowed, retryAfter, nil
}
