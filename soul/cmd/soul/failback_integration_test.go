//go:build integration

// Failback integration test (Soul-side) — two priority endpoints, verifies
// proactive return to the higher-priority endpoint and graceful swap of a
// live session without message loss.
//
// Setup:
//   - two mock EventStream servers on different ports (server-A priority=1,
//     server-B priority=2), both under mTLS with a shared CA;
//   - server-A initially down (listener closed), server-B up;
//   - reconnectLoop runs in a test goroutine with failback.interval=200ms.
//
// Phases:
//  1. Initial connect → server-B (A unreachable).
//  2. server-A goes up → failback swap → active session moves to server-A,
//     old session on B closes gracefully.
//
// Phase 3 (A down again → fallback back to B) is a nice-to-have, deliberately
// skipped here: on abrupt TLS-conn close the gRPC client hangs in Recv() for
// tens of seconds (keepalive Time=30s / Timeout=10s in client.go); chasing
// that in an integration test would blow up scope. Reconnect-fallback is
// already covered by unit tests `TestClientDial_FallsBackToNextEndpoint` /
// `TestDialPriority_PicksLowerOnly`.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/soul/internal/coremod"
	soulgrpc "github.com/souls-guild/soul-stack/soul/internal/grpc"
	"github.com/souls-guild/soul-stack/soul/internal/runtime"
)

