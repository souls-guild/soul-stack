package grpc

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// outboundBufferSize — the length of the per-stream outbound channel
// (PM-decision 1 M2.5: buffered=10 + drop+log on overflow). A stale stream
// doesn't block the scenario-runner; back-pressure to the caller is
// handled explicitly — the caller gets [ErrOutboundQueueFull].
//
// The size was chosen empirically: in normal flow one Soul sees a sequence
// of `ApplyRequest` (1 per run), with occasional `CancelApply` and
// `SeedRotationReply` — 10 elements cover the burst of "the operator hit
// Cancel repeatedly" without accumulating a mailbox backlog.
const outboundBufferSize = 10

// Sentinel errors for [StreamManager] / [Outbound].
var (
	// ErrSoulNotConnected — there's no active EventStream on this Keeper
	// for the requested SID. The caller (scenario-runner) must check
	// `souls.last_seen_at` and/or the Redis heartbeat cache before
	// sending — here we're just recording the fact that "this particular
	// Keeper instance has no stream" (cluster-mode: another Keeper may
	// hold the lease for the same SID; routing between Keeper instances is
	// a separate slice).
	ErrSoulNotConnected = errors.New("grpc: no active EventStream for sid")

	// ErrOutboundQueueFull — the per-stream outbound channel is full. The
	// Soul isn't keeping up (slow network / a stuck receive loop on the
	// client). PM-decision 1: drop+log, the caller gets the sentinel and
	// decides for itself (for ApplyDispatch — fail the run; for
	// CancelApply — retry or skip; for SeedRotationReply — the Soul will
	// retry via its own rotation loop).
	ErrOutboundQueueFull = errors.New("grpc: outbound queue full for sid")
)

// streamEntry — per-stream state held in [StreamManager].
//
// Stored behind a pointer so Lookup can hand it out and guarantee that a
// concurrent Unregister won't yank the channel out from under an active
// caller (channel close is the only race-free operation, see
// [StreamManager.Unregister]).
//
// cancel — cancels the per-stream ctx (see [StreamManager.RegisterStream]).
// Needed for active shedding (Watchman, soul-shedding S2): when a Keeper
// instance is being drained, [StreamManager.CloseAll] cancels every
// stream's ctx → the EventStream handler exits its receive loop, does a
// normal teardown (Unregister/lease-Release LIFO), gRPC sends the Soul an
// EOF, and the Soul moves to a live Keeper via its reconnect-loop/failback
// list. nil means the stream was registered without a cancel
// (tests/Outbound via [StreamManager.Register]): CloseAll skips such a
// stream (the outCh channel still gets closed via the normal Unregister).
type streamEntry struct {
	sid     string
	outCh   chan *keeperv1.FromKeeper
	cancel  context.CancelFunc
	closeMu sync.Mutex
	closed  bool
}

// StreamManager — the registry of active EventStreams on the current
// Keeper instance.
//
// Key — SID (authoritative from the mTLS peer cert; see
// [authenticatedSIDFrom]). Value — the outbound channel that `FromKeeper`
// messages are written to; the EventStream handler's send loop reads them
// and calls `stream.Send`.
//
// Cluster-mode: a per-Keeper-instance registry; routing between Keeper
// instances (when a Soul holds a stream on Keeper-B but the Operator API
// calls SendApply on Keeper-A) is a separate slice (post-M2.5, via Redis
// pub/sub).
type StreamManager struct {
	mu      sync.RWMutex
	entries map[string]*streamEntry
	logger  *slog.Logger
}

// NewStreamManager assembles an empty registry.
func NewStreamManager(logger *slog.Logger) *StreamManager {
	return &StreamManager{
		entries: make(map[string]*streamEntry),
		logger:  logger,
	}
}

// Register registers a new stream for a SID WITHOUT a per-stream cancel
// and returns the outbound channel. A thin wrapper over
// [StreamManager.RegisterStream] for callers that don't need shedding
// (unit tests, ad-hoc registration in Outbound tests): [StreamManager.CloseAll]
// doesn't cancel such a stream (nothing to cancel), and normal teardown
// still happens via Unregister.
func (m *StreamManager) Register(sid string) <-chan *keeperv1.FromKeeper {
	return m.RegisterStream(sid, nil)
}

