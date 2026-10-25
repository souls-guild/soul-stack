package jwt

import (
	"errors"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

type revokedIDs map[string]bool

func (r revokedIDs) IsJWTRevoked(id string) bool { return r[id] }

// The jti goes into the token and comes back out of Verify: it is the
// operator_jwts row the token is revoked by (NIM-1045).
func TestIssueVerify_CarriesTheJTI(t *testing.T) {
	iss, err := NewIssuer(testSigningKey, testIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue("01JTESTJT2000000000000000B", "archon-alice", nil, time.Hour, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	v, err := NewVerifier(testSigningKey, testIssuer, NoRevocation)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	claims, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.ID != "01JTESTJT2000000000000000B" {
		t.Errorf("claims.ID = %q, want the issued jti", claims.ID)
	}
}

func TestIssue_RequiresAnID(t *testing.T) {
	iss, err := NewIssuer(testSigningKey, testIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	if _, err := iss.Issue("", "archon-alice", nil, time.Hour, false); err == nil {
		t.Error("Issue with an empty id minted a token no row can revoke")
	}
}

func TestNewVerifier_RequiresARevocationSource(t *testing.T) {
	if _, err := NewVerifier(testSigningKey, testIssuer, nil); err == nil {
		t.Error("NewVerifier accepted a nil revocation source")
	}
}

func TestVerify_RefusesARevokedJTI(t *testing.T) {
	iss, _ := NewIssuer(testSigningKey, testIssuer)
	revoked, _ := iss.Issue("01JREV0KEDJWT0000000000000", "archon-alice", nil, time.Hour, false)
	live, _ := iss.Issue("01JG00DJWT0000000000000000", "archon-alice", nil, time.Hour, false)
	v, err := NewVerifier(testSigningKey, testIssuer, revokedIDs{"01JREV0KEDJWT0000000000000": true})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	if _, err := v.Verify(revoked); !errors.Is(err, ErrRevokedJWT) {
		t.Errorf("Verify(revoked) err = %v, want ErrRevokedJWT", err)
	}
	if got := ClassifyVerifyErr(ErrRevokedJWT); got != "token revoked" {
		t.Errorf("ClassifyVerifyErr(ErrRevokedJWT) = %q", got)
	}
	if _, err := v.Verify(live); err != nil {
		t.Errorf("Verify(live token of the same operator) = %v, want nil", err)
	}
}

// A token minted before per-token accounting has no jti: it verifies until exp
// and is not refused by any revocation source.
func TestVerify_TokenWithoutJTIIsNotAskedAbout(t *testing.T) {
	tok := mustIssueLegacy(t)
	v, err := NewVerifier(testSigningKey, testIssuer, revokedIDs{"": true})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := v.Verify(tok); err != nil {
		t.Errorf("Verify(legacy token) = %v, want nil", err)
	}
}

func mustIssueLegacy(t *testing.T) string {
	t.Helper()
	now := time.Now()
	claims := archonClaims{RegisteredClaims: jwtv5.RegisteredClaims{
		Issuer:    testIssuer,
		Subject:   "archon-alice",
		IssuedAt:  jwtv5.NewNumericDate(now),
		ExpiresAt: jwtv5.NewNumericDate(now.Add(time.Hour)),
	}}
	tok, err := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims).SignedString(testSigningKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}
