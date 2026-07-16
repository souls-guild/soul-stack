// Verifier parses and validates HS256 JWTs issued by [Issuer].
//
// MVP (ADR-014): HS256, signing key from Vault KV `secret/keeper/jwt-signing-key`,
// pins `iss` and checks `exp`. Used by the Operator API HTTP middleware
// ([keeper/internal/api/middleware/auth.go]).
//
// Verifier does not distinguish "expired" from "not yet valid" with a
// separate sentinel: the expired rate is high (short TTLs), and the rest
// (`nbf`) is not set by the issuer. All other parse/signature/issuer errors
// collapse to [ErrInvalidToken] and [ErrInvalidIssuer] for predictable
// classification by the 401 handler.
package jwt

import (
	"errors"
	"fmt"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// Claims is an extract from the parsed JWT that is safe to pass through an
// HTTP context. It does not include the original jwtv5 fields
// (RegisteredClaims), so middleware consumers do not depend on the internal
// representation or accidentally read unvalidated fields.
type Claims struct {
	Subject          string // sub → AID
	Issuer           string // iss
	Roles            []string
	BootstrapInitial bool
	IssuedAt         time.Time
	ExpiresAt        time.Time
}

// ErrInvalidToken is the general token parse/signature/structure error.
// Includes: malformed, plus-bad-segments, invalid signature, missing required
// claims (sub/iss/exp/iat), wrong-alg.
var ErrInvalidToken = errors.New("jwt: invalid token")

// ErrExpiredToken means `exp` is in the past. It is a separate sentinel so
// middleware can return a more informative `detail` in the 401 response.
var ErrExpiredToken = errors.New("jwt: token expired")

// ErrInvalidIssuer means the `iss` claim does not match the issuer configured
// on the Verifier.
var ErrInvalidIssuer = errors.New("jwt: invalid issuer")

// publicDetail* are fixed strings returned by [ClassifyVerifyErr] in the HTTP
// response. Do not format with err.Error(): golang-jwt/v5 internal messages
// (for example, the parser path) are unnecessary oracle-attack surface via
// distinguishable 401 causes.
const (
	publicDetailInvalidToken  = "invalid token"
	publicDetailExpiredToken  = "token expired"
	publicDetailInvalidIssuer = "token issuer not trusted"
)

// ClassifyVerifyErr returns a public-safe detail string for an HTTP 401
// response to a [Verifier.Verify] error. It guarantees that raw err.Error()
// (the internal golang-jwt/v5 message) is NEVER passed through.
//
// This contract is fragile, so the classifier lives here instead of in every
// middleware: when this package adds a new sentinel, the switch must be
// extended together with the exported constant so it is visible in one code
// review.
//
// nil err -> empty string (the caller must check err != nil itself).
func ClassifyVerifyErr(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrExpiredToken):
		return publicDetailExpiredToken
	case errors.Is(err, ErrInvalidIssuer):
		return publicDetailInvalidIssuer
	default:
		// ErrInvalidToken + any unwrapped error -> one generic detail.
		return publicDetailInvalidToken
	}
}

// Verifier is the configuration for one pinned issuer and signing key. It is
// safe for concurrent use (immutable after construction).
type Verifier struct {
	signingKey []byte
	issuer     string
}

// NewVerifier creates a verifier. signingKey must be >= 32 bytes (the HS256
// requirement in RFC 7518 section 3.2), and issuer must be a non-empty string
// for the pin.
func NewVerifier(signingKey []byte, issuer string) (*Verifier, error) {
	if len(signingKey) < minSigningKeyBytes {
		return nil, fmt.Errorf("jwt: signing key length %d < %d (HS256 minimum)", len(signingKey), minSigningKeyBytes)
	}
	if issuer == "" {
		return nil, errors.New("jwt: issuer is empty")
	}
	return &Verifier{signingKey: signingKey, issuer: issuer}, nil
}

// Verify parses and validates tokenString. It returns extracted claims or one
// of three sentinel errors ([ErrInvalidToken], [ErrExpiredToken],
// [ErrInvalidIssuer]) for predictable HTTP 401 mapping.
//
// Checks:
//
//   - HMAC method (rejects `alg: none` and any asym algorithm);
//   - HS256 signature with `signingKey`;
//   - `iss == verifier.issuer`;
//   - `exp` strictly in the future (jwtv5.WithExpirationRequired);
//   - non-empty `sub` (otherwise the token is useless: there is nobody to
//     attribute actions to).
func (v *Verifier) Verify(tokenString string) (*Claims, error) {
	if tokenString == "" {
		return nil, ErrInvalidToken
	}

	parsed, err := jwtv5.ParseWithClaims(tokenString, &archonClaims{},
		func(t *jwtv5.Token) (interface{}, error) {
			// Reject any non-HMAC method: protection against `alg: none` and
			// against substituting an asym key that an attacker could pass as the
			// HMAC secret.
			if _, ok := t.Method.(*jwtv5.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return v.signingKey, nil
		},
		jwtv5.WithValidMethods([]string{"HS256"}),
		jwtv5.WithExpirationRequired(),
		jwtv5.WithIssuedAt(),
	)
	if err != nil {
		switch {
		case errors.Is(err, jwtv5.ErrTokenExpired):
			return nil, ErrExpiredToken
		default:
			return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err.Error())
		}
	}
	if !parsed.Valid {
		return nil, ErrInvalidToken
	}

	claims, ok := parsed.Claims.(*archonClaims)
	if !ok {
		return nil, ErrInvalidToken
	}

	if claims.Issuer != v.issuer {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrInvalidIssuer, claims.Issuer, v.issuer)
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("%w: empty sub", ErrInvalidToken)
	}
	if claims.IssuedAt == nil {
		return nil, fmt.Errorf("%w: missing iat", ErrInvalidToken)
	}
	if claims.ExpiresAt == nil {
		return nil, fmt.Errorf("%w: missing exp", ErrInvalidToken)
	}

	return &Claims{
		Subject:          claims.Subject,
		Issuer:           claims.Issuer,
		Roles:            claims.Roles,
		BootstrapInitial: claims.BootstrapInitial,
		IssuedAt:         claims.IssuedAt.Time,
		ExpiresAt:        claims.ExpiresAt.Time,
	}, nil
}
