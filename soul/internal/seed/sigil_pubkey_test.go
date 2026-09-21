package seed

import (
	"os"
	"path/filepath"
	"testing"
)

// materialWithSigil builds a consistent Material with the given Sigil trust anchor.
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

// TestWriteAndLoad_WithSigilPubKey: trust anchor is written to a version and
// read back (ADR-026, S2b). Symmetric with TestWriteAndLoad_RoundTrip.
func TestWriteAndLoad_WithSigilPubKey(t *testing.T) {
	dir := t.TempDir()
	const pub = "-----BEGIN PUBLIC KEY-----\nSIGILPUB\n-----END PUBLIC KEY-----\n"
	m := materialWithSigil(t, "CA", pub)
	if err := Write(dir, m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// sigil_pubkey.pem file is physically present in the active version.
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

// TestWriteAndLoad_SigilDisabled: an empty trust anchor (Sigil disabled) means
// no file is created; Load returns nil without ErrIncomplete. This is a valid
// state.
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

// TestWriteAndLoad_EmptySigilNotWritten: an explicit []byte("") is equivalent
// to nil — no file is written (Write checks len == 0).
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

// TestRotation_SigilPubKeyPerVersion: trust anchor switches atomically with
// the version. Rotating Sigil off → on and back is reflected in Load.
func TestRotation_SigilPubKeyPerVersion(t *testing.T) {
	dir := t.TempDir()
	const pub1 = "PUB-V2\n"

	// v1: Sigil disabled.
	if err := Write(dir, validMaterial(t, "ca1")); err != nil {
		t.Fatalf("Write v1: %v", err)
	}
	// v2: Sigil enabled (new trust anchor arrived via bootstrap rotation).
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

	// v3: Sigil disabled again — the active version must not carry the old anchor.
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

// TestWriteAndLoad_MultiPEMSigil: a set of anchors (concatenated PEM blocks,
// ADR-026(h) R3) is written and read back as-is, and after round-trip
// ParseSigilPubKeys recovers all keys in their original order.
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

// TestPathsIn_SigilPubKey: the trust-anchor path goes through dir/current/.
func TestPathsIn_SigilPubKey(t *testing.T) {
	p := PathsIn("/var/lib/soul-stack/seed")
	if p.SigilPubKey != "/var/lib/soul-stack/seed/current/sigil_pubkey.pem" {
		t.Errorf("SigilPubKey = %q", p.SigilPubKey)
	}
}
