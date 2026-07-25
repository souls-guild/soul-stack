package consolerunner

import (
	"bytes"
	"io"
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

// testLogger keeps test output quiet but still exercises the logging paths.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
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
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
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
