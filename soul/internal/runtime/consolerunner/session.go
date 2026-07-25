package consolerunner

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// session is one live pty and the two goroutines that drive it.
//
// Goroutine split is the whole point: `readLoop` only ever touches the pty and a
// bounded queue, `sendLoop` only ever touches the queue and the EventStream. A
// congested stream therefore cannot block the pty reader, and a flooding pty
// cannot hold the stream's shared write mutex — apply's TaskEvent/RunResult keep
// flowing while a console spews. The price is that output over budget is dropped
// rather than buffered; the dropped byte count rides out on the next chunk.
//
// Ownership rules (they keep teardown race-free):
//   - readLoop owns pty reads, calls cmd.Wait exactly once, then closes outQ.
//   - sendLoop owns every Send for this session, in order:
//     ConsoleOpened → ConsoleChunk* → ConsoleExit, then closes done.
//   - terminate only signals; it never reaps and never sends.
type session struct {
	id     string
	shell  string
	cmd    *exec.Cmd
	ptmx   *os.File
	limits Limits
	logger *slog.Logger

	// outQ carries pty output from readLoop to sendLoop. Bounded: a full queue
	// means the console is producing faster than the stream drains, and the
	// overflow is counted in dropped rather than blocking the reader.
	outQ chan []byte

	// dropped accumulates bytes discarded by flow control since the last chunk
	// that reported them. Written by readLoop, drained by sendLoop.
	dropped atomic.Uint64

	// exitCode is written by readLoop before it closes outQ, read by sendLoop
	// after it observes the close — the channel close is the happens-before edge.
	exitCode int32

	reasonMu sync.Mutex
	reason   keeperv1.ConsoleExitReason

	ptmxOnce  sync.Once
	killOnce  sync.Once
	drainOnce sync.Once

	// draining is closed the moment teardown starts. It releases sendLoop from
	// rate-limit pacing: a throttled session can owe seconds of sleep, and making
	// teardown wait that out would look like a leak while the pty is already dead.
	draining chan struct{}

	// done is closed by sendLoop once ConsoleExit has been handed to the stream.
	// The session is fully finished and reaped at that point.
	done chan struct{}
}

// startSession spawns the shell under a fresh pty and starts both goroutines.
// A non-nil error means nothing was started and nothing needs cleanup.
func startSession(id string, req *keeperv1.ConsoleOpen, limits Limits, logger *slog.Logger, sink Sink, metrics *Metrics, onFinish func()) (*session, error) {
	shell, err := resolveShell(req.GetShell(), limits.Shell)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(shell)
	cmd.Env = consoleEnv()

	ws := &pty.Winsize{
		Cols: firstNonZero(uint16(req.GetCols()), uint16(limits.Cols)),
		Rows: firstNonZero(uint16(req.GetRows()), uint16(limits.Rows)),
	}
	// StartWithSize sets Setsid+Setctty, so the shell becomes the leader of a new
	// session AND process group with pgid == pid. That is what makes the
	// kill-the-whole-group teardown below correct: everything the operator starts
	// inside the console inherits that group and dies with it.
	ptmx, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return nil, fmt.Errorf("console: start %s under pty: %w", shell, err)
	}

	s := &session{
		id:       id,
		shell:    shell,
		cmd:      cmd,
		ptmx:     ptmx,
		limits:   limits,
		logger:   logger,
		outQ:     make(chan []byte, limits.QueueChunks),
		exitCode: -1,
		draining: make(chan struct{}),
		done:     make(chan struct{}),
	}

	go s.readLoop()
	go s.sendLoop(sink, metrics, onFinish)
	return s, nil
}

// pid is the shell's pid, which doubles as its process-group id (Setsid).
func (s *session) pid() int {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// write pushes operator keystrokes into the pty master. Called from the session
// recv-loop; a write to a pty is a bounded kernel buffer copy, so it does not
// block the loop in practice.
func (s *session) write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	_, err := s.ptmx.Write(data)
	return err
}

// resize applies a new winsize; the kernel raises SIGWINCH on the foreground
// process group so full-screen programs reflow. Zeros are ignored — a resize is
// never a request for a zero-sized terminal.
func (s *session) resize(cols, rows uint32) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	return pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// setReason records why the session is ending, first writer wins. readLoop reads
// it once the process is reaped: a natural exit leaves it UNSPECIFIED and becomes
// PROCESS_EXITED, while a terminate() beforehand names the real cause.
func (s *session) setReason(r keeperv1.ConsoleExitReason) {
	s.reasonMu.Lock()
	defer s.reasonMu.Unlock()
	if s.reason == keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_UNSPECIFIED {
		s.reason = r
	}
}

