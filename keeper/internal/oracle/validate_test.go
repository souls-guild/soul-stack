package oracle

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
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

// boolRow — a one-column row scanning into *bool, for the SELECT EXISTS behind
// [subject.ExistsIncarnation].
type boolRow bool

func (r boolRow) Scan(dest ...any) error {
	if len(dest) == 1 {
		if p, ok := dest[0].(*bool); ok {
			*p = bool(r)
		}
	}
	return nil
}

// TestValidateSubject_ExactlyOneOf pins the exactly-one-of-four invariant the DB
// CHECK `*_subject_one_of` states declaratively. Two dimensions is as invalid as
// none, and a pair dimension half-written (an incarnation with no service, a trait
// key with no value) is not "partially specified" — it is invalid, because the
// missing half is part of the address and not a default.
func TestValidateSubject_ExactlyOneOf(t *testing.T) {
	svc := newTestService(t, &fakeDB{queryRowRow: boolRow(true)})
	cases := []struct {
		name string
		sel  subject.Selector
		ok   bool
	}{
		{"coven only", subject.Selector{Covens: []string{"web"}}, true},
		{"sid only", subject.Selector{SIDs: []string{"h1.example"}}, true},
		{"incarnation only", subject.Selector{Service: "redis", Incarnation: "redis-prod"}, true},
		{"trait only", subject.Selector{TraitKey: "tier", TraitValue: "gold"}, true},
		{"coven + sid -> reject", subject.Selector{Covens: []string{"web"}, SIDs: []string{"h1.example"}}, false},
		{"coven + incarnation -> reject", subject.Selector{Covens: []string{"web"}, Service: "redis", Incarnation: "redis-prod"}, false},
		{"none -> reject", subject.Selector{}, false},
		{"empty coven slice -> reject", subject.Selector{Covens: []string{}}, false},
		{"bad coven element", subject.Selector{Covens: []string{"WEB"}}, false},
		{"incarnation without service -> reject", subject.Selector{Incarnation: "redis-prod"}, false},
		{"service without incarnation -> reject", subject.Selector{Service: "redis"}, false},
		{"trait key without value -> reject", subject.Selector{TraitKey: "tier"}, false},
		{"trait value without key -> reject", subject.Selector{TraitValue: "gold"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := svc.validateSubject(context.Background(), c.sel)
			if (err == nil) != c.ok {
				t.Errorf("validateSubject(%+v) err=%v, want ok=%v", c.sel, err, c.ok)
			}
			if err != nil && !errors.Is(err, ErrValidation) {
				t.Errorf("error should be marked ErrValidation: %v", err)
			}
		})
	}
}

// TestValidateSubject_IncarnationMustExist — an `incarnation=` subject that names
// nothing is rejected on create rather than accepted as a rule that silently reaches
// no host. A typo here is invisible at runtime: an operator sees a rule that has
// simply not fired yet, which looks identical to one that never can.
func TestValidateSubject_IncarnationMustExist(t *testing.T) {
	sel := subject.Selector{Service: "redis", Incarnation: "redis-typo"}

	svc := newTestService(t, &fakeDB{queryRowRow: boolRow(false)})
	err := svc.validateSubject(context.Background(), sel)
	if !errors.Is(err, ErrValidation) {
		t.Errorf("unknown incarnation err = %v, want ErrValidation", err)
	}

	svc = newTestService(t, &fakeDB{queryRowRow: boolRow(true)})
	if err := svc.validateSubject(context.Background(), sel); err != nil {
		t.Errorf("existing incarnation must pass: %v", err)
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
