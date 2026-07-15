package grpc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

func TestOrderedEndpoints_PrioritySorted(t *testing.T) {
	t.Parallel()
	in := []Endpoint{
		{Addr: "c", Priority: 3},
		{Addr: "a", Priority: 1},
		{Addr: "b", Priority: 2},
	}
	got := orderedEndpoints(in)
	if got[0].Addr != "a" || got[1].Addr != "b" || got[2].Addr != "c" {
		t.Errorf("orderedEndpoints = %+v, want a,b,c", got)
	}
}

func TestOrderedEndpoints_ZeroPriorityTreatedAsOne(t *testing.T) {
	t.Parallel()
	in := []Endpoint{
		{Addr: "p2", Priority: 2},
		{Addr: "p0a"}, // 0 → normalized 1
		{Addr: "p1", Priority: 1},
		{Addr: "p0b"},
	}
	got := orderedEndpoints(in)
	if normalizedPriority(got[0].Priority) != 1 || normalizedPriority(got[3].Priority) != 2 {
		t.Errorf("orderedEndpoints = %+v", got)
	}
}

func TestOrderedEndpoints_PreservesInput(t *testing.T) {
	t.Parallel()
	in := []Endpoint{
		{Addr: "a", Priority: 2},
		{Addr: "b", Priority: 1},
	}
	cp := []Endpoint{in[0], in[1]}
	_ = orderedEndpoints(in)
	if in[0] != cp[0] || in[1] != cp[1] {
		t.Errorf("input mutated: %+v", in)
	}
}

func TestNewClient_RejectsEmptyEndpoints(t *testing.T) {
	t.Parallel()
	_, err := NewClient(ClientConfig{}, nil)
	if err == nil || !contains(err.Error(), "endpoints is empty") {
		t.Fatalf("NewClient: %v", err)
	}
}

func TestNewClient_RejectsIncompleteSeed(t *testing.T) {
	t.Parallel()
	_, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{{Addr: "x"}},
		SID:       "host",
	}, nil)
	if err == nil || !contains(err.Error(), "SoulSeed") {
		t.Fatalf("NewClient: %v", err)
	}
}

func TestNewClient_MaxRecvMsgSizeDefault(t *testing.T) {
	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{{Addr: "k:9443"}},
		SeedCert:  "/c", SeedKey: "/k", CAPath: "/a",
		SID: "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	want := config.DefaultMaxApplySizeMB * 1024 * 1024
	if cli.cfg.MaxRecvMsgSize != want {
		t.Fatalf("default MaxRecvMsgSize: want %d (8 MiB), got %d", want, cli.cfg.MaxRecvMsgSize)
	}
}

func TestNewClient_MaxRecvMsgSizeFromConfig(t *testing.T) {
	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{{Addr: "k:9443"}},
		SeedCert:  "/c", SeedKey: "/k", CAPath: "/a",
		SID:            "host.example",
		MaxRecvMsgSize: 32 * 1024 * 1024,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if cli.cfg.MaxRecvMsgSize != 32*1024*1024 {
		t.Fatalf("MaxRecvMsgSize from config: want %d, got %d", 32*1024*1024, cli.cfg.MaxRecvMsgSize)
	}
}

// --- end-to-end via mock EventStream server ---

func TestClientDial_HandshakeSuccess(t *testing.T) {
	srv := newMockEventStream(t, nil)
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:        []Endpoint{{Addr: srv.addr}},
		SeedCert:         srv.clientCert,
		SeedKey:          srv.clientKey,
		CAPath:           srv.caPath,
		HandshakeTimeout: 5 * time.Second,
		SID:              "host.example",
		SoulVersion:      "0.0.0-test",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	sess, err := cli.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()

	if sess.KID() != "test-kid" {
		t.Errorf("KID = %q", sess.KID())
	}
	if sess.SessionID() == "" {
		t.Error("SessionID is empty")
	}

	// SID_echo reaches the server.
	srv.mu.Lock()
	hello := srv.received
	srv.mu.Unlock()
	if hello == nil || hello.GetSidEcho() != "host.example" {
		t.Errorf("server received hello = %+v", hello)
	}
}

