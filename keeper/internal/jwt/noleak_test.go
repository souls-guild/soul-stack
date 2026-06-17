package jwt

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// leakMarker — распознаваемый паттерн внутри signing-key. Если значение
// ключа когда-либо попадёт в текст ошибки (например, через случайный `%v`
// на []byte при правке), этот маркер всплывёт в err.Error() и тест упадёт.
// Длина ключа >= 32 байт (HS256-минимум), чтобы конструкторы не отбраковали
// его раньше по длине.
var leakMarkerKey = []byte("SIGNKEY-MUST-NOT-LEAK-0123456789abcdef")

// assertNoKeyLeak — общий помощник: ошибка не должна содержать ни маркер,
// ни сырое значение ключа целиком.
func assertNoKeyLeak(t *testing.T, where string, err error, key []byte) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "SIGNKEY-MUST-NOT-LEAK") {
		t.Errorf("%s: signing key marker leaked into error: %q", where, msg)
	}
	if bytes.Contains([]byte(msg), key) {
		t.Errorf("%s: raw signing key leaked into error: %q", where, msg)
	}
}

// TestSigningKey_NotLeaked_Constructors — ошибки NewIssuer/NewVerifier не
// содержат значения ключа (только его длину).
func TestSigningKey_NotLeaked_Constructors(t *testing.T) {
	// Слишком короткий ключ с маркером → ошибка по длине; маркера в ней быть
	// не должно (логируется len, а не значение).
	shortKeyWithMarker := []byte("SIGNKEY-MUST-NOT-LEAK")

	_, errIss := NewIssuer(shortKeyWithMarker, "keeper.test")
	if errIss == nil {
		t.Fatalf("NewIssuer with short key: expected length error")
	}
	assertNoKeyLeak(t, "NewIssuer", errIss, shortKeyWithMarker)

	_, errVer := NewVerifier(shortKeyWithMarker, "keeper.test")
	if errVer == nil {
		t.Fatalf("NewVerifier with short key: expected length error")
	}
	assertNoKeyLeak(t, "NewVerifier", errVer, shortKeyWithMarker)
}

// TestSigningKey_NotLeaked_IssueAndVerify — ошибки Issue (bad input) и Verify
// (bad signature / malformed) не содержат значения ключа.
func TestSigningKey_NotLeaked_IssueAndVerify(t *testing.T) {
	iss, err := NewIssuer(leakMarkerKey, "keeper.test")
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	// Issue с заведомо невалидным input → ошибки валидации. Ключ в них
	// фигурировать не должен.
	if _, err := iss.Issue("", []string{"x"}, time.Hour, false); err == nil {
		t.Fatalf("Issue empty aid: expected error")
	} else {
		assertNoKeyLeak(t, "Issue(empty aid)", err, leakMarkerKey)
	}
	if _, err := iss.Issue("archon-x", nil, -time.Hour, false); err == nil {
		t.Fatalf("Issue non-positive ttl: expected error")
	} else {
		assertNoKeyLeak(t, "Issue(bad ttl)", err, leakMarkerKey)
	}

	// Verify с verifier-ом на ДРУГОМ ключе того же маркера-семейства →
	// bad-signature. err.Error() оборачивает внутреннее сообщение
	// golang-jwt (verifier.go:138), но ключа там быть не должно.
	tok, err := iss.Issue("archon-alice", []string{"cluster-admin"}, time.Hour, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	otherKey := []byte("OTHER-SIGNKEY-MUST-NOT-LEAK-0123456789")
	ver, err := NewVerifier(otherKey, "keeper.test")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := ver.Verify(tok); err == nil {
		t.Fatalf("Verify with wrong key: expected bad-signature error")
	} else {
		assertNoKeyLeak(t, "Verify(bad signature, issuer key)", err, leakMarkerKey)
		assertNoKeyLeak(t, "Verify(bad signature, verifier key)", err, otherKey)
	}

	// Malformed token → ErrInvalidToken c обёрнутым jwt-сообщением; ключа нет.
	ver2, err := NewVerifier(leakMarkerKey, "keeper.test")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := ver2.Verify("not.a.jwt"); err == nil {
		t.Fatalf("Verify malformed: expected error")
	} else {
		assertNoKeyLeak(t, "Verify(malformed)", err, leakMarkerKey)
	}
}
