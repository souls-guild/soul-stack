package redis

// Toll cluster-detector Redis primitives (ADR-038). Thin helpers — the
// single place for all Redis ops of the Toll infrastructure (the pattern of
// heartbeat.go / soullease.go / conclave.go): the `toll` package consumes
// them through narrow interfaces (toll.Publisher / toll.DegradedReader /
// ...), doesn't pull in *redis.Client directly.
//
// Keys: see the toll package's doc-comments — SortedSetKey
// ("toll:disconnects"), LeaseKey ("cluster:toll:leader"), DegradedKey
// ("cluster:degraded"). They're deliberately duplicated as strings here:
// keeperredis is the low-level layer, it must not import toll (the
// direction is toll → keeperredis). Any mismatch is caught by the `toll`
// package's tests (integration).

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	tollSortedSetKey = "toll:disconnects"
	tollDegradedKey  = "cluster:degraded"
)

// PublishTollDisconnect — ZADD of a single disconnect event into the
// `toll:disconnects` sorted set (score=unix-seconds, value=member). The
// caller (toll.Watcher via an adapter) builds the member via
// toll.EncodeDisconnect. Idempotency isn't guaranteed (for member
// uniqueness, EncodeDisconnect adds a UnixNano suffix).
func PublishTollDisconnect(ctx context.Context, c *Client, member string, atUnix int64) error {
	if c == nil {
		return fmt.Errorf("redis.PublishTollDisconnect: nil client")
	}
	if member == "" {
		return fmt.Errorf("redis.PublishTollDisconnect: empty member")
	}
	if err := c.underlying().ZAdd(ctx, tollSortedSetKey,
		redis.Z{Score: float64(atUnix), Member: member},
	).Err(); err != nil {
		return fmt.Errorf("redis.PublishTollDisconnect: ZADD %q: %w", tollSortedSetKey, err)
	}
	return nil
}

// TollCountInWindow — ZCOUNT over the sorted set for range [fromUnix,
// toUnix]. The caller (toll.Leader via an adapter) computes the rate.
func TollCountInWindow(ctx context.Context, c *Client, fromUnix, toUnix int64) (int64, error) {
	if c == nil {
		return 0, fmt.Errorf("redis.TollCountInWindow: nil client")
	}
	n, err := c.underlying().ZCount(ctx, tollSortedSetKey,
		fmt.Sprintf("%d", fromUnix),
		fmt.Sprintf("%d", toUnix),
	).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.TollCountInWindow: ZCOUNT %q: %w", tollSortedSetKey, err)
	}
	return n, nil
}

// TollCountByCovenInWindow — ZRANGEBYSCORE over the range + group-by coven
// (ADR-038 amendment 2026-05-27, per-coven thresholds).
//
// Member-value `<sid>|<kid>|<coven>|<nano>` (see toll.EncodeDisconnect):
// extract coven from the 3rd `|`-segment. Invalid/too-short members are
// skipped (defensive: tolerate old-format Redis data after a rolling
// upgrade without crashing).
//
// Returns map[coven]count; an empty coven lands under the "" key (the
// Watcher allows an empty coven label). On an empty window — an empty map,
// no error.
func TollCountByCovenInWindow(ctx context.Context, c *Client, fromUnix, toUnix int64) (map[string]int64, error) {
	if c == nil {
		return nil, fmt.Errorf("redis.TollCountByCovenInWindow: nil client")
	}
	members, err := c.underlying().ZRangeByScore(ctx, tollSortedSetKey, &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", fromUnix),
		Max: fmt.Sprintf("%d", toUnix),
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("redis.TollCountByCovenInWindow: ZRANGEBYSCORE %q: %w", tollSortedSetKey, err)
	}
	counts := make(map[string]int64, len(members))
	for _, m := range members {
		coven, ok := extractCovenFromMember(m)
		if !ok {
			continue
		}
		counts[coven]++
	}
	return counts, nil
}

// extractCovenFromMember parses the member-value
// `<sid>|<kid>|<coven>|<nano>` and returns the 3rd segment. ok=false with <
// 3 segments (invalid/truncated member).
func extractCovenFromMember(m string) (string, bool) {
	// Find the 1st `|` → start of kid.
	i1 := indexByte(m, '|')
	if i1 < 0 {
		return "", false
	}
	rest := m[i1+1:]
	// 2nd `|` → start of coven.
	i2 := indexByte(rest, '|')
	if i2 < 0 {
		return "", false
	}
	rest = rest[i2+1:]
	// 3rd `|` → end of coven (always present: EncodeDisconnect adds a nano suffix).
	i3 := indexByte(rest, '|')
	if i3 < 0 {
		return "", false
	}
	return rest[:i3], true
}

// indexByte — a local analog of strings.IndexByte without importing strings
// for a single function. Inline-friendly.
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// TollTrimBelow — ZREMRANGEBYSCORE removes everything with score <
// beforeUnix. Idempotent (returns 0 without error on an empty selection).
// The caller (Leader) trims the window tail on every tick.
func TollTrimBelow(ctx context.Context, c *Client, beforeUnix int64) error {
	if c == nil {
		return fmt.Errorf("redis.TollTrimBelow: nil client")
	}
	if err := c.underlying().ZRemRangeByScore(ctx, tollSortedSetKey,
		"-inf",
		fmt.Sprintf("(%d", beforeUnix),
	).Err(); err != nil {
		return fmt.Errorf("redis.TollTrimBelow: ZREMRANGEBYSCORE %q: %w", tollSortedSetKey, err)
	}
	return nil
}

// TollSetDegraded — `SET cluster:degraded <holder> EX <ttl>`. Not NX — the
// Leader refreshes the TTL on every tick of its own (re-arm). Holder is for
// diagnosing "which instance raised the flag".
func TollSetDegraded(ctx context.Context, c *Client, holder string, ttl time.Duration) error {
	if c == nil {
		return fmt.Errorf("redis.TollSetDegraded: nil client")
	}
	if holder == "" {
		return fmt.Errorf("redis.TollSetDegraded: empty holder")
	}
	if ttl <= 0 {
		return fmt.Errorf("redis.TollSetDegraded: ttl must be > 0, got %v", ttl)
	}
	if err := c.underlying().Set(ctx, tollDegradedKey, holder, ttl).Err(); err != nil {
		return fmt.Errorf("redis.TollSetDegraded: SET %q: %w", tollDegradedKey, err)
	}
	return nil
}

// TollClearDegraded — `DEL cluster:degraded`. Idempotent.
func TollClearDegraded(ctx context.Context, c *Client) error {
	if c == nil {
		return fmt.Errorf("redis.TollClearDegraded: nil client")
	}
	if err := c.underlying().Del(ctx, tollDegradedKey).Err(); err != nil {
		return fmt.Errorf("redis.TollClearDegraded: DEL %q: %w", tollDegradedKey, err)
	}
	return nil
}

// TollIsDegraded — EXISTS cluster:degraded. true = the flag is set, false =
// it isn't. EXISTS is cheaper than GET — the value isn't needed (the flag
// is binary).
func TollIsDegraded(ctx context.Context, c *Client) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("redis.TollIsDegraded: nil client")
	}
	n, err := c.underlying().Exists(ctx, tollDegradedKey).Result()
	if err != nil {
		return false, fmt.Errorf("redis.TollIsDegraded: EXISTS %q: %w", tollDegradedKey, err)
	}
	return n == 1, nil
}
