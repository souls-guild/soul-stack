package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/console"
	"github.com/souls-guild/soul-stack/keeper/internal/console/consoletest"
	keepergrpc "github.com/souls-guild/soul-stack/keeper/internal/grpc"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// End-to-end tests of `GET /v1/console` over a REAL WebSocket against a real
// chi chain (metrics middleware + RequireJWT + RequirePermission), with a fake
// Soul on the other side of the Hub.
//
// The chain matters as much as the handler: every /v1 request is wrapped by
// ResponseWriter recorders, and an upgrade only works if each of them forwards
// Hijack. A handler-only test would pass while production returned 500.

const (
	consoleTestIssuer = "keeper.console.test"
	consoleTestAID    = "archon-console"
)

var consoleTestKey = bytes.Repeat([]byte{0x5c}, 32)

// fakeSoul stands in for a Soul agent: it records what Keeper dispatched and
// can answer back through the Hub, which is exactly what the EventStream
// handler does in production.
type fakeSoul struct {
	hub *console.Hub

	mu      sync.Mutex
	opens   []*keeperv1.ConsoleOpen
	stdin   [][]byte
	resizes []*keeperv1.ConsoleResize
	closes  []*keeperv1.ConsoleClose

	// sidOf remembers which host each session was opened on. Upstream frames
	// must arrive on that host's own stream — the Hub drops a frame whose sid
	// does not own the session, since a session id is a route and the peer cert
	// is the authority (ADR-012(i)).
	sidOf map[string]string

	// autoOpen answers every ConsoleOpen with ConsoleOpened, like a healthy host.
	autoOpen bool
	// openErr makes the dispatch fail (a Soul that is not connected).
	openErr error
	// stdinErr makes keystroke dispatch fail (a congested outbound queue).
	stdinErr error
	// closeDelay stalls the close dispatch, which is how a test can make the
	// teardown tail take an observable amount of time on purpose.
	//
	// [console.Hub.Close] unregisters the session — the thing hub.Count()
	// observes — and only then dispatches the close, closes the recording and
	// writes the audit entry. So the count reaching zero is the START of the
	// teardown tail, not the end of it, and this delay lands squarely inside
	// that window. On a loaded machine the scheduler supplies the same delay
	// for free, which is what NIM-523/NIM-537 were.
	closeDelay time.Duration
}

// setCloseDelay arms the stall. Under the lock, like every other field of this
// fake — and the lock is the whole reason to note it, because today it buys
// nothing. Both callers arm the delay BEFORE s.dial, and the goroutine that
// reads it is the one running consoleConn.run -> Hub.Close -> SendConsoleClose,
// which does not exist until the upgrade: establishing the connection is the
// ordering edge, and a plain write would be correct. It is written this way for
// the test that arms or re-arms the delay on a live socket, where the edge is
// gone and the plain write is a data race — one that -race would report inside
// the guard for a race, which is not a place to spend a session. Do not take
// the lock back out on the strength of the current call sites.
func (f *fakeSoul) setCloseDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeDelay = d
}

func (f *fakeSoul) SendConsoleOpen(ctx context.Context, sid string, msg *keeperv1.ConsoleOpen) error {
	f.mu.Lock()
	if f.openErr != nil {
		err := f.openErr
		f.mu.Unlock()
		return err
	}
	f.opens = append(f.opens, msg)
	if f.sidOf == nil {
		f.sidOf = make(map[string]string)
	}
	f.sidOf[msg.GetSessionId()] = sid
	auto := f.autoOpen
	f.mu.Unlock()

	if auto {
		// Answer from another goroutine: in production ConsoleOpened arrives on
		// the EventStream, which may well beat SendConsoleOpen's return.
		go f.hub.Deliver(context.Background(), sid, &keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
				SessionId: msg.GetSessionId(),
				Pid:       4242,
				Shell:     "/bin/bash",
			}},
		})
	}
	return nil
}

func (f *fakeSoul) SendConsoleStdin(_ context.Context, _ string, msg *keeperv1.ConsoleStdin) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stdinErr != nil {
		return f.stdinErr
	}
	f.stdin = append(f.stdin, msg.GetData())
	return nil
}

func (f *fakeSoul) SendConsoleResize(_ context.Context, _ string, msg *keeperv1.ConsoleResize) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, msg)
	return nil
}

func (f *fakeSoul) SendConsoleClose(_ context.Context, _ string, msg *keeperv1.ConsoleClose) error {
	f.mu.Lock()
	delay := f.closeDelay
	f.mu.Unlock()
	// Outside the lock, like autoOpen's reply: a stall that also froze
	// closedIDs() would be testing the fake rather than the socket.
	if delay > 0 {
		time.Sleep(delay)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, msg)
	return nil
}

// sessionIDs returns the Keeper-minted ids of every dispatched open.
func (f *fakeSoul) sessionIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.opens))
	for _, o := range f.opens {
		out = append(out, o.GetSessionId())
	}
	return out
}

func (f *fakeSoul) closedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.closes))
	for _, c := range f.closes {
		out = append(out, c.GetSessionId())
	}
	return out
}

func (f *fakeSoul) stdinBytes() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.stdin...)
}

// sidFor is the host a session was opened on; unknown ids fall back to the
// default fixture host.
func (f *fakeSoul) sidFor(sessionID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sid, ok := f.sidOf[sessionID]; ok {
		return sid
	}
	return "host-a"
}

// sendChunk pushes pty output up through the Hub.
func (f *fakeSoul) sendChunk(sessionID string, data []byte, seq uint64, dropped uint64) {
	f.hub.Deliver(context.Background(), f.sidFor(sessionID), &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
			SessionId:    sessionID,
			Stream:       keeperv1.ConsoleStream_CONSOLE_STREAM_STDOUT,
			Data:         data,
			Seq:          seq,
			DroppedBytes: dropped,
		}},
	})
}

func (f *fakeSoul) sendExit(sessionID string, code int32, reason keeperv1.ConsoleExitReason) {
	f.hub.Deliver(context.Background(), f.sidFor(sessionID), &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: &keeperv1.ConsoleExit{
			SessionId: sessionID,
			ExitCode:  code,
			Reason:    reason,
		}},
	})
}

// The console gate reads RBAC through TWO surfaces, and they answer different
// questions: HoldsAction ("may this Archon open consoles at all") guards the
// upgrade, Check ("on THIS host") guards each `open` frame. Every fake here
// implements both, or a test would exercise a gate production does not have.
type allowAllRBAC struct{}

func (allowAllRBAC) Check(string, string, string, map[string]string) error { return nil }
func (allowAllRBAC) HoldsAction(string, string, string) bool               { return true }

type denyHostRBAC struct{ deny string }

func (d denyHostRBAC) Check(_, _, _ string, ctx map[string]string) error {
	if ctx["host"] == d.deny {
		return errors.New("forbidden")
	}
	return nil
}
func (denyHostRBAC) HoldsAction(string, string, string) bool { return true }

