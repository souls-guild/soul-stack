package grpc

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestBootstrapServer_RecvCeilingFitsARealRequest pins the recv ceiling against
// the largest request a real client sends.
//
// The ceiling was cut from 256 KiB to 16 KiB so a queued caller cannot pin
// hundreds of megabytes (NIM-839). Nothing else in the tree relates that number
// to a BootstrapRequest, so a future tightening would break live onboarding
// with a green suite — an RSA CSR is the one field whose size is not obvious.
func TestBootstrapServer_RecvCeilingFitsARealRequest(t *testing.T) {
	req := &keeperv1.BootstrapRequest{
		// 253 bytes is the longest legal FQDN.
		Sid:            strings.Repeat("a", 253),
		BootstrapToken: strings.Repeat("t", bootstrapMaxTokenLen),
		CsrPem:         makeCSRPEM(t, "host.example.com"), // RSA-2048, as soul/internal/bootstrap builds
		SoulVersion:    "v1.2.3-rc1+build.20260912",
	}
	size := proto.Size(req)
	if size >= bootstrapMaxRecvMsgSize {
		t.Fatalf("a maximal legitimate BootstrapRequest is %d bytes, ceiling is %d — real onboarding would be refused",
			size, bootstrapMaxRecvMsgSize)
	}
	// Headroom, not just "fits": the CSR grows with the key size, and a client
	// moving to RSA-4096 roughly doubles it.
	if got := bootstrapMaxRecvMsgSize / size; got < 4 {
		t.Errorf("only %dx headroom over a maximal request (%d bytes of %d) — too tight for a larger key",
			got, size, bootstrapMaxRecvMsgSize)
	}
}

// TestBootstrapServer_ConnectionCeilingDefaults — an unset MaxConns must resolve
// to the default and never to zero.
//
// `netutil.LimitListener(ln, 0)` does not refuse connections, it blocks Accept
// forever: the listener would be up, bound, and answering nothing. A caller
// that passes no MaxConns is the production caller, so the branch that decides
// this is the one nothing else exercises.
func TestBootstrapServer_ConnectionCeilingDefaults(t *testing.T) {
	dir := t.TempDir()
	cp, kp := mustSelfSigned(t, dir)
	cfg := config.KeeperListenGRPCBootstrap{
		Addr: "127.0.0.1:0",
		TLS:  config.KeeperListenGRPCBootstrapTLS{Cert: cp, Key: kp},
	}
	srv, err := NewBootstrapServer(cfg, fakeValidDeps(), discardLogger(t))
	if err != nil {
		t.Fatalf("NewBootstrapServer: %v", err)
	}
	if srv.maxConns <= 0 {
		t.Fatalf("maxConns = %d — LimitListener would block Accept forever", srv.maxConns)
	}
	if srv.maxConns != defaultBootstrapMaxConns {
		t.Errorf("maxConns = %d, want the default %d", srv.maxConns, defaultBootstrapMaxConns)
	}
}

// TestBootstrapServer_ConnectionCeiling is the other half of the NIM-839 guard:
// the per-connection stream ceiling bounded nothing while the number of
// connections was the attacker's to choose.
//
// It is written as a pair because there is no clean signal for "not accepted" —
// a connection over the ceiling sits in the kernel backlog, and the only thing
// the client observes is its own deadline. So the positive arm here IS a
// deadline observation and nothing stronger; what makes the pair honest is the
// control arm, which runs the same call on a longer deadline once the held
// connection is closed and expects it to succeed. Read it as "the ceiling, and
// nothing else about this machine, is what stopped it" — not as a refusal.
// Drop the LimitListener wrap in Start and the first arm succeeds.
func TestBootstrapServer_ConnectionCeiling(t *testing.T) {
	dir := t.TempDir()
	cp, kp := mustSelfSigned(t, dir)
	cfg := config.KeeperListenGRPCBootstrap{
		Addr: "127.0.0.1:0",
		TLS:  config.KeeperListenGRPCBootstrapTLS{Cert: cp, Key: kp},
	}
	deps := fakeValidDeps()
	deps.MaxConns = 1

	srv, err := NewBootstrapServer(cfg, deps, discardLogger(t))
	if err != nil {
		t.Fatalf("NewBootstrapServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startCh := make(chan error, 1)
	go func() { startCh <- srv.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case <-startCh:
		case <-time.After(5 * time.Second):
			t.Error("Start did not return after ctx cancel")
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (srv.Addr() == "" || srv.Addr() == cfg.Addr) {
		time.Sleep(10 * time.Millisecond)
	}

	// One raw socket occupies the single slot. It never speaks TLS: holding a
	// slot without completing a handshake is the cheapest form of the attack,
	// and bootstrapConnectionTimeout is what eventually takes it back.
	hog, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial hog connection: %v", err)
	}

	if err := pingBootstrap(t, srv.Addr(), 1500*time.Millisecond); err == nil {
		_ = hog.Close()
		t.Fatal("Ping succeeded while the connection ceiling was spent — the listener accepted past MaxConns")
	}

	_ = hog.Close()

	// The control arm: the slot is free, the same call on the same deadline
	// goes through.
	if err := pingBootstrap(t, srv.Addr(), 5*time.Second); err != nil {
		t.Fatalf("Ping failed after the ceiling was freed: %v — the refusal above was not the ceiling", err)
	}
}

func pingBootstrap(t *testing.T, addr string, timeout time.Duration) error {
	t.Helper()
	conn, err := grpclib.NewClient(
		addr,
		grpclib.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})),
	)
	if err != nil {
		t.Fatalf("grpc NewClient: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err = keeperv1.NewKeeperClient(conn).Ping(ctx, &keeperv1.PingRequest{}, grpclib.WaitForReady(true))
	return err
}
