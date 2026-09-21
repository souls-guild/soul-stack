package pluginhost

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// otherPlatform returns a (os, arch) pair that is not the one these tests run on, so a
// grant built from it is genuinely unapproved here rather than accidentally matching.
func otherPlatform() (string, string) {
	if runtime.GOOS == "linux" && runtime.GOARCH == "riscv64" {
		return "darwin", "amd64"
	}
	return "linux", "riscv64"
}

// releaseRecord builds a grant over `artifacts` for the given bytes, signed the way
// Keeper signs it, and returns the record with the anchor set that verifies it.
func releaseRecord(t *testing.T, artifacts []SigilArtifact) (*SigilRecord, *AnchorSet) {
	t.Helper()
	schemaDoc, err := schema.Marshal(soulModuleDoc(modDef("acl", nil, nil)))
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	digest := SchemaDigest(schemaDoc)
	block, err := BuildSigilBlock(testSource, sharedplugin.SourceKindArtifact, testRef, digest[:], artifacts)
	if err != nil {
		t.Fatalf("BuildSigilBlock: %v", err)
	}
	return &SigilRecord{
		Alias:     testAlias,
		Source:    testSource,
		Ref:       testRef,
		Kind:      sharedplugin.SourceKindArtifact,
		Artifacts: artifacts,
		Signature: ed25519.Sign(priv, block),
		Schema:    schemaDoc,
	}, NewAnchorSet([]ed25519.PublicKey{pub})
}

func digestHexOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hostRow is what a caller on THIS machine selects before verifying — the same pair
// the install flow forms, with the selection outside verify since NIM-795. nil when the
// release covers no row for this platform, which is the fail-closed case below.
func hostRow(rec *SigilRecord) *SigilArtifact {
	return SelectArtifact(rec.Artifacts, runtime.GOOS, runtime.GOARCH)
}

// ★ The acceptance guard, in the form of real code. A `kind: artifact` release with no
// row for this platform gives this host nothing to run, whatever the bytes are and
// whoever signed them.
//
// The reason is NOT a digest mismatch, and the distinction is the point: nothing was
// approved to compare against. An operator reading `digest_mismatch` would go looking
// for tampering; what they have to do is publish and re-approve a release that covers
// the platform.
func TestVerify_NoArtifactForPlatform(t *testing.T) {
	data := []byte("#!/bin/sh\nexit 0\n")
	goos, goarch := otherPlatform()
	rec, anchors := releaseRecord(t, []SigilArtifact{
		{OS: goos, Arch: goarch, Path: "redis_" + goos + "_" + goarch, SHA256: digestHexOf(data)},
	})

	err := VerifyArtifactBytes(data, rec, hostRow(rec), anchors)
	if !errors.Is(err, ErrSigilVerify) {
		t.Fatalf("err = %v, want a fail-closed verify error", err)
	}
	var ve *VerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *VerifyError", err)
	}
	if ve.Reason != VerifyReasonNoArtifactForPlatform {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoArtifactForPlatform)
	}
}

// The same release WITH a row for this platform verifies. Without this the test above
// would pass on a fixture that was broken for some unrelated reason.
func TestVerify_ArtifactForThisPlatformPasses(t *testing.T) {
	data := []byte("#!/bin/sh\nexit 0\n")
	goos, goarch := otherPlatform()
	rec, anchors := releaseRecord(t, []SigilArtifact{
		{OS: goos, Arch: goarch, Path: "elsewhere", SHA256: digestHexOf([]byte("other-platform-bytes"))},
		{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: "here", SHA256: digestHexOf(data)},
	})
	if err := VerifyArtifactBytes(data, rec, hostRow(rec), anchors); err != nil {
		t.Fatalf("VerifyArtifactBytes: %v", err)
	}
}

// This host's row selects this host's bytes and no others. Another platform's build is
// a digest mismatch here — approved, but not approved for this machine.
func TestVerify_AnotherPlatformsBytesAreRefused(t *testing.T) {
	mine := []byte("#!/bin/sh\nexit 0\n")
	theirs := []byte("#!/bin/sh\nexit 1\n")
	goos, goarch := otherPlatform()
	rec, anchors := releaseRecord(t, []SigilArtifact{
		{OS: goos, Arch: goarch, Path: "elsewhere", SHA256: digestHexOf(theirs)},
		{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: "here", SHA256: digestHexOf(mine)},
	})

	err := VerifyArtifactBytes(theirs, rec, hostRow(rec), anchors)
	var ve *VerifyError
	if !errors.As(err, &ve) || ve.Reason != VerifyReasonDigestMismatch {
		t.Fatalf("err = %v, want digest_mismatch on another platform's approved bytes", err)
	}
}

