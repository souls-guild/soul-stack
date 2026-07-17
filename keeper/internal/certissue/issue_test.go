package certissue

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
)

// genCertPEM issues a self-signed cert and returns its PEM + the parsed
// *x509.Certificate (for fingerprint verification via keepercert.FingerprintFromCert).
func genCertPEM(t *testing.T) ([]byte, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "redis.tls"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsecert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert
}

type fakeSigner struct {
	signed                    *SignedCert
	err                       error
	gotMount, gotRole, gotCSR string
	calls                     int
}

func (s *fakeSigner) SignCSR(_ context.Context, mount, role, csrPEM string) (*SignedCert, error) {
	s.calls++
	s.gotMount, s.gotRole, s.gotCSR = mount, role, csrPEM
	return s.signed, s.err
}

type kvCall struct {
	path string
	data map[string]any
}

type fakeKV struct {
	calls   []kvCall
	errPath string // WriteKV(path==errPath) returns errFor
	errFor  error
}

func (w *fakeKV) WriteKV(_ context.Context, path string, data map[string]any) error {
	w.calls = append(w.calls, kvCall{path: path, data: data})
	if w.errPath != "" && path == w.errPath {
		return w.errFor
	}
	return nil
}

func TestVaultPath(t *testing.T) {
	if got := VaultPath("redis", "inc-1", keepercert.KindCert); got != "secret/redis/inc-1/tls/cert" {
		t.Errorf("cert path = %q", got)
	}
	if got := VaultPath("redis", "inc-1", keepercert.KindKey); got != "secret/redis/inc-1/tls/key" {
		t.Errorf("key path = %q", got)
	}
}

// TestVaultPath_RejectsUnsafeSegment — * defense-in-depth (NIM-99 review M4): an unsafe
// service/incarnation (traversal `..` / `/` separator / empty) must not yield a path outside
// secret/<service>/<incarnation>/ — a caller invariant, the panic is immediately visible.
func TestVaultPath_RejectsUnsafeSegment(t *testing.T) {
	cases := []struct{ service, incarnation string }{
		{"a/../b", "inc"},
		{"svc", "x/y"},
		{"svc", ".."},
		{"", "inc"},
		{"svc", ""},
	}
	for _, tc := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("VaultPath(%q,%q) should panic (unsafe segment)", tc.service, tc.incarnation)
				}
			}()
			_ = VaultPath(tc.service, tc.incarnation, keepercert.KindCert)
		}()
	}
}

