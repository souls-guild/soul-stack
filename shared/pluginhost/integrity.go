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
)

// ErrPluginDigestMismatch — the plugin binary did not match the SHA-256 pinned
// in the sidecar. Signals binary tampering in the host cache after first load
// (security fix H2, docs/keeper/plugins.md → Integrity-model).
//
// Callers compare via errors.Is to distinguish tamper from other Spawn I/O errors.
var ErrPluginDigestMismatch = errors.New("pluginhost: plugin binary digest mismatch")

// DigestSidecarName is the sidecar file next to the plugin binary in the host
// cache. The leading dot doesn't hide it from a listing but visually separates it
// from the binary and manifest.yaml. Exported for the core.module.installed
// install-flow (ADR-065): on slot binary replacement the stale sidecar is removed
// before the atomic rename.
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

// verifySigilAndSeal — fail-closed verify of the plugin binary against the Sigil
// trust seal (ADR-026, slice S6b), replacing the first-load TOFU branch. Returns
// nil only if the plugin is allowed and integrity is confirmed; any failure →
// *VerifyError (errors.Is(err, ErrSigilVerify)) and the plugin does NOT start.
//
// Steps (normative order, symmetric to the Keeper-side Sign in keeper/internal/sigil):
//  1. binary digest from disk (binDigestHex);
//  2. lookup by (ns, name); rec == nil → fail-closed no_sigil (a Sigil that didn't
//     arrive = "not allowed", NOT "error → allow");
//  3. empty anchor set → fail-closed no_trust_anchor (Sigil not configured on Keeper);
//  4. compare the binary digest with the allowed hash (binary_sha256) → digest_mismatch;
//  5. manifest_sha256 = SHA-256(NormalizeManifestBytes(rec.Manifest)) — bytes FROM
//     TRANSPORT (M1), NOT the file on disk;
//  6. block = BuildSigilBlock(...) — the same helper Keeper uses at Sign (sign↔verify
//     symmetry guaranteed by the compiler, not a second implementation);
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
// ns/name come from the plugin manifest (d.Manifest.Namespace/.Name); ref from rec
// (operator-asserted, not checked against disk, single-slot). anchors — a snapshot of
// the trust-anchor set (ADR-026(h)); empty set → no_trust_anchor.
func verifySigilAndSeal(dir, binaryPath, namespace, name string, anchors []ed25519.PublicKey, sigils SigilLookup) error {
	binDigestHex, err := computeFileDigest(binaryPath)
	if err != nil {
		return err
	}

	var rec *SigilRecord
	if sigils != nil {
		rec = sigils.Get(namespace, name)
	}
	if err := verifyRecordAgainstDigest(binDigestHex, namespace, name, rec, anchors); err != nil {
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
func VerifyArtifactBytes(data []byte, rec *SigilRecord, anchors *AnchorSet) error {
	sum := sha256.Sum256(data)
	var namespace, name string
	if rec != nil {
		namespace, name = rec.Namespace, rec.Name
	}
	return verifyRecordAgainstDigest(hex.EncodeToString(sum[:]), namespace, name, rec, anchors.snapshot())
}

// verifyRecordAgainstDigest — the shared middle of verify (steps 2–7 of the
// normative order in [verifySigilAndSeal]): lookup result → anchors → digest compare
// → block signature. binDigestHex is the actual digest of the artifact under check
// (file or in-memory bytes).
func verifyRecordAgainstDigest(binDigestHex, namespace, name string, rec *SigilRecord, anchors []ed25519.PublicKey) error {
	if rec == nil {
		return verifyErrorFor(VerifyReasonNoSigil, namespace, name, "")
	}
	if len(anchors) == 0 {
		return verifyErrorFor(VerifyReasonNoTrustAnchor, namespace, name, rec.Ref)
	}

	binRaw, err := hex.DecodeString(rec.BinarySHA256hex)
	if err != nil {
		// Allowed hash isn't hex — the record is broken, fail-closed as mismatch
		// (the binary can't match an invalid reference).
		return verifyErrorFor(VerifyReasonDigestMismatch, namespace, name, rec.Ref)
	}
	actualRaw, err := hex.DecodeString(binDigestHex)
	if err != nil {
		// computeFileDigest always returns valid hex — defensive.
		return fmt.Errorf("pluginhost: decode binary digest %q: %w", binDigestHex, err)
	}
	if subtle.ConstantTimeCompare(binRaw, actualRaw) != 1 {
		return verifyErrorFor(VerifyReasonDigestMismatch, namespace, name, rec.Ref)
	}

	manifestDigest := sha256.Sum256(NormalizeManifestBytes(rec.Manifest))
	block := BuildSigilBlock(rec.Namespace, rec.Name, rec.Ref, binRaw, manifestDigest[:])
	if !verifyAnyAnchor(anchors, block, rec.Signature) {
		return verifyErrorFor(VerifyReasonBadSignature, namespace, name, rec.Ref)
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