func TestClientDial_FallsBackToNextEndpoint(t *testing.T) {
	srv := newMockEventStream(t, nil)
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{
			{Addr: "127.0.0.1:1"}, // dead port
			{Addr: srv.addr, Priority: 2},
		},
		SeedCert:         srv.clientCert,
		SeedKey:          srv.clientKey,
		CAPath:           srv.caPath,
		HandshakeTimeout: 3 * time.Second,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()
}

func TestDialPriority_SkipsCurrentOrLower(t *testing.T) {
	srv := newMockEventStream(t, nil)
	defer srv.stop()

	// One endpoint at priority=2; requesting maxPriority=2 should return the
	// "no higher-priority" sentinel.
	cli, err := NewClient(ClientConfig{
		Endpoints:        []Endpoint{{Addr: srv.addr, Priority: 2}},
		SeedCert:         srv.clientCert,
		SeedKey:          srv.clientKey,
		CAPath:           srv.caPath,
		HandshakeTimeout: 3 * time.Second,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.DialPriority(context.Background(), 2)
	if !IsNoHigherPriority(err) {
		t.Fatalf("DialPriority(maxPriority=2): err=%v, want IsNoHigherPriority", err)
	}
}

func TestDialPriority_PicksLowerOnly(t *testing.T) {
	srv := newMockEventStream(t, nil)
	defer srv.stop()

	// Two endpoints: priority=1 (live mock), priority=2 (dead). Requesting
	// maxPriority=3 picks priority=1.
	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{
			{Addr: srv.addr, Priority: 1},
			{Addr: "127.0.0.1:1", Priority: 2},
		},
		SeedCert:         srv.clientCert,
		SeedKey:          srv.clientKey,
		CAPath:           srv.caPath,
		HandshakeTimeout: 3 * time.Second,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.DialPriority(context.Background(), 3)
	if err != nil {
		t.Fatalf("DialPriority(maxPriority=3): %v", err)
	}
	defer sess.Close()
	if sess.Priority() != 1 {
		t.Errorf("session.Priority = %d, want 1", sess.Priority())
	}
}

// alreadyExistsHandler is a handshake handler that rejects Hello with gRPC
// AlreadyExists (like keeper on a held SID lease: acquireSoulLease returns
// AlreadyExists before HelloReply). Returning an error from the handler makes
// EventStream return immediately, closing the stream at handshake.
func alreadyExistsHandler(_ *keeperv1.Hello) (*keeperv1.HelloReply, error) {
	return nil, status.Errorf(codes.AlreadyExists, "soul lease held by another keeper")
}

// TestDial_AllEndpointsLeaseHeld_IsLeaseHeld — all endpoints return
// AlreadyExists at handshake, so Dial's error must be recognized by
// IsLeaseHeld (reconnect-loop applies the modest cap, not the transport cap).
func TestDial_AllEndpointsLeaseHeld_IsLeaseHeld(t *testing.T) {
	srv := newMockEventStream(t, alreadyExistsHandler)
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:        []Endpoint{{Addr: srv.addr}},
		SeedCert:         srv.clientCert,
		SeedKey:          srv.clientKey,
		CAPath:           srv.caPath,
		HandshakeTimeout: 3 * time.Second,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.Dial(context.Background())
	if err == nil {
		t.Fatal("Dial: expected error (lease held), got nil")
	}
	if !IsLeaseHeld(err) {
		t.Fatalf("Dial err=%v, want IsLeaseHeld", err)
	}
}

// TestDial_LeaseHeldOnOneEndpoint_SpraysToOther — AlreadyExists on one
// endpoint must not stall the spray: the walk continues and the second (live)
// endpoint picks up the session. Guards the legitimate fallback/spray path.
func TestDial_LeaseHeldOnOneEndpoint_SpraysToOther(t *testing.T) {
	leaseHeld := newMockEventStream(t, alreadyExistsHandler)
	defer leaseHeld.stop()
	// leaseHeld is priority=1 (always tried first), alive is priority=2 on the
	// same CA; Dial should pass over lease-held to alive.
	alive := newMockEventStreamWithCA(t, nil, leaseHeld)
	defer alive.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{
			{Addr: leaseHeld.addr, Priority: 1},
			{Addr: alive.addr, Priority: 2},
		},
		SeedCert:         leaseHeld.clientCert,
		SeedKey:          leaseHeld.clientKey,
		CAPath:           leaseHeld.caPath,
		HandshakeTimeout: 3 * time.Second,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: expected fallback to alive endpoint, got err=%v", err)
	}
	defer sess.Close()
	// Reached priority=2 (lease-held on priority=1 didn't stall the walk).
	if sess.Priority() != 2 {
		t.Errorf("session.Priority = %d, want 2 (sprayed past lease-held endpoint)", sess.Priority())
	}
	// No final error at all: this is a success, not a lease-held failure.
}

// TestDial_TransportFailure_NotLeaseHeld — a transport failure (dead port)
// must not be classified as lease-held: reconnect-loop should keep the
// general transport cap (regression guard on the soft/transport distinction).
func TestDial_TransportFailure_NotLeaseHeld(t *testing.T) {
	cert, key, ca := mustWriteClientSeed(t)
	cli, err := NewClient(ClientConfig{
		Endpoints:        []Endpoint{{Addr: "127.0.0.1:1"}}, // dead port
		SeedCert:         cert,
		SeedKey:          key,
		CAPath:           ca,
		HandshakeTimeout: 500 * time.Millisecond,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.Dial(context.Background())
	if err == nil {
		t.Fatal("Dial: expected transport error, got nil")
	}
	if IsLeaseHeld(err) {
		t.Fatalf("transport failure misclassified as lease-held: %v", err)
	}
}

// TestDial_MixedLeaseHeldAndTransport_NotLeaseHeld — one endpoint lease-held,
// another transport failure → NOT lease-held (not all failures are
// AlreadyExists, so there's real unavailability; the transport cap fits
// better than the modest cap).
func TestDial_MixedLeaseHeldAndTransport_NotLeaseHeld(t *testing.T) {
	leaseHeld := newMockEventStream(t, alreadyExistsHandler)
	defer leaseHeld.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints: []Endpoint{
			{Addr: leaseHeld.addr, Priority: 1},
			{Addr: "127.0.0.1:1", Priority: 2}, // dead port
		},
		SeedCert:         leaseHeld.clientCert,
		SeedKey:          leaseHeld.clientKey,
		CAPath:           leaseHeld.caPath,
		HandshakeTimeout: 500 * time.Millisecond,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cli.Dial(context.Background())
	if err == nil {
		t.Fatal("Dial: expected error, got nil")
	}
	if IsLeaseHeld(err) {
		t.Fatalf("mixed lease-held + transport misclassified as lease-held: %v", err)
	}
}

func TestStreamSession_PriorityFromDial(t *testing.T) {
	srv := newMockEventStream(t, nil)
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:        []Endpoint{{Addr: srv.addr, Priority: 2}},
		SeedCert:         srv.clientCert,
		SeedKey:          srv.clientKey,
		CAPath:           srv.caPath,
		HandshakeTimeout: 3 * time.Second,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()
	if sess.Priority() != 2 {
		t.Errorf("session.Priority = %d, want 2", sess.Priority())
	}
}

func TestStreamSession_SendTaskEventAndRunResult(t *testing.T) {
	srv := newMockEventStream(t, nil)
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:        []Endpoint{{Addr: srv.addr}},
		SeedCert:         srv.clientCert,
		SeedKey:          srv.clientKey,
		CAPath:           srv.caPath,
		HandshakeTimeout: 5 * time.Second,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()

	if err := sess.SendTaskEvent(&keeperv1.TaskEvent{ApplyId: "x", TaskIdx: 0}); err != nil {
		t.Errorf("SendTaskEvent: %v", err)
	}
	if err := sess.SendRunResult(&keeperv1.RunResult{ApplyId: "x"}); err != nil {
		t.Errorf("SendRunResult: %v", err)
	}

	// Wait for the server to see both messages.
	if !waitFor(func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.taskCount >= 1 && srv.resultCount >= 1
	}, time.Second) {
		t.Errorf("server didn't receive TaskEvent + RunResult in time")
	}
}

// TestStreamSession_SendWardRoster — WardRoster (Soul reconcile) reaches the
// server with a populated set (apply_id + attempt). The empty-set case is a
// separate sub-test: an explicit declaration of "nothing in flight".
func TestStreamSession_SendWardRoster(t *testing.T) {
	srv := newMockEventStream(t, nil)
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:        []Endpoint{{Addr: srv.addr}},
		SeedCert:         srv.clientCert,
		SeedKey:          srv.clientKey,
		CAPath:           srv.caPath,
		HandshakeTimeout: 5 * time.Second,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()

	active := []*keeperv1.ActiveApply{
		{ApplyId: "apply-1", Attempt: 4},
		{ApplyId: "apply-2", Attempt: 0},
	}
	if err := sess.SendWardRoster(active); err != nil {
		t.Fatalf("SendWardRoster: %v", err)
	}

	if !waitFor(func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.wardCount >= 1
	}, time.Second) {
		t.Fatal("server didn't receive WardRoster in time")
	}
	srv.mu.Lock()
	got := srv.lastWard
	srv.mu.Unlock()
	if n := len(got.GetActive()); n != 2 {
		t.Fatalf("WardRoster.active len = %d, want 2", n)
	}
	if got.GetActive()[0].GetApplyId() != "apply-1" || got.GetActive()[0].GetAttempt() != 4 {
		t.Errorf("active[0] = %v, want {apply-1, 4}", got.GetActive()[0])
	}
}

// TestStreamSession_SendWardRoster_Empty — an empty/nil set sends WardRoster
// with an explicit empty active[] (declares "nothing in flight"), not a skip.
func TestStreamSession_SendWardRoster_Empty(t *testing.T) {
	srv := newMockEventStream(t, nil)
	defer srv.stop()

	cli, err := NewClient(ClientConfig{
		Endpoints:        []Endpoint{{Addr: srv.addr}},
		SeedCert:         srv.clientCert,
		SeedKey:          srv.clientKey,
		CAPath:           srv.caPath,
		HandshakeTimeout: 5 * time.Second,
		SID:              "host.example",
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess, err := cli.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()

	if err := sess.SendWardRoster(nil); err != nil {
		t.Fatalf("SendWardRoster(nil): %v", err)
	}
	if !waitFor(func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.wardCount >= 1
	}, time.Second) {
		t.Fatal("server didn't receive empty WardRoster in time")
	}
	srv.mu.Lock()
	got := srv.lastWard
	srv.mu.Unlock()
	if got == nil {
		t.Fatal("WardRoster не получен")
	}
	if n := len(got.GetActive()); n != 0 {
		t.Errorf("WardRoster.active len = %d, want 0 (пустой набор)", n)
	}
}

// --- mock EventStream server (mTLS) ---

type mockEventStream struct {
	keeperv1.UnimplementedKeeperServer
	addr       string
	caCert     *x509.Certificate
	caKey      *rsa.PrivateKey
	caPath     string
	clientCert string
	clientKey  string
	srv        *grpc.Server
	wg         sync.WaitGroup

	mu          sync.Mutex
	received    *keeperv1.Hello
	taskCount   int
	resultCount int
	wardCount   int
	lastWard    *keeperv1.WardRoster
	// streamCount is the number of opened EventStream calls = number of
	// dialOne attempts to this server (each dialOne opens a new stream).
	// Per-endpoint retry tests count attempts via this field.
	streamCount int

	handler func(*keeperv1.Hello) (*keeperv1.HelloReply, error)
	// ctxHandler is a handshake handler with access to the stream context.
	// Needed for hang cases (per-endpoint failover-latency,
	// ctx-cancel-during-dialOne): it blocks on stream.Context().Done() so the
	// server unblocks when the client closes the connection
	// (sessCancel/conn.Close in dialOne) and GracefulStop in stop() doesn't
	// hang. Takes priority over handler/default when set.
	ctxHandler func(ctx context.Context, hello *keeperv1.Hello) (*keeperv1.HelloReply, error)
}

// dialCount is the thread-safe count of dialOne (EventStream) calls to the server.
func (m *mockEventStream) dialCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streamCount
}

func (m *mockEventStream) EventStream(stream grpc.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper]) error {
	m.mu.Lock()
	m.streamCount++
	m.mu.Unlock()

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return errors.New("expected Hello")
	}
	m.mu.Lock()
	m.received = hello
	m.mu.Unlock()

	var reply *keeperv1.HelloReply
	switch {
	case m.ctxHandler != nil:
		r, err := m.ctxHandler(stream.Context(), hello)
		if err != nil {
			return err
		}
		reply = r
	case m.handler != nil:
		r, err := m.handler(hello)
		if err != nil {
			return err
		}
		reply = r
	default:
		reply = &keeperv1.HelloReply{
			SessionId:  "sess-1",
			Kid:        "test-kid",
			ServerTime: timestamppb.Now(),
		}
	}
	if err := stream.Send(&keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_HelloReply{HelloReply: reply}}); err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) || err != nil {
			return nil
		}
		m.mu.Lock()
		switch p := msg.GetPayload().(type) {
		case *keeperv1.FromSoul_TaskEvent:
			m.taskCount++
		case *keeperv1.FromSoul_RunResult:
			m.resultCount++
		case *keeperv1.FromSoul_WardRoster:
			m.wardCount++
			m.lastWard = p.WardRoster
		}
		m.mu.Unlock()
	}
}