// hostScopedRBAC models the role the ADR is written for: `soul.console on
// host=<allow>`. It holds the action (so the socket must open) but denies every
// scope-aware Check that names another host — and, critically, denies a Check
// with NO host at all, exactly as the real Enforcer does for an absent
// dimension (ADR-047 §g G1).
type hostScopedRBAC struct{ allow string }

func (h hostScopedRBAC) Check(_, _, _ string, ctx map[string]string) error {
	if host, ok := ctx["host"]; ok && host == h.allow {
		return nil
	}
	return errors.New("forbidden")
}
func (hostScopedRBAC) HoldsAction(string, string, string) bool { return true }

// consoleTestServer is a live HTTP server carrying the real /v1 chain.
type consoleTestServer struct {
	srv  *httptest.Server
	soul *fakeSoul
	hub  *console.Hub
	tok  string
	// reg holds the console collectors, so a test can observe the LAST step of
	// socket teardown rather than an early one (see socketsActive).
	reg *obs.Registry
}

// socketsActive reads keeper_console_sockets_active off the registry.
//
// DecSocketsActive is the final statement of consoleConn.run — after the pumps
// are joined, after every session is reaped and after the reap line is logged,
// and the handler does nothing after calling it (console_ws.go) — so
// this reaching zero is the only observation that means "the socket handler is
// finished" rather than "the socket handler got somewhere". A test asserting
// that something was NOT logged needs exactly that: any earlier vantage point
// makes the absence a statement about the scheduler.
func (s *consoleTestServer) socketsActive(t *testing.T) float64 {
	t.Helper()
	families, err := s.reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather console metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "keeper_console_sockets_active" {
			continue
		}
		for _, m := range f.GetMetric() {
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("keeper_console_sockets_active is not registered — this server was built without console metrics")
	return 0
}

// consoleRBAC is what the console plane actually needs from RBAC: the
// existence gate for the socket and the scope-aware check per session.
type consoleRBAC interface {
	apimiddleware.PermissionChecker
	apimiddleware.ActionHolder
}

func newConsoleTestServer(t *testing.T, rbac consoleRBAC, limits console.Limits, opts ...func(*consoleWSDeps)) *consoleTestServer {
	t.Helper()
	return newConsoleTestServerWriteWait(t, rbac, limits, 0, opts...)
}

// newConsoleTestServerWriteWait is newConsoleTestServer with the socket's write
// budget compressed, so a test can reach its expiry without waiting out the
// production value. Zero keeps the default.
func newConsoleTestServerWriteWait(t *testing.T, rbac consoleRBAC, limits console.Limits, writeWait time.Duration, opts ...func(*consoleWSDeps)) *consoleTestServer {
	t.Helper()
	return newConsoleTestServerLogging(t, rbac, limits, writeWait,
		slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
}

// newConsoleTestServerLogging is newConsoleTestServerWriteWait with Keeper's own
// logger supplied, for tests that assert on what the socket plane REPORTS
// rather than on what it does.
func newConsoleTestServerLogging(t *testing.T, rbac consoleRBAC, limits console.Limits, writeWait time.Duration, logger *slog.Logger, opts ...func(*consoleWSDeps)) *consoleTestServer {
	t.Helper()

	soul := &fakeSoul{autoOpen: true}
	// Real collectors, not the nil-safe no-op: the socket gauge is the only
	// handle a test has on the END of teardown (see socketsActive).
	reg := obs.NewRegistry()
	metrics := console.RegisterMetrics(reg)
	// The REAL recorder over an in-memory store: recording is mandatory
	// (ADR-0074(g)), so a socket test that skipped it would be testing a Keeper
	// that cannot exist. The size cap is off here — the backpressure test floods
	// far past it on purpose, and the cap has its own guard in the console
	// package; leaving it on would make one contract's test fail on the other's.
	recorder, err := console.NewRecorder(consoletest.NewStore(),
		console.StaticRecorderConfig(console.RecorderConfig{MaxBytes: -1}), logger)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	hub, err := console.NewHub(console.HubDeps{
		Dispatcher: soul,
		Recorder:   recorder,
		Limits:     console.StaticLimits(limits),
		Metrics:    metrics,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	soul.hub = hub

	verifier, err := keeperjwt.NewVerifier(consoleTestKey, consoleTestIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	issuer, err := keeperjwt.NewIssuer(consoleTestKey, consoleTestIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := issuer.Issue(consoleTestAID, []string{"cluster-admin"}, time.Hour, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// The production chain: HTTP metrics recorder (which wraps the
	// ResponseWriter) + JWT + the socket-level permission gate.
	httpMetrics := obs.RegisterHTTPMetrics(reg)
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(httpMetrics.MiddlewareForPath(func(*http.Request) string { return "/v1/console" }))
		r.Use(apimiddleware.RequireJWT(verifier))
		// The EXISTENCE gate, exactly as router.go mounts it — a scope-aware
		// Check here would deny host-scoped roles before they can name a host.
		r.With(apimiddleware.RequireAction(rbac, "soul", "console")).
			Group(func(r chi.Router) {
				deps := &consoleWSDeps{
					Hub: hub, Enforcer: rbac, Logger: logger, WriteWait: writeWait,
					Metrics: metrics,
				}
				for _, o := range opts {
					o(deps)
				}
				registerConsoleWS(r, deps)
			})
	})

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &consoleTestServer{srv: srv, soul: soul, hub: hub, tok: tok, reg: reg}
}

// dial opens an operator socket the way the browser client does.
func (s *consoleTestServer) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	return s.dialWith(t, websocket.DefaultDialer)
}

// dialWith is dial with the caller's dialer, for tests that need to control the
// handshake itself (compression, byte counting).
func (s *consoleTestServer) dialWith(t *testing.T, d *websocket.Dialer) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/v1/console"
	ws, resp, err := d.Dial(url, http.Header{
		"Sec-WebSocket-Protocol": []string{console.Subprotocol + ", " + console.BearerSubprotocolPrefix + s.tok},
	})
	if err != nil {
		// Report the server's own words, not just the status: a bare
		// "bad handshake (401)" says nothing about WHICH gate refused, and this
		// dial passes through JWT, the existence gate and the upgrader.
		status, body := 0, ""
		if resp != nil {
			status = resp.StatusCode
			if b, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<10)); readErr == nil {
				body = strings.TrimSpace(string(b))
			}
			_ = resp.Body.Close()
		}
		t.Fatalf("dial: %v (status %d, body %q)", err, status, body)
	}
	if got := ws.Subprotocol(); got != console.Subprotocol {
		t.Fatalf("negotiated subprotocol = %q, want %q", got, console.Subprotocol)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func writeFrame(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	if err := ws.WriteJSON(v); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// readFrame reads one server frame as a generic map.
func readFrame(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode frame %q: %v", raw, err)
	}
	return m
}

// readFrameOfType skips frames until one of the wanted type arrives.
func readFrameOfType(t *testing.T, ws *websocket.Conn, want string) map[string]any {
	t.Helper()
	for i := 0; i < 50; i++ {
		f := readFrame(t, ws)
		if f["type"] == want {
			return f
		}
	}
	t.Fatalf("no %q frame within 50 frames", want)
	return nil
}

// waitForRecord polls until a WARN+ record whose message contains sub is
// captured, and returns it.
//
// Separate from waitFor because a line that never arrives is diagnosed by what
// DID arrive, and waitFor's timeout cannot carry that: its description is built
// before the wait starts. A test that fails with "timed out waiting for the
// reap log" sends the reader to the source; one that fails with the four lines
// the socket actually logged usually does not.
//
// Every assertion that a line WAS logged belongs behind this rather than behind
// a bare find() — the negative case cannot use it and does not (a line that is
// never expected is never waited for; it settles on a later vantage point
// instead, see socketsActive). The two lines this file cares about are written
// in different goroutines and strictly ordered, and hub.Count() — the thing
// tests used to wait on — drops at the FIRST step of teardown, with the close
// dispatch, the recording close and the audit write still to come (NIM-523,
// NIM-537).
func waitForRecord(t *testing.T, logs *warnCapture, sub, complaint string) slog.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rec, ok := logs.find(sub); ok {
			return rec
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s; logged:\n%s", complaint, logs.dump())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- the happy path ---

func TestConsoleWS_OpenStdinChunkExit(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{
		"type": "open", "session_id": "pane-1", "sid": "host-a.example.com",
		"cols": 120, "rows": 40,
	})

	opened := readFrameOfType(t, ws, "opened")
	if opened["session_id"] != "pane-1" {
		t.Fatalf("opened.session_id = %v, want the CLIENT id pane-1", opened["session_id"])
	}
	if opened["sid"] != "host-a.example.com" {
		t.Fatalf("opened.sid = %v", opened["sid"])
	}
	if opened["pid"] != float64(4242) {
		t.Fatalf("opened.pid = %v, want 4242", opened["pid"])
	}

	// The Soul side must NOT have seen the client's id — it gets a ULID.
	ids := s.soul.sessionIDs()
	if len(ids) != 1 {
		t.Fatalf("dispatched opens = %d, want 1", len(ids))
	}
	if ids[0] == "pane-1" {
		t.Fatal("the client session_id leaked to the Soul side; it must be a Keeper-minted ULID")
	}
	if len(ids[0]) != 26 {
		t.Fatalf("Soul-side session id %q is not a 26-char ULID", ids[0])
	}

	// Keystrokes reach the pty verbatim, including control characters.
	writeFrame(t, ws, map[string]any{
		"type": "stdin", "session_id": "pane-1",
		"data": base64.StdEncoding.EncodeToString([]byte("top\r\x03")),
	})
	waitFor(t, "stdin to reach the soul", func() bool { return len(s.soul.stdinBytes()) == 1 })
	if got := string(s.soul.stdinBytes()[0]); got != "top\r\x03" {
		t.Fatalf("stdin bytes = %q, want %q", got, "top\r\x03")
	}

	// Output comes back base64-encoded and byte-exact — including bytes JSON
	// cannot carry raw (NUL, a lone high byte, ANSI escapes).
	payload := []byte("\x1b[2J\x00\xff done")
	s.soul.sendChunk(ids[0], payload, 1, 0)
	chunk := readFrameOfType(t, ws, "chunk")
	if chunk["session_id"] != "pane-1" {
		t.Fatalf("chunk.session_id = %v, want pane-1", chunk["session_id"])
	}
	if chunk["stream"] != "stdout" {
		t.Fatalf("chunk.stream = %v, want stdout", chunk["stream"])
	}
	got, err := base64.StdEncoding.DecodeString(chunk["data"].(string))
	if err != nil {
		t.Fatalf("chunk data is not base64: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("chunk bytes = %q, want %q", got, payload)
	}

	// Resize must survive to TIOCSWINSZ.
	writeFrame(t, ws, map[string]any{"type": "resize", "session_id": "pane-1", "cols": 200, "rows": 50})
	waitFor(t, "resize to reach the soul", func() bool {
		s.soul.mu.Lock()
		defer s.soul.mu.Unlock()
		return len(s.soul.resizes) == 1
	})

	// The shell exits: the pane gets a terminal frame and the slot is freed.
	s.soul.sendExit(ids[0], 130, keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED)
	exit := readFrameOfType(t, ws, "exit")
	if exit["session_id"] != "pane-1" {
		t.Fatalf("exit.session_id = %v", exit["session_id"])
	}
	if exit["code"] != float64(130) {
		t.Fatalf("exit.code = %v, want 130", exit["code"])
	}
	if exit["reason"] != "process_exited" {
		t.Fatalf("exit.reason = %v, want process_exited", exit["reason"])
	}
	waitFor(t, "the session slot to be released", func() bool { return s.hub.Count() == 0 })
}

// One socket must carry independent sessions — that is the whole point of a
// multi-console wall.
func TestConsoleWS_MultiplexesSessions(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	for _, pane := range []string{"pane-1", "pane-2", "pane-3"} {
		writeFrame(t, ws, map[string]any{"type": "open", "session_id": pane, "sid": pane + ".example.com"})
		readFrameOfType(t, ws, "opened")
	}
	if s.hub.Count() != 3 {
		t.Fatalf("live sessions = %d, want 3", s.hub.Count())
	}

	ids := s.soul.sessionIDs()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate Soul-side session id %q — ids must be unique per stream", id)
		}
		seen[id] = true
	}

	// Closing one pane leaves the others alone.
	writeFrame(t, ws, map[string]any{"type": "close", "session_id": "pane-2"})
	waitFor(t, "pane-2 to close", func() bool { return s.hub.Count() == 2 })
	if closed := s.soul.closedIDs(); len(closed) != 1 || closed[0] != ids[1] {
		t.Fatalf("closed ids = %v, want exactly the second session", closed)
	}
}

// --- kill on disconnect ---

func TestConsoleWS_SocketCloseReapsEverySession(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	for _, pane := range []string{"a", "b", "c"} {
		writeFrame(t, ws, map[string]any{"type": "open", "session_id": pane, "sid": pane + ".example.com"})
		readFrameOfType(t, ws, "opened")
	}
	ids := s.soul.sessionIDs()

	// The operator's browser goes away.
	_ = ws.Close()

	waitFor(t, "all sessions to be reaped", func() bool { return s.hub.Count() == 0 })
	waitFor(t, "a close to be dispatched for each pty", func() bool { return len(s.soul.closedIDs()) == 3 })

	closed := map[string]bool{}
	for _, id := range s.soul.closedIDs() {
		closed[id] = true
	}
	for _, id := range ids {
		if !closed[id] {
			t.Fatalf("session %q was never closed on the Soul side — an orphaned root shell", id)
		}
	}
}

// The reap must not depend on a graceful close: a peer that vanishes without a
// close frame (a dropped laptop) is the common case.
func TestConsoleWS_AbruptDisconnectReapsSessions(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")

	// Kill the TCP connection under the WebSocket, with no close handshake.
	if err := ws.UnderlyingConn().Close(); err != nil {
		t.Fatalf("close underlying conn: %v", err)
	}

	waitFor(t, "the session to be reaped after an abrupt drop", func() bool { return s.hub.Count() == 0 })
}

// --- backpressure ---

// A flooding console must not grow Keeper's memory without bound, and the loss
// must be reported rather than silently splicing the byte stream.
func TestConsoleWS_BackpressureDropsChunksAndReportsBytes(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	id := s.soul.sessionIDs()[0]

	// Flood far past the socket queue while the client reads nothing. The
	// volume has to exceed the kernel socket buffers too, or the writer would
	// quietly absorb everything and nothing would ever queue up.
	const flood = consoleOutQueueDepth * 8
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for i := 0; i < flood; i++ {
		s.soul.sendChunk(id, chunk, uint64(i+1), 0)
	}

	// Now drain. Some chunks are gone — that is the contract — but the loss
	// must be reported, and reported even though the flood has ENDED: the
	// report cannot ride on "the next chunk", because there is no next chunk.
	//
	// The budgets are deliberately generous. flushDropped only gets a queue slot
	// once the client has drained what is already queued, so this loop has to read
	// out consoleOutQueueDepth frames of 64 KiB before the marker can arrive — tens
	// of MB of JSON under -race. A tight per-read deadline turns "the box is busy"
	// into a read error, and the assertions below would then blame the product for
	// a report that simply had not been reached yet.
	var totalDropped float64
	var chunks int
	var readErr error
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && totalDropped == 0 {
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, raw, err := ws.ReadMessage()
		if err != nil {
			readErr = err
			break
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if m["type"] != "chunk" {
			continue
		}
		chunks++
		if d, ok := m["dropped_bytes"].(float64); ok {
			totalDropped += d
		}
	}

	if chunks == 0 {
		t.Fatal("no chunks delivered at all - backpressure must drop, not deadlock")
	}
	if chunks >= flood {
		t.Fatalf("delivered %d of %d chunks - nothing was dropped, so the queue is unbounded", chunks, flood)
	}
	if totalDropped == 0 {
		t.Fatalf("chunks were dropped but dropped_bytes never reported it - the client would splice a corrupted stream (read %d chunks, stopped on: %v)", chunks, readErr)
	}
}

// A flood on one session must not wedge the EventStream goroutine, which the
// apply cycle shares. Delivery has to stay non-blocking even with nobody
// reading the socket.
func TestConsoleWS_FloodDoesNotBlockDelivery(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	id := s.soul.sessionIDs()[0]

	done := make(chan struct{})
	go func() {
		defer close(done)
		payload := bytes.Repeat([]byte("y"), 32<<10)
		for i := 0; i < consoleOutQueueDepth*20; i++ {
			s.soul.sendChunk(id, payload, uint64(i+1), 0)
		}
	}()

	// A blocked Deliver never finishes, so the budget only has to outlast the
	// honest cost of pushing 160 MB through the send path under -race on a busy
	// box — being generous here costs nothing and stops a loaded machine from
	// reading as a wedged EventStream goroutine.
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Deliver blocked on a full socket queue - this would stall apply traffic on the shared stream")
	}

	// Tear the socket down before returning. t.Cleanup would close the client end
	// eventually, but until the session is reaped this Keeper keeps pushing the
	// tail of a 160 MB flood into a socket nobody reads — and the next test in the
	// package then starts against a busy server and sees ITS socket go quiet.
	// Leaving that to cleanup is what made the backpressure test flaky.
	_ = ws.Close()
	waitFor(t, "the flooded session to be reaped", func() bool { return s.hub.Count() == 0 })
}

// --- write-side liveness (NIM-242) ---

// The write budget must not be a stricter liveness rule than the pong budget.
//
// Backpressure fills the socket buffer BY CONSTRUCTION — that is the state the
// drop machinery exists for — so a write parks for as long as the operator
// takes to drain. gorilla's connection is unusable after a failed write, so a
// write budget shorter than pongWait means "a browser that stopped reading for
// that long loses every pty on this socket", while the read side still holds
// the very same peer to be alive. Two contradicting liveness rules, and the
// stricter one wins silently. There is one peer, so there is one budget.
func TestConsoleWS_WriteBudgetIsNoStricterThanTheLivenessBudget(t *testing.T) {
	if consoleWriteWait < consolePongWait {
		t.Fatalf("consoleWriteWait = %v is shorter than consolePongWait = %v: an operator who merely stopped reading for %v loses every session on the socket, while the read side still considers that peer alive",
			consoleWriteWait, consolePongWait, consoleWriteWait)
	}
}

// When the write budget DOES expire, the socket must die loudly.
//
// This is the NIM-242 failure itself: the writer gave up, but nothing tore the
// socket down — the read pump stayed parked in ReadMessage, so the connection
// stayed open with nobody writing to it. The operator's wall froze mid-stream,
// no loss report could ever arrive, and every root shell behind it kept running
// until the pong deadline finally noticed a minute later.
func TestConsoleWS_WriteBudgetExpiryClosesTheSocketInsteadOfGoingSilent(t *testing.T) {
	// Compressed budget: the invariant is what happens WHEN it expires, and the
	// production value is deliberately too generous to sit and wait for.
	s := newConsoleTestServerWriteWait(t, allowAllRBAC{}, console.Limits{}, 300*time.Millisecond)
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	id := s.soul.sessionIDs()[0]

	// Past the kernel socket buffers so the writer parks, but well inside the
	// queue so nothing is dropped: the writer has to block on the SOCKET, which
	// is what the budget bounds. The client reads nothing from here on.
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for i := 0; i < consoleOutQueueDepth/2; i++ {
		s.soul.sendChunk(id, chunk, uint64(i+1), 0)
	}

	waitFor(t, "the sessions to be reaped once the writer gave up",
		func() bool { return s.hub.Count() == 0 })

	// And the peer must SEE the socket end. Draining now yields whatever the
	// kernel already buffered and then the close; a read timeout here means the
	// connection was left open with a dead writer behind it.
	var readErr error
	for readErr == nil {
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, readErr = ws.ReadMessage()
	}
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatalf("client read timed out: the socket was left open with no writer behind it (%v)", readErr)
	}
	_ = ws.Close()
}

// A writer parked mid-frame must not hold the teardown hostage.
//
// The generous write budget above makes this half mandatory: when the READ side
// ends first — the operator closed the tab while their socket was still backed
// up — the ptys must not wait out that whole budget before being reaped. A
// writer parked inside a socket write cannot reach `done` to learn any of this,
// so teardown takes the socket away from it instead.
func TestConsoleWS_TeardownDoesNotWaitOutAParkedWriter(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	id := s.soul.sessionIDs()[0]

	// Park the writer on the socket: past the kernel buffers, inside the queue,
	// and the client reads none of it.
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for i := 0; i < consoleOutQueueDepth/2; i++ {
		s.soul.sendChunk(id, chunk, uint64(i+1), 0)
	}

	// The operator closes the tab. That is a WRITE, so it lands even though this
	// client never reads: the server's read pump ends while its writer is still
	// parked on the backlog.
	if err := ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("send close: %v", err)
	}

	waitFor(t, "the sessions to be reaped without waiting out the write budget",
		func() bool { return s.hub.Count() == 0 })
	_ = ws.Close()
}

