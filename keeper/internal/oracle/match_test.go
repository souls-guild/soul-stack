package oracle

import (
	"testing"
	"time"
)

func strptr(s string) *string { return &s }

func TestSubjectMatches_SID(t *testing.T) {
	d := &Decree{Name: "d", SubjectSID: strptr("host-a.example.com")}

	if !SubjectMatches(d, "host-a.example.com", nil) {
		t.Error("sid-Decree должен сматчить совпадающий SID")
	}
	if SubjectMatches(d, "host-b.example.com", []string{"web"}) {
		t.Error("sid-Decree НЕ должен матчить другой SID (covens не учитываются)")
	}
}

func TestSubjectMatches_Coven(t *testing.T) {
	d := &Decree{Name: "d", SubjectCoven: []string{"web", "prod"}}

	if !SubjectMatches(d, "host-a", []string{"prod", "eu"}) {
		t.Error("coven-Decree должен сматчить при пересечении (prod)")
	}
	if SubjectMatches(d, "host-a", []string{"db", "eu"}) {
		t.Error("coven-Decree НЕ должен матчить без пересечения")
	}
	if SubjectMatches(d, "host-a", nil) {
		t.Error("coven-Decree НЕ должен матчить хост без covens")
	}
}

func TestSubjectMatches_EmptySubjectFailSafe(t *testing.T) {
	// The schema's XOR invariant won't allow such a row, but this is a fail-safe for a
	// programming error: empty subject → no match (default-deny).
	d := &Decree{Name: "d"}
	if SubjectMatches(d, "host-a", []string{"web"}) {
		t.Error("Decree без субъекта должен давать no-match (fail-safe default-deny)")
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
		{"никогда не срабатывал", "5m", time.Time{}, false, false},
		{"cooldown выключен (0s)", "0s", now.Add(-1 * time.Second), true, false},
		{"в окне cooldown", "5m", now.Add(-1 * time.Minute), true, true},
		{"ровно на границе (>= cooldown → нет)", "5m", now.Add(-5 * time.Minute), true, false},
		{"за окном cooldown", "5m", now.Add(-10 * time.Minute), true, false},
		{"битый формат → cooldown выключен", "nonsense", now.Add(-1 * time.Second), true, false},
		{"суффикс дня (1d)", "1d", now.Add(-1 * time.Hour), true, true},
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