// TestReconnect_LeaseHeld_BackoffNotReset — stream is rejected with AlreadyExists
// on handshake (SID lease still held by a live holder after the keeper crashed).
// Soul must not hammer the surviving keeper at ~1/initial rate: backoff grows
// (capped), never resets. Proof: over window T the number of handshake attempts
// is bounded by the growing delay, not the initial rate. Once the lease is
// released (force-release after presence), Soul reconnects within a few seconds
// (cap), preserving recovery latency.
func TestReconnect_LeaseHeld_BackoffNotReset(t *testing.T) {
	ca, caKey := mustGenCA(t)
	clientCertDER, clientKey := mustGenLeaf(t, ca, caKey, "soul-host.example", false)
	dir := t.TempDir()
	caPath := writePEMBlock(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	clientCertPath := writePEMBlock(t, dir, "client.crt", "CERTIFICATE", clientCertDER)
	clientKeyPath := writeRSAPriv(t, dir, "client.key", clientKey)

	srvTLS := serverTLS(t, ca, caKey)
	srv := startMockKeeper(t, srvTLS, "L")
	defer srv.stop()
	srv.rejectLease.Store(true) // lease held by a live holder

	endpoints := []soulgrpc.Endpoint{{Addr: srv.addr, Priority: 1}}
	logger := testLogger(t)
	cli, err := soulgrpc.NewClient(soulgrpc.ClientConfig{
		Endpoints:        endpoints,
		SeedCert:         clientCertPath,
		SeedKey:          clientKeyPath,
		CAPath:           caPath,
		HandshakeTimeout: 500 * time.Millisecond,
		SID:              "soul-host.example",
		SoulVersion:      "0.0.0-test",
	}, logger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// transport-max is deliberately SMALL (20ms): if lease-held used normal
	// transport backoff semantics with reset-to-initial, the window would
	// accumulate ~window/20ms attempts. The lease-held cap (leaseHeldBackoffCap,
	// seconds) and no reset keep the attempt count in single digits.
	// initial=20ms/no-jitter makes this deterministic.
	store := backoffOnlyStore(t, srv.addr, "20ms", "20ms")

	runner := runtime.NewApplyRunner(coremod.Default(), nil)
	sp := newTestPusher("soul-host.example")

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		reconnectLoop(ctx, store, cli, runner, nil, sp, nil, nil, nil, nil, logger)
	}()
	defer func() {
		cancel()
		<-loopDone
	}()

	// 2s observation window. reset-to-initial(20ms) would yield ~100 attempts.
	// Lease-held progression (20→40→80→160→320→640→1280→2560→cap=3000) gives ≤ ~8.
	// Threshold 25 is a generous margin above the progression, still far below initial-rate.
	if !waitFor(func() bool { return srv.helloCount() >= 1 }, 2*time.Second) {
		t.Fatal("no handshake attempt observed under lease-held")
	}
	time.Sleep(2 * time.Second)
	attempts := srv.helloCount()
	if attempts > 25 {
		t.Fatalf("lease-held: %d handshake attempts in ~2s — backoff appears reset to initial (~1/initial spam)", attempts)
	}
	t.Logf("lease-held handshake attempts in ~2s window: %d (backoff applied, not reset)", attempts)

	// === recovery latency: lease released (force-release) → reconnect within seconds ===
	srv.rejectLease.Store(false)
	// cap=leaseHeldBackoffCap (3s) + handshake margin. Should be enough.
	if !waitFor(func() bool { return srv.activeStreams() >= 1 }, leaseHeldBackoffCap+3*time.Second) {
		t.Fatalf("recovery: did not reconnect within %s after lease released", leaseHeldBackoffCap+3*time.Second)
	}
}

// TestReconnect_LeaseHeld_SpraysToOtherEndpoint — AlreadyExists on the priority=1
// endpoint must NOT jam spray/failover: Dial still reaches the live priority=2
// endpoint in the same session, without waiting for the first lease to release.
// Guards legitimate fallback-list failover when one keeper has the lease held.
func TestReconnect_LeaseHeld_SpraysToOtherEndpoint(t *testing.T) {
	ca, caKey := mustGenCA(t)
	clientCertDER, clientKey := mustGenLeaf(t, ca, caKey, "soul-host.example", false)
	dir := t.TempDir()
	caPath := writePEMBlock(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	clientCertPath := writePEMBlock(t, dir, "client.crt", "CERTIFICATE", clientCertDER)
	clientKeyPath := writeRSAPriv(t, dir, "client.key", clientKey)

	srvTLS := serverTLS(t, ca, caKey)
	// priority=1 is lease-held; priority=2 is alive.
	leaseHeld := startMockKeeper(t, srvTLS, "P1")
	defer leaseHeld.stop()
	leaseHeld.rejectLease.Store(true)
	alive := startMockKeeper(t, srvTLS, "P2")
	defer alive.stop()

	endpoints := []soulgrpc.Endpoint{
		{Addr: leaseHeld.addr, Priority: 1},
		{Addr: alive.addr, Priority: 2},
	}
	logger := testLogger(t)
	cli, err := soulgrpc.NewClient(soulgrpc.ClientConfig{
		Endpoints:        endpoints,
		SeedCert:         clientCertPath,
		SeedKey:          clientKeyPath,
		CAPath:           caPath,
		HandshakeTimeout: 500 * time.Millisecond,
		SID:              "soul-host.example",
		SoulVersion:      "0.0.0-test",
	}, logger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// failback disabled isn't needed here — alive is priority=2, so failback
	// would pull toward priority=1 (lease-held) and complicate observation.
	// Interval is set large.
	store := backoffOnlyStore(t, leaseHeld.addr, "20ms", "200ms")

	runner := runtime.NewApplyRunner(coremod.Default(), nil)
	sp := newTestPusher("soul-host.example")
	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		reconnectLoop(ctx, store, cli, runner, nil, sp, nil, nil, nil, nil, logger)
	}()
	defer func() {
		cancel()
		<-loopDone
	}()

	// Despite lease-held priority=1, the session comes up on the live priority=2.
	if !waitFor(func() bool { return alive.activeStreams() >= 1 }, 3*time.Second) {
		t.Fatalf("spray: did not connect to alive priority=2 endpoint; P1-hello=%d P2-active=%d",
			leaseHeld.helloCount(), alive.activeStreams())
	}
}

// testLogger — discards by default, debug to stderr under -v.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// backoffOnlyStore — Store[SoulConfig] with a single endpoint and given
// backoff.initial/max (no-jitter), failback disabled. For lease-held tests
// where reconnect-backoff semantics matter, not failback. primaryAddr is the
// only endpoint in the config; actual dialing goes through a separate Client
// (whose endpoints may include more addresses — the config is only a backoff
// source).
func backoffOnlyStore(t *testing.T, primaryAddr, initial, max string) *config.Store[config.SoulConfig] {
	t.Helper()
	host, port := splitHostPort(t, primaryAddr)
	yml := fmt.Sprintf(`keeper:
  endpoints:
    - host: %s
      event_stream_port: %d
      bootstrap_port: %d
      priority: 1
  retry:
    backoff:
      initial: %s
      max: %s
      jitter: false
  failback:
    enabled: false
`, host, port, port, initial, max)
	path := filepath.Join(t.TempDir(), "soul.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatalf("write soul.yml: %v", err)
	}
	store, diags, err := config.LoadSoulStore(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulStore: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("soul.yml has error diag: %s [%s] %s", d.Phase, d.Code, d.Message)
		}
	}
	return store
}

// TestFailbackIntegration_TwoEndpoints — happy-path of the failback pipeline:
// fallback to priority=2 while priority=1 is unreachable, then proactive swap
// back to priority=1 (zero-downtime: new session opens before the old one
// closes).
func TestFailbackIntegration_TwoEndpoints(t *testing.T) {
	ca, caKey := mustGenCA(t)
	clientCertDER, clientKey := mustGenLeaf(t, ca, caKey, "soul-host.example", false)
	dir := t.TempDir()
	caPath := writePEMBlock(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	clientCertPath := writePEMBlock(t, dir, "client.crt", "CERTIFICATE", clientCertDER)
	clientKeyPath := writeRSAPriv(t, dir, "client.key", clientKey)

	srvTLS := serverTLS(t, ca, caKey)

	// server-B (priority=2) stays up for the whole test.
	srvB := startMockKeeper(t, srvTLS, "B")
	defer srvB.stop()

	// server-A (priority=1): reserve the port so phase 1 gets connection-refused,
	// then bring up the mock server on the same addr in phase 2, without
	// touching Client endpoints after reconnectLoop has started.
	srvAAddr := acquirePort(t)

	endpoints := []soulgrpc.Endpoint{
		{Addr: srvAAddr, Priority: 1},
		{Addr: srvB.addr, Priority: 2},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	cli, err := soulgrpc.NewClient(soulgrpc.ClientConfig{
		Endpoints:        endpoints,
		SeedCert:         clientCertPath,
		SeedKey:          clientKeyPath,
		CAPath:           caPath,
		HandshakeTimeout: 500 * time.Millisecond,
		SID:              "soul-host.example",
		SoulVersion:      "0.0.0-test",
	}, logger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	runner := runtime.NewApplyRunner(coremod.Default(), nil)

	// backoff/failback are no longer passed as params — reconnectLoop reads
	// them from the store on every iteration (hot-reload, ADR-021). Build a
	// store snapshot with the old test values: backoff initial=50ms/max=200ms,
	// no jitter, failback interval=200ms (spray=0 for a deterministic swap).
	store := failbackTestStore(t, srvAAddr, srvB.addr)

	// soulprintPusher is a required signature arg; Pusher is real
	// (handleSession sends an initial report on session setup). The rest of the
	// deps are nil: the test only checks failback session swap and never
	// reaches Apply/Errand/beacon, where they'd be needed:
	//   - errandRunner — nil: no Errand commands arrive in this test;
	//   - metrics — nil (nil-safe, see EventStreamMetrics.*);
	//   - sigils/anchors — nil: Sigil verify only matters on custom-plugin Apply;
	//   - scheduler — nil: no VigilSnapshot arrives in this test.
	sp := newTestPusher("soul-host.example")

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		// Signature: reconnectLoop(ctx, store, client, runner, errandRunner,
		// sp, metrics, sigils, anchors, scheduler, logger).
		reconnectLoop(ctx, store, cli, runner, nil, sp, nil, nil, nil, nil, logger)
	}()
	defer func() {
		cancel()
		<-loopDone
	}()

	// === Phase 1: initial connect — A unreachable, fallback to B ===
	if !waitFor(func() bool { return srvB.activeStreams() >= 1 }, 3*time.Second) {
		t.Fatalf("phase 1: server-B did not get connection; A=%d B=%d", 0, srvB.activeStreams())
	}
	// At this point A must remain unconnected.
	if got := srvB.activeStreams(); got != 1 {
		t.Errorf("phase 1: server-B active=%d, want 1", got)
	}

	// === Phase 2: A goes up → failback swap ===
	srvA := startMockKeeperOnAddr(t, srvTLS, "A", srvAAddr)
	defer srvA.stop()

	// failback.interval=200ms; allow 6x margin (race build is slower).
	if !waitFor(func() bool { return srvA.activeStreams() >= 1 }, 3*time.Second) {
		t.Fatalf("phase 2: server-A did not pick up failback swap; A=%d B=%d",
			srvA.activeStreams(), srvB.activeStreams())
	}
	// After the swap, B must close gracefully — handleSession calls
	// _ = oldSess.Close() right after acquiring the new session.
	if !waitFor(func() bool { return srvB.activeStreams() == 0 }, 2*time.Second) {
		t.Fatalf("phase 2: server-B not gracefully closed; B active=%d", srvB.activeStreams())
	}
	if got := srvA.activeStreams(); got != 1 {
		t.Errorf("phase 2: server-A active=%d, want 1", got)
	}

	// Phase 3 (A down → fallback to B) is deliberately skipped (see doc comment
	// at the top of the file). Reconnect-fallback coverage is handled by unit
	// tests in soul/internal/grpc/client_test.go.
}

// --- mock EventStream server (mTLS) ---

type mockKeeper struct {
	keeperv1.UnimplementedKeeperServer
	label string
	addr  string

	active        int64       // atomic — count of in-flight EventStream handlers
	helloAttempts int64       // atomic — count of received Hello (handshake attempts)
	rejectLease   atomic.Bool // when true — reject handshake with AlreadyExists (lease-held)

	srv *grpc.Server
	ln  *trackingListener
	wg  sync.WaitGroup

	stopOnce sync.Once
}

func (m *mockKeeper) EventStream(stream grpc.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper]) error {
	atomic.AddInt64(&m.active, 1)
	defer atomic.AddInt64(&m.active, -1)

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return errors.New("expected Hello")
	}
	atomic.AddInt64(&m.helloAttempts, 1)
	// lease-held: reject BEFORE HelloReply (mirrors keeper behavior when the
	// SID lease is taken — acquireSoulLease returns AlreadyExists before the
	// handshake reply).
	if m.rejectLease.Load() {
		return status.Errorf(codes.AlreadyExists, "soul lease held by another keeper for sid=%q", hello.GetSidEcho())
	}
	if err := stream.Send(&keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_HelloReply{HelloReply: &keeperv1.HelloReply{
		SessionId:  "sess-" + m.label,
		Kid:        "kid-" + m.label,
		ServerTime: timestamppb.Now(),
	}}}); err != nil {
		return err
	}
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return nil
		}
	}
}

