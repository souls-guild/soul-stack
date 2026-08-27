package jwt

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

const testIssuer = "keeper.test"

func mustIssue(t *testing.T, key []byte, issuer, sub string, ttl time.Duration, bootstrap bool, roles []string) string {
	t.Helper()
	iss, err := NewIssuer(key, issuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue(sub, roles, ttl, bootstrap)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

func TestNewVerifier_RejectsShortKey(t *testing.T) {
	if _, err := NewVerifier(bytes.Repeat([]byte{0x01}, 16), testIssuer); err == nil {
		t.Fatalf("NewVerifier with 16-byte key: expected error")
	}
}

func TestNewVerifier_RejectsEmptyIssuer(t *testing.T) {
	if _, err := NewVerifier(testSigningKey, ""); err == nil {
		t.Fatalf("NewVerifier with empty issuer: expected error")
	}
}

func TestVerify_Happy(t *testing.T) {
	tok := mustIssue(t, testSigningKey, testIssuer, "archon-alice",
		time.Hour, true, []string{"cluster-admin"})
	v, err := NewVerifier(testSigningKey, testIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	claims, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "archon-alice" {
		t.Errorf("Subject = %q, want archon-alice", claims.Subject)
	}
	if claims.Issuer != testIssuer {
		t.Errorf("Issuer = %q, want %s", claims.Issuer, testIssuer)
	}
	if !claims.BootstrapInitial {
		t.Errorf("BootstrapInitial = false, want true")
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "cluster-admin" {
		t.Errorf("Roles = %v, want [cluster-admin]", claims.Roles)
	}
	if claims.IssuedAt.IsZero() {
		t.Errorf("IssuedAt is zero")
	}
	if claims.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt is zero")
	}
}

func TestVerify_Expired(t *testing.T) {
	// Create a token with exp in the past; the manual ParseWithClaims workaround
	// cannot be used (Issue does not allow negative ttl). Use jwtv5 directly.
	claims := archonClaims{
		Roles: []string{"cluster-admin"},
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   "archon-alice",
			IssuedAt:  jwtv5.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwtv5.NewNumericDate(time.Now().Add(-time.Hour)),
		},
	}
	tok, err := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims).SignedString(testSigningKey)
	if err != nil {
		t.Fatalf("manual sign: %v", err)
	}
	v, _ := NewVerifier(testSigningKey, testIssuer)
	_, err = v.Verify(tok)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("Verify expired: err = %v, want ErrExpiredToken", err)
	}
}

func TestVerify_BadSignature(t *testing.T) {
	tok := mustIssue(t, testSigningKey, testIssuer, "archon-alice", time.Hour, false, nil)
	otherKey := bytes.Repeat([]byte{0xcd}, 32)
	v, _ := NewVerifier(otherKey, testIssuer)
	_, err := v.Verify(tok)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify bad-sig: err = %v, want ErrInvalidToken", err)
	}
}

func TestVerify_WrongIssuer(t *testing.T) {
	tok := mustIssue(t, testSigningKey, "other.issuer", "archon-alice", time.Hour, false, nil)
	v, _ := NewVerifier(testSigningKey, testIssuer)
	_, err := v.Verify(tok)
	if !errors.Is(err, ErrInvalidIssuer) {
		t.Fatalf("Verify wrong-issuer: err = %v, want ErrInvalidIssuer", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	cases := []string{
		"",
		"not-a-jwt",
		"a.b.c",
		"header.payload",
		strings.Repeat("a", 50),
	}
	v, _ := NewVerifier(testSigningKey, testIssuer)
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			_, err := v.Verify(tc)
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("Verify(%q): err = %v, want ErrInvalidToken", tc, err)
			}
		})
	}
}

// TestVerify_RejectsNoneAlg checks that a token with `alg: none` (unsigned) is
// rejected regardless of the signature.
func TestVerify_RejectsNoneAlg(t *testing.T) {
	claims := archonClaims{
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   "archon-alice",
			IssuedAt:  jwtv5.NewNumericDate(time.Now()),
			ExpiresAt: jwtv5.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tok, err := jwtv5.NewWithClaims(jwtv5.SigningMethodNone, claims).SignedString(jwtv5.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("manual none-sign: %v", err)
	}
	v, _ := NewVerifier(testSigningKey, testIssuer)
	_, err = v.Verify(tok)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify alg-none: err = %v, want ErrInvalidToken", err)
	}
}

// TestVerify_MissingClaims checks that a token without required claims
// (sub/iat/exp) returns ErrInvalidToken.
func TestVerify_MissingSubject(t *testing.T) {
	claims := archonClaims{
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer: testIssuer,
			// Subject is intentionally empty.
			IssuedAt:  jwtv5.NewNumericDate(time.Now()),
			ExpiresAt: jwtv5.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tok, err := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims).SignedString(testSigningKey)
	if err != nil {
		t.Fatalf("manual sign: %v", err)
	}
	v, _ := NewVerifier(testSigningKey, testIssuer)
	_, err = v.Verify(tok)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify missing-sub: err = %v, want ErrInvalidToken", err)
	}
}

