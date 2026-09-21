package sigil

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"testing"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
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
		arts := []pluginhost.SigilArtifact{{OS: "linux", Arch: "amd64", Path: "p", SHA256: h}}
		if _, err := s.Sign(testSource, sharedplugin.SourceKindArtifact, "v1", arts,
			[]byte(`{"kind":"ssh_provider"}`)); err == nil {
			t.Errorf("Sign accepted bad digest %q", h)
		}
	}
}

// A release with no artifact at all, and one naming a platform twice: neither can be
// signed. The first would be a seal over nothing; the second would leave the approved
// bytes depending on which row a reader happened to hit first.
func TestSign_RejectsUnsignableArtifactList(t *testing.T) {
	s, _ := newTestSigner(t)
	doc := []byte(`{"kind":"ssh_provider"}`)
	one := pluginhost.SigilArtifact{OS: "linux", Arch: "amd64", Path: "p", SHA256: hex.EncodeToString(sha256Of("bin"))}

	if _, err := s.Sign(testSource, sharedplugin.SourceKindArtifact, "v1", nil, doc); err == nil {
		t.Error("Sign accepted a release with no artifacts")
	}
	dup := []pluginhost.SigilArtifact{one, {OS: "linux", Arch: "amd64", Path: "q", SHA256: hex.EncodeToString(sha256Of("other"))}}
	if _, err := s.Sign(testSource, sharedplugin.SourceKindArtifact, "v1", dup, doc); err == nil {
		t.Error("Sign accepted a release declaring linux/amd64 twice")
	}
}

// An unknown source kind is refused before anything is signed: the kind tells a Soul
// where to fetch from, and a value nothing implements would be a signed instruction
// nobody can carry out.
func TestSign_RejectsUnknownKind(t *testing.T) {
	s, _ := newTestSigner(t)
	arts := gitArtifacts(hex.EncodeToString(sha256Of("bin")))
	if _, err := s.Sign(testSource, "torrent", "v1", arts, []byte(`{"kind":"ssh_provider"}`)); err == nil {
		t.Error("Sign accepted an unknown source kind")
	}
}

// TestSign_RejectsEmptyIdentityOrSchema — the three inputs without which a signature
// would mean nothing. An empty source leaves the grant with no identity at all (the
// artifact carries none); an empty schema would produce a valid seal over "this
// artifact discloses nothing".
func TestSign_RejectsEmptyIdentityOrSchema(t *testing.T) {
	s, _ := newTestSigner(t)
	arts := gitArtifacts(hex.EncodeToString(sha256Of("bin")))
	doc := []byte(`{"kind":"ssh_provider"}`)

	if _, err := s.Sign("", sharedplugin.SourceKindGit, "v1", arts, doc); err == nil {
		t.Error("Sign accepted an empty source")
	}
	if _, err := s.Sign(testSource, sharedplugin.SourceKindGit, "", arts, doc); err == nil {
		t.Error("Sign accepted an empty ref")
	}
	if _, err := s.Sign(testSource, sharedplugin.SourceKindGit, "v1", arts, nil); err == nil {
		t.Error("Sign accepted an empty schema document")
	}
}

// gitArtifacts is the one-row unplatformed list a kind=git grant carries.
func gitArtifacts(digestHex string) []pluginhost.SigilArtifact {
	return []pluginhost.SigilArtifact{{
		OS: pluginhost.AnyPlatform, Arch: pluginhost.AnyPlatform, SHA256: digestHex,
	}}
}

