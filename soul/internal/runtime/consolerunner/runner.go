// Package consolerunner is the Soul-side executor for interactive console (PTY)
// sessions.
//
// A console is a LIVING pty on the host. It is deliberately NOT built on Errand
// (ADR-033): an Errand is one module call answered by a single ≤64 KiB blob,
// whereas a console is a bidirectional byte stream with no known end — a true
// tty, so `top`, `vim` and `cd` behave the way an operator expects.
//
// A session rides its own `ConsoleStream` RPC (NIM-188, transport.go), falling
// back to the EventStream `console_*` members (ADR-012(c)) when the Keeper on the
// other end does not serve it.
//
// Lifecycle, per session_id (a ULID minted by Keeper):
//
//	ConsoleOpen  → spawn a shell under a pty → ConsoleOpened
//	ConsoleStdin → write to the pty master   → ConsoleChunk (pty echo + output)
//	ConsoleResize→ TIOCSWINSZ (+ SIGWINCH)
//	ConsoleClose → kill the process group    → ConsoleExit
//
// Two invariants carry the risk of this component:
//
//   - Kill-on-disconnect. A console never outlives its EventStream session.
//     [Runner.CloseAll] runs on stream teardown (Keeper gone, failback swap,
//     shutdown) and WAITS for every pty to be reaped, so a broken stream cannot
//     leave an orphaned root shell behind. A session on its own stream dies with
//     that stream too, which is the same rule one level down.
//   - Flow control. The pty reader and the stream sender are decoupled by a
//     bounded queue (see session.go). On its own stream a `yes`-style flood
//     throttles the pty; on the EventStream carrier it is dropped and counted,
//     because blocking there would hold the write mutex apply traffic shares.
//
// One Runner belongs to exactly one EventStream session (like the Augur client):
// its Sink is that session's stream, and its Dialer opens console streams on that
// session's connection. A failback swap replaces the Runner, closing the old
// sessions with it.
package consolerunner

