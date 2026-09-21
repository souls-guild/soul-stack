// Package bootstraptoken provides types and CRUD for the one-time
// SoulSeed token registry (`bootstrap_tokens`) per docs/soul/onboarding.md.
//
// The plain token is generated in [Generate] (256-bit crypto-random,
// base64url), returned to the operator **once**, and never persisted again.
// Only `token_hash` = SHA-256 (hex) is stored in the DB. On presentation to
// the `Bootstrap` RPC the hash is recomputed and matched against the
// registry (see [Burn]).
//
// "Burning" a token is a single-transaction UPDATE guarded by
// `used_at IS NULL AND expires_at > NOW()` (race-safe against the same
// token being presented twice concurrently).
package bootstraptoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// TokenByteLen is the plain token length before base64url encoding. 32
// bytes = 256 bits of entropy, enough to rule out guessing even without a
// TTL. Canonical format is documented in docs/soul/onboarding.md.
const TokenByteLen = 32

// HashHexLen is the length of the SHA-256 hex representation (64 chars).
// Mirrors the CHECK `bootstrap_tokens_token_hash_format` from migration 008.
const HashHexLen = 64

// DefaultTokenTTL is the default bootstrap token TTL
// (docs/soul/onboarding.md: "token is one-time, default TTL 24h"). Used by
// the Operator API when issuing via `POST /v1/souls` and `issue-token` if
// the operator didn't specify otherwise (override field is post-MVP).
const DefaultTokenTTL = 24 * time.Hour

// PlainToken is a sensitive wrapper around the token's plain value,
// returned once by [Generate] / [Insert]. It has no String() method, but
// that is not a safety net: Go's default struct formatting still leaks the
// unexported field (fmt.Print(token) prints `{<plain>}`).
//
// To obtain the plain value for a file write or client response, the
// caller must call [PlainToken.Reveal] — a deliberately explicit step so
// `grep .Reveal(` finds every place the plain value enters I/O.
type PlainToken struct {
	v string
}

// Reveal returns the token's plain value. ONLY for writing to a file,
// returning to the client, or tests. NEVER log it.
func (t PlainToken) Reveal() string { return t.v }

// Hash returns the SHA-256 of the plain token in hex. Used by [Insert]
// (on issuance) and [Burn] (on presentation to the Bootstrap RPC).
func (t PlainToken) Hash() string {
	sum := sha256.Sum256([]byte(t.v))
	return hex.EncodeToString(sum[:])
}

// HashToken hashes an arbitrary string to SHA-256 hex. Used by the gRPC
// `Bootstrap` handler, which gets the plain value from a protobuf field
// and must compare it against the registry immediately, without wrapping
// it in a PlainToken.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Generate issues a new plain token (32 bytes crypto-random → base64url,
// no padding). Returns the [PlainToken] wrapper; the hex hash is available
// via Hash().
//
// Used only by the Operator API when issuing a new token; in the gRPC
// Bootstrap handler the plain token comes from the client, not generated
// here.
func Generate() (PlainToken, error) {
	buf := make([]byte, TokenByteLen)
	if _, err := rand.Read(buf); err != nil {
		return PlainToken{}, fmt.Errorf("bootstraptoken: crypto/rand: %w", err)
	}
	// base64url without padding: compact URL-safe format, symmetric with
	// JWT (golang-jwt also uses base64url-no-padding).
	return PlainToken{v: base64.RawURLEncoding.EncodeToString(buf)}, nil
}

// Record is the runtime representation of a `bootstrap_tokens` row. The
// token's plain value is never stored in Record — only the hash.
type Record struct {
	TokenID      string     `json:"token_id"`
	SID          string     `json:"sid"`
	TokenHash    string     `json:"-"` // sensitive: never serialize out.
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	UsedAt       *time.Time `json:"used_at,omitempty"`
	UsedByKID    *string    `json:"used_by_kid,omitempty"`
	CreatedByAID *string    `json:"created_by_aid,omitempty"`
}

// IsActive reports whether the token is neither burned nor expired (as of
// this Go-side check; the authoritative check happens in the DB via the
// WHERE clause in [Burn]).
func (r *Record) IsActive(now time.Time) bool {
	return r.UsedAt == nil && r.ExpiresAt.After(now)
}

// ValidHashFormat reports whether the string is a valid SHA-256 hex (64
// lower-hex chars). Mirrors the CHECK `bootstrap_tokens_token_hash_format`
// to fail before a round trip.
func ValidHashFormat(hash string) bool {
	if len(hash) != HashHexLen {
		return false
	}
	for _, c := range hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// errInvalidHash is a utility error for the CRUD functions' validation
// phase (not a sentinel, not meant as an `errors.Is` target).
var errInvalidHash = errors.New("bootstraptoken: token_hash format invalid (must be 64 lower-hex chars)")