// --- wire compression (NIM-274) ---

// countingConn tallies what actually crossed the socket, below the WebSocket
// framing — the only place the compression is observable from a test.
type countingConn struct {
	net.Conn
	read *atomic.Int64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// pty output must not go out uncompressed.
//
// Terminal output is enormously redundant, and it is the only high-volume
// traffic Keeper serves to a browser. Sending it as raw base64 JSON is what
// makes an ordinary `find /` outrun the socket and start costing chunks — so
// the drop machinery fires on a flood that would comfortably fit compressed.
//
// This measures bytes on the wire rather than asserting the flag, because the
// flag is not the contract: gorilla negotiates the extension at the handshake
// and only compresses data frames, so a change in either would leave the flag
// set and the output uncompressed.
func TestConsoleWS_PtyOutputIsCompressedOnTheWire(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})

	var wire atomic.Int64
	ws := s.dialWith(t, &websocket.Dialer{
		EnableCompression: true,
		NetDial: func(network, addr string) (net.Conn, error) {
			conn, err := net.Dial(network, addr)
			if err != nil {
				return nil, err
			}
			return countingConn{Conn: conn, read: &wire}, nil
		},
	})

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	id := s.soul.sessionIDs()[0]

	// Realistic terminal output: repetitive, which is exactly why compressing it
	// pays. Well inside the queue so nothing is dropped — this measures the wire,
	// not backpressure.
	chunk := bytes.Repeat([]byte("drwxr-xr-x 2 root root 4096 Jul 28 10:31 /usr/share/doc\n"), 512)
	const chunks = 16
	payload := int64(len(chunk)) * chunks

	start := wire.Load()
	for i := 0; i < chunks; i++ {
		s.soul.sendChunk(id, chunk, uint64(i+1), 0)
	}
	for got := 0; got < chunks; {
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, raw, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read chunk %d of %d: %v", got, chunks, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if m["type"] == "chunk" {
			got++
		}
	}
	onWire := wire.Load() - start
	t.Logf("%d bytes of pty output crossed the wire as %d (x%.1f)", payload, onWire,
		float64(payload)/float64(onWire))

	// Uncompressed this would exceed the payload itself — base64 alone is 4/3 of
	// it. Halving is a floor far below the ~6x measured on real terminal output;
	// the guard is "compressed at all", not a benchmark.
	if onWire > payload/2 {
		t.Fatalf("%d bytes of pty output crossed the wire as %d bytes - not compressed (base64 JSON alone would be about %d)",
			payload, onWire, payload*4/3)
	}

	// Tear the session down before returning, so the next test does not start
	// against a Keeper still draining this one.
	_ = ws.Close()
	waitFor(t, "the session to be reaped", func() bool { return s.hub.Count() == 0 })
}

