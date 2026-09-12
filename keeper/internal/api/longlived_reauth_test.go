package api

// Guards for the re-authorization of the long-lived operator channels (NIM-844).
//
// SECURITY invariant, and the reason the file exists:
//
//	An established console session and an open run-events stream STOP when the
//	Archon holding them stops being allowed to hold them.
//
// Before this, both decided access once — the console at the upgrade and per
// `open` frame, the stream in authorizeRunEventsSSE — and never again.
// `apimiddleware.RejectRevoked` refuses the NEXT request, and these channels have
// no next request: a revoked operator kept typing at a root prompt indefinitely
// and kept receiving task payloads for up to sseMaxLifetime. That was the one
// hole in the perimeter NIM-421 made uniform, and it was there by construction
// rather than by omission.
//
// Each direction has a negative control. "The channel closes when access is
// withdrawn" alone is satisfied by a loop that closes every channel on its first
// tick, which would be a far worse defect than the one being fixed — so every
// case below is paired with one where nothing is withdrawn and the channel must
// survive several intervals.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/console"
)

// reauthTestInterval is short enough that a test reaches several checks and long
// enough that it is not racing the goroutine that starts the loop.
const reauthTestInterval = 25 * time.Millisecond

// mutableRBAC answers the console and stream surfaces, and can be changed while a
// channel is open — which is the whole point: the defect was that nothing read it
// a second time.
type mutableRBAC struct {
	mu sync.Mutex
	// revoked mirrors the snapshot's revoked projection.
	revoked bool
	// denyHosts are SIDs this operator may no longer reach with soul.console.
	denyHosts map[string]bool
	// allow is the permission allow-set for the stream, keyed "resource.action".
	allow map[string]bool
	// actionGone makes HoldsAction answer false — `soul.console` is not held in
	// any form, which is the one thing still knowable when the coven read fails.
	actionGone bool
}

func (m *mutableRBAC) Check(_, resource, action string, ctx map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A right that is not held at all is denied in every context — a fake that
	// allowed the scope-aware call while HoldsAction said no would model no
	// enforcer that exists.
	if m.actionGone {
		return errNoConsoleForHost
	}
	if host, ok := ctx["host"]; ok && m.denyHosts[host] {
		return errNoConsoleForHost
	}
	if m.allow == nil {
		return nil
	}
	if m.allow[resource+"."+action] {
		return nil
	}
	return errNoConsoleForHost
}

// HoldsAction answers the existence question the re-check falls back on when a
// host's Coven labels cannot be resolved. Held unless a test takes it away.
func (m *mutableRBAC) HoldsAction(string, string, string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.actionGone
}

func (m *mutableRBAC) withdrawAction() {
	m.mu.Lock()
	m.actionGone = true
	m.mu.Unlock()
}

func (m *mutableRBAC) IsRevoked(string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revoked
}

func (m *mutableRBAC) revoke() {
	m.mu.Lock()
	m.revoked = true
	m.mu.Unlock()
}

func (m *mutableRBAC) denyHost(sid string) {
	m.mu.Lock()
	if m.denyHosts == nil {
		m.denyHosts = map[string]bool{}
	}
	m.denyHosts[sid] = true
	m.mu.Unlock()
}

func (m *mutableRBAC) withdraw(perm string) {
	m.mu.Lock()
	delete(m.allow, perm)
	m.mu.Unlock()
}

// errNoConsoleForHost is the denial this fake returns; only its non-nil-ness is read.
var errNoConsoleForHost = &reauthDenied{}

type reauthDenied struct{}

func (*reauthDenied) Error() string { return "denied" }

// --- console ---