import (
	"log/slog"
	"sync"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Sink is the narrow EventStream surface the runner needs: sending FromSoul.
// Declared here (mirroring the Augur client's requestSender) so the package
// depends on neither soul/internal/grpc nor soul/internal/runtime, and so tests
// need no live gRPC connection.
//
// Implementations must be safe for concurrent use — one goroutine per live
// session sends through it. *soulgrpc.StreamSession satisfies that via writeMu.
type Sink interface {
	SendFromSoul(*keeperv1.FromSoul) error
}

// Runner owns the live console sessions of one EventStream session.
type Runner struct {
	sink    Sink
	dialer  Dialer
	limits  Limits
	logger  *slog.Logger
	metrics *Metrics

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
}

// Option tunes a Runner at construction.
type Option func(*Runner)

// WithDialer gives the runner the dedicated console transport (NIM-188): each
// session then dials its own stream and falls back to the Sink only when the
// Keeper on the other end does not serve the RPC. Without it every session
// rides the Sink, which is what a Soul built before the split did.
func WithDialer(d Dialer) Option { return func(r *Runner) { r.dialer = d } }

// New builds a Runner bound to one stream. logger=nil → slog.Default();
// metrics=nil → no-op (the Metrics methods are nil-safe); sink is required —
// a nil one is a wire-up bug, not runtime data.
func New(sink Sink, limits Limits, logger *slog.Logger, metrics *Metrics, opts ...Option) *Runner {
	if sink == nil {
		panic("consolerunner: sink is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Runner{
		sink:     sink,
		limits:   limits.withDefaults(),
		logger:   logger,
		metrics:  metrics,
		sessions: make(map[string]*session),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Open starts a pty session. It never blocks on the stream: the spawn itself is
// a bounded fork/exec, and every Send is left to the session's own goroutine.
//
// A rejection (limit reached, bad shell, stream already torn down) is reported as
// a terminal ConsoleExit with no preceding ConsoleOpened, so Keeper always sees
// exactly one terminal per session_id it minted.
func (r *Runner) Open(req *keeperv1.ConsoleOpen) {
	id := req.GetSessionId()
	if id == "" {
		r.logger.Warn("console: open with empty session_id — ignoring")
		return
	}

	if r.limits.Disabled {
		// The host forbids consoles (`console.enabled: false`). Refusing here,
		// with a terminal, is the point of the switch: Keeper finds out at once
		// instead of holding a session open until an idle timeout. Soul is the
		// last word on its own host — Keeper-side RBAC cannot override it.
		r.logger.Warn("console: open refused, consoles are disabled on this host",
			slog.String("session_id", id))
		r.rejectOpen(id, keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_LIMIT_EXCEEDED,
			"console_disabled_on_host")
		return
	}

	r.mu.Lock()
	_, taken := r.sessions[id]
	switch {
	case r.closed:
		r.mu.Unlock()
		r.rejectOpen(id, keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN,
			"console: EventStream session is closing")
		return
	case taken:
		// Keeper mints a ULID per session, so this is either a duplicated
		// message or a Keeper bug. Dropping it is right either way: replying
		// with a terminal would kill the healthy session that owns the id.
		r.mu.Unlock()
		r.logger.Warn("console: duplicate open for a live session — ignoring",
			slog.String("session_id", id))
		return
	case len(r.sessions) >= r.limits.MaxSessions:
		r.mu.Unlock()
		r.logger.Warn("console: open refused, per-host session limit reached",
			slog.String("session_id", id), slog.Int("limit", r.limits.MaxSessions))
		r.rejectOpen(id, keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_LIMIT_EXCEEDED,
			"console_session_limit_exceeded")
		return
	}
	// Reserve the id before unlocking so two concurrent Opens cannot both pass
	// the limit check. The placeholder is replaced by the real session below.
	r.sessions[id] = nil
	r.mu.Unlock()

	// Dial before the spawn so the ack travels while the pty comes up: the
	// transport is settled by the time the session has anything to send.
	transport := r.dialConsoleStream(id)
	car := carrier{sink: r.sink}
	if transport != nil {
		car = carrier{sink: transport, lossless: true, cut: transport.cut}
	}

	onFinish := func() { r.forget(id) }
	if transport != nil {
		onFinish = func() { r.forget(id); transport.close() }
	}

	s, err := startSession(id, req, r.limits, r.logger, car, r.metrics, onFinish)
	if err != nil {
		r.forget(id)
		if transport != nil {
			transport.cut()
		}
		r.logger.Warn("console: open failed",
			slog.String("session_id", id), slog.Any("error", err))
		r.rejectOpen(id, keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_OPEN_FAILED, err.Error())
		return
	}

	r.mu.Lock()
	if r.closed {
		// CloseAll landed between the reservation and here; honour the teardown
		// instead of leaving a pty that outlives its stream.
		r.mu.Unlock()
		if transport != nil {
			transport.cut()
		}
		s.terminate(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN)
		return
	}
	r.sessions[id] = s
	active := len(r.sessions)
	r.mu.Unlock()

	// Only now: a stream that breaks on its first read must find a session to
	// kill, and the ack that settles the transport arrives on this loop.
	if transport != nil {
		go r.consoleStreamLoop(transport)
	}

	r.metrics.ObserveSessionStart(active)
	r.logger.Info("console: session opened",
		slog.String("session_id", id),
		slog.String("shell", s.shell),
		slog.Int("pid", s.pid()),
		slog.Int("active", active))
}

// Stdin writes operator keystrokes into the pty. An unknown session_id is a
// benign race (the session exited and its ConsoleExit is in flight), so it is
// silently ignored.
func (r *Runner) Stdin(req *keeperv1.ConsoleStdin) {
	s := r.lookup(req.GetSessionId())
	if s == nil {
		return
	}
	if err := s.write(req.GetData()); err != nil {
		r.logger.Debug("console: stdin write failed",
			slog.String("session_id", req.GetSessionId()), slog.Any("error", err))
	}
}

// Resize applies a new terminal geometry to a live session.
func (r *Runner) Resize(req *keeperv1.ConsoleResize) {
	s := r.lookup(req.GetSessionId())
	if s == nil {
		return
	}
	if err := s.resize(req.GetCols(), req.GetRows()); err != nil {
		r.logger.Debug("console: resize failed",
			slog.String("session_id", req.GetSessionId()), slog.Any("error", err))
	}
}

// Close ends a session on Keeper's request. It returns as soon as the kill is
// signalled; ConsoleExit follows from the session's own goroutine once the
// process group is reaped. Idempotent — closing an unknown session is a no-op.
func (r *Runner) Close(req *keeperv1.ConsoleClose) {
	s := r.lookup(req.GetSessionId())
	if s == nil {
		return
	}
	r.logger.Info("console: close received",
		slog.String("session_id", req.GetSessionId()),
		slog.String("reason", req.GetReason()))
	s.terminate(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_CLOSED_BY_KEEPER)
}

// CloseAll terminates every live session and WAITS for them to be reaped. This
// is the kill-on-disconnect invariant: the caller (handleSession's defer) must
// not return while a pty from its stream is still running, or a broken Keeper
// connection would leave an orphaned root shell on the host.
//
// The wait is bounded — a session that survives both the SIGKILL and the
// force-close escalation is reported rather than hanging Soul's shutdown.
// Terminal: after CloseAll the Runner refuses new Opens.
func (r *Runner) CloseAll(reason keeperv1.ConsoleExitReason) {
	r.mu.Lock()
	r.closed = true
	live := make([]*session, 0, len(r.sessions))
	for _, s := range r.sessions {
		if s != nil {
			live = append(live, s)
		}
	}
	r.mu.Unlock()

	if len(live) == 0 {
		return
	}
	r.logger.Info("console: closing all sessions", slog.Int("count", len(live)))
	for _, s := range live {
		s.terminate(reason)
	}

	// Budget covers the full escalation (SIGHUP → pty hangup + SIGKILL → session
	// sweep → cut the transport) plus slack for the final Send.
	budget := time.NewTimer(4*r.limits.KillGrace + time.Second)
	defer budget.Stop()
	for _, s := range live {
		select {
		case <-s.done:
		case <-budget.C:
			r.logger.Error("console: sessions still running after teardown budget — possible leak",
				slog.String("session_id", s.id), slog.Int("pid", s.pid()))
			return
		}
	}
}

// ActiveCount reports how many sessions are live. Used by metrics and tests as
// the leak assertion.
func (r *Runner) ActiveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

func (r *Runner) lookup(id string) *session {
	if id == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

// forget drops a finished session. Called from the session's own goroutine once
// its ConsoleExit has been handed to the stream.
func (r *Runner) forget(id string) {
	r.mu.Lock()
	delete(r.sessions, id)
	active := len(r.sessions)
	r.mu.Unlock()
	r.metrics.ObserveSessionActive(active)
}

// rejectOpen reports a session that never started. No ConsoleOpened precedes it,
// and exit_code is -1: there was no process to exit.
func (r *Runner) rejectOpen(id string, reason keeperv1.ConsoleExitReason, msg string) {
	r.metrics.ObserveSessionEnd(reason)
	if err := r.sink.SendFromSoul(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: &keeperv1.ConsoleExit{
			SessionId:    id,
			ExitCode:     -1,
			Reason:       reason,
			ErrorMessage: msg,
		}},
	}); err != nil {
		r.logger.Debug("console: send open-rejection failed (stream broken)",
			slog.String("session_id", id), slog.Any("error", err))
	}
}
