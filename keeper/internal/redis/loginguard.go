package redis

// LoginGuard — anti-bruteforce-примитив для публичных login-эндпоинтов
// (ADR-058(g), HIGH-3). Два механизма поверх Redis, оба cluster-shared
// (авторитет в Redis, не размножается ×N по stateless-HA-инстансам Keeper-а,
// ADR-002/ADR-006):
//
//  1. ТРОТТЛ ЧАСТОТЫ (token-bucket) — лимит на ЧИСЛО ПОПЫТОК с одного принципала
//     (IP или username) в единицу времени. Берётся ДО обработки запроса, на
//     КАЖДУЮ попытку. Гасит флуд (включая flood flow-state на /auth/oidc/login —
//     каждый login-старт тратит токен). Совпадает по алгоритму с Tempo-bucket-ом
//     (ratelimit.go), но отдельный key-prefix `authrl:` и принципал = IP/username,
//     а не AID (логин — pre-JWT, AID ещё нет).
//
//  2. LOCKOUT ПО НЕУДАЧАМ — счётчик ПРОВАЛЕННЫХ логинов в скользящем окне; при
//     достижении порога принципал блокируется на backoff-интервал (для IP и для
//     username независимо). RecordFailure инкрементит счётчик ПОСЛЕ неудачи;
//     Locked/RetryAfter проверяет блокировку ДО обработки. Успешный логин счётчик
//     НЕ трогает (provision/role-смена — не неудача); счётчик сам истекает по TTL
//     окна. Anti-bruteforce: подбор пароля и перебор username отбиваются.
//
// Fail-closed на Lockout-проверке: при Redis-ошибке Locked возвращает ошибку,
// middleware решает политику (HIGH-3 требует fail-closed на login — в отличие от
// Tempo fail-open: login — security-периметр, недоступность Redis НЕ должна
// открывать брутфорс). Троттл-часть (Allow) при Redis-ошибке — на усмотрение
// middleware; реализация лишь сигнализирует ошибку.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// LoginGuard — handle для anti-bruteforce-операций login-эндпоинтов. Stateless
// относительно конкретного принципала: ключ передаётся в каждый вызов.
type LoginGuard struct {
	client *Client
}

// NewLoginGuard оборачивает Redis-клиент. Ошибка только на nil-клиенте
// (программный wire-up: middleware при отсутствии Redis получает nil-guard).
func NewLoginGuard(c *Client) (*LoginGuard, error) {
	if c == nil {
		return nil, errors.New("redis.NewLoginGuard: nil client")
	}
	return &LoginGuard{client: c}, nil
}

// throttleKey — ключ token-bucket-а троттла попыток. `authrl:<scope>:<principal>`
// (scope = "ip"|"user", principal = IP-адрес/username). Отдельный prefix от
// `tempo:` — auth-троттл живёт независимо от Tempo-квот операций.
func throttleKey(scope, principal string) string {
	return "authrl:" + scope + ":" + principal
}

// lockoutCountKey — ключ счётчика неудач. `authlock:<scope>:<principal>:n`.
func lockoutCountKey(scope, principal string) string {
	return "authlock:" + scope + ":" + principal + ":n"
}

// lockoutFlagKey — ключ флага блокировки. `authlock:<scope>:<principal>:locked`.
func lockoutFlagKey(scope, principal string) string {
	return "authlock:" + scope + ":" + principal + ":locked"
}

// Allow атомарно берёт один токен троттла попыток для принципала (scope+principal).
// Алгоритм/контракт идентичны [TokenBucket.Allow] (тот же Lua refill+take), но
// отдельный key-prefix. allowed=false → retryAfter до пополнения токена.
//
// Caller (middleware) на этой ветке может деградировать по своей политике;
// рекомендация HIGH-3 — для login fail-closed недопустим только на LOCKOUT,
// троттл при Redis-флапе допускает fail-open (доступность login-страницы).
func (g *LoginGuard) Allow(ctx context.Context, scope, principal string, rate float64, burst int) (allowed bool, retryAfter time.Duration, err error) {
	if g == nil || g.client == nil {
		return false, 0, errors.New("redis.LoginGuard.Allow: nil guard/client")
	}
	if scope == "" || principal == "" {
		return false, 0, errors.New("redis.LoginGuard.Allow: empty scope/principal")
	}
	if rate <= 0 {
		return false, 0, fmt.Errorf("redis.LoginGuard.Allow: rate must be > 0, got %v", rate)
	}
	if burst <= 0 {
		return false, 0, fmt.Errorf("redis.LoginGuard.Allow: burst must be > 0, got %d", burst)
	}

	key := throttleKey(scope, principal)
	res, runErr := allowScript.Run(ctx, g.client.underlying(),
		[]string{key},
		rate,
		burst,
		bucketTTL(rate, burst).Milliseconds(),
	).Int64Slice()
	if runErr != nil {
		return false, 0, fmt.Errorf("redis.LoginGuard.Allow %q: %w", key, runErr)
	}
	if len(res) != 2 {
		return false, 0, fmt.Errorf("redis.LoginGuard.Allow %q: unexpected script result len=%d", key, len(res))
	}
	allowed = res[0] == 1
	if !allowed {
		retryAfter = time.Duration(res[1]) * time.Millisecond
	}
	return allowed, retryAfter, nil
}