// TestConsoleReauth_RevokedOperatorLosesTheSocket — the case NIM-844 exists for.
// Remove the reauthorize loop and this test hangs at the read: the socket stays
// open and the pty behind it keeps accepting keystrokes.
func TestConsoleReauth_RevokedOperatorLosesTheSocket(t *testing.T) {
	rbac := &mutableRBAC{}
	s := newConsoleTestServer(t, rbac, console.Limits{}, func(d *consoleWSDeps) {
		d.ReauthInterval = reauthTestInterval
	})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")

	rbac.revoke()

	// The socket must say why before it goes: an operator whose terminal dies in
	// silence goes looking for a network fault.
	errFrame := readFrameOfType(t, ws, "error")
	if errFrame["code"] != console.ErrCodeForbidden {
		t.Errorf("error.code = %v, want %s", errFrame["code"], console.ErrCodeForbidden)
	}
	if msg, _ := errFrame["message"].(string); !strings.Contains(msg, "revoked") {
		t.Errorf("error.message = %q, want it to name the revocation", msg)
	}

	// And the shells must actually be gone — a close frame the Hub does not act on
	// would leave the pty running on the host.
	waitFor(t, "the revoked operator's sessions to be reaped", func() bool { return s.hub.Count() == 0 })
}

// TestConsoleReauth_WithdrawnHostScopeLosesOnlyThatPane — the narrowing half. A
// `soul.console on host=` grant losing one host must cost that pane and no
// other: closing the socket would take down sessions the operator is still
// entitled to.
func TestConsoleReauth_WithdrawnHostScopeLosesOnlyThatPane(t *testing.T) {
	rbac := &mutableRBAC{}
	s := newConsoleTestServer(t, rbac, console.Limits{}, func(d *consoleWSDeps) {
		d.ReauthInterval = reauthTestInterval
	})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-a", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-b", "sid": "host-b"})
	readFrameOfType(t, ws, "opened")

	rbac.denyHost("host-b")

	errFrame := readFrameOfType(t, ws, "error")
	if errFrame["session_id"] != "pane-b" {
		t.Fatalf("error names session %v, want pane-b — the wrong pane was closed", errFrame["session_id"])
	}
	if errFrame["code"] != console.ErrCodeForbidden {
		t.Errorf("error.code = %v, want %s", errFrame["code"], console.ErrCodeForbidden)
	}

	waitFor(t, "the withdrawn pane to be reaped", func() bool { return s.hub.Count() == 1 })

	// pane-a has to still work, not merely still exist: the socket is shared and a
	// teardown that took the whole connection would leave this unreadable.
	writeFrame(t, ws, map[string]any{"type": "stdin", "session_id": "pane-a", "data": "aWQK"})
	if n := s.hub.Count(); n != 1 {
		t.Fatalf("live sessions = %d after writing to the surviving pane, want 1", n)
	}
}

// TestConsoleReauth_UnchangedRightsKeepTheSocket — the negative control. Without
// it, a loop that closed every socket on its first tick would pass both tests
// above while making the console unusable.
func TestConsoleReauth_UnchangedRightsKeepTheSocket(t *testing.T) {
	rbac := &mutableRBAC{}
	s := newConsoleTestServer(t, rbac, console.Limits{}, func(d *consoleWSDeps) {
		d.ReauthInterval = reauthTestInterval
	})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")

	// Several intervals, so this is "the loop ran and kept deciding yes" rather
	// than "the loop has not started".
	time.Sleep(6 * reauthTestInterval)

	if n := s.hub.Count(); n != 1 {
		t.Fatalf("live sessions = %d after %v with rights unchanged, want 1 — the re-check is closing sessions it should keep",
			n, 6*reauthTestInterval)
	}
	// Nothing may have been sent either: an error frame on an untouched session
	// would strand the pane in the UI even with the pty alive.
	_ = ws.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, raw, err := ws.ReadMessage(); err == nil {
		var f map[string]any
		_ = json.Unmarshal(raw, &f)
		if f["type"] == "error" {
			t.Fatalf("an untouched session received %s", raw)
		}
	}
}

// --- console: the Coven half, and the case the fix could have broken ---

