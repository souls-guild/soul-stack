package console

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recordingDispatcher captures what the Hub sent toward the Soul.
type recordingDispatcher struct {
	mu      sync.Mutex
	opens   []*keeperv1.ConsoleOpen
	stdin   []*keeperv1.ConsoleStdin
	resizes []*keeperv1.ConsoleResize
	closes  []*keeperv1.ConsoleClose
	// closeSIDs records which host each close was addressed to — an upstream
	// frame carries only a session id, so getting the SID right is the thing
	// that makes an orphan reap land on the correct machine.
	closeSIDs []string
	openErr   error
}

func (d *recordingDispatcher) SendConsoleOpen(_ context.Context, _ string, m *keeperv1.ConsoleOpen) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.openErr != nil {
		return d.openErr
	}
	d.opens = append(d.opens, m)
	return nil
}

func (d *recordingDispatcher) SendConsoleStdin(_ context.Context, _ string, m *keeperv1.ConsoleStdin) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stdin = append(d.stdin, m)
	return nil
}

func (d *recordingDispatcher) SendConsoleResize(_ context.Context, _ string, m *keeperv1.ConsoleResize) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resizes = append(d.resizes, m)
	return nil
}

func (d *recordingDispatcher) SendConsoleClose(_ context.Context, sid string, m *keeperv1.ConsoleClose) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closes = append(d.closes, m)
	d.closeSIDs = append(d.closeSIDs, sid)
	return nil
}

func (d *recordingDispatcher) closeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.closes)
}

func (d *recordingDispatcher) openCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.opens)
}

func (d *recordingDispatcher) stdinCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.stdin)
}

func (d *recordingDispatcher) resizeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.resizes)
}

// captureSink records the frames the Hub delivered to an operator.
type captureSink struct {
	mu     sync.Mutex
	opened []OpenedFrame
	chunks []ChunkFrame
	exits  []ExitFrame
	errs   []ErrorFrame
}

func (s *captureSink) DeliverChunk(sessionID string, stream keeperv1.ConsoleStream, data []byte, soulDropped uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = append(s.chunks, NewChunk(sessionID, stream, data, soulDropped))
}

func (s *captureSink) DeliverOpened(f OpenedFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened = append(s.opened, f)
}

func (s *captureSink) DeliverExit(f ExitFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exits = append(s.exits, f)
}

func (s *captureSink) DeliverError(f ErrorFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, f)
}

func (s *captureSink) counts() (opened, chunks, exits, errs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.opened), len(s.chunks), len(s.exits), len(s.errs)
}

// lastErrorCode is the code of the most recent `error` frame, or "" if none.
func (s *captureSink) lastErrorCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) == 0 {
		return ""
	}
	return s.errs[len(s.errs)-1].Code
}

// staticCapabilities answers the console-capability gate.
type staticCapabilities struct {
	has bool
	err error
}

func (c staticCapabilities) HasCapability(context.Context, string, string) (bool, error) {
	return c.has, c.err
}

func newTestHub(t *testing.T, deps HubDeps) (*Hub, *recordingDispatcher) {
	t.Helper()
	d, _ := deps.Dispatcher.(*recordingDispatcher)
	if d == nil {
		d = &recordingDispatcher{}
		deps.Dispatcher = d
	}
	if deps.Logger == nil {
		deps.Logger = testLogger()
	}
	if deps.Recorder == nil {
		deps.Recorder = newTestRecorder(t, newFakeRecordingStore(), RecorderConfig{})
	}
	h, err := NewHub(deps)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	return h, d
}

func mustOpen(t *testing.T, h *Hub, clientID, sid, aid string, sink Sink) *Session {
	t.Helper()
	sess, err := h.Open(context.Background(), OpenRequest{
		ClientID: clientID, SID: sid, AID: aid, Sink: sink,
	})
	if err != nil {
		t.Fatalf("Open(%s): %v", clientID, err)
	}
	return sess
}

