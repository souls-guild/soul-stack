package seed

import (
	"os"
	"path/filepath"
	"testing"
)

// materialWithSigil — согласованный Material с заданным trust-anchor-ом Sigil.
func materialWithSigil(t *testing.T, ca, sigilPub string) *Material {
	t.Helper()
	cert, key := keypair(t)
	return &Material{
		CertPEM:        cert,
		KeyPEM:         key,
		CAPEM:          []byte(ca),
		SigilPubKeyPEM: []byte(sigilPub),
	}
}

// TestWriteAndLoad_WithSigilPubKey: trust-anchor пишется в версию и читается
// обратно (ADR-026, S2b). Симметрично TestWriteAndLoad_RoundTrip.
func TestWriteAndLoad_WithSigilPubKey(t *testing.T) {
	dir := t.TempDir()
	const pub = "-----BEGIN PUBLIC KEY-----\nSIGILPUB\n-----END PUBLIC KEY-----\n"
	m := materialWithSigil(t, "CA", pub)
	if err := Write(dir, m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Файл sigil_pubkey.pem физически в активной версии.
	if _, err := os.Stat(filepath.Join(dir, "v1", SigilPubKeyFile)); err != nil {
		t.Fatalf("stat %s: %v", SigilPubKeyFile, err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got.SigilPubKeyPEM) != pub {
		t.Fatalf("SigilPubKeyPEM = %q; want %q", got.SigilPubKeyPEM, pub)
	}
}

// TestWriteAndLoad_SigilDisabled: пустой trust-anchor (Sigil выключен) — файл
// НЕ создаётся, Load даёт nil без ErrIncomplete. Это валидное состояние.
func TestWriteAndLoad_SigilDisabled(t *testing.T) {
	dir := t.TempDir()
	m := validMaterial(t, "CA") // SigilPubKeyPEM == nil
	if err := Write(dir, m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "v1", SigilPubKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("sigil_pubkey.pem should be absent when disabled, stat err = %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SigilPubKeyPEM != nil {
		t.Fatalf("SigilPubKeyPEM = %q; want nil (Sigil disabled)", got.SigilPubKeyPEM)
	}
}

// TestWriteAndLoad_EmptySigilNotWritten: явный []byte("") эквивалентен nil —
// файл не пишется (len == 0 проверяется в Write).
func TestWriteAndLoad_EmptySigilNotWritten(t *testing.T) {
	dir := t.TempDir()
	m := materialWithSigil(t, "CA", "")
	if err := Write(dir, m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v1", SigilPubKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("empty sigil pubkey must not produce a file, stat err = %v", err)
	}
}

// TestRotation_SigilPubKeyPerVersion: trust-anchor переключается атомарно
// вместе с версией. Ротация Sigil-выкл → Sigil-вкл и обратно отражается в Load.
func TestRotation_SigilPubKeyPerVersion(t *testing.T) {
	dir := t.TempDir()
	const pub1 = "PUB-V2\n"

	// v1: Sigil выключен.
	if err := Write(dir, validMaterial(t, "ca1")); err != nil {
		t.Fatalf("Write v1: %v", err)
	}
	// v2: Sigil включён (новый trust-anchor приехал в bootstrap-ротации).
	if err := Write(dir, materialWithSigil(t, "ca2", pub1)); err != nil {
		t.Fatalf("Write v2: %v", err)
	}
	assertCurrent(t, dir, "v2")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after v2: %v", err)
	}
	if string(got.SigilPubKeyPEM) != pub1 {
		t.Fatalf("after v2 SigilPubKeyPEM = %q; want %q", got.SigilPubKeyPEM, pub1)
	}

	// v3: Sigil снова выключен — активная версия не должна тащить старый anchor.
	if err := Write(dir, validMaterial(t, "ca3")); err != nil {
		t.Fatalf("Write v3: %v", err)
	}
	assertCurrent(t, dir, "v3")
	got, err = Load(dir)
	if err != nil {
		t.Fatalf("Load after v3: %v", err)
	}
	if got.SigilPubKeyPEM != nil {
		t.Fatalf("after v3 SigilPubKeyPEM = %q; want nil (disabled in v3)", got.SigilPubKeyPEM)
	}
}

// TestWriteAndLoad_MultiPEMSigil: набор якорей (конкатенация нескольких
// PEM-блоков, ADR-026(h) R3) пишется и читается как есть, и после round-trip-а
// ParseSigilPubKeys восстанавливает все ключи в исходном порядке.
func TestWriteAndLoad_MultiPEMSigil(t *testing.T) {
	dir := t.TempDir()
	pem1, want1 := ed25519PubPEM(t)
	pem2, want2 := ed25519PubPEM(t)
	concat := append(append([]byte{}, pem1...), pem2...)

	cert, key := keypair(t)
	m := &Material{CertPEM: cert, KeyPEM: key, CAPEM: []byte("CA"), SigilPubKeyPEM: concat}
	if err := Write(dir, m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got.SigilPubKeyPEM) != string(concat) {
		t.Fatalf("multi-PEM not round-tripped byte-for-byte")
	}

	keys, err := ParseSigilPubKeys(got.SigilPubKeyPEM)
	if err != nil {
		t.Fatalf("ParseSigilPubKeys after round-trip: %v", err)
	}
	if len(keys) != 2 || !keys[0].Equal(want1) || !keys[1].Equal(want2) {
		t.Fatalf("round-trip set mismatch: got %d keys", len(keys))
	}
}

// TestPathsIn_SigilPubKey: путь trust-anchor-а идёт через dir/current/.
func TestPathsIn_SigilPubKey(t *testing.T) {
	p := PathsIn("/var/lib/soul-stack/seed")
	if p.SigilPubKey != "/var/lib/soul-stack/seed/current/sigil_pubkey.pem" {
		t.Errorf("SigilPubKey = %q", p.SigilPubKey)
	}
}
