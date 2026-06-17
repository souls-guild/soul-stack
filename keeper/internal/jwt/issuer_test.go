package jwt

import (
	"bytes"
	"strings"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// testSigningKey — 32-байтовый ключ, минимум для HS256.
var testSigningKey = bytes.Repeat([]byte{0xab}, 32)

func TestNewIssuer_RejectsShortKey(t *testing.T) {
	tests := []struct {
		name    string
		keyLen  int
		wantErr bool
	}{
		{"empty", 0, true},
		{"31_bytes", 31, true},
		{"32_bytes_min", 32, false},
		{"64_bytes", 64, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := bytes.Repeat([]byte{0x01}, tt.keyLen)
			iss, err := NewIssuer(key, "keeper.test")
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewIssuer(len=%d): err=%v, wantErr=%v", tt.keyLen, err, tt.wantErr)
			}
			if !tt.wantErr && iss == nil {
				t.Fatalf("NewIssuer: nil issuer without error")
			}
		})
	}
}

func TestNewIssuer_RejectsEmptyIssuer(t *testing.T) {
	if _, err := NewIssuer(testSigningKey, ""); err == nil {
		t.Fatalf("NewIssuer with empty issuer: expected error, got nil")
	}
}

// TestIssue_ClaimsCorrect — decode результирующий JWT и проверить все поля.
func TestIssue_ClaimsCorrect(t *testing.T) {
	const (
		issuer = "keeper.test"
		aid    = "archon-alice"
	)
	roles := []string{"cluster-admin", "read-only"}
	ttl := 30 * 24 * time.Hour

	iss, err := NewIssuer(testSigningKey, issuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	before := time.Now().UTC()
	tok, err := iss.Issue(aid, roles, ttl, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	after := time.Now().UTC()

	// Структура: header.payload.signature.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT must have 3 parts, got %d: %s", len(parts), tok)
	}

	// Декодируем claims через jwtv5 (тот же лагоритм). KeyFunc возвращает
	// тот же signing key — иначе ParseWithClaims отбракует подпись.
	parsed, err := jwtv5.ParseWithClaims(tok, &archonClaims{}, func(t *jwtv5.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwtv5.SigningMethodHMAC); !ok {
			return nil, jwtv5.ErrTokenSignatureInvalid
		}
		return testSigningKey, nil
	})
	if err != nil {
		t.Fatalf("ParseWithClaims: %v", err)
	}
	if !parsed.Valid {
		t.Fatalf("token invalid")
	}
	claims, ok := parsed.Claims.(*archonClaims)
	if !ok {
		t.Fatalf("claims type = %T, want *archonClaims", parsed.Claims)
	}
	if claims.Issuer != issuer {
		t.Errorf("iss = %q, want %q", claims.Issuer, issuer)
	}
	if claims.Subject != aid {
		t.Errorf("sub = %q, want %q", claims.Subject, aid)
	}
	if claims.IssuedAt == nil {
		t.Fatalf("iat = nil")
	}
	if iat := claims.IssuedAt.Time; iat.Before(before.Add(-time.Second)) || iat.After(after.Add(time.Second)) {
		t.Errorf("iat = %v, outside [%v, %v]", iat, before, after)
	}
	if claims.ExpiresAt == nil {
		t.Fatalf("exp = nil")
	}
	expWant := before.Add(ttl)
	if exp := claims.ExpiresAt.Time; exp.Before(expWant.Add(-2*time.Second)) || exp.After(expWant.Add(2*time.Second)) {
		t.Errorf("exp = %v, want around %v", exp, expWant)
	}
	if got, want := claims.Roles, roles; !equalStrings(got, want) {
		t.Errorf("roles = %v, want %v", got, want)
	}
	if claims.BootstrapInitial {
		t.Errorf("bootstrap_initial = true, want false")
	}
}

func TestIssue_BootstrapInitial(t *testing.T) {
	iss, err := NewIssuer(testSigningKey, "keeper.test")
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue("archon-bootstrap", []string{"cluster-admin"}, time.Hour, true)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	parsed, err := jwtv5.ParseWithClaims(tok, &archonClaims{}, func(*jwtv5.Token) (interface{}, error) {
		return testSigningKey, nil
	})
	if err != nil {
		t.Fatalf("ParseWithClaims: %v", err)
	}
	claims := parsed.Claims.(*archonClaims)
	if !claims.BootstrapInitial {
		t.Errorf("bootstrap_initial = false, want true")
	}
}

// TestIssue_Signature — токен подписан HS256 нашим ключом; чужой ключ
// должен отбраковать подпись.
func TestIssue_Signature(t *testing.T) {
	iss, err := NewIssuer(testSigningKey, "keeper.test")
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue("archon-alice", []string{"cluster-admin"}, time.Hour, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Чужой ключ → подпись невалидна.
	otherKey := bytes.Repeat([]byte{0xcd}, 32)
	_, err = jwtv5.ParseWithClaims(tok, &archonClaims{}, func(*jwtv5.Token) (interface{}, error) {
		return otherKey, nil
	})
	if err == nil {
		t.Fatalf("ParseWithClaims with wrong key: expected error, got nil")
	}

	// Свой ключ → подпись валидна.
	parsed, err := jwtv5.ParseWithClaims(tok, &archonClaims{}, func(*jwtv5.Token) (interface{}, error) {
		return testSigningKey, nil
	})
	if err != nil {
		t.Fatalf("ParseWithClaims with correct key: %v", err)
	}
	if !parsed.Valid {
		t.Fatalf("token invalid with correct key")
	}
	if alg := parsed.Method.Alg(); alg != "HS256" {
		t.Errorf("alg = %q, want HS256", alg)
	}
}

func TestIssue_RejectsBadInput(t *testing.T) {
	iss, err := NewIssuer(testSigningKey, "keeper.test")
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	if _, err := iss.Issue("", []string{"cluster-admin"}, time.Hour, false); err == nil {
		t.Errorf("Issue with empty aid: expected error")
	}
	if _, err := iss.Issue("archon-x", nil, 0, false); err == nil {
		t.Errorf("Issue with zero ttl: expected error")
	}
	if _, err := iss.Issue("archon-x", nil, -time.Hour, false); err == nil {
		t.Errorf("Issue with negative ttl: expected error")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
