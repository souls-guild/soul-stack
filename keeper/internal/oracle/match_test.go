package oracle

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
)

func strptr(s string) *string { return &s }

// hostA — a bare host with no labels and no membership: the baseline every
// dimension but `sid` must fail to reach.
func hostA(sid string) subject.Host { return subject.Host{SID: sid} }

func TestSubjectMatches_SID(t *testing.T) {
	d := &Decree{ID: "d", SubjectSID: []string{"host-a.example.com"}}

	if !SubjectMatches(d, hostA("host-a.example.com")) {
		t.Error("sid-Decree should match a matching SID")
	}
	if SubjectMatches(d, subject.Host{SID: "host-b.example.com", Covens: []string{"web"}}) {
		t.Error("sid-Decree must NOT match a different SID (covens are not considered)")
	}
}

func TestSubjectMatches_Coven(t *testing.T) {
	d := &Decree{ID: "d", SubjectCoven: []string{"web", "prod"}}

	if !SubjectMatches(d, subject.Host{SID: "host-a", Covens: []string{"prod", "eu"}}) {
		t.Error("coven-Decree should match on intersection (prod)")
	}
	if SubjectMatches(d, subject.Host{SID: "host-a", Covens: []string{"db", "eu"}}) {
		t.Error("coven-Decree must NOT match without an intersection")
	}
	if SubjectMatches(d, hostA("host-a")) {
		t.Error("coven-Decree must NOT match a host with no covens")
	}
}

// TestSubjectMatches_CovenReachesIncarnationMembers pins the second level of the
// label dimensions (NIM-280): a coven put on an INCARNATION reaches every host that
// is a member of it, even though the host itself carries no such label. This is what
// makes "the hosts of this incarnation" expressible again after NIM-281 removed
// inheritance — the label is still only where an operator attached it, but a rule
// reads both places.
func TestSubjectMatches_CovenReachesIncarnationMembers(t *testing.T) {
	d := &Decree{ID: "d", SubjectCoven: []string{"prod"}}

	member := subject.Host{
		SID:    "host-a",
		Covens: nil, // the host itself is unlabelled
		Member: []subject.Incarnation{{Service: "redis", Name: "redis-prod", Covens: []string{"prod"}}},
	}
	if !SubjectMatches(d, member) {
		t.Error("coven-Decree should reach a member of an incarnation carrying the label")
	}

	other := subject.Host{
		SID:    "host-b",
		Member: []subject.Incarnation{{Service: "redis", Name: "redis-stage", Covens: []string{"stage"}}},
	}
	if SubjectMatches(d, other) {
		t.Error("coven-Decree must NOT reach a member of an incarnation carrying a different label")
	}
}

// TestSubjectMatches_Incarnation is the NIM-280 acceptance criterion itself: an
// `incarnation=` subject reaches the hosts that are MEMBERS of that incarnation, and
// membership is the only thing it reads. A host merely tagged with a coven spelled
// like the incarnation is not a member and must not be reached — that conflation was
// exactly the escalation NIM-281 closed.
func TestSubjectMatches_Incarnation(t *testing.T) {
	d := &Decree{
		ID:                 "d",
		SubjectService:     strptr("redis"),
		SubjectIncarnation: strptr("redis-prod"),
	}

	member := subject.Host{
		SID:    "host-a",
		Member: []subject.Incarnation{{Service: "redis", Name: "redis-prod"}},
	}
	if !SubjectMatches(d, member) {
		t.Error("incarnation-Decree should reach a member of that incarnation")
	}

	lookalike := subject.Host{SID: "host-b", Covens: []string{"redis-prod"}}
	if SubjectMatches(d, lookalike) {
		t.Error("incarnation-Decree must NOT reach a host merely tagged with a coven of the same spelling")
	}

	// The name is unique only within its service, so the service half is part of
	// the address and not decoration: same name, different service → no match.
	otherService := subject.Host{
		SID:    "host-c",
		Member: []subject.Incarnation{{Service: "valkey", Name: "redis-prod"}},
	}
	if SubjectMatches(d, otherService) {
		t.Error("incarnation-Decree must NOT reach the same name under a different service")
	}
}

func TestSubjectMatches_Trait(t *testing.T) {
	d := &Decree{ID: "d", SubjectTraitKey: strptr("tier"), SubjectTraitValue: strptr("gold")}

	own := subject.Host{SID: "host-a", Traits: map[string]any{"tier": "gold"}}
	if !SubjectMatches(d, own) {
		t.Error("trait-Decree should match a host carrying the pair")
	}
	if SubjectMatches(d, subject.Host{SID: "host-a", Traits: map[string]any{"tier": "silver"}}) {
		t.Error("trait-Decree must NOT match a different value under the same key")
	}
	if SubjectMatches(d, hostA("host-a")) {
		t.Error("trait-Decree must NOT match a host with no traits")
	}

	// Same two-level reach as coven: a trait on the incarnation reaches its members.
	member := subject.Host{
		SID: "host-b",
		Member: []subject.Incarnation{
			{Service: "redis", Name: "redis-prod", Traits: map[string]any{"tier": "gold"}},
		},
	}
	if !SubjectMatches(d, member) {
		t.Error("trait-Decree should reach a member of an incarnation carrying the pair")
	}
}

func TestSubjectMatches_EmptySubjectFailSafe(t *testing.T) {
	// The schema's exactly-one-of invariant won't allow such a row, but this is a
	// fail-safe for a programming error: empty subject → no match (default-deny).
	d := &Decree{ID: "d"}
	full := subject.Host{
		SID:    "host-a",
		Covens: []string{"web"},
		Traits: map[string]any{"tier": "gold"},
		Member: []subject.Incarnation{{Service: "redis", Name: "redis-prod"}},
	}
	if SubjectMatches(d, full) {
		t.Error("Decree with no subject should yield no-match (fail-safe default-deny)")
	}
}

func TestWithinCooldown(t *testing.T) {
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		cooldown  string
		lastFired time.Time
		hasFired  bool
		want      bool
	}{
		{"never fired", "5m", time.Time{}, false, false},
		{"cooldown disabled (0s)", "0s", now.Add(-1 * time.Second), true, false},
		{"inside the cooldown window", "5m", now.Add(-1 * time.Minute), true, true},
		{"exactly on the boundary (>= cooldown -> no)", "5m", now.Add(-5 * time.Minute), true, false},
		{"outside the cooldown window", "5m", now.Add(-10 * time.Minute), true, false},
		{"malformed format -> cooldown disabled", "nonsense", now.Add(-1 * time.Second), true, false},
		{"day suffix (1d)", "1d", now.Add(-1 * time.Hour), true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := WithinCooldown(c.cooldown, c.lastFired, c.hasFired, now)
			if got != c.want {
				t.Errorf("WithinCooldown(%q, fired=%v) = %v, want %v", c.cooldown, c.hasFired, got, c.want)
			}
		})
	}
}