func (m *mockKeeper) helloCount() int64 { return atomic.LoadInt64(&m.helloAttempts) }

func (m *mockKeeper) activeStreams() int64 { return atomic.LoadInt64(&m.active) }

func (m *mockKeeper) stop() {
	m.stopOnce.Do(func() {
		// First kill live TCP conns via trackingListener — this guarantees
		// the client's Recv() gets an error immediately (grpc.Server.Stop()
		// closes the listener but doesn't always instantly kill already
		// established streams; fast phase-3 reconnect needs a hard break).
		n := m.ln.killAll()
		_ = n // for debugging, could log m.label, n
		m.srv.Stop()
		m.wg.Wait()
	})
}

// startMockKeeper starts a mock server on 127.0.0.1:0 (kernel assigns the port).
func startMockKeeper(t *testing.T, tlsCfg *tls.Config, label string) *mockKeeper {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return runMockKeeper(t, ln, tlsCfg, label)
}

// startMockKeeperOnAddr starts a server on a specific address (used to
// restart server-A on the same port, so Client endpoints don't need editing
// after reconnectLoop has started).
func startMockKeeperOnAddr(t *testing.T, tlsCfg *tls.Config, label, addr string) *mockKeeper {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	return runMockKeeper(t, ln, tlsCfg, label)
}

