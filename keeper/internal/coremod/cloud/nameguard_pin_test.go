package cloud

import (
	"os"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// nameGuardProvisionScenarios — provision-тела, куда NIM-58 вписал guard-assert.
// Путь относительно пакета keeper/internal/coremod/cloud (4 уровня до корня репо).
var nameGuardProvisionScenarios = []string{
	"../../../../examples/service/redis/scenario/redis-provision.yml",
	"../../../../examples/service/dragonfly/scenario/dragonfly-provision.yml",
}

// TestProvisionScenariosPinVMNameBasePattern — pin-guard NIM-58: каждое
// provision-тело обязано сверять incarnation.name РОВНО против [VMNameBasePattern]
// (единый источник паттерна). Ловит дрейф YAML-литерала vs Go-const — главный
// риск варианта B (assert-строка и драйвер разъедутся молча).
func TestProvisionScenariosPinVMNameBasePattern(t *testing.T) {
	want := "matches('" + VMNameBasePattern + "')"
	for _, path := range nameGuardProvisionScenarios {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s: guard-assert %q отсутствует — YAML-литерал разошёлся с cloud.VMNameBasePattern (NIM-58 pin-дрейф); синхронизируй assert в provision-теле с Go-const", path, want)
		}
	}
}

// TestVMNameBaseDomainMismatch — домен-mismatch NIM-58: incarnation.NamePattern
// (^[a-z0-9][a-z0-9-]{0,62}$) — НАДМНОЖЕСТВО VMNameBasePattern. «Плохие» имена
// проходят create инкарнации (ValidName=true), но валят драйвер провижна
// (ValidVMNameBase=false) — ровно тот разрыв, что guard-assert закрывает pre-persist.
func TestVMNameBaseDomainMismatch(t *testing.T) {
	long51 := strings.Repeat("a", 51)       // > 50 → валит VM-базу, но ≤ 63 → имя ок
	long63 := strings.Repeat("a", 63)       // предельная длина имени инкарнации
	good48 := "r" + strings.Repeat("a", 47) // ровно 48 → годно обоим доменам

	cases := []struct {
		name      string
		value     string
		validName bool // валидное имя инкарнации (incarnation.ValidName)
		validVM   bool // валидная VM-база (cloud.VMNameBaseRe)
	}{
		// Домен-mismatch: имя инкарнации ок, VM-база НЕТ (create пройдёт, драйвер упадёт).
		{"start-digit", "9redis", true, false},
		{"start-digit-dash", "01-cache", true, false},
		{"trailing-dash", "redis-cache-", true, false},
		{"len-51", long51, true, false},
		{"len-63", long63, true, false},
		// Согласованные: годны и как имя инкарнации, и как VM-база.
		{"plain", "redis", true, true},
		{"with-dash", "redis-prod", true, true},
		{"len-48", good48, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := incarnation.ValidName(tc.value); got != tc.validName {
				t.Errorf("incarnation.ValidName(%q) = %v, want %v", tc.value, got, tc.validName)
			}
			if got := VMNameBaseRe.MatchString(tc.value); got != tc.validVM {
				t.Errorf("cloud.VMNameBaseRe.MatchString(%q) = %v, want %v", tc.value, got, tc.validVM)
			}
		})
	}
}
