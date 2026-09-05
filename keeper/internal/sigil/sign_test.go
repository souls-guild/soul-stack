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

// testSource is the git remote the fixtures pretend their artifact came from — with no
// self-name in the artifact, this and the ref are the whole signed identity.
const testSource = "https://example.com/soul-ssh-hetzner.git"

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
		if _, err := s.Sign(testSource, "v1", h, []byte(`{"kind":"ssh_provider"}`)); err == nil {
			t.Errorf("Sign accepted bad digest %q", h)
		}
	}
}

// TestSign_RejectsEmptyIdentityOrSchema — the three inputs without which a signature
// would mean nothing. An empty source leaves the grant with no identity at all (the
// artifact carries none); an empty schema would produce a valid seal over "this
// artifact discloses nothing".
func TestSign_RejectsEmptyIdentityOrSchema(t *testing.T) {
	s, _ := newTestSigner(t)
	binHex := hex.EncodeToString(sha256Of("bin"))
	doc := []byte(`{"kind":"ssh_provider"}`)

	if _, err := s.Sign("", "v1", binHex, doc); err == nil {
		t.Error("Sign accepted an empty source")
	}
	if _, err := s.Sign(testSource, "", binHex, doc); err == nil {
		t.Error("Sign accepted an empty ref")
	}
	if _, err := s.Sign(testSource, "v1", binHex, nil); err == nil {
		t.Error("Sign accepted an empty schema document")
	}
}

// Sign → Verify roundtrip: the signature validates against a block rebuilt from the
// same inputs (the path Soul takes at S6).
func TestSign_VerifyRoundtrip(t *testing.T) {
	s, pub := newTestSigner(t)

	const ref = "v1.0.0"
	binDigest := sha256.Sum256([]byte("the-plugin-binary"))
	binHex := hex.EncodeToString(binDigest[:])
	doc := []byte(`{"kind":"ssh_provider","protocol_version":1}`)

	sig, err := s.Sign(testSource, ref, binHex, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature size = %d, want %d", len(sig), ed25519.SignatureSize)
	}

	// Recover the block exactly as the S6 verifier would.
	schemaDigest := pluginhost.SchemaDigest(doc)
	block := pluginhost.BuildSigilBlock(testSource, ref, binDigest[:], schemaDigest[:])

	if !ed25519.Verify(pub, block, sig) {
		t.Fatal("Verify failed on honest block")
	}
}

// TestSign_AliasIsNotSigned is the guard for NIM-438. The alias must NOT reach the
// block: it is operator-chosen, so trust anchored to it could be moved by renaming it.
// Two grants over the same artifact under two aliases therefore carry the SAME
// signature, and a forged alias cannot reach a signature at all.
func TestSign_AliasIsNotSigned(t *testing.T) {
	s, _ := newTestSigner(t)

	const ref = "v1.0.0"
	binHex := hex.EncodeToString(sha256Of("the-plugin-binary"))
	doc := []byte(`{"kind":"soul_module","protocol_version":1}`)

	// Sign takes no alias at all — that is the property. What it does take is the
	// source, and changing THAT must change the signature.
	a, err := s.Sign(testSource, ref, binHex, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	b, err := s.Sign(testSource, ref, binHex, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if string(a) != string(b) {
		t.Error("the same (source, ref, digest, schema) must yield the same signature")
	}
	c, err := s.Sign("https://example.com/other.git", ref, binHex, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if string(a) == string(c) {
		t.Error("a different source must yield a different signature")
	}
}

// TestSign_V1DomainSeparatorDoesNotVerify pins the DST bump. The v1 block was keyed on
// (namespace, name) and tagged `soul-stack/sigil/v1`; a signature made over the v2
// block must not verify against a v1-shaped block, so no pre-NIM-377 grant can be
// replayed into the new identity model.
func TestSign_V1DomainSeparatorDoesNotVerify(t *testing.T) {
	s, pub := newTestSigner(t)

	const ref = "v1.0.0"
	binDigest := sha256.Sum256([]byte("the-plugin-binary"))
	binHex := hex.EncodeToString(binDigest[:])
	doc := []byte(`{"kind":"ssh_provider","protocol_version":1}`)
	schemaDigest := pluginhost.SchemaDigest(doc)

	sig, err := s.Sign(testSource, ref, binHex, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Rebuild the v1 block by hand: DST v1, then LP(namespace) LP(name) LP(ref)
	// LP(binary) LP(manifest). This is what a pre-NIM-377 verifier computes.
	v1 := []byte("soul-stack/sigil/v1")
	for _, field := range [][]byte{
		[]byte("ssh"), []byte("hetzner"), []byte(ref), binDigest[:], schemaDigest[:],
	} {
		n := uint32(len(field))
		v1 = append(v1, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		v1 = append(v1, field...)
	}
	if ed25519.Verify(pub, v1, sig) {
		t.Fatal("a v2 signature verified against a v1-DST block — the version bump is not doing its job")
	}
}

// Tampering with any block field breaks verify. Covers every signed Sigil field.
func TestSign_VerifyFailsOnTamper(t *testing.T) {
	s, pub := newTestSigner(t)

	const ref = "v1.0.0"
	binDigest := sha256.Sum256([]byte("orig-binary"))
	binHex := hex.EncodeToString(binDigest[:])
	doc := []byte(`{"kind":"ssh_provider","protocol_version":1}`)

	sig, err := s.Sign(testSource, ref, binHex, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	schemaDigest := pluginhost.SchemaDigest(doc)

	tampered := []struct {
		name  string
		block []byte
	}{
		{"source", pluginhost.BuildSigilBlock("https://evil.example/repo.git", ref, binDigest[:], schemaDigest[:])},
		{"ref", pluginhost.BuildSigilBlock(testSource, "v9.9.9", binDigest[:], schemaDigest[:])},
		{"binary_sha256", pluginhost.BuildSigilBlock(testSource, ref, sha256Of("evil-binary"), schemaDigest[:])},
		{"schema_sha256", pluginhost.BuildSigilBlock(testSource, ref, binDigest[:], sha256Of(`{"kind":"evil"}`))},
	}
	for _, tc := range tampered {
		if ed25519.Verify(pub, tc.block, sig) {
			t.Errorf("Verify succeeded on tampered %s field", tc.name)
		}
	}
}

// PublicKeyPEM → parse back (SPKI) → matches priv.Public(). This is the path
// trust-anchor takes to Soul in bootstrap (ADR-026, S6).
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
