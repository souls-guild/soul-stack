package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// The Keeper-side session manager of Multi-console.
//
// It sits between two transports that do not know about each other: the
// operator's WebSocket (`/v1/console`, one socket multiplexing N sessions) and
// the Keeper<->Soul EventStream (bidi gRPC, one stream per host, shared with
// the apply cycle). The Hub owns the mapping between them and every invariant
// that spans both:
//
//   - id translation — the client's socket-local `session_id` against the ULID
//     Keeper mints for the Soul, which must be unique per EventStream;
//   - the operator envelope — per-Archon and per-instance session caps, plus
//     the idle timeout for a terminal nobody is watching;
//   - kill-on-disconnect — a socket that drops takes every pty behind it down,
//     because an orphaned interactive root shell is the worst failure this
//     feature can produce;
//   - cross-Keeper routing — the operator's socket and the host's EventStream
//     routinely land on different instances of a stateless cluster (ADR-002).

// Dispatcher sends the Keeper->Soul half of the contract. Implemented by
// grpc.Outbound, which already routes cluster-wide (local stream, else the
// lease holder over Redis pub/sub) — so the downstream direction needs nothing
// of its own here.
type Dispatcher interface {
	SendConsoleOpen(ctx context.Context, sid string, msg *keeperv1.ConsoleOpen) error
	SendConsoleStdin(ctx context.Context, sid string, msg *keeperv1.ConsoleStdin) error
	SendConsoleResize(ctx context.Context, sid string, msg *keeperv1.ConsoleResize) error
	SendConsoleClose(ctx context.Context, sid string, msg *keeperv1.ConsoleClose) error
}

// CapabilityChecker reports whether a Soul announced the `console` capability
// in its Hello. Checked BEFORE minting a session, fail-closed: an old binary
// drops ConsoleOpen into the default branch of its recv-loop and never answers,
// which would leave the operator watching a dead terminal until a timeout.
type CapabilityChecker interface {
	HasCapability(ctx context.Context, sid, capability string) (bool, error)
}

// Sink is the socket half of a session — where the Hub hands frames to the
// operator. Implemented by the WebSocket connection.
type Sink interface {
	// DeliverChunk queues pty output. Best-effort by contract: under
	// backpressure the chunk is DROPPED and its size counted, never buffered
	// without bound. soulDropped is what the Soul-side flow control already
	// discarded before this chunk.
	DeliverChunk(sessionID string, stream keeperv1.ConsoleStream, data []byte, soulDropped uint64)

	// DeliverOpened / DeliverExit / DeliverError queue lifecycle frames. These
	// are never dropped: losing an `opened` strands the pane on "connecting",
	// and losing an `exit` leaves it live forever.
	DeliverOpened(f OpenedFrame)
	DeliverExit(f ExitFrame)
	DeliverError(f ErrorFrame)
}

// Session is one live console: a client-side id on a socket, a Keeper-minted
// id on the EventStream, and the host in between.
type Session struct {
	// KeeperID is the ULID Keeper minted for the Soul side.
	KeeperID string
	// ClientID is the id the browser generated; unique only within its socket.
	ClientID string
	// SID is the target host.
	SID string
	// AID is the Archon who opened it — the unit the per-operator cap counts.
	AID string

	sink Sink

	// recording is the session's mandatory recording (ADR-0074(g), NIM-145).
	// Never nil for a session the Hub handed out: it is opened before the
	// session is dispatched and the open fails if it cannot be.
	recording Recording

	// lastInput is the Unix-nano stamp of the last operator keystroke, for the
	// idle sweep. Output does not touch it: a `tail -f` left running is exactly
	// the abandoned terminal the sweep exists to reap.
	lastInput atomic.Int64

	// closed guards the terminal transition, so a ConsoleExit racing an
	// operator-initiated close cannot double-release a limiter slot.
	closed atomic.Bool

	// readyMu guards the pre-`opened` window (NIM-188). Until the Soul answers
	// ConsoleOpened, nobody here knows which transport the session ended up on
	// — the dedicated console stream, or EventStream for a Soul (or a Keeper)
	// that does not have one. Input sent during that window would race the
	// switch: a keystroke on EventStream and the next one on the console
	// stream are two independent HTTP/2 streams, and the Soul reads them from
	// two goroutines, so they can arrive out of order. Keystrokes must not be
	// reordered, so they wait instead.
	//
	// The window is one round trip and the operator has not seen a prompt yet;
	// what lands in it is an early resize from the terminal widget and, at
	// most, an impatient paste.
	readyMu sync.Mutex
	ready   bool
	pending []func(context.Context) error
}

