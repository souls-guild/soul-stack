package consolerunner

import (
	"strings"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestConsole_IsARealTTY is the acceptance check that this is a pty and not a
// dressed-up Errand: keystrokes reach the shell, the terminal echoes them back,
// and `stty` — which only works on a real tty — reports the geometry we asked
// for. Without a pty none of the three would hold.
func TestConsole_IsARealTTY(t *testing.T) {
	requireShell(t)
	sink := &recordingSink{}
	r := New(sink, Limits{Cols: 80, Rows: 24}, testLogger(t), nil)
	t.Cleanup(func() { r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN) })

	r.Open(&keeperv1.ConsoleOpen{SessionId: "s1", Cols: 80, Rows: 24})
	waitFor(t, 5*time.Second, "ConsoleOpened", func() bool { return sink.opened("s1") != nil })

	if pid := sink.opened("s1").GetPid(); !processAlive(int(pid)) {
		t.Fatalf("shell pid %d is not running after ConsoleOpened", pid)
	}
	waitShellReady(t, r, sink, "s1")

	r.Stdin(&keeperv1.ConsoleStdin{SessionId: "s1", Data: []byte("stty size\n")})
	waitFor(t, 5*time.Second, "stty output", func() bool {
		return strings.Contains(sink.output("s1"), "24 80")
	})

	// The command text itself coming back is the terminal's own echo — proof of
	// a tty, since nothing in Soul echoes stdin.
	if !strings.Contains(sink.output("s1"), "stty size") {
		t.Errorf("no terminal echo of the typed command; output:\n%s", sink.output("s1"))
	}
}

// TestConsole_ResizeAppliesWinsize covers ConsoleResize end to end: the new
// geometry must be visible to the program running inside the terminal, which is
// what makes full-screen tools reflow.
func TestConsole_ResizeAppliesWinsize(t *testing.T) {
	requireShell(t)
	sink := &recordingSink{}
	r := New(sink, Limits{}, testLogger(t), nil)
	t.Cleanup(func() { r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN) })

	r.Open(&keeperv1.ConsoleOpen{SessionId: "s1", Cols: 80, Rows: 24})
	waitFor(t, 5*time.Second, "ConsoleOpened", func() bool { return sink.opened("s1") != nil })

	waitShellReady(t, r, sink, "s1")
	r.Resize(&keeperv1.ConsoleResize{SessionId: "s1", Cols: 120, Rows: 40})
	r.Stdin(&keeperv1.ConsoleStdin{SessionId: "s1", Data: []byte("stty size\n")})
	waitFor(t, 5*time.Second, "resized stty output", func() bool {
		return strings.Contains(sink.output("s1"), "40 120")
	})
}

// TestConsole_NaturalExitReportsCode checks the terminal contract for a shell
// that ends on its own: exactly one ConsoleExit, carrying the real status.
func TestConsole_NaturalExitReportsCode(t *testing.T) {
	requireShell(t)
	sink := &recordingSink{}
	r := New(sink, Limits{}, testLogger(t), nil)

	r.Open(&keeperv1.ConsoleOpen{SessionId: "s1"})
	waitFor(t, 5*time.Second, "ConsoleOpened", func() bool { return sink.opened("s1") != nil })

	waitShellReady(t, r, sink, "s1")
	r.Stdin(&keeperv1.ConsoleStdin{SessionId: "s1", Data: []byte("exit 7\n")})
	waitFor(t, 5*time.Second, "ConsoleExit", func() bool { return sink.exit("s1") != nil })

	exit := sink.exit("s1")
	if exit.GetExitCode() != 7 {
		t.Errorf("exit_code = %d, want 7", exit.GetExitCode())
	}
	if exit.GetReason() != keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED {
		t.Errorf("reason = %v, want PROCESS_EXITED", exit.GetReason())
	}
	waitFor(t, time.Second, "session bookkeeping released", func() bool { return r.ActiveCount() == 0 })
}

