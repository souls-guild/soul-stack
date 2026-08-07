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
// The record carries two identities, and the split is the point:
//
//   - Alias is the operator's registration, and the runtime LOOKUP key. It is NOT in
//     the signed block. A host holds a slot named by the alias and nothing else that
//     could find the grant, and re-registering the same bytes under a second alias
//     must not need a second signature.
//   - Source and Ref are what was actually SIGNED. The artifact carries no self-name,
//     so where it came from is the only identity a signature can be over. Ref is
//     operator-asserted and not checked against disk.
//
// The remaining fields are the seal itself:
//   - BinarySHA256hex — the approved artifact hash (64 lowercase hex chars), checked
//     against the actual digest. This is the one real control on the spawn path;
//   - Signature — raw bytes of the block's ed25519 signature (64 bytes);
//   - Schema — the canonical schema-document bytes from transport (M1), hashed via
//     [SchemaDigest]: NOT the trailer read from disk, otherwise the hash could diverge
//     from what Keeper signed while still looking self-consistent.
type SigilRecord struct {
	Alias           string
	Source          string
	Ref             string
	BinarySHA256hex string
	Signature       []byte
	Schema          []byte
}

// SigilLookup is the read surface for the active grant by registration alias.
// Single-slot: exactly one active Sigil is allowed per alias (ADR-026(g)), so the key
// has no ref. Implemented by the Soul-side adapter over the runtime Sigil cache
// (soul/internal/sigilcache); a nil result = no grant → verify fail-closed (reason
// no_sigil).
//
// An implementation MUST return either nil or a record whose [SigilRecord.Alias]
// equals the requested alias. Verify enforces this rather than trusting it: a lookup
// answering with another registration's grant would hand an approval to the wrong
// alias, and every later check would pass against bytes nobody approved for THIS one.
type SigilLookup interface {
	Get(alias string) *SigilRecord
}

// VerifyReason is a machine-distinguishable reason for a Sigil-verify failure
// (ADR-026, event plugin.verify_failed). Every value → fail-closed: the plugin does
// NOT run (G-sigil-5, without an allow-TOFU flag).
type VerifyReason string

const (
	// VerifyReasonNoSigil — the grant for this alias did not reach the Soul
	// (rec == nil). NOT "error → allow": an ungranted plugin = "not granted", must
	// not run.
	VerifyReasonNoSigil VerifyReason = "no_sigil"
	// VerifyReasonNoTrustAnchor — the Soul has no Sigil trust anchor (pubkey nil):
	// Sigil is not configured on the Keeper, nothing to verify the signature with.
	VerifyReasonNoTrustAnchor VerifyReason = "no_trust_anchor"
	// VerifyReasonDigestMismatch — the actual digest of the artifact on disk did not
	// match the approved hash (binary_sha256 in the Sigil).
	VerifyReasonDigestMismatch VerifyReason = "digest_mismatch"
	// VerifyReasonBadSignature — the Sigil signature failed verification by the trust
	// anchor (schema/artifact/source/ref tampered with, or key rotation without
	// recreating the grant).
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
	// Alias — the registration the host was spawning, always known.
	Alias string
	// Source — the artifact source from the grant. Empty on no_sigil: with no grant
	// there is nothing that says where the artifact should have come from.
	Source string
	// Hint — a human-readable actionable hint for the operator.
	Hint string
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("%s: %s [%s]: %s", ErrSigilVerify.Error(), e.Alias, e.Reason, e.Hint)
}

func (e *VerifyError) Unwrap() error { return ErrSigilVerify }

// verifyErrorFor builds a [VerifyError] with an actionable hint for each reason.
// alias/source/ref are printed in the hint so the operator can copy the grant command
// without guessing.
func verifyErrorFor(reason VerifyReason, alias, source, ref string) *VerifyError {
	var hint string
	switch reason {
	case VerifyReasonNoSigil:
		hint = fmt.Sprintf("plugin %q is not allowed; run `keeper.plugin.allow alias=%s source=<source> ref=<ref>`",
			alias, alias)
	case VerifyReasonNoTrustAnchor:
		hint = "Sigil is not configured on Keeper (no trust-anchor to verify plugin signatures)"
	case VerifyReasonDigestMismatch:
		hint = "artifact does not match the approved hash (binary substitution or a stale allow)"
	case VerifyReasonBadSignature:
		hint = fmt.Sprintf("allow signature is invalid (signing key rotated? recreate the allow: `keeper.plugin.allow alias=%s source=%s ref=%s`)",
			alias, source, ref)
	default:
		hint = "Sigil-verify failed"
	}
	return &VerifyError{Reason: reason, Alias: alias, Source: source, Hint: hint}
}
