package oracle

import (
	"testing"
	"time"
)

func strptr(s string) *string { return &s }

func TestSubjectMatches_SID(t *testing.T) {
	d := &Decree{Name: "d", SubjectSID: strptr("host-a.example.com")}

	if !SubjectMatches(d, "host-a.example.com", nil) {
		t.Error("sid-Decree should match a matching SID")
	}
	if SubjectMatches(d, "host-b.example.com", []string{"web"}) {
		t.Error("sid-Decree must NOT match a different SID (covens are not considered)")
	}
}

func TestSubjectMatches_Coven(t *testing.T) {
	d := &Decree{Name: "d", SubjectCoven: []string{"web", "prod"}}

	if !SubjectMatches(d, "host-a", []string{"prod", "eu"}) {
		t.Error("coven-Decree should match on intersection (prod)")
	}
	if SubjectMatches(d, "host-a", []string{"db", "eu"}) {
		t.Error("coven-Decree must NOT match without an intersection")
	}
	if SubjectMatches(d, "host-a", nil) {
		t.Error("coven-Decree must NOT match a host with no covens")
	}
}

func TestSubjectMatches_EmptySubjectFailSafe(t *testing.T) {
	// The schema's XOR invariant won't allow such a row, but this is a fail-safe for a
	// programming error: empty subject → no match (default-deny).
	d := &Decree{Name: "d"}
	if SubjectMatches(d, "host-a", []string{"web"}) {
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
