package middleware

// Anti-bruteforce middleware for the public login endpoints (ADR-058(g), HIGH-3).
// Attached to the chi group `/auth/*` BEFORE the login huma operations. Two layers,
// both cluster-shared via Redis (LoginGuard):
//
//  1. LOCKOUT (fail-CLOSED): a principal (IP / username) that has hit the failure
//     threshold is blocked for a backoff period. Checked BEFORE next. On a Redis error
//     it is treated as "blocked" — login is a security perimeter, Redis unavailability must
//     NOT open the door to bruteforce (UNLIKE Tempo's fail-open).
//  2. Rate THROTTLE (fail-OPEN): a token-bucket per principal, damps floods
//     (including flow-state flood on /auth/oidc/login). Taken BEFORE next, on every
//     attempt. On a Redis error — passthrough (login-page availability).
//
// AFTER next: if the response is an auth-failure (401/403), increment the failure
// counter for the IP and username (RecordFailure). Success (2xx/302) and other codes leave the
// counter untouched. anti-oracle: a single 429 + Retry-After without disclosing whether
// it was by IP or by username, locked or throttled.

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

// LoginGuard — the anti-bruteforce primitive surface the middleware needs.
// Implemented by *redis.LoginGuard. An interface (not a concrete type) — so
// the middleware lives in api/middleware without an import dependency on internal/redis, and so
// unit tests can swap the guard with a fake (like Tempo's RateLimiter).
type LoginGuard interface {
	Allow(ctx context.Context, scope, principal string, rate float64, burst int) (allowed bool, retryAfter time.Duration, err error)
	Locked(ctx context.Context, scope, principal string) (locked bool, retryAfter time.Duration, err error)
	RecordFailure(ctx context.Context, scope, principal string, threshold int, window, lockout time.Duration) (lockedNow bool, err error)
}

const (
	authScopeIP   = "ip"
	authScopeUser = "user"

	// maxLoginBodySnoop — the ceiling on reading the login body to extract the username.
	// The login body is a tiny JSON; reading more is pointless (anti-DoS is already covered by
	// the shared maxBody on the /auth group). What's read is buffered and returned into
	// r.Body so the handler can re-read it in full.
	maxLoginBodySnoop = 4 << 10 // 4 KiB
)

// AuthLoginLimitConfig — the static parameters of the anti-bruteforce limit. Read
// once when the middleware is assembled (NOT a hot path — login is rare). Resolved from
// config.KeeperAuth.ResolvedLoginRateLimit() in the daemon.
type AuthLoginLimitConfig struct {
	Rate             float64
	Burst            int
	LockoutThreshold int
	LockoutWindow    time.Duration
	LockoutBackoff   time.Duration
}

// AuthLoginLimit returns the login anti-bruteforce chi-middleware.
//
// guard=nil → passthrough (no Redis — login without throttling, like Tempo with
// limiter=nil; consistent with the OPTIONAL tier). extractUsername — an optional extractor
// of the username from the request (LDAP: from the JSON body; OIDC-login: nil — per-IP only).
// recordFailures — whether to write the failure counter: true for login (LDAP) and the
// OIDC callback (where success/failure is decided); for OIDC-login (start of the flow,
// there is no "failure") — false (throttling of floods only).
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

			// 1. LOCKOUT (fail-closed). IP — always; username — if extracted.
			if blocked, retryAfter := lockedAny(r.Context(), guard, ip, username, logger); blocked {
				writeAuth429(w, r, retryAfter)
				return
			}

			// 2. THROTTLE (fail-open). IP — always; username — if extracted.
			if throttled, retryAfter := throttledAny(r.Context(), guard, cfg, ip, username, logger); throttled {
				writeAuth429(w, r, retryAfter)
				return
			}

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// 3. AFTER: an auth-failure (401/403) → increment the failure counter.
			if recordFailures && isAuthFailure(rec.status) {
				recordFailureFor(r.Context(), guard, cfg, authScopeIP, ip, logger)
				if username != "" {
					recordFailureFor(r.Context(), guard, cfg, authScopeUser, username, logger)
				}
			}
		})
	}
}

// lockedAny checks lockout by IP and (if set) username. Fail-closed: on a
// Redis error it returns blocked=true (the login perimeter is not opened to bruteforce).
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
			// Fail-CLOSED (HIGH-3): Redis unavailable → treat as blocked.
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

// throttledAny takes a throttle token by IP and (if set) username. Fail-open: on a
// Redis error/broken config — passthrough (do not block).
func throttledAny(ctx context.Context, guard LoginGuard, cfg AuthLoginLimitConfig, ip, username string, logger *slog.Logger) (bool, time.Duration) {
	if cfg.Rate <= 0 || cfg.Burst <= 0 {
		return false, 0 // broken config → fail-open (like Tempo)
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
			// Fail-OPEN: a Redis flap does not block the throttle (login availability).
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

// recordFailureFor increments the principal's failure counter; an error is only logged
// (the counter is best-effort — losing an increment must not break the login response).
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
		// Principal locked out — INFO (operationally useful), no secret in it.
		logger.Info("auth/limit: principal locked out after repeated login failures",
			slog.String("scope", scope))
	}
}

// defaultLockedRetry — Retry-After for a fail-closed lockout (Redis error): we do
// not disclose the real backoff (we don't know it), we give a conservative pause instead.
const defaultLockedRetry = 60 * time.Second

// isAuthFailure — the response codes treated as a failed authentication for the
// failure counter: 401 (bad credentials, ErrAuthFailed) and 403 (revoked / no role
// mapping / provisioning disabled). 2xx/302 (success) and 5xx (our own error) are NOT
// a user failure, the counter is left untouched.
func isAuthFailure(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// LDAPUsernameExtractor extracts `username` from the JSON body of POST /auth/ldap/login,
// buffers it, and returns the body into r.Body (the handler re-reads it in full). A parse
// error / non-JSON body → "" (the per-username layer simply does not apply, per-IP
// still stands). Does not log the body (it contains the password).
func LDAPUsernameExtractor(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBodySnoop+1))
	_ = r.Body.Close()
	// Return the body to the handler in any case (even if the parse failed).
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

// clientIP — the IP of the direct peer (r.RemoteAddr). Does NOT trust X-Forwarded-For/
// X-Real-IP (spoofable without a trusted-proxy configuration, which Keeper does not
// have yet — see observations). Behind an L4 LB (passthrough) RemoteAddr is the real client;
// behind an L7 proxy all attempts collapse onto the proxy's IP (then the per-username layer carries
// the main defense). The port is dropped (otherwise every ephemeral port is its own
// principal, and the per-IP limit is bypassed).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // no port (rare) — return as-is
	}
	return host
}

// writeAuth429 writes 429 + Retry-After + RFC 7807 problem+json
// ([problem.TypeAuthThrottled]). anti-oracle: the detail does not disclose scope/reason.
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

// statusRecorder — a ResponseWriter wrapper that records the status code (for the
// post-hoc auth-failure check). Minimal: huma writes the body itself, we only need the code.
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

// NIM-37: SSE flush passthrough
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
