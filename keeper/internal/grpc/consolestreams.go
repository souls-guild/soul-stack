package grpc

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// The registry of dedicated console streams (ADR-0074 amendment, NIM-188).
//
// It sits next to [StreamManager] rather than inside it because the two answer
// different questions: StreamManager maps "which Soul" (key: SID, one
// EventStream each), this one maps "which console session" (key: the ULID
// Keeper minted, one bidi stream each). A Soul dials one console stream per
// session it is asked to open, on the SAME mTLS connection — so a registered
// console stream is always co-located with that SID's EventStream, and cluster
// routing needs no change: the frame still reaches this instance the usual way
// (local stream or Redis pub/sub) and only the last hop differs.

const (
	// consoleStreamBufferSize — the per-session downstream queue. Sized well
	// above [outboundBufferSize] because the traffic is different in kind:
	// this queue carries keystrokes and resizes, one small message each, and a
	// paste is a legitimate burst. It no longer shares anything with apply, so
	// depth here costs nothing but a few pointers.
	consoleStreamBufferSize = 64

	// maxConsoleStreamsPerSID caps how many console streams one Soul may hold
	// at once. The Soul-side session limit already bounds this in a healthy
	// binary; the cap is what makes a MISBEHAVING one bounded too, since a
	// stream costs a goroutine on this side.
	maxConsoleStreamsPerSID = 16
)

// ErrConsoleStreamLimit — the Soul is at [maxConsoleStreamsPerSID].
var ErrConsoleStreamLimit = errors.New("grpc: too many console streams for sid")

// ErrConsoleStreamTaken — a console stream is already attached to this
// session_id. A second attach is either a duplicate dial or a Soul trying to
// steal a route; both are refused, and the live stream is left alone.
var ErrConsoleStreamTaken = errors.New("grpc: console stream already attached for session")

// consoleStreamEntry is one attached console stream.
//
// `sid` is the authenticated peer of the dialing Soul, not an echo from the
// payload: [ConsoleStreamManager.lookup] compares it against the SID the
// session belongs to, so a stream that attached to somebody else's session id
// can never receive that session's keystrokes (ADR-012(i)).
type consoleStreamEntry struct {
	sid       string
	sessionID string
	outCh     chan *keeperv1.ConsoleToSoul

	closeMu sync.Mutex
	closed  bool
}

// ConsoleStreamManager is the per-instance registry of attached console
// streams, keyed by the Keeper-minted session id.
type ConsoleStreamManager struct {
	mu      sync.RWMutex
	entries map[string]*consoleStreamEntry
	perSID  map[string]int
	logger  *slog.Logger
}

// NewConsoleStreamManager assembles an empty registry.
func NewConsoleStreamManager(logger *slog.Logger) *ConsoleStreamManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &ConsoleStreamManager{
		entries: make(map[string]*consoleStreamEntry),
		perSID:  make(map[string]int),
		logger:  logger,
	}
}

