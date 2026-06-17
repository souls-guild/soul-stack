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
		{"под_чёрк", false},
		{"", false},
	}
	for _, c := range cases {
		if ValidName(c.name) != c.ok {
			t.Errorf("ValidName(%q) = %v, want %v", c.name, !c.ok, c.ok)
		}
	}
}

// TestValidCheckAddr проверяет инвариант (часть инварианта «keeper-enum ==
// soul-registry == shared»): keeper-enum знает РОВНО канонический набор
// [beaconaddr.All]. Soul-side половина (registry == beaconaddr.All) проверяется
// в soul/internal/beacon (ADR-011 запрещает keeper→soul import, поэтому общий
// источник — shared/beaconaddr, и обе стороны сверяются с ним). Транзитивно это
// даёт keeper-enum == soul-registry — корень устранённого S3-бага.
func TestValidCheckAddr(t *testing.T) {
	canonical := beaconaddr.All()
	for _, addr := range canonical {
		if !ValidCheckAddr(addr) {
			t.Errorf("%s должен быть известным core-beacon", addr)
		}
	}
	if len(canonical) != len(knownBeaconAddrs) {
		t.Errorf("keeper-enum (%d) рассинхронен с beaconaddr.All (%d)", len(knownBeaconAddrs), len(canonical))
	}
	if ValidCheckAddr("core.beacon.bogus") {
		t.Error("неизвестный beacon не должен проходить")
	}
}

func TestValidIncarnationName(t *testing.T) {
	if !ValidIncarnationName("prod-db") {
		t.Error("prod-db валиден")
	}
	if ValidIncarnationName("BAD..NAME") {
		t.Error("BAD..NAME невалиден")
	}
	if ValidIncarnationName("-leading") {
		t.Error("ведущий дефис невалиден")
	}
}

func TestValidScenario(t *testing.T) {
	if !ValidScenario("restart_service") {
		t.Error("restart_service валиден")
	}
	if ValidScenario("Bad-Scenario") {
		t.Error("Bad-Scenario невалиден (заглавные / дефис)")
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
				t.Errorf("ошибка должна быть помечена ErrValidation: %v", err)
			}
		})
	}
}

func TestValidateInterval(t *testing.T) {
	if err := validateInterval("30s"); err != nil {
		t.Errorf("30s валиден: %v", err)
	}
	if err := validateInterval("nope"); err == nil {
		t.Error("nope невалиден")
	}
	if err := validateInterval(""); err == nil {
		t.Error("пустой interval невалиден")
	}
}

func TestValidateCooldown(t *testing.T) {
	if err := validateCooldown(""); err != nil {
		t.Errorf("пустой cooldown допустим (DEFAULT 0s): %v", err)
	}
	if err := validateCooldown("5m"); err != nil {
		t.Errorf("5m валиден: %v", err)
	}
	if err := validateCooldown("nope"); err == nil {
		t.Error("nope невалиден")
	}
}
