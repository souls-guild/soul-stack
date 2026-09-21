package grpc

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// End-to-end guards for the ConsoleStream RPC over a real gRPC connection. The
// SID normally comes from the mTLS peer cert via [streamSeedAuthInterceptor];
// here it is injected, so the tests exercise the handler rather than the PKI.

type recordingHub struct {
	mu   sync.Mutex
	sids []string
	msgs []*keeperv1.FromSoul
}

func (r *recordingHub) Deliver(_ context.Context, sid string, msg *keeperv1.FromSoul) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sids = append(r.sids, sid)
	r.msgs = append(r.msgs, msg)
}

func (r *recordingHub) snapshot() ([]string, []*keeperv1.FromSoul) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sids...), append([]*keeperv1.FromSoul(nil), r.msgs...)
}

// waitFor polls until cond holds or the budget runs out. The handler forwards
// from its own goroutine, so the assertion needs a settling point.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startConsoleRPC(t *testing.T, sid string, deps EventStreamDeps) keeperv1.KeeperClient {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpclib.NewServer(grpclib.StreamInterceptor(
		func(s any, ss grpclib.ServerStream, _ *grpclib.StreamServerInfo, handler grpclib.StreamHandler) error {
			return handler(s, &authStream{ServerStream: ss, ctx: withAuthenticatedSID(ss.Context(), sid)})
		},
	))
	keeperv1.RegisterKeeperServer(srv, newEventStreamHandler(deps, discardLogger(t)))
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
	return keeperv1.NewKeeperClient(conn)
}