// maxPendingFrames caps the pre-`opened` queue. Overflow is reported as
// [ErrSessionNotReady] rather than dropped: silently losing keystrokes is the
// one thing a terminal may never do.
const maxPendingFrames = 64

// touch stamps operator activity.
func (s *Session) touch() { s.lastInput.Store(time.Now().UnixNano()) }

// idleFor reports how long the session has been without operator input.
func (s *Session) idleFor(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, s.lastInput.Load()))
}

// Sentinel errors returned by [Hub.Open]. The WebSocket layer maps them to the
// `error` frame codes in protocol.go.
var (
	// ErrLimitExceeded — the operator or this instance is at its session cap.
	ErrLimitExceeded = errors.New("console: session limit exceeded")
	// ErrDuplicateSession — the client reused a live session_id on one socket.
	ErrDuplicateSession = errors.New("console: duplicate session id on socket")
	// ErrConsoleUnsupported — the Soul binary does not announce `console`.
	ErrConsoleUnsupported = errors.New("console: soul does not support consoles")
	// ErrSoulOffline — no EventStream for the SID anywhere in the cluster.
	ErrSoulOffline = errors.New("console: soul is not connected")
	// ErrSessionNotReady — too much input arrived before the Soul answered
	// ConsoleOpened (NIM-188). Transient by nature: the operator retries.
	ErrSessionNotReady = errors.New("console: session is not ready for input yet")
)

// RecordingID returns the id of the session's recording, for the audit trail
// and for NIM-148 to fetch the body by.
func (s *Session) RecordingID() string {
	if s.recording == nil {
		return ""
	}
	return s.recording.ID()
}

// closeRecording finishes the session's recording.
//
// Nil-safe for one window: the recording's own writer goroutine is running
// before [Hub.Open] has attached it to the session, so a failure raced into the
// teardown path could otherwise arrive at a session that has no recording yet.
// Nothing is queued at that point, so it is insurance rather than a live case —
// but the failure mode it insures against is a panic in a console goroutine.
func (s *Session) closeRecording(ctx context.Context, reason string) {
	if s.recording != nil {
		s.recording.Close(ctx, reason)
	}
}

// HubDeps wires the session manager.
type HubDeps struct {
	// Dispatcher is required — without it there is nothing to talk to.
	Dispatcher Dispatcher

	// Capabilities gates ConsoleOpen on the Soul's announced `console` support.
	// nil → the check is skipped (unit tests); production wire-up passes the
	// soul registry.
	Capabilities CapabilityChecker

	// Cluster routes upstream frames to the Keeper instance that holds the
	// operator's socket. nil → single-instance mode: a session whose Soul
	// stream lands on another Keeper simply receives nothing, so the wire-up
	// passes it whenever Redis is configured.
	Cluster *ClusterBridge

	// AuditWriter records session open/close as facts. An interactive root
	// shell is the most privileged thing an operator can do, so it is audited
	// independently of the recording — the one fact that survives every
	// degradation of the recording path (ADR-0074(f)).
	// nil → audit disabled (unit/dev).
	AuditWriter audit.Writer

	// Recorder is REQUIRED — [NewHub] refuses to build without one. That is
	// where "recording cannot be turned off" is enforced (ADR-0074(g)): there
	// is no nil-means-disabled branch to reach, no config key that produces
	// one, and no second code path for an unrecorded session.
	Recorder Recorder

	Limits  Limits
	Metrics *Metrics
	Logger  *slog.Logger
}

// Hub is the registry of live console sessions on this Keeper instance.
type Hub struct {
	deps   HubDeps
	limits Limits

	mu sync.RWMutex
	// byKeeperID routes an upstream frame (which carries only the Keeper ULID)
	// back to its socket.
	byKeeperID map[string]*Session
	// perAID counts live sessions per Archon for the operator cap.
	perAID map[string]int

	// newID mints the Soul-side session id. A field so tests can pin it.
	newID func() string
}