// Register attaches a stream to a session. The caller (the ConsoleStream
// handler) must Unregister the returned entry when its receive loop ends.
func (m *ConsoleStreamManager) Register(sid, sessionID string) (*consoleStreamEntry, error) {
	if sid == "" || sessionID == "" {
		return nil, errors.New("grpc: console stream needs both sid and session_id")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, taken := m.entries[sessionID]; taken {
		return nil, fmt.Errorf("%w: %s", ErrConsoleStreamTaken, sessionID)
	}
	if m.perSID[sid] >= maxConsoleStreamsPerSID {
		return nil, fmt.Errorf("%w: %s at %d", ErrConsoleStreamLimit, sid, maxConsoleStreamsPerSID)
	}

	entry := &consoleStreamEntry{
		sid:       sid,
		sessionID: sessionID,
		outCh:     make(chan *keeperv1.ConsoleToSoul, consoleStreamBufferSize),
	}
	m.entries[sessionID] = entry
	m.perSID[sid]++
	return entry, nil
}

// Unregister detaches the stream and closes its queue, which is what ends the
// handler's writer goroutine. Idempotent, and safe against a newer stream
// having taken the same session id: only the owning entry is removed.
func (m *ConsoleStreamManager) Unregister(entry *consoleStreamEntry) {
	if entry == nil {
		return
	}

	m.mu.Lock()
	if cur, ok := m.entries[entry.sessionID]; ok && cur == entry {
		delete(m.entries, entry.sessionID)
		if n := m.perSID[entry.sid] - 1; n > 0 {
			m.perSID[entry.sid] = n
		} else {
			delete(m.perSID, entry.sid)
		}
	}
	m.mu.Unlock()

	entry.close()
}

// lookup returns the stream attached to sessionID, but only when it was dialed
// by the SID that owns the session. A mismatch is not an error to report to
// anyone — the caller falls back to the EventStream, where the frame is
// checked the same way.
func (m *ConsoleStreamManager) lookup(sessionID, sid string) *consoleStreamEntry {
	if sessionID == "" || sid == "" {
		return nil
	}

	m.mu.RLock()
	entry := m.entries[sessionID]
	m.mu.RUnlock()

	if entry == nil || entry.sid != sid {
		return nil
	}
	return entry
}

// CloseAllFor detaches every console stream of one SID and returns how many
// were closed. Called when that Soul's EventStream ends: a console never
// outlives the stream that authorized it, and leaving the dedicated streams
// registered would keep routing keystrokes into a host nobody is talking to.
func (m *ConsoleStreamManager) CloseAllFor(sid string) int {
	m.mu.Lock()
	doomed := make([]*consoleStreamEntry, 0, m.perSID[sid])
	for id, entry := range m.entries {
		if entry.sid == sid {
			doomed = append(doomed, entry)
			delete(m.entries, id)
		}
	}
	delete(m.perSID, sid)
	m.mu.Unlock()

	for _, entry := range doomed {
		entry.close()
	}
	return len(doomed)
}

// Count reports how many console streams are attached on this instance.
func (m *ConsoleStreamManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// close — idempotent queue close, guarded like [streamEntry.close].
func (e *consoleStreamEntry) close() {
	e.closeMu.Lock()
	defer e.closeMu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	close(e.outCh)
}

// send — non-blocking enqueue of one downstream console frame. false means the
// queue is full or the stream is detached; the caller reports it as
// [ErrOutboundQueueFull], the same contract [streamEntry.send] has.
func (e *consoleStreamEntry) send(msg *keeperv1.ConsoleToSoul) bool {
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

// consoleToSoul unwraps a downstream console payload from the EventStream
// envelope into the dedicated stream's envelope. nil for anything else,
// INCLUDING ConsoleOpen: that one is what makes the Soul dial, so it can only
// travel on EventStream.
//
// Keeping the internal plumbing on `FromKeeper` all the way to the last hop is
// deliberate — the Redis pub/sub envelope between Keeper instances is
// unchanged, so a cluster running mixed versions routes consoles as before.
func consoleToSoul(msg *keeperv1.FromKeeper) *keeperv1.ConsoleToSoul {
	switch p := msg.GetPayload().(type) {
	case *keeperv1.FromKeeper_ConsoleStdin:
		return &keeperv1.ConsoleToSoul{
			Payload: &keeperv1.ConsoleToSoul_ConsoleStdin{ConsoleStdin: p.ConsoleStdin},
		}
	case *keeperv1.FromKeeper_ConsoleResize:
		return &keeperv1.ConsoleToSoul{
			Payload: &keeperv1.ConsoleToSoul_ConsoleResize{ConsoleResize: p.ConsoleResize},
		}
	case *keeperv1.FromKeeper_ConsoleClose:
		return &keeperv1.ConsoleToSoul{
			Payload: &keeperv1.ConsoleToSoul_ConsoleClose{ConsoleClose: p.ConsoleClose},
		}
	default:
		return nil
	}
}

// downstreamConsoleSessionID returns the session a Keeper -> Soul console
// frame belongs to; "" for anything that is not routable to a console stream.
func downstreamConsoleSessionID(msg *keeperv1.FromKeeper) string {
	switch p := msg.GetPayload().(type) {
	case *keeperv1.FromKeeper_ConsoleStdin:
		return p.ConsoleStdin.GetSessionId()
	case *keeperv1.FromKeeper_ConsoleResize:
		return p.ConsoleResize.GetSessionId()
	case *keeperv1.FromKeeper_ConsoleClose:
		return p.ConsoleClose.GetSessionId()
	default:
		return ""
	}
}

// consoleFromSoul wraps an upstream console frame from the dedicated stream
// back into the EventStream envelope, so the session manager sees one shape
// regardless of which transport carried it. nil for a frame with no payload or
// for ConsoleAttach, which is transport bookkeeping and never reaches the Hub.
func consoleFromSoul(msg *keeperv1.ConsoleFromSoul) *keeperv1.FromSoul {
	switch p := msg.GetPayload().(type) {
	case *keeperv1.ConsoleFromSoul_ConsoleOpened:
		return &keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: p.ConsoleOpened},
		}
	case *keeperv1.ConsoleFromSoul_ConsoleChunk:
		return &keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: p.ConsoleChunk},
		}
	case *keeperv1.ConsoleFromSoul_ConsoleExit:
		return &keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: p.ConsoleExit},
		}
	default:
		return nil
	}
}

// upstreamConsoleSessionID returns the session id carried by an upstream frame
// of the dedicated stream; "" for a frame that carries none.
func upstreamConsoleSessionID(msg *keeperv1.ConsoleFromSoul) string {
	switch p := msg.GetPayload().(type) {
	case *keeperv1.ConsoleFromSoul_ConsoleAttach:
		return p.ConsoleAttach.GetSessionId()
	case *keeperv1.ConsoleFromSoul_ConsoleOpened:
		return p.ConsoleOpened.GetSessionId()
	case *keeperv1.ConsoleFromSoul_ConsoleChunk:
		return p.ConsoleChunk.GetSessionId()
	case *keeperv1.ConsoleFromSoul_ConsoleExit:
		return p.ConsoleExit.GetSessionId()
	default:
		return ""
	}
}
