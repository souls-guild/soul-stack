// Verifier parses and validates HS256 JWTs issued by [Issuer].
//
// MVP (ADR-014): HS256, signing key from Vault KV `secret/keeper/jwt-signing-key`,
// pins `iss` and checks `exp`. Used by the Operator API HTTP middleware
// ([keeper/internal/api/middleware/auth.go]).
//
// `iat` and `nbf` are checked with [clockSkewLeeway], and an `iat` further
// ahead than that gets its own sentinel ([ErrClockSkew]) rather than collapsing
// into [ErrInvalidToken]: the two are the same 401 to a caller, but "the clocks
// disagree" and "the signature does not match" send an operator to opposite
// ends of the system. `exp` is exempt from the leeway and stays strict — see
// the constant. `nbf` is not set by the issuer today and has no sentinel; it
// inherits the tolerance so that adding one later behaves like `iat` rather
// than like a second expiry. All other parse/signature/issuer errors collapse
// to [ErrInvalidToken] and [ErrInvalidIssuer] for predictable classification by
// the 401 handler.
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

// clockSkewLeeway is the tolerance for `iat` and `nbf` (golang-jwt applies it
// to `exp` as well; Verify takes that back — see below). It is not zero
// because keeper runs as several stateless instances behind one address
// ([ADR-002]) over one signing key from Vault ([ADR-014]): a token minted by
// instance A is routinely verified by instance B, and the nodes' clocks drift
// apart as a matter of course rather than as an incident. At zero tolerance a
// verifier one second behind the issuer rejects a one-second-old token because
// its `iat` is "in the future", and the operator's retry lands on another
// instance and succeeds — authentication that flaps instead of a clock that
// reports. The project already treats skew as a fact of life elsewhere
// ([ADR-018] warns above 10 minutes rather than refusing).
//
// 60s is the entire budget and is deliberately small: RFC 7519 §4.1.4 allows
// "some small leeway, usually no more than a few minutes", and the point of
// validating `iat` at all is to reject one that was made up, which a wide
// window would give away.
//
// It does NOT reach `exp`, even though golang-jwt applies one leeway to every
// time claim and offers no way to split them — [Verifier.Verify] re-checks
// expiry strictly afterwards for that reason.
//
// Neither claim is cheap to get wrong. A verifier running more than that budget
// behind the issuer refuses a fresh token on `iat`, and minting another never
// helps: the replacement's `iat` is just as far in that verifier's future. A
// verifier running ahead refuses one on `exp` early by exactly the magnitude of
// the drift: if this instance's clock is N seconds ahead of the issuer's, a
// token is rejected N seconds before its real `exp`. Minting another helps only
// until the drift consumes the replacement's entire remaining life. That life
// is not a constant to look up: on the cookie exchange [ADR-058] caps it at
// `min(auth.jwt.exchange_ttl, remaining_until_cookie.exp)`, and `exchange_ttl`
// itself defaults to 10m with a 1m floor — so a nearly-spent session hands out
// tokens shorter than even the floor, and at the 60s of drift this very
// constant calls ordinary, those arrive expired. Both are repaired by fixing
// the clock, and until then both flap, because the operator's retry lands on an
// instance that may not be skewed.
//
// The asymmetry that decides it is in what tolerance is bought with. `iat` is
// a sanity check that the timestamp was not fabricated, so slack there gives
// away nearly nothing. `exp` is the authorization boundary itself, and slack
// there would extend how long every token stays usable — a stolen one included
// — at every instance, drifted or not. The strict re-check trades a guaranteed
// 60s extension at every instance for an asymmetry that only penalises the
// drifted ones: an instance whose clock is ahead pays the full drift in lost
// token lifetime, while an instance whose clock is behind retains a residual
// acceptance window equal to its own lag (it honours a token past its real
// `exp` by exactly how late its local clock is). The overrun at least tracks
// an actual fault instead of being granted to everyone.
//
// [ADR-002]: docs/adr/0002-transport-grpc-ha.md
// [ADR-014]: docs/adr/0014-operator-identity.md
// [ADR-018]: docs/adr/0018-soulprint-typed.md
// [ADR-058]: docs/adr/0058-operator-auth-ldap-oidc.md
const clockSkewLeeway = 60 * time.Second

