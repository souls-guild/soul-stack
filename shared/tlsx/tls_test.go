package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadServerOnlyTLS_HappyPath(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := mustWriteSelfSigned(t, dir, "test.local")

	cfg, err := LoadServerOnlyTLS(ServerConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("LoadServerOnlyTLS: %v", err)
	}

	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want %x", cfg.MinVersion, tls.VersionTLS13)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert", cfg.ClientAuth)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("Certificates len = %d, want 1", len(cfg.Certificates))
	}
}

func TestLoadServerOnlyTLS_EmptyPaths(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
		want error
	}{
		{"empty cert", ServerConfig{CertPath: "", KeyPath: "k"}, ErrServerCertEmpty},
		{"empty key", ServerConfig{CertPath: "c", KeyPath: ""}, ErrServerKeyEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadServerOnlyTLS(tc.cfg)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is(%v)", err, tc.want)
			}
		})
	}
}

func TestLoadServerOnlyTLS_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadServerOnlyTLS(ServerConfig{
		CertPath: filepath.Join(dir, "nope.pem"),
		KeyPath:  filepath.Join(dir, "nope.key"),
	})
	if err == nil {
		t.Fatal("LoadServerOnlyTLS: nil err, want stat error")
	}
}

func TestLoadServerOnlyTLS_MalformedCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadServerOnlyTLS(ServerConfig{CertPath: certPath, KeyPath: keyPath})
	if err == nil {
		t.Fatal("LoadServerOnlyTLS: nil err on malformed PEM, want error")
	}
}

// Cert present, key missing — the second pre-flight stat (on KeyPath) fails.
func TestLoadServerOnlyTLS_MissingKeyOnly(t *testing.T) {
	dir := t.TempDir()
	certPath, _ := mustWriteSelfSigned(t, dir, "test.local")
	_, err := LoadServerOnlyTLS(ServerConfig{
		CertPath: certPath,
		KeyPath:  filepath.Join(dir, "absent.key"),
	})
	if err == nil {
		t.Fatal("LoadServerOnlyTLS: nil err, want stat error on missing key")
	}
	if !os.IsNotExist(errors.Unwrap(err)) {
		t.Errorf("err = %v, want wrapped os.ErrNotExist", err)
	}
}

// CAPath is ignored in server-only mode: even a non-empty, broken CA has no effect.
func TestLoadServerOnlyTLS_CAPathIgnored(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := mustWriteSelfSigned(t, dir, "test.local")
	cfg, err := LoadServerOnlyTLS(ServerConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
		CAPath:   filepath.Join(dir, "does-not-exist-and-must-be-ignored.ca"),
	})
	if err != nil {
		t.Fatalf("LoadServerOnlyTLS: CAPath must be ignored, got %v", err)
	}
	if cfg.ClientCAs != nil {
		t.Error("ClientCAs must be nil in server-only mode")
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert", cfg.ClientAuth)
	}
}

func TestLoadMutualTLS_HappyPath(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := mustWriteSelfSigned(t, dir, "test.local")
	// CA bundle for the test is the server cert itself (self-signed = self-CA).
	caPath := filepath.Join(dir, "ca.pem")
	srcPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, srcPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadMutualTLS(MutualConfig{
		CertPath: certPath, KeyPath: keyPath, CAPath: caPath,
	})
	if err != nil {
		t.Fatalf("LoadMutualTLS: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want %x", cfg.MinVersion, tls.VersionTLS13)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Fatal("ClientCAs is nil")
	}
}

func TestLoadMutualTLS_EmptyPaths(t *testing.T) {
	cases := []struct {
		name string
		cfg  MutualConfig
		want error
	}{
		{"empty cert", MutualConfig{KeyPath: "k", CAPath: "ca"}, ErrServerCertEmpty},
		{"empty key", MutualConfig{CertPath: "c", CAPath: "ca"}, ErrServerKeyEmpty},
		{"empty ca", MutualConfig{CertPath: "c", KeyPath: "k"}, ErrServerCAEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadMutualTLS(tc.cfg)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is(%v)", err, tc.want)
			}
		})
	}
}

func TestLoadMutualTLS_MissingCAFile(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := mustWriteSelfSigned(t, dir, "test.local")
	_, err := LoadMutualTLS(MutualConfig{
		CertPath: certPath, KeyPath: keyPath,
		CAPath: filepath.Join(dir, "nope.ca"),
	})
	if err == nil {
		t.Fatal("LoadMutualTLS: nil err, want stat error on missing CA")
	}
}

func TestLoadMutualTLS_MalformedCA(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := mustWriteSelfSigned(t, dir, "test.local")
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, []byte("not a pem bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadMutualTLS(MutualConfig{
		CertPath: certPath, KeyPath: keyPath, CAPath: caPath,
	})
	if err == nil {
		t.Fatal("LoadMutualTLS: nil err on malformed CA bundle")
	}
}

