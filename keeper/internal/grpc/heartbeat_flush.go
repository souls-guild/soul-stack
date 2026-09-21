package grpc

import (
	"sync"
	"time"
)

// defaultLastSeenFlushFactor — divisor of the reaper's `stale_after` that
// gives the throttle interval for the `last_seen_at` PG flush. With the
// default `mark_disconnected.stale_after = 90s`, that's a flush every 30s
// per SID.
//
// 1/3 is chosen so that the disconnect-threshold window reliably fits ≥2
// flushes: even if one is missed (no app messages in the window), the next
// one refreshes the snapshot before the Reaper considers the stream dead.
// A smaller divisor means extra PG UPDATEs; a larger one risks a false
// disconnected on low-traffic streams.
const defaultLastSeenFlushFactor = 3

// lastSeenFlusher — per-SID throttle for the `souls.last_seen_at` PG flush.
//
// A live stream updates the heartbeat in Redis on every app message
// ([eventStreamHandler.touchSeen]); that's the fast layer. The PG snapshot
// is needed by the Reaper (`mark_disconnected` looks at
// `souls.last_seen_at`) and the Operator API — but writing to PG on every
// message is too heavy. The flusher lets through at most one PG write per
// [lastSeenFlusher.interval] per SID (in-memory last-flush time).
//
// State is tied to the handler instance (one per EventStream listener).
// Under multi-instance, each Keeper only flushes SIDs of its own streams
// (guaranteed by [SoulLease] — one Keeper per SID at a time); when a stream
// moves to another Keeper, the new handler doesn't know the throttle time
// and flushes right away — safe (one extra UPDATE on failover).
type lastSeenFlusher struct {
	interval time.Duration

	mu       sync.Mutex
	lastByID map[string]time.Time
}

func newLastSeenFlusher(interval time.Duration) *lastSeenFlusher {
	return &lastSeenFlusher{
		interval: interval,
		lastByID: make(map[string]time.Time),
	}
}

// shouldFlush reports whether it's time to flush `last_seen_at` for sid to
// PG, and on a positive answer atomically records now as the last-flush
// moment. This guarantees at least [lastSeenFlusher.interval] between two
// true answers for the same SID, even under concurrent calls.
func (f *lastSeenFlusher) shouldFlush(sid string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	last, ok := f.lastByID[sid]
	if ok && now.Sub(last) < f.interval {
		return false
	}
	f.lastByID[sid] = now
	return true
}

// forget removes the SID from the throttle state — called when the stream
// closes, so the map doesn't grow with disconnected Souls. The next
// connection of the same SID starts with a clean slate (flush right away),
// which is what we want.
func (f *lastSeenFlusher) forget(sid string) {
	f.mu.Lock()
	delete(f.lastByID, sid)
	f.mu.Unlock()
}
