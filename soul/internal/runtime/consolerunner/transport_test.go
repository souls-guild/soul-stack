package consolerunner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Guards for the dedicated console transport (NIM-188). The property under
// test is the one the ticket exists for: console traffic must not touch the
// EventStream Sink, because that Sink is a shared write mutex.

// fakeConsoleStream stands in for the bidi RPC. `down` feeds Keeper -> Soul
// frames; sends are recorded, and `sendBlock` simulates a Keeper that stopped
// reading.
type fakeConsoleStream struct {
	down chan *keeperv1.ConsoleToSoul

	mu        sync.Mutex
	sent      []*keeperv1.ConsoleFromSoul
	sendErr   error
	sendBlock chan struct{}
	sendDelay time.Duration

	closedSend bool
	recvErr    error
}

func newFakeConsoleStream() *fakeConsoleStream {
	return &fakeConsoleStream{down: make(chan *keeperv1.ConsoleToSoul, 16)}
}

func (f *fakeConsoleStream) Send(msg *keeperv1.ConsoleFromSoul) error {
	f.mu.Lock()
	block, err, delay := f.sendBlock, f.sendErr, f.sendDelay
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	f.mu.Unlock()
	return nil
}

func (f *fakeConsoleStream) Recv() (*keeperv1.ConsoleToSoul, error) {
	msg, ok := <-f.down
	if !ok {
		f.mu.Lock()
		err := f.recvErr
		f.mu.Unlock()
		if err == nil {
			err = errors.New("console stream closed")
		}
		return nil, err
	}
	return msg, nil
}

func (f *fakeConsoleStream) CloseSend() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedSend = true
	return nil
}

// ack releases the transport as usable, the way a real Keeper does.
func (f *fakeConsoleStream) ack(sessionID string) {
	f.down <- &keeperv1.ConsoleToSoul{
		Payload: &keeperv1.ConsoleToSoul_ConsoleAttached{
			ConsoleAttached: &keeperv1.ConsoleAttached{SessionId: sessionID},
		},
	}
}

func (f *fakeConsoleStream) snapshot() []*keeperv1.ConsoleFromSoul {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*keeperv1.ConsoleFromSoul(nil), f.sent...)
}

func (f *fakeConsoleStream) chunks(sessionID string) string {
	var b strings.Builder
	for _, m := range f.snapshot() {
		if c := m.GetConsoleChunk(); c != nil && c.GetSessionId() == sessionID {
			b.Write(c.GetData())
		}
	}
	return b.String()
}

// fakeDialer hands out one prepared stream, and acks it as soon as it is
// dialed unless the test says otherwise.
type fakeDialer struct {
	stream  *fakeConsoleStream
	err     error
	autoAck bool

	mu    sync.Mutex
	dials int
}

func (d *fakeDialer) ConsoleStream(_ context.Context) (ConsoleStreamClient, error) {
	d.mu.Lock()
	d.dials++
	d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	if d.autoAck {
		d.stream.ack("")
	}
	return d.stream, nil
}

