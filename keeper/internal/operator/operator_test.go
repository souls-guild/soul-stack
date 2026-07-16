package operator

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValidAID(t *testing.T) {
	cases := []struct {
		name string
		aid  string
		want bool
	}{
		// Positive: legacy archon form + new external names
		// (ADR-014 amendment 2026-05-29 — prefix dropped, charset a-z0-9._@-).
		{"legacy-archon", "archon-alice", true},
		{"legacy-with-digits", "archon-ops-01", true},
		{"multi-dash", "archon-team-db-prod", true},
		{"plain-name", "alice", true},
		{"arbitrary-prefix", "user-alice", true},
		{"email-like", "alice@corp.com", true},
		{"underscore", "uid_4815", true},
		{"starts-with-digit", "0day", true},
		{"two-chars-min", "ab", true},
		{"max-length-128", "a" + repeat("b", 127), true},

		// Negative.
		{"empty", "", false},
		{"single-char-too-short", "a", false},
		{"uppercase", "archon-Alice", false},
		{"starts-with-dot", ".hidden", false},
		{"starts-with-dash", "-leading", false},
		{"starts-with-at", "@alice", false},
		{"path-traversal-slash", "archon/../evil", false},
		{"backslash", "archon\\evil", false},
		{"too-long-129", "a" + repeat("b", 128), false},
		{"trailing-space", "archon-alice ", false},
		{"leading-space", " archon-alice", false},
		{"invalid-char", "archon:alice", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidAID(tc.aid); got != tc.want {
				t.Errorf("ValidAID(%q) = %v, want %v", tc.aid, got, tc.want)
			}
		})
	}
}

func TestOperator_IsRevoked(t *testing.T) {
	active := &Operator{AID: "archon-alice"}
	if active.IsRevoked() {
		t.Error("active operator: IsRevoked() = true, want false")
	}
	now := time.Now().UTC()
	revoked := &Operator{AID: "archon-bob", RevokedAt: &now}
	if !revoked.IsRevoked() {
		t.Error("revoked operator: IsRevoked() = false, want true")
	}
}

func TestOperator_IsBootstrap(t *testing.T) {
	// ADR-058(d): IsBootstrap determined by created_via='bootstrap', NOT by
	// created_by_aid==nil. First Archon.
	first := &Operator{AID: "archon-alice", CreatedVia: CreatedViaBootstrap}
	if !first.IsBootstrap() {
		t.Error("first operator: IsBootstrap() = false, want true")
	}
	// Operator created by another Archon (created_via='user').
	parent := "archon-alice"
	derived := &Operator{AID: "archon-bob", CreatedByAID: &parent, CreatedVia: CreatedViaUser}
	if derived.IsBootstrap() {
		t.Error("derived operator: IsBootstrap() = true, want false")
	}
	// ADR-058(d) guard: archon-system / federated operators also have
	// created_by_aid=nil, but are NOT bootstrap — created_via distinguishes.
	sys := &Operator{AID: "archon-system", CreatedVia: CreatedViaSystem}
	if sys.IsBootstrap() {
		t.Error("system operator (created_by_aid=nil, created_via=system): IsBootstrap() = true, want false")
	}
	fed := &Operator{AID: "alice", CreatedVia: CreatedViaLDAP}
	if fed.IsBootstrap() {
		t.Error("federated operator (created_by_aid=nil, created_via=ldap): IsBootstrap() = true, want false")
	}
}

// TestOperator_JSONMarshal — JSON tags will be useful in Operator API (M0.6).
// Minimal check: nil pointers and nil maps must not render
// (omitempty), so payloads of audit events and API responses remain
// compact.
func TestOperator_JSONMarshal(t *testing.T) {
	op := &Operator{
		AID:         "archon-alice",
		DisplayName: "Alice",
		AuthMethod:  AuthMethodJWT,
		CreatedAt:   time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"aid":"archon-alice"`,
		`"auth_method":"jwt"`,
		`"created_at":"2026-05-22T12:00:00Z"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Marshal output missing %q\n got: %s", want, s)
		}
	}
	for _, banned := range []string{"created_by_aid", "revoked_at", "metadata"} {
		if strings.Contains(s, banned) {
			t.Errorf("Marshal output must omit %q (omitempty)\n got: %s", banned, s)
		}
	}
}

func repeat(s string, n int) string { return strings.Repeat(s, n) }
