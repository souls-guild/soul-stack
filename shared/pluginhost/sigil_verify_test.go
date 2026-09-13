package pluginhost

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// lookupStub — a minimal SigilLookup for tests. A nil result on a missing key models
// "the sigil didn't arrive".
type lookupStub map[string]*SigilRecord

func (l lookupStub) Get(_ context.Context, alias string) (*SigilRecord, error) { return l[alias], nil }

// lookupErrStub is a lookup that cannot answer — the Keeper's registry read failing,
// not a grant that is absent (NIM-814).
type lookupErrStub struct{ err error }

func (l lookupErrStub) Get(context.Context, string) (*SigilRecord, error) { return nil, l.err }

// signFixture, symmetric with keeper/internal/sigil.Signer.Sign, builds the signature
// over the same block with the same helpers (BuildSigilBlock + SchemaDigest). If
// verify and this function diverge, the compiler/test catches it: the helpers are
// shared, there's no second hashing implementation (S3↔S6 symmetry).
func signFixture(t testing.TB, priv ed25519.PrivateKey, source, kind, ref string, artifacts []SigilArtifact, schemaDoc []byte) []byte {
	t.Helper()
	schemaDigest := SchemaDigest(schemaDoc)
	block, err := BuildSigilBlock(source, kind, ref, schemaDigest[:], artifacts)
	if err != nil {
		t.Fatalf("BuildSigilBlock: %v", err)
	}
	return ed25519.Sign(priv, block)
}

// gitArtifacts is the one-row unplatformed list a kind=git grant carries: the
// repository publishes one binary in dist/ and states no platform for it.
func gitArtifacts(digestHex string) []SigilArtifact {
	return []SigilArtifact{{OS: AnyPlatform, Arch: AnyPlatform, SHA256: digestHex}}
}

// sigilTestEnv — a stamped artifact on disk + a matching valid SigilRecord.
type sigilTestEnv struct {
	dir        string
	binPath    string
	discovered Discovered
	rec        *SigilRecord
	pub        ed25519.PublicKey
	// priv is held so a test can re-sign after editing the grant — the block covers
	// the artifact list, so an edited record without a fresh signature would fail as
	// bad_signature and stop testing what it names.
	priv ed25519.PrivateKey
}

const (
	testAlias  = "redis"
	testSource = "https://github.com/souls-guild/redis"
	testRef    = "v1.0.0"
)

func setupSigilEnv(t *testing.T) sigilTestEnv {
	t.Helper()
	return setupSigilEnvPadded(t, 0)
}

// setupSigilEnvPadded is [setupSigilEnv] with an artifact of a chosen size. The pad is
// a shell comment, so the fixture stays an executable script; it exists so a test can
// make the cost of hashing the artifact large enough to measure (NIM-818).
func setupSigilEnvPadded(t testing.TB, pad int) sigilTestEnv {
	t.Helper()
	dir := t.TempDir()
	doc := soulModuleDoc(modDef("acl", nil, nil))
	script := exitScript
	if pad > 0 {
		script += "# " + strings.Repeat("x", pad) + "\n"
	}
	binPath := writeArtifact(t, dir, testAlias, doc, script)

	found, warns := DiscoverSlot(testAlias, dir)
	if len(warns) != 0 || len(found) != 1 {
		t.Fatalf("discover fixture: found=%d warns=%v", len(found), warns)
	}
	schemaDoc, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return sigilTestEnv{
		dir:        dir,
		binPath:    binPath,
		discovered: found[0],
		rec: &SigilRecord{
			Alias:     testAlias,
			Source:    testSource,
			Ref:       testRef,
			Kind:      sharedplugin.SourceKindGit,
			Artifacts: gitArtifacts(found[0].Digest),
			Signature: signFixture(t, priv, testSource, sharedplugin.SourceKindGit, testRef,
				gitArtifacts(found[0].Digest), schemaDoc),
			Schema: schemaDoc,
		},
		pub:  pub,
		priv: priv,
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
		look[e.rec.Alias] = e.rec
	}
	h.Sigils = look
	return h
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

// TestSigilVerifySuccess — a valid sigil + artifact + schema from transport: verify
// passes, the sidecar is sealed. Spawn then fails at handshake (the fixture exits
// without one), but the integrity gate ran BEFORE exec.
func TestSigilVerifySuccess(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)

	_, err := h.Spawn(context.Background(), e.discovered)
	if errors.Is(err, ErrSigilVerify) {
		t.Fatalf("verify must pass for valid sigil, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(e.dir, DigestSidecarName)); serr != nil {
		t.Fatalf("sidecar not sealed after verify-pass: %v", serr)
	}
}

// The grant is looked up by the REGISTRATION ALIAS: it is the only identity a host
// holds, since the artifact carries none and the source it was signed under is not on
// disk. A grant filed under another alias is no grant at all.
func TestSigilVerifyLookupIsByAlias(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, false)
	h.Sigils = lookupStub{"some-other-alias": e.rec}

	_, err := h.Spawn(context.Background(), e.discovered)
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonNoSigil {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoSigil)
	}
}

func TestSigilVerifyNoSigil(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, false) // sigil didn't arrive

	_, err := h.Spawn(context.Background(), e.discovered)
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

	_, err := h.Spawn(context.Background(), e.discovered)
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonNoTrustAnchor {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoTrustAnchor)
	}
}