// Locked сообщает, заблокирован ли принципал прямо сейчас (флаг-ключ существует),
// и сколько до снятия блокировки (PTTL). Проверяется ДО обработки логина.
//
// Fail-closed: при Redis-ошибке возвращает err — caller на login-периметре
// ОБЯЗАН трактовать ошибку как «заблокировано» (HIGH-3: недоступность Redis не
// должна открывать брутфорс).
func (g *LoginGuard) Locked(ctx context.Context, scope, principal string) (locked bool, retryAfter time.Duration, err error) {
	if g == nil || g.client == nil {
		return false, 0, errors.New("redis.LoginGuard.Locked: nil guard/client")
	}
	if scope == "" || principal == "" {
		return false, 0, errors.New("redis.LoginGuard.Locked: empty scope/principal")
	}
	key := lockoutFlagKey(scope, principal)
	ttl, runErr := g.client.underlying().PTTL(ctx, key).Result()
	if runErr != nil {
		return false, 0, fmt.Errorf("redis.LoginGuard.Locked %q: %w", key, runErr)
	}
	// PTTL: -2 = нет ключа (не заблокирован), -1 = без TTL (не должно случаться —
	// флаг ставится с PEXPIRE), >0 = заблокирован, осталось ttl.
	if ttl < 0 {
		return false, 0, nil
	}
	return true, ttl, nil
}

// recordFailureScript атомарно инкрементит счётчик неудач принципала и, при
// достижении порога, выставляет lockout-флаг с backoff-TTL.
//
// KEYS[1] — счётчик неудач (authlock:<...>:n).
// KEYS[2] — флаг блокировки (authlock:<...>:locked).
// ARGV[1] — threshold (порог неудач для блокировки).
// ARGV[2] — windowMs (TTL окна счётчика, мс).
// ARGV[3] — lockoutMs (backoff-TTL флага блокировки, мс).
//
// Возврат — {count, locked}: count — текущее число неудач, locked=1 если порог
// достигнут и флаг выставлен. Счётчик скользит по TTL окна (каждая неудача
// продлевает окно — наблюдаемое поведение «N неудач за окно»). При выставлении
// флага счётчик сбрасывается (DEL), чтобы после снятия блокировки порог считался
// заново, а не блокировал бесконечно одной старой серией.
var recordFailureScript = redis.NewScript(`
local threshold = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local lockout_ms = tonumber(ARGV[3])

local count = redis.call("INCR", KEYS[1])
redis.call("PEXPIRE", KEYS[1], window_ms)

if count >= threshold then
  redis.call("SET", KEYS[2], "1", "PX", lockout_ms)
  redis.call("DEL", KEYS[1])
  return {count, 1}
end
return {count, 0}
`)

// RecordFailure инкрементит счётчик неудач принципала после проваленного логина;
// при достижении threshold выставляет lockout-флаг на lockout-интервал. Окно
// счётчика — window. Возвращает, привела ли эта неудача к блокировке.
//
// Успешный логин RecordFailure НЕ вызывает — счётчик истечёт сам по TTL окна
// (нет необходимости в явном reset-е: безуспешные серии естественно затухают).
func (g *LoginGuard) RecordFailure(ctx context.Context, scope, principal string, threshold int, window, lockout time.Duration) (lockedNow bool, err error) {
	if g == nil || g.client == nil {
		return false, errors.New("redis.LoginGuard.RecordFailure: nil guard/client")
	}
	if scope == "" || principal == "" {
		return false, errors.New("redis.LoginGuard.RecordFailure: empty scope/principal")
	}
	if threshold <= 0 {
		return false, fmt.Errorf("redis.LoginGuard.RecordFailure: threshold must be > 0, got %d", threshold)
	}
	res, runErr := recordFailureScript.Run(ctx, g.client.underlying(),
		[]string{lockoutCountKey(scope, principal), lockoutFlagKey(scope, principal)},
		threshold,
		window.Milliseconds(),
		lockout.Milliseconds(),
	).Int64Slice()
	if runErr != nil {
		return false, fmt.Errorf("redis.LoginGuard.RecordFailure: %w", runErr)
	}
	if len(res) != 2 {
		return false, fmt.Errorf("redis.LoginGuard.RecordFailure: unexpected script result len=%d", len(res))
	}
	return res[1] == 1, nil
}
