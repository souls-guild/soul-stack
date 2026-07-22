package jwt

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// leakMarker — a recognizable pattern inside the signing key. If the key
// value ever ends up in an error message (e.g. via a stray `%v` on []byte
// during a future edit), this marker will surface in err.Error() and fail
// the test. Key length is >= 32 bytes (HS256 minimum) so constructors
// don't reject it on length first.
var leakMarkerKey = []byte("SIGNKEY-MUST-NOT-LEAK-0123456789abcdef")

// assertNoKeyLeak is a shared helper: the error must contain neither the
// marker nor the full raw key value.
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

// TestSigningKey_NotLeaked_Constructors ensures NewIssuer/NewVerifier errors
// do not contain the key value (only its length).
func TestSigningKey_NotLeaked_Constructors(t *testing.T) {
	// A too-short key with a marker returns a length error; the marker must not
	// appear in it (len is logged, not the value).
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

// TestSigningKey_NotLeaked_IssueAndVerify ensures Issue (bad input) and Verify
// (bad signature / malformed) errors do not contain the key value.
func TestSigningKey_NotLeaked_IssueAndVerify(t *testing.T) {
	iss, err := NewIssuer(leakMarkerKey, "keeper.test")
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	// Issue with deliberately invalid input returns validation errors. The key
	// must not appear in them.
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

	// Verify with a verifier on a DIFFERENT key from the same marker family
	// returns bad-signature. err.Error() wraps the internal golang-jwt message
	// (verifier.go:138), but the key must not appear there.
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

	// Malformed token -> ErrInvalidToken with a wrapped JWT message; no key.
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