func TestIssue_HappyPath(t *testing.T) {
	certPEM, cert := genCertPEM(t)
	wantFP := keepercert.FingerprintFromCert(cert)
	notAfter := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)

	signer := &fakeSigner{signed: &SignedCert{
		CertificatePEM: certPEM,
		CAChainPEM:     []byte("CA-CHAIN-DISCARDED"),
		SerialNumber:   "SER-123",
		NotAfter:       notAfter,
	}}
	kv := &fakeKV{}
	var gotCN string
	var gotDNS []string
	csrgen := func(cn string, dns []string) ([]byte, []byte, error) {
		gotCN, gotDNS = cn, dns
		return []byte("PRIV-KEY-MATERIAL"), []byte("CSR-BYTES"), nil
	}
	p := Params{
		CommonName: "redis-prod.tls",
		DNSNames:   []string{"redis-prod.tls", "redis-prod"},
		Mount:      "pki",
		Role:       "redis-server",
		CertPath:   "secret/redis/redis-prod/tls/cert",
		KeyPath:    "secret/redis/redis-prod/tls/key",
	}

	m, err := Issue(context.Background(), signer, kv, csrgen, p)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// csrgen received CN/DNS.
	if gotCN != "redis-prod.tls" {
		t.Errorf("csrgen CN = %q", gotCN)
	}
	if len(gotDNS) != 2 || gotDNS[0] != "redis-prod.tls" || gotDNS[1] != "redis-prod" {
		t.Errorf("csrgen DNS = %v", gotDNS)
	}
	// signer received mount/role/csr.
	if signer.gotMount != "pki" || signer.gotRole != "redis-server" {
		t.Errorf("signer mount/role = %q/%q", signer.gotMount, signer.gotRole)
	}
	if signer.gotCSR != "CSR-BYTES" {
		t.Errorf("signer csr = %q", signer.gotCSR)
	}
	// WriteKV called 2x with the right paths and fields.
	if len(kv.calls) != 2 {
		t.Fatalf("WriteKV calls = %d, want 2", len(kv.calls))
	}
	if kv.calls[0].path != p.CertPath {
		t.Errorf("cert WriteKV path = %q, want %q", kv.calls[0].path, p.CertPath)
	}
	if kv.calls[0].data["cert"] != string(certPEM) {
		t.Errorf("cert WriteKV data[cert] mismatch")
	}
	if kv.calls[1].path != p.KeyPath {
		t.Errorf("key WriteKV path = %q, want %q", kv.calls[1].path, p.KeyPath)
	}
	if kv.calls[1].data["key"] != "PRIV-KEY-MATERIAL" {
		t.Errorf("key WriteKV data[key] mismatch")
	}
	// Material.
	if string(m.CertPEM) != string(certPEM) {
		t.Errorf("Material.CertPEM mismatch")
	}
	if string(m.KeyPEM) != "PRIV-KEY-MATERIAL" {
		t.Errorf("Material.KeyPEM = %q", m.KeyPEM)
	}
	if m.SerialNumber != "SER-123" {
		t.Errorf("Material.SerialNumber = %q", m.SerialNumber)
	}
	if m.Fingerprint != wantFP {
		t.Errorf("Material.Fingerprint = %q, want %q", m.Fingerprint, wantFP)
	}
	if !m.NotAfter.Equal(notAfter) {
		t.Errorf("Material.NotAfter = %v, want %v", m.NotAfter, notAfter)
	}
	if m.CertRef != p.CertPath+"#cert" {
		t.Errorf("Material.CertRef = %q", m.CertRef)
	}
	if m.KeyRef != p.KeyPath+"#key" {
		t.Errorf("Material.KeyRef = %q", m.KeyRef)
	}
}

// TestIssue_SignerError_NoPrivLeak — * R2: on a signer error, the private material must not be
// in the error text; WriteKV is not called (sign fails before the write).
func TestIssue_SignerError_NoPrivLeak(t *testing.T) {
	const privMarker = "TOP-SECRET-PRIVATE-KEY-DO-NOT-LEAK"
	signer := &fakeSigner{err: errors.New("vault pki unreachable")}
	kv := &fakeKV{}
	csrgen := func(_ string, _ []string) ([]byte, []byte, error) {
		return []byte(privMarker), []byte("CSR"), nil
	}

	_, err := Issue(context.Background(), signer, kv, csrgen, Params{
		CommonName: "x.tls", Mount: "pki", Role: "r",
		CertPath: "secret/x/tls/cert", KeyPath: "secret/x/tls/key",
	})
	if err == nil {
		t.Fatal("expected error from signer")
	}
	if strings.Contains(err.Error(), privMarker) {
		t.Errorf("private material leaked into the error text: %v", err)
	}
	if len(kv.calls) != 0 {
		t.Errorf("WriteKV called %d times with a failed signer (material must not be written)", len(kv.calls))
	}
}

// TestIssue_KeyWriteError_NoPrivLeak — * R2: even when the key-record write fails,
// the private material must not appear in the error text (WriteKV does not log values).
func TestIssue_KeyWriteError_NoPrivLeak(t *testing.T) {
	const privMarker = "TOP-SECRET-PRIVATE-KEY-DO-NOT-LEAK"
	keyPath := "secret/x/tls/key"
	signer := &fakeSigner{signed: &SignedCert{CertificatePEM: []byte("cert-pem"), SerialNumber: "s"}}
	kv := &fakeKV{errPath: keyPath, errFor: errors.New("vault write denied")}
	csrgen := func(_ string, _ []string) ([]byte, []byte, error) {
		return []byte(privMarker), []byte("CSR"), nil
	}

	_, err := Issue(context.Background(), signer, kv, csrgen, Params{
		CommonName: "x.tls", Mount: "pki", Role: "r",
		CertPath: "secret/x/tls/cert", KeyPath: keyPath,
	})
	if err == nil {
		t.Fatal("expected error from WriteKV(key)")
	}
	if strings.Contains(err.Error(), privMarker) {
		t.Errorf("private material leaked into the error text on a WriteKV(key) failure: %v", err)
	}
}