// --- freshness under flood (NIM-254) ---

// floodUntilQueueIsFull pushes chunks past the socket queue with nobody reading,
// and returns the session id. The dial must be the uncompressed one: with
// permessage-deflate the writer clears repetitive output faster than this can
// produce it, and the queue never fills at all (measured in NIM-254).
func floodUntilQueueIsFull(t *testing.T, s *consoleTestServer, n int) string {
	t.Helper()
	id := s.soul.sessionIDs()[0]
	chunk := bytes.Repeat([]byte("stale output nobody will ever read\n"), 1900)
	for i := 0; i < n; i++ {
		s.soul.sendChunk(id, chunk, uint64(i+1), 0)
	}
	return id
}

// A terminal is worth reading because it is CURRENT. Under a sustained flood the
// socket must give up its oldest queued output rather than refuse the newest —
// otherwise the operator is pinned to a screen from minutes ago while the host
// races on, and the newest output, the only part they actually want, is exactly
// what gets thrown away.
func TestConsoleWS_FloodKeepsTheNewestOutputNotTheOldest(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	id := floodUntilQueueIsFull(t, s, consoleOutQueueDepth*8)

	// The last thing the host said. Under drop-the-newest this is precisely the
	// chunk that never arrives.
	const sentinel = "THE-LATEST-LINE-THE-OPERATOR-NEEDS"
	s.soul.sendChunk(id, []byte(sentinel), 9999, 0)

	var seen bool
	for !seen {
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, raw, err := ws.ReadMessage()
		if err != nil {
			break
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if m["type"] != "chunk" {
			continue
		}
		data, _ := m["data"].(string)
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			t.Fatalf("decode chunk data: %v", err)
		}
		seen = bytes.Contains(decoded, []byte(sentinel))
	}
	if !seen {
		t.Fatal("the newest chunk never reached the operator - the queue refused it and kept stale output instead")
	}

	_ = ws.Close()
	waitFor(t, "the flooded session to be reaped", func() bool { return s.hub.Count() == 0 })
}

