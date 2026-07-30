package consolerunner

import (
	"strings"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestConsole_DisabledHostRefusesEveryOpen covers `console.enabled: false`: the
// host is the last word on whether an interactive shell may run on it, whatever
// Keeper-side RBAC allows. The refusal must be a terminal, so Keeper drops its
// session immediately instead of waiting out an idle timeout.
func TestConsole_DisabledHostRefusesEveryOpen(t *testing.T) {
	sink := &recordingSink{}
	r := New(sink, Limits{Disabled: true}, testLogger(t), nil)

	r.Open(&keeperv1.ConsoleOpen{SessionId: "s1"})
	waitFor(t, 2*time.Second, "ConsoleExit", func() bool { return sink.exit("s1") != nil })

	if sink.opened("s1") != nil {
		t.Error("a disabled host must not start a pty")
	}
	if got := sink.exit("s1").GetErrorMessage(); !strings.Contains(got, "console_disabled_on_host") {
		t.Errorf("error_message = %q, want it to name the host-side switch", got)
	}
	if r.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", r.ActiveCount())
	}
}

// TestConsole_SingleSessionPolicy is the operator policy this config surface
// exists for: `max_sessions: 1` means one console at a time on the host, and the
// slot must come back once that console ends.
func TestConsole_SingleSessionPolicy(t *testing.T) {
	requireShell(t)
	sink := &recordingSink{}
	r := New(sink, Limits{MaxSessions: 1, KillGrace: 300 * time.Millisecond}, testLogger(t), nil)
	t.Cleanup(func() { r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN) })

	r.Open(&keeperv1.ConsoleOpen{SessionId: "first"})
	waitFor(t, 5*time.Second, "first console", func() bool { return sink.opened("first") != nil })

	r.Open(&keeperv1.ConsoleOpen{SessionId: "second"})
	waitFor(t, 2*time.Second, "second refused", func() bool { return sink.exit("second") != nil })
	if sink.opened("second") != nil {
		t.Fatal("max_sessions=1 must allow only one live console")
	}

	// Freeing the slot must let the next operator in — the limit is a ceiling on
	// concurrency, not a quota for the lifetime of the stream.
	r.Close(&keeperv1.ConsoleClose{SessionId: "first"})
	waitFor(t, 5*time.Second, "first to end", func() bool { return r.ActiveCount() == 0 })

	r.Open(&keeperv1.ConsoleOpen{SessionId: "third"})
	waitFor(t, 5*time.Second, "third console", func() bool { return sink.opened("third") != nil })
}

// TestConsole_ThrottledSessionStillTearsDownFast is the teardown counterpart of
// the rate limit becoming operator-configurable.
//
// At `rate_limit_kbps: 1` a queued chunk owes tens of seconds of pacing. Teardown
// must not wait that out: the pty is killed immediately, so a CloseAll that sat
// out its budget would log a phantom leak and leave a sender goroutine behind.
func TestConsole_ThrottledSessionStillTearsDownFast(t *testing.T) {
	requireProgram(t, floodProgram)
	sink := &recordingSink{}
	r := New(sink, Limits{
		Shell:           floodProgram, // a deterministic producer, not someone's shell
		RateBytesPerSec: 1024,         // 1 KB/s
		BurstBytes:      1,            // no free burst: the very first chunk has to pace
		KillGrace:       300 * time.Millisecond,
	}, testLogger(t), nil)

	r.Open(&keeperv1.ConsoleOpen{SessionId: "slow"})
	waitFor(t, 5*time.Second, "ConsoleOpened", func() bool { return sink.opened("slow") != nil })
	// Let the sender fall behind and park in the token bucket.
	waitFor(t, 5*time.Second, "output to start flowing", func() bool { return chunkCount(sink, "slow") > 0 })
	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN)
	elapsed := time.Since(start)

	// Two regimes, an order of magnitude apart, and the threshold belongs between
	// them rather than hugging one of them:
	//
	//   correct teardown — bounded by escalation, 3*KillGrace + 1s ≈ 1.9s here;
	//   the bug — teardown waits out the token bucket. `yes` fills the queue
	//   (DefaultQueueChunks=32 chunks of up to DefaultReadBufferBytes=32 KiB) and
	//   at RateBytesPerSec=1024 even ONE chunk owes ~32 s of pacing, a full queue
	//   ~1000 s.
	//
	// The threshold used to be 2s — a 5% margin over the legitimate budget, so on
	// a loaded machine or under -race it measured the machine rather than the
	// behaviour, and this test became one of the victims that made the whole
	// integration job look flaky (NIM-349). 10s is ~5x the escalation budget and
	// still ~3x below the cheapest possible bug signature, so it separates the two
	// regimes with margin on both sides.
	//
	// This is not the "widen the window until it passes" move that
	// docs/testing/README.md warns against: that one lets a broken thing through,
	// whereas the bug this guards against overshoots the new threshold by more
	// than 3x and still fails.
	const teardownBudget = 10 * time.Second
	if elapsed > teardownBudget {
		t.Errorf("CloseAll took %s on a throttled session (budget %s) — teardown is waiting out the rate limit;"+
			" correct teardown is bounded by escalation at ~%s, pacing out one queued chunk would take ~%s",
			elapsed, teardownBudget, 3*300*time.Millisecond+time.Second, 32*time.Second)
	}
	if r.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d after CloseAll, want 0", r.ActiveCount())
	}
}

// TestConsole_ConfiguredShellIsUsed pins that `console.shell` actually selects
// the program, and that it is reported back so Keeper logs what really ran.
func TestConsole_ConfiguredShellIsUsed(t *testing.T) {
	requireShell(t)
	sink := &recordingSink{}
	r := New(sink, Limits{Shell: "/bin/sh", KillGrace: 300 * time.Millisecond}, testLogger(t), nil)
	t.Cleanup(func() { r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN) })

	r.Open(&keeperv1.ConsoleOpen{SessionId: "s1"})
	waitFor(t, 5*time.Second, "ConsoleOpened", func() bool { return sink.opened("s1") != nil })

	if got := sink.opened("s1").GetShell(); got != "/bin/sh" {
		t.Errorf("shell = %q, want /bin/sh from the host config", got)
	}
}

// TestLimits_DefaultsFillEveryUnsetField guards the config seam: a partially
// filled `console:` block must inherit the rest of the envelope rather than
// silently running with zeros (a zero queue or rate would wedge every session).
func TestLimits_DefaultsFillEveryUnsetField(t *testing.T) {
	got := Limits{MaxSessions: 2}.withDefaults()

	if got.MaxSessions != 2 {
		t.Errorf("MaxSessions = %d, want the configured 2", got.MaxSessions)
	}
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"ReadBufferBytes", got.ReadBufferBytes, DefaultReadBufferBytes},
		{"QueueChunks", got.QueueChunks, DefaultQueueChunks},
		{"RateBytesPerSec", got.RateBytesPerSec, DefaultRateBytesPerSec},
		{"BurstBytes", got.BurstBytes, DefaultBurstBytes},
		{"Cols", int(got.Cols), DefaultCols},
		{"Rows", int(got.Rows), DefaultRows},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want the default %d", tc.name, tc.got, tc.want)
		}
	}
	if got.KillGrace != DefaultKillGrace {
		t.Errorf("KillGrace = %s, want the default %s", got.KillGrace, DefaultKillGrace)
	}
	if got.Disabled {
		t.Error("Disabled must stay false by default — a console is part of the product, not an opt-in")
	}
}