// NewHub assembles the session manager. Dispatcher is required.
func NewHub(deps HubDeps) (*Hub, error) {
	if deps.Dispatcher == nil {
		return nil, errors.New("console: Hub dispatcher is required")
	}
	if deps.Logger == nil {
		return nil, errors.New("console: Hub logger is required")
	}
	// ADR-0074(g): a Hub that could run without a recorder would be a Hub that
	// can open an unrecorded console, and no amount of configuration discipline
	// downstream would take that branch away.
	if deps.Recorder == nil {
		return nil, errors.New("console: Hub recorder is required — console sessions are recorded unconditionally (ADR-0074(g))")
	}
	return &Hub{
		deps:       deps,
		limits:     deps.Limits.resolve(),
		byKeeperID: make(map[string]*Session),
		perAID:     make(map[string]int),
		newID:      audit.NewULID,
	}, nil
}

// OpenRequest is one operator request to start a console.
type OpenRequest struct {
	ClientID string
	SID      string
	AID      string
	Cols     uint32
	Rows     uint32
	Shell    string
	Sink     Sink
}

// Open mints a session, registers it and dispatches ConsoleOpen to the Soul.
//
// Order matters and is not cosmetic: the session is registered BEFORE the
// dispatch, because the Soul may answer ConsoleOpened before SendConsoleOpen
// has even returned on this side — registering afterwards would drop the first
// frames of every fast host. A failed dispatch unregisters again.
//
// The caller owns socket-level bookkeeping (client-id uniqueness within its own
// socket); the Hub owns everything cluster- and operator-wide.
func (h *Hub) Open(ctx context.Context, req OpenRequest) (*Session, error) {
	if req.Sink == nil {
		return nil, errors.New("console: Open requires a sink")
	}
	if req.SID == "" {
		return nil, errors.New("console: Open requires a sid")
	}

	// Capability gate first: refusing here costs nothing, while a session
	// minted for an old binary would hang until the idle timeout.
	if h.deps.Capabilities != nil {
		ok, err := h.deps.Capabilities.HasCapability(ctx, req.SID, CapabilityConsole)
		if err != nil {
			return nil, fmt.Errorf("console: capability lookup for %q: %w", req.SID, err)
		}
		if !ok {
			return nil, ErrConsoleUnsupported
		}
	}

	sess := &Session{
		KeeperID: h.newID(),
		ClientID: req.ClientID,
		SID:      req.SID,
		AID:      req.AID,
		sink:     req.Sink,
	}
	sess.touch()

	if err := h.register(sess); err != nil {
		return nil, err
	}

	// Recording, before anything that could produce a pty (ADR-0074(g)). The
	// order is the whole guarantee: a shell that has already started cannot be
	// un-started, so the decision "this session will be recorded" must be
	// settled while the only thing at stake is an error frame.
	rec, err := h.deps.Recorder.Open(ctx, RecordingSpec{
		SessionID: sess.KeeperID,
		Kind:      RecordingInteractive,
		SID:       req.SID,
		AID:       req.AID,
		Cols:      req.Cols,
		Rows:      req.Rows,
		OnFailure: func(cause error) {
			// The recording died after the session opened. A console that is no
			// longer being recorded is closed, not degraded — including one
			// sitting idle at a prompt, which is why this is a callback and not
			// a check on the next keystroke.
			h.closeUnrecorded(context.WithoutCancel(ctx), sess, cause, true)
		},
	})
	if err != nil {
		h.unregister(sess)
		return nil, err
	}
	sess.recording = rec

	// Claim the upstream route before the Soul can answer: ConsoleOpened may
	// arrive on another Keeper instance within milliseconds, and it can only be
	// forwarded here once this claim is visible.
	if h.deps.Cluster != nil {
		h.deps.Cluster.ClaimSession(ctx, sess.KeeperID, sess.SID)
	}

	if err := h.deps.Dispatcher.SendConsoleOpen(ctx, req.SID, &keeperv1.ConsoleOpen{
		SessionId: sess.KeeperID,
		TargetSid: req.SID,
		Cols:      req.Cols,
		Rows:      req.Rows,
		Shell:     req.Shell,
	}); err != nil {
		h.unregister(sess)
		if h.deps.Cluster != nil {
			h.deps.Cluster.ReleaseSession(ctx, sess.KeeperID)
		}
		rec.Close(ctx, "dispatch failed")
		return nil, fmt.Errorf("%w: %v", ErrSoulOffline, err)
	}

	h.deps.Metrics.IncSessionsActive()
	h.auditOpened(ctx, sess)
	h.deps.Logger.Info("console: session opened",
		slog.String("session_id", sess.KeeperID),
		slog.String("sid", sess.SID),
		slog.String("aid", sess.AID),
		slog.String("recording_id", rec.ID()),
		slog.String("transport", h.transportOf(ctx, req.SID)),
	)
	return sess, nil
}

