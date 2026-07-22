package redis

// Lease — Redis-based leadership for Keeper background tasks
// (ADR-006(d), Reaper loop in M0.6+). Algorithm:
//
//   - Acquire — `SET key holder NX EX ttl`. If the key is already held
//     by another holder — [ErrLeaseTaken]; the caller retries itself
//     with backoff.
//   - Renew — Lua-script CAS: if `GET key == holder`, refresh the TTL
//     via `PEXPIRE`; otherwise [ErrLeaseLost] (someone else took over).
//   - Release — Lua-script CAS: `DEL key`, only if the holder matches.
//     On `not-mine` it exits silently (idempotent stop).
//
// The holder string is the Keeper instance's `kid` (see
// shared/config/keeper.go::KID), the caller passes it in. This gives
// human-readable logs like "lease acquired by keeper-eu-west-01", and
// lets the Reaper distinguish leadership changes between instances of
// the same cluster in the future.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrLeaseTaken — [Acquire] didn't get the lease, the key is held by another
// holder. The caller (Reaper runner) retries with backoff.
var ErrLeaseTaken = errors.New("redis: lease already taken")

// ErrLeaseLost — [Lease.Renew] found that the key belongs to another
// holder (or expired and was reacquired). The caller must stop the
// loop — split-brain is not acceptable.
var ErrLeaseLost = errors.New("redis: lease lost (no longer leader)")

// Lease is a handle to a held key.
//
// Lease is NOT thread-safe with respect to Release vs Renew: the caller must
// order the calls itself (typically one renewal goroutine + one Release from
// a main defer). Concurrent Renew calls are safe among themselves (Redis
// serializes commands), but there's no point in doing that.
type Lease struct {
	client *Client
	key    string
	holder string
	ttl    time.Duration
}

// Key is the Redis key the lease holds. Useful for logs.
func (l *Lease) Key() string { return l.key }

// Holder is the value written into the key. Useful for logs.
func (l *Lease) Holder() string { return l.holder }

// TTL is the key's current target TTL. Renew extends it to this value.
func (l *Lease) TTL() time.Duration { return l.ttl }

// Acquire tries to acquire the lease. On success returns a handle; on
// conflict — [ErrLeaseTaken]; on a network/protocol error — a wrapped err.
//
// `ttl` must be > 0 — a negative/zero value would mean either an
// instantly-expiring lease (a race window) or an infinite one (Redis
// `SET ... EX 0` is an error). Either way it's a caller bug.
func Acquire(ctx context.Context, c *Client, key, holder string, ttl time.Duration) (*Lease, error) {
	if c == nil {
		return nil, errors.New("redis.Acquire: nil client")
	}
	if key == "" {
		return nil, errors.New("redis.Acquire: empty key")
	}
	if holder == "" {
		return nil, errors.New("redis.Acquire: empty holder")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("redis.Acquire: ttl must be > 0, got %v", ttl)
	}

	ok, err := c.underlying().SetNX(ctx, key, holder, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("redis.Acquire: SETNX %q: %w", key, err)
	}
	if !ok {
		return nil, ErrLeaseTaken
	}
	return &Lease{client: c, key: key, holder: holder, ttl: ttl}, nil
}

// PeekLeaseHolder reads the lease key's current holder (value = holder's KID)
// WITHOUT acquiring it — a plain `GET`. Returns (holder, true) if the key is
// alive; (_, false) if the lease is free / expired (`redis.Nil`).
//
// A read-only inspection for observability (`GET /v1/cluster` → who's
// currently the Reaper leader for the reaper.LeaderLeaseKey key). Does NOT
// participate in the Acquire/Renew/Release CAS protocol: it only shows who
// owns the key right now. The value is ephemeral (lease under TTL) — the
// caller should treat the answer as a point-in-time snapshot.
func PeekLeaseHolder(ctx context.Context, c *Client, key string) (string, bool, error) {
	if c == nil {
		return "", false, errors.New("redis.PeekLeaseHolder: nil client")
	}
	if key == "" {
		return "", false, errors.New("redis.PeekLeaseHolder: empty key")
	}
	v, err := c.underlying().Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("redis.PeekLeaseHolder: GET %q: %w", key, err)
	}
	return v, true, nil
}

// renewScript is CAS-renew: returns 1 if the key is still ours and the TTL
// was refreshed, otherwise 0. PEXPIRE applies to an existing key — the
// atomicity of GET+PEXPIRE is guaranteed by the Lua script executing
// atomically inside Redis.
//
// PEXPIRE (milliseconds) is chosen over EXPIRE (seconds) so that sub-second
// lock_ttl values in tests (typical miniredis tests use 50-200 ms) work
// precisely; production doesn't need sub-second precision, but the overhead
// is zero.
var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
else
	return 0
end
`)

// Renew extends the key's TTL to [Lease.TTL] (via PEXPIRE), but only if the
// value still equals [Lease.Holder]. On a holder mismatch — [ErrLeaseLost];
// if the key is already gone (expired and not recreated) — also
// [ErrLeaseLost] (CAS returns 0 because GET returned nil).
func (l *Lease) Renew(ctx context.Context) error {
	if l == nil || l.client == nil {
		return errors.New("redis.Lease.Renew: nil lease/client")
	}
	res, err := renewScript.Run(ctx, l.client.underlying(),
		[]string{l.key},
		l.holder,
		l.ttl.Milliseconds(),
	).Int64()
	if err != nil {
		return fmt.Errorf("redis.Lease.Renew %q: %w", l.key, err)
	}
	if res != 1 {
		return ErrLeaseLost
	}
	return nil
}

// releaseScript is CAS-delete: delete the key only if the value is ours.
// Returns the number of keys deleted (0 or 1).
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
else
	return 0
end
`)

// Release deletes the key if it still belongs to us. On a foreign holder
// or an expired key — no-op (idempotent). A network error is propagated,
// but the caller usually ignores it — Release is called from a
// defer-shutdown, where Redis may already be unreachable.
//
// After Release, a repeat Renew/Release is safe — it returns [ErrLeaseLost]
// or no-op respectively (Redis-state-driven, no flag in the Go struct).
func (l *Lease) Release(ctx context.Context) error {
	if l == nil || l.client == nil {
		return errors.New("redis.Lease.Release: nil lease/client")
	}
	_, err := releaseScript.Run(ctx, l.client.underlying(),
		[]string{l.key},
		l.holder,
	).Int64()
	if err != nil {
		return fmt.Errorf("redis.Lease.Release %q: %w", l.key, err)
	}
	return nil
}