func (d *fakeDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

// strictSink fails the test if a console payload ever reaches the EventStream.
// It is the assertion the whole ticket reduces to.
type strictSink struct {
	t *testing.T
	recordingSink
}

func (s *strictSink) SendFromSoul(msg *keeperv1.FromSoul) error {
	switch msg.GetPayload().(type) {
	case *keeperv1.FromSoul_ConsoleOpened, *keeperv1.FromSoul_ConsoleChunk, *keeperv1.FromSoul_ConsoleExit:
		s.t.Errorf("console payload %T reached the EventStream sink", msg.GetPayload())
	}
	return s.recordingSink.SendFromSoul(msg)
}

func openOnDedicatedStream(t *testing.T, sink Sink, limits Limits) (*Runner, *fakeConsoleStream, string) {
	t.Helper()
	shell := requireShell(t)
	stream := newFakeConsoleStream()
	r := New(sink, limits, testLogger(), nil, WithDialer(&fakeDialer{stream: stream}))
	id := "session-1"

	// The queue is buffered, so the ack can wait there for the reader goroutine
	// the runner starts once the session is registered.
	stream.ack(id)
	r.Open(&keeperv1.ConsoleOpen{SessionId: id, Shell: shell, Cols: 80, Rows: 24})
	return r, stream, id
}

// The core claim: with a dedicated stream, no console byte goes near the
// EventStream's write mutex.
func TestTransport_ConsoleTrafficNeverTouchesTheEventStream(t *testing.T) {
	sink := &strictSink{t: t}
	r, stream, id := openOnDedicatedStream(t, sink, Limits{})
	defer r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN)

	waitFor(t, 10*time.Second, "ConsoleOpened on the dedicated stream", func() bool {
		for _, m := range stream.snapshot() {
			if m.GetConsoleOpened().GetSessionId() == id {
				return true
			}
		}
		return false
	})

	r.Stdin(&keeperv1.ConsoleStdin{SessionId: id, Data: []byte("echo MAR\"\"KER\n")})
	waitFor(t, 10*time.Second, "output on the dedicated stream", func() bool {
		return strings.Contains(stream.chunks(id), "MARKER")
	})

	if got := len(sink.snapshot()); got != 0 {
		t.Fatalf("EventStream sink saw %d messages, want none", got)
	}
}

// Keeper -> Soul frames arrive on the same stream and drive the pty.
func TestTransport_DownstreamFramesDriveThePty(t *testing.T) {
	sink := &strictSink{t: t}
	r, stream, id := openOnDedicatedStream(t, sink, Limits{})
	defer r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN)

	waitFor(t, 10*time.Second, "the session to open", func() bool { return r.ActiveCount() == 1 })

	stream.down <- &keeperv1.ConsoleToSoul{
		Payload: &keeperv1.ConsoleToSoul_ConsoleStdin{
			ConsoleStdin: &keeperv1.ConsoleStdin{SessionId: id, Data: []byte("echo DOWN\"\"STREAM\n")},
		},
	}
	waitFor(t, 10*time.Second, "the pty to answer a downstream keystroke", func() bool {
		return strings.Contains(stream.chunks(id), "DOWNSTREAM")
	})
}

// A Keeper that predates the RPC must not cost the operator their console. The
// dial itself fails there, and the session rides the EventStream exactly as a
// pre-NIM-188 Soul did.
func TestTransport_FallsBackWhenTheKeeperHasNoConsoleRPC(t *testing.T) {
	shell := requireShell(t)
	sink := &recordingSink{}
	dialer := &fakeDialer{err: status.Error(codes.Unimplemented, "unknown method ConsoleStream")}
	r := New(sink, Limits{}, testLogger(), nil, WithDialer(dialer))
	defer r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN)

	r.Open(&keeperv1.ConsoleOpen{SessionId: "session-1", Shell: shell})

	waitFor(t, 10*time.Second, "ConsoleOpened on the EventStream", func() bool {
		return sink.opened("session-1") != nil
	})
	if dialer.dialCount() != 1 {
		t.Fatalf("dials = %d, want exactly one attempt", dialer.dialCount())
	}
	waitShellReady(t, r, sink, "session-1")
}

// A Keeper that accepts the stream and then never acks is indistinguishable
// from a hung one. The session must not wait on it forever.
func TestTransport_FallsBackWhenTheAckNeverArrives(t *testing.T) {
	stream := newFakeConsoleStream()
	tr := &sessionTransport{
		id:       "session-1",
		stream:   stream,
		cancel:   func() {},
		fallback: &recordingSink{},
		logger:   testLogger(),
		ready:    make(chan struct{}),
	}
	go tr.watchdog()

	start := time.Now()
	if err := tr.SendFromSoul(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ConsoleOpened{
			ConsoleOpened: &keeperv1.ConsoleOpened{SessionId: "session-1"},
		},
	}); err != nil {
		t.Fatalf("SendFromSoul: %v", err)
	}
	if elapsed := time.Since(start); elapsed < attachAckTimeout {
		t.Fatalf("send returned after %s, want a wait for the ack budget", elapsed)
	}
	if tr.alive {
		t.Fatal("transport settled as usable without an ack")
	}
	if got := len(tr.fallback.(*recordingSink).snapshot()); got != 1 {
		t.Fatalf("EventStream carrier saw %d frames, want the one that fell back", got)
	}
	if got := len(stream.snapshot()); got != 0 {
		t.Fatalf("dedicated stream saw %d frames after the fallback", got)
	}
}