// closeUnrecorded ends a session whose recording can no longer be kept.
//
// Fail-closed is only real if it holds AFTER the shell exists: a recording that
// stops halfway leaves exactly the trail ADR-0074(g) says is not adequate — a
// line saying a shell was opened, and no record of what was done with it.
//
// `notify` is false for the callers that RETURN the error, because the socket
// turns that into the same frame and the operator should not read the same
// close twice. The asynchronous path — a store that broke while the operator
// was reading — has nobody to return to, so it notifies.
func (h *Hub) closeUnrecorded(ctx context.Context, sess *Session, cause error, notify bool) {
	if sess.closed.Load() {
		return
	}
	h.deps.Metrics.IncRecordingFailure()
	h.deps.Logger.Warn("console: closing session — its recording could not be kept",
		slog.String("session_id", sess.KeeperID),
		slog.String("sid", sess.SID),
		slog.Any("error", cause),
	)
	h.Close(ctx, sess, CloseRecordingUnavailable)
	if notify {
		sess.sink.DeliverError(NewError(sess.ClientID, ErrCodeRecordingUnavailable,
			"console closed: session recording could not be kept ("+cause.Error()+")"))
	}
}

// transportOf reports which carrier this Soul will use, from its announcement
// (NIM-188). Diagnostic only — nothing branches on it, both carriers work — but
// "is this console on the shared stream" is the first question to ask about an
// interactive session that feels slow, and the answer should not require
// reading the Soul's version.
func (h *Hub) transportOf(ctx context.Context, sid string) string {
	if h.deps.Capabilities == nil {
		return "unknown"
	}
	ok, err := h.deps.Capabilities.HasCapability(ctx, sid, CapabilityConsoleStream)
	switch {
	case err != nil:
		return "unknown"
	case ok:
		return "console_stream"
	default:
		return "eventstream"
	}
}

// register inserts the session under both caps. The two checks and the two
// inserts are one critical section: a per-AID slot taken without the global one
// would leak on the failure path.
func (h *Hub) register(sess *Session) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.limits.MaxSessionsGlobal > 0 && len(h.byKeeperID) >= h.limits.MaxSessionsGlobal {
		return fmt.Errorf("%w: keeper instance at %d sessions", ErrLimitExceeded, h.limits.MaxSessionsGlobal)
	}
	if h.limits.MaxSessionsPerAID > 0 && sess.AID != "" && h.perAID[sess.AID] >= h.limits.MaxSessionsPerAID {
		return fmt.Errorf("%w: operator at %d sessions", ErrLimitExceeded, h.limits.MaxSessionsPerAID)
	}

	h.byKeeperID[sess.KeeperID] = sess
	if sess.AID != "" {
		h.perAID[sess.AID]++
	}
	return nil
}

// unregister removes the session and frees its slots. Idempotent — a
// ConsoleExit racing an operator close must not decrement twice.
func (h *Hub) unregister(sess *Session) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.byKeeperID[sess.KeeperID]; !ok {
		return false
	}
	delete(h.byKeeperID, sess.KeeperID)
	if sess.AID != "" {
		if n := h.perAID[sess.AID] - 1; n > 0 {
			h.perAID[sess.AID] = n
		} else {
			delete(h.perAID, sess.AID)
		}
	}
	return true
}

// lookup returns the session for a Keeper-minted id.
func (h *Hub) lookup(keeperID string) *Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.byKeeperID[keeperID]
}

// Stdin forwards operator keystrokes to the pty.
//
// Recorded BEFORE it is dispatched, and the same order holds in the other
// direction (see [Hub.deliverLocal]): a byte that reached the shell but not the
// record is precisely the gap recording exists to close, and getting the order
// wrong would make the guarantee true only while the store was healthy.
func (h *Hub) Stdin(ctx context.Context, sess *Session, data []byte) error {
	sess.touch()
	if err := sess.recording.Input(data); err != nil {
		h.closeUnrecorded(ctx, sess, err, false)
		return err
	}
	return h.dispatch(ctx, sess, func(ctx context.Context) error {
		return h.deps.Dispatcher.SendConsoleStdin(ctx, sess.SID, &keeperv1.ConsoleStdin{
			SessionId: sess.KeeperID,
			Data:      data,
		})
	})
}