func (s *session) exitReason() keeperv1.ConsoleExitReason {
	s.reasonMu.Lock()
	defer s.reasonMu.Unlock()
	if s.reason == keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_UNSPECIFIED {
		return keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED
	}
	return s.reason
}

// terminate escalates the session to death without ever blocking the caller. It
// returns as soon as the first signal is out; reaping and the ConsoleExit stay
// with readLoop/sendLoop.
//
// The escalation exists because a pty shell is INTERACTIVE, and an interactive
// shell turns job control on: every job it starts lands in its own process group.
// So signalling the shell's group reaches the shell but not the `top` the
// operator left running — the session id is the only thing they all share.
func (s *session) terminate(reason keeperv1.ConsoleExitReason) {
	s.setReason(reason)
	// Release pacing first: whatever is still queued should reach the operator at
	// full speed, and teardown must not be held hostage by a rate limit.
	s.drainOnce.Do(func() { close(s.draining) })
	s.killOnce.Do(func() {
		pid := s.pid()
		if pid <= 0 {
			s.closePTY()
			return
		}
		// Step 1, polite: SIGHUP the shell's own group. A shell handles it by
		// running its EXIT trap and closing down, which lets the last output
		// reach the operator instead of being cut off.
		_ = killGroup(pid, sigHUP)
		go s.escalate(pid)
	})
}

// escalate is the teardown watchdog. Each step is strictly stronger than the
// last, and every step is skipped the moment the session actually finishes.
func (s *session) escalate(pid int) {
	if s.waitDone(s.limits.KillGrace) {
		return
	}

	// Step 2, hangup: closing the pty master makes the kernel raise SIGHUP on the
	// terminal's foreground process group and its session leader — that is what
	// reaches a foreground job in its own group. It also unblocks readLoop, which
	// a child holding the slave open would otherwise keep parked forever.
	s.logger.Warn("console: session ignored SIGHUP, hanging up the pty",
		slog.String("session_id", s.id), slog.Int("pid", pid))
	s.closePTY()
	_ = killGroup(pid, sigKILL)

	if s.waitDone(s.limits.KillGrace) {
		return
	}

	// Step 3, sweep: SIGKILL everything still in this pty's session. This is the
	// backstop that makes kill-on-disconnect a guarantee rather than a hope —
	// without it, a job control child outlives the stream that authorized it.
	killed := killSession(pid, sigKILL)
	s.logger.Warn("console: sweeping leftover processes in the pty session",
		slog.String("session_id", s.id), slog.Int("pid", pid), slog.Int("killed", killed))
}

// waitDone reports whether the session finished within d.
func (s *session) waitDone(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.done:
		return true
	case <-t.C:
		return false
	}
}

func (s *session) closePTY() {
	s.ptmxOnce.Do(func() { _ = s.ptmx.Close() })
}

// readLoop drains the pty until it dies, reaps the process, then closes outQ to
// hand the terminal state to sendLoop.
func (s *session) readLoop() {
	defer close(s.outQ)
	defer s.closePTY()

	buf := make([]byte, s.limits.ReadBufferBytes)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.enqueue(buf[:n])
		}
		if err != nil {
			// A pty master reports the child's death as EIO, not EOF; a
			// force-close reports ErrClosed. Both are the normal end of a
			// session, not a fault worth logging above debug.
			break
		}
	}

	// Wait is the only reaping point, so a console can never leave a zombie
	// behind. It returns as soon as the process is gone; the escalate watchdog
	// guarantees that happens.
	err := s.cmd.Wait()
	s.exitCode = exitCodeOf(err)
}

// enqueue hands a copy of the pty output to sendLoop, dropping it when the queue
// is full. Dropping is deliberate: blocking here would let a flooding console
// stall the pty and, worse, tie the reader to the stream's pace.
func (s *session) enqueue(b []byte) {
	chunk := make([]byte, len(b))
	copy(chunk, b)
	select {
	case s.outQ <- chunk:
	default:
		s.dropped.Add(uint64(len(chunk)))
	}
}

