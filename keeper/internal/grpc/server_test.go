package grpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeTxBeginner — a stub for unit-testing server assembly. Begin
// shouldn't be called in server-startup tests (only the Bootstrap RPC
// touches Tx). QueryRow returns ErrNoRows: the pre-check treats this as an
// invalid token (anti-enum PermissionDenied), which suits the Bootstrap RPC
// tests that don't need a valid token.
type fakeTxBeginner struct{}

func (fakeTxBeginner) Begin(_ context.Context) (pgx.Tx, error) {
	return nil, nil
}

func (fakeTxBeginner) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return scanErrRow{err: pgx.ErrNoRows}
}

func (fakeTxBeginner) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (fakeTxBeginner) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

// scanErrRow — a pgx.Row that always returns the given error from Scan.
type scanErrRow struct{ err error }

func (r scanErrRow) Scan(_ ...any) error { return r.err }

type fakeSigner struct{}

func (fakeSigner) SignCSR(_ context.Context, _, _, _ string) (*keepervault.SignedCertificate, error) {
	return nil, nil
}

type nopAudit struct{}

func (nopAudit) Write(_ context.Context, _ *audit.Event) error { return nil }

func TestNewBootstrapServer_EmptyAddr(t *testing.T) {
	_, err := NewBootstrapServer(config.KeeperListenGRPCBootstrap{}, BootstrapDeps{}, slog.Default())
	if err == nil || !contains(err.Error(), "addr is empty") {
		t.Fatalf("err = %v, want addr is empty", err)
	}
}

func TestNewBootstrapServer_NilLogger(t *testing.T) {
	_, err := NewBootstrapServer(
		config.KeeperListenGRPCBootstrap{Addr: "127.0.0.1:0"},
		BootstrapDeps{},
		nil,
	)
	if err == nil || !contains(err.Error(), "logger is required") {
		t.Fatalf("err = %v, want logger is required", err)
	}
}

func TestNewBootstrapServer_DepsValidation(t *testing.T) {
	dir := t.TempDir()
	cp, kp := mustSelfSigned(t, dir)
	cfg := config.KeeperListenGRPCBootstrap{
		Addr: "127.0.0.1:0",
		TLS:  config.KeeperListenGRPCBootstrapTLS{Cert: cp, Key: kp},
	}
	_, err := NewBootstrapServer(cfg, BootstrapDeps{}, slog.Default())
	if err == nil || !contains(err.Error(), "Pool is required") {
		t.Fatalf("err = %v, want Pool is required", err)
	}
}

func TestNewBootstrapServer_MissingTLSFiles(t *testing.T) {
	cfg := config.KeeperListenGRPCBootstrap{
		Addr: "127.0.0.1:0",
		TLS: config.KeeperListenGRPCBootstrapTLS{
			Cert: "/no/such/cert.pem",
			Key:  "/no/such/key.pem",
		},
	}
	_, err := NewBootstrapServer(cfg, fakeValidDeps(), slog.Default())
	if err == nil {
		t.Fatal("expected error on missing cert/key files")
	}
}

// TestBootstrapServer_StartShutdown brings up a gRPC server on an
// ephemeral port, does a Ping via a TLS client, and confirms that
// a graceful shutdown on ctx.Done() returns nil.
func TestBootstrapServer_StartShutdown(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// Wait until the listener binds.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.Addr(); a != "" && a != cfg.Addr {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Addr() == cfg.Addr {
		t.Fatalf("Addr not updated to actual listener: %q", srv.Addr())
	}

	// Smoke: TLS dial with InsecureSkipVerify (test self-signed cert).
	conn, err := grpclib.NewClient(
		srv.Addr(),
		grpclib.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})),
	)
	if err != nil {
		t.Fatalf("grpc Dial: %v", err)
	}
	defer conn.Close()
	client := keeperv1.NewKeeperClient(conn)
	pCtx, pCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pCancel()
	reply, err := client.Ping(pCtx, &keeperv1.PingRequest{})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if reply.GetVersion() != "kid-test" {
		t.Errorf("Ping.Version = %q, want kid-test", reply.GetVersion())
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start returned err on graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}
}

// TestBootstrapServer_RejectsPlaintextClient — a client without TLS
// shouldn't pass the handshake (Ping returns an error before the dial
// timeout).
func TestBootstrapServer_RejectsPlaintextClient(t *testing.T) {
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

	conn, err := grpclib.NewClient(srv.Addr(), grpclib.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc Dial: %v", err)
	}
	defer conn.Close()
	client := keeperv1.NewKeeperClient(conn)
	pCtx, pCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pCancel()
	if _, err := client.Ping(pCtx, &keeperv1.PingRequest{}); err == nil {
		t.Fatal("Ping over plaintext succeeded, want TLS handshake failure")
	}
}

// fakeValidDeps — non-nil deps only for exercising the validate phase and
// server assembly. Real Pool/Vault aren't used — the Bootstrap RPC isn't
// called in these tests (only Ping).
func fakeValidDeps() BootstrapDeps {
	return BootstrapDeps{
		Pool:        fakeTxBeginner{},
		VaultClient: fakeSigner{},
		AuditWriter: nopAudit{},
		KID:         "kid-test",
		PKIMount:    "pki",
		PKIRole:     "soul-seed",
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func discardLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mustSelfSigned — a copy of the helper from shared/tlsx (test-only, not
// exported); generates an ECDSA cert + key in the given directory.
func mustSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"test.local", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	_ = certOut.Close()
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatal(err)
	}
	_ = keyOut.Close()
	return certPath, keyPath
}