// dispatch sends one Keeper -> Soul frame, or parks it until the session's
// transport is settled (see [Session.readyMu]). Ordering within a session is
// preserved either way: everything before `opened` goes out in arrival order at
// flush time, everything after goes straight through.
func (h *Hub) dispatch(ctx context.Context, sess *Session, send func(context.Context) error) error {
	sess.readyMu.Lock()
	if sess.ready {
		sess.readyMu.Unlock()
		return send(ctx)
	}
	if len(sess.pending) >= maxPendingFrames {
		sess.readyMu.Unlock()
		return fmt.Errorf("%w: %d frames queued", ErrSessionNotReady, maxPendingFrames)
	}
	sess.pending = append(sess.pending, send)
	sess.readyMu.Unlock()
	return nil
}

// markReady releases the parked frames. Called once, when ConsoleOpened proves
// the Soul is up and — on the dedicated transport — that its console stream is
// attached, since the frame arrived on it.
func (h *Hub) markReady(ctx context.Context, sess *Session) {
	sess.readyMu.Lock()
	if sess.ready {
		sess.readyMu.Unlock()
		return
	}
	sess.ready = true
	parked := sess.pending
	sess.pending = nil
	sess.readyMu.Unlock()

	for _, send := range parked {
		if err := send(ctx); err != nil {
			h.deps.Logger.Debug("console: parked frame dispatch failed",
				slog.String("session_id", sess.KeeperID),
				slog.Any("error", err))
		}
	}
}

// Resize applies a new terminal geometry. A zero dimension is dropped here
// rather than sent: a resize is never a request for a 0-sized terminal, and the
// Soul would ignore it anyway.
func (h *Hub) Resize(ctx context.Context, sess *Session, cols, rows uint32) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	sess.touch()
	if err := sess.recording.Resize(cols, rows); err != nil {
		h.closeUnrecorded(ctx, sess, err, false)
		return err
	}
	return h.dispatch(ctx, sess, func(ctx context.Context) error {
		return h.deps.Dispatcher.SendConsoleResize(ctx, sess.SID, &keeperv1.ConsoleResize{
			SessionId: sess.KeeperID,
			Cols:      cols,
			Rows:      rows,
		})
	})
}

// Close ends a session from the Keeper side and tells the Soul to kill the
// process group. Idempotent; safe to call on an already-exited session.
//
// It does NOT synthesize an `exit` frame for the operator: the Soul answers
// every close with a ConsoleExit, and inventing a second terminal here would
// make the client show an exit code that no shell produced. The one case that
// does need synthesis — the socket dying, where nobody is left to receive a
// frame — is handled by [Hub.CloseAllFor].
//
// Unlike input, a close is NEVER parked (see [Hub.dispatch]): parking it would
// wait for a ConsoleOpened that a session closed before it opened will never
// send, and the pty would outlive the operator's intent to kill it. It rides
// EventStream, the same stream that carried ConsoleOpen, so the Soul processes
// the two in order and a close can never overtake the open it belongs to.
func (h *Hub) Close(ctx context.Context, sess *Session, reason CloseReason) {
	if !sess.closed.CompareAndSwap(false, true) {
		return
	}
	if h.unregister(sess) {
		h.deps.Metrics.DecSessionsActive()
	}
	if h.deps.Cluster != nil {
		h.deps.Cluster.ReleaseSession(ctx, sess.KeeperID)
	}
	// A Keeper-side close is terminal too, and it is the ONLY record of why a
	// socket's sessions died: the operator's UI is gone by definition, so no
	// frame reports it. Counting it here is also what lets the totals reconcile
	// against the gauge — the Soul's ConsoleExit for a session closed from this
	// side arrives after it has been unregistered, and is dropped as an orphan
	// frame rather than counted (see [Hub.Deliver]), so this is not a second
	// count of the same session.
	h.deps.Metrics.IncSessionTerminal(reason.Label())

	if err := h.deps.Dispatcher.SendConsoleClose(ctx, sess.SID, &keeperv1.ConsoleClose{
		SessionId: sess.KeeperID,
		Reason:    reason.Text(),
	}); err != nil {
		// The pty still dies: a console never outlives its EventStream, so a
		// Soul we cannot reach is a Soul whose consoles are already gone.
		h.deps.Logger.Debug("console: close dispatch failed (pty dies with its stream)",
			slog.String("session_id", sess.KeeperID),
			slog.String("sid", sess.SID),
			slog.Any("error", err),
		)
	}
	sess.closeRecording(ctx, reason.Text())
	h.auditClosed(ctx, sess, reason.Text())
}