// Cert missing, key and CA present — the first pre-flight stat (cert) fails.
func TestLoadMutualTLS_MissingCertOnly(t *testing.T) {
	dir := t.TempDir()
	_, keyPath := mustWriteSelfSigned(t, dir, "test.local")
	caPath := writeCABundle(t, dir, "test.local")
	_, err := LoadMutualTLS(MutualConfig{
		CertPath: filepath.Join(dir, "absent.pem"),
		KeyPath:  keyPath,
		CAPath:   caPath,
	})
	if err == nil {
		t.Fatal("LoadMutualTLS: nil err, want stat error on missing cert")
	}
	if !os.IsNotExist(errors.Unwrap(err)) {
		t.Errorf("err = %v, want wrapped os.ErrNotExist", err)
	}
}

// Key missing, cert and CA present — the second pre-flight stat (key) fails.
func TestLoadMutualTLS_MissingKeyOnly(t *testing.T) {
	dir := t.TempDir()
	certPath, _ := mustWriteSelfSigned(t, dir, "test.local")
	caPath := writeCABundle(t, dir, "test.local")
	_, err := LoadMutualTLS(MutualConfig{
		CertPath: certPath,
		KeyPath:  filepath.Join(dir, "absent.key"),
		CAPath:   caPath,
	})
	if err == nil {
		t.Fatal("LoadMutualTLS: nil err, want stat error on missing key")
	}
	if !os.IsNotExist(errors.Unwrap(err)) {
		t.Errorf("err = %v, want wrapped os.ErrNotExist", err)
	}
}

// All three files exist (stat passes) but the key does not match the cert —
// tls.LoadX509KeyPair fails. Covers the pair-parsing error branch.
func TestLoadMutualTLS_CertKeyMismatch(t *testing.T) {
	dir := t.TempDir()
	certPath, _ := mustWriteSelfSigned(t, dir, "cert.local")
	// A foreign key from another pair — stat passes, but the pair won't match.
	otherDir := filepath.Join(dir, "other")
	if err := os.Mkdir(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, foreignKey := mustWriteSelfSigned(t, otherDir, "other.local")
	caPath := writeCABundle(t, dir, "cert.local")

	_, err := LoadMutualTLS(MutualConfig{
		CertPath: certPath, KeyPath: foreignKey, CAPath: caPath,
	})
	if err == nil {
		t.Fatal("LoadMutualTLS: nil err on cert/key mismatch, want error")
	}
}

// CAPath points at a directory: os.Stat passes, os.ReadFile fails (EISDIR). A real case
// of "the operator pointed at a directory instead of the CA file", covering the CA-read
// error branch after a successful stat without injecting an fs error.
func TestLoadMutualTLS_CAPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := mustWriteSelfSigned(t, dir, "test.local")
	caDir := filepath.Join(dir, "cadir")
	if err := os.Mkdir(caDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := LoadMutualTLS(MutualConfig{
		CertPath: certPath, KeyPath: keyPath, CAPath: caDir,
	})
	if err == nil {
		t.Fatal("LoadMutualTLS: nil err when CAPath is a directory, want read error")
	}
}

func TestLoadClientTLS_BootstrapMode(t *testing.T) {
	dir := t.TempDir()
	caPath := writeCABundle(t, dir, "keeper.local")

	cfg, err := LoadClientTLS(ClientConfig{
		CAPath:     caPath,
		ServerName: "keeper.local",
	})
	if err != nil {
		t.Fatalf("LoadClientTLS bootstrap: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want %x", cfg.MinVersion, tls.VersionTLS13)
	}
	if cfg.RootCAs == nil {
		t.Fatal("RootCAs is nil")
	}
	if cfg.ServerName != "keeper.local" {
		t.Errorf("ServerName = %q, want keeper.local", cfg.ServerName)
	}
	// Bootstrap = server-only: there must be no client cert.
	if len(cfg.Certificates) != 0 {
		t.Errorf("Certificates len = %d, want 0 in bootstrap mode", len(cfg.Certificates))
	}
}

func TestLoadClientTLS_MutualMode(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := mustWriteSelfSigned(t, dir, "soul.local")
	caPath := writeCABundle(t, dir, "keeper.local")

	cfg, err := LoadClientTLS(ClientConfig{
		CertPath:   certPath,
		KeyPath:    keyPath,
		CAPath:     caPath,
		ServerName: "keeper.local",
	})
	if err != nil {
		t.Fatalf("LoadClientTLS mutual: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want %x", cfg.MinVersion, tls.VersionTLS13)
	}
	if cfg.RootCAs == nil {
		t.Fatal("RootCAs is nil")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("Certificates len = %d, want 1 in mutual mode", len(cfg.Certificates))
	}
}

func TestLoadClientTLS_EmptyCA(t *testing.T) {
	_, err := LoadClientTLS(ClientConfig{CAPath: ""})
	if !errors.Is(err, ErrServerCAEmpty) {
		t.Errorf("err = %v, want errors.Is(ErrServerCAEmpty)", err)
	}
}

func TestLoadClientTLS_MissingCAFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadClientTLS(ClientConfig{CAPath: filepath.Join(dir, "nope.ca")})
	if err == nil {
		t.Fatal("LoadClientTLS: nil err, want stat error on missing CA")
	}
	if !os.IsNotExist(errors.Unwrap(err)) {
		t.Errorf("err = %v, want wrapped os.ErrNotExist", err)
	}
}

