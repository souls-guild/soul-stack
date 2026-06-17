package sigil

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"testing"

	"github.com/souls-guild/soul-stack/shared/pluginhost"
)

func newTestSigner(t *testing.T) (*Signer, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	s, err := NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s, pub
}

func TestNewSigner_RejectsBadSize(t *testing.T) {
	if _, err := NewSigner(ed25519.PrivateKey([]byte("short"))); err == nil {
		t.Fatal("NewSigner accepted undersized key")
	}
	if _, err := NewSigner(nil); err == nil {
		t.Fatal("NewSigner accepted nil key")
	}
}

func TestSign_RejectsBadDigestFormat(t *testing.T) {
	s, _ := newTestSigner(t)
	bad := []string{
		"",
		"deadbeef", // too short
		"ZZZZ" + "0000000000000000000000000000000000000000000000000000000000",   // non-hex
		"AB" + "00000000000000000000000000000000000000000000000000000000000000", // uppercase
	}
	for _, h := range bad {
		if _, err := s.Sign("ns", "name", "ref", h, []byte("manifest")); err == nil {
			t.Errorf("Sign accepted bad digest %q", h)
		}
	}
}

// Sign → Verify roundtrip: подпись валидна публичным ключом против блока,
// собранного из тех же входов (тот путь, что пройдёт Soul на S6).
func TestSign_VerifyRoundtrip(t *testing.T) {
	s, pub := newTestSigner(t)

	ns, name, ref := "cloud", "hetzner", "v1.0.0"
	binDigest := sha256.Sum256([]byte("the-plugin-binary"))
	binHex := hex.EncodeToString(binDigest[:])
	manifest := []byte("kind: cloud_driver\nname: hetzner\n")

	sig, err := s.Sign(ns, name, ref, binHex, manifest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature size = %d, want %d", len(sig), ed25519.SignatureSize)
	}

	// Восстанавливаем блок ровно так, как сделал бы verifier S6.
	manDigest := sha256.Sum256(pluginhost.NormalizeManifestBytes(manifest))
	block := pluginhost.BuildSigilBlock(ns, name, ref, binDigest[:], manDigest[:])

	if !ed25519.Verify(pub, block, sig) {
		t.Fatal("Verify failed on honest block")
	}
}

// Подделка любого поля блока ломает verify. Покрывает все поля Sigil.
func TestSign_VerifyFailsOnTamper(t *testing.T) {
	s, pub := newTestSigner(t)

	ns, name, ref := "cloud", "hetzner", "v1.0.0"
	binDigest := sha256.Sum256([]byte("orig-binary"))
	binHex := hex.EncodeToString(binDigest[:])
	manifest := []byte("kind: cloud_driver\n")

	sig, err := s.Sign(ns, name, ref, binHex, manifest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	manDigest := sha256.Sum256(pluginhost.NormalizeManifestBytes(manifest))

	tampered := []struct {
		name  string
		block []byte
	}{
		{"namespace", pluginhost.BuildSigilBlock("evil", name, ref, binDigest[:], manDigest[:])},
		{"name", pluginhost.BuildSigilBlock(ns, "evil", ref, binDigest[:], manDigest[:])},
		{"ref", pluginhost.BuildSigilBlock(ns, name, "v9.9.9", binDigest[:], manDigest[:])},
		{"binary_sha256", pluginhost.BuildSigilBlock(ns, name, ref, sha256Of("evil-binary"), manDigest[:])},
		{"manifest_sha256", pluginhost.BuildSigilBlock(ns, name, ref, binDigest[:], sha256Of("evil: manifest"))},
	}
	for _, tc := range tampered {
		if ed25519.Verify(pub, tc.block, sig) {
			t.Errorf("Verify succeeded on tampered %s field", tc.name)
		}
	}
}

// PublicKeyPEM → парс обратно (SPKI) → совпадает с priv.Public(). Это путь
// trust-anchor-а Soul-у в bootstrap (ADR-026, S6).
func TestPublicKeyPEM_RoundtripsToPublicKey(t *testing.T) {
	s, pub := newTestSigner(t)

	pemBytes, err := s.PublicKeyPEM()
	if err != nil {
		t.Fatalf("PublicKeyPEM: %v", err)
	}

	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("pem.Decode returned nil block")
	}
	if block.Type != "PUBLIC KEY" {
		t.Fatalf("pem block type = %q, want PUBLIC KEY", block.Type)
	}

	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("ParsePKIXPublicKey: %v", err)
	}
	got, ok := parsed.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("parsed key is %T, want ed25519.PublicKey", parsed)
	}
	if !got.Equal(pub) {
		t.Error("recovered public key does not equal signer's public key")
	}
	if !got.Equal(s.Public()) {
		t.Error("PublicKeyPEM disagrees with Signer.Public()")
	}
}

func sha256Of(s string) []byte {
	d := sha256.Sum256([]byte(s))
	return d[:]
}