// Making room must never come out of a lifecycle frame, and a congested queue
// must no longer cost the whole socket.
//
// `exit` is the frame that frees the pane; losing it leaves a terminal live
// forever. Before NIM-254 a full queue could not take one at all, so the socket
// was closed and every session behind it reaped — a flood on ONE pane killing
// the operator's whole wall. Now the control frame displaces stale output, which
// is the cheaper thing to lose by any measure.
func TestConsoleWS_CongestedQueueDeliversExitInsteadOfKillingTheSocket(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	id := floodUntilQueueIsFull(t, s, consoleOutQueueDepth*8)

	s.soul.sendExit(id, 0, keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED)

	var exited bool
	for !exited {
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, raw, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("socket died before the exit arrived (%v) - a congested queue must not cost the socket", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		exited = m["type"] == "exit" && m["session_id"] == "pane-1"
	}

	_ = ws.Close()
	waitFor(t, "the session slot to be released", func() bool { return s.hub.Count() == 0 })
}

// --- limits ---

func TestConsoleWS_PerOperatorLimit(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{MaxSessionsPerAID: 2})
	ws := s.dial(t)

	for _, pane := range []string{"a", "b"} {
		writeFrame(t, ws, map[string]any{"type": "open", "session_id": pane, "sid": pane + ".example.com"})
		readFrameOfType(t, ws, "opened")
	}

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "c", "sid": "c.example.com"})
	errFrame := readFrameOfType(t, ws, "error")
	if errFrame["code"] != console.ErrCodeLimitExceeded {
		t.Fatalf("error.code = %v, want %s", errFrame["code"], console.ErrCodeLimitExceeded)
	}
	if errFrame["session_id"] != "c" {
		t.Fatalf("the error must be scoped to the refused pane, got session_id=%v", errFrame["session_id"])
	}
	if s.hub.Count() != 2 {
		t.Fatalf("live sessions = %d, want the cap of 2", s.hub.Count())
	}

	// Closing one frees a slot immediately — an operator at the cap must be
	// able to reopen without waiting for anything.
	writeFrame(t, ws, map[string]any{"type": "close", "session_id": "a"})
	waitFor(t, "the slot to free", func() bool { return s.hub.Count() == 1 })
	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "d", "sid": "d.example.com"})
	readFrameOfType(t, ws, "opened")
}

