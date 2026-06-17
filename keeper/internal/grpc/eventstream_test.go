package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeSeedDB — реализация [soulseed.ExecQueryRower] на in-memory map-е.
// Используется для unit-тестов [SeedAuthenticator].
type fakeSeedDB struct {
	byFingerprint map[string]fakeSeedRow
	queryErr      error
}

type fakeSeedRow struct {
	seedID, sid string
	status      soulseed.Status
}

func (db *fakeSeedDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (db *fakeSeedDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}

func (db *fakeSeedDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	if db.queryErr != nil {
		return errRow{err: db.queryErr}
	}
	if len(args) == 0 {
		return errRow{err: pgx.ErrNoRows}
	}
	fp, _ := args[0].(string)
	row, ok := db.byFingerprint[fp]
	if !ok {
		return errRow{err: pgx.ErrNoRows}
	}
	return fakeRowVal{row: row, fp: fp}
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type fakeRowVal struct {
	row fakeSeedRow
	fp  string
}

func (r fakeRowVal) Scan(dest ...any) error {
	// Порядок совпадает с selectByFingerprintSQL в soulseed/crud.go:
	// seed_id, sid, fingerprint, serial_number, issued_at, expires_at,
	// issued_by_kid, status, revocation_reason.
	if len(dest) != 9 {
		return pgx.ErrNoRows
	}
	*(dest[0].(*string)) = r.row.seedID
	*(dest[1].(*string)) = r.row.sid
	*(dest[2].(*string)) = r.fp
	*(dest[3].(*string)) = "serial-123"
	*(dest[4].(*time.Time)) = time.Now().Add(-time.Hour)
	*(dest[5].(*time.Time)) = time.Now().Add(time.Hour)
	*(dest[6].(**string)) = nil
	*(dest[7].(*string)) = string(r.row.status)
	*(dest[8].(**string)) = nil
	return nil
}

func TestSeedAuthenticator_NoPeer(t *testing.T) {
	a := NewSeedAuthenticator(&fakeSeedDB{}, discardLogger(t))
	_, err := a.Authenticate(context.Background())
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
}

func TestSeedAuthenticator_NotTLS(t *testing.T) {
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		AuthInfo: nil,
	})
	a := NewSeedAuthenticator(&fakeSeedDB{}, discardLogger(t))
	_, err := a.Authenticate(ctx)
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
}

func TestSeedAuthenticator_NoCert(t *testing.T) {
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: nil}},
	})
	a := NewSeedAuthenticator(&fakeSeedDB{}, discardLogger(t))
	_, err := a.Authenticate(ctx)
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
}

func TestSeedAuthenticator_UnknownFingerprint(t *testing.T) {
	dir := t.TempDir()
	ctx := ctxWithLeafCert(t, dir)
	a := NewSeedAuthenticator(&fakeSeedDB{byFingerprint: map[string]fakeSeedRow{}}, discardLogger(t))
	_, err := a.Authenticate(ctx)
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
}

func TestSeedAuthenticator_Revoked(t *testing.T) {
	dir := t.TempDir()
	ctx := ctxWithLeafCert(t, dir)
	cert := mustLoadCert(t, filepath.Join(dir, "leaf.pem"))
	fp := soulseed.FingerprintFromCert(cert)
	a := NewSeedAuthenticator(&fakeSeedDB{
		byFingerprint: map[string]fakeSeedRow{
			fp: {seedID: "seed-1", sid: "host.example.com", status: soulseed.StatusRevoked},
		},
	}, discardLogger(t))
	_, err := a.Authenticate(ctx)
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
}

func TestSeedAuthenticator_HappyPath(t *testing.T) {
	dir := t.TempDir()
	ctx := ctxWithLeafCert(t, dir)
	cert := mustLoadCert(t, filepath.Join(dir, "leaf.pem"))
	fp := soulseed.FingerprintFromCert(cert)
	a := NewSeedAuthenticator(&fakeSeedDB{
		byFingerprint: map[string]fakeSeedRow{
			fp: {seedID: "seed-1", sid: "host.example.com", status: soulseed.StatusActive},
		},
	}, discardLogger(t))
	sid, err := a.Authenticate(ctx)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if sid != "host.example.com" {
		t.Errorf("sid = %q, want host.example.com", sid)
	}
}

