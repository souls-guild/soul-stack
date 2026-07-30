package consolerunner

import (
	"bytes"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// recordingSink captures everything a session sends, standing in for a live
// EventStream. Concurrency-safe because each session sends from its own
// goroutine — exactly like the real StreamSession's writeMu.
type recordingSink struct {
	mu   sync.Mutex
	msgs []*keeperv1.FromSoul

	// delay simulates a congested stream so flow control can be exercised.
	delay time.Duration
	// err, when set, simulates a broken stream.
	err error
}

func (s *recordingSink) SendFromSoul(msg *keeperv1.FromSoul) error {
	s.mu.Lock()
	delay, err := s.delay, s.err
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	s.mu.Lock()
	s.msgs = append(s.msgs, msg)
	s.mu.Unlock()
	return err
}

func (s *recordingSink) snapshot() []*keeperv1.FromSoul {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*keeperv1.FromSoul(nil), s.msgs...)
}

// output concatenates every ConsoleChunk payload of a session, in order.
func (s *recordingSink) output(sessionID string) string {
	var buf bytes.Buffer
	for _, m := range s.snapshot() {
		if c := m.GetConsoleChunk(); c != nil && c.GetSessionId() == sessionID {
			buf.Write(c.GetData())
		}
	}
	return buf.String()
}

func (s *recordingSink) opened(sessionID string) *keeperv1.ConsoleOpened {
	for _, m := range s.snapshot() {
		if o := m.GetConsoleOpened(); o != nil && o.GetSessionId() == sessionID {
			return o
		}
	}
	return nil
}

func (s *recordingSink) exit(sessionID string) *keeperv1.ConsoleExit {
	for _, m := range s.snapshot() {
		if e := m.GetConsoleExit(); e != nil && e.GetSessionId() == sessionID {
			return e
		}
	}
	return nil
}

// droppedTotal sums the flow-control drop counters reported across all chunks.
func (s *recordingSink) droppedTotal(sessionID string) uint64 {
	var total uint64
	for _, m := range s.snapshot() {
		if c := m.GetConsoleChunk(); c != nil && c.GetSessionId() == sessionID {
			total += c.GetDroppedBytes()
		}
	}
	return total
}

// capturedLog is a concurrency-safe sink for the runner's own log lines. Sessions
// log from their own goroutines, so a bare bytes.Buffer would be a data race in a
// package that is run under `-race`.
type capturedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// testLogger CAPTURES the runner's account of the run and prints it when the test
// fails — it does not silence it (NIM-349).
//
// It used to be `io.Discard` at `LevelError`, described as "keeps test output
// quiet but still exercises the logging paths". Both halves of that were wrong.
// Below the threshold slog never calls Handle at all, so the Warn and Info paths
// were not exercised; and every line that WAS emitted went nowhere. The lines
// thrown away are exactly the ones that explain a missing terminal event —
// `console: open refused, consoles are disabled on this host`, `console: open
// failed`, `console: duplicate open for a live session`, `console: sessions still
// running after teardown budget — possible leak`.
//
// That is what made `TestLiveGRPC_ConsoleRoundTripOverItsOwnStream` unreadable
// when it failed in CI with `timed out after 10s waiting for ConsoleExit`: the
// run takes ~1.4 s locally, so a 10 s budget was not marginally missed, something
// stalled — and the account of what stalled had been discarded. A test whose
// failure carries no evidence cannot distinguish "the event never happened" from
// "it had not happened yet when we gave up", and those have opposite answers.
//
// Debug level on purpose: the cost is a few KiB per test, and the point is to
// have the whole sequence rather than its last line.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	l, _ := testLoggerWithCapture(t)
	return l
}

