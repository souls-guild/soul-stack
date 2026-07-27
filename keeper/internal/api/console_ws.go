package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/console"
	keepergrpc "github.com/souls-guild/soul-stack/keeper/internal/grpc"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// `GET /v1/console` — the operator's WebSocket into interactive PTY sessions
// (NIM-143, docs/keeper/console.md).
//
// This is the only WebSocket in Keeper; every other surface is HTTP/1.1 + SSE.
// A console needs a real bidirectional channel — SSE cannot carry keystrokes
// back, and a POST per keypress would be absurd on a terminal.
//
// It is NOT a huma operation, and deliberately absent from the OpenAPI spec:
// huma models request/response bodies, while an upgrade replaces the response
// with a raw socket. The wire contract lives in docs/keeper/console.md and is
// mirrored by the client in `soul-stack-web/src/api/consoleProtocol.ts`.
//
// Auth is the standard /v1 RequireJWT chain, with the token arriving as the
// `bearer.<jwt>` subprotocol (see middleware.RequireJWT): the browser WebSocket
// API cannot set an Authorization header. The `soul.console` permission gates
// the upgrade itself; the per-host `host=<sid>` scope is checked per session,
// in-handler, because the SID arrives in the `open` frame rather than the URL.

// consoleWSDeps wires the endpoint.
type consoleWSDeps struct {
	Hub      *console.Hub
	Enforcer middleware.PermissionChecker
	Metrics  *console.Metrics
	Logger   *slog.Logger
}

// consoleUpgrader turns the request into a socket.
//
// CheckOrigin returns true because the endpoint is not cookie-authenticated:
// every request carries an explicit bearer token that a cross-origin page
// cannot obtain, so there is no CSRF surface for an Origin check to close. The
// buffers are sized for the traffic shape — small inbound frames (keystrokes,
// resizes) against 32 KiB pty chunks outbound.
var consoleUpgrader = websocket.Upgrader{
	ReadBufferSize:  4 << 10,
	WriteBufferSize: 32 << 10,
	Subprotocols:    []string{console.Subprotocol},
	CheckOrigin:     func(*http.Request) bool { return true },
}

// registerConsoleWS mounts the endpoint on a chi router.
func registerConsoleWS(r interface {
	Get(pattern string, h http.HandlerFunc)
}, deps *consoleWSDeps) {
	r.Get("/console", consoleWSHandler(deps))
}

func consoleWSHandler(deps *consoleWSDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := middleware.ClaimsFromContext(r.Context())
		if !ok {
			// The JWT middleware did not run — a chain misconfiguration, not a
			// client error.
			problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "console: auth chain not wired"))
			return
		}

		// The client must offer our subprotocol: gorilla only echoes a
		// subprotocol it was offered, and a browser closes a socket whose
		// negotiated subprotocol is missing. Refusing before the upgrade turns
		// a silent client-side close into a legible 400.
		if !websocket.IsWebSocketUpgrade(r) || !offersSubprotocol(r) {
			problem.Write(w, problem.New(problem.TypeValidationFailed, r.URL.Path,
				"console: expected a WebSocket upgrade offering the "+console.Subprotocol+" subprotocol"))
			return
		}

		ws, err := consoleUpgrader.Upgrade(w, r, http.Header{})
		if err != nil {
			// Upgrade already wrote its own HTTP error.
			deps.Logger.Debug("console: upgrade failed", slog.Any("error", err))
			return
		}

		c := newConsoleConn(ws, claims.Subject, deps)
		c.run(r.Context())
	}
}

// offersSubprotocol reports whether the client advertised our subprotocol.
func offersSubprotocol(r *http.Request) bool {
	for _, p := range websocket.Subprotocols(r) {
		if p == console.Subprotocol {
			return true
		}
	}
	return false
}

// outFrame is one queued message for the socket writer.
type outFrame struct {
	payload []byte
	// control marks a lifecycle frame (opened/exit/error). Control frames are
	// never dropped — losing an `opened` strands a pane on "connecting" and
	// losing an `exit` leaves it live forever — so a full queue closes the
	// socket instead, and kill-on-disconnect reaps the sessions behind it.
	control bool
}

