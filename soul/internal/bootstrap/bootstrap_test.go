package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/soul/internal/seed"
)

func TestValidSID(t *testing.T) {
	t.Parallel()
	good := []string{"host", "host1.example.com", "a.b.c"}
	bad := []string{"", "Foo", "-bad", ".bad", "host:8443"}
	for _, s := range good {
		if !ValidSID(s) {
			t.Errorf("ValidSID(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidSID(s) {
			t.Errorf("ValidSID(%q) = true, want false", s)
		}
	}
}

func TestHostFromAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantHost string
		wantOK   bool
	}{
		{"host:8443", "host", true},
		{"k1.dc1.example:8443", "k1.dc1.example", true},
		{"127.0.0.1:9000", "127.0.0.1", true},
		{"", "", false},
		{"noport", "", false},
		{"fe80::1:8443", "", false},
	}
	for _, c := range cases {
		got, ok := hostFromAddr(c.in)
		if got != c.wantHost || ok != c.wantOK {
			t.Errorf("hostFromAddr(%q) = (%q,%v); want (%q,%v)", c.in, got, ok, c.wantHost, c.wantOK)
		}
	}
}

func TestGenerateKeyAndCSR(t *testing.T) {
	t.Parallel()
	key, csrPEM, err := generateKeyAndCSR("host.example")
	if err != nil {
		t.Fatalf("generateKeyAndCSR: %v", err)
	}
	if key == nil {
		t.Fatal("key is nil")
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("csr pem block = %+v", block)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificateRequest: %v", err)
	}
	if csr.Subject.CommonName != "host.example" {
		t.Errorf("CN = %q, want host.example", csr.Subject.CommonName)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Errorf("CheckSignature: %v", err)
	}
}

// --- end-to-end via a mock Keeper Bootstrap server ---

func TestRun_SuccessWritesSeed(t *testing.T) {
	srv := newMockKeeper(t, nil)
	defer srv.stop()

	dir := t.TempDir()
	cfg := Config{
		SID:              "soul-host.example",
		Token:            "plain-token",
		SeedDir:          filepath.Join(dir, "seed"),
		KeeperCA:         srv.caPath,
		Endpoints:        []string{srv.addr},
		HandshakeTimeout: 5 * time.Second,
		SoulVersion:      "0.0.0-test",
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SID != "soul-host.example" {
		t.Errorf("SID = %q", res.SID)
	}
	if res.Endpoint != srv.addr {
		t.Errorf("Endpoint = %q", res.Endpoint)
	}
	if res.KID != "test-kid" {
		t.Errorf("KID = %q", res.KID)
	}

	srv.receivedM.Lock()
	got := srv.received
	srv.receivedM.Unlock()
	if got == nil {
		t.Fatal("mock received no Bootstrap request")
	}
	if got.GetSid() != "soul-host.example" || got.GetBootstrapToken() != "plain-token" {
		t.Errorf("unexpected request payload: %+v", got)
	}
	if got.GetSoulVersion() != "0.0.0-test" {
		t.Errorf("soul_version = %q", got.GetSoulVersion())
	}

	// seed.Load validates the cert↔key pair via X509KeyPair — success means the
	// written cert is consistent with Soul's private key.
	mat, err := seed.Load(cfg.SeedDir)
	if err != nil {
		t.Fatalf("seed.Load: %v", err)
	}
	if len(mat.KeyPEM) == 0 {
		t.Error("key.pem is empty")
	}
	block, _ := pem.Decode(mat.CertPEM)
	if block == nil {
		t.Fatal("cert.pem is not valid PEM")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert.pem: %v", err)
	}
	if crt.Subject.CommonName != "soul-host.example" {
		t.Errorf("cert CN = %q; want soul-host.example", crt.Subject.CommonName)
	}
	if len(mat.CAPEM) == 0 {
		t.Error("ca.pem is empty")
	}
}

func TestRun_FallsBackToSecondEndpoint(t *testing.T) {
	good := newMockKeeper(t, nil)
	defer good.stop()

	cfg := Config{
		SID:              "host.example",
		Token:            "tok",
		SeedDir:          t.TempDir(),
		KeeperCA:         good.caPath,
		Endpoints:        []string{"127.0.0.1:1", good.addr}, // first is a dead port
		HandshakeTimeout: 2 * time.Second,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Endpoint != good.addr {
		t.Errorf("expected fallback to %q, got %q", good.addr, res.Endpoint)
	}
}

func TestRun_AllEndpointsFail(t *testing.T) {
	caPath := writeStandaloneCAForTest(t)
	cfg := Config{
		SID:              "host.example",
		Token:            "tok",
		SeedDir:          t.TempDir(),
		KeeperCA:         caPath,
		Endpoints:        []string{"127.0.0.1:1"},
		HandshakeTimeout: 500 * time.Millisecond,
	}
	_, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run on dead endpoint must fail")
	}
}

func TestRun_RejectsBadSID(t *testing.T) {
	cfg := Config{
		SID:       "BAD UPPER",
		Token:     "tok",
		SeedDir:   t.TempDir(),
		KeeperCA:  writeStandaloneCAForTest(t),
		Endpoints: []string{"127.0.0.1:1"},
	}
	_, err := Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "invalid sid") {
		t.Fatalf("Run = %v; want invalid sid error", err)
	}
}

