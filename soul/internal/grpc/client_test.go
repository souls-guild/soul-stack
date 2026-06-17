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
	"google.golang.org/grpc/credentials"
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

// --- end-to-end через mock EventStream-сервер ---

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

	// SID_echo доходит до сервера.
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

	// Один endpoint с priority=2; запрашиваем maxPriority=2 → должно вернуть
	// sentinel «нет higher-priority».
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

	// Два endpoint-а: priority=1 (живой mock), priority=2 (dead). Запрос
	// maxPriority=3 → берётся priority=1.
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

	// Дожидаемся, пока сервер увидит оба сообщения.
	if !waitFor(func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.taskCount >= 1 && srv.resultCount >= 1
	}, time.Second) {
		t.Errorf("server didn't receive TaskEvent + RunResult in time")
	}
}

// TestStreamSession_SendWardRoster — WardRoster (Soul-reconcile) доходит до
// сервера с заполненным набором (apply_id + attempt). Пустой набор — отдельный
// под-тест: явная декларация «ничего не ведётся».
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

// TestStreamSession_SendWardRoster_Empty — пустой/nil набор шлёт WardRoster с
// пустым active[] (явная декларация «ничего не ведётся»), а не пропускает.
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

	handler func(*keeperv1.Hello) (*keeperv1.HelloReply, error)
}

func (m *mockEventStream) EventStream(stream grpc.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper]) error {
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
	if m.handler != nil {
		r, err := m.handler(hello)
		if err != nil {
			return err
		}
		reply = r
	} else {
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

	caCert, caKey := mustGenerateCA(t)
	serverCertDER, serverKey := mustGenerateLeafCert(t, caCert, caKey, "127.0.0.1", true /* server */)
	clientCertDER, clientKey := mustGenerateLeafCert(t, caCert, caKey, "soul-host.example", false /* client */)

	dir := t.TempDir()
	caPath := writePEM(t, dir, "ca.pem", "CERTIFICATE", caCert.Raw)
	clientCertPath := writePEM(t, dir, "client.crt", "CERTIFICATE", clientCertDER)
	clientKeyPath := writeRSAKey(t, dir, "client.key", clientKey)

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
		caPath:     caPath,
		clientCert: clientCertPath,
		clientKey:  clientKeyPath,
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
