package middleware

// Anti-bruteforce middleware публичных login-эндпоинтов (ADR-058(g), HIGH-3).
// Навешивается на chi-группу `/auth/*` ПЕРЕД huma-операциями login-а. Два слоя,
// оба cluster-shared через Redis (LoginGuard):
//
//  1. LOCKOUT (fail-CLOSED): принципал (IP / username), набравший порог неудач,
//     блокируется на backoff. Проверяется ДО next. При Redis-ошибке трактуется
//     как «заблокировано» — login — security-периметр, недоступность Redis НЕ
//     должна открывать брутфорс (в ОТЛИЧИЕ от Tempo fail-open).
//  2. THROTTLE частоты (fail-OPEN): token-bucket на принципал, гасит флуд
//     (включая flow-state-flood на /auth/oidc/login). Берётся ДО next, на каждую
//     попытку. При Redis-ошибке — passthrough (доступность login-страницы).
//
// ПОСЛЕ next: если ответ — auth-failure (401/403), инкрементим счётчик неудач
// для IP и username (RecordFailure). Успех (2xx/302) и прочие коды счётчик не
// трогают. anti-oracle: единый 429 + Retry-After без раскрытия, по IP это или по
// username, locked или throttled.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// LoginGuard — поверхность anti-bruteforce-примитива, нужная middleware.
// Реализуется *redis.LoginGuard. Интерфейс (а не конкретный тип) — чтобы
// middleware жил в api/middleware без import-зависимости на internal/redis и
// чтобы unit-тесты подменяли guard fake-ом (как RateLimiter у Tempo).
type LoginGuard interface {
	Allow(ctx context.Context, scope, principal string, rate float64, burst int) (allowed bool, retryAfter time.Duration, err error)
	Locked(ctx context.Context, scope, principal string) (locked bool, retryAfter time.Duration, err error)
	RecordFailure(ctx context.Context, scope, principal string, threshold int, window, lockout time.Duration) (lockedNow bool, err error)
}

const (
	authScopeIP   = "ip"
	authScopeUser = "user"

	// maxLoginBodySnoop — потолок чтения тела login-а для извлечения username.
	// Тело login-а — крошечный JSON; больше читать незачем (anti-DoS дублирует
	// общий maxBody на /auth-группе). Прочитанное буферизуется и возвращается в
	// r.Body, чтобы handler перечитал его целиком.
	maxLoginBodySnoop = 4 << 10 // 4 KiB
)

// AuthLoginLimitConfig — статические параметры anti-bruteforce-лимита. Читаются
// один раз на сборке middleware (НЕ hot-path — login редок). Резолв из
// config.KeeperAuth.ResolvedLoginRateLimit() в daemon.
type AuthLoginLimitConfig struct {
	Rate             float64
	Burst            int
	LockoutThreshold int
	LockoutWindow    time.Duration
	LockoutBackoff   time.Duration
}

// AuthLoginLimit возвращает chi-middleware anti-bruteforce login-а.
//
// guard=nil → passthrough (нет Redis — login без throttle, как Tempo при
// limiter=nil; согласуется с OPTIONAL-tier). extractUsername — опц. экстрактор
// username из запроса (LDAP: из JSON-тела; OIDC-login: nil — только per-IP).
// recordFailures — писать ли счётчик неудач: true для login-а (LDAP) и
// OIDC-callback (где решается успех/неудача); для OIDC-login (старт flow,
// «неудачи» нет) — false (только throttle флуда).
func AuthLoginLimit(
	guard LoginGuard,
	cfg AuthLoginLimitConfig,
	extractUsername func(r *http.Request) string,
	recordFailures bool,
	logger *slog.Logger,
) func(http.Handler) http.Handler {
	if guard == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			username := ""
			if extractUsername != nil {
				username = extractUsername(r)
			}

			// 1. LOCKOUT (fail-closed). IP — всегда; username — если извлёкся.
			if blocked, retryAfter := lockedAny(r.Context(), guard, ip, username, logger); blocked {
				writeAuth429(w, r, retryAfter)
				return
			}

			// 2. THROTTLE (fail-open). IP — всегда; username — если извлёкся.
			if throttled, retryAfter := throttledAny(r.Context(), guard, cfg, ip, username, logger); throttled {
				writeAuth429(w, r, retryAfter)
				return
			}

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// 3. ПОСЛЕ: auth-failure (401/403) → инкремент счётчика неудач.
			if recordFailures && isAuthFailure(rec.status) {
				recordFailureFor(r.Context(), guard, cfg, authScopeIP, ip, logger)
				if username != "" {
					recordFailureFor(r.Context(), guard, cfg, authScopeUser, username, logger)
				}
			}
		})
	}
}

// lockedAny проверяет lockout по IP и (если задан) username. Fail-closed: при
// Redis-ошибке возвращает blocked=true (login-периметр не открываем брутфорсу).
func lockedAny(ctx context.Context, guard LoginGuard, ip, username string, logger *slog.Logger) (bool, time.Duration) {
	for _, p := range []struct{ scope, principal string }{
		{authScopeIP, ip},
		{authScopeUser, username},
	} {
		if p.principal == "" {
			continue
		}
		locked, retryAfter, err := guard.Locked(ctx, p.scope, p.principal)
		if err != nil {
			// Fail-CLOSED (HIGH-3): Redis недоступен → считаем заблокированным.
			if logger != nil {
				logger.Warn("auth/limit: lockout check failed — fail-closed (treating as locked)",
					slog.String("scope", p.scope), slog.Any("error", err))
			}
			return true, defaultLockedRetry
		}
		if locked {
			return true, retryAfter
		}
	}
	return false, 0
}

