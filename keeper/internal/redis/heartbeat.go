package redis

// Heartbeat cache for Soul agents in Redis (ADR-006(a) →
// docs/keeper/storage.md — role (a) Heartbeat cache).
//
// PG `souls.last_seen_at` / `last_seen_by_kid` is a flush snapshot;
// the real-time value lives here. On every app message on the
// EventStream (Hello / TaskEvent / RunResult / SoulprintReport) the
// EventStream handler writes [TouchHeartbeat]. The same handler
// flushes the PG snapshot (`souls.last_seen_at`) throttled — no more
// than once per `stale_after/3` per SID
// (keeper/internal/grpc/heartbeat_flush.go), otherwise the Reaper
// rule `mark_disconnected` would falsely mark a live stream as
// disconnected.
//
// The structure is a Hash `soul:<sid>:hb` with fields `at`
// (RFC3339Nano, UTC) and `kid`. A Hash rather than two separate keys
// so both fields update atomically in one command and read in one
// HGETALL on flush. No TTL is set: the record lives until an explicit
// DEL (Reaper rules `purge_souls` / `mark_disconnected`); a full
// Redis restart loses the data, and the flush snapshot from PG serves
// as a fallback until the next new message.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// HeartbeatKey is the Redis key of the heartbeat-cache Hash for a given SID.
func HeartbeatKey(sid string) string { return "soul:" + sid + ":hb" }

// heartbeatCapsField is the field of the `soul:<sid>:hb` Hash holding the set of
// Soul capabilities announced at connect time (ADR-056 §S5 forward-compat) — a
// sorted, deduplicated, comma-joined list. It lives in the same Hash as the
// heartbeat (not a separate key with its own TTL): the target resolver's presence
// filter already drops offline hosts by live SID lease BEFORE reading caps, so a
// dead host's stale caps don't target anyone until Reaper purge. On Hello, caps
// are ALWAYS written by overwrite (including empty) — otherwise an old binary
// reconnecting after a newer one would inherit a stale "passage" flag.
const heartbeatCapsField = "caps"

// TouchHeartbeat atomically updates the heartbeat cache for a SID. Writes
// `at = now` (UTC, RFC3339Nano) and `kid = <kid>` in a single HSET call.
//
// Does not return an error on conflict with a competing writer — last write
// wins (a Soul stream is served by exactly one Keeper at a time via
// [SoulLease], so there's normally no contention).
func TouchHeartbeat(ctx context.Context, c *Client, sid, kid string, now time.Time) error {
	if c == nil {
		return errors.New("redis.TouchHeartbeat: nil client")
	}
	if sid == "" {
		return errors.New("redis.TouchHeartbeat: empty sid")
	}
	if kid == "" {
		return errors.New("redis.TouchHeartbeat: empty kid")
	}
	if now.IsZero() {
		now = time.Now()
	}
	err := c.underlying().HSet(ctx, HeartbeatKey(sid),
		"at", now.UTC().Format(time.RFC3339Nano),
		"kid", kid,
	).Err()
	if err != nil {
		return fmt.Errorf("redis.TouchHeartbeat: HSET %q: %w", HeartbeatKey(sid), err)
	}
	return nil
}

// SetSoulCapabilities overwrites the SID's capability set in the heartbeat Hash
// (field [heartbeatCapsField]) with a single HSET. Called on Hello — ALWAYS,
// including with an empty set (explicit overwrite of a stale flag on an old
// binary's reconnect).
//
// caps is normalized (deduplicated, sorted, empties dropped) and serialized
// comma-joined. An empty set writes an empty string — [SoulHasCapability] treats
// it as "no capabilities" (fail-closed for feature-dependent runs).
func SetSoulCapabilities(ctx context.Context, c *Client, sid string, caps []string) error {
	if c == nil {
		return errors.New("redis.SetSoulCapabilities: nil client")
	}
	if sid == "" {
		return errors.New("redis.SetSoulCapabilities: empty sid")
	}
	err := c.underlying().HSet(ctx, HeartbeatKey(sid), heartbeatCapsField, joinCaps(caps)).Err()
	if err != nil {
		return fmt.Errorf("redis.SetSoulCapabilities: HSET %q: %w", HeartbeatKey(sid), err)
	}
	return nil
}

