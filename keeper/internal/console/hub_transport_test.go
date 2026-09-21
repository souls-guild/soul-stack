package console

import (
	"context"
	"errors"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Guards for the transport split (NIM-188) at the session-manager level: the
// window between ConsoleOpen and ConsoleOpened, and the rule that a session id
// is a route rather than a credential.

func (d *recordingDispatcher) stdinData() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.stdin))
	for _, m := range d.stdin {
		out = append(out, string(m.GetData()))
	}
	return out
}

// Input typed before the pty answers must not go out yet: which carrier the
// session ends up on is not known until then, and two carriers are two
// independent streams with no ordering between them.
func TestHub_InputBeforeOpenedIsHeldBack(t *testing.T) {
	h, d := newTestHub(t, HubDeps{})
	sess := mustOpen(t, h, "pane", "host-a", "archon-a", &captureSink{})

	if err := h.Stdin(context.Background(), sess, []byte("a")); err != nil {
		t.Fatalf("Stdin: %v", err)
	}
	if got := d.stdinData(); len(got) != 0 {
		t.Fatalf("dispatched %v before ConsoleOpened", got)
	}

	markOpened(t, h, sess, "host-a")
	if got := d.stdinData(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("dispatched %v after ConsoleOpened, want [a]", got)
	}
}

// Held-back keystrokes are released in the order they were typed. A terminal
// that reorders input is worse than one that refuses it.
func TestHub_HeldInputKeepsItsOrder(t *testing.T) {
	h, d := newTestHub(t, HubDeps{})
	sess := mustOpen(t, h, "pane", "host-a", "archon-a", &captureSink{})

	for _, k := range []string{"l", "s", "\n"} {
		if err := h.Stdin(context.Background(), sess, []byte(k)); err != nil {
			t.Fatalf("Stdin(%q): %v", k, err)
		}
	}
	markOpened(t, h, sess, "host-a")

	if err := h.Stdin(context.Background(), sess, []byte("!")); err != nil {
		t.Fatalf("Stdin after opened: %v", err)
	}
	got := d.stdinData()
	want := []string{"l", "s", "\n", "!"}
	if len(got) != len(want) {
		t.Fatalf("dispatched %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dispatched %v, want %v", got, want)
		}
	}
}

// The hold is bounded. Overflow is reported to the operator, never dropped
// silently — losing keystrokes is the one thing a terminal may not do.
func TestHub_HeldInputIsBounded(t *testing.T) {
	h, _ := newTestHub(t, HubDeps{})
	sess := mustOpen(t, h, "pane", "host-a", "archon-a", &captureSink{})

	for i := 0; i < maxPendingFrames; i++ {
		if err := h.Stdin(context.Background(), sess, []byte("x")); err != nil {
			t.Fatalf("Stdin #%d: %v", i, err)
		}
	}
	if err := h.Stdin(context.Background(), sess, []byte("x")); !errors.Is(err, ErrSessionNotReady) {
		t.Fatalf("err = %v, want ErrSessionNotReady", err)
	}
}

// A close must never be held back: a session closed before it opened will never
// send the ConsoleOpened that would release it, and the pty would outlive the
// operator's intent to kill it.
func TestHub_CloseIsNeverHeldBack(t *testing.T) {
	h, d := newTestHub(t, HubDeps{})
	sess := mustOpen(t, h, "pane", "host-a", "archon-a", &captureSink{})

	h.Close(context.Background(), sess, "operator changed their mind")

	if d.closeCount() != 1 {
		t.Fatalf("closes dispatched = %d, want 1 even before ConsoleOpened", d.closeCount())
	}
}

// perCapability answers each capability independently, which the shared
// staticCapabilities fake cannot: `console` and `console_stream` are asked
// different questions.
type perCapability map[string]bool

func (c perCapability) HasCapability(_ context.Context, _, capability string) (bool, error) {
	return c[capability], nil
}

// The carrier a session ended up on is the first question to ask about an
// interactive session that feels slow, so it must not require reading the
// Soul's version.
func TestHub_ReportsTheCarrierOfASession(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps CapabilityChecker
		want string
	}{
		{"dedicated", perCapability{CapabilityConsole: true, CapabilityConsoleStream: true}, "console_stream"},
		{"shared", perCapability{CapabilityConsole: true}, "eventstream"},
		{"no checker", nil, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHub(t, HubDeps{Capabilities: tc.caps})
			if got := h.transportOf(context.Background(), "host-a"); got != tc.want {
				t.Fatalf("transportOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// A session id is a route, not a credential: the authority for "whose session"
// is the peer cert of the stream the frame arrived on (ADR-012(i)).
func TestHub_FrameFromAnotherSIDIsDropped(t *testing.T) {
	h, _ := newTestHub(t, HubDeps{})
	sink := &captureSink{}
	sess := mustOpen(t, h, "pane", "host-a", "archon-a", sink)

	h.Deliver(context.Background(), "host-evil", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID, Data: []byte("injected"),
		}},
	})

	if _, chunks, _, _ := sink.counts(); chunks != 0 {
		t.Fatalf("chunks delivered = %d, want 0 — another host wrote into this pane", chunks)
	}
}

// The same rule applies to a terminal: a foreign ConsoleExit would let one host
// close another host's session.
func TestHub_ExitFromAnotherSIDCannotCloseTheSession(t *testing.T) {
	h, _ := newTestHub(t, HubDeps{})
	sess := mustOpen(t, h, "pane", "host-a", "archon-a", &captureSink{})

	h.Deliver(context.Background(), "host-evil", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: &keeperv1.ConsoleExit{
			SessionId: sess.KeeperID,
			Reason:    keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED,
		}},
	})

	if h.Count() != 1 {
		t.Fatalf("live sessions = %d, want the session to survive a foreign terminal", h.Count())
	}
}