// mutableCovenReader is [wsCovenReader] that can change under an open socket —
// a label moving off a host, or the read failing outright. The per-`open` tests
// have no use for either; both are what a re-check has to tell apart.
type mutableCovenReader struct {
	mu      sync.Mutex
	covens  map[string][]string
	failErr error
	reads   int
	park    bool
}

// parkReads makes every later read wait for its context instead of answering.
// Switched on AFTER the session is open on purpose: `handleOpen` resolves the
// same labels on the read pump and carries no budget of its own, so a reader that
// parked from the start would hang the open rather than the re-check.
func (r *mutableCovenReader) parkReads() {
	r.mu.Lock()
	r.park = true
	r.mu.Unlock()
}

func (r *mutableCovenReader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

func (r *mutableCovenReader) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("mutableCovenReader: Exec not expected")
}

func (r *mutableCovenReader) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("mutableCovenReader: Query not expected")
}

func (r *mutableCovenReader) QueryRow(ctx context.Context, _ string, args ...any) pgx.Row {
	r.mu.Lock()
	r.reads++
	parking := r.park
	r.mu.Unlock()
	if parking {
		// A pool with no free connection. Returns only when the caller's own
		// budget expires, so a caller without one waits forever.
		<-ctx.Done()
		return wsCovenScanRow{err: ctx.Err()}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failErr != nil {
		return wsCovenScanRow{err: r.failErr}
	}
	sid, _ := args[0].(string)
	covens, ok := r.covens[sid]
	if !ok {
		return wsCovenScanRow{err: pgx.ErrNoRows}
	}
	return wsCovenScanRow{covens: covens}
}

func (r *mutableCovenReader) setCovens(sid string, covens []string) {
	r.mu.Lock()
	r.covens[sid] = covens
	r.mu.Unlock()
}

func (r *mutableCovenReader) failWith(err error) {
	r.mu.Lock()
	r.failErr = err
	r.mu.Unlock()
}

// covenReauthServer — a socket held by a `soul.console on coven=web` role, with a
// real coven read behind it. The ordinary fixture leaves SoulReader nil, which
// makes HostContextsBySID answer `{host}` without a query — so every claim about
// Coven labels would be vacuous there.
func covenReauthServer(t *testing.T, reader *mutableCovenReader) *consoleTestServer {
	t.Helper()
	return newConsoleTestServer(t, covenScopedConsoleRBAC(t, true), console.Limits{},
		func(d *consoleWSDeps) {
			d.ReauthInterval = reauthTestInterval
			d.SoulReader = reader
		})
}

// TestConsoleReauth_CovenGrantKeepsAPaneOnAHostInItsCoven — the positive control
// for the Coven half, and the one that makes the case below mean something. A
// re-check that never reads the labels denies every `coven=` grant, which closes
// the pane just as a real withdrawal does — so "the pane closed" proves nothing on
// its own. Only a pane that SURVIVES proves the labels were fetched and compared.
func TestConsoleReauth_CovenGrantKeepsAPaneOnAHostInItsCoven(t *testing.T) {
	reader := &mutableCovenReader{covens: map[string][]string{"web-01.example.com": {"web"}}}
	s := covenReauthServer(t, reader)
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "web-01.example.com"})
	readFrameOfType(t, ws, "opened")

	time.Sleep(6 * reauthTestInterval)
	if n := s.hub.Count(); n != 1 {
		t.Fatalf("live sessions = %d after %v on a host IN the granted coven, want 1 — the re-check is not resolving the host's labels, so it denies every coven-scoped grant",
			n, 6*reauthTestInterval)
	}
}