// consoleConn is one operator socket multiplexing N console sessions.
//
// Two goroutines: the HTTP handler's own (the read pump) and a writer. A
// WebSocket permits exactly one concurrent writer, so every frame — including
// the ones the EventStream produces from other goroutines — funnels through
// `out`.
type consoleConn struct {
	ws      *websocket.Conn
	aid     string
	hub     *console.Hub
	deps    *consoleWSDeps
	logger  *slog.Logger
	metrics *console.Metrics

	out  chan outFrame
	done chan struct{}
	// closeOnce guards `done`, which both pumps may close.
	closeOnce sync.Once

	mu sync.Mutex
	// sessions maps the client's socket-local id to the Hub session.
	sessions map[string]*console.Session
	// dropped accumulates Keeper-side discarded bytes per client session,
	// reported on the next chunk that does get through.
	dropped map[string]*atomic.Uint64
}

func newConsoleConn(ws *websocket.Conn, aid string, deps *consoleWSDeps) *consoleConn {
	return &consoleConn{
		ws:       ws,
		aid:      aid,
		hub:      deps.Hub,
		deps:     deps,
		logger:   deps.Logger,
		metrics:  deps.Metrics,
		out:      make(chan outFrame, consoleOutQueueDepth),
		done:     make(chan struct{}),
		sessions: make(map[string]*console.Session),
		dropped:  make(map[string]*atomic.Uint64),
	}
}

// Frame-plane constants mirroring keeper/internal/console/limits.go. Duplicated
// rather than exported: they describe THIS socket implementation (queue depth,
// deadlines), not the operator-facing policy the console package owns.
const (
	consoleOutQueueDepth  = 256
	consoleWriteWait      = 10 * time.Second
	consolePongWait       = 60 * time.Second
	consolePingPeriod     = (consolePongWait * 9) / 10
	consoleMaxClientFrame = 1 << 20
	// consoleClaimRefresh re-stamps cluster claims well inside their TTL, so a
	// long-lived console stays routable — and so a LAPSED claim reliably means
	// this Keeper died rather than merely being slow.
	consoleClaimRefresh = 30 * time.Second
	// consoleDropFlush is how often a session's accumulated drop count is
	// pushed out on its own. Without it a gap would only surface on the NEXT
	// chunk that fits in the queue — and a flood that ends (a `find /` that
	// finishes) produces no next chunk, so the operator would be left looking
	// at spliced output with no sign anything was lost.
	consoleDropFlush = 250 * time.Millisecond
)

// run drives the socket until either side closes it, then reaps every session.
func (c *consoleConn) run(ctx context.Context) {
	c.metrics.IncSocketsActive()
	// Detach from the request context: after an upgrade the request is over,
	// and a context tied to it would cancel while the socket is alive. Server
	// shutdown closes the listener and the socket with it.
	ctx = context.WithoutCancel(ctx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.writePump()
	}()

	c.readPump(ctx)

	// Read loop is over: stop the writer, then tear down every pty. Order
	// matters — the sessions must die even if the writer is wedged on a peer
	// that stopped reading.
	c.shutdown()
	wg.Wait()
	_ = c.ws.Close()

	sessions := c.takeAllSessions()
	if n := c.hub.CloseAllFor(ctx, sessions, "operator socket closed"); n > 0 {
		c.logger.Info("console: socket closed, sessions reaped",
			slog.String("aid", c.aid), slog.Int("sessions", n))
	}
	c.metrics.DecSocketsActive()
}

// shutdown signals both pumps to stop. Idempotent.
func (c *consoleConn) shutdown() {
	c.closeOnce.Do(func() { close(c.done) })
}

