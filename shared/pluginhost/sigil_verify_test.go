package pluginhost

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// lookupStub — a minimal SigilLookup for tests. A nil result on a missing key models
// "the sigil didn't arrive".
type lookupStub map[string]*SigilRecord

func (l lookupStub) Get(ns, name string) *SigilRecord { return l[ns+"."+name] }

// signFixture, symmetric with keeper/internal/sigil.Signer.Sign, builds the signature
// over the same block with the same helpers (BuildSigilBlock + NormalizeManifestBytes).
// If verify and this function diverge, the compiler/test catches it: the helpers are
// shared, there's no second hashing implementation (S3↔S6 symmetry).
func signFixture(t *testing.T, priv ed25519.PrivateKey, ns, name, ref string, binDigestHex string, manifest []byte) []byte {
	t.Helper()
	binRaw, err := hex.DecodeString(binDigestHex)
	if err != nil {
		t.Fatalf("decode bin digest: %v", err)
	}
	manifestDigest := sha256.Sum256(NormalizeManifestBytes(manifest))
	block := BuildSigilBlock(ns, name, ref, binRaw, manifestDigest[:])
	return ed25519.Sign(priv, block)
}

// sigilTestEnv — a built plugin on disk + a matching valid SigilRecord.
type sigilTestEnv struct {
	dir      string
	binPath  string
	manifest *sharedplugin.Manifest
	rec      *SigilRecord
	pub      ed25519.PublicKey
}

func setupSigilEnv(t *testing.T) sigilTestEnv {
	t.Helper()
	const (
		ns       = "acme"
		name     = "x"
		ref      = "v1.0.0"
		manifest = "kind: soul_module\nnamespace: acme\nname: x\nprotocol_version: 1\n"
	)
	dir := t.TempDir()
	binPath := filepath.Join(dir, "soul-mod-x")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	binDigest, err := computeFileDigest(binPath)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	sig := signFixture(t, priv, ns, name, ref, binDigest, []byte(manifest))
	return sigilTestEnv{
		dir:     dir,
		binPath: binPath,
		manifest: &sharedplugin.Manifest{
			Kind: sharedplugin.KindSoulModule, ProtocolVersion: 1, Namespace: ns, Name: name,
		},
		rec: &SigilRecord{
			Namespace:       ns,
			Name:            name,
			Ref:             ref,
			BinarySHA256hex: binDigest,
			Signature:       sig,
			Manifest:        []byte(manifest),
		},
		pub: pub,
	}
}

func (e sigilTestEnv) host(t *testing.T, withRec bool) *Host {
	t.Helper()
	h, err := NewHost(nil, filepath.Join(t.TempDir(), "sock"))
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	h.SigilAnchors = NewAnchorSet([]ed25519.PublicKey{e.pub})
	look := lookupStub{}
	if withRec {
		look[e.rec.Namespace+"."+e.rec.Name] = e.rec
	}
	h.Sigils = look
	return h
}

func (e sigilTestEnv) discovered() Discovered {
	return Discovered{Manifest: e.manifest, BinaryPath: e.binPath, Dir: e.dir}
}

// asVerifyError extracts *VerifyError from a wrapped Spawn error.
func asVerifyError(t *testing.T, err error) *VerifyError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrSigilVerify) {
		t.Fatalf("error %v is not ErrSigilVerify", err)
	}
	var ve *VerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not *VerifyError", err)
	}
	return ve
}

// TestSigilVerifySuccess — a valid sigil + binary + manifest from transport: verify
// passes, the sidecar is sealed. Spawn then fails at handshake (the binary is a stub
// without handshake), but the integrity gate ran BEFORE exec.
func TestSigilVerifySuccess(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)

	// Spawn will fail after verify (no handshake), but the verify stage must not
	// produce a VerifyError — we check specifically for the absence of ErrSigilVerify.
	_, err := h.Spawn(context.Background(), e.discovered())
	if errors.Is(err, ErrSigilVerify) {
		t.Fatalf("verify must pass for valid sigil, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(e.dir, DigestSidecarName)); serr != nil {
		t.Fatalf("sidecar not sealed after verify-pass: %v", serr)
	}
}

func TestSigilVerifyNoSigil(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, false) // sigil didn't arrive

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonNoSigil {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoSigil)
	}
	if _, serr := os.Stat(filepath.Join(e.dir, DigestSidecarName)); !os.IsNotExist(serr) {
		t.Fatalf("sidecar must NOT be sealed on fail-closed, stat err = %v", serr)
	}
}

func TestSigilVerifyNoTrustAnchor(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	h.SigilAnchors = NewAnchorSet(nil) // empty anchor set: Sigil is off on the Keeper

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonNoTrustAnchor {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoTrustAnchor)
	}
}

// TestSigilVerifyNilAnchorHolder — a nil SigilAnchors holder (not set at all) is
// equivalent to an empty set: verify fails closed with no_trust_anchor (nil-safe
// snapshot). Covers backward compatibility of the old "nil pubkey".
func TestSigilVerifyNilAnchorHolder(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	h.SigilAnchors = nil

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonNoTrustAnchor {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoTrustAnchor)
	}
}