// CloseAllFor tears down every session of one socket — the kill-on-disconnect
// path. Returns how many were closed.
//
// `reason` reaches the Soul-side log, and the operator's UI is already gone by
// definition, so no frames are emitted.
func (h *Hub) CloseAllFor(ctx context.Context, sessions []*Session, reason CloseReason) int {
	n := 0
	for _, sess := range sessions {
		if sess.closed.Load() {
			continue
		}
		h.Close(ctx, sess, reason)
		n++
	}
	return n
}

// Deliver routes one upstream console message to its socket.
//
// Called from the EventStream handler, which knows the SID the frame arrived on
// but sees only the Keeper-minted session id inside it. Three outcomes: the
// session lives here (deliver), it lives on another Keeper instance (forward
// over the cluster bridge), or it has no owner left at all — an orphan, which
// this instance must reap because it is the one holding the stream.
func (h *Hub) Deliver(ctx context.Context, sid string, msg *keeperv1.FromSoul) {
	sessionID := upstreamSessionID(msg)
	if sessionID == "" {
		return
	}

	sess := h.lookup(sessionID)
	if sess == nil {
		// Not ours: either another instance holds the socket, or nobody does.
		// The bridge knows the claim and tells us which.
		if h.deps.Cluster != nil && h.deps.Cluster.Forward(ctx, sid, sessionID, msg) {
			h.reapOrphan(ctx, sid, sessionID, msg)
		}
		return
	}
	if sess.SID != sid {
		// The session id names a host other than the one whose authenticated
		// stream this frame arrived on. A session id is a route, not a
		// credential — the authority is the peer cert (ADR-012(i)) — so a Soul
		// naming somebody else's session gets nothing.
		h.deps.Logger.Warn("console: frame for a session that belongs to another sid — dropping",
			slog.String("session_id", sessionID),
			slog.String("frame_sid", sid),
			slog.String("session_sid", sess.SID),
		)
		return
	}
	h.deliverLocal(ctx, sess, msg)
}

// reapOrphan kills a pty whose operator socket has no owner left anywhere.
//
// This is the cluster-mode hole in kill-on-disconnect. Locally the socket dying
// IS the signal, but when the Keeper that HELD that socket dies, nothing on this
// side notices: the EventStream never broke, so the Soul keeps the shell alive
// and its own kill-on-disconnect never fires. The result would be an
// interactive root shell running on a managed host with nobody watching it —
// the exact failure this feature exists to prevent — surviving until the idle
// sweep of a Hub that no longer has the session.
//
// So the stream holder reaps it. A terminal frame is skipped: the session is
// ending on its own, and closing it again would be noise.
func (h *Hub) reapOrphan(ctx context.Context, sid, sessionID string, msg *keeperv1.FromSoul) {
	if _, terminal := msg.GetPayload().(*keeperv1.FromSoul_ConsoleExit); terminal {
		return
	}
	if !h.deps.Cluster.ShouldReap(sessionID) {
		return
	}
	h.deps.Logger.Warn("console: reaping orphaned session — its socket owner is gone",
		slog.String("session_id", sessionID),
		slog.String("sid", sid),
	)
	if err := h.deps.Dispatcher.SendConsoleClose(ctx, sid, &keeperv1.ConsoleClose{
		SessionId: sessionID,
		Reason:    "socket owner keeper is gone",
	}); err != nil {
		h.deps.Logger.Warn("console: orphan reap dispatch failed",
			slog.String("session_id", sessionID),
			slog.String("sid", sid),
			slog.Any("error", err),
		)
		return
	}
	h.deps.Metrics.IncSessionTerminal("orphan_reaped")
	h.auditOrphanReaped(ctx, sid, sessionID)
}