// readPump consumes client frames until the socket dies.
func (c *consoleConn) readPump(ctx context.Context) {
	c.ws.SetReadLimit(consoleMaxClientFrame)
	_ = c.ws.SetReadDeadline(time.Now().Add(consolePongWait))
	c.ws.SetPongHandler(func(string) error {
		// A pong proves the peer is alive; without this the deadline would fire
		// on an idle-but-healthy terminal.
		return c.ws.SetReadDeadline(time.Now().Add(consolePongWait))
	})

	for {
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.logger.Debug("console: socket read ended",
					slog.String("aid", c.aid), slog.Any("error", err))
			}
			return
		}

		frame, err := console.DecodeClientFrame(raw)
		if err != nil {
			// A malformed frame is the client's bug, not a reason to drop a
			// socket that may hold healthy sessions.
			c.sendError("", console.ErrCodeBadFrame, err.Error())
			continue
		}
		c.handleFrame(ctx, frame)
	}
}

func (c *consoleConn) handleFrame(ctx context.Context, f *console.ClientFrame) {
	switch f.Type {
	case console.TypeOpen:
		c.handleOpen(ctx, f)
	case console.TypeStdin:
		c.handleStdin(ctx, f)
	case console.TypeResize:
		c.handleResize(ctx, f)
	case console.TypeClose:
		c.handleClose(ctx, f)
	}
}

func (c *consoleConn) handleOpen(ctx context.Context, f *console.ClientFrame) {
	if c.lookupSession(f.SessionID) != nil {
		c.sendError(f.SessionID, console.ErrCodeDuplicateSession, "session_id is already open on this socket")
		return
	}

	// Per-host scope. The socket-level `soul.console` gate already ran as chi
	// middleware; this is the `host=<sid>` half, which only becomes checkable
	// once the target arrives in the frame.
	if c.deps.Enforcer != nil {
		if err := c.deps.Enforcer.Check(c.aid, "soul", "console", map[string]string{"host": f.SID}); err != nil {
			c.sendError(f.SessionID, console.ErrCodeForbidden, "no console permission for this host")
			return
		}
	}

	sess, err := c.hub.Open(ctx, console.OpenRequest{
		ClientID: f.SessionID,
		SID:      f.SID,
		AID:      c.aid,
		Cols:     f.Cols,
		Rows:     f.Rows,
		Shell:    f.Shell,
		Sink:     c,
	})
	if err != nil {
		c.sendError(f.SessionID, openErrorCode(err), err.Error())
		return
	}
	c.addSession(f.SessionID, sess)
}

// dispatchErrorCode distinguishes a congested queue from an absent Soul.
//
// Both congestion cases are the same story for the operator: the host is
// healthy and the right move is to type again. Reporting either as
// `soul_offline` would send them hunting for a dead agent instead. The queue is
// the session's own console stream once it is up (NIM-188), and the pre-`opened`
// park before that ([console.ErrSessionNotReady]); a Soul still on the
// EventStream transport fills the per-SID outbound queue it shares with apply.
func dispatchErrorCode(err error) string {
	if errors.Is(err, keepergrpc.ErrOutboundQueueFull) || errors.Is(err, console.ErrSessionNotReady) {
		return console.ErrCodeBusy
	}
	return console.ErrCodeSoulOffline
}

// openErrorCode maps a Hub failure to the closed set of wire error codes.
func openErrorCode(err error) string {
	switch {
	case errors.Is(err, console.ErrLimitExceeded):
		return console.ErrCodeLimitExceeded
	case errors.Is(err, console.ErrConsoleUnsupported):
		return console.ErrCodeUnsupported
	case errors.Is(err, keepergrpc.ErrOutboundQueueFull):
		return console.ErrCodeBusy
	case errors.Is(err, console.ErrSoulOffline):
		return console.ErrCodeSoulOffline
	case errors.Is(err, console.ErrDuplicateSession):
		return console.ErrCodeDuplicateSession
	default:
		return console.ErrCodeInternal
	}
}