// TestSigilVerifyMultiAnchorOR — an OR loop over the anchor set (ADR-026(h)): the
// signature is issued by ONE key, but the set also contains foreign anchors. verify
// passes if the signing key is present in the set among the others (seamless rotation).
func TestSigilVerifyMultiAnchorOR(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)

	otherPub1, _, _ := ed25519.GenerateKey(nil)
	otherPub2, _, _ := ed25519.GenerateKey(nil)
	// Set: foreign, the signing key (e.pub), another foreign. Neither the order nor
	// the presence of foreign anchors should interfere — OR finds the signer.
	h.SigilAnchors = NewAnchorSet([]ed25519.PublicKey{otherPub1, e.pub, otherPub2})

	_, err := h.Spawn(context.Background(), e.discovered())
	if errors.Is(err, ErrSigilVerify) {
		t.Fatalf("verify must pass when signer is one of the anchors, got %v", err)
	}
}

// TestSigilVerifyMultiAnchorAllForeign — a non-empty set, but the signing key is NOT in
// it: no anchor verifies → bad_signature (fail-closed). This separates "empty set"
// (no_trust_anchor) from "anchors present, but not the right one" (bad_signature).
func TestSigilVerifyMultiAnchorAllForeign(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)

	f1, _, _ := ed25519.GenerateKey(nil)
	f2, _, _ := ed25519.GenerateKey(nil)
	h.SigilAnchors = NewAnchorSet([]ed25519.PublicKey{f1, f2})

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

func TestSigilVerifyDigestMismatch(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	// Tamper with the binary after the sigil was issued for the old hash.
	if err := os.WriteFile(e.binPath, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatalf("tamper bin: %v", err)
	}

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonDigestMismatch {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonDigestMismatch)
	}
}

func TestSigilVerifyBadSignatureManifestTampered(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	// The manifest in the record is tampered after signing — the manifest hash diverges
	// from the one in the signed block; the binary digest still matches.
	e.rec.Manifest = []byte("kind: soul_module\nnamespace: acme\nname: x\nprotocol_version: 1\nside_effects: true\n")

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

func TestSigilVerifyBadSignatureCorrupted(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	e.rec.Signature = make([]byte, ed25519.SignatureSize) // zero signature

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

func TestSigilVerifyRefTampered(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	// ref is part of the signed block — tampering with it breaks the signature.
	e.rec.Ref = "v9.9.9"

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

// TestSigilSymmetryBlockMatchesSign — the block that verify builds matches byte-for-byte
// the block that the Keeper Sign flow signs: both call BuildSigilBlock +
// NormalizeManifestBytes (shared code, not a second implementation).
func TestSigilSymmetryBlockMatchesSign(t *testing.T) {
	const (
		ns, name, ref = "core", "git", "v2.0.0"
		manifest      = "kind: soul_module\r\nnamespace: core\r\nname: git\r\n" // CRLF → normalized
	)
	binRaw := sha256.Sum256([]byte("binary-bytes"))
	binHex := hex.EncodeToString(binRaw[:])

	// Verify side.
	manDigest := sha256.Sum256(NormalizeManifestBytes([]byte(manifest)))
	verifyBlock := BuildSigilBlock(ns, name, ref, binRaw[:], manDigest[:])

	// Sign side reproduces exactly the same steps (like keeper Sign).
	signBinRaw, _ := hex.DecodeString(binHex)
	signManDigest := sha256.Sum256(NormalizeManifestBytes([]byte(manifest)))
	signBlock := BuildSigilBlock(ns, name, ref, signBinRaw, signManDigest[:])

	if string(verifyBlock) != string(signBlock) {
		t.Fatalf("verify block != sign block:\n verify=%x\n sign  =%x", verifyBlock, signBlock)
	}
}

// TestSigilReExecBySidecar — after a verify-pass, a subsequent Spawn from cache passes
// the integrity gate via the sidecar (re-exec defense-in-depth), even while the sigil is
// still valid. Verify re-checks the sidecar without recreating it.
func TestSigilReExecBySidecar(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)

	// First Spawn: verify-pass → seal.
	_, _ = h.Spawn(context.Background(), e.discovered())
	sidecar := filepath.Join(e.dir, DigestSidecarName)
	st1, err := os.Stat(sidecar)
	if err != nil {
		t.Fatalf("sidecar after first spawn: %v", err)
	}

	// Second Spawn: the sidecar already exists, verify checks it, doesn't fail at verify.
	_, err = h.Spawn(context.Background(), e.discovered())
	if errors.Is(err, ErrSigilVerify) {
		t.Fatalf("re-exec must pass integrity, got %v", err)
	}
	st2, err := os.Stat(sidecar)
	if err != nil {
		t.Fatalf("sidecar after second spawn: %v", err)
	}
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Errorf("sidecar rewritten on re-exec: mtime %v -> %v", st1.ModTime(), st2.ModTime())
	}
}