// --- auth and RBAC ---

func TestConsoleWS_NoTokenRejected(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	url := "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/v1/console"

	_, resp, err := websocket.DefaultDialer.Dial(url, http.Header{
		"Sec-WebSocket-Protocol": []string{console.Subprotocol},
	})
	if err == nil {
		t.Fatal("dial without a token succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

func TestConsoleWS_WithoutPermissionRejectedBeforeUpgrade(t *testing.T) {
	s := newConsoleTestServer(t, denyAllRBAC{}, console.Limits{})
	url := "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/v1/console"

	_, resp, err := websocket.DefaultDialer.Dial(url, http.Header{
		"Sec-WebSocket-Protocol": []string{console.Subprotocol + ", " + console.BearerSubprotocolPrefix + s.tok},
	})
	if err == nil {
		t.Fatal("dial without soul.console succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", resp)
	}
}

// REGRESSION (ADR-0074(c)): the upgrade gate must be existence-only. With a
// scope-aware Check and no host in context, a role scoped to `host=<sid>` —
// the very shape the feature exists to serve — is denied the socket before it
// can name a host. The bug shipped in the first cut and survived every live
// test, because those ran as cluster-admin holding `*`.
func TestConsoleWS_HostScopedRoleCanOpenTheSocket(t *testing.T) {
	const allowed = "web-01.example.com"
	s := newConsoleTestServer(t, hostScopedRBAC{allow: allowed}, console.Limits{})

	// The upgrade itself must succeed: HoldsAction is true even though a
	// Check with no host would deny.
	ws := s.dial(t)

	// And the per-host scope still bites on a host outside the role.
	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "p1", "sid": "db-99.example.com"})
	errFrame := readFrameOfType(t, ws, "error")
	if errFrame["code"] != console.ErrCodeForbidden {
		t.Fatalf("error.code = %v, want %s for a host outside the role", errFrame["code"], console.ErrCodeForbidden)
	}

	// The host the role does name works on the same socket.
	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "p2", "sid": allowed})
	readFrameOfType(t, ws, "opened")
	if s.hub.Count() != 1 {
		t.Fatalf("live sessions = %d, want the in-scope one", s.hub.Count())
	}
}

type denyAllRBAC struct{}

func (denyAllRBAC) Check(string, string, string, map[string]string) error {
	return errors.New("forbidden")
}
func (denyAllRBAC) HoldsAction(string, string, string) bool { return false }

// The socket-level gate cannot see the target host, so a per-host scope has to
// be re-checked when the `open` frame names it.
func TestConsoleWS_PerHostScopeCheckedOnOpen(t *testing.T) {
	s := newConsoleTestServer(t, denyHostRBAC{deny: "secret-host"}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "secret-host"})
	errFrame := readFrameOfType(t, ws, "error")
	if errFrame["code"] != console.ErrCodeForbidden {
		t.Fatalf("error.code = %v, want %s", errFrame["code"], console.ErrCodeForbidden)
	}
	if s.hub.Count() != 0 {
		t.Fatal("a session was created for a host the operator may not reach")
	}

	// A host within scope still works on the same socket.
	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-2", "sid": "allowed-host"})
	readFrameOfType(t, ws, "opened")
}

// --- malformed input ---

func TestConsoleWS_BadFrameDoesNotKillHealthySessions(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")

	for _, bad := range []string{
		`{"type":"stdin"}`,                                   // no session_id
		`{"type":"nonsense","session_id":"pane-1"}`,          // unknown type
		`{"type":"stdin","session_id":"pane-1","data":"!!"}`, // not base64
		`not json at all`,
	} {
		if err := ws.WriteMessage(websocket.TextMessage, []byte(bad)); err != nil {
			t.Fatalf("write %q: %v", bad, err)
		}
		errFrame := readFrameOfType(t, ws, "error")
		if errFrame["code"] != console.ErrCodeBadFrame {
			t.Fatalf("for %q got code %v, want %s", bad, errFrame["code"], console.ErrCodeBadFrame)
		}
	}

	// The healthy session on the same socket is untouched.
	if s.hub.Count() != 1 {
		t.Fatalf("live sessions = %d, want the healthy one to survive", s.hub.Count())
	}
	id := s.soul.sessionIDs()[0]
	s.soul.sendChunk(id, []byte("still alive"), 1, 0)
	chunk := readFrameOfType(t, ws, "chunk")
	if chunk["session_id"] != "pane-1" {
		t.Fatalf("chunk.session_id = %v", chunk["session_id"])
	}
}

func TestConsoleWS_DuplicateSessionIDRefused(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-b"})

	errFrame := readFrameOfType(t, ws, "error")
	if errFrame["code"] != console.ErrCodeDuplicateSession {
		t.Fatalf("error.code = %v, want %s", errFrame["code"], console.ErrCodeDuplicateSession)
	}
	if s.hub.Count() != 1 {
		t.Fatalf("live sessions = %d, want 1", s.hub.Count())
	}
}

func TestConsoleWS_OpenAgainstOfflineSoul(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	s.soul.mu.Lock()
	s.soul.openErr = errors.New("grpc: no active EventStream for sid")
	s.soul.mu.Unlock()

	ws := s.dial(t)
	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "gone-host"})

	errFrame := readFrameOfType(t, ws, "error")
	if errFrame["code"] != console.ErrCodeSoulOffline {
		t.Fatalf("error.code = %v, want %s", errFrame["code"], console.ErrCodeSoulOffline)
	}
	// A failed dispatch must not leave a registered session behind, or the
	// operator's cap would leak a slot on every offline host they try.
	if s.hub.Count() != 0 {
		t.Fatalf("live sessions = %d after a failed open, want 0", s.hub.Count())
	}
}