// throttledAny берёт токен троттла по IP и (если задан) username. Fail-open: при
// Redis-ошибке/битом конфиге — passthrough (не блокируем).
func throttledAny(ctx context.Context, guard LoginGuard, cfg AuthLoginLimitConfig, ip, username string, logger *slog.Logger) (bool, time.Duration) {
	if cfg.Rate <= 0 || cfg.Burst <= 0 {
		return false, 0 // битый конфиг → fail-open (как Tempo)
	}
	for _, p := range []struct{ scope, principal string }{
		{authScopeIP, ip},
		{authScopeUser, username},
	} {
		if p.principal == "" {
			continue
		}
		allowed, retryAfter, err := guard.Allow(ctx, p.scope, p.principal, cfg.Rate, cfg.Burst)
		if err != nil {
			// Fail-OPEN: троттл при Redis-флапе не блокирует (доступность login).
			if logger != nil {
				logger.Debug("auth/limit: throttle check failed — fail-open",
					slog.String("scope", p.scope), slog.Any("error", err))
			}
			continue
		}
		if !allowed {
			return true, retryAfter
		}
	}
	return false, 0
}

// recordFailureFor инкрементит счётчик неудач принципала; ошибку только логирует
// (счётчик best-effort — потеря инкремента не должна валить ответ login-а).
func recordFailureFor(ctx context.Context, guard LoginGuard, cfg AuthLoginLimitConfig, scope, principal string, logger *slog.Logger) {
	if cfg.LockoutThreshold <= 0 {
		return
	}
	lockedNow, err := guard.RecordFailure(ctx, scope, principal, cfg.LockoutThreshold, cfg.LockoutWindow, cfg.LockoutBackoff)
	if err != nil {
		if logger != nil {
			logger.Debug("auth/limit: record failure failed",
				slog.String("scope", scope), slog.Any("error", err))
		}
		return
	}
	if lockedNow && logger != nil {
		// Принципал заблокирован — INFO (операционно полезно), без секрета.
		logger.Info("auth/limit: principal locked out after repeated login failures",
			slog.String("scope", scope))
	}
}

// defaultLockedRetry — Retry-After при fail-closed lockout (Redis-ошибка): не
// раскрываем реальный backoff (его не знаем), даём консервативную паузу.
const defaultLockedRetry = 60 * time.Second

// isAuthFailure — код ответа, трактуемый как проваленная аутентификация для
// счётчика неудач: 401 (bad credentials, ErrAuthFailed) и 403 (revoked / no role
// mapping / provisioning disabled). 2xx/302 (успех) и 5xx (наша ошибка) — НЕ
// неудача пользователя, счётчик не трогаем.
func isAuthFailure(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// LDAPUsernameExtractor извлекает `username` из JSON-тела POST /auth/ldap/login,
// буферизует и возвращает тело в r.Body (handler перечитает целиком). Ошибка
// парсинга / не-JSON → "" (per-username слой просто не применяется, per-IP
// остаётся). Не логирует тело (содержит пароль).
func LDAPUsernameExtractor(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBodySnoop+1))
	_ = r.Body.Close()
	// Вернуть тело handler-у в любом случае (даже если парс не удался).
	r.Body = io.NopCloser(bytes.NewReader(buf))
	if err != nil || len(buf) > maxLoginBodySnoop {
		return ""
	}
	var body struct {
		Username string `json:"username"`
	}
	if json.Unmarshal(buf, &body) != nil {
		return ""
	}
	return body.Username
}

// clientIP — IP прямого пира (r.RemoteAddr). НЕ доверяет X-Forwarded-For/
// X-Real-IP (spoofable без trusted-proxy-конфигурации, которой у Keeper-а пока
// нет — см. observations). За L4-LB (passthrough) RemoteAddr — реальный клиент;
// за L7-proxy все попытки сойдутся на IP прокси (тогда per-username-слой несёт
// основную защиту). Порт отбрасывается (иначе каждый эфемерный порт — свой
// принципал, и per-IP-лимит обходится).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // без порта (редко) — как есть
	}
	return host
}

// writeAuth429 пишет 429 + Retry-After + RFC 7807 problem+json
// ([problem.TypeAuthThrottled]). anti-oracle: detail без указания scope/причины.
func writeAuth429(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	secs := int(retryAfter / time.Second)
	if retryAfter%time.Second > 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	problem.Write(w, problem.New(
		problem.TypeAuthThrottled,
		r.URL.Path,
		"too many login attempts; retry after the Retry-After interval",
	))
}

// statusRecorder — обёртка ResponseWriter, фиксирующая статус-код (для пост-
// проверки auth-failure). Минимальная: huma пишет тело сам, нам нужен лишь код.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true // implicit 200
	}
	return s.ResponseWriter.Write(b)
}