// markOpened delivers the ConsoleOpened that ends the pre-`opened` park
// (NIM-188), so a test can exercise steady-state input dispatch.
func markOpened(t *testing.T, h *Hub, sess *Session, sid string) {
	t.Helper()
	h.Deliver(context.Background(), sid, &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: sess.KeeperID, Pid: 4242,
		}},
	})
}

// The Soul side must never see the client's socket-local id: uniqueness there
// is per EventStream, and two operators would collide on "pane-1".
func TestHub_MintsItsOwnSessionID(t *testing.T) {
	h, d := newTestHub(t, HubDeps{})
	sink := &captureSink{}

	a := mustOpen(t, h, "pane-1", "host-a", "archon-a", sink)
	b := mustOpen(t, h, "pane-1", "host-b", "archon-b", sink)

	if a.KeeperID == "pane-1" || b.KeeperID == "pane-1" {
		t.Fatal("the client id leaked to the Soul side")
	}
	if a.KeeperID == b.KeeperID {
		t.Fatal("two sessions share a Soul-side id — a chunk could not be routed")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.opens) != 2 {
		t.Fatalf("dispatched opens = %d, want 2", len(d.opens))
	}
	for _, o := range d.opens {
		if o.GetTargetSid() == "" {
			t.Fatal("ConsoleOpen carries no target_sid")
		}
	}
}

// Upstream frames must be addressed by the CLIENT id, since that is what the
// operator's socket knows.
func TestHub_DeliverTranslatesIDsBack(t *testing.T) {
	h, _ := newTestHub(t, HubDeps{})
	sink := &captureSink{}
	sess := mustOpen(t, h, "pane-7", "host-a", "archon-a", sink)

	h.Deliver(context.Background(), "host-a", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: sess.KeeperID, Pid: 99,
		}},
	})
	h.Deliver(context.Background(), "host-a", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: sess.KeeperID, Data: []byte("hi"),
		}},
	})

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.opened) != 1 || sink.opened[0].SessionID != "pane-7" {
		t.Fatalf("opened frames = %+v, want one addressed to pane-7", sink.opened)
	}
	if len(sink.chunks) != 1 || sink.chunks[0].SessionID != "pane-7" {
		t.Fatalf("chunk frames = %+v, want one addressed to pane-7", sink.chunks)
	}
}

// A frame for a session that already ended is a routine race (the operator
// closed while output was in flight), not an error.
func TestHub_DeliverForUnknownSessionIsDropped(t *testing.T) {
	h, _ := newTestHub(t, HubDeps{})
	sink := &captureSink{}

	h.Deliver(context.Background(), "host-x", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId: "01JGONE0000000000000000000", Data: []byte("late"),
		}},
	})

	if _, chunks, _, _ := sink.counts(); chunks != 0 {
		t.Fatalf("chunks delivered = %d, want 0", chunks)
	}
}

func TestHub_PerOperatorAndGlobalLimits(t *testing.T) {
	t.Run("per operator", func(t *testing.T) {
		h, _ := newTestHub(t, HubDeps{Limits: Limits{MaxSessionsPerAID: 2}})
		sink := &captureSink{}
		mustOpen(t, h, "a", "host-a", "archon-a", sink)
		mustOpen(t, h, "b", "host-b", "archon-a", sink)

		if _, err := h.Open(context.Background(), OpenRequest{
			ClientID: "c", SID: "host-c", AID: "archon-a", Sink: sink,
		}); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("third open err = %v, want ErrLimitExceeded", err)
		}
		// A different operator has their own budget.
		mustOpen(t, h, "d", "host-d", "archon-b", sink)
	})

	t.Run("global", func(t *testing.T) {
		h, _ := newTestHub(t, HubDeps{Limits: Limits{MaxSessionsGlobal: 2}})
		sink := &captureSink{}
		mustOpen(t, h, "a", "host-a", "archon-a", sink)
		mustOpen(t, h, "b", "host-b", "archon-b", sink)

		if _, err := h.Open(context.Background(), OpenRequest{
			ClientID: "c", SID: "host-c", AID: "archon-c", Sink: sink,
		}); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("third open err = %v, want ErrLimitExceeded", err)
		}
	})
}