// TestConsole_CloseKillsSessionIncludingJobControlChildren is the central leak
// guard.
//
// It reproduces the case the naive implementation gets wrong: an interactive
// shell enables job control, so the command the operator runs lands in its OWN
// process group. Signalling the shell's group alone would leave that child alive
// on the host after the console is gone. The assertion is that both the shell and
// its child are dead once ConsoleExit lands.
func TestConsole_CloseKillsSessionIncludingJobControlChildren(t *testing.T) {
	requireShell(t)
	sink := &recordingSink{}
	r := New(sink, Limits{KillGrace: 300 * time.Millisecond}, testLogger(t), nil)

	r.Open(&keeperv1.ConsoleOpen{SessionId: "s1"})
	waitFor(t, 5*time.Second, "ConsoleOpened", func() bool { return sink.opened("s1") != nil })
	shellPID := int(sink.opened("s1").GetPid())
	waitShellReady(t, r, sink, "s1")

	// Start a long sleeper and have the shell report its pid, so the test can
	// assert on the child directly rather than trusting the teardown.
	r.Stdin(&keeperv1.ConsoleStdin{SessionId: "s1", Data: []byte("sleep 300 & echo CHILD=$!\n")})
	// Wait for the expanded pid, not for the literal `CHILD=$!` that the terminal
	// echoes back first.
	waitFor(t, 5*time.Second, "child pid", func() bool {
		return childPIDIn(sink.output("s1")) > 0
	})
	childPID := childPIDIn(sink.output("s1"))
	if !processAlive(childPID) {
		t.Fatalf("child %d should be running before close", childPID)
	}

	r.Close(&keeperv1.ConsoleClose{SessionId: "s1", Reason: "test"})
	waitFor(t, 5*time.Second, "ConsoleExit", func() bool { return sink.exit("s1") != nil })

	if got := sink.exit("s1").GetReason(); got != keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_CLOSED_BY_KEEPER {
		t.Errorf("reason = %v, want CLOSED_BY_KEEPER", got)
	}
	waitFor(t, 5*time.Second, "shell to die", func() bool { return !processAlive(shellPID) })
	waitFor(t, 5*time.Second, "job-control child to die", func() bool { return !processAlive(childPID) })
	if r.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d after close, want 0", r.ActiveCount())
	}
}

// TestConsole_CloseAllOnStreamTeardown is the kill-on-disconnect invariant: when
// the EventStream goes away, CloseAll must not return until every pty it
// authorized is dead. A stream break that leaves a live root shell behind is the
// worst failure this component can have.
func TestConsole_CloseAllOnStreamTeardown(t *testing.T) {
	requireShell(t)
	sink := &recordingSink{}
	r := New(sink, Limits{KillGrace: 300 * time.Millisecond}, testLogger(t), nil)

	const sessions = 3
	pids := make([]int, 0, sessions)
	for _, id := range []string{"a", "b", "c"} {
		r.Open(&keeperv1.ConsoleOpen{SessionId: id})
		waitFor(t, 5*time.Second, "ConsoleOpened "+id, func() bool { return sink.opened(id) != nil })
		pids = append(pids, int(sink.opened(id).GetPid()))
	}
	if r.ActiveCount() != sessions {
		t.Fatalf("ActiveCount = %d, want %d", r.ActiveCount(), sessions)
	}

	// Simulate the stream having already broken: sends fail, yet teardown must
	// still complete and still reap.
	sink.mu.Lock()
	sink.err = errStreamBroken
	sink.mu.Unlock()

	r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN)

	// CloseAll is synchronous by contract — no polling here on purpose.
	if r.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d after CloseAll, want 0", r.ActiveCount())
	}
	for _, pid := range pids {
		if processAlive(pid) {
			t.Errorf("pty process %d survived CloseAll — orphaned shell", pid)
		}
	}
}

// TestConsole_NoFDLeakAcrossSessions guards the other half of the pty lifecycle:
// every session must give its master fd back. A leak here is invisible in normal
// use and only bites after a few hundred consoles, so it is asserted directly.
func TestConsole_NoFDLeakAcrossSessions(t *testing.T) {
	requireShell(t)
	sink := &recordingSink{}
	r := New(sink, Limits{KillGrace: 300 * time.Millisecond}, testLogger(t), nil)

	// One warm-up cycle first: the runtime allocates epoll/pidfd descriptors on
	// the first pty, and counting those as a leak would make this test lie.
	runSessionCycle(t, r, sink, "warmup")
	before := openFDCount(t)

	for _, id := range []string{"c1", "c2", "c3", "c4", "c5"} {
		runSessionCycle(t, r, sink, id)
	}

	after := openFDCount(t)
	// An exact match is the intent; a small allowance keeps the test honest about
	// unrelated runtime descriptors rather than flaky.
	if after > before+2 {
		t.Errorf("fd count grew from %d to %d across 5 console sessions — pty master leak", before, after)
	}
	if r.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", r.ActiveCount())
	}
}