// TestConsoleReauth_CovenLabelMovingOffTheHostClosesThePane — the claim
// reauthorize's comment makes about reading the labels AFRESH. Capture the
// contexts at `open` instead and this pane lives on: the permission never
// changed, only the host's membership did.
func TestConsoleReauth_CovenLabelMovingOffTheHostClosesThePane(t *testing.T) {
	reader := &mutableCovenReader{covens: map[string][]string{"web-01.example.com": {"web"}}}
	s := covenReauthServer(t, reader)
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "web-01.example.com"})
	readFrameOfType(t, ws, "opened")

	// The host is relabelled out of the coven the grant names.
	reader.setCovens("web-01.example.com", []string{"db"})

	waitFor(t, "the pane on a host that left the granted coven to be reaped",
		func() bool { return s.hub.Count() == 0 })
}

// TestConsoleReauth_UnreadableCovenKeepsThePane — the case this loop could have
// made WORSE than not existing. `soul.HostContextsBySID` answers an unreadable
// row with the `{host}` context alone, which denies every `coven=` grant; on the
// `open` path that is the right fail-closed, and on this path it would turn one
// exhausted pool into "your permission was withdrawn" on every coven-scoped
// console at once. Swap HostContextsBySIDOrError back for HostContextsBySID in
// reauthorize and this test reaps a pane nobody withdrew.
func TestConsoleReauth_UnreadableCovenKeepsThePane(t *testing.T) {
	reader := &mutableCovenReader{covens: map[string][]string{"web-01.example.com": {"web"}}}
	s := covenReauthServer(t, reader)
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "web-01.example.com"})
	readFrameOfType(t, ws, "opened")

	reader.failWith(errors.New("pool closed"))
	// The loop has to have ASKED and kept the pane anyway: "still 1 session" is
	// also what a goroutine that never started produces.
	waitFor(t, "the re-check to ask the failing reader", func() bool { return reader.count() > 0 })
	time.Sleep(4 * reauthTestInterval)
	if n := s.hub.Count(); n != 1 {
		t.Fatalf("live sessions = %d while the coven read was failing, want 1 — a database blip is being reported to the operator as a withdrawn permission", n)
	}

	// And the decision is only deferred, not abandoned: once the read answers
	// again, the withdrawal it was hiding takes effect.
	reader.failWith(nil)
	reader.setCovens("web-01.example.com", []string{"db"})
	waitFor(t, "the deferred withdrawal to land once the coven read recovers",
		func() bool { return s.hub.Count() == 0 })
}

// TestConsoleReauth_UnreadableCovenStillClosesWhenTheRightIsGoneEntirely — the
// other half of the deferral, and the mistake opposite to the one it fixes.
//
// An unresolved Coven label makes a `coven=`-scoped grant unjudgeable. It says
// nothing about whether `soul.console` is held AT ALL, and that question needs no
// row — so a deferral that skipped it would keep a pane whose permission was
// deleted outright for as long as the pool stayed down. Take the `continue` out
// from behind HoldsAction and this pane survives a withdrawal that no scope could
// have rescued.
func TestConsoleReauth_UnreadableCovenStillClosesWhenTheRightIsGoneEntirely(t *testing.T) {
	rbac := &mutableRBAC{}
	reader := &mutableCovenReader{covens: map[string][]string{"host-a": {"web"}}}
	s := newConsoleTestServer(t, rbac, console.Limits{}, func(d *consoleWSDeps) {
		d.ReauthInterval = reauthTestInterval
		d.SoulReader = reader
	})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")

	// The read stops working AND the right goes away. The first alone defers; the
	// pair must not.
	reader.failWith(errors.New("pool closed"))
	rbac.withdrawAction()

	waitFor(t, "the pane to close once soul.console is not held in any form",
		func() bool { return s.hub.Count() == 0 })
}

