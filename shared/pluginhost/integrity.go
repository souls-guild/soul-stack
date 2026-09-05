package pluginhost

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// ErrPluginDigestMismatch — the plugin binary did not match the SHA-256 pinned
// in the sidecar. Signals binary tampering in the host cache after first load
// (security fix H2, docs/keeper/plugins.md → Integrity-model).
//
// Callers compare via errors.Is to distinguish tamper from other Spawn I/O errors.
var ErrPluginDigestMismatch = errors.New("pluginhost: plugin binary digest mismatch")

// DigestSidecarName is the sidecar file next to the artifact in the host cache. The
// leading dot both separates it visually from the artifact and keeps slot discovery
// from mistaking it for one (see artifactIn: a slot holds exactly one executable, and
// dot-files are not candidates). Exported for the core.module.installed install-flow
// (ADR-065): on slot artifact replacement the stale sidecar is removed before the
// atomic rename.
const DigestSidecarName = ".sha256"

// digestSidecarMode — the sidecar is written read-only: no writes are expected
// after first load, and protection against swapping the digest along with the
// binary rests on directory permissions (least-privilege service-user, see
// docs/keeper/plugins.md → Permissions).
const digestSidecarMode = 0o400

// computeFileDigest streams the file's SHA-256 (without reading it wholly into
// memory — plugin binaries can be tens of MB). Returns a lowercase hex string.
func computeFileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("pluginhost: open for digest %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("pluginhost: read for digest %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifySigilAndSeal — fail-closed verify of the artifact against the Sigil trust seal
// (ADR-026, slice S6b), replacing the first-load TOFU branch. Returns nil only if the
// artifact is allowed and integrity is confirmed; any failure → *VerifyError
// (errors.Is(err, ErrSigilVerify)) and the plugin does NOT start.
//
// Steps (normative order, symmetric to the Keeper-side Sign in keeper/internal/sigil):
//  1. artifact digest from disk (binDigestHex);
//  2. lookup by registration alias; rec == nil → fail-closed no_sigil (a Sigil that
//     didn't arrive = "not allowed", NOT "error → allow");
//  3. empty anchor set → fail-closed no_trust_anchor (Sigil not configured on Keeper);
//  4. select this host's artifact row from the grant ([SelectArtifact], NIM-793); no
//     row for the platform → no_artifact_for_platform, fail-closed. Then compare the
//     artifact digest with that row's approved hash → digest_mismatch.
//     This is the one real control: the operator approved a sha256 FOR THIS PLATFORM,
//     and no other bytes get to exec;
//  5. schema_sha256 = SchemaDigest(rec.Schema) — bytes FROM TRANSPORT (M1), NOT the
//     trailer on disk;
//  6. block = BuildSigilBlock(source, kind, ref, …) over the WHOLE artifact list — the
//     same helper Keeper uses at Sign (sign↔verify symmetry guaranteed by the compiler,
//     not a second implementation). The signature covers every row, so substituting one
//     platform's digest breaks the check for all of them;
//  7. OR loop over the anchor set (ADR-026(h), multi-anchor): the signature is valid
//     if ANY anchor in the set verifies it via ed25519.Verify → pass; none of a
//     non-empty set → bad_signature. OR semantics give seamless signing-key rotation
//     (old and new anchor coexist in the set);
//  8. OK → seal sidecar (for re-exec defense-in-depth) + nil.
//
// re-exec stays defense-in-depth: subsequent execs from the shared cache check the
// sidecar (see [Host.Spawn] calls again — the sidecar matches). But a missing sidecar
// no longer means "trust": it means "Sigil-verify needed", performed here every time.
//
// Verification is per ARTIFACT and knows nothing about modules: a grant approves
// bytes, and every module of an artifact is those same bytes. alias is the slot's
// registration; source/ref come from rec (operator-asserted, not checked against disk,
// single-slot). anchors — a snapshot of the trust-anchor set (ADR-026(h)); empty set →
// no_trust_anchor.
func verifySigilAndSeal(dir, binaryPath, alias string, anchors []ed25519.PublicKey, sigils SigilLookup) error {
	binDigestHex, err := computeFileDigest(binaryPath)
	if err != nil {
		return err
	}

	var rec *SigilRecord
	if sigils != nil {
		rec = sigils.Get(alias)
	}
	// The spawn path selects by the RUNNING binary's platform, and that is exact
	// rather than a guess: this process is about to exec the artifact, so the platform
	// is literally this one. The install path decides differently (see
	// [VerifyArtifactBytes]) because it is choosing what to download, not what to run.
	if err := verifyRecordAgainstDigest(binDigestHex, alias, rec, rec.Artifact(runtime.GOOS, runtime.GOARCH), anchors); err != nil {
		return err
	}

	// Verify passed → seal sidecar for re-exec defense-in-depth. If the sidecar
	// already exists (repeat Spawn from cache), compare the digest instead of writing.
	sidecarPath := filepath.Join(dir, DigestSidecarName)
	want, rerr := os.ReadFile(sidecarPath)
	switch {
	case rerr == nil:
		return verifyDigest(binaryPath, string(want))
	case errors.Is(rerr, os.ErrNotExist):
		return sealDigest(binaryPath, sidecarPath)
	default:
		return fmt.Errorf("pluginhost: read digest sidecar %q: %w", sidecarPath, rerr)
	}
}

// VerifyArtifactBytes — fail-closed Sigil-verify of DOWNLOADED artifact bytes
// BEFORE materializing to disk (ADR-065(f), core.module.installed install-flow).
// Same normative chain as [verifySigilAndSeal] (digest → signature over the block),
// but the input is in-memory bytes; no sidecar-seal is done — the file doesn't exist
// yet, the first Spawn after install seals it.
//
// approved is the grant row the CALLER selected and fetched — passed in rather than
// re-derived here, and that is the point. The install flow picks the row from the
// host's own Soulprint facts (ADR-018); this package can only see runtime.GOOS/GOARCH.
// Two derivations of one platform agree until either changes, and the disagreement
// would surface as digest_mismatch — the diagnostic for tampering, on a host that
// merely spells its architecture differently. One selection, checked here.
//
// nil approved is fail-closed as no_artifact_for_platform: the caller found no row,
// and there is nothing safe to substitute.
func VerifyArtifactBytes(data []byte, rec *SigilRecord, approved *SigilArtifact, anchors *AnchorSet) error {
	sum := sha256.Sum256(data)
	var alias string
	if rec != nil {
		alias = rec.Alias
	}
	return verifyRecordAgainstDigest(hex.EncodeToString(sum[:]), alias, rec, approved, anchors.snapshot())
}

// verifyRecordAgainstDigest — the shared middle of verify (steps 2–7 of the
// normative order in [verifySigilAndSeal]): lookup result → anchors → digest compare
// → block signature. binDigestHex is the actual digest of the artifact under check
// (file or in-memory bytes).
func verifyRecordAgainstDigest(binDigestHex, alias string, rec *SigilRecord, approved *SigilArtifact, anchors []ed25519.PublicKey) error {
	if rec == nil {
		return verifyErrorFor(VerifyReasonNoSigil, alias, "", "")
	}
	// A lookup must answer for the key it was asked about. Nothing downstream reads
	// the alias — it is a selector, not a claim — but a lookup that answered with
	// some OTHER registration's grant would move an approval between registrations
	// without anything noticing: the digest and signature would check out, against
	// bytes approved for a different alias. So the contract is enforced rather than
	// assumed, and a mis-wired adapter fails closed instead of silently widening.
	if rec.Alias != alias {
		return verifyErrorFor(VerifyReasonNoSigil, alias, "", "")
	}
	if len(anchors) == 0 {
		return verifyErrorFor(VerifyReasonNoTrustAnchor, alias, rec.Source, rec.Ref)
	}

	// The grant approves a release, so exactly one of its rows says which bytes are
	// approved here — and the caller has already decided which, because the caller is
	// the one that knows this host's platform.
	if approved == nil {
		return verifyErrorFor(VerifyReasonNoArtifactForPlatform, alias, rec.Source, rec.Ref)
	}

	binRaw, err := hex.DecodeString(approved.SHA256)
	if err != nil {
		// Approved hash isn't hex — the record is broken, fail-closed as mismatch
		// (the artifact can't match an invalid reference).
		return verifyErrorFor(VerifyReasonDigestMismatch, alias, rec.Source, rec.Ref)
	}
	actualRaw, err := hex.DecodeString(binDigestHex)
	if err != nil {
		// computeFileDigest always returns valid hex — defensive.
		return fmt.Errorf("pluginhost: decode artifact digest %q: %w", binDigestHex, err)
	}
	if subtle.ConstantTimeCompare(binRaw, actualRaw) != 1 {
		return verifyErrorFor(VerifyReasonDigestMismatch, alias, rec.Source, rec.Ref)
	}

	schemaDigest := SchemaDigest(rec.Schema)
	block, err := BuildSigilBlock(rec.Source, rec.Kind, rec.Ref, schemaDigest[:], rec.Artifacts)
	if err != nil {
		// The list the grant carries is not one a signature can exist over (empty,
		// duplicate platform, malformed digest). Nothing signed it, so treat it the
		// way an unsigned grant is treated rather than reporting a broken digest.
		return verifyErrorFor(VerifyReasonBadSignature, alias, rec.Source, rec.Ref)
	}
	if !verifyAnyAnchor(anchors, block, rec.Signature) {
		return verifyErrorFor(VerifyReasonBadSignature, alias, rec.Source, rec.Ref)
	}
	return nil
}

// verifyAnyAnchor — OR-check of the signature against the trust-anchor set
// (ADR-026(h), multi-anchor). Returns true if at least one anchor verifies the
// signature; iteration stops at the first success.
//
// Iteration is sequential (not constant-time in the anchor count): anchors are
// publicly-known public keys, not secret, and ed25519.Verify itself is constant-time
// with respect to the signature. Anchors number a few (primary + rotated), so the
// scan cost is negligible on the cold Spawn path.
func verifyAnyAnchor(anchors []ed25519.PublicKey, block, signature []byte) bool {
	for _, pub := range anchors {
		if len(pub) == 0 {
			continue
		}
		if ed25519.Verify(pub, block, signature) {
			return true
		}
	}
	return false
}

// verifyDigest compares the binary's actual digest with the expected one (from the
// sidecar). Surrounding whitespace/newlines in the sidecar are ignored.
func verifyDigest(binaryPath, wantRaw string) error {
	want := trimDigest(wantRaw)
	got, err := computeFileDigest(binaryPath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: %s (want %s, got %s)", ErrPluginDigestMismatch, binaryPath, want, got)
	}
	return nil
}

// sealDigest records the binary digest into the sidecar on first load. The write is
// atomic via temp-file + rename so concurrent Spawns don't see a half-written sidecar.
// If the sidecar appeared between check and write (a race of two first-load Spawns),
// switch to verify.
func sealDigest(binaryPath, sidecarPath string) error {
	digest, err := computeFileDigest(binaryPath)
	if err != nil {
		return err
	}

	dir := filepath.Dir(sidecarPath)
	tmp, err := os.CreateTemp(dir, ".sha256-*.tmp")
	if err != nil {
		return fmt.Errorf("pluginhost: create temp digest sidecar in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.WriteString(digest); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pluginhost: write temp digest sidecar %q: %w", tmpPath, err)
	}
	if err := tmp.Chmod(digestSidecarMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pluginhost: chmod temp digest sidecar %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pluginhost: close temp digest sidecar %q: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, sidecarPath); err != nil {
		return fmt.Errorf("pluginhost: seal digest sidecar %q: %w", sidecarPath, err)
	}
	return nil
}

// trimDigest strips surrounding whitespace/newlines from the sidecar content.
func trimDigest(s string) string {
	start, end := 0, len(s)
	for start < end && isSpaceByte(s[start]) {
		start++
	}
	for end > start && isSpaceByte(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