// Sign → Verify roundtrip: the signature validates against a block rebuilt from the
// same inputs (the path Soul takes at S6).
func TestSign_VerifyRoundtrip(t *testing.T) {
	s, pub := newTestSigner(t)

	const ref = "v1.0.0"
	binDigest := sha256.Sum256([]byte("the-plugin-binary"))
	binHex := hex.EncodeToString(binDigest[:])
	doc := []byte(`{"kind":"ssh_provider","protocol_version":1}`)

	arts := gitArtifacts(binHex)
	sig, err := s.Sign(testSource, sharedplugin.SourceKindGit, ref, arts, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature size = %d, want %d", len(sig), ed25519.SignatureSize)
	}

	// Recover the block exactly as the S6 verifier would.
	schemaDigest := pluginhost.SchemaDigest(doc)
	block, err := pluginhost.BuildSigilBlock(testSource, sharedplugin.SourceKindGit, ref, schemaDigest[:], arts)
	if err != nil {
		t.Fatalf("BuildSigilBlock: %v", err)
	}

	if !ed25519.Verify(pub, block, sig) {
		t.Fatal("Verify failed on honest block")
	}
	_ = binDigest
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
	arts := gitArtifacts(binHex)
	a, err := s.Sign(testSource, sharedplugin.SourceKindGit, ref, arts, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	b, err := s.Sign(testSource, sharedplugin.SourceKindGit, ref, arts, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if string(a) != string(b) {
		t.Error("the same (source, kind, ref, artifacts, schema) must yield the same signature")
	}
	c, err := s.Sign("https://example.com/other.git", sharedplugin.SourceKindGit, ref, arts, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if string(a) == string(c) {
		t.Error("a different source must yield a different signature")
	}
}

// TestSign_OldDomainSeparatorsDoNotVerify pins the DST bumps. The v1 block was keyed on
// (namespace, name); the v2 block carried ONE binary digest. A signature made over the
// v3 block must verify against neither, so no pre-NIM-377 and no pre-NIM-793 grant can
// be replayed into the new shape — which is what makes emptying the table at migration
// 119 the honest move rather than a convenience.
func TestSign_OldDomainSeparatorsDoNotVerify(t *testing.T) {
	s, pub := newTestSigner(t)

	const ref = "v1.0.0"
	binDigest := sha256.Sum256([]byte("the-plugin-binary"))
	binHex := hex.EncodeToString(binDigest[:])
	doc := []byte(`{"kind":"ssh_provider","protocol_version":1}`)
	schemaDigest := pluginhost.SchemaDigest(doc)

	sig, err := s.Sign(testSource, sharedplugin.SourceKindGit, ref, gitArtifacts(binHex), doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	lp := func(dst []byte, fields ...[]byte) []byte {
		for _, field := range fields {
			n := uint32(len(field))
			dst = append(dst, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
			dst = append(dst, field...)
		}
		return dst
	}
	// The v1 block by hand: DST v1, then LP(namespace) LP(name) LP(ref) LP(binary)
	// LP(manifest). This is what a pre-NIM-377 verifier computes.
	v1 := lp([]byte("soul-stack/sigil/v1"),
		[]byte("ssh"), []byte("hetzner"), []byte(ref), binDigest[:], schemaDigest[:])
	if ed25519.Verify(pub, v1, sig) {
		t.Fatal("a v3 signature verified against a v1-DST block — the version bump is not doing its job")
	}
	// The v2 block by hand: DST v2, then LP(source) LP(ref) LP(binary) LP(schema).
	// This is what a pre-NIM-793 verifier computes for the SAME artifact, so a grant
	// carried over the migration would have to fail here — and does.
	v2 := lp([]byte("soul-stack/sigil/v2"),
		[]byte(testSource), []byte(ref), binDigest[:], schemaDigest[:])
	if ed25519.Verify(pub, v2, sig) {
		t.Fatal("a v3 signature verified against a v2-DST block — the version bump is not doing its job")
	}
}

// Tampering with any block field breaks verify. Covers every signed Sigil field.
func TestSign_VerifyFailsOnTamper(t *testing.T) {
	s, pub := newTestSigner(t)

	const ref = "v1.0.0"
	binDigest := sha256.Sum256([]byte("orig-binary"))
	binHex := hex.EncodeToString(binDigest[:])
	doc := []byte(`{"kind":"ssh_provider","protocol_version":1}`)

	// A two-platform release, so "one row was swapped" is a case the fixture can
	// actually express.
	evilHex := hex.EncodeToString(sha256Of("evil-binary"))
	amd := pluginhost.SigilArtifact{OS: "linux", Arch: "amd64", Path: "p_amd64", SHA256: binHex}
	arm := pluginhost.SigilArtifact{OS: "linux", Arch: "arm64", Path: "p_arm64", SHA256: evilHex}
	honest := []pluginhost.SigilArtifact{amd, arm}

	sig, err := s.Sign(testSource, sharedplugin.SourceKindArtifact, ref, honest, doc)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	schemaDigest := pluginhost.SchemaDigest(doc)
	block := func(source, kind, r string, schema []byte, arts []pluginhost.SigilArtifact) []byte {
		b, berr := pluginhost.BuildSigilBlock(source, kind, r, schema, arts)
		if berr != nil {
			t.Fatalf("BuildSigilBlock: %v", berr)
		}
		return b
	}

	swappedDigest := []pluginhost.SigilArtifact{amd,
		{OS: "linux", Arch: "arm64", Path: "p_arm64", SHA256: hex.EncodeToString(sha256Of("substituted"))}}
	swappedPath := []pluginhost.SigilArtifact{amd,
		{OS: "linux", Arch: "arm64", Path: "elsewhere", SHA256: evilHex}}

	tampered := []struct {
		name  string
		block []byte
	}{
		{"source", block("https://evil.example/repo.git", sharedplugin.SourceKindArtifact, ref, schemaDigest[:], honest)},
		{"kind", block(testSource, sharedplugin.SourceKindGit, ref, schemaDigest[:], honest)},
		{"ref", block(testSource, sharedplugin.SourceKindArtifact, "v9.9.9", schemaDigest[:], honest)},
		{"schema_sha256", block(testSource, sharedplugin.SourceKindArtifact, ref, sha256Of(`{"kind":"evil"}`), honest)},
		// The three that only a list-valued block can be tested for: substituting ONE
		// platform's bytes, redirecting ONE platform's download, and dropping a row.
		{"one artifact's digest", block(testSource, sharedplugin.SourceKindArtifact, ref, schemaDigest[:], swappedDigest)},
		{"one artifact's path", block(testSource, sharedplugin.SourceKindArtifact, ref, schemaDigest[:], swappedPath)},
		{"a row dropped", block(testSource, sharedplugin.SourceKindArtifact, ref, schemaDigest[:], []pluginhost.SigilArtifact{amd})},
	}
	for _, tc := range tampered {
		if ed25519.Verify(pub, tc.block, sig) {
			t.Errorf("Verify succeeded on tampered %s field", tc.name)
		}
	}
	_ = binDigest
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