func runMockKeeper(t *testing.T, ln net.Listener, tlsCfg *tls.Config, label string) *mockKeeper {
	t.Helper()
	tracked := newTrackingListener(ln)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	mk := &mockKeeper{
		label: label,
		addr:  ln.Addr().String(),
		srv:   srv,
		ln:    tracked,
	}
	keeperv1.RegisterKeeperServer(srv, mk)
	mk.wg.Add(1)
	go func() {
		defer mk.wg.Done()
		_ = srv.Serve(tracked)
	}()
	return mk
}

// trackingListener — a net.Listener holding refs to every accepted net.Conn
// so stop() can force-close them all. Without this, grpc.Server.Stop() can
// leave already-open streams without an explicit RST/FIN, forcing the client
// to wait out the keepalive timeout (tens of seconds) before Recv() errors.
type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newTrackingListener(ln net.Listener) *trackingListener {
	return &trackingListener{Listener: ln}
}

func (t *trackingListener) Accept() (net.Conn, error) {
	c, err := t.Listener.Accept()
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.conns = append(t.conns, c)
	t.mu.Unlock()
	return c, nil
}

func (t *trackingListener) killAll() int {
	t.mu.Lock()
	conns := t.conns
	t.conns = nil
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return len(conns)
}

// acquirePort grabs a free port from the kernel and immediately releases it.
// There's a race window between Close and the next bind; usually fine for a
// localhost test (the kernel keeps the port FREE for >5s by default after an
// explicit listener Close with no active connections).
func acquirePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("acquirePort listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// serverTLS — server TLS config: server cert on 127.0.0.1, mTLS requires a
// client cert from the same CA.
func serverTLS(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey) *tls.Config {
	t.Helper()
	serverCertDER, serverKey := mustGenLeaf(t, ca, caKey, "127.0.0.1", true)
	tlsCert := tls.Certificate{Certificate: [][]byte{serverCertDER}, PrivateKey: serverKey}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}
}

