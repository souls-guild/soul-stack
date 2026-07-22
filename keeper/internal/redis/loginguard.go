package redis

// LoginGuard is an anti-bruteforce primitive for public login endpoints
// (ADR-058(g), HIGH-3). Two mechanisms on top of Redis, both cluster-shared
// (authority lives in Redis, not duplicated ×N across Keeper's stateless HA
// instances, ADR-002/ADR-006):
//
//  1. RATE THROTTLE (token bucket) — limits the NUMBER OF ATTEMPTS from one
//     principal (IP or username) per unit of time. Taken BEFORE processing the
//     request, on EVERY attempt. Damps flooding (including flood flow-state on
//     /auth/oidc/login — every login start spends a token). Same algorithm as
//     the Tempo bucket (ratelimit.go), but a separate `authrl:` key prefix and
//     principal = IP/username, not AID (login is pre-JWT, there's no AID yet).
//
//  2. FAILURE LOCKOUT — a counter of FAILED logins in a sliding window; once
//     the threshold is reached, the principal is locked out for a backoff
//     interval (independently for IP and for username). RecordFailure
//     increments the counter AFTER a failure; Locked/RetryAfter checks the
//     lockout BEFORE processing. A successful login does NOT touch the
//     counter (provisioning/role change isn't a failure); the counter expires
//     on its own via the window TTL. Anti-bruteforce: password guessing and
//     username enumeration are both fended off.
//
// Fail-closed on the Lockout check: on a Redis error, Locked returns an
// error, and the middleware decides the policy (HIGH-3 requires fail-closed
// on login — unlike Tempo's fail-open: login is a security perimeter,
// Redis unavailability must NOT open the door to bruteforce). The throttle
// part (Allow) is left to the middleware's discretion on a Redis error; the
// implementation only signals the error.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// LoginGuard is a handle for anti-bruteforce operations on login endpoints.
// Stateless with respect to a specific principal: the key is passed on every
// call.
type LoginGuard struct {
	client *Client
}

// NewLoginGuard wraps a Redis client. Errors only on a nil client
// (wiring: the middleware gets a nil guard when Redis is absent).
func NewLoginGuard(c *Client) (*LoginGuard, error) {
	if c == nil {
		return nil, errors.New("redis.NewLoginGuard: nil client")
	}
	return &LoginGuard{client: c}, nil
}

// throttleKey is the attempt-throttle token bucket's key.
// `authrl:<scope>:<principal>` (scope = "ip"|"user", principal =
// IP address/username). A separate prefix from `tempo:` — the auth throttle
// lives independently of operation Tempo quotas.
func throttleKey(scope, principal string) string {
	return "authrl:" + scope + ":" + principal
}

// lockoutCountKey is the failure counter's key. `authlock:<scope>:<principal>:n`.
func lockoutCountKey(scope, principal string) string {
	return "authlock:" + scope + ":" + principal + ":n"
}

// lockoutFlagKey is the lockout flag's key. `authlock:<scope>:<principal>:locked`.
func lockoutFlagKey(scope, principal string) string {
	return "authlock:" + scope + ":" + principal + ":locked"
}

// Allow atomically takes one attempt-throttle token for the principal
// (scope+principal). Algorithm/contract are identical to [TokenBucket.Allow]
// (the same Lua refill+take), but with a separate key prefix. allowed=false →
// retryAfter until the token refills.
//
// The caller (middleware) can degrade on this branch by its own policy; the
// HIGH-3 recommendation is that fail-closed for login is required only on
// LOCKOUT — the throttle allows fail-open on a Redis flap (keeping the login
// page available).
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

// Locked reports whether the principal is currently locked out (the flag key
// exists), and how long until the lockout is lifted (PTTL). Checked BEFORE
// processing the login.
//
// Fail-closed: on a Redis error it returns err — the caller on the login
// perimeter MUST treat the error as "locked" (HIGH-3: Redis unavailability
// must not open the door to bruteforce).
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
	// PTTL: -2 = no key (not locked), -1 = no TTL (shouldn't happen — the flag
	// is always set with PEXPIRE), >0 = locked, ttl remaining.
	if ttl < 0 {
		return false, 0, nil
	}
	return true, ttl, nil
}

// recordFailureScript atomically increments the principal's failure counter
// and, once the threshold is reached, sets the lockout flag with a
// backoff TTL.
//
// KEYS[1] — failure counter (authlock:<...>:n).
// KEYS[2] — lockout flag (authlock:<...>:locked).
// ARGV[1] — threshold (failure count that triggers lockout).
// ARGV[2] — windowMs (counter window TTL, ms).
// ARGV[3] — lockoutMs (lockout flag backoff TTL, ms).
//
// Returns {count, locked}: count is the current failure count, locked=1 if
// the threshold was reached and the flag was set. The counter slides with the
// window TTL (each failure extends the window — observable as "N failures per
// window"). When the flag is set, the counter is reset (DEL), so once the
// lockout is lifted the threshold is counted fresh, instead of locking
// forever off one old streak.
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

// RecordFailure increments the principal's failure counter after a failed
// login; once threshold is reached it sets the lockout flag for the lockout
// interval. The counter's window is window. Returns whether this failure
// triggered a lockout.
//
// RecordFailure is NOT called on a successful login — the counter expires on
// its own via the window TTL (no explicit reset needed: failure streaks decay
// naturally).
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
