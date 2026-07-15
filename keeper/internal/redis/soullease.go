package redis

// SoulLease coordinates "which Keeper instance holds the active EventStream
// to this Soul" via a Redis lease (ADR-006(b) → [storage.md → Lease on SID]).
//
// The key is `soul:<sid>:lock`, the value is the Keeper's `kid`; the TTL is
// renewed by a renewal goroutine (pattern identical to the Reaper).
//
// Leadership semantics for one Soul stream:
//   - One Keeper holds the lease at a time → accepts the EventStream.
//   - A competing Keeper trying to accept a stream for the same SID gets
//     [ErrLeaseTaken]; the handler closes the stream with `code.AlreadyExists`.
//   - On graceful shutdown, the Keeper does Release; the next Keeper freely
//     takes over on the Soul's next reconnect.
//   - On Keeper crash: the TTL expires, and the Soul's next reconnect
//     acquires the lease on a new Keeper.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// SoulStreamAlive reports whether any Keeper instance holds a live
// EventStream to this SID — based on the presence of the SID lease key
// `soul:<sid>:lock`.
//
// The lease (NOT the heartbeat hash) is the authoritative signal of a live
// stream: the key only exists while the handler's renewal goroutine keeps
// renewing it (TTL [defaultSoulLeaseTTL]), and disappears on a graceful
// Release or when the TTL expires after an instance crash. The heartbeat
// hash (`soul:<sid>:hb`) doesn't work for this check — it has no TTL (see
// heartbeat.go) and would outlive a long-closed stream, giving a
// false-alive answer.
//
// Purpose: the lease-aware Reaper rule `mark_disconnected` — an idle Soul
// on a live stream (sending only soulprint once per refresh_interval, with
// a stale PG `last_seen_at`) shouldn't be falsely marked `disconnected`
// while its stream still holds the lease (ADR-006(a)).
func SoulStreamAlive(ctx context.Context, c *Client, sid string) (bool, error) {
	if c == nil {
		return false, errors.New("redis.SoulStreamAlive: nil client")
	}
	if sid == "" {
		return false, errors.New("redis.SoulStreamAlive: empty sid")
	}
	n, err := c.underlying().Exists(ctx, SoulLeaseKey(sid)).Result()
	if err != nil {
		return false, fmt.Errorf("redis.SoulStreamAlive: EXISTS %q: %w", SoulLeaseKey(sid), err)
	}
	return n > 0, nil
}

// SoulsStreamAlive is the batched variant of [SoulStreamAlive] for a set of
// SIDs (the target resolver's presence filter, ADR-006(a)). Returns the set
// of SIDs with a live lease key `soul:<sid>:lock` (EXISTS).
//
// Implementation: one Redis pipeline with an EXISTS command per SID —
// O(1) round trips instead of O(N) sequential EXISTS calls. The resolver
// targets hosts within a SINGLE incarnation (tens to hundreds), so the
// pipeline is cheap; a separate Redis Set of live SIDs (Variant B) isn't
// introduced — it would be a redundant source of truth alongside the lease
// key.
//
// Empty `sids` → empty result without hitting Redis. nil elements / empty
// strings in `sids` are skipped (they don't form a valid lease key). A
// pipeline error → the whole call returns the error (the caller — the
// resolver — degrades fail-safe to SQL presence, see topology.Resolver).
func SoulsStreamAlive(ctx context.Context, c *Client, sids []string) (map[string]struct{}, error) {
	if c == nil {
		return nil, errors.New("redis.SoulsStreamAlive: nil client")
	}
	alive := make(map[string]struct{}, len(sids))
	if len(sids) == 0 {
		return alive, nil
	}

	pipe := c.underlying().Pipeline()
	// Parallel slice (sid ↔ its *IntCmd): after Exec, read each command's
	// result by index. Empty SIDs are skipped — no command is queued for
	// them, no index is reserved.
	type pending struct {
		sid string
		cmd *redis.IntCmd
	}
	cmds := make([]pending, 0, len(sids))
	for _, sid := range sids {
		if sid == "" {
			continue
		}
		cmds = append(cmds, pending{sid: sid, cmd: pipe.Exists(ctx, SoulLeaseKey(sid))})
	}
	if len(cmds) == 0 {
		return alive, nil
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("redis.SoulsStreamAlive: pipeline EXEC: %w", err)
	}
	for _, p := range cmds {
		n, err := p.cmd.Result()
		if err != nil {
			return nil, fmt.Errorf("redis.SoulsStreamAlive: EXISTS %q: %w", SoulLeaseKey(p.sid), err)
		}
		if n > 0 {
			alive[p.sid] = struct{}{}
		}
	}
	return alive, nil
}