// ErrInvalidToken is the general token parse/signature/structure error.
// Includes: malformed, plus-bad-segments, invalid signature, missing required
// claims (sub/iss/exp/iat), wrong-alg.
var ErrInvalidToken = errors.New("jwt: invalid token")

// ErrExpiredToken means `exp` is in the past. It is a separate sentinel so
// middleware can return a more informative `detail` in the 401 response.
var ErrExpiredToken = errors.New("jwt: token expired")

// ErrClockSkew means `iat` is further in the future than [clockSkewLeeway]
// allows — the issuing instance's clock is ahead of this one by more than the
// budget. It is a separate sentinel because the alternative was reporting it as
// [ErrInvalidToken], which is the same string a forged signature produces: a
// cluster whose clocks had drifted looked exactly like one being attacked, and
// the diagnosis went to the wrong half of the system (NIM-621).
var ErrClockSkew = errors.New("jwt: token issued in the future")

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
	// publicDetailClockSkew names a cause the caller cannot act on by retrying
	// and the operator can fix in one place. It reveals nothing: the caller
	// supplied the `iat` this is measured against.
	publicDetailClockSkew = "token issued in the future"
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
	case errors.Is(err, ErrClockSkew):
		return publicDetailClockSkew
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
// of four sentinel errors ([ErrInvalidToken], [ErrExpiredToken],
// [ErrClockSkew], [ErrInvalidIssuer]) for predictable HTTP 401 mapping. Adding
// a fifth means extending [ClassifyVerifyErr] in the same change — see the
// contract on the publicDetail constants.
//
// Checks:
//
//   - HMAC method (rejects `alg: none` and any asym algorithm);
//   - HS256 signature with `signingKey`;
//   - `iss == verifier.issuer`;
//   - `exp` strictly in the future (jwtv5.WithExpirationRequired, plus an
//     explicit re-check that [clockSkewLeeway] does not soften it);
//   - `iat` no further ahead than [clockSkewLeeway], else [ErrClockSkew];
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
		jwtv5.WithLeeway(clockSkewLeeway),
	)
	if err != nil {
		switch {
		case errors.Is(err, jwtv5.ErrTokenExpired):
			return nil, ErrExpiredToken
		case errors.Is(err, jwtv5.ErrTokenUsedBeforeIssued):
			// Past the leeway, so this is not ordinary drift between instances.
			// Kept out of ErrInvalidToken so the 401 says which half to look at.
			return nil, fmt.Errorf("%w: %s", ErrClockSkew, err.Error())
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
	// Expiry, strictly — undoing the part of [clockSkewLeeway] that golang-jwt
	// applied to `exp` because it cannot apply a leeway to one claim only. The
	// reasoning is on the constant; the effect is that a token past its `exp` is
	// refused here at the same local-clock boundary this instance would have
	// enforced before the leeway existed, and with the same [ErrExpiredToken].
	// Callers depend on that specific answer: the cookie exchange
	// (keeper/internal/api/huma_auth_token.go) tells the browser "your session
	// ended, sign in again" only because expiry arrives as its own error rather
	// than as a generic 401.
	//
	// Order change from NIM-621: this re-check sits below the issuer check,
	// whereas the parser used to reject expiry above everything. A token that is
	// expired by less than [clockSkewLeeway] and carries `iss != verifier.issuer`
	// (e.g. minted by a sibling instance whose kid differs) now answers
	// [ErrInvalidIssuer]; before NIM-621 the parser answered [ErrExpiredToken]
	// first. Past the leeway the parser still gets there first, so the change is
	// confined to that window. Both answers are 401 and the holder can read
	// either fact out of its own token, so nothing downstream tells them apart —
	// recorded here because there is otherwise no way to notice.
	if time.Now().After(claims.ExpiresAt.Time) {
		return nil, ErrExpiredToken
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