// A refused open must leave no trace: otherwise every attempt against an
// offline host would burn a slot from the operator's budget.
func TestHub_FailedDispatchLeaksNoSlot(t *testing.T) {
	d := &recordingDispatcher{openErr: errors.New("no active EventStream")}
	h, _ := newTestHub(t, HubDeps{Dispatcher: d, Limits: Limits{MaxSessionsPerAID: 1}})
	sink := &captureSink{}

	for i := 0; i < 5; i++ {
		if _, err := h.Open(context.Background(), OpenRequest{
			ClientID: "pane", SID: "gone", AID: "archon-a", Sink: sink,
		}); !errors.Is(err, ErrSoulOffline) {
			t.Fatalf("open err = %v, want ErrSoulOffline", err)
		}
	}
	if h.Count() != 0 || h.CountFor("archon-a") != 0 {
		t.Fatalf("after 5 failed opens: total=%d perAID=%d, want 0/0", h.Count(), h.CountFor("archon-a"))
	}

	// The budget is intact — a real host still opens.
	d.mu.Lock()
	d.openErr = nil
	d.mu.Unlock()
	mustOpen(t, h, "pane", "host-a", "archon-a", sink)
}

// Fail-closed on a Soul that cannot host consoles: minting the session would
// leave the operator watching a terminal that never answers.
func TestHub_CapabilityGate(t *testing.T) {
	t.Run("missing capability refuses", func(t *testing.T) {
		h, d := newTestHub(t, HubDeps{Capabilities: staticCapabilities{has: false}})
		_, err := h.Open(context.Background(), OpenRequest{
			ClientID: "pane", SID: "old-binary", AID: "archon-a", Sink: &captureSink{},
		})
		if !errors.Is(err, ErrConsoleUnsupported) {
			t.Fatalf("err = %v, want ErrConsoleUnsupported", err)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.opens) != 0 {
			t.Fatal("ConsoleOpen was dispatched to a Soul without the capability")
		}
	})

	t.Run("lookup failure refuses", func(t *testing.T) {
		h, _ := newTestHub(t, HubDeps{Capabilities: staticCapabilities{err: errors.New("redis down")}})
		if _, err := h.Open(context.Background(), OpenRequest{
			ClientID: "pane", SID: "host-a", AID: "archon-a", Sink: &captureSink{},
		}); err == nil {
			t.Fatal("a capability lookup failure must refuse the open, not guess")
		}
	})

	t.Run("present capability allows", func(t *testing.T) {
		h, _ := newTestHub(t, HubDeps{Capabilities: staticCapabilities{has: true}})
		mustOpen(t, h, "pane", "host-a", "archon-a", &captureSink{})
	})
}

// The terminal frame both reaches the operator and frees the slot — exactly
// once, whichever side got there first.
func TestHub_ExitIsTerminalAndIdempotent(t *testing.T) {
	h, d := newTestHub(t, HubDeps{})
	sink := &captureSink{}
	sess := mustOpen(t, h, "pane-1", "host-a", "archon-a", sink)

	exit := &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: &keeperv1.ConsoleExit{
			SessionId: sess.KeeperID,
			ExitCode:  137,
			Reason:    keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED,
		}},
	}
	h.Deliver(context.Background(), "host-a", exit)

	if h.Count() != 0 || h.CountFor("archon-a") != 0 {
		t.Fatalf("after exit: total=%d perAID=%d, want 0/0", h.Count(), h.CountFor("archon-a"))
	}
	sink.mu.Lock()
	if len(sink.exits) != 1 || sink.exits[0].Code != 137 || sink.exits[0].Reason != "process_exited" {
		t.Fatalf("exit frames = %+v", sink.exits)
	}
	sink.mu.Unlock()

	// A late operator close must not dispatch a second ConsoleClose or
	// double-release the slot.
	h.Close(context.Background(), sess, "operator detached")
	if got := d.closeCount(); got != 0 {
		t.Fatalf("ConsoleClose dispatched %d times after the session already exited, want 0", got)
	}
	if h.CountFor("archon-a") != 0 {
		t.Fatal("the per-operator counter went negative after a close following an exit")
	}
}