// The pty cannot outlive the stream that carries it — the same invariant the
// EventStream carrier has always had, now per session.
func TestTransport_LostStreamKillsTheSession(t *testing.T) {
	sink := &strictSink{t: t}
	r, stream, _ := openOnDedicatedStream(t, sink, Limits{})
	defer r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN)

	waitFor(t, 10*time.Second, "the session to open", func() bool { return r.ActiveCount() == 1 })

	close(stream.down) // the transport dies with no terminal frame

	waitFor(t, 10*time.Second, "the session to be reaped", func() bool { return r.ActiveCount() == 0 })
}

// Losslessness is what the private stream buys: with nowhere to spill into a
// shared mutex, output is throttled at the pty instead of discarded.
func TestTransport_DedicatedStreamDoesNotDropOutput(t *testing.T) {
	flood := requireProgram(t, floodProgram)
	sink := &strictSink{t: t}
	stream := newFakeConsoleStream()
	// A slow reader, a queue of one and a tiny read buffer: the exact shape
	// that makes the shared carrier drop constantly (see the test below).
	stream.sendDelay = 5 * time.Millisecond
	r := New(sink, Limits{QueueChunks: 1, ReadBufferBytes: 64}, testLogger(), nil,
		WithDialer(&fakeDialer{stream: stream}))
	stream.ack("session-1")
	r.Open(&keeperv1.ConsoleOpen{SessionId: "session-1", Shell: flood})

	waitFor(t, 15*time.Second, "the flood to back up against a slow reader", func() bool {
		return len(stream.chunks("session-1")) > 512
	})
	r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_CLOSED_BY_KEEPER)

	for _, m := range stream.snapshot() {
		if c := m.GetConsoleChunk(); c != nil && c.GetDroppedBytes() != 0 {
			t.Fatalf("dedicated stream reported %d dropped bytes — it must throttle, not discard",
				c.GetDroppedBytes())
		}
	}
}

// The shared carrier keeps its old behaviour: it drops rather than block,
// because blocking there would hold up apply traffic.
func TestTransport_SharedCarrierStillDrops(t *testing.T) {
	flood := requireProgram(t, floodProgram)
	sink := &recordingSink{delay: 20 * time.Millisecond}
	r := New(sink, Limits{QueueChunks: 1, ReadBufferBytes: 64}, testLogger(), nil)
	r.Open(&keeperv1.ConsoleOpen{SessionId: "session-1", Shell: flood})

	waitFor(t, 15*time.Second, "flow control to drop on the shared carrier", func() bool {
		return sink.droppedTotal("session-1") > 0
	})
	r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_CLOSED_BY_KEEPER)
}

// Teardown must never be held hostage by a Keeper that stopped reading: the
// only thing that unblocks a gRPC Send is cancelling the RPC.
func TestTransport_CutUnblocksASenderWedgedOnSend(t *testing.T) {
	stream := newFakeConsoleStream()
	stream.sendBlock = make(chan struct{})
	cancelled := make(chan struct{})
	tr := &sessionTransport{
		id:       "session-1",
		stream:   stream,
		cancel:   func() { close(cancelled) },
		fallback: &recordingSink{},
		logger:   testLogger(),
		ready:    make(chan struct{}),
	}
	tr.settle(true, "")

	sent := make(chan error, 1)
	go func() {
		sent <- tr.SendFromSoul(&keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_ConsoleChunk{
				ConsoleChunk: &keeperv1.ConsoleChunk{SessionId: "session-1", Data: []byte("x")},
			},
		})
	}()

	select {
	case <-sent:
		t.Fatal("Send returned while the stream was blocked")
	case <-time.After(50 * time.Millisecond):
	}

	tr.cut()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cut did not cancel the RPC")
	}
	close(stream.sendBlock)
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("sender never returned after the cut")
	}
}
