package pluginhost

import (
	"errors"
	"fmt"
)

// Verify replaces TOFU with Sigil (ADR-026, slice S6b). Before this slice, the first
// load of a plugin binary was trusted "as is" (TOFU) — a closed first-load gap: an
// unsigned malicious binary got control. Here the TOFU branch is replaced with a
// fail-closed verify against the Sigil trust seal delivered to the Soul by broadcast
// from the Keeper.
//
// This file holds the verify DI contract by which shared/pluginhost gets grants and
// the trust anchor WITHOUT depending on keeper-proto: the narrow [SigilRecord] + the
// [SigilLookup] interface. The Soul-side adapter (soul/) maps keeperv1.PluginSigil →
// SigilRecord without dragging proto/gen/go/keeper/v1 into shared.

// SigilRecord is the verify DTO of one Sigil trust seal in the form shared/pluginhost
// needs for verification. A narrow projection of keeperv1.PluginSigil: shared does
// NOT import keeper-proto, the Soul-side adapter fills this struct.
//
// Fields are symmetric to the signed block [BuildSigilBlock]:
//   - Namespace / Name / Ref — grant identity (Ref is operator-asserted, not checked
//     against disk, included in the signed block);
//   - BinarySHA256hex — the allowed binary hash (64 lowercase hex chars), checked
//     against the actual digest of the binary on disk;
//   - Signature — raw bytes of the block's ed25519 signature (64 bytes);
//   - Manifest — RAW manifest.yaml bytes from transport (M1), which verify runs
//     through [NormalizeManifestBytes] before hashing (S3↔S6 invariant: NOT the file
//     from disk, otherwise the hash diverges from the Keeper's signature).
type SigilRecord struct {
	Namespace       string
	Name            string
	Ref             string
	BinarySHA256hex string
	Signature       []byte
	Manifest        []byte
}

// SigilLookup is the read surface for the active grant by (namespace, name).
// Single-slot: exactly one active Sigil is allowed per pair (ADR-026(g)), so the key
// has no ref. Implemented by the Soul-side adapter over the runtime Sigil cache
// (soul/internal/sigilcache); a nil result = no grant → verify fail-closed (reason
// no_sigil).
type SigilLookup interface {
	Get(namespace, name string) *SigilRecord
}

// VerifyReason is a machine-distinguishable reason for a Sigil-verify failure
// (ADR-026, event plugin.verify_failed). Every value → fail-closed: the plugin does
// NOT run (G-sigil-5, without an allow-TOFU flag).
type VerifyReason string

const (
	// VerifyReasonNoSigil — the grant for (namespace, name) did not reach the Soul
	// (rec == nil). NOT "error → allow": an ungranted plugin = "not granted", must
	// not run.
	VerifyReasonNoSigil VerifyReason = "no_sigil"
	// VerifyReasonNoTrustAnchor — the Soul has no Sigil trust anchor (pubkey nil):
	// Sigil is not configured on the Keeper, nothing to verify the signature with.
	VerifyReasonNoTrustAnchor VerifyReason = "no_trust_anchor"
	// VerifyReasonDigestMismatch — the actual digest of the binary on disk did not
	// match the allowed hash (binary_sha256 in the Sigil).
	VerifyReasonDigestMismatch VerifyReason = "digest_mismatch"
	// VerifyReasonBadSignature — the Sigil signature failed verification by the trust
	// anchor (manifest/binary/ref tampered with, or key rotation without recreating
	// the grant).
	VerifyReasonBadSignature VerifyReason = "bad_signature"
)

// ErrSigilVerify is a sentinel wrapping any fail-closed Sigil-verify failure. Callers
// distinguish tamper/no-trust from other Spawn I/O errors via
// errors.Is(err, ErrSigilVerify); the specific reason comes via
// errors.As(err, &*VerifyError) and the [VerifyError.Reason] field.
var ErrSigilVerify = errors.New("pluginhost: sigil verification failed")

// VerifyError is a detailed Sigil-verify failure: reason + an actionable message for
// the operator (event plugin.verify_failed, ADR-026). Wraps [ErrSigilVerify] for
// errors.Is.
type VerifyError struct {
	// Reason — machine-distinguishable reason (for metrics/logs/tests).
	Reason VerifyReason
	// Namespace / Name — address of the plugin that failed verify.
	Namespace string
	Name      string
	// Hint — a human-readable actionable hint for the operator.
	Hint string
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("%s: %s.%s [%s]: %s", ErrSigilVerify.Error(), e.Namespace, e.Name, e.Reason, e.Hint)
}

func (e *VerifyError) Unwrap() error { return ErrSigilVerify }

// verifyErrorFor builds a [VerifyError] with an actionable hint for each reason.
// ns/name/ref are printed in the hint so the operator can copy the grant command
// without guessing.
func verifyErrorFor(reason VerifyReason, namespace, name, ref string) *VerifyError {
	var hint string
	switch reason {
	case VerifyReasonNoSigil:
		hint = fmt.Sprintf("плагин (%s, %s) не допущен; выполните `keeper.plugin.allow ns=%s name=%s ref=<ref>`",
			namespace, name, namespace, name)
	case VerifyReasonNoTrustAnchor:
		hint = "Sigil не настроен на Keeper (нет trust-anchor для verify подписи плагинов)"
	case VerifyReasonDigestMismatch:
		hint = "бинарь плагина не совпадает с допущенным хешем (подмена бинаря или устаревший допуск)"
	case VerifyReasonBadSignature:
		hint = fmt.Sprintf("подпись допуска недействительна (ротация ключа подписи? пересоздайте допуск: `keeper.plugin.allow ns=%s name=%s ref=%s`)",
			namespace, name, ref)
	default:
		hint = "Sigil-verify не пройден"
	}
	return &VerifyError{Reason: reason, Namespace: namespace, Name: name, Hint: hint}
}