// Kill-on-disconnect: the socket dying takes every pty with it.
func TestHub_CloseAllForReapsSessions(t *testing.T) {
	h, d := newTestHub(t, HubDeps{})
	sink := &captureSink{}

	sessions := []*Session{
		mustOpen(t, h, "a", "host-a", "archon-a", sink),
		mustOpen(t, h, "b", "host-b", "archon-a", sink),
		mustOpen(t, h, "c", "host-c", "archon-a", sink),
	}

	if n := h.CloseAllFor(context.Background(), sessions, CloseSocketClosed); n != 3 {
		t.Fatalf("closed = %d, want 3", n)
	}
	if h.Count() != 0 {
		t.Fatalf("live sessions = %d after the socket died", h.Count())
	}
	if got := d.closeCount(); got != 3 {
		t.Fatalf("ConsoleClose dispatched %d times, want 3 — a pty was orphaned", got)
	}

	// Repeating the teardown is a no-op, so a concurrent close cannot
	// double-dispatch.
	if n := h.CloseAllFor(context.Background(), sessions, CloseSocketClosed); n != 0 {
		t.Fatalf("second CloseAllFor closed %d, want 0", n)
	}
}

// An unreachable Soul must not block teardown: its ptys are already dead,
// because a console never outlives its EventStream.
func TestHub_CloseSurvivesDispatchFailure(t *testing.T) {
	d := &recordingDispatcher{}
	h, _ := newTestHub(t, HubDeps{Dispatcher: d})
	sink := &captureSink{}
	sess := mustOpen(t, h, "pane", "host-a", "archon-a", sink)

	d.mu.Lock()
	d.openErr = nil
	d.mu.Unlock()

	h.Close(context.Background(), sess, "socket closed")
	if h.Count() != 0 {
		t.Fatal("the session survived a close whose dispatch failed")
	}
}

// --- idle sweep ---

func TestHub_SweepIdleClosesAbandonedSessions(t *testing.T) {
	h, d := newTestHub(t, HubDeps{Limits: Limits{IdleTimeout: 30 * time.Millisecond}})
	sink := &captureSink{}
	idle := mustOpen(t, h, "idle", "host-a", "archon-a", sink)
	active := mustOpen(t, h, "active", "host-b", "archon-a", sink)

	time.Sleep(50 * time.Millisecond)
	// Typing keeps a session alive; output would not.
	if err := h.Stdin(context.Background(), active, []byte("x")); err != nil {
		t.Fatalf("Stdin: %v", err)
	}

	if n := h.SweepIdle(context.Background()); n != 1 {
		t.Fatalf("swept %d sessions, want 1", n)
	}
	if h.Count() != 1 {
		t.Fatalf("live sessions = %d, want the active one", h.Count())
	}
	if d.closeCount() != 1 {
		t.Fatalf("ConsoleClose dispatched %d times, want 1", d.closeCount())
	}
	if idle.KeeperID == active.KeeperID {
		t.Fatal("test setup is broken: both sessions share an id")
	}
	// The operator is told why the pane went away.
	if _, _, _, errs := sink.counts(); errs != 1 {
		t.Fatalf("error frames = %d, want 1 explaining the idle close", errs)
	}
}

// Output alone must NOT count as activity — a `tail -f` left running overnight
// is exactly the abandoned root shell the sweep exists for.
func TestHub_OutputDoesNotResetIdleTimer(t *testing.T) {
	h, _ := newTestHub(t, HubDeps{Limits: Limits{IdleTimeout: 30 * time.Millisecond}})
	sink := &captureSink{}
	sess := mustOpen(t, h, "tail", "host-a", "archon-a", sink)

	deadline := time.Now().Add(60 * time.Millisecond)
	for time.Now().Before(deadline) {
		h.Deliver(context.Background(), "host-a", &keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
				SessionId: sess.KeeperID, Data: []byte("log line\n"),
			}},
		})
		time.Sleep(5 * time.Millisecond)
	}

	if n := h.SweepIdle(context.Background()); n != 1 {
		t.Fatalf("swept %d, want 1 — streaming output kept an abandoned shell alive", n)
	}
}