// ★ The signature covers the WHOLE list. Substituting one row — even a row for a
// platform this host will never select — breaks verification for the row it does
// select. That is what makes one approval cover a release rather than covering only
// whichever file the approving operator happened to be running on.
func TestVerify_SignatureCoversEveryArtifact(t *testing.T) {
	data := []byte("#!/bin/sh\nexit 0\n")
	goos, goarch := otherPlatform()
	other := SigilArtifact{OS: goos, Arch: goarch, Path: "elsewhere", SHA256: digestHexOf([]byte("theirs"))}
	mine := SigilArtifact{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: "here", SHA256: digestHexOf(data)}

	rec, anchors := releaseRecord(t, []SigilArtifact{other, mine})
	if err := VerifyArtifactBytes(data, rec, hostRow(rec), anchors); err != nil {
		t.Fatalf("honest release must verify: %v", err)
	}

	for name, tamper := range map[string]func(r *SigilRecord){
		"another platform's digest swapped": func(r *SigilRecord) {
			r.Artifacts = []SigilArtifact{
				{OS: other.OS, Arch: other.Arch, Path: other.Path, SHA256: digestHexOf([]byte("substituted"))},
				mine,
			}
		},
		"another platform's path redirected": func(r *SigilRecord) {
			r.Artifacts = []SigilArtifact{
				{OS: other.OS, Arch: other.Arch, Path: "https-elsewhere", SHA256: other.SHA256},
				mine,
			}
		},
		"another platform's row dropped": func(r *SigilRecord) {
			r.Artifacts = []SigilArtifact{mine}
		},
		"a row appended": func(r *SigilRecord) {
			r.Artifacts = []SigilArtifact{other, mine,
				{OS: "windows", Arch: "amd64", Path: "extra", SHA256: digestHexOf([]byte("extra"))}}
		},
		"the source kind flipped": func(r *SigilRecord) {
			r.Kind = sharedplugin.SourceKindGit
		},
	} {
		t.Run(name, func(t *testing.T) {
			tampered, _ := releaseRecord(t, []SigilArtifact{other, mine})
			tampered.Signature = rec.Signature
			tamper(tampered)

			err := VerifyArtifactBytes(data, tampered, hostRow(tampered), anchors)
			var ve *VerifyError
			if !errors.As(err, &ve) || ve.Reason != VerifyReasonBadSignature {
				t.Fatalf("err = %v, want bad_signature — the approval did not cover the whole release", err)
			}
		})
	}
}

// A grant carrying a list no signature could exist over is treated the way an unsigned
// grant is treated. It is not repaired into something plausible and it is not reported
// as a digest problem: nothing signed it.
func TestVerify_UnsignableArtifactListIsBadSignature(t *testing.T) {
	data := []byte("#!/bin/sh\nexit 0\n")
	rec, anchors := releaseRecord(t, []SigilArtifact{
		{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: "here", SHA256: digestHexOf(data)},
	})
	// One platform, two rows: which one is "the approved bytes" has no answer.
	rec.Artifacts = append(rec.Artifacts, SigilArtifact{
		OS: runtime.GOOS, Arch: runtime.GOARCH, Path: "also-here", SHA256: digestHexOf(data),
	})

	err := VerifyArtifactBytes(data, rec, hostRow(rec), anchors)
	var ve *VerifyError
	if !errors.As(err, &ve) || ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("err = %v, want bad_signature on an unsignable list", err)
	}
}

// --- the SPAWN path selects a platform too, and nothing else covered it ---
//
// Every other spawn fixture in this package uses a git grant: ONE unplatformed row,
// which [SelectArtifact] hands back for any host through its fallback. That makes the
// spawn path's platform decision invisible — swap `runtime.GOOS/GOARCH` for `"", ""`
// and the whole suite still passes. These two tests are the only thing standing on it.
//
// Spawn is where the platform is least in doubt: this process is about to exec the
// artifact, so "this host" is literally this binary. It is therefore the one caller
// that SHOULD re-derive rather than be handed a row — the install path is handed one
// because it is choosing what to download, from facts about a host it manages.

// A platformed release whose row for THIS host matches the bytes on disk passes verify.
//
// Asserting "not ErrSigilVerify" alone would also hold if verify were skipped outright,
// so the assertion is on the SIDE EFFECT that only a PASSING verify produces: the digest
// sidecar. verifySigilAndSeal writes it after the checks and before returning, so its
// presence is the one observable that separates "verified" from "never ran". (Spawn
// itself still fails afterwards — the fixture artifact does not speak the handshake.)
func TestSpawn_PlatformedGrantForThisHostVerifies(t *testing.T) {
	e := setupPlatformedSigilEnv(t, runtime.GOOS, runtime.GOARCH)
	h := e.host(t, true)

	if _, err := h.Spawn(context.Background(), e.discovered); errors.Is(err, ErrSigilVerify) {
		t.Fatalf("a release covering this platform failed verify: %v", err)
	}
	sidecar := filepath.Join(e.dir, DigestSidecarName)
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("no digest sidecar: verify did not run to completion (%v)", err)
	}
}

// The same release built for somebody else refuses on the SPAWN path — and as
// no_artifact_for_platform, not digest_mismatch. Nothing was approved to run here.
func TestSpawn_PlatformedGrantForAnotherHostIsRefused(t *testing.T) {
	goos, goarch := otherPlatform()
	e := setupPlatformedSigilEnv(t, goos, goarch)
	h := e.host(t, true)

	_, err := h.Spawn(context.Background(), e.discovered)
	ve := asVerifyError(t, err)
	if ve.Reason != VerifyReasonNoArtifactForPlatform {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoArtifactForPlatform)
	}
}

// setupPlatformedSigilEnv is [setupSigilEnv] with an ARTIFACT-kind grant whose single
// row names (goos, goarch) — so the caller decides whether this host is covered.
func setupPlatformedSigilEnv(t *testing.T, goos, goarch string) sigilTestEnv {
	t.Helper()
	e := setupSigilEnv(t)
	e.rec.Kind = sharedplugin.SourceKindArtifact
	e.rec.Artifacts = []SigilArtifact{{
		OS: goos, Arch: goarch, Path: "redis_" + goos + "_" + goarch,
		SHA256: e.discovered.Digest,
	}}
	digest := SchemaDigest(e.rec.Schema)
	block, err := BuildSigilBlock(e.rec.Source, e.rec.Kind, e.rec.Ref, digest[:], e.rec.Artifacts)
	if err != nil {
		t.Fatalf("BuildSigilBlock: %v", err)
	}
	e.rec.Signature = ed25519.Sign(e.priv, block)
	return e
}