// A congested per-SID queue is NOT an offline Soul: the host is healthy and the
// operator should retry, not go hunting for a dead agent. Reporting it as
// `soul_offline` was the misleading behaviour a live-stand run surfaced.
func TestConsoleWS_CongestedQueueIsNotReportedAsOffline(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")

	// The dispatcher now behaves like a full outbound queue.
	s.soul.mu.Lock()
	s.soul.stdinErr = fmt.Errorf("%w: host-a", keepergrpc.ErrOutboundQueueFull)
	s.soul.mu.Unlock()

	writeFrame(t, ws, map[string]any{
		"type": "stdin", "session_id": "pane-1",
		"data": base64.StdEncoding.EncodeToString([]byte("x")),
	})
	errFrame := readFrameOfType(t, ws, "error")
	if errFrame["code"] != console.ErrCodeBusy {
		t.Fatalf("error.code = %v, want %s — a full queue must not read as an offline host",
			errFrame["code"], console.ErrCodeBusy)
	}
	// The session survives: the keystroke was refused, not the console.
	if s.hub.Count() != 1 {
		t.Fatalf("live sessions = %d, want the session to survive a refused keystroke", s.hub.Count())
	}
}

// --- leaks ---

// Every socket spawns a writer goroutine and every session holds Hub state.
// Repeated connect/disconnect cycles must return both to the baseline.
func TestConsoleWS_NoGoroutineOrSessionLeakAcrossSockets(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{})

	// Warm up once so one-off runtime goroutines are not counted as a leak.
	warm := s.dial(t)
	writeFrame(t, warm, map[string]any{"type": "open", "session_id": "warm", "sid": "host-warm"})
	readFrameOfType(t, warm, "opened")
	_ = warm.Close()
	waitFor(t, "warm-up teardown", func() bool { return s.hub.Count() == 0 })

	settle(t)
	before := runtime.NumGoroutine()

	for i := 0; i < 12; i++ {
		ws := s.dial(t)
		for _, pane := range []string{"p1", "p2"} {
			writeFrame(t, ws, map[string]any{"type": "open", "session_id": pane, "sid": pane + ".example.com"})
			readFrameOfType(t, ws, "opened")
		}
		_ = ws.Close()
		waitFor(t, "sessions to drain between cycles", func() bool { return s.hub.Count() == 0 })
	}

	settle(t)
	after := runtime.NumGoroutine()

	// A small delta is normal (http server internals, test client conns); a
	// per-socket leak would show up as ~12+ residual goroutines.
	if after-before > 8 {
		t.Fatalf("goroutines grew from %d to %d across 12 socket cycles - a pump is leaking", before, after)
	}
	if s.hub.Count() != 0 {
		t.Fatalf("live sessions = %d after every socket closed", s.hub.Count())
	}
	if s.hub.CountFor(consoleTestAID) != 0 {
		t.Fatalf("per-operator counter = %d, want 0 - the limiter slot leaked", s.hub.CountFor(consoleTestAID))
	}
}

// readOpenVerdict reads until the socket answers an `open` either way,
// skipping unrelated traffic (a trailing chunk, the exit of an earlier pane).
func readOpenVerdict(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()
	for i := 0; i < 50; i++ {
		f := readFrame(t, ws)
		if f["type"] == "opened" || f["type"] == "error" {
			return f
		}
	}
	t.Fatal("no verdict on the open within 50 frames")
	return nil
}

// settle gives closed connections a moment to unwind their goroutines.
func settle(t *testing.T) {
	t.Helper()
	for i := 0; i < 10; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
}

// A ConsoleExit racing an operator close must not double-release a limiter
// slot, or the per-operator counter would underflow and the cap stop working.
func TestConsoleWS_ExitRacingCloseReleasesSlotOnce(t *testing.T) {
	s := newConsoleTestServer(t, allowAllRBAC{}, console.Limits{MaxSessionsPerAID: 4})
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	id := s.soul.sessionIDs()[0]

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.soul.sendExit(id, 0, keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED)
	}()
	go func() {
		defer wg.Done()
		writeFrame(t, ws, map[string]any{"type": "close", "session_id": "pane-1"})
	}()
	wg.Wait()

	waitFor(t, "the session to settle", func() bool { return s.hub.Count() == 0 })
	if got := s.hub.CountFor(consoleTestAID); got != 0 {
		t.Fatalf("per-operator counter = %d after a close/exit race, want 0", got)
	}

	// The cap must still admit exactly its limit afterwards. The terminal frame
	// of the raced session may still be queued ahead of these, so skip frames
	// that are not a verdict on the open — but treat an `error` as one.
	for i, pane := range []string{"n1", "n2", "n3", "n4"} {
		writeFrame(t, ws, map[string]any{"type": "open", "session_id": pane, "sid": "host-a"})
		f := readOpenVerdict(t, ws)
		if f["type"] != "opened" {
			t.Fatalf("open %d returned %v, want opened - the counter was corrupted by the race", i, f)
		}
	}
}

// --- diagnosing a socket that died on the write side (NIM-253) ---

// warnCapture collects WARN+ records, so a test can assert on what an operator
// would actually find in the log at a production level.
type warnCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *warnCapture) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= slog.LevelWarn }

// Handle keeps the record, which is exactly the case slog says to Clone for: a
// Record shares the backing array of its attributes with the caller, and the
// caller is free to reuse it once Handle returns. Without the clone the attrs a
// test reads back are whatever the logger last wrote there — and `aid` and
// `reason` are read back below.
func (h *warnCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *warnCapture) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *warnCapture) WithGroup(_ string) slog.Handler      { return h }

// find returns the first captured record whose message contains sub.
func (h *warnCapture) find(sub string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if strings.Contains(r.Message, sub) {
			return r, true
		}
	}
	return slog.Record{}, false
}

// count returns how many records were captured. Under the lock, like find and
// dump: the pumps write into this handler from their own goroutines, so reading
// the slice header directly is a race — and one that only shows up in the case
// the caller cares about, a WARN arriving late enough to be worth catching.
func (h *warnCapture) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

// dump renders everything captured, for a failure message that says what WAS
// logged rather than only what was not.
func (h *warnCapture) dump() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) == 0 {
		return "(nothing at WARN or above)"
	}
	var b strings.Builder
	for _, r := range h.records {
		b.WriteString("  " + r.Level.String() + " " + r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" " + a.Key + "=" + a.Value.String())
			return true
		})
		b.WriteString("\n")
	}
	return b.String()
}

// attr returns the value of one attribute of a record.
func recordAttr(r slog.Record, key string) (string, bool) {
	var out string
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out, found = a.Value.String(), true
			return false
		}
		return true
	})
	return out, found
}

