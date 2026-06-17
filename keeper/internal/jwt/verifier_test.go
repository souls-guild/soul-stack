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
	// Создаём токен с exp в прошлом — через manual ParseWithClaims-обход
	// нельзя (Issue не даёт negative ttl). Используем jwtv5 напрямую.
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

// TestVerify_RejectsNoneAlg — токен с `alg: none` (un-signed) должен
// отбраковываться независимо от подписи.
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

// TestVerify_MissingClaims — токен без обязательных claims (sub/iat/exp)
// → ErrInvalidToken.
func TestVerify_MissingSubject(t *testing.T) {
	claims := archonClaims{
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer: testIssuer,
			// Subject пустой намеренно.
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
