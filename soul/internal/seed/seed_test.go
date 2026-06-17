package seed

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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

// keypair генерирует согласованную пару (cert.pem, key.pem) — self-signed
// ECDSA-cert, который проходит tls.X509KeyPair. ECDSA — быстрее RSA в тестах.
func keypair(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "soul-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// validMaterial — согласованный Material с заданной CA-строкой (ca не участвует
// в X509-валидации, поэтому может быть произвольной).
func validMaterial(t *testing.T, ca string) *Material {
	t.Helper()
	cert, key := keypair(t)
	return &Material{CertPEM: cert, KeyPEM: key, CAPEM: []byte(ca)}
}

func TestWriteAndLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := validMaterial(t, "CA")
	if err := Write(dir, m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got.CertPEM) != string(m.CertPEM) ||
		string(got.KeyPEM) != string(m.KeyPEM) ||
		string(got.CAPEM) != "CA" {
		t.Fatalf("Load returned mismatched material")
	}

	// Первичный Write создаёт v1 + current -> v1.
	if v := readVersions(t, dir); len(v) != 1 || v[0] != 1 {
		t.Fatalf("versions = %v; want [1]", v)
	}
	assertCurrent(t, dir, "v1")

	// key.pem активной версии должен быть 0o400.
	st, err := os.Stat(filepath.Join(dir, "v1", KeyFile))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o400 {
		t.Errorf("key.pem mode = %o, want 0400", perm)
	}
}

func TestLoad_MissingCurrent(t *testing.T) {
	// Пустой каталог: нет current → ErrIncomplete.
	_, err := Load(t.TempDir())
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Load: %v; want ErrIncomplete", err)
	}
}

// TestLoad_FlatFormatHardCut: в каталоге лежат плоские cert/key/ca без current.
// Hard-cut M1 — авто-миграции нет, Load даёт ErrIncomplete (а не читает плоский
// формат).
func TestLoad_FlatFormatHardCut(t *testing.T) {
	dir := t.TempDir()
	cert, key := keypair(t)
	if err := os.WriteFile(filepath.Join(dir, CertFile), cert, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, KeyFile), key, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CAFile), []byte("ca"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Load на плоском формате: %v; want ErrIncomplete (hard-cut)", err)
	}
}

// TestWrite_Rotation: вторая запись создаёт v2, current -> v2, v1 сохранён
// (current + 1 предыдущая).
func TestWrite_Rotation(t *testing.T) {
	dir := t.TempDir()
	first := validMaterial(t, "ca1")
	second := validMaterial(t, "ca2")
	if err := Write(dir, first); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := Write(dir, second); err != nil {
		t.Fatalf("rotation Write: %v", err)
	}
	// v1 + v2 на диске, current -> v2.
	if v := readVersions(t, dir); len(v) != 2 || v[0] != 1 || v[1] != 2 {
		t.Fatalf("versions = %v; want [1 2]", v)
	}
	assertCurrent(t, dir, "v2")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after rotation: %v", err)
	}
	if string(got.CAPEM) != "ca2" || string(got.CertPEM) != string(second.CertPEM) {
		t.Fatalf("Load after rotation returned old version")
	}
}

// TestWrite_PrunesOldVersions: после трёх ротаций остаётся только current и
// одна предыдущая (v3 + v4), v1/v2 удалены.
func TestWrite_PrunesOldVersions(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 4; i++ {
		if err := Write(dir, validMaterial(t, "ca")); err != nil {
			t.Fatalf("Write #%d: %v", i+1, err)
		}
	}
	v := readVersions(t, dir)
	if len(v) != 2 || v[0] != 3 || v[1] != 4 {
		t.Fatalf("versions after 4 writes = %v; want [3 4] (current + 1)", v)
	}
	assertCurrent(t, dir, "v4")
}
