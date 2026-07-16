// Package sigil provides keeper-side plugin binary signatures and allowlist registry
// (ADR-026, slice S3, Sigil trust seal).
//
// Package contents:
//   - key.go    — load ed25519 signing private key from Vault KV;
//   - sign.go   — hash assembly manifest+binary and ed25519 signature of Sigil block;
//   - store.go  — CRUD plugin_sigils registry (allow / revoke / list / lookup).
//
// S3↔S6 invariant (normative, maintained jointly with
// shared/pluginhost.NormalizeManifestBytes): manifest.yaml bytes that
// Keeper hashes in [Signer.Sign] MUST be identical to what Soul re-hashes
// in verify (S6). Guarantee — (1) manifest+binary delivered in single
// artifact stream, (2) both sides run raw bytes through
// NormalizeManifestBytes before SHA-256. Signed block assembled by clean
// deterministic shared/pluginhost.BuildSigilBlock — common code for Sign (S3)
// and Verify (S6), no proto-marshal.
//
// Key asymmetry is mandatory: Sigil signature — ed25519 (private on Keeper,
// public key sent to Soul in bootstrap). This is NOT HS256-symmetric signing-key
// for JWT (ADR-014); extractor here — separate, ed25519-specific.
package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// vaultSigningKeyField is the field name inside Vault KV-secret where
// ed25519 signing private key for Sigil lies. Matches JWT signing-key
// convention (field `signing_key`) so local-dev `vault kv put` command and golden fixtures
// are uniform; secret path distinguishes purpose (sigil vs jwt).
const vaultSigningKeyField = "signing_key"

// ErrSigningKeyMissing is returned when Vault KV lacks `signing_key` field or it is empty.
var ErrSigningKeyMissing = errors.New("sigil: signing_key field missing or empty in Vault KV")

// ErrSigningKeyFormat is returned when `signing_key` field exists but cannot be parsed into
// ed25519.PrivateKey in any supported format (see parseEd25519Key).
// Invalid/HS256-format → this error, not silent ignore.
var ErrSigningKeyFormat = errors.New("sigil: signing_key is not a valid ed25519 private key")

// ErrAnchorPubkeyFormat is returned when pubkey_pem string in sigil_signing_keys registry does not
// parse into ed25519.PublicKey (SPKI PEM). Registry writes PubkeyPEM itself (via
// [Signer.PublicKeyPEM] on Introduce side), so normally unreachable —
// fail-closed guard against manual string corruption in DB.
var ErrAnchorPubkeyFormat = errors.New("sigil: anchor pubkey_pem is not a valid ed25519 public key (SPKI PEM)")

// KVReader is a narrow subset of [keepervault.Client], needed for
// signing key load: single KV-secret read by logical-path. Narrowing allows
// unit-testing multi-key load via fake-reader; actual
// *keepervault.Client satisfies automatically (symmetric with ExecQueryRower).
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

var _ KVReader = (*keepervault.Client)(nil)

// LoadSigningKey reads ed25519 signing private key for Sigil from Vault KV via
// config reference `sigil.signing_key_ref` (`vault:<mount>/<path>`).
//
// ParseRef + ReadKV pattern is symmetric with bootstrap.LoadSigningKey, BUT extractor
// is ed25519-specific (parseEd25519Key), not HS256-raw-bytes: key forms
// are incompatible, extractSigningKey cannot be reused.
//
// Return is nil-safe for S3 callers: on empty ref / nil-client — error, not
// panic. Key absence in config handled by caller (S4 returns «sigil key
// not configured»); empty ref should not reach here.
func LoadSigningKey(ctx context.Context, vc KVReader, signingKeyRef string) (ed25519.PrivateKey, error) {
	if vc == nil {
		return nil, fmt.Errorf("sigil: vault client is nil")
	}
	if signingKeyRef == "" {
		return nil, fmt.Errorf("sigil: signing_key_ref is empty")
	}
	path, err := keepervault.ParseRef(signingKeyRef)
	if err != nil {
		return nil, fmt.Errorf("sigil: signing_key_ref: %w", err)
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("sigil: read vault %q: %w", path, err)
	}
	return extractEd25519Key(kv)
}

// LoadSigner builds a multi-anchor [Signer] from active set of
// sigil_signing_keys registry (R3, ADR-026(h)):
//
//   - primary-key (keys[i].IsPrimary) → its private key read from Vault via
//     VaultRef and becomes signing key;
//   - anchors set = public keys (PubkeyPEM) of ALL active-keys.
//
// Private keys of NON-primary keys are NOT read: only primary needed for signing,
// public parts from registry sufficient for verify. This narrows private key
// exposure (read from Vault exactly one) and aligns with ADR-026(h) model,
// where anchors are handed to Soul as public keys.
//
// keys expected as result of [ListActiveKeys] (order — primary first).
// Exactly one primary guaranteed by registry invariant (keys.go); here —
// fail-closed checks: empty set → [ErrKeyNotFound], no primary →
// [ErrKeyNotFound] (no signer). Caller (setupSigil) on empty registry
// decides fallback to cfg, see daemon.
func LoadSigner(ctx context.Context, vc KVReader, keys []*SigningKey) (*Signer, error) {
	if vc == nil {
		return nil, fmt.Errorf("sigil: vault client is nil")
	}
	if len(keys) == 0 {
		return nil, ErrKeyNotFound
	}

	var primary *SigningKey
	anchors := make([]ed25519.PublicKey, 0, len(keys))
	for _, k := range keys {
		pub, err := parseEd25519PublicPEM(k.PubkeyPEM)
		if err != nil {
			return nil, fmt.Errorf("sigil: anchor key_id=%s: %w", k.KeyID, err)
		}
		anchors = append(anchors, pub)
		if k.IsPrimary {
			if primary != nil {
				// Invariant "exactly one primary" violated — registry corrupted,
				// fail-closed (do not pick arbitrary).
				return nil, fmt.Errorf("sigil: more than one primary key in active set (key_id=%s and %s)", primary.KeyID, k.KeyID)
			}
			primary = k
		}
	}
	if primary == nil {
		return nil, ErrKeyNotFound
	}

	priv, err := LoadSigningKey(ctx, vc, primary.VaultRef)
	if err != nil {
		return nil, fmt.Errorf("sigil: load primary private key (key_id=%s): %w", primary.KeyID, err)
	}
	return NewMultiSigner(priv, anchors)
}