// sendLoop is the session's only writer to the EventStream. It emits
// ConsoleOpened, then paces chunks through a token bucket, then closes with
// exactly one ConsoleExit — including when the stream is already broken, so the
// runner's bookkeeping is always released.
func (s *session) sendLoop(sink Sink, metrics *Metrics, onFinish func()) {
	defer close(s.done)
	defer onFinish()

	if err := sink.SendFromSoul(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{ConsoleOpened: &keeperv1.ConsoleOpened{
			SessionId: s.id,
			Pid:       int32(s.pid()),
			Shell:     s.shell,
		}},
	}); err != nil {
		s.logger.Warn("console: send opened failed (stream broken)",
			slog.String("session_id", s.id), slog.Any("error", err))
	}

	bucket := newTokenBucket(s.limits.RateBytesPerSec, s.limits.BurstBytes)
	var seq uint64
	for chunk := range s.outQ {
		// Pacing happens before the Send, so the wait costs this goroutine time
		// rather than holding the stream's write mutex. It is cut short once
		// teardown starts (s.draining).
		bucket.take(len(chunk), s.draining)
		seq++
		dropped := s.dropped.Swap(0)
		metrics.ObserveOutput(len(chunk), dropped)
		if err := sink.SendFromSoul(&keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleChunk{ConsoleChunk: &keeperv1.ConsoleChunk{
				SessionId:    s.id,
				Stream:       keeperv1.ConsoleStream_CONSOLE_STREAM_STDOUT,
				Data:         chunk,
				Seq:          seq,
				DroppedBytes: dropped,
			}},
		}); err != nil {
			// The stream is gone; keep draining so readLoop is never blocked and
			// the session still reaches its terminal bookkeeping.
			s.logger.Debug("console: send chunk failed (stream broken)",
				slog.String("session_id", s.id), slog.Any("error", err))
		}
	}

	reason := s.exitReason()
	metrics.ObserveSessionEnd(reason)
	if err := sink.SendFromSoul(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleExit{ConsoleExit: &keeperv1.ConsoleExit{
			SessionId: s.id,
			ExitCode:  s.exitCode,
			Reason:    reason,
		}},
	}); err != nil {
		s.logger.Debug("console: send exit failed (stream broken)",
			slog.String("session_id", s.id), slog.Any("error", err))
	}
}

// resolveShell picks the program to run: an explicit per-session request wins,
// then the runner's configured default, then the host's login shell.
//
// An explicit path MUST be absolute. Resolving a bare name through PATH would let
// whoever can open a console pick up a shadowed binary from the daemon's
// environment; the console is already privileged enough without that.
func resolveShell(requested, configured string) (string, error) {
	if requested != "" {
		if !filepath.IsAbs(requested) {
			return "", fmt.Errorf("console: shell %q must be an absolute path", requested)
		}
		return requested, nil
	}
	if configured != "" {
		return configured, nil
	}
	return resolveDefaultShell()
}

// resolveDefaultShell walks the usual candidates and returns the first that
// actually exists on this host.
func resolveDefaultShell() (string, error) {
	candidates := []string{os.Getenv("SHELL"), "/bin/bash", "/bin/sh"}
	for _, c := range candidates {
		if c == "" || !filepath.IsAbs(c) {
			continue
		}
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("console: no usable shell found (tried $SHELL, /bin/bash, /bin/sh)")
}

// consoleEnv is the daemon's environment plus a sane TERM. A service manager
// starts Soul without TERM, and without it curses programs (top, vim) refuse to
// draw — which would make the session a pty in name only.
func consoleEnv() []string {
	env := os.Environ()
	for _, kv := range env {
		if len(kv) >= 5 && kv[:5] == "TERM=" {
			return env
		}
	}
	return append(env, "TERM=xterm-256color")
}

// exitCodeOf maps cmd.Wait's error to a shell-style status: the real code when
// the process exited, 128+N when a signal killed it (the teardown path always
// does), and -1 when it is genuinely unknown (Wait itself failed).
func exitCodeOf(err error) int32 {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code, ok := signalExitCode(ee); ok {
			return code
		}
		return int32(ee.ExitCode())
	}
	return -1
}

func firstNonZero(a, b uint16) uint16 {
	if a != 0 {
		return a
	}
	return b
}
