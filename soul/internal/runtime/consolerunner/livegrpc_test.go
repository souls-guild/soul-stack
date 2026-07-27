package consolerunner

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// The acceptance test for the split: a REAL pty, driven over a REAL gRPC bidi
// stream, against a Keeper-shaped server. Everything between the shell and the
// wire is production code — the fakes elsewhere in this package stub the stream
// out, and a transport ticket that only ever ran against a stub would be
// testing its own mock.
//
// The server here is a stand-in for Keeper, not a copy of it: the Keeper-side
// handler has its own end-to-end guards over gRPC in keeper/internal/grpc. The
// two halves cannot meet in one test — a Soul module may not import keeper
// (ADR-011) — so each is exercised against the other's shape.

// stubKeeper implements the ConsoleStream half of the service the way Keeper
// does: ack the attach, forward downstream, collect upstream.
type stubKeeper struct {
	keeperv1.UnimplementedKeeperServer

	// down is handed to the handler so the test can push keystrokes.
	down chan *keeperv1.ConsoleToSoul

	mu       sync.Mutex
	attached string
	up       []*keeperv1.ConsoleFromSoul
}

func (s *stubKeeper) ConsoleStream(stream grpclib.BidiStreamingServer[keeperv1.ConsoleFromSoul, keeperv1.ConsoleToSoul]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	attach := first.GetConsoleAttach()
	if attach == nil {
		return waitForClose(stream)
	}
	s.mu.Lock()
	s.attached = attach.GetSessionId()
	s.mu.Unlock()

	if err := stream.Send(&keeperv1.ConsoleToSoul{
		Payload: &keeperv1.ConsoleToSoul_ConsoleAttached{
			ConsoleAttached: &keeperv1.ConsoleAttached{SessionId: attach.GetSessionId()},
		},
	}); err != nil {
		return err
	}

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case msg := <-s.down:
				if err := stream.Send(msg); err != nil {
					return
				}
			case <-stream.Context().Done():
				return
			}
		}
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			<-writerDone
			return nil
		}
		s.mu.Lock()
		s.up = append(s.up, msg)
		s.mu.Unlock()
	}
}

// waitForClose ends a stream that opened with the wrong first frame.
func waitForClose(stream grpclib.BidiStreamingServer[keeperv1.ConsoleFromSoul, keeperv1.ConsoleToSoul]) error {
	<-stream.Context().Done()
	return stream.Context().Err()
}

func (s *stubKeeper) output(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, m := range s.up {
		if c := m.GetConsoleChunk(); c != nil && c.GetSessionId() == sessionID {
			b.Write(c.GetData())
		}
	}
	return b.String()
}

func (s *stubKeeper) exit(sessionID string) *keeperv1.ConsoleExit {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.up {
		if e := m.GetConsoleExit(); e != nil && e.GetSessionId() == sessionID {
			return e
		}
	}
	return nil
}

func (s *stubKeeper) sawOpened(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.up {
		if m.GetConsoleOpened().GetSessionId() == sessionID {
			return true
		}
	}
	return false
}

// noDrops reports whether every chunk arrived without a flow-control gap.
func (s *stubKeeper) noDrops(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.up {
		if c := m.GetConsoleChunk(); c != nil && c.GetDroppedBytes() != 0 {
			t.Fatalf("a live flood reported %d dropped bytes", c.GetDroppedBytes())
		}
	}
}

func (s *stubKeeper) attachedID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attached
}

// liveDialer opens the real generated client against the stub server.
type liveDialer struct{ client keeperv1.KeeperClient }

func (d liveDialer) ConsoleStream(ctx context.Context) (ConsoleStreamClient, error) {
	return d.client.ConsoleStream(ctx)
}

func startStubKeeper(t *testing.T) (*stubKeeper, Dialer) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stub := &stubKeeper{down: make(chan *keeperv1.ConsoleToSoul, 16)}
	srv := grpclib.NewServer()
	keeperv1.RegisterKeeperServer(srv, stub)
	go func() { _ = srv.Serve(ln) }()

	conn, err := grpclib.NewClient(ln.Addr().String(),
		grpclib.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
	})
	return stub, liveDialer{keeperv1.NewKeeperClient(conn)}
}

// A round trip through a real shell: keystrokes down the RPC reach the pty, and
// its output comes back up the same stream.
func TestLiveGRPC_ConsoleRoundTripOverItsOwnStream(t *testing.T) {
	shell := requireShell(t)
	stub, dialer := startStubKeeper(t)

	// The sink fails the test if anything console-shaped reaches the
	// EventStream: that is the property the ticket bought.
	sink := &strictSink{t: t}
	r := New(sink, Limits{}, testLogger(), nil, WithDialer(dialer))
	defer r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN)

	const id = "01LIVEGRPC0000000000000000"
	r.Open(&keeperv1.ConsoleOpen{SessionId: id, Shell: shell, Cols: 80, Rows: 24})

	waitFor(t, 10*time.Second, "the attach to reach the server", func() bool {
		return stub.attachedID() == id
	})
	waitFor(t, 10*time.Second, "ConsoleOpened", func() bool { return stub.sawOpened(id) })

	// Retry the probe: an interactive shell may still be sourcing rc files, and
	// keystrokes typed into that window are swallowed.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stub.output(id), "LIVE") {
		stub.down <- &keeperv1.ConsoleToSoul{
			Payload: &keeperv1.ConsoleToSoul_ConsoleStdin{
				ConsoleStdin: &keeperv1.ConsoleStdin{SessionId: id, Data: []byte("echo LI\"\"VE\n")},
			},
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(stub.output(id), "LIVE") {
		t.Fatalf("the shell never answered over the dedicated stream; output:\n%q", stub.output(id))
	}

	// A close travels the same stream and produces exactly one terminal.
	stub.down <- &keeperv1.ConsoleToSoul{
		Payload: &keeperv1.ConsoleToSoul_ConsoleClose{
			ConsoleClose: &keeperv1.ConsoleClose{SessionId: id, Reason: "test done"},
		},
	}
	waitFor(t, 10*time.Second, "ConsoleExit", func() bool { return stub.exit(id) != nil })
	if got := stub.exit(id).GetReason(); got != keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_CLOSED_BY_KEEPER {
		t.Fatalf("exit reason = %v, want CLOSED_BY_KEEPER", got)
	}
	waitFor(t, 10*time.Second, "the session to be reaped", func() bool { return r.ActiveCount() == 0 })

	if n := len(sink.snapshot()); n != 0 {
		t.Fatalf("EventStream sink saw %d messages over a live session", n)
	}
}

// A real flood over a real stream must arrive intact: with its own flow-control
// window there is nothing to protect by discarding.
func TestLiveGRPC_FloodArrivesWithoutDrops(t *testing.T) {
	flood := requireProgram(t, floodProgram)
	stub, dialer := startStubKeeper(t)

	sink := &strictSink{t: t}
	r := New(sink, Limits{QueueChunks: 1, ReadBufferBytes: 64}, testLogger(), nil, WithDialer(dialer))

	const id = "01LIVEFLOOD000000000000000"
	r.Open(&keeperv1.ConsoleOpen{SessionId: id, Shell: flood})

	waitFor(t, 15*time.Second, "the flood to reach the server", func() bool {
		return len(stub.output(id)) > 64*1024
	})
	r.CloseAll(keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_CLOSED_BY_KEEPER)

	stub.noDrops(t)
}