func attachRPC(t *testing.T, client keeperv1.KeeperClient, sessionID string) grpclib.BidiStreamingClient[keeperv1.ConsoleFromSoul, keeperv1.ConsoleToSoul] {
	t.Helper()
	stream, err := client.ConsoleStream(context.Background())
	if err != nil {
		t.Fatalf("ConsoleStream: %v", err)
	}
	if err := stream.Send(&keeperv1.ConsoleFromSoul{
		Payload: &keeperv1.ConsoleFromSoul_ConsoleAttach{
			ConsoleAttach: &keeperv1.ConsoleAttach{SessionId: sessionID},
		},
	}); err != nil {
		t.Fatalf("send attach: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	if ack.GetConsoleAttached().GetSessionId() != sessionID {
		t.Fatalf("ack = %+v, want ConsoleAttached for %s", ack.GetPayload(), sessionID)
	}
	return stream
}

// The ack is the Soul's proof that this Keeper implements the RPC — without it
// a Soul could not tell "not supported" from "not answered yet", and would
// have to guess which carrier to use.
func TestConsoleStreamRPC_AttachIsAcknowledgedAndRegistered(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	client := startConsoleRPC(t, "host-a", EventStreamDeps{Manager: m, ConsoleHub: &recordingHub{}})

	stream := attachRPC(t, client, "session-1")
	defer func() { _ = stream.CloseSend() }()

	waitFor(t, "the stream to register", func() bool { return m.Consoles().Count() == 1 })
}

// The first frame names the session. Anything else and the stream has no route.
func TestConsoleStreamRPC_RefusesAFirstFrameThatIsNotAttach(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	client := startConsoleRPC(t, "host-a", EventStreamDeps{Manager: m, ConsoleHub: &recordingHub{}})

	stream, err := client.ConsoleStream(context.Background())
	if err != nil {
		t.Fatalf("ConsoleStream: %v", err)
	}
	if err := stream.Send(&keeperv1.ConsoleFromSoul{
		Payload: &keeperv1.ConsoleFromSoul_ConsoleChunk{
			ConsoleChunk: &keeperv1.ConsoleChunk{SessionId: "session-1", Data: []byte("hi")},
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	if m.Consoles().Count() != 0 {
		t.Fatalf("a stream registered without attaching: %d", m.Consoles().Count())
	}
}

// Both directions on one stream: pty output up, keystrokes down.
func TestConsoleStreamRPC_CarriesBothDirections(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	hub := &recordingHub{}
	client := startConsoleRPC(t, "host-a", EventStreamDeps{Manager: m, ConsoleHub: hub})
	_ = m.Register("host-a")
	ob := newOutboundForTest(t, m, nopAudit{})

	stream := attachRPC(t, client, "session-1")
	defer func() { _ = stream.CloseSend() }()

	if err := stream.Send(&keeperv1.ConsoleFromSoul{
		Payload: &keeperv1.ConsoleFromSoul_ConsoleChunk{
			ConsoleChunk: &keeperv1.ConsoleChunk{SessionId: "session-1", Data: []byte("out")},
		},
	}); err != nil {
		t.Fatalf("send chunk: %v", err)
	}
	waitFor(t, "the chunk to reach the hub", func() bool {
		_, msgs := hub.snapshot()
		return len(msgs) == 1 && string(msgs[0].GetConsoleChunk().GetData()) == "out"
	})
	sids, _ := hub.snapshot()
	if sids[0] != "host-a" {
		t.Fatalf("hub saw sid %q, want the authenticated host-a", sids[0])
	}

	waitFor(t, "the stream to register", func() bool { return m.Consoles().Count() == 1 })
	if err := ob.SendConsoleStdin(context.Background(), "host-a",
		&keeperv1.ConsoleStdin{SessionId: "session-1", Data: []byte("ls\n")}); err != nil {
		t.Fatalf("SendConsoleStdin: %v", err)
	}
	got, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv stdin: %v", err)
	}
	if string(got.GetConsoleStdin().GetData()) != "ls\n" {
		t.Fatalf("downstream = %+v, want the keystrokes", got.GetPayload())
	}
}

// One stream carries one session. A frame naming another one is dropped rather
// than forwarded: it is the invariant that makes the per-stream SID check
// sufficient.
func TestConsoleStreamRPC_DropsFramesForAnotherSession(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	hub := &recordingHub{}
	client := startConsoleRPC(t, "host-a", EventStreamDeps{Manager: m, ConsoleHub: hub})

	stream := attachRPC(t, client, "session-1")
	if err := stream.Send(&keeperv1.ConsoleFromSoul{
		Payload: &keeperv1.ConsoleFromSoul_ConsoleChunk{
			ConsoleChunk: &keeperv1.ConsoleChunk{SessionId: "session-other", Data: []byte("nope")},
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := stream.Send(&keeperv1.ConsoleFromSoul{
		Payload: &keeperv1.ConsoleFromSoul_ConsoleChunk{
			ConsoleChunk: &keeperv1.ConsoleChunk{SessionId: "session-1", Data: []byte("mine")},
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	waitFor(t, "the legitimate chunk", func() bool {
		_, msgs := hub.snapshot()
		return len(msgs) >= 1
	})
	_, msgs := hub.snapshot()
	for _, msg := range msgs {
		if id := msg.GetConsoleChunk().GetSessionId(); id != "session-1" {
			t.Fatalf("hub saw a frame for %q", id)
		}
	}
}

// A transport that dies without a terminal leaves the operator watching a pane
// that is already dead. The pty cannot outlive its stream, so the exit is real
// — it just has nobody left to send it.
func TestConsoleStreamRPC_SynthesizesExitWhenTheStreamDiesSilently(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	hub := &recordingHub{}
	client := startConsoleRPC(t, "host-a", EventStreamDeps{Manager: m, ConsoleHub: hub})

	stream := attachRPC(t, client, "session-1")
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	waitFor(t, "the synthesized exit", func() bool {
		_, msgs := hub.snapshot()
		return len(msgs) == 1 && msgs[0].GetConsoleExit() != nil
	})
	_, msgs := hub.snapshot()
	exit := msgs[0].GetConsoleExit()
	if exit.GetSessionId() != "session-1" ||
		exit.GetReason() != keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN {
		t.Fatalf("exit = %+v, want a SOUL_SHUTDOWN terminal for session-1", exit)
	}
	waitFor(t, "the stream to detach", func() bool { return m.Consoles().Count() == 0 })
}

// A Soul that answers its own ConsoleExit must not then get a second, invented
// one: two terminals would show the operator an exit code no shell produced.
func TestConsoleStreamRPC_NoSyntheticExitAfterARealOne(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	hub := &recordingHub{}
	client := startConsoleRPC(t, "host-a", EventStreamDeps{Manager: m, ConsoleHub: hub})

	stream := attachRPC(t, client, "session-1")
	if err := stream.Send(&keeperv1.ConsoleFromSoul{
		Payload: &keeperv1.ConsoleFromSoul_ConsoleExit{
			ConsoleExit: &keeperv1.ConsoleExit{
				SessionId: "session-1",
				ExitCode:  0,
				Reason:    keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED,
			},
		},
	}); err != nil {
		t.Fatalf("send exit: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	waitFor(t, "the stream to detach", func() bool { return m.Consoles().Count() == 0 })
	_, msgs := hub.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("hub saw %d terminals, want exactly the Soul's own", len(msgs))
	}
	if msgs[0].GetConsoleExit().GetReason() != keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED {
		t.Fatalf("reason = %v, want the Soul's PROCESS_EXITED", msgs[0].GetConsoleExit().GetReason())
	}
}
