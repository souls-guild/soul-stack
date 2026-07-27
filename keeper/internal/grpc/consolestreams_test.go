package grpc

import (
	"context"
	"errors"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// The guard set for the transport split (NIM-188). The claim being defended is
// symmetric and load-bearing: a console cannot be starved by apply traffic, and
// apply traffic cannot be starved by a console.

func attachForTest(t *testing.T, m *StreamManager, sid, sessionID string) *consoleStreamEntry {
	t.Helper()
	entry, err := m.Consoles().Register(sid, sessionID)
	if err != nil {
		t.Fatalf("Register(%s, %s): %v", sid, sessionID, err)
	}
	return entry
}

func stdinFor(sessionID string) *keeperv1.ConsoleStdin {
	return &keeperv1.ConsoleStdin{SessionId: sessionID, Data: []byte("x")}
}

// A wedged EventStream must not stop keystrokes: that is the whole point of
// giving the session its own stream.
func TestConsoleStream_NotBlockedByAFullEventStreamQueue(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	_ = m.Register("host-a")
	ob := newOutboundForTest(t, m, nopAudit{})
	entry := attachForTest(t, m, "host-a", "session-1")

	// Wedge the EventStream: fill its queue to the brim with apply traffic.
	for i := 0; i < outboundBufferSize; i++ {
		if err := ob.SendApply(context.Background(), "host-a", &keeperv1.ApplyRequest{ApplyId: "x"}); err != nil {
			t.Fatalf("SendApply #%d: %v", i, err)
		}
	}
	if err := ob.SendApply(context.Background(), "host-a", &keeperv1.ApplyRequest{ApplyId: "x"}); !errors.Is(err, ErrOutboundQueueFull) {
		t.Fatalf("EventStream queue is not full: %v", err)
	}

	// The console is untouched by that.
	for i := 0; i < consoleStreamBufferSize; i++ {
		if err := ob.SendConsoleStdin(context.Background(), "host-a", stdinFor("session-1")); err != nil {
			t.Fatalf("SendConsoleStdin #%d: %v", i, err)
		}
	}
	if got := len(entry.outCh); got != consoleStreamBufferSize {
		t.Fatalf("console queue holds %d frames, want %d — keystrokes went elsewhere", got, consoleStreamBufferSize)
	}
}

// And the mirror image: a console flooding its own stream must not consume the
// queue apply dispatch depends on.
func TestConsoleStream_FloodDoesNotConsumeTheEventStreamQueue(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("host-a")
	ob := newOutboundForTest(t, m, nopAudit{})
	attachForTest(t, m, "host-a", "session-1")

	for i := 0; i < consoleStreamBufferSize; i++ {
		if err := ob.SendConsoleStdin(context.Background(), "host-a", stdinFor("session-1")); err != nil {
			t.Fatalf("SendConsoleStdin #%d: %v", i, err)
		}
	}
	// Overflowing the console's own queue is reported to its operator...
	if err := ob.SendConsoleStdin(context.Background(), "host-a", stdinFor("session-1")); !errors.Is(err, ErrOutboundQueueFull) {
		t.Fatalf("console overflow err = %v, want ErrOutboundQueueFull", err)
	}
	// ...and costs the run nothing.
	if len(out) != 0 {
		t.Fatalf("EventStream queue holds %d frames, want 0 — the console leaked into it", len(out))
	}
	if err := ob.SendApply(context.Background(), "host-a", &keeperv1.ApplyRequest{ApplyId: "x"}); err != nil {
		t.Fatalf("SendApply after a console flood: %v", err)
	}
}

// A Soul that never attaches a console stream is served exactly as before. This
// is the backward-compatibility guarantee: an old binary keeps working against
// a new Keeper with no capability negotiation involved.
func TestConsoleStream_UnattachedSessionFallsBackToEventStream(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("host-a")
	ob := newOutboundForTest(t, m, nopAudit{})

	if err := ob.SendConsoleStdin(context.Background(), "host-a", stdinFor("session-1")); err != nil {
		t.Fatalf("SendConsoleStdin: %v", err)
	}

	msg, ok := <-out
	if !ok {
		t.Fatal("EventStream queue closed")
	}
	if msg.GetConsoleStdin().GetSessionId() != "session-1" {
		t.Fatalf("payload = %T, want ConsoleStdin for session-1", msg.GetPayload())
	}
}

// ConsoleOpen never rides the dedicated stream: it is the message that makes
// the Soul dial one, so routing it there would deadlock the open.
func TestConsoleStream_OpenAlwaysRidesTheEventStream(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("host-a")
	ob := newOutboundForTest(t, m, nopAudit{})
	entry := attachForTest(t, m, "host-a", "session-1")

	if err := ob.SendConsoleOpen(context.Background(), "host-a", &keeperv1.ConsoleOpen{
		SessionId: "session-1", TargetSid: "host-a",
	}); err != nil {
		t.Fatalf("SendConsoleOpen: %v", err)
	}

	if len(entry.outCh) != 0 {
		t.Fatalf("ConsoleOpen went to the dedicated stream (%d frames)", len(entry.outCh))
	}
	if msg := <-out; msg.GetConsoleOpen().GetSessionId() != "session-1" {
		t.Fatalf("payload = %T, want ConsoleOpen on the EventStream", msg.GetPayload())
	}
}

// A session id is a route, not a credential. A Soul that attaches to another
// host's session must not receive that session's keystrokes — the authority is
// the peer cert (ADR-012(i)).
func TestConsoleStream_ForeignSIDNeverReceivesTheSession(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("host-a")
	_ = m.Register("host-evil")
	ob := newOutboundForTest(t, m, nopAudit{})
	impostor := attachForTest(t, m, "host-evil", "session-1")

	if err := ob.SendConsoleStdin(context.Background(), "host-a", stdinFor("session-1")); err != nil {
		t.Fatalf("SendConsoleStdin: %v", err)
	}

	if len(impostor.outCh) != 0 {
		t.Fatalf("keystrokes reached a stream dialed by another sid (%d frames)", len(impostor.outCh))
	}
	if msg := <-out; msg.GetConsoleStdin() == nil {
		t.Fatalf("payload = %T, want the frame on host-a's own EventStream", msg.GetPayload())
	}
}

// A console never outlives the stream that authorized it: when the Soul's
// EventStream goes, its console streams go with it (ADR-0074).
func TestConsoleStream_ClosedWithItsEventStream(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("host-a")
	entry := attachForTest(t, m, "host-a", "session-1")
	other := attachForTest(t, m, "host-a", "session-2")

	m.Unregister("host-a", out)

	if m.Consoles().Count() != 0 {
		t.Fatalf("console streams still registered: %d", m.Consoles().Count())
	}
	for name, e := range map[string]*consoleStreamEntry{"session-1": entry, "session-2": other} {
		if _, open := <-e.outCh; open {
			t.Fatalf("%s: queue still open after its EventStream ended", name)
		}
	}
}

// A reconnect evicts the old EventStream entry. The eviction must not take the
// fresh connection's consoles down with the stale one.
func TestConsoleStream_SurvivesTheEvictionOfAStaleEventStream(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	stale := m.Register("host-a")
	_ = m.Register("host-a") // reconnect: evicts `stale`
	entry := attachForTest(t, m, "host-a", "session-1")

	m.Unregister("host-a", stale) // the old handler's defer, arriving late

	if m.Consoles().Count() != 1 {
		t.Fatalf("console streams = %d, want the live one to survive", m.Consoles().Count())
	}
	if !entry.send(&keeperv1.ConsoleToSoul{}) {
		t.Fatal("the live console stream was closed by a stale teardown")
	}
}

func TestConsoleStreamManager_RefusesADuplicateAttach(t *testing.T) {
	m := NewConsoleStreamManager(discardLogger(t))
	if _, err := m.Register("host-a", "session-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := m.Register("host-a", "session-1"); !errors.Is(err, ErrConsoleStreamTaken) {
		t.Fatalf("second attach err = %v, want ErrConsoleStreamTaken", err)
	}
}

// A misbehaving Soul must not be able to spend this instance's goroutines on
// console streams it never uses.
func TestConsoleStreamManager_CapsStreamsPerSID(t *testing.T) {
	m := NewConsoleStreamManager(discardLogger(t))
	for i := 0; i < maxConsoleStreamsPerSID; i++ {
		if _, err := m.Register("host-a", string(rune('a'+i))); err != nil {
			t.Fatalf("Register #%d: %v", i, err)
		}
	}
	if _, err := m.Register("host-a", "one-too-many"); !errors.Is(err, ErrConsoleStreamLimit) {
		t.Fatalf("err = %v, want ErrConsoleStreamLimit", err)
	}
	// The cap is per SID, not global.
	if _, err := m.Register("host-b", "session-1"); err != nil {
		t.Fatalf("Register for another sid: %v", err)
	}
}