// A socket that dies because a write failed must say so at a level an operator
// runs with.
//
// This is the event NIM-242 stopped hiding and left unexplained: the write
// failed, so gorilla's connection is unusable, so kill-on-disconnect reaps
// EVERY pty on that socket at once. The operator sees their whole wall die.
// Nothing can tell them why over the socket — it is gone, and no close frame
// can be written down a connection that just failed a write — so the log is the
// only place the answer can exist, and it was Debug.
func TestConsoleWS_WriteFailureIsReportedAtWarn(t *testing.T) {
	logs := &warnCapture{}
	// Compressed budget: the invariant is what gets REPORTED when the write
	// gives up, and the production value is too generous to sit and wait for.
	s := newConsoleTestServerLogging(t, allowAllRBAC{}, console.Limits{},
		300*time.Millisecond, slog.New(logs))
	// A teardown tail that takes a visible amount of time, on purpose. Waiting
	// for hub.Count() to reach zero and then reading the reap log is asserting
	// that the socket finished tearing down within the same instant its first
	// step completed — a claim about the scheduler, not about the socket. It
	// held on an idle machine and failed on a loaded one, which cost NIM-523
	// and NIM-537 and one red release gate; the flake reproduced nowhere it
	// could be looked at, because reproducing it meant reproducing the load.
	// With this it is not a flake at all: the wait below is either there or the
	// test is red every single time.
	s.soul.setCloseDelay(250 * time.Millisecond)
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	id := s.soul.sessionIDs()[0]

	// Past the kernel socket buffers so the writer parks on the SOCKET, but well
	// inside the queue so nothing is dropped. The client reads nothing from here.
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for i := 0; i < consoleOutQueueDepth/2; i++ {
		s.soul.sendChunk(id, chunk, uint64(i+1), 0)
	}

	waitFor(t, "the sessions to be reaped once the writer gave up",
		func() bool { return s.hub.Count() == 0 })

	// The whole message, not "write failed": the keepalive path logs "operator
	// socket KEEPALIVE write failed", which contains that substring and is a
	// different event with a different close reason. A test that accepts either
	// one passes on the wrong failure the day the ping beats the queue.
	rec := waitForRecord(t, logs, "console: operator socket write failed",
		"a socket that died on a failed write never said why")
	if aid, ok := recordAttr(rec, "aid"); !ok || aid != consoleTestAID {
		t.Errorf("write failure logged without the operator: aid=%q ok=%v", aid, ok)
	}

	// And the reap itself must name the cause, because that line is the one
	// carrying how many ptys went with it. Waited for, not read: the count above
	// went to zero at unregister, which is the first step of Hub.Close and
	// several before this line — the close dispatch (stalled on purpose above),
	// the recording close and the audit write all sit in between.
	reap := waitForRecord(t, logs, "sessions reaped",
		"a socket that lost every session to a failed write reaped them quietly")
	if reason, _ := recordAttr(reap, "reason"); reason != string(console.CloseSocketWriteFailed) {
		t.Errorf("reap reason = %q, want %q — an operator cannot tell a stalled socket from a closed tab",
			reason, console.CloseSocketWriteFailed)
	}

	drainAndClose(t, ws)
}

// An ordinary disconnect must stay quiet.
//
// This is what makes the WARN above safe, and it is not obvious: teardown
// deliberately closes the socket out from under a writer parked mid-frame
// (NIM-242 — otherwise every pty waits out the whole write budget), and that
// write then fails. Reporting it would put a WARN on the most routine event
// there is — closing a tab while output is streaming — and the noise would bury
// the real one.
func TestConsoleWS_OrdinaryDisconnectReportsNoFailure(t *testing.T) {
	logs := &warnCapture{}
	s := newConsoleTestServerLogging(t, allowAllRBAC{}, console.Limits{}, 0, slog.New(logs))
	// Armed here for the same reason as in the test above, and it is what makes
	// the wait below load-bearing rather than decorative. settle() alone spans
	// 200ms; without a teardown tail longer than that, settling on the session
	// count would happen to cover the rest of teardown anyway on an idle machine
	// and the vantage point would be untestable — green either way here, red in
	// CI under load, which is precisely NIM-537. With the stall, a WARN written
	// anywhere after the close dispatch is caught or missed depending on where
	// the test looks from, and that is a difference a mutation can show.
	s.soul.setCloseDelay(250 * time.Millisecond)
	ws := s.dial(t)

	writeFrame(t, ws, map[string]any{"type": "open", "session_id": "pane-1", "sid": "host-a"})
	readFrameOfType(t, ws, "opened")
	id := s.soul.sessionIDs()[0]

	// Park the writer on the socket: past the kernel buffers, inside the queue,
	// and the client reads none of it.
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for i := 0; i < consoleOutQueueDepth/2; i++ {
		s.soul.sendChunk(id, chunk, uint64(i+1), 0)
	}

	// The operator closes the tab. That is a WRITE, so it lands even though this
	// client never reads: the read pump ends while the writer is still parked,
	// and teardown takes the socket away from it.
	if err := ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("send close: %v", err)
	}

	waitFor(t, "the sessions to be reaped", func() bool { return s.hub.Count() == 0 })
	// The count drops at Hub.Close's FIRST step, and the socket is far from
	// done there: the close dispatch, the recording close, the audit write, the
	// reap line and the gauge all come after it, and the audit and recording
	// paths warn when they fail. `count() == 0` below is an absence claim over
	// all of them, so it has to be made from a vantage point later than all of
	// them — settling on the session count settles in the middle of teardown,
	// and reads a WARN written a moment afterwards as silence (NIM-523).
	//
	// The named line is not what forces this: `write failed` comes from the
	// writer pump, which consoleConn.run joins before it reaps anything, so
	// even the session count already outlives it. It is the blanket assertion
	// that needs the room, and only the gauge gives it — see socketsActive.
	//
	// Known-bad for this wait, run on a copy: make socketDiedBadly treat an
	// ordinary close as a failure, so the reap line goes out at WARN. From here
	// the test is red; from the session count plus settle() it is green 5 times
	// out of 5, reporting silence over a warning that had already been written.
	waitFor(t, "the socket handler to finish", func() bool { return s.socketsActive(t) == 0 })
	settle(t)

	// The bare substring on purpose here, unlike the positive test: it also
	// catches the keepalive variant, and neither belongs on a closed tab.
	if _, ok := logs.find("write failed"); ok {
		t.Errorf("closing a tab mid-stream reported a write failure — every ordinary disconnect would; logged:\n%s", logs.dump())
	}
	if n := logs.count(); n != 0 {
		t.Errorf("an ordinary disconnect logged %d WARN+ records:\n%s", n, logs.dump())
	}
	_ = ws.Close()
}

// drainAndClose reads until the socket ends, then closes the client end, so the
// next test in the package does not start against a server still pushing a
// backlog into a socket nobody reads (NIM-221).
func drainAndClose(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	var err error
	for err == nil {
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err = ws.ReadMessage()
	}
	_ = ws.Close()
}