// issueAtSkew mints what an issuer whose clock is `skew` ahead of ours would
// produce: same key, same claims, only the time ones moved. [Issuer] reads
// time.Now() itself, so the shift belongs here rather than in it.
func issueAtSkew(t *testing.T, key []byte, issuer, sub string, skew, ttl time.Duration) string {
	t.Helper()
	now := time.Now().UTC().Add(skew)
	claims := archonClaims{
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    issuer,
			Subject:   sub,
			IssuedAt:  jwtv5.NewNumericDate(now),
			ExpiresAt: jwtv5.NewNumericDate(now.Add(ttl)),
		},
	}
	signed, err := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign at skew %s: %v", skew, err)
	}
	return signed
}

// TestVerify_ClockSkew covers what [clockSkewLeeway] exists for. Keeper is a
// multi-instance cluster over one shared signing key (ADR-002, ADR-014), so a
// token is routinely verified by a node other than the one that minted it and
// their clocks differ as a matter of course. At zero tolerance a token one
// second old is refused, and refused as "invalid token" — the same answer a
// forged signature gets, which is how a clock problem gets diagnosed as an
// attack (NIM-621).
func TestVerify_ClockSkew(t *testing.T) {
	v, err := NewVerifier(testSigningKey, testIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	tests := []struct {
		name string
		skew time.Duration
		want error // nil → has to be accepted
	}{
		{"one second ahead — the drift actually seen", time.Second, nil},
		{"half the budget ahead", clockSkewLeeway / 2, nil},
		{"a second inside the budget", clockSkewLeeway - time.Second, nil},
		{"far past the budget is still refused", clockSkewLeeway * 10, ErrClockSkew},
		// Absolute, deliberately NOT expressed through the constant. Every other
		// duration here scales with it, so widening clockSkewLeeway leaves them
		// all green while `iat` validation quietly stops meaning anything — the
		// floor was guarded and the ceiling was not. Ten minutes is the outer end
		// of RFC 7519's "some small leeway"; a budget that accepts an `iat` this
		// far ahead is no longer tolerating drift, it is taking the token's word.
		{"ten minutes ahead is never drift", 10 * time.Minute, ErrClockSkew},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tok := issueAtSkew(t, testSigningKey, testIssuer, "archon-alice", tc.skew, time.Hour)
			_, err := v.Verify(tok)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("Verify refused a token issued %s ahead: %v. Inside the %s budget it has to pass, "+
					"or two instances of one cluster reject each other's tokens", tc.skew, err, clockSkewLeeway)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("Verify(skew=%s) = %v, want %v — the budget has to still mean something, "+
					"otherwise a made-up `iat` passes", tc.skew, err, tc.want)
			}
		})
	}

	// The cause has to survive the trip to the caller. Collapsing it into
	// "invalid token" is what pointed the last diagnosis at the signing key.
	tok := issueAtSkew(t, testSigningKey, testIssuer, "archon-alice", clockSkewLeeway*10, time.Hour)
	_, err = v.Verify(tok)
	if got := ClassifyVerifyErr(err); got != publicDetailClockSkew {
		t.Fatalf("ClassifyVerifyErr = %q, want %q. Indistinguishable from a bad signature is the defect itself",
			got, publicDetailClockSkew)
	}
}

// TestVerify_LeewayDoesNotReachExpiry pins the asymmetry [clockSkewLeeway] is
// built on: the tolerance is for `iat`, and `exp` does not get it.
//
// golang-jwt applies one leeway to every time claim and gives no way to split
// them, so the leeway alone would have accepted a token up to a minute past
// `exp` — on a Bearer whose configured floor is one minute
// (`auth.jwt.exchange_ttl`, ADR-058), doubling how long a stolen one keeps
// working. Verify re-checks expiry strictly for that reason, and this is what
// says so: delete the re-check and the first case below goes green while the
// window silently reopens.
//
// The boundary case is the one that matters. Expiring by a whole budget-length
// would be caught either way and would prove nothing.
func TestVerify_LeewayDoesNotReachExpiry(t *testing.T) {
	v, err := NewVerifier(testSigningKey, testIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	tests := []struct {
		name    string
		expired time.Duration // how long ago `exp` passed
	}{
		{"one second past exp — inside the leeway, still expired", time.Second},
		{"half a budget past exp", clockSkewLeeway / 2},
		{"a second short of a full budget", clockSkewLeeway - time.Second},
		{"well past the budget", clockSkewLeeway * 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// iat sits one budget before exp, so `iat` itself is never the reason.
			tok := issueAtSkew(t, testSigningKey, testIssuer, "archon-alice",
				-(tc.expired + clockSkewLeeway), clockSkewLeeway)
			if _, err := v.Verify(tok); !errors.Is(err, ErrExpiredToken) {
				t.Fatalf("Verify on a token that expired %s ago = %v, want ErrExpiredToken. "+
					"The clock-skew budget is for `iat`; letting it reach `exp` extends every "+
					"issued Bearer by its own length, and the cookie exchange stops being able "+
					"to tell the browser its session ended", tc.expired, err)
			}
		})
	}

	// A token still inside its life must not be caught by the strict re-check —
	// the other direction, without which the test above passes on a verifier
	// that rejects everything.
	live := issueAtSkew(t, testSigningKey, testIssuer, "archon-alice", 0, time.Hour)
	if _, err := v.Verify(live); err != nil {
		t.Fatalf("Verify refused a token with an hour left: %v", err)
	}
}