// CAPath points at a directory: os.Stat passes, os.ReadFile fails.
func TestLoadClientTLS_CAPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	caDir := filepath.Join(dir, "cadir")
	if err := os.Mkdir(caDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := LoadClientTLS(ClientConfig{CAPath: caDir})
	if err == nil {
		t.Fatal("LoadClientTLS: nil err when CAPath is a directory, want read error")
	}
}

func TestLoadClientTLS_MalformedCA(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, []byte("not a pem bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadClientTLS(ClientConfig{CAPath: caPath})
	if err == nil {
		t.Fatal("LoadClientTLS: nil err on malformed CA bundle")
	}
}

// CA valid, key set, but cert empty → ErrServerCertEmpty
// (path asymmetry: one is set, the other is not).
func TestLoadClientTLS_CertEmptyKeySet(t *testing.T) {
	dir := t.TempDir()
	caPath := writeCABundle(t, dir, "keeper.local")
	_, err := LoadClientTLS(ClientConfig{
		CAPath:  caPath,
		KeyPath: filepath.Join(dir, "k.pem"),
	})
	if !errors.Is(err, ErrServerCertEmpty) {
		t.Errorf("err = %v, want errors.Is(ErrServerCertEmpty)", err)
	}
}

// CA valid, cert set, but key empty → ErrServerKeyEmpty.
func TestLoadClientTLS_KeyEmptyCertSet(t *testing.T) {
	dir := t.TempDir()
	certPath, _ := mustWriteSelfSigned(t, dir, "soul.local")
	caPath := writeCABundle(t, dir, "keeper.local")
	_, err := LoadClientTLS(ClientConfig{
		CAPath:   caPath,
		CertPath: certPath,
	})
	if !errors.Is(err, ErrServerKeyEmpty) {
		t.Errorf("err = %v, want errors.Is(ErrServerKeyEmpty)", err)
	}
}

// CA + key valid, cert path missing on disk → stat cert fails.
func TestLoadClientTLS_MissingCertFile(t *testing.T) {
	dir := t.TempDir()
	_, keyPath := mustWriteSelfSigned(t, dir, "soul.local")
	caPath := writeCABundle(t, dir, "keeper.local")
	_, err := LoadClientTLS(ClientConfig{
		CAPath:   caPath,
		CertPath: filepath.Join(dir, "absent.pem"),
		KeyPath:  keyPath,
	})
	if err == nil {
		t.Fatal("LoadClientTLS: nil err, want stat error on missing cert")
	}
	if !os.IsNotExist(errors.Unwrap(err)) {
		t.Errorf("err = %v, want wrapped os.ErrNotExist", err)
	}
}

// CA + cert valid, key path missing on disk → stat key fails.
func TestLoadClientTLS_MissingKeyFile(t *testing.T) {
	dir := t.TempDir()
	certPath, _ := mustWriteSelfSigned(t, dir, "soul.local")
	caPath := writeCABundle(t, dir, "keeper.local")
	_, err := LoadClientTLS(ClientConfig{
		CAPath:   caPath,
		CertPath: certPath,
		KeyPath:  filepath.Join(dir, "absent.key"),
	})
	if err == nil {
		t.Fatal("LoadClientTLS: nil err, want stat error on missing key")
	}
	if !os.IsNotExist(errors.Unwrap(err)) {
		t.Errorf("err = %v, want wrapped os.ErrNotExist", err)
	}
}

// CA + cert + key exist (stat passes) but the key does not match the cert —
// tls.LoadX509KeyPair fails.
func TestLoadClientTLS_CertKeyMismatch(t *testing.T) {
	dir := t.TempDir()
	certPath, _ := mustWriteSelfSigned(t, dir, "soul.local")
	otherDir := filepath.Join(dir, "other")
	if err := os.Mkdir(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, foreignKey := mustWriteSelfSigned(t, otherDir, "other.local")
	caPath := writeCABundle(t, dir, "keeper.local")

	_, err := LoadClientTLS(ClientConfig{
		CAPath:   caPath,
		CertPath: certPath,
		KeyPath:  foreignKey,
	})
	if err == nil {
		t.Fatal("LoadClientTLS: nil err on cert/key mismatch, want error")
	}
}

// writeCABundle writes a standalone CA certificate (CERTIFICATE-PEM only, no key) and
// returns its path. Usable with AppendCertsFromPEM (valid PEM).
func writeCABundle(t *testing.T, dir, cn string) (caPath string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	caPath = filepath.Join(dir, "ca-bundle.pem")
	out, err := os.OpenFile(caPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(out, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	return caPath
}

// mustWriteSelfSigned writes a self-signed ECDSA cert + key into the given directory and
// returns the paths. Used both in tlsx unit tests and (indirectly via a test helper) in
// gRPC integration tests.
func mustWriteSelfSigned(t *testing.T, dir, cn string) (certPath, keyPath string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cn, "localhost"},
		IsCA:         false,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
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
