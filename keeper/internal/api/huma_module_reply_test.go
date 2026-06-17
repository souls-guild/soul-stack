// GOLDEN byte-exact wire-guard для NATIVE wire-DTO MODULE-домена (handler-native T5d).
// module больше НЕ зависит от legacy-генерата — golden сверяет json native-значения с ЗАФИКСИРОВАННОЙ
// строкой-эталоном (pinned). Покрыт reply-роут form-prep: sids ([]string, required) + truncated;
// nil-ветка sids → wire `null`. Мутация формы native-struct (убрать поле / сменить json-тег /
// сменить тип) краснит case.
//
// ModuleCatalogReply/Item — УЖЕ native (N6, handler-local-структуры), здесь не дублируются.
package api

import (
	"encoding/json"
	"testing"
)

func goldenModuleWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_ModuleReply(t *testing.T) {
	// --- full: sids непустой + truncated ---
	sids := []string{"web1.example.com", "web2.example.com"}
	goldenModuleWire(t, "FormPrepReply/full",
		ModuleFormPrepReply{Sids: sids, Truncated: true},
		`{"sids":["web1.example.com","web2.example.com"],"truncated":true}`)
	// --- nil: пустой набор (handler даёт отсортированный срез; nil → wire `null`) ---
	goldenModuleWire(t, "FormPrepReply/nil_sids",
		ModuleFormPrepReply{Sids: nil, Truncated: false},
		`{"sids":null,"truncated":false}`)
}