// TestConsole_LimitExceededIsRefusedWithTerminal checks the per-host ceiling: the
// refusal must still be a terminal ConsoleExit (Keeper minted a session_id and
// waits for exactly one), and no pty may be spawned for it.
func TestConsole_LimitExceededIsRefusedWithTerminal(t *testing.T) {
	requireShell(t)
	sink := &recordingSink{}
	r := New(sink, Limits{MaxSessions: 1, KillGrace: 300 * time.Millisecond}, testLogger(t), nil)
	t.Cleanup(func() { r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN) })

	r.Open(&keeperv1.ConsoleOpen{SessionId: "first"})
	waitFor(t, 5*time.Second, "first ConsoleOpened", func() bool { return sink.opened("first") != nil })

	r.Open(&keeperv1.ConsoleOpen{SessionId: "second"})
	waitFor(t, 2*time.Second, "second ConsoleExit", func() bool { return sink.exit("second") != nil })

	if sink.opened("second") != nil {
		t.Error("a refused session must not report ConsoleOpened")
	}
	exit := sink.exit("second")
	if exit.GetReason() != keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_LIMIT_EXCEEDED {
		t.Errorf("reason = %v, want LIMIT_EXCEEDED", exit.GetReason())
	}
	if exit.GetExitCode() != -1 {
		t.Errorf("exit_code = %d, want -1 (no process ever ran)", exit.GetExitCode())
	}
	if r.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1 (the refusal must not occupy a slot)", r.ActiveCount())
	}
}

// TestConsole_RelativeShellIsRejected pins the defense-in-depth rule from
// console.proto: a shell path is never resolved through PATH, so a shadowed
// binary in the daemon's environment cannot become the console.
func TestConsole_RelativeShellIsRejected(t *testing.T) {
	sink := &recordingSink{}
	r := New(sink, Limits{}, testLogger(t), nil)

	r.Open(&keeperv1.ConsoleOpen{SessionId: "s1", Shell: "bash"})
	waitFor(t, 2*time.Second, "ConsoleExit", func() bool { return sink.exit("s1") != nil })

	exit := sink.exit("s1")
	if exit.GetReason() != keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_OPEN_FAILED {
		t.Errorf("reason = %v, want OPEN_FAILED", exit.GetReason())
	}
	if !strings.Contains(exit.GetErrorMessage(), "absolute path") {
		t.Errorf("error_message = %q, want it to name the absolute-path rule", exit.GetErrorMessage())
	}
	if r.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d after a failed open, want 0", r.ActiveCount())
	}
}

// TestConsole_UnknownSessionIsIgnored covers the benign race where Keeper's
// message and the session's own exit cross paths. None of these may panic or
// resurrect anything.
func TestConsole_UnknownSessionIsIgnored(t *testing.T) {
	sink := &recordingSink{}
	r := New(sink, Limits{}, testLogger(t), nil)

	r.Stdin(&keeperv1.ConsoleStdin{SessionId: "ghost", Data: []byte("x")})
	r.Resize(&keeperv1.ConsoleResize{SessionId: "ghost", Cols: 10, Rows: 10})
	r.Close(&keeperv1.ConsoleClose{SessionId: "ghost"})

	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("sent %d messages for an unknown session, want 0", got)
	}
}

// TestConsole_OpenAfterCloseAllIsRefused makes the runner terminal: once the
// stream is torn down, a late ConsoleOpen must not spawn a pty that would outlive
// it.
func TestConsole_OpenAfterCloseAllIsRefused(t *testing.T) {
	sink := &recordingSink{}
	r := New(sink, Limits{}, testLogger(t), nil)
	r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN)

	r.Open(&keeperv1.ConsoleOpen{SessionId: "late"})
	waitFor(t, 2*time.Second, "ConsoleExit", func() bool { return sink.exit("late") != nil })

	if sink.opened("late") != nil {
		t.Error("no pty may be started after CloseAll")
	}
	if got := sink.exit("late").GetReason(); got != keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN {
		t.Errorf("reason = %v, want SOUL_SHUTDOWN", got)
	}
	if r.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", r.ActiveCount())
	}
}

// runSessionCycle opens a console, waits for it to be live, closes it and waits
// for the terminal — one complete lifecycle, used by the fd-leak guard.
func runSessionCycle(t *testing.T, r *Runner, sink *recordingSink, id string) {
	t.Helper()
	r.Open(&keeperv1.ConsoleOpen{SessionId: id})
	waitFor(t, 5*time.Second, "ConsoleOpened "+id, func() bool { return sink.opened(id) != nil })
	r.Close(&keeperv1.ConsoleClose{SessionId: id})
	waitFor(t, 5*time.Second, "ConsoleExit "+id, func() bool { return sink.exit(id) != nil })
	waitFor(t, 5*time.Second, "release of "+id, func() bool { return r.ActiveCount() == 0 })
}

// childPIDIn extracts the pid the test shell printed as `CHILD=<pid>`, or 0 when
// the expanded line has not arrived yet. The terminal echoes the typed command
// first, so a line still containing `$!` is the echo and not the answer.
func childPIDIn(out string) int {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if !strings.HasPrefix(line, "CHILD=") || strings.Contains(line, "$!") {
			continue
		}
		if pid := readPID(strings.TrimPrefix(line, "CHILD=")); pid > 0 {
			return pid
		}
	}
	return 0
}