func (c *consoleConn) handleStdin(ctx context.Context, f *console.ClientFrame) {
	sess := c.lookupSession(f.SessionID)
	if sess == nil {
		// The session exited while the keystroke was in flight — benign.
		return
	}
	data, err := f.DecodeData()
	if err != nil {
		c.sendError(f.SessionID, console.ErrCodeBadFrame, err.Error())
		return
	}
	if err := c.hub.Stdin(ctx, sess, data); err != nil {
		c.sendError(f.SessionID, dispatchErrorCode(err), err.Error())
	}
}

func (c *consoleConn) handleResize(ctx context.Context, f *console.ClientFrame) {
	sess := c.lookupSession(f.SessionID)
	if sess == nil {
		return
	}
	if err := c.hub.Resize(ctx, sess, f.Cols, f.Rows); err != nil {
		c.logger.Debug("console: resize dispatch failed",
			slog.String("session_id", sess.KeeperID), slog.Any("error", err))
	}
}

func (c *consoleConn) handleClose(ctx context.Context, f *console.ClientFrame) {
	sess := c.removeSession(f.SessionID)
	if sess == nil {
		return
	}
	c.hub.Close(ctx, sess, "operator detached the pane")
}

// writePump owns the socket's write side: it drains `out`, and pings to keep
// the peer's liveness observable.
func (c *consoleConn) writePump() {
	ticker := time.NewTicker(consolePingPeriod)
	claims := time.NewTicker(consoleClaimRefresh)
	drops := time.NewTicker(consoleDropFlush)
	defer ticker.Stop()
	defer claims.Stop()
	defer drops.Stop()

	for {
		select {
		case <-c.done:
			// Best-effort goodbye; a dead peer just makes this fail.
			_ = c.ws.SetWriteDeadline(time.Now().Add(consoleWriteWait))
			_ = c.ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return

		case f := <-c.out:
			_ = c.ws.SetWriteDeadline(time.Now().Add(consoleWriteWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, f.payload); err != nil {
				c.logger.Debug("console: socket write failed",
					slog.String("aid", c.aid), slog.Any("error", err))
				c.shutdown()
				return
			}

		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(consoleWriteWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				// The peer is gone (a slept laptop keeps a half-open TCP
				// connection for minutes); every pty behind it must die now,
				// not when TCP eventually notices.
				c.shutdown()
				return
			}

		case <-claims.C:
			c.refreshClaims()

		case <-drops.C:
			c.flushDropped()
		}
	}
}

// flushDropped emits a marker chunk for every session carrying discarded bytes,
// so a gap is reported even when the flood that caused it has ended.
//
// The marker is an ordinary `chunk` with empty data: the client already knows
// how to render `dropped_bytes`, and an empty payload appends nothing to the
// terminal. Skipped while the queue is still congested — the point is to report
// the gap once there is room, not to compete with live output.
func (c *consoleConn) flushDropped() {
	for _, pending := range c.takePendingDrops() {
		payload, err := json.Marshal(console.ChunkFrame{
			Type:         console.TypeChunk,
			SessionID:    pending.clientID,
			Stream:       "stdout",
			DroppedBytes: pending.bytes,
		})
		if err != nil {
			continue
		}
		select {
		case c.out <- outFrame{payload: payload}:
		default:
			// Still congested: hand the count back and try on the next tick.
			c.creditDropped(pending.clientID, pending.bytes)
			return
		}
	}
}

// pendingDrop is one session's accumulated Keeper-side loss.
type pendingDrop struct {
	clientID string
	bytes    uint64
}

// takePendingDrops atomically drains the drop counters of every live session.
func (c *consoleConn) takePendingDrops() []pendingDrop {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []pendingDrop
	for id, ctr := range c.dropped {
		if n := ctr.Swap(0); n > 0 {
			out = append(out, pendingDrop{clientID: id, bytes: n})
		}
	}
	return out
}

// creditDropped returns an undelivered count to a session's counter.
func (c *consoleConn) creditDropped(clientID string, n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctr, ok := c.dropped[clientID]; ok {
		ctr.Add(n)
	}
}

// refreshClaims re-stamps the cluster routing claims of this socket's sessions.
func (c *consoleConn) refreshClaims() {
	claims := c.sessionClaims()
	if len(claims) == 0 {
		return
	}
	c.hub.RefreshClaims(context.Background(), claims)
}

// --- console.Sink ---

// DeliverChunk queues pty output, dropping it when the socket is behind.
//
// Called from the EventStream goroutine, so it must never block: a console that
// could stall here would stall the apply traffic sharing that stream. Dropping
// is the same bargain the Soul side makes — loss is possible under a flood, but
// it is always counted and reported, so the terminal renders an explicit gap
// instead of silently splicing a corrupted ANSI stream.
func (c *consoleConn) DeliverChunk(sessionID string, stream keeperv1.ConsoleStream, data []byte, soulDropped uint64) {
	counter := c.droppedCounter(sessionID)
	pending := counter.Swap(0) + soulDropped

	payload, err := json.Marshal(console.NewChunk(sessionID, stream, data, pending))
	if err != nil {
		c.logger.Warn("console: chunk marshal failed", slog.Any("error", err))
		return
	}

	select {
	case c.out <- outFrame{payload: payload}:
	default:
		// Put the accounting back: this chunk's bytes plus whatever we were
		// already carrying belong to the next one that gets through.
		counter.Add(pending + uint64(len(data)))
		c.metrics.AddDroppedBytes(len(data))
	}
}

func (c *consoleConn) DeliverOpened(f console.OpenedFrame) { c.sendControl(f) }
func (c *consoleConn) DeliverExit(f console.ExitFrame)     { c.sendControl(f) }
func (c *consoleConn) DeliverError(f console.ErrorFrame)   { c.sendControl(f) }

// sendControl queues a lifecycle frame. A full queue means the peer stopped
// reading entirely, and a control frame cannot be dropped without stranding the
// UI — so the socket is closed and every session behind it reaped.
func (c *consoleConn) sendControl(frame any) {
	payload, err := json.Marshal(frame)
	if err != nil {
		c.logger.Warn("console: control frame marshal failed", slog.Any("error", err))
		return
	}
	select {
	case c.out <- outFrame{payload: payload, control: true}:
	case <-c.done:
	default:
		c.logger.Warn("console: control queue full — closing socket",
			slog.String("aid", c.aid))
		c.shutdown()
	}
}

func (c *consoleConn) sendError(sessionID, code, message string) {
	c.sendControl(console.NewError(sessionID, code, message))
}

// --- session bookkeeping ---

func (c *consoleConn) addSession(clientID string, sess *console.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[clientID] = sess
	c.dropped[clientID] = &atomic.Uint64{}
}

func (c *consoleConn) lookupSession(clientID string) *console.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[clientID]
}

func (c *consoleConn) removeSession(clientID string) *console.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	sess := c.sessions[clientID]
	delete(c.sessions, clientID)
	delete(c.dropped, clientID)
	return sess
}

// takeAllSessions drains the registry, so the teardown cannot race a concurrent
// `close` frame into a double release.
func (c *consoleConn) takeAllSessions() []*console.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*console.Session, 0, len(c.sessions))
	for id, sess := range c.sessions {
		out = append(out, sess)
		delete(c.sessions, id)
		delete(c.dropped, id)
	}
	return out
}

func (c *consoleConn) sessionClaims() []console.SessionClaim {
	c.mu.Lock()
	defer c.mu.Unlock()
	claims := make([]console.SessionClaim, 0, len(c.sessions))
	for _, sess := range c.sessions {
		claims = append(claims, console.SessionClaim{SessionID: sess.KeeperID, SID: sess.SID})
	}
	return claims
}

// droppedCounter returns the per-session drop accumulator, tolerating a chunk
// that arrives for a session already removed (the counter is then transient).
func (c *consoleConn) droppedCounter(clientID string) *atomic.Uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctr, ok := c.dropped[clientID]; ok {
		return ctr
	}
	return &atomic.Uint64{}
}
