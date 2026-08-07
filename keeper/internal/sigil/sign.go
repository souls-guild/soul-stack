package sigil

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"regexp"

	"github.com/souls-guild/soul-stack/shared/pluginhost"
)

// reSHA256Hex is a 64 lowercase hex digit format for binary fingerprint.
// Matches plugin_sigils.sha256 CHECK constraint (migration 028).
var reSHA256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Signer holds the PRIMARY ed25519 private key of Keeper (used to sign new
// Sigil blocks) and the complete set of active trust anchors (public keys of all
// active signing keys, R3 multi-anchor, ADR-026(h)). Created once per process at
// `keeper run` startup ([LoadSigner] from sigil_signing_keys registry or
// [NewSigner] single-fallback from cfg) and is thread-safe for concurrent Sign
// calls (ed25519.Sign does not mutate the key, anchors are immutable after
// construction).
//
// Signature is always made with the primary key; anchors verify (on Soul/keeper-host,
// S5/S6): any anchor from the set validates the signature, providing seamless key
// rotation (ADR-026(h)). primary is always present in anchors.
type Signer struct {
	priv    ed25519.PrivateKey  // PRIMARY: signs new Sigils
	anchors []ed25519.PublicKey // all active pubkeys (including primary), R3
}

// NewSigner wraps a single private key into a Signer (single-anchor mode:
// anchor set = {primary}). priv must be a valid ed25519 key of full size
// (as returned by [LoadSigningKey]); empty or invalid size returns error to
// prevent signing with garbage. Used for cfg-fallback when key registry is empty
// (setupSigil) and in tests.
func NewSigner(priv ed25519.PrivateKey) (*Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("sigil: invalid ed25519 private key size %d (want %d)", len(priv), ed25519.PrivateKeySize)
	}
	pub := priv.Public().(ed25519.PublicKey)
	return &Signer{priv: priv, anchors: []ed25519.PublicKey{pub}}, nil
}

// NewMultiSigner assembles a multi-anchor Signer: priv is the PRIMARY private key
// (for signing), anchors are public keys of all active signing keys (R3 multi-anchor,
// ADR-026(h)). priv must be a valid ed25519 key; empty anchor set returns error
// (verify would have no anchors). The public part of primary is guaranteed to be
// included in the final set (even if caller did not provide it) — otherwise Soul
// could not verify a signature just issued by the primary key.
//
// The anchor set is deduplicated by raw key bytes: the registry holds primary
// separately, and its pubkey is already in anchors from pubkey_pem — no need to
// add primary again.
func NewMultiSigner(priv ed25519.PrivateKey, anchors []ed25519.PublicKey) (*Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("sigil: invalid ed25519 private key size %d (want %d)", len(priv), ed25519.PrivateKeySize)
	}
	primaryPub := priv.Public().(ed25519.PublicKey)

	seen := make(map[string]struct{}, len(anchors)+1)
	set := make([]ed25519.PublicKey, 0, len(anchors)+1)
	add := func(pub ed25519.PublicKey) {
		if len(pub) != ed25519.PublicKeySize {
			return
		}
		k := string(pub)
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		set = append(set, pub)
	}
	add(primaryPub)
	for _, a := range anchors {
		add(a)
	}
	if len(set) == 0 {
		// primaryPub is valid (size checked above) → unreachable; defensive.
		return nil, fmt.Errorf("sigil: empty anchor set")
	}
	return &Signer{priv: priv, anchors: set}, nil
}

// Public returns the public part of the PRIMARY key. Sent to Soul in bootstrap as
// a single trust anchor (ADR-026(d), sigil_pubkey_pem field); multi-anchor set
// for bootstrap (sigil_pubkey_pem_set) and runtime broadcast — S6 ([AnchorSet]).
func (s *Signer) Public() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// AnchorSet returns the public keys of all active signing keys (including
// primary), R3 multi-anchor (ADR-026(h)). This is the future set for the bootstrap
// field sigil_pubkey_pem_set and the runtime message SigilTrustAnchors
// (ReplaceAll semantics, seamless rotation) — distribution to S6.
//
// Returns a copy of the slice header: the ed25519.PublicKey values themselves are
// immutable, but we protect the slice from mutation by the caller. Order is as
// assembled (primary first in multi-mode via [LoadSigner], where ListActiveKeys
// returns primary first).
func (s *Signer) AnchorSet() []ed25519.PublicKey {
	out := make([]ed25519.PublicKey, len(s.anchors))
	copy(out, s.anchors)
	return out
}

