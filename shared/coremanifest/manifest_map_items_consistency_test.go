// ADR-045 S7-amend — согласованность объявления `items` под `type: map`
// (тип значения, map[string]<items>) между встроенным core-манифестом и тем,
// что валидатор `params:` реально принимает.
//
// Покрывает два кейса, симметрично list-полю:
//   - map + items.type=string — плоская строковая map (env/headers/vars): items
//     задан, тип значения скалярный → UI рисует KEY→VALUE-редактор;
//   - map без items — произвольная структура (cloud profile) → UI рисует JSON.
//
// Файл физически в shared/coremanifest/ (внешний тест-пакет), production-код не
// трогает — та же мотивация, что у manifest_validator_consistency_test.go.
package coremanifest_test

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// mapFieldAddr — адрес одного map-параметра core-модуля для проверки items.
type mapFieldAddr struct {
	module, state, param string
}

// TestS7Amend_MapValueItemsDeclared — плоские строковые map несут
// items.type=string (KEY→VALUE-редактор в UI), а произвольная cloud-profile —
// БЕЗ items (JSON-редактор). Источник правды — встроенный coremanifest.
func TestS7Amend_MapValueItemsDeclared(t *testing.T) {
	reg := coremanifest.Default()

	lookup := func(f mapFieldAddr) (plugin.InputParamDef, bool) {
		def, ok := reg.State(f.module, f.state)
		if !ok {
			return plugin.InputParamDef{}, false
		}
		p, ok := def.Input[f.param]
		return p, ok
	}

	withStringItems := []mapFieldAddr{
		{"core.cmd", "shell", "env"},
		{"core.exec", "run", "env"},
		{"core.file", "rendered", "vars"},
		{"core.http", "probe", "headers"},
		{"core.url", "fetched", "headers"},
	}
	for _, f := range withStringItems {
		p, ok := lookup(f)
		if !ok {
			t.Errorf("%s.%s.%s: параметр не найден в coremanifest", f.module, f.state, f.param)
			continue
		}
		if p.Type != "map" {
			t.Errorf("%s.%s.%s: ожидали type=map, получили %q", f.module, f.state, f.param, p.Type)
		}
		if p.Items == nil {
			t.Errorf("%s.%s.%s: ожидали items (тип значения map), получили nil", f.module, f.state, f.param)
			continue
		}
		if p.Items.Type != "string" {
			t.Errorf("%s.%s.%s: ожидали items.type=string, получили %q", f.module, f.state, f.param, p.Items.Type)
		}
	}

	// cloud profile — намеренно без items (произвольная структура → JSON в UI).
	profile, ok := lookup(mapFieldAddr{"core.cloud", "created", "profile"})
	if !ok {
		t.Fatal("core.cloud.created.profile не найден в coremanifest")
	}
	if profile.Type != "map" {
		t.Errorf("core.cloud.created.profile: ожидали type=map, получили %q", profile.Type)
	}
	if profile.Items != nil {
		t.Errorf("core.cloud.created.profile: ожидали БЕЗ items (произвольная структура), получили %+v", profile.Items)
	}
}

// TestS7Amend_MapWithItemsValidatorAccepts — declared map+items не вызывает
// items-ошибок у manifest-валидатора, а задача с map-параметром проходит
// config-валидатор (публичный путь, как в TestP5_*).
func TestS7Amend_MapWithItemsValidatorAccepts(t *testing.T) {
	const withItems = `kind: soul_module
protocol_version: 1
namespace: core
name: probe
spec:
  states:
    s:
      input:
        env: { type: map, items: { type: string } }
`
	if _, diags := plugin.LoadFromBytes("manifest.yaml", []byte(withItems)); hasItemsErr(diags) {
		t.Errorf("map+items.type=string дал items-ошибку: %v", diags)
	}

	const noItems = `kind: soul_module
protocol_version: 1
namespace: core
name: probe
spec:
  states:
    s:
      input:
        profile: { type: map }
`
	if _, diags := plugin.LoadFromBytes("manifest.yaml", []byte(noItems)); hasItemsErr(diags) {
		t.Errorf("map без items дал items-ошибку: %v", diags)
	}

	src := "- name: probe\n  module: core.cmd.shell\n  params:\n    cmd: \"echo hi\"\n    env: { FOO: bar }\n"
	if _, diags, _ := config.LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), config.ValidateOptions{}); hasMapValueErr(diags) {
		t.Errorf("задача с map-параметром env дала items/value-ошибку: %v", diags)
	}
}

func hasItemsErr(diags []diag.Diagnostic) bool {
	for _, d := range diags {
		if strings.HasPrefix(d.Code, "input_items_") {
			return true
		}
	}
	return false
}

func hasMapValueErr(diags []diag.Diagnostic) bool {
	for _, d := range diags {
		switch d.Code {
		case "param_type_mismatch", "input_items_invalid_for_type":
			return true
		}
	}
	return false
}