// SoulHasCapability reports whether the SID's active stream announced the given
// capability (ADR-056 §S5). Reads the [heartbeatCapsField] field for one SID
// (HGET).
//
// Missing key/field or an empty set → false (fail-closed): an old Soul without a
// capability announcement, or a host that never sent Hello, is treated as NOT
// supporting the feature. A network/protocol failure of HGET → error (the
// caller — the staged gate in run.go — must reject the run rather than guess
// support).
func SoulHasCapability(ctx context.Context, c *Client, sid, capability string) (bool, error) {
	if c == nil {
		return false, errors.New("redis.SoulHasCapability: nil client")
	}
	if sid == "" {
		return false, errors.New("redis.SoulHasCapability: empty sid")
	}
	v, err := c.underlying().HGet(ctx, HeartbeatKey(sid), heartbeatCapsField).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		return false, fmt.Errorf("redis.SoulHasCapability: HGET %q[%s]: %w", HeartbeatKey(sid), heartbeatCapsField, err)
	}
	for _, c := range strings.Split(v, ",") {
		if c == capability {
			return true, nil
		}
	}
	return false, nil
}

// SoulsLackingCapability is a batched check over a set of SIDs: returns the
// subset of SIDs that did NOT announce the given capability (ADR-056 §S5 staged
// gate). One Redis pipeline HGET per SID (O(1) round trips).
//
// A SID with a missing key/field or an empty set ends up in the result
// (fail-closed). Empty `sids` → empty result without hitting Redis. Pipeline
// error → the whole call fails (the caller rejects the staged run rather than
// guessing).
func SoulsLackingCapability(ctx context.Context, c *Client, sids []string, capability string) ([]string, error) {
	if c == nil {
		return nil, errors.New("redis.SoulsLackingCapability: nil client")
	}
	if len(sids) == 0 {
		return nil, nil
	}
	pipe := c.underlying().Pipeline()
	type pending struct {
		sid string
		cmd *redis.StringCmd
	}
	cmds := make([]pending, 0, len(sids))
	for _, sid := range sids {
		if sid == "" {
			continue
		}
		cmds = append(cmds, pending{sid: sid, cmd: pipe.HGet(ctx, HeartbeatKey(sid), heartbeatCapsField)})
	}
	if len(cmds) == 0 {
		return nil, nil
	}
	// Pipeline.Exec returns the error of the first failed command; redis.Nil
	// (missing field) is NOT a reason to fail the whole batch — handled per-cmd.
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("redis.SoulsLackingCapability: pipeline EXEC: %w", err)
	}
	var lacking []string
	for _, p := range cmds {
		v, err := p.cmd.Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				lacking = append(lacking, p.sid) // missing key/field → fail-closed.
				continue
			}
			return nil, fmt.Errorf("redis.SoulsLackingCapability: HGET %q: %w", HeartbeatKey(p.sid), err)
		}
		if !capsContain(v, capability) {
			lacking = append(lacking, p.sid)
		}
	}
	return lacking, nil
}

// joinCaps normalizes a capability set into a stable comma-joined string
// (deduplicated, sorted, empties dropped).
func joinCaps(caps []string) string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// capsContain reports whether the capability is present in the comma-joined string.
func capsContain(joined, capability string) bool {
	for _, c := range strings.Split(joined, ",") {
		if c == capability {
			return true
		}
	}
	return false
}

// ReadHeartbeat returns the latest value from the cache. Useful for the
// Operator API / diagnostics; the main consumer is the future batch flush.
//
// Returns (time.Time{}, "", false, nil) if the key is missing —
// "no heartbeat yet", not an error.
func ReadHeartbeat(ctx context.Context, c *Client, sid string) (at time.Time, kid string, ok bool, err error) {
	if c == nil {
		return time.Time{}, "", false, errors.New("redis.ReadHeartbeat: nil client")
	}
	res, err := c.underlying().HGetAll(ctx, HeartbeatKey(sid)).Result()
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("redis.ReadHeartbeat: HGETALL %q: %w", HeartbeatKey(sid), err)
	}
	if len(res) == 0 {
		return time.Time{}, "", false, nil
	}
	atStr := res["at"]
	kid = res["kid"]
	if atStr == "" || kid == "" {
		// Corrupt record (partially written). Treat as absent.
		return time.Time{}, "", false, nil
	}
	at, err = time.Parse(time.RFC3339Nano, atStr)
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("redis.ReadHeartbeat: parse at %q: %w", atStr, err)
	}
	return at, kid, true, nil
}
