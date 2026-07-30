package consolerunner

import (
	"errors"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// errStreamBroken stands in for a dead EventStream in the tests.
var errStreamBroken = errors.New("stream broken")

// TestFlowControl_FloodIsDroppedNotBuffered is the backpressure guard.
//
// The console and the apply cycle share ONE write mutex on the EventStream. If a
// flooding console could block its sender, it would hold that mutex and stall
// apply's TaskEvent/RunResult on the same stream. The design answer is to drop
// rather than buffer, so this asserts the observable consequence: the session
// still reaches its terminal against a deliberately slow stream, and the dropped
// bytes are reported instead of silently vanishing.
func TestFlowControl_FloodIsDroppedNotBuffered(t *testing.T) {
	// `yes` under the pty instead of a shell: the runaway is the point here, and
	// an interactive shell would make the traffic depend on the developer's
	// prompt theme rather than on the code under test.
	requireProgram(t, floodProgram)
	// A slow sink plus a shallow queue is a congested stream in miniature.
	sink := &recordingSink{delay: 20 * time.Millisecond}
	r := New(sink, Limits{
		Shell:           floodProgram,
		QueueChunks:     2,
		ReadBufferBytes: 4096,
		KillGrace:       300 * time.Millisecond,
	}, testLogger(t), nil)

	r.Open(&keeperv1.ConsoleOpen{SessionId: "flood"})
	waitFor(t, 5*time.Second, "ConsoleOpened", func() bool { return sink.opened("flood") != nil })

	deadline := time.Now().Add(10 * time.Second)
	for sink.droppedTotal("flood") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no flow-control drops in 10s: %d chunks, %d bytes of output, exit=%v",
				chunkCount(sink, "flood"), len(sink.output("flood")), sink.exit("flood"))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The decisive part: despite the flood and the congested stream, teardown
	// still completes promptly. A blocked reader would hang here.
	done := make(chan struct{})
	go func() {
		r.Close(&keeperv1.ConsoleClose{SessionId: "flood"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a flooding session — the reader is coupled to the stream")
	}

	waitFor(t, 10*time.Second, "ConsoleExit", func() bool { return sink.exit("flood") != nil })
	waitFor(t, 5*time.Second, "session release", func() bool { return r.ActiveCount() == 0 })
}

// TestFlowControl_ChunkSeqIsMonotonic pins the wire contract B2/B5 rely on to
// render an explicit gap: seq counts chunks from 1 without holes, so any loss is
// visible only through dropped_bytes and never as a silent splice.
func TestFlowControl_ChunkSeqIsMonotonic(t *testing.T) {
	requireShell(t)
	sink := &recordingSink{}
	r := New(sink, Limits{KillGrace: 300 * time.Millisecond}, testLogger(t), nil)
	t.Cleanup(func() { r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN) })

	r.Open(&keeperv1.ConsoleOpen{SessionId: "s1"})
	waitFor(t, 5*time.Second, "ConsoleOpened", func() bool { return sink.opened("s1") != nil })
	waitShellReady(t, r, sink, "s1")
	r.Stdin(&keeperv1.ConsoleStdin{SessionId: "s1", Data: []byte("echo one; echo two; echo three\n")})
	waitFor(t, 5*time.Second, "several chunks", func() bool { return chunkCount(sink, "s1") >= 2 })

	var want uint64
	for _, m := range sink.snapshot() {
		c := m.GetConsoleChunk()
		if c == nil || c.GetSessionId() != "s1" {
			continue
		}
		want++
		if c.GetSeq() != want {
			t.Fatalf("chunk seq = %d, want %d (seq must be gap-free)", c.GetSeq(), want)
		}
		if c.GetStream() != keeperv1.ConsoleStream_CONSOLE_STREAM_STDOUT {
			t.Errorf("stream = %v, want STDOUT (a pty merges stdout and stderr)", c.GetStream())
		}
	}
}

// TestBucket_PacesToRate checks the token bucket in isolation with a fake clock:
// the burst goes out free, and everything past it costs exactly rate-proportional
// time.
func TestBucket_PacesToRate(t *testing.T) {
	now := time.Unix(0, 0)
	var slept time.Duration
	b := newTokenBucket(1000, 100)
	b.now = func() time.Time { return now }
	b.sleep = func(d time.Duration, _ <-chan struct{}) {
		slept += d
		now = now.Add(d)
	}

	never := make(chan struct{})
	b.take(100, never) // exactly the burst — free
	if slept != 0 {
		t.Fatalf("slept %s spending the burst, want 0", slept)
	}

	b.take(500, never) // 500 bytes at 1000 B/s = 500ms
	if slept < 450*time.Millisecond || slept > 550*time.Millisecond {
		t.Errorf("slept %s for 500 bytes at 1000 B/s, want ~500ms", slept)
	}
}

// TestBucket_AbortCutsPacingShort guards the teardown path: `rate_limit_kbps` is
// operator-configurable, so a throttled session can owe seconds of sleep. If the
// abort signal did not release it, CloseAll would sit out its whole budget and
// report a leak for a pty that is already dead.
func TestBucket_AbortCutsPacingShort(t *testing.T) {
	// A real (not faked) sleep, so this exercises interruptibleSleep itself.
	b := newTokenBucket(1, 1) // 1 B/s: 1000 bytes would be ~16 minutes
	abort := make(chan struct{})
	close(abort)

	done := make(chan struct{})
	go func() {
		b.take(1000, abort)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("take() ignored the abort signal — teardown would hang on pacing")
	}
}

// TestBucket_OversizePayloadStillPasses guards against a deadlock: a chunk larger
// than the burst must be paid for and sent, not refused forever.
func TestBucket_OversizePayloadStillPasses(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTokenBucket(1000, 100)
	b.now = func() time.Time { return now }
	b.sleep = func(d time.Duration, _ <-chan struct{}) { now = now.Add(d) }

	done := make(chan struct{})
	go func() {
		b.take(10_000, make(chan struct{}))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("take() never returned for a payload larger than the burst")
	}
}

func chunkCount(sink *recordingSink, sessionID string) int {
	n := 0
	for _, m := range sink.snapshot() {
		if c := m.GetConsoleChunk(); c != nil && c.GetSessionId() == sessionID {
			n++
		}
	}
	return n
}
