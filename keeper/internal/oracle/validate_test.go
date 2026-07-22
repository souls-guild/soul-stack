package oracle

import (
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
)

func TestValidName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"web-conf", true},
		{"a", true},
		{"WEB", false},
		{"under_score", false},
		{"", false},
	}
	for _, c := range cases {
		if ValidName(c.name) != c.ok {
			t.Errorf("ValidName(%q) = %v, want %v", c.name, !c.ok, c.ok)
		}
	}
}

// TestValidCheckAddr checks the invariant (part of the "keeper-enum ==
// soul-registry == shared" invariant): keeper-enum knows EXACTLY the
// canonical [beaconaddr.All] set. The soul-side half (registry ==
// beaconaddr.All) is checked in soul/internal/beacon (ADR-011 forbids a
// keeper→soul import, so the shared source is shared/beaconaddr, and both
// sides check against it). Transitively this gives keeper-enum ==
// soul-registry — the root of the fixed S3 bug.
func TestValidCheckAddr(t *testing.T) {
	canonical := beaconaddr.All()
	for _, addr := range canonical {
		if !ValidCheckAddr(addr) {
			t.Errorf("%s should be a known core-beacon", addr)
		}
	}
	if len(canonical) != len(knownBeaconAddrs) {
		t.Errorf("keeper-enum (%d) is out of sync with beaconaddr.All (%d)", len(knownBeaconAddrs), len(canonical))
	}
	if ValidCheckAddr("core.beacon.bogus") {
		t.Error("unknown beacon should not pass")
	}
}

func TestValidIncarnationName(t *testing.T) {
	if !ValidIncarnationName("prod-db") {
		t.Error("prod-db is valid")
	}
	if ValidIncarnationName("BAD..NAME") {
		t.Error("BAD..NAME is invalid")
	}
	if ValidIncarnationName("-leading") {
		t.Error("leading hyphen is invalid")
	}
}

func TestValidScenario(t *testing.T) {
	if !ValidScenario("restart_service") {
		t.Error("restart_service is valid")
	}
	if ValidScenario("Bad-Scenario") {
		t.Error("Bad-Scenario is invalid (uppercase / hyphen)")
	}
}

func TestValidateSubjectXOR(t *testing.T) {
	sid := "h1.example"
	cases := []struct {
		name  string
		coven []string
		sid   *string
		ok    bool
	}{
		{"coven only", []string{"web"}, nil, true},
		{"sid only", nil, &sid, true},
		{"both → reject", []string{"web"}, &sid, false},
		{"neither → reject", nil, nil, false},
		{"empty coven + nil sid → reject", []string{}, nil, false},
		{"bad coven element", []string{"WEB"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSubjectXOR(c.coven, c.sid)
			if (err == nil) != c.ok {
				t.Errorf("validateSubjectXOR(%v, %v) err=%v, want ok=%v", c.coven, c.sid, err, c.ok)
			}
			if err != nil && !errors.Is(err, ErrValidation) {
				t.Errorf("error should be marked ErrValidation: %v", err)
			}
		})
	}
}

func TestValidateInterval(t *testing.T) {
	if err := validateInterval("30s"); err != nil {
		t.Errorf("30s is valid: %v", err)
	}
	if err := validateInterval("nope"); err == nil {
		t.Error("nope is invalid")
	}
	if err := validateInterval(""); err == nil {
		t.Error("empty interval is invalid")
	}
}

func TestValidateCooldown(t *testing.T) {
	if err := validateCooldown(""); err != nil {
		t.Errorf("empty cooldown is allowed (DEFAULT 0s): %v", err)
	}
	if err := validateCooldown("5m"); err != nil {
		t.Errorf("5m is valid: %v", err)
	}
	if err := validateCooldown("nope"); err == nil {
		t.Error("nope is invalid")
	}
}