// ctxWithLeafCert — кладёт в context фейковый peer-cert, генерируя его
// в указанной директории (PEM-файл `leaf.pem` остаётся для последующего
// mustLoadCert в том же тесте).
func ctxWithLeafCert(t *testing.T, dir string) context.Context {
	t.Helper()
	certPath, _ := mustSelfSigned(t, dir)
	// Сохраним copy под именем leaf.pem, чтобы тест мог восстановить
	// fingerprint независимо от внутренних имён mustSelfSigned.
	if certPath != filepath.Join(dir, "leaf.pem") {
		mustCopyFile(t, certPath, filepath.Join(dir, "leaf.pem"))
	}
	cert := mustLoadCert(t, filepath.Join(dir, "leaf.pem"))
	return peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{cert},
		}},
	})
}

func mustLoadCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	pemBytes, err := readFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	cert, err := parseFirstCert(pemBytes)
	if err != nil {
		t.Fatalf("parse cert from %s: %v", path, err)
	}
	return cert
}

func TestNewEventStreamServer_EmptyAddr(t *testing.T) {
	_, err := NewEventStreamServer(config.KeeperListenGRPCEventStream{}, EventStreamDeps{}, slog.Default())
	if err == nil || !contains(err.Error(), "addr is empty") {
		t.Fatalf("err = %v, want addr is empty", err)
	}
}

func TestNewEventStreamServer_NilLogger(t *testing.T) {
	_, err := NewEventStreamServer(
		config.KeeperListenGRPCEventStream{Addr: "127.0.0.1:0"},
		EventStreamDeps{}, nil,
	)
	if err == nil || !contains(err.Error(), "logger is required") {
		t.Fatalf("err = %v, want logger is required", err)
	}
}

func TestNewEventStreamServer_DepsValidation(t *testing.T) {
	dir := t.TempDir()
	cp, kp := mustSelfSigned(t, dir)
	ca := filepath.Join(dir, "ca.pem")
	mustCopyFile(t, cp, ca)
	cfg := config.KeeperListenGRPCEventStream{
		Addr: "127.0.0.1:0",
		TLS:  config.KeeperListenGRPCEventStreamTLS{Cert: cp, Key: kp, CA: ca},
	}
	_, err := NewEventStreamServer(cfg, EventStreamDeps{}, slog.Default())
	if err == nil || !contains(err.Error(), "SeedDB is required") {
		t.Fatalf("err = %v, want SeedDB is required", err)
	}
}

func TestNewEventStreamServer_MissingCAFile(t *testing.T) {
	dir := t.TempDir()
	cp, kp := mustSelfSigned(t, dir)
	cfg := config.KeeperListenGRPCEventStream{
		Addr: "127.0.0.1:0",
		TLS:  config.KeeperListenGRPCEventStreamTLS{Cert: cp, Key: kp, CA: filepath.Join(dir, "nope.ca")},
	}
	_, err := NewEventStreamServer(cfg, EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: nopAudit{},
		KID:         "kid-test",
	}, slog.Default())
	if err == nil {
		t.Fatal("expected error on missing CA")
	}
}

func TestEventStreamServer_StartShutdown(t *testing.T) {
	dir := t.TempDir()
	cp, kp := mustSelfSigned(t, dir)
	ca := filepath.Join(dir, "ca.pem")
	mustCopyFile(t, cp, ca)
	cfg := config.KeeperListenGRPCEventStream{
		Addr: "127.0.0.1:0",
		TLS:  config.KeeperListenGRPCEventStreamTLS{Cert: cp, Key: kp, CA: ca},
	}
	srv, err := NewEventStreamServer(cfg, EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: nopAudit{},
		KID:         "kid-test",
	}, discardLogger(t))
	if err != nil {
		t.Fatalf("NewEventStreamServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.Addr(); a != "" && a != cfg.Addr {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Addr() == cfg.Addr {
		cancel()
		<-errCh
		t.Fatalf("Addr not updated to actual listener: %q", srv.Addr())
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

// mustCopyFile / readFile / parseFirstCert — мелкие helpers, не светим
// наружу. parseCertificatePEM из bootstrap.go доступен в том же пакете.
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func parseFirstCert(pemBytes []byte) (*x509.Certificate, error) {
	return parseCertificatePEM(pemBytes)
}

func mustCopyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}