// DeliverLocal routes a message that arrived over the cluster bridge — it is
// ours by construction, so an unknown id is dropped rather than re-forwarded
// (that would bounce the frame between instances).
func (h *Hub) DeliverLocal(ctx context.Context, msg *keeperv1.FromSoul) {
	sessionID := upstreamSessionID(msg)
	if sessionID == "" {
		return
	}
	sess := h.lookup(sessionID)
	if sess == nil {
		h.deps.Logger.Debug("console: bridged frame for an unknown session — dropping",
			slog.String("session_id", sessionID))
		return
	}
	h.deliverLocal(ctx, sess, msg)
}

func (h *Hub) deliverLocal(ctx context.Context, sess *Session, msg *keeperv1.FromSoul) {
	switch p := msg.GetPayload().(type) {
	case *keeperv1.FromSoul_ConsoleOpened:
		sess.sink.DeliverOpened(NewOpened(sess.ClientID, sess.SID, p.ConsoleOpened.GetPid()))
		// The pty is up and its transport is settled — release anything the
		// operator typed while it was coming up, in order.
		h.markReady(ctx, sess)

	case *keeperv1.FromSoul_ConsoleChunk:
		c := p.ConsoleChunk
		// Recorded before delivery, so the operator never sees a byte that is
		// not in the record. The recording therefore holds MORE than the pane
		// did: a chunk the socket drops under backpressure is already recorded
		// by the time it is dropped.
		if err := sess.recording.Output(c.GetStream(), c.GetData(), c.GetDroppedBytes()); err != nil {
			h.closeUnrecorded(ctx, sess, err, true)
			return
		}
		h.deps.Metrics.AddOutputBytes(len(c.GetData()))
		sess.sink.DeliverChunk(sess.ClientID, c.GetStream(), c.GetData(), c.GetDroppedBytes())

	case *keeperv1.FromSoul_ConsoleExit:
		e := p.ConsoleExit
		// Terminal: free the slot first so a limit-bound operator can reopen
		// immediately, then hand the frame over.
		if sess.closed.CompareAndSwap(false, true) {
			if h.unregister(sess) {
				h.deps.Metrics.DecSessionsActive()
			}
			if h.deps.Cluster != nil {
				h.deps.Cluster.ReleaseSession(ctx, sess.KeeperID)
			}
			h.deps.Metrics.IncSessionTerminal(exitReasonName(e.GetReason()))
			sess.closeRecording(ctx, "soul:"+exitReasonName(e.GetReason()))
			h.auditClosed(ctx, sess, "soul:"+exitReasonName(e.GetReason()))
		}
		sess.sink.DeliverExit(NewExit(sess.ClientID, e.GetExitCode(), e.GetReason(), e.GetErrorMessage()))
	}
}

// upstreamSessionID pulls the session id out of any of the three Soul->Keeper
// console payloads; "" for anything else.
func upstreamSessionID(msg *keeperv1.FromSoul) string {
	switch p := msg.GetPayload().(type) {
	case *keeperv1.FromSoul_ConsoleOpened:
		return p.ConsoleOpened.GetSessionId()
	case *keeperv1.FromSoul_ConsoleChunk:
		return p.ConsoleChunk.GetSessionId()
	case *keeperv1.FromSoul_ConsoleExit:
		return p.ConsoleExit.GetSessionId()
	default:
		return ""
	}
}

// SweepIdle closes every session with no operator input for longer than the
// configured idle timeout and returns how many it reaped. The caller runs it on
// a ticker; a non-positive timeout disables the sweep.
//
// The operator gets an `exit` from the Soul in the normal case — the close is a
// real ConsoleClose, not a synthesized terminal.
func (h *Hub) SweepIdle(ctx context.Context) int {
	if h.limits.IdleTimeout <= 0 {
		return 0
	}
	now := time.Now()

	h.mu.RLock()
	stale := make([]*Session, 0)
	for _, sess := range h.byKeeperID {
		if sess.idleFor(now) > h.limits.IdleTimeout {
			stale = append(stale, sess)
		}
	}
	h.mu.RUnlock()

	for _, sess := range stale {
		h.deps.Logger.Info("console: closing idle session",
			slog.String("session_id", sess.KeeperID),
			slog.String("sid", sess.SID),
			slog.String("aid", sess.AID),
			slog.Duration("idle", sess.idleFor(now)),
		)
		h.Close(ctx, sess, CloseIdleTimeout)
		sess.sink.DeliverError(NewError(sess.ClientID, ErrCodeLimitExceeded,
			"console closed after "+h.limits.IdleTimeout.String()+" without input"))
	}
	return len(stale)
}