// TestConsoleReauth_WedgedCovenReadDoesNotBlockTeardown — the read is bounded, and
// this is what the bound is for. [consoleConn.run] joins the re-authorization
// goroutine before it reaps the ptys, and the loop's context is detached from the
// request, so an unbounded read would hold a socket's worth of root shells open
// for as long as the pool stayed wedged — on the very failure the deferral above
// exists to tolerate. Drop the context timeout and this test hangs instead of
// failing.
func TestConsoleReauth_WedgedCovenReadDoesNotBlockTeardown(t *testing.T) {
	reader := &mutableCovenReader{covens: map[string][]string{"host-a": {"web"}}}
	s := newConsoleTestServer(t, &mutableRBAC{}, console.Limits{}, func(d *consoleWSDeps) {
		d.ReauthInterval = reauthTestInterval
		d.SoulReader = reader
	})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")

	before := reader.count()
	reader.parkReads()
	waitFor(t, "the re-check to reach the parked reader", func() bool { return reader.count() > before })

	// The operator closes the tab while a read is parked.
	_ = ws.Close()

	// socketsActive drops at the END of teardown, after the ptys are reaped — the
	// one observation that says the join on the re-auth goroutine returned.
	waitFor(t, "the socket to tear down while a coven read is parked",
		func() bool { return s.socketsActive(t) == 0 })
}

// TestConsoleReauth_ForgottenHostClosesThePane — the other side of the same
// branch: a row that is GONE is an answer, not a failure to get one, so the pane
// is not defended. Widening the "keep it" branch to every error would strand a
// pane on a host that was forgotten under it.
func TestConsoleReauth_ForgottenHostClosesThePane(t *testing.T) {
	reader := &mutableCovenReader{covens: map[string][]string{"web-01.example.com": {"web"}}}
	s := covenReauthServer(t, reader)
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "web-01.example.com"})
	readFrameOfType(t, ws, "opened")

	reader.mu.Lock()
	delete(reader.covens, "web-01.example.com") // QueryRow now answers pgx.ErrNoRows
	reader.mu.Unlock()

	waitFor(t, "the pane on a forgotten host to be reaped",
		func() bool { return s.hub.Count() == 0 })
}

// TestConsoleReauth_RevokedTeardownIsNotReportedAsAFailure — `socketDiedBadly`
// decides whether the reap line is WARN or INFO, and a revoked-operator teardown
// is a policy decision rather than a malfunction: reauthorize already wrote its own
// WARN naming the Archon, and a second one reading like a fault is what buries the
// real write failures. Revert socketDiedBadly to `reason != CloseSocketClosed` and
// this goes red.
func TestConsoleReauth_RevokedTeardownIsNotReportedAsAFailure(t *testing.T) {
	if socketDiedBadly(console.CloseAccessRevoked) {
		t.Error("a socket closed because access was revoked is reported as a failure — it is a decision Keeper made, and the reauth path already logged it")
	}
	// The controls: the ordinary close is still not a failure and a real one still is.
	if socketDiedBadly(console.CloseSocketClosed) {
		t.Error("the ordinary close became a failure")
	}
	if !socketDiedBadly(console.CloseSocketWriteFailed) {
		t.Error("a failed write stopped being a failure — the one reason that is worth an alert")
	}
}

// TestConsoleDrain_QueuedFrameSurvivesTeardown — the frame explaining a close
// arrives, and arrives socket-scoped, with output already in flight behind it.
//
// What it does NOT do is force the interleaving `drainQueued` exists for. That
// needs the writer to be mid-frame at the instant teardown begins, and a test
// cannot schedule it: a handful of small frames is written out before the revoke
// lands, so removing the drain leaves this green. The drain's claim rests on
// reading the teardown branch — `sendControl` enqueues, then `shutdown` closes
// `done`, and a `select` with both cases ready picks either — not on this test.
// Kept anyway, because "the operator is told why, and told about the socket rather
// than about one pane" is the user-visible half and nothing else asserts it.
func TestConsoleDrain_QueuedFrameSurvivesTeardown(t *testing.T) {
	rbac := &mutableRBAC{}
	s := newConsoleTestServer(t, rbac, console.Limits{}, func(d *consoleWSDeps) {
		d.ReauthInterval = reauthTestInterval
	})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")

	// Queue output the client has not read yet, so the drain has something to do
	// and the `error` frame is not the only thing in the queue.
	for i := 0; i < 8; i++ {
		s.soul.sendChunk("pane-1", []byte("tick\n"), uint64(i+1), 0)
	}
	rbac.revoke()

	// The socket-scoped refusal must arrive THROUGH the backlog.
	errFrame := readFrameOfType(t, ws, "error")
	if errFrame["code"] != console.ErrCodeForbidden {
		t.Fatalf("error.code = %v, want %s", errFrame["code"], console.ErrCodeForbidden)
	}
	if sid, ok := errFrame["session_id"].(string); ok && sid != "" {
		t.Errorf("the revocation frame is session-scoped (session_id=%q); the socket itself is what closed", sid)
	}
}