func TestRun_RejectsEmptyToken(t *testing.T) {
	_, err := Run(context.Background(), Config{
		SeedDir:   t.TempDir(),
		KeeperCA:  "x",
		Endpoints: []string{"127.0.0.1:1"},
	})
	if err == nil || !strings.Contains(err.Error(), "token is empty") {
		t.Fatalf("Run = %v; want token is empty error", err)
	}
}

func TestRun_BootstrapRPCError(t *testing.T) {
	srv := newMockKeeper(t, func(req *keeperv1.BootstrapRequest) (*keeperv1.BootstrapReply, error) {
		return nil, status.Error(codes.PermissionDenied, "token rejected")
	})
	defer srv.stop()

	cfg := Config{
		SID:       "host.example",
		Token:     "tok",
		SeedDir:   t.TempDir(),
		KeeperCA:  srv.caPath,
		Endpoints: []string{srv.addr},
	}
	_, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected RPC error, got nil")
	}
	if !strings.Contains(err.Error(), "PermissionDenied") {
		t.Errorf("error = %v; want to contain PermissionDenied", err)
	}
	// On failure, seed is not written to disk (no active current version).
	if _, loadErr := seed.Load(cfg.SeedDir); loadErr == nil {
		t.Error("seed written despite RPC failure")
	}
}

// --- mock keeper ---

type mockKeeper struct {
	keeperv1.UnimplementedKeeperServer
	addr       string
	caPath     string
	srv        *grpc.Server
	wg         sync.WaitGroup
	handler    func(*keeperv1.BootstrapRequest) (*keeperv1.BootstrapReply, error)
	issuerCert *x509.Certificate
	issuerKey  *rsa.PrivateKey
	receivedM  sync.Mutex
	received   *keeperv1.BootstrapRequest
}

func (m *mockKeeper) Bootstrap(ctx context.Context, req *keeperv1.BootstrapRequest) (*keeperv1.BootstrapReply, error) {
	m.receivedM.Lock()
	m.received = req
	m.receivedM.Unlock()
	if m.handler != nil {
		return m.handler(req)
	}
	// Issue a REAL cert under the public key from Soul's CSR — otherwise
	// seed.Write would reject the mismatched cert↔key pair on X509 validation.
	certPEM := m.issueCertFromCSR(req.GetCsrPem())
	return &keeperv1.BootstrapReply{
		CertificatePem: certPEM,
		CaChainPem:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.issuerCert.Raw}),
		NotAfter:       timestamppb.New(time.Now().Add(24 * time.Hour)),
		Kid:            "test-kid",
	}, nil
}

// issueCertFromCSR parses the CSR, issues a client cert under its public key,
// signed by the mock's CA. This gives a matching pair (cert + Soul's private
// key) that passes tls.X509KeyPair in seed.Write.
func (m *mockKeeper) issueCertFromCSR(csrPEM []byte) []byte {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return []byte("invalid-csr")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return []byte("invalid-csr")
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      csr.Subject,
		DNSNames:     csr.DNSNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.issuerCert, csr.PublicKey, m.issuerKey)
	if err != nil {
		return []byte("issue-failed")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func (m *mockKeeper) stop() {
	m.srv.GracefulStop()
	m.wg.Wait()
}

// newMockKeeper starts a gRPC server with server-only TLS on :0 and a
// self-signed CA. Returns addr and the path to the CA bundle (for cfg.KeeperCA).
func newMockKeeper(t *testing.T, handler func(*keeperv1.BootstrapRequest) (*keeperv1.BootstrapReply, error)) *mockKeeper {
	t.Helper()

	caCert, caKey := mustGenerateCA(t)
	serverCertDER, serverKey := mustGenerateServerCert(t, caCert, caKey, "127.0.0.1")

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	tlsCert := tls.Certificate{
		Certificate: [][]byte{serverCertDER},
		PrivateKey:  serverKey,
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS13,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	mk := &mockKeeper{
		addr:       ln.Addr().String(),
		caPath:     caPath,
		srv:        srv,
		handler:    handler,
		issuerCert: caCert,
		issuerKey:  caKey,
	}
	keeperv1.RegisterKeeperServer(srv, mk)

	mk.wg.Add(1)
	go func() {
		defer mk.wg.Done()
		_ = srv.Serve(ln)
	}()
	return mk
}

// writeStandaloneCAForTest writes any valid CA PEM to a file and returns the
// path. Used in tests where the server is unreachable but Config.KeeperCA
// still needs a valid PEM (otherwise LoadClientTLS errors before dialing).
func writeStandaloneCAForTest(t *testing.T) string {
	t.Helper()
	cert, _ := mustGenerateCA(t)
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
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

func mustGenerateServerCert(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, host string) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		IPAddresses:  []net.IP{net.ParseIP(host)},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, key.Public(), caKey)
	if err != nil {
		t.Fatalf("CreateCertificate(server): %v", err)
	}
	return der, key
}