// SweepOrphans reaps bridged sessions whose socket owner has vanished, and
// returns how many it closed.
//
// The counterpart to [Hub.SweepIdle], for a failure the idle sweep cannot see:
// an idle session is one THIS Hub holds, while an orphan is one whose Hub died
// on another instance. Only the Keeper holding the host's EventStream is left to
// notice, and only it can act — which is what this does.
func (h *Hub) SweepOrphans(ctx context.Context) int {
	orphans := h.deps.Cluster.SweepOrphans(ctx)
	for _, o := range orphans {
		h.deps.Logger.Warn("console: reaping orphaned session — its socket owner is gone",
			slog.String("session_id", o.SessionID),
			slog.String("sid", o.SID),
		)
		if err := h.deps.Dispatcher.SendConsoleClose(ctx, o.SID, &keeperv1.ConsoleClose{
			SessionId: o.SessionID,
			Reason:    "socket owner keeper is gone",
		}); err != nil {
			h.deps.Logger.Warn("console: orphan reap dispatch failed",
				slog.String("session_id", o.SessionID),
				slog.String("sid", o.SID),
				slog.Any("error", err),
			)
			continue
		}
		h.deps.Metrics.IncSessionTerminal("orphan_reaped")
		h.auditOrphanReaped(ctx, o.SID, o.SessionID)
	}
	return len(orphans)
}

// auditOrphanReaped closes the audit trail of a session this instance killed on
// someone else's behalf.
//
// Without it the log would hold a `console.opened` with no matching close: the
// Keeper that opened the session (and knew the Archon) is the one that died.
// `archon_aid` is therefore empty and the source is keeper_internal — the same
// shape Reaper events use. The session id ties it back to the open, which does
// carry the operator.
func (h *Hub) auditOrphanReaped(ctx context.Context, sid, sessionID string) {
	if h.deps.AuditWriter == nil {
		return
	}
	if err := h.deps.AuditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventConsoleClosed,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: sessionID,
		Payload: map[string]any{
			"sid":        sid,
			"session_id": sessionID,
			"reason":     "orphan reaped: socket owner keeper is gone",
		},
	}); err != nil {
		h.deps.Logger.Warn("console: audit of orphan reap failed",
			slog.String("session_id", sessionID), slog.Any("error", err))
	}
}

// RefreshClaims re-stamps the cluster routing claims of the given sessions.
// A no-op in single-instance mode.
func (h *Hub) RefreshClaims(ctx context.Context, claims []SessionClaim) {
	h.deps.Cluster.RefreshClaims(ctx, claims)
}

// Count returns the number of live sessions on this instance.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byKeeperID)
}

// CountFor returns the number of live sessions held by one Archon.
func (h *Hub) CountFor(aid string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.perAID[aid]
}

// auditOpened records the fact of the session. `recording_id` is the link from
// the fact to the artifact — the audit log stays the index, and the recording
// stays a separate, richer record (ADR-0074(f)).
func (h *Hub) auditOpened(ctx context.Context, sess *Session) {
	h.writeAudit(ctx, audit.EventConsoleOpened, sess, map[string]any{
		"sid":          sess.SID,
		"session_id":   sess.KeeperID,
		"recording_id": sess.RecordingID(),
	})
}

func (h *Hub) auditClosed(ctx context.Context, sess *Session, reason string) {
	h.writeAudit(ctx, audit.EventConsoleClosed, sess, map[string]any{
		"sid":          sess.SID,
		"session_id":   sess.KeeperID,
		"recording_id": sess.RecordingID(),
		"reason":       reason,
	})
}

func (h *Hub) writeAudit(ctx context.Context, evt audit.EventType, sess *Session, payload map[string]any) {
	if h.deps.AuditWriter == nil {
		return
	}
	if err := h.deps.AuditWriter.Write(ctx, &audit.Event{
		EventType:     evt,
		Source:        audit.SourceAPI,
		ArchonAID:     sess.AID,
		CorrelationID: sess.KeeperID,
		Payload:       payload,
	}); err != nil {
		h.deps.Logger.Warn("console: audit write failed",
			slog.String("event", string(evt)),
			slog.String("session_id", sess.KeeperID),
			slog.Any("error", err),
		)
	}
}