// PublicKeyPEM returns the public part of the signing key in PEM form
// (SPKI: x509 MarshalPKIXPublicKey → "PUBLIC KEY" PEM block). This PEM is sent
// to Soul in bootstrap-reply ([keeperv1.BootstrapReply.SigilPubkeyPem]) as a
// trust anchor for verifying plugin grants (ADR-026, S6). Populated in
// [BootstrapDeps.SigilPubKeyPEM] at `keeper run` startup, only when Sigil is
// configured. MarshalPKIXPublicKey does not return error for a valid ed25519 key
// (size guaranteed by [NewSigner]) — defensive wrapper for future changes.
func (s *Signer) PublicKeyPEM() ([]byte, error) {
	return publicKeyToPEM(s.priv.Public().(ed25519.PublicKey))
}

// AnchorSetPEM returns the COMPLETE set of trust anchors ([Signer.AnchorSet]) in
// PEM form — one SPKI "PUBLIC KEY" line per anchor, in the same order (primary
// first). This is the set for the bootstrap field
// [keeperv1.BootstrapReply.SigilPubkeyPemSet] (R3-S4 reads set>single) and for
// the runtime message [keeperv1.SigilTrustAnchors] (broadcast + re-broadcast, S6).
//
// Each PEM line is symmetric to single [Signer.PublicKeyPEM]: Soul parses it with
// the same seed.ParseSigilPubKeys. Order is preserved to keep primary first
// (meaningless for verify — OR over set — but convenient for logs/diagnostics).
func (s *Signer) AnchorSetPEM() ([]string, error) {
	out := make([]string, 0, len(s.anchors))
	for i, pub := range s.anchors {
		pemBytes, err := publicKeyToPEM(pub)
		if err != nil {
			return nil, fmt.Errorf("sigil: anchor %d to PEM: %w", i, err)
		}
		out = append(out, string(pemBytes))
	}
	return out, nil
}

// publicKeyToPEM encodes an ed25519 public key into SPKI PEM block "PUBLIC KEY".
// Shared code for [Signer.PublicKeyPEM] (primary) and [Signer.AnchorSetPEM] (full set):
// the form must be byte-identical, otherwise Soul-side ParseSigilPubKeys will diverge
// between bootstrap-single and runtime-set.
func publicKeyToPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("sigil: marshal public key (SPKI): %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// Sign signs a Sigil grant for the artifact that came from source at ref, with digest
// binarySHA256hex and canonical schema document schemaDoc.
//
// # What is signed, and what is not
//
// The artifact carries no self-name since NIM-377 — no namespace, no name, no
// publisher — so the only identity a signature can be over is the one the operator
// asserted while approving it: the SOURCE it came from, at a REF. The registration
// alias is deliberately absent from the block: an alias is a local naming choice, so
// registering the same bytes a second time under a second alias must not require a
// second signature, and forging an alias must not be able to reach a signature at all.
//
// Steps (normative order, symmetric to verify on Soul, S6):
//  1. binarySHA256Raw = hex-decode(binarySHA256hex) — the raw 32 bytes of the digest;
//  2. schemaSHA256Raw = SchemaDigest(schemaDoc) — a plain SHA-256. There is no
//     normalization step because there is nothing left to normalize: the document is
//     canonical JSON from one serializer and travels with the artifact, so both sides
//     of the seal hash the same function over the same bytes;
//  3. block = BuildSigilBlock(source, ref, …) — the deterministic DST-v2 block with
//     length-prefixed fields;
//  4. signature = ed25519.Sign(priv, block) — raw 64 bytes.
//
// binarySHA256hex must be 64 lowercase hex chars. schemaDoc must be non-empty: an
// empty document has no disclosure to approve, and signing one would produce a valid
// seal over "this artifact says nothing".
func (s *Signer) Sign(source, ref, binarySHA256hex string, schemaDoc []byte) ([]byte, error) {
	if source == "" {
		return nil, fmt.Errorf("sigil: source is empty (the artifact's only signed identity)")
	}
	if ref == "" {
		return nil, fmt.Errorf("sigil: ref is empty")
	}
	if !reSHA256Hex.MatchString(binarySHA256hex) {
		return nil, fmt.Errorf("sigil: binary sha256 %q must be 64 lower-hex chars", binarySHA256hex)
	}
	if len(schemaDoc) == 0 {
		return nil, fmt.Errorf("sigil: schema document is empty (nothing to disclose, nothing to approve)")
	}
	binarySHA256Raw, err := hex.DecodeString(binarySHA256hex)
	if err != nil {
		// reSHA256Hex already guarantees valid hex — defensive, should not happen.
		return nil, fmt.Errorf("sigil: decode binary sha256: %w", err)
	}

	schemaDigest := pluginhost.SchemaDigest(schemaDoc)
	block := pluginhost.BuildSigilBlock(source, ref, binarySHA256Raw, schemaDigest[:])

	return ed25519.Sign(s.priv, block), nil
}