func TestHub_SweepDisabledByZeroTimeout(t *testing.T) {
	h, _ := newTestHub(t, HubDeps{Limits: Limits{IdleTimeout: -1}})
	mustOpen(t, h, "pane", "host-a", "archon-a", &captureSink{})

	time.Sleep(10 * time.Millisecond)
	if n := h.SweepIdle(context.Background()); n != 0 {
		t.Fatalf("swept %d with the sweep disabled, want 0", n)
	}
	if h.Count() != 1 {
		t.Fatal("a session was closed with the idle sweep disabled")
	}
}

// A zero dimension is not a resize request; forwarding it would be noise on the
// shared stream.
func TestHub_ResizeIgnoresZeroGeometry(t *testing.T) {
	h, d := newTestHub(t, HubDeps{})
	sess := mustOpen(t, h, "pane", "host-a", "archon-a", &captureSink{})
	markOpened(t, h, sess, "host-a")

	for _, g := range [][2]uint32{{0, 24}, {80, 0}, {0, 0}} {
		if err := h.Resize(context.Background(), sess, g[0], g[1]); err != nil {
			t.Fatalf("Resize%v: %v", g, err)
		}
	}
	if got := d.resizeCount(); got != 0 {
		t.Fatalf("dispatched %d zero-geometry resizes, want 0", got)
	}

	if err := h.Resize(context.Background(), sess, 120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if got := d.resizeCount(); got != 1 {
		t.Fatalf("dispatched %d real resizes, want 1", got)
	}
}

// Concurrent opens and closes must keep the counters exact — the cap is a
// security-adjacent limit, not a hint.
func TestHub_ConcurrentOpenCloseKeepsCountersExact(t *testing.T) {
	h, _ := newTestHub(t, HubDeps{Limits: Limits{MaxSessionsPerAID: 1000, MaxSessionsGlobal: 1000}})
	sink := &captureSink{}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sess, err := h.Open(context.Background(), OpenRequest{
				ClientID: "pane", SID: "host", AID: "archon-a", Sink: sink,
			})
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			if i%2 == 0 {
				h.Close(context.Background(), sess, "done")
				return
			}
			h.Deliver(context.Background(), "host", &keeperv1.FromSoul{
				Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: &keeperv1.ConsoleExit{
					SessionId: sess.KeeperID,
					Reason:    keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED,
				}},
			})
		}(i)
	}
	wg.Wait()

	if h.Count() != 0 {
		t.Fatalf("live sessions = %d after every session ended", h.Count())
	}
	if h.CountFor("archon-a") != 0 {
		t.Fatalf("per-operator counter = %d, want 0", h.CountFor("archon-a"))
	}
}

func TestNewHub_RequiresDispatcher(t *testing.T) {
	if _, err := NewHub(HubDeps{Logger: testLogger()}); err == nil {
		t.Fatal("NewHub accepted a nil dispatcher")
	}
}

// A nil bridge is single-instance mode, and every cluster call must be a no-op
// rather than a panic.
func TestClusterBridge_NilIsSafe(t *testing.T) {
	var b *ClusterBridge
	ctx := context.Background()

	b.ClaimSession(ctx, "s", "host-x")
	b.ReleaseSession(ctx, "s")
	b.RefreshClaims(ctx, []SessionClaim{{SessionID: "s", SID: "host-x"}})
	b.Forward(ctx, "host-x", "s", &keeperv1.FromSoul{})

	if NewClusterBridge(nil, "kid", testLogger()) != nil {
		t.Fatal("NewClusterBridge with a nil redis client must yield nil")
	}
}