// SoulLeaseOwner returns the KID of the Keeper instance holding the active
// EventStream to this SID — the value of the lease key `soul:<sid>:lock`
// (GET).
//
// Unlike [SoulStreamAlive] (EXISTS — is there any owner at all), this needs
// the actual OWNER: the multi-keeper-guard run-goroutine path
// (acolytes=0) compares it against its own KID. If SendApply targets a
// Soul whose stream is held by ANOTHER Keeper instance, the RunResult goes
// there while the owning run hangs here in applying (a footgun, fixable
// only by moving to acolytes>0 / a work queue, ADR-027).
//
// Return: ok=false with no error — there's no lease key (the Soul isn't on
// anyone's stream: EXISTS would return false, redis.Nil isn't an error
// here). ok=true + kid — the owner is known. An error only on a
// network/protocol failure of GET (the caller — the guard — degrades
// silently: no warn is printed on a check failure).
func SoulLeaseOwner(ctx context.Context, c *Client, sid string) (kid string, ok bool, err error) {
	if c == nil {
		return "", false, errors.New("redis.SoulLeaseOwner: nil client")
	}
	if sid == "" {
		return "", false, errors.New("redis.SoulLeaseOwner: empty sid")
	}
	v, gerr := c.underlying().Get(ctx, SoulLeaseKey(sid)).Result()
	if gerr != nil {
		if errors.Is(gerr, redis.Nil) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("redis.SoulLeaseOwner: GET %q: %w", SoulLeaseKey(sid), gerr)
	}
	return v, true, nil
}

// SoulLeaseKey builds the Redis lease key for a given SID.
//
// The `soul:<sid>:lock` convention is fixed in docs/keeper/storage.md
// (Redis — hot layer, role (b) Lease on SID).
func SoulLeaseKey(sid string) string {
	return "soul:" + sid + ":lock"
}

// AcquireSoulLease is a wrapper around [Acquire] with a fixed key prefix.
// Parameters mirror the Reaper's use case:
//
//   - sid — the Soul's SID (FQDN), used to build the key.
//   - kid — the Keeper instance identifier, written as the value.
//   - ttl — how long the key lives without a Renew. The renewal goroutine
//     must renew more often than ttl (typically ttl/3).
//
// Returns [ErrLeaseTaken] on conflict; the caller (the EventStream handler)
// closes the stream with `code.AlreadyExists`.
func AcquireSoulLease(ctx context.Context, c *Client, sid, kid string, ttl time.Duration) (*Lease, error) {
	return Acquire(ctx, c, SoulLeaseKey(sid), kid, ttl)
}

// forceAcquireSoulLeaseScript is a CAS-by-prev-holder re-acquire of the
// SID lease. Re-acquires the key for `newKID` (ARGV[2]) only in two cases:
//   - the key still belongs to the PROVEN-DEAD `prevKID` (ARGV[1]) — DEL+SET;
//   - the key is absent (the prev-holder already TTL-expired) — SETNX.
//
// If the key raced to a THIRD owner (another Keeper already re-acquired it
// / the Soul reconnected somewhere else) — leave it alone (return 0). This
// is exactly the split-brain protection: a blind DEL of someone else's live
// lease would break SID-lease delivery dedup (ADR-027(n)); this only lifts
// the lease from the holder whose death the caller has already confirmed
// via a presence check ([InstanceAlive]).
//
// PX (milliseconds) — as in renewScript: precise sub-second TTL for
// miniredis tests, zero overhead in production.
var forceAcquireSoulLeaseScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if cur == false then
	if redis.call("SET", KEYS[1], ARGV[2], "NX", "PX", ARGV[3]) then
		return 1
	end
	return 0
elseif cur == ARGV[1] then
	redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[3])
	return 1
else
	return 0
end
`)

// ForceAcquireSoulLease is a presence-gated re-acquire of the SID lease
// from a proven-dead prev-holder (ADR-027(n), server-side foundation S0;
// wiring into the acquireSoulLease handler is S2).
//
// Contract: the caller invokes THIS only after:
//   - a plain [AcquireSoulLease] returned [ErrLeaseTaken];
//   - [SoulLeaseOwner] gave `prevKID` — the key's current owner;
//   - [InstanceAlive](prevKID) confirmed the prev-holder is DEAD in the
//     Conclave.
//
// The re-acquire is atomic (Lua CAS-by-prev-holder): the key is overwritten
// to `newKID` ONLY if it's still `== prevKID` (or absent — plain SETNX). If
// the key raced to a third owner between the presence check and this call,
// [ErrLeaseTaken] is returned (we do NOT re-acquire someone else's live
// lease).
//
// On success → a `*Lease` for the new holder, ready for Renew/Release (the
// handler's renewal goroutine renews it like any normal AcquireSoulLease lease).
func ForceAcquireSoulLease(ctx context.Context, c *Client, sid, prevKID, newKID string, ttl time.Duration) (*Lease, error) {
	if c == nil {
		return nil, errors.New("redis.ForceAcquireSoulLease: nil client")
	}
	if sid == "" {
		return nil, errors.New("redis.ForceAcquireSoulLease: empty sid")
	}
	if prevKID == "" {
		return nil, errors.New("redis.ForceAcquireSoulLease: empty prevKID")
	}
	if newKID == "" {
		return nil, errors.New("redis.ForceAcquireSoulLease: empty newKID")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("redis.ForceAcquireSoulLease: ttl must be > 0, got %v", ttl)
	}
	key := SoulLeaseKey(sid)
	res, err := forceAcquireSoulLeaseScript.Run(ctx, c.underlying(),
		[]string{key},
		prevKID, newKID, ttl.Milliseconds(),
	).Int64()
	if err != nil {
		return nil, fmt.Errorf("redis.ForceAcquireSoulLease %q: %w", key, err)
	}
	if res != 1 {
		return nil, ErrLeaseTaken
	}
	return &Lease{client: c, key: key, holder: newKID, ttl: ttl}, nil
}