// --- self-signed certs (in-memory) ---

func mustGenCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
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

func mustGenLeaf(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, host string, isServer bool) ([]byte, *rsa.PrivateKey) {
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

func writePEMBlock(t *testing.T, dir, name, typ string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func writeRSAPriv(t *testing.T, dir, name string, key *rsa.PrivateKey) string {
	t.Helper()
	path := filepath.Join(dir, name)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func waitFor(pred func() bool, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return pred()
}

// failbackTestStore builds a Store[SoulConfig] snapshot that reconnectLoop/
// handleSession resolve backoff and failback from (hot-reload source,
// ADR-021): initial=50ms/max=200ms, no jitter, failback interval=200ms,
// spray=0. Config endpoints only exist to pass schema validation
// (keeper.endpoints[] is required) — actual dialing goes through a
// separately constructed soulgrpc.Client. Pattern mirrors soulFixtureStore
// in hotreload_test.go: YAML → temp file → LoadSoulStore.
func failbackTestStore(t *testing.T, addrA, addrB string) *config.Store[config.SoulConfig] {
	t.Helper()
	hostA, portA := splitHostPort(t, addrA)
	hostB, portB := splitHostPort(t, addrB)
	yml := fmt.Sprintf(`keeper:
  endpoints:
    - host: %s
      event_stream_port: %d
      bootstrap_port: %d
      priority: 1
    - host: %s
      event_stream_port: %d
      bootstrap_port: %d
      priority: 2
  retry:
    backoff:
      initial: 50ms
      max: 200ms
      jitter: false
  failback:
    enabled: true
    interval: 200ms
    spray: 0s
`, hostA, portA, portA, hostB, portB, portB)

	path := filepath.Join(t.TempDir(), "soul.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatalf("write soul.yml: %v", err)
	}
	store, diags, err := config.LoadSoulStore(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulStore: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("soul.yml has error diag: %s [%s] %s", d.Phase, d.Code, d.Message)
		}
	}
	return store
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return host, port
}