// TestSigilVerifyNilAnchorHolder — a nil SigilAnchors holder (not set at all) is
// equivalent to an empty set: verify fails closed with no_trust_anchor (nil-safe
// snapshot).
func TestSigilVerifyNilAnchorHolder(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	h.SigilAnchors = nil

	_, err := h.Spawn(context.Background(), e.discovered)
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
	h.SigilAnchors = NewAnchorSet([]ed25519.PublicKey{otherPub1, e.pub, otherPub2})

	_, err := h.Spawn(context.Background(), e.discovered)
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

	_, err := h.Spawn(context.Background(), e.discovered)
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

// GUARD: the one real control. The operator approved a sha256; an artifact whose bytes
// differ does not exec, whatever else is in order.
func TestSigilVerifyDigestMismatch(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	// Swap the artifact after the sigil was issued for the old hash, keeping a valid
	// trailer so nothing else about the slot looks wrong.
	writeArtifact(t, e.dir, testAlias, soulModuleDoc(modDef("acl", nil, nil)), "#!/bin/sh\necho pwned\nexit 0\n")

	_, err := h.Spawn(context.Background(), e.discovered)
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonDigestMismatch {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonDigestMismatch)
	}
}

// The schema is what the operator approved, so tampering with it after signing breaks
// the seal even though the artifact bytes still match.
func TestSigilVerifyBadSignatureSchemaTampered(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	e.rec.Schema = append(e.rec.Schema, ' ')

	_, err := h.Spawn(context.Background(), e.discovered)
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

func TestSigilVerifyBadSignatureCorrupted(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	e.rec.Signature = make([]byte, ed25519.SignatureSize) // zero signature

	_, err := h.Spawn(context.Background(), e.discovered)
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

func TestSigilVerifyRefTampered(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	// ref is part of the signed block — tampering with it breaks the signature.
	e.rec.Ref = "v9.9.9"

	_, err := h.Spawn(context.Background(), e.discovered)
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

// The source is the signed identity: claiming the artifact came from somewhere else
// breaks the seal.
func TestSigilVerifySourceTampered(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	e.rec.Source = "https://evil.example.com/redis"

	_, err := h.Spawn(context.Background(), e.discovered)
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

// The alias is NOT signed, so re-filing the same grant under a second alias verifies
// unchanged — the same bytes registered twice, one signature.
func TestSigilVerifySecondAliasNeedsNoNewSignature(t *testing.T) {
	e := setupSigilEnv(t)

	second := t.TempDir()
	writeArtifact(t, second, "artifact", soulModuleDoc(modDef("acl", nil, nil)), exitScript)
	found, warns := DiscoverSlot("redis-community", second)
	if len(warns) != 0 || len(found) != 1 {
		t.Fatalf("discover second slot: found=%d warns=%v", len(found), warns)
	}

	renamed := *e.rec
	renamed.Alias = "redis-community"
	h := e.host(t, false)
	h.Sigils = lookupStub{"redis-community": &renamed}

	_, err := h.Spawn(context.Background(), found[0])
	if errors.Is(err, ErrSigilVerify) {
		t.Fatalf("the same artifact under a second alias must verify, got %v", err)
	}
}

// TestSigilSymmetryBlockMatchesSign — the block that verify builds matches byte-for-byte
// the block the Keeper Sign flow signs: both call BuildSigilBlock + SchemaDigest
// (shared code, not a second implementation).
func TestSigilSymmetryBlockMatchesSign(t *testing.T) {
	const source, ref = "https://example.com/git", "v2.0.0"
	schemaDoc, err := schema.Marshal(soulModuleDoc(modDef("clone", nil, nil)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	binRaw := SchemaDigest([]byte("artifact-bytes"))
	binHex := hex.EncodeToString(binRaw[:])
	arts := gitArtifacts(binHex)

	// Verify side.
	verifyDigest := SchemaDigest(schemaDoc)
	verifyBlock, err := BuildSigilBlock(source, sharedplugin.SourceKindGit, ref, verifyDigest[:], arts)
	if err != nil {
		t.Fatalf("verify block: %v", err)
	}

	// Sign side reproduces exactly the same steps (like keeper Sign).
	signDigest := SchemaDigest(schemaDoc)
	signBlock, err := BuildSigilBlock(source, sharedplugin.SourceKindGit, ref, signDigest[:], arts)
	if err != nil {
		t.Fatalf("sign block: %v", err)
	}

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
	_, _ = h.Spawn(context.Background(), e.discovered)
	sidecar := filepath.Join(e.dir, DigestSidecarName)
	st1, err := os.Stat(sidecar)
	if err != nil {
		t.Fatalf("sidecar after first spawn: %v", err)
	}

	// Second Spawn: the sidecar already exists, verify checks it, doesn't fail at verify.
	_, err = h.Spawn(context.Background(), e.discovered)
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

// GUARD: a lookup that answers with another registration's grant is refused, even
// though that grant is otherwise perfectly valid — right signature, right digest, and
// the artifact on disk is exactly the bytes it approves. Only the alias disagrees, and
// that is enough: an approval belongs to the registration it was issued for.
func TestSigilVerifyRefusesGrantFiledUnderAnotherAlias(t *testing.T) {
	e := setupSigilEnv(t)

	// Same artifact, same source, same signature — only Alias differs from the key
	// the host will ask for. A mis-wired adapter looks exactly like this.
	misfiled := *e.rec
	misfiled.Alias = "redis-community"

	h := e.host(t, false)
	h.Sigils = lookupStub{testAlias: &misfiled}

	_, err := h.Spawn(context.Background(), e.discovered)
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonNoSigil {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoSigil)
	}
	if _, serr := os.Stat(filepath.Join(e.dir, DigestSidecarName)); !os.IsNotExist(serr) {
		t.Fatalf("sidecar sealed for a misfiled grant, stat err = %v", serr)
	}
}