// RegisterStream registers a new stream for a SID with a per-stream cancel
// and returns the outbound channel that the handler hands to its send
// loop. If an entry already exists for the same SID, the old one is closed
// (channel.close) and the new stream evicts it.
//
// cancel — cancels the per-stream ctx derived from `stream.Context()`; it
// gives active shedding ([StreamManager.CloseAll], Watchman S2) a point to
// force-close the stream. nil is allowed (see [streamEntry.cancel] /
// [StreamManager.Register]).
//
// Eviction is symmetric with the Redis SoulLease (see
// [eventStreamHandler.acquireSoulLease]): if a Soul reconnects within the
// same Keeper instance (e.g. after a client-side reconnect), the new
// stream takes priority — the old receive loop will get io.EOF/Canceled on
// its next Recv regardless. The evicted stream's cancel is NOT invoked:
// its receive loop is already exiting on its own (the eviction is driven
// by a new Register on the same SID, i.e. the Soul reconnected and the old
// stream is being closed at the gRPC level), and an extra cancel would hit
// an entry that's already been replaced in the map.
func (m *StreamManager) RegisterStream(sid string, cancel context.CancelFunc) <-chan *keeperv1.FromKeeper {
	m.mu.Lock()
	defer m.mu.Unlock()

	if old, ok := m.entries[sid]; ok {
		old.close()
		m.logger.Warn("streammanager: existing stream evicted by new Register",
			slog.String("sid", sid))
	}

	entry := &streamEntry{
		sid:    sid,
		outCh:  make(chan *keeperv1.FromKeeper, outboundBufferSize),
		cancel: cancel,
	}
	m.entries[sid] = entry
	return entry.outCh
}

// Unregister removes the entry and closes the outbound channel.
// Idempotent: a repeated call is a no-op. The caller (handler defer) must
// call it after the receive loop ends.
//
// Takes the SID and a pointer to the owning entry (the owner handle
// guarantees we're removing OUR OWN entry, not one that evicted us via a
// concurrent Register).
func (m *StreamManager) Unregister(sid string, owner <-chan *keeperv1.FromKeeper) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[sid]
	if !ok {
		return
	}
	// Comparing channels compares the pointers inside the chan header; if
	// the map already holds a newer entry (we were evicted by a Register),
	// the owner channel won't match — don't touch it.
	if (<-chan *keeperv1.FromKeeper)(entry.outCh) != owner {
		return
	}
	entry.close()
	delete(m.entries, sid)
}

// lookup — fetches the entry under a read lock. nil if there's no stream.
func (m *StreamManager) lookup(sid string) *streamEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.entries[sid]
}

// SIDs returns a snapshot of the SIDs of all active streams on this Keeper
// instance. The snapshot is copied under RLock — the caller iterates
// freely, without holding up concurrent Register/Unregister calls. Used by
// the cluster-wide Sigil re-broadcast (S6c): on an invalidate signal, a
// node distributes the fresh active set to each of its connected Souls.
// Order is not guaranteed (map iteration).
func (m *StreamManager) SIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.entries))
	for sid := range m.entries {
		out = append(out, sid)
	}
	return out
}

// CloseAll force-closes ALL local streams — cancels the per-stream ctx of
// every registered entry (soul-shedding S2, Watchman). Returns the number
// of streams whose cancel was invoked (for the caller's log/metric).
//
// Cancelling the ctx wakes up the EventStream handler's receive loop
// (`ctx.Err() != nil` → return nil), and the handler does its OWN normal
// teardown (Unregister → lease-Release LIFO). CloseAll itself does NOT
// close the outCh channels and does NOT remove entries from the map — the
// handler's Unregister does that, so it doesn't race with its own defer
// chain (double close/remove-under-your-feet). So a repeated CloseAll
// before the handlers have actually exited invokes the same cancels again
// — this is idempotent (context.CancelFunc is safe to call repeatedly).
//
// The snapshot of cancels is taken under RLock, and the cancels themselves
// are invoked outside the lock: cancel is cheap, but the handler's
// teardown (which fires synchronously with the cancel, in another
// goroutine) may try to take the Lock in Unregister — holding a write lock
// here would be deadlock-prone; RLock plus calling outside it avoids that.
func (m *StreamManager) CloseAll() int {
	m.mu.RLock()
	cancels := make([]context.CancelFunc, 0, len(m.entries))
	for _, e := range m.entries {
		if e.cancel != nil {
			cancels = append(cancels, e.cancel)
		}
	}
	m.mu.RUnlock()

	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

// close — idempotent channel close. Guarded by closeMu so a repeated
// Unregister/eviction doesn't panic on a double close.
func (e *streamEntry) close() {
	e.closeMu.Lock()
	defer e.closeMu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	close(e.outCh)
}

// send — non-blocking enqueue. true → accepted, false → the channel
// buffer is full or closed (we treat both as failure; a closed channel
// shouldn't exist without an Unregister, but we guard against the race
// window).
func (e *streamEntry) send(msg *keeperv1.FromKeeper) bool {
	e.closeMu.Lock()
	if e.closed {
		e.closeMu.Unlock()
		return false
	}
	e.closeMu.Unlock()

	select {
	case e.outCh <- msg:
		return true
	default:
		return false
	}
}
