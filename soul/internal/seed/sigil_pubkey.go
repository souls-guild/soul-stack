package seed

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// ErrSigilPubKeyFormat — sigil_pubkey.pem exists but doesn't parse as an
// ed25519.PublicKey (not PEM / not SPKI / not an ed25519 key). A broken
// trust anchor is an explicit error, not a silent verify bypass (otherwise a
// tampered-with or empty file would quietly open up fail-open).
var ErrSigilPubKeyFormat = errors.New("seed: sigil_pubkey.pem is not a valid ed25519 SPKI public key")

// ParseSigilPubKeys parses the SET of Sigil trust anchors from the PEM bytes
// in [Material.SigilPubKeyPEM]. sigil_pubkey.pem may carry several PEM
// blocks back to back (concatenation) — multi-anchor for gapless signing-key
// rotation (ADR-026(h), R3). Each block is SPKI ed25519, matching what
// keeper-side sigil.Signer.PublicKeyPEM writes:
//
//   - empty input (Sigil disabled on Keeper) → (nil, nil): a valid state,
//     the anchor set is empty, plugin verify fail-closes on no_trust_anchor;
//   - one block → a list of length 1 (backward compat with a single-anchor seed);
//   - N blocks → N keys in write order;
//   - any broken block (not PEM / not SPKI / RSA-ECDSA / trailing garbage) →
//     (nil, ErrSigilPubKeyFormat): the caller must refuse to start, not
//     silently disable verify (fail-closed on a broken trust anchor).
func ParseSigilPubKeys(pemBytes []byte) ([]ed25519.PublicKey, error) {
	if len(pemBytes) == 0 {
		return nil, nil
	}
	var keys []ed25519.PublicKey
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			// No block at all → broken input. Already parsed at least one, and
			// the tail isn't PEM → also a rejection (not a silent set truncation).
			if len(keys) == 0 {
				return nil, fmt.Errorf("%w: not a PEM block", ErrSigilPubKeyFormat)
			}
			if hasNonSpace(rest) {
				return nil, fmt.Errorf("%w: trailing data after %d PEM block(s)", ErrSigilPubKeyFormat, len(keys))
			}
			return keys, nil
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: parse SPKI (block %d): %v", ErrSigilPubKeyFormat, len(keys)+1, err)
		}
		pub, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: SPKI key (block %d) is %T, want ed25519.PublicKey", ErrSigilPubKeyFormat, len(keys)+1, parsed)
		}
		keys = append(keys, pub)
	}
}

// hasNonSpace reports whether the tail after pem.Decode has meaningful bytes
// (not spaces/newlines). A tail of blank lines between/after PEM blocks is
// normal; a non-blank tail means broken input.
func hasNonSpace(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return true
		}
	}
	return false
}