// testLoggerWithCapture is testLogger plus read access to what was captured, for
// guards that assert on the runner's own account rather than on its side effects
// (NIM-397). Same behaviour otherwise: Debug level, printed only on failure.
func testLoggerWithCapture(t *testing.T) (*slog.Logger, *capturedLog) {
	t.Helper()
	captured := &capturedLog{}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		if s := captured.String(); s != "" {
			t.Logf("runner log for this test (what the code itself reported):\n%s", s)
		} else {
			t.Logf("runner log for this test: EMPTY — the runner logged nothing at all, " +
				"so the code under test was never reached, not merely slow")
		}
	})
	return slog.New(slog.NewTextHandler(captured, &slog.HandlerOptions{Level: slog.LevelDebug})), captured
}

// requireShell skips a test on a host with no usable shell (the pty tests are
// meaningless there) and returns the resolved path.
func requireShell(t *testing.T) string {
	t.Helper()
	sh, err := resolveDefaultShell()
	if err != nil {
		t.Skipf("no usable shell on this host: %v", err)
	}
	return sh
}

// floodProgram is a deterministic runaway producer for the flow-control tests.
//
// Those tests must NOT drive an interactive shell. A developer's `$SHELL` may
// carry a themed prompt and a line editor that redraw on every keystroke, so the
// pty traffic — and therefore whether the queue overflows — would depend on
// someone's rc files rather than on the code under test. `yes` floods on its own,
// with no rc files and no line editor.
const floodProgram = "/usr/bin/yes"

// requireProgram skips when a test's fixture binary is missing on this host.
func requireProgram(t *testing.T, path string) string {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s is not available on this host: %v", path, err)
	}
	return path
}

// waitShellReady blocks until the shell inside a console actually answers.
//
// ConsoleOpened only means the process was forked. An interactive shell may still
// be sourcing its rc files at that point, and keystrokes typed into that window
// can be swallowed — which is why the probe is RETRIED rather than sent once.
// Every test that types into a console must call this first, or it races the
// developer's own shell startup.
//
// The probe is written as `echo REA""DY` so that the terminal's echo of the
// command text is distinguishable from the shell's actual output: the echo
// contains `REA""DY`, only the answer contains `READY`.
func waitShellReady(t *testing.T, r *Runner, sink *recordingSink, id string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r.Stdin(&keeperv1.ConsoleStdin{SessionId: id, Data: []byte("echo REA\"\"DY\n")})
		probe := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(probe) {
			if strings.Contains(sink.output(id), "READY") {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatalf("shell in session %q never answered a probe: %d chunks, %d dropped bytes, exit=%v, output:\n%q",
		id, chunkCount(sink, id), sink.droppedTotal(id), sink.exit(id), sink.output(id))
}

// waitFor polls cond until it holds or the deadline passes. Used instead of a
// fixed sleep so the pty tests stay fast and non-flaky.
//
// On timeout it reports how long it actually waited and how many times it asked,
// and says out loud that the runner's own log follows (testLogger prints it on
// failure). The old message was one line — `timed out after 10s waiting for X` —
// which is indistinguishable between "this never happens" and "this had not
// happened yet", and those are a regression and a slow machine respectively
// (NIM-349). Raising the budget is not the fix and is why it is named here: these
// waits carry a large margin already, so a fired deadline means a stall worth
// reading, not a number worth increasing.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(timeout)
	polls := 0
	for time.Now().Before(deadline) {
		polls++
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waiting for %s: still false after %s (%d polls).\n"+
		"This is a stall, not a tight budget — do NOT raise the timeout. Read the runner\n"+
		"log printed below: the whole sequence present means the assertion looks at the\n"+
		"wrong state, the sequence stopping partway names the step that hung, and no log\n"+
		"at all means the code under test was never reached.",
		what, time.Since(start).Round(time.Millisecond), polls)
}

// processAlive reports whether pid still exists. Signal 0 performs the existence
// and permission check without delivering anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// openFDCount counts this process's open file descriptors — the pty leak
// assertion. A session that fails to close its master shows up here.
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd unavailable: %v", err)
	}
	return len(entries)
}

// readPID parses a pid a test shell printed into the console.
func readPID(s string) int {
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return pid
}