// --- run-events SSE ---

// sseReauthHarness is [sseTestHarness] with the re-check interval compressed.
func sseReauthHarness(t *testing.T, bus *applybus.EventBus, access runEventsAccess, rbac runEventsRBAC) (string, func(aid string) string) {
	t.Helper()
	srv, mint := sseTestHarness(t, bus, access, rbac, func(d *runEventsDeps) {
		d.ReauthInterval = reauthTestInterval
	})
	return srv.URL, mint
}

// openSSE opens the stream and returns the live body.
func openSSE(t *testing.T, url, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// expectStreamEnds reads until the body ends, which is how the stream reports
// that the subscription is over (no closing frame is invented — see
// streamRunEvents).
func expectStreamEnds(t *testing.T, body io.Reader, complaint string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(complaint)
	}
}

// TestRunEventsReauth_RevokedSubscriberLosesTheStream — the initiator case, and
// the one the initiator short-circuit makes non-obvious: having started the run
// is what lets an operator with no permission at all watch it, so only the
// revocation check can end this stream.
func TestRunEventsReauth_RevokedSubscriberLosesTheStream(t *testing.T) {
	bus := applybus.NewBus(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	rbac := &mutableRBAC{allow: map[string]bool{}}
	const applyID = "01APPLYREVOKED000000000000"
	url, mint := sseReauthHarness(t, bus,
		fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod", StartedByAID: ptrStr("archon-op")}}, rbac)

	resp := openSSE(t, url+"/v1/incarnations/redis-prod/runs/"+applyID+"/events", mint("archon-op"))
	waitSubscribed(t, bus, applyID)

	rbac.revoke()
	expectStreamEnds(t, resp.Body,
		"the stream outlived the revocation of the Archon holding it — RejectRevoked cannot reach a request already inside its handler, so nothing else will")
}

// TestRunEventsReauth_NarrowedSubscriberLosesTheStream — the permission case: a
// watcher who is not the initiator keeps the stream only while
// incarnation.get/history still answers yes.
func TestRunEventsReauth_NarrowedSubscriberLosesTheStream(t *testing.T) {
	bus := applybus.NewBus(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	rbac := &mutableRBAC{allow: map[string]bool{"incarnation.get": true}}
	const applyID = "01APPLYNARROWED00000000000"
	url, mint := sseReauthHarness(t, bus,
		fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod", StartedByAID: ptrStr("archon-else")}}, rbac)

	resp := openSSE(t, url+"/v1/incarnations/redis-prod/runs/"+applyID+"/events", mint("archon-op"))
	waitSubscribed(t, bus, applyID)

	rbac.withdraw("incarnation.get")
	expectStreamEnds(t, resp.Body,
		"the stream outlived the permission that opened it — a narrowing short of revocation must reach it too")
}

// TestRunEventsReauth_UnchangedRightsKeepTheStream — the negative control, and
// the one that says the loop is re-deciding rather than merely expiring streams.
func TestRunEventsReauth_UnchangedRightsKeepTheStream(t *testing.T) {
	bus := applybus.NewBus(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	rbac := &mutableRBAC{allow: map[string]bool{"incarnation.get": true}}
	const applyID = "01APPLYKEPT0000000000000000"
	url, mint := sseReauthHarness(t, bus,
		fakeRunAccess{acc: &applyrun.Access{IncarnationName: "redis-prod", StartedByAID: ptrStr("archon-else")}}, rbac)

	resp := openSSE(t, url+"/v1/incarnations/redis-prod/runs/"+applyID+"/events", mint("archon-op"))
	waitSubscribed(t, bus, applyID)

	time.Sleep(6 * reauthTestInterval)

	// The stream is still there AND still delivering: a subscriber that survived
	// on the bus while the writer had given up would look identical from the
	// server side.
	bus.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindTaskExecuted,
		Payload: map[string]any{"apply_id": applyID},
	})
	ev, _ := readFirstSSEFrame(t, resp.Body)
	if ev != "task.executed" {
		t.Fatalf("event = %q after %v with rights unchanged, want task.executed — the re-check is closing streams it should keep",
			ev, 6*reauthTestInterval)
	}
}

// TestRunEventsStillAuthorized_Matrix pins the decision itself, including the two
// asymmetries that are easy to write backwards: revocation outranks the initiator
// short-circuit, and a nil checker leaves the initiator admitted because the
// opening decision does too (a continuing check stricter than the opening one
// produces a stream that dies for a reason that was already true).
func TestRunEventsStillAuthorized_Matrix(t *testing.T) {
	const sub = "archon-op"
	grantInitiator := runEventsGrant{incarnation: "redis-prod", initiator: true}
	grantWatcher := runEventsGrant{incarnation: "redis-prod"}

	cases := []struct {
		name  string
		deps  *runEventsDeps
		grant runEventsGrant
		want  bool
	}{
		{"initiator-keeps", &runEventsDeps{RBAC: stubRBACChecker{}}, grantInitiator, true},
		{"initiator-revoked-loses", &runEventsDeps{RBAC: stubRBACChecker{revoked: map[string]bool{sub: true}}}, grantInitiator, false},
		{"watcher-with-get-keeps", &runEventsDeps{RBAC: stubRBACChecker{allow: map[string]bool{"incarnation.get": true}}}, grantWatcher, true},
		{"watcher-with-history-keeps", &runEventsDeps{RBAC: stubRBACChecker{allow: map[string]bool{"incarnation.history": true}}}, grantWatcher, true},
		{"watcher-narrowed-loses", &runEventsDeps{RBAC: stubRBACChecker{}}, grantWatcher, false},
		{"watcher-revoked-loses", &runEventsDeps{RBAC: stubRBACChecker{allow: map[string]bool{"incarnation.get": true}, revoked: map[string]bool{sub: true}}}, grantWatcher, false},
		{"nil-rbac-watcher-loses", &runEventsDeps{RBAC: nil}, grantWatcher, false},
		{"nil-rbac-initiator-keeps", &runEventsDeps{RBAC: nil}, grantInitiator, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runEventsStillAuthorized(c.deps, sub, c.grant); got != c.want {
				t.Errorf("runEventsStillAuthorized = %v, want %v", got, c.want)
			}
		})
	}
}

// TestConsoleReauth_CloseReasonIsALabel — CloseAccessRevoked is a Prometheus
// label as well as a sentence (closereason.go), so it has to be in the registry
// rather than merely declared; an unregistered reason collapses to `unknown` and
// the metric loses the bucket that says rights were withdrawn.
func TestConsoleReauth_CloseReasonIsALabel(t *testing.T) {
	if got := console.CloseAccessRevoked.Label(); got != "access_revoked" {
		t.Errorf("CloseAccessRevoked.Label() = %q, want access_revoked (missing from closeReasonText)", got)
	}
	if got := console.CloseAccessRevoked.Text(); got == string(console.CloseAccessRevoked) {
		t.Errorf("CloseAccessRevoked.Text() fell through to the raw value %q — no sentence is registered for it", got)
	}
}
