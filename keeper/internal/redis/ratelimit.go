package redis

// TokenBucket is a per-AID rate limiter for Tempo (ADR-050) on top of Redis.
//
// The algorithm is a classic token bucket whose state lives in a Redis hash
// `tempo:<aid>:<bucket>` (fields `tokens` / `last_refill_ts`). Refill+take is
// atomic via ONE Lua script (a read-modify-write of the bucket in a single
// round trip), which is what gives a coherent limit across the stateless HA
// Keeper cluster: the limit is authoritative in Redis rather than multiplied
// ×N across instances (ADR-050(a)).
//
// Style follows its neighbors [Lease] (lease.go) / SoulLease (soullease.go):
// all atomic logic lives in the Lua script, the Go wrapper only builds the
// key, passes arguments, and interprets the result.
//
// The bucket reads time FROM REDIS ITSELF (`redis.call("TIME")`), not from
// the caller's Go clock: otherwise refill would depend on clock skew between
// N Keeper instances, and the bucket would "drift" across requests hitting
// different instances. TIME is the single source for all takes on one
// bucket. NB: `redis.call("TIME")` is non-deterministic, so the script
// requires Redis >= 5 (effects-replication: replica/AOF record the
// HSET/PEXPIRE effects, not the script itself).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// TokenBucket is a handle for take operations against token buckets in
// Redis. Stateless with respect to any particular bucket: the key is passed
// to every [Allow] call, and one TokenBucket serves any number of
// AID/bucket combinations.
type TokenBucket struct {
	client *Client
}

// NewTokenBucket wraps a Redis client as a Tempo rate limiter. Only errors
// on a nil client — that's a wire-up bug (when Redis is absent, middleware
// gets a nil limiter and runs passthrough, see api/middleware/ratelimit.go,
// so a nil client shouldn't reach here).
func NewTokenBucket(c *Client) (*TokenBucket, error) {
	if c == nil {
		return nil, errors.New("redis.NewTokenBucket: nil client")
	}
	return &TokenBucket{client: c}, nil
}

// tokenBucketKey builds the bucket's Redis key. The `tempo:<aid>:<bucket>`
// convention is fixed by canon (ADR-050(a), naming-rules.md): per-AID,
// per-logical-endpoint bucket.
func tokenBucketKey(aid, bucket string) string {
	return "tempo:" + aid + ":" + bucket
}

// allowScript is the atomic refill+take of a token bucket.
//
// KEYS[1] — the bucket key (`tempo:<aid>:<bucket>`).
// ARGV[1] — rate (tokens per second, float).
// ARGV[2] — burst (bucket capacity, integer > 0).
// ARGV[3] — key TTL in milliseconds (PEXPIRE, slides on every take).
//
// Returns an array `{allowed, retry_after_ms}`:
//   - allowed=1, retry_after_ms=0      — token taken, request passes;
//   - allowed=0, retry_after_ms=<ms>   — bucket empty, retry_after_ms
//     milliseconds left until at least one token refills.
//
// Time comes from `redis.call("TIME")` (see the package doc comment). The
// fractional part of tokens is stored in the hash as a float string:
// otherwise a high-rate-low-burst scenario would lose accumulation between
// takes (rounding to zero every time).
//
// The first request against a nonexistent bucket is treated as a full
// bucket (tokens = burst): a new operator isn't penalized by a "cold" bucket.
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
  -- Cold bucket - start full.
  tokens = burst
  last_ms = now_ms
end

-- Refill: over the elapsed time accumulate rate*dt tokens, but no more than burst.
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
  -- Short by (1 - tokens) tokens; at rate tokens/sec the wait is
  -- (1 - tokens)/rate seconds. rate > 0 is guaranteed by Go validation.
  retry_after_ms = math.ceil(((1 - tokens) / rate) * 1000)
end

redis.call("HSET", KEYS[1], "tokens", tostring(tokens), "last_refill_ts", tostring(now_ms))
redis.call("PEXPIRE", KEYS[1], ttl_ms)

return {allowed, retry_after_ms}
`)

// bucketTTL is how long it takes a fully empty bucket to refill to
// capacity: burst/rate seconds plus margin. No point holding the key longer
// (the bucket would already be full), but cutting it shorter would let an
// active limit be "forgotten" between bursts. Returned in milliseconds for
// PEXPIRE.
func bucketTTL(rate float64, burst int) time.Duration {
	refillSeconds := float64(burst) / rate
	// Double + 1s rounding safety margin — TTL isn't precision-critical, it
	// just needs to outlive the bucket's actual usage window.
	ttl := time.Duration((refillSeconds*2)+1) * time.Second
	return ttl
}

// Allow atomically tries to take one token from the `tempo:<aid>:<bucket>`
// bucket.
//
//   - allowed=true             — token taken, request can pass;
//     retryAfter == 0.
//   - allowed=false            — bucket empty; retryAfter is the time until
//     at least one token refills (for the HTTP Retry-After header).
//
// `rate` is tokens per second (> 0), `burst` is bucket capacity (> 0).
// Invalid arguments are a caller bug (config is validated upstream); here
// they're rejected with an explicit error rather than silently.
//
// An error is returned only on a Redis network/protocol failure or invalid
// arguments. On a Redis error, the caller (middleware) degrades fail-OPEN
// (passthrough, ADR-050(b)) — Allow itself doesn't decide to fail open, it
// just signals the error.
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