// parseEd25519PublicPEM parses SPKI PEM block ("PUBLIC KEY") into ed25519.PublicKey.
// Non-ed25519 (RSA/ECDSA) or unreadable PEM → [ErrAnchorPubkeyFormat].
func parseEd25519PublicPEM(pemStr string) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("%w: not a PEM block", ErrAnchorPubkeyFormat)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: parse SPKI: %v", ErrAnchorPubkeyFormat, err)
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: SPKI key is %T, want ed25519.PublicKey", ErrAnchorPubkeyFormat, parsed)
	}
	return pub, nil
}

// extractEd25519Key extracts `signing_key` field from Vault KV payload and parses
// it into ed25519.PrivateKey. Supported value forms:
//
//   - string, PEM (`-----BEGIN PRIVATE KEY-----`, PKCS#8) → parsed via x509;
//   - string, base64 of PKCS#8 DER → decoded and parsed via x509;
//   - string, base64 of raw 64-byte ed25519 seed||pub (ed25519.PrivateKey
//     format) → used directly;
//   - string, base64 of raw 32-byte seed → ed25519.NewKeyFromSeed;
//   - []byte with same raw variants (64 / 32 bytes).
//
// Any other form (including short HS256-secret of arbitrary length) →
// [ErrSigningKeyFormat]. Missing/empty → [ErrSigningKeyMissing].
func extractEd25519Key(kv map[string]any) (ed25519.PrivateKey, error) {
	raw, ok := kv[vaultSigningKeyField]
	if !ok {
		return nil, ErrSigningKeyMissing
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil, ErrSigningKeyMissing
		}
		return parseEd25519Key([]byte(v))
	case []byte:
		if len(v) == 0 {
			return nil, ErrSigningKeyMissing
		}
		return parseEd25519Key(v)
	default:
		return nil, fmt.Errorf("sigil: signing_key has unsupported type %T (want string or []byte)", raw)
	}
}

// parseEd25519Key attempts to recognize ed25519 private key from set of forms in
// order: PEM → base64(DER/raw) → raw-bytes. Returns [ErrSigningKeyFormat]
// if no form matches (including valid but non-ed25519 PKCS#8 key —
// e.g., RSA/ECDSA).
func parseEd25519Key(b []byte) (ed25519.PrivateKey, error) {
	// (1) PEM wrapper (PKCS#8).
	if block, _ := pem.Decode(b); block != nil {
		key, err := parsePKCS8Ed25519(block.Bytes)
		if err != nil {
			return nil, err
		}
		return key, nil
	}

	// (2) base64 → DER (PKCS#8) or raw-bytes.
	if decoded, err := base64.StdEncoding.DecodeString(string(b)); err == nil {
		if key := tryRawEd25519(decoded); key != nil {
			return key, nil
		}
		if key, err := parsePKCS8Ed25519(decoded); err == nil {
			return key, nil
		}
		// base64 decoded, but content is not ed25519: explicit format error,
		// do not fall through to raw branch (would yield garbage key).
		return nil, fmt.Errorf("%w: base64 payload is neither raw ed25519 (32/64 bytes) nor PKCS#8 ed25519", ErrSigningKeyFormat)
	}

	// (3) raw bytes (non-base64 KV value).
	if key := tryRawEd25519(b); key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("%w: value is not PEM, base64 or raw ed25519 key", ErrSigningKeyFormat)
}

// tryRawEd25519 interprets bytes as raw ed25519 key: 64 bytes =
// seed||pub (ed25519.PrivateKey as-is), 32 bytes = seed (via
// NewKeyFromSeed). Otherwise nil — caller decides if error or reason to try
// another form.
func tryRawEd25519(b []byte) ed25519.PrivateKey {
	switch len(b) {
	case ed25519.PrivateKeySize: // 64
		key := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
		copy(key, b)
		return key
	case ed25519.SeedSize: // 32
		return ed25519.NewKeyFromSeed(b)
	default:
		return nil
	}
}

// parsePKCS8Ed25519 parses PKCS#8 DER and verifies it contains exactly
// ed25519 key. RSA/ECDSA key in PKCS#8 → [ErrSigningKeyFormat].
func parsePKCS8Ed25519(der []byte) (ed25519.PrivateKey, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("%w: parse PKCS#8: %v", ErrSigningKeyFormat, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: PKCS#8 key is %T, want ed25519.PrivateKey", ErrSigningKeyFormat, parsed)
	}
	return key, nil
}
