// Package toll — cluster-wide detector of mass Soul churn (ADR-038).
//
// Architecture (ADR-038(2)):
//
//   - [Watcher] — per-Keeper-instance goroutine source: gRPC EventStream handler
//     calls [Watcher.NotifyDisconnect] on receive-loop exit. Watcher
//     filters graceful-shutdown / warmup-immunity and publishes surviving
//     disconnect-event to common Redis sorted-set (via [Publisher]).
//   - [Publisher] — thin ZADD-adapter over *redis.Client (sorted-set
//     `toll:disconnects`, score = unix-timestamp, value = `<sid>|<kid>|<coven>`).
//     Disconnect events published FROM ALL instances to common key.
//   - [Leader] — background goroutine winning Redis-lease `cluster:toll:leader`
//     (similar to Reaper). Periodically reads sorted-set over window, compares
//     with baseline `souls.status='connected'` (cached), sets/clears
//     Redis key `cluster:degraded` with asymmetric hysteresis.
//   - [DegradedReader] — read-only surface for HTTP-middleware: checks
//     presence of key `cluster:degraded` on each write-API request.
//   - [DegradedMiddleware] — chi-compatible middleware: blocks POST
//     scenarios/push-apply when degraded (503 + Retry-After), passes
//     everything else.
//
// Invariant ADR-038(c): Toll is passive observer. Does not close streams (that is
// Watchman's job), does not perform recovery actions, only observes + blocks
// write-API.
package toll

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// SortedSetKey — Redis sorted-set to which all per-instance Watchers
// publish disconnect events. score = unix-seconds, value = `<sid>|<kid>|<coven>`.
// Coven optional (Watcher may not know it on cleanup-handler — empty
// segment allowed). Leader cleans records older than window via ZREMRANGEBYSCORE.
const SortedSetKey = "toll:disconnects"

// LeaseKey — Redis-lease `cluster:toll:leader` for leader-election of Leader-loop.
// Holder = `kid` of keeper instance (read-friendly for logs).
const LeaseKey = "cluster:toll:leader"

// DegradedKey — Redis key-flag cluster:degraded (set by leader, TTL =
// DegradedTTL). Read on each write-API request via [DegradedReader].
const DegradedKey = "cluster:degraded"

// Publisher — narrow surface for Watcher: ZADD one disconnect-event
// to common sorted-set. Daemon wraps [keeperredis.PublishTollDisconnect]
// in implementation; interface allows fake in unit-tests ([Watcher]-tests
// check warmup/graceful filtering without live Redis).
type Publisher interface {
	PublishDisconnect(ctx context.Context, sid, kid, coven string, at time.Time) error
}

// EncodeDisconnect builds value-string of sorted-set record. Includes
// `at.UnixNano` as suffix for member uniqueness: ZADD by sorted-set rules
// updates score of existing member, and two disconnects
// «sid=foo|kid=A|coven=» in one second from same field set
// would merge into one record. UnixNano-suffix guarantees uniqueness
// without side effects for aggregation (Leader parses prefix, ignores
// suffix).
func EncodeDisconnect(sid, kid, coven string, at time.Time) string {
	var sb strings.Builder
	sb.Grow(len(sid) + len(kid) + len(coven) + 32)
	sb.WriteString(sid)
	sb.WriteByte('|')
	sb.WriteString(kid)
	sb.WriteByte('|')
	sb.WriteString(coven)
	sb.WriteByte('|')
	sb.WriteString(strconv.FormatInt(at.UnixNano(), 10))
	return sb.String()
}

// DegradedReader — read-only surface of cluster:degraded flag. Middleware
// calls IsDegraded on each blocked endpoint request; cost = one
// Redis EXISTS, no round-trip for reading value. Daemon wraps
// [keeperredis.TollIsDegraded] in implementation; for single-instance/dev without
// Redis — [NoopDegradedReader] (always false).
type DegradedReader interface {
	IsDegraded(ctx context.Context) (bool, error)
}

// NoopDegradedReader — fallback for single-instance/dev without Redis. Always
// returns false: no blocking, middleware passes all requests.
type NoopDegradedReader struct{}

// IsDegraded — always false.
func (NoopDegradedReader) IsDegraded(context.Context) (bool, error) { return false, nil }