func (m *mockEventStream) stop() {
	m.srv.GracefulStop()
	m.wg.Wait()
}

func newMockEventStream(t *testing.T, handler func(*keeperv1.Hello) (*keeperv1.HelloReply, error)) *mockEventStream {
	t.Helper()
	return newMockEventStreamWithCA(t, handler, nil)
}

// newMockEventStreamCtx starts a mock server with a ctx-aware handshake
// handler (hang cases: failover-latency / ctx-cancel-during-dialOne). src is
// shared CA material as in newMockEventStreamWithCA; nil generates its own CA.
func newMockEventStreamCtx(t *testing.T, ctxHandler func(ctx context.Context, hello *keeperv1.Hello) (*keeperv1.HelloReply, error), src *mockEventStream) *mockEventStream {
	t.Helper()
	mk := newMockEventStreamWithCA(t, nil, src)
	mk.ctxHandler = ctxHandler
	return mk
}

// hangHandler is a handshake handler that never responds: it blocks until the
// client closes the stream (sessCancel/conn.Close in dialOne). The client
// always times out via its local handshake_timeout (time.After in dialOne), so
// dialOne returns "handshake timeout" (codes.Unknown → retriable). The server
// goroutine unblocks on disconnect, so GracefulStop doesn't hang.
func hangHandler(ctx context.Context, _ *keeperv1.Hello) (*keeperv1.HelloReply, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// seqHandler plays back a given sequence of outcomes indexed by dialOne call
// count against this server. On the i-th call (1-based) it returns
// codes[i-1]: codes.OK for a successful HelloReply, otherwise
// status.Error(code). Past the sequence's end, the last outcome repeats.
// Models "one endpoint, different errors on different attempts".
func seqHandler(seq ...codes.Code) func(*keeperv1.Hello) (*keeperv1.HelloReply, error) {
	var calls int
	return func(_ *keeperv1.Hello) (*keeperv1.HelloReply, error) {
		calls++
		idx := calls - 1
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		code := seq[idx]
		if code == codes.OK {
			return &keeperv1.HelloReply{SessionId: "sess-seq", Kid: "test-kid", ServerTime: timestamppb.Now()}, nil
		}
		return nil, status.Errorf(code, "seq attempt %d code %s", calls, code)
	}
}

// newMockEventStreamWithCA starts a mock server. If src != nil, the new server
// reuses src's CA/cert material (shared trust for a test with multiple
// endpoints under one client CAPath — a spray within one client config can't
// verify servers on different CAs). src=nil generates its own CA.
func newMockEventStreamWithCA(t *testing.T, handler func(*keeperv1.Hello) (*keeperv1.HelloReply, error), src *mockEventStream) *mockEventStream {
	t.Helper()

	var (
		caCert     *x509.Certificate
		caKey      *rsa.PrivateKey
		caPath     string
		clientCert string
		clientKey  string
	)
	if src != nil {
		caCert, caKey = src.caCert, src.caKey
		caPath, clientCert, clientKey = src.caPath, src.clientCert, src.clientKey
	} else {
		caCert, caKey = mustGenerateCA(t)
		clientCertDER, clientKeyRSA := mustGenerateLeafCert(t, caCert, caKey, "soul-host.example", false /* client */)
		dir := t.TempDir()
		caPath = writePEM(t, dir, "ca.pem", "CERTIFICATE", caCert.Raw)
		clientCert = writePEM(t, dir, "client.crt", "CERTIFICATE", clientCertDER)
		clientKey = writeRSAKey(t, dir, "client.key", clientKeyRSA)
	}

	serverCertDER, serverKey := mustGenerateLeafCert(t, caCert, caKey, "127.0.0.1", true /* server */)
	tlsCert := tls.Certificate{Certificate: [][]byte{serverCertDER}, PrivateKey: serverKey}
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS13,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	mk := &mockEventStream{
		addr:       ln.Addr().String(),
		caCert:     caCert,
		caKey:      caKey,
		caPath:     caPath,
		clientCert: clientCert,
		clientKey:  clientKey,
		srv:        srv,
		handler:    handler,
	}
	keeperv1.RegisterKeeperServer(srv, mk)

	mk.wg.Add(1)
	go func() {
		defer mk.wg.Done()
		_ = srv.Serve(ln)
	}()
	return mk
}

func mustGenerateCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("CreateCertificate(ca): %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert, key
}

func mustGenerateLeafCert(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, host string, isServer bool) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, key.Public(), caKey)
	if err != nil {
		t.Fatalf("CreateCertificate(%s): %v", host, err)
	}
	return der, key
}

func writePEM(t *testing.T, dir, name, typ string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func writeRSAKey(t *testing.T, dir, name string, key *rsa.PrivateKey) string {
	t.Helper()
	path := filepath.Join(dir, name)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// mustWriteClientSeed generates and writes a client mTLS seed (cert/key/ca) to
// disk without starting a server. For dial tests against an unreachable
// endpoint, where only the client material matters. Returns paths (cert, key, ca).
func mustWriteClientSeed(t *testing.T) (cert, key, ca string) {
	t.Helper()
	caCert, caKey := mustGenerateCA(t)
	clientCertDER, clientKeyRSA := mustGenerateLeafCert(t, caCert, caKey, "soul-host.example", false)
	dir := t.TempDir()
	ca = writePEM(t, dir, "ca.pem", "CERTIFICATE", caCert.Raw)
	cert = writePEM(t, dir, "client.crt", "CERTIFICATE", clientCertDER)
	key = writeRSAKey(t, dir, "client.key", clientKeyRSA)
	return cert, key, ca
}

func waitFor(pred func() bool, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return pred()
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
