package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestStateRef_IncarnationStateOK — каноническая форма `incarnation.state.<path>`
// (read-only снимок incarnation.state в scenario render, ADR-009/010 Вариант A)
// валидна в предикате и apply.input. Не должна давать state_naked_reference:
// `state` тут — поле incarnation (перед ним стоит `.`), не корневой идентификатор.
func TestStateRef_IncarnationStateOK(t *testing.T) {
	src := `name: ok
tasks:
  - name: t1
    where: "size(incarnation.state.redis_users) > 0"
    apply:
      destiny: redis
      input:
        current: "${ incarnation.state.redis_users }"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "state_naked_reference") {
		dump(t, diags)
		t.Fatalf("ложно-позитивный state_naked_reference на канонической incarnation.state.*")
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("ожидаем ноль ошибок на incarnation.state.*")
	}
}

// TestStateRef_NakedInPredicateFlagged — голый `state.<path>` в where: (state в
// scenario-CEL не объявлен, migration-only) → state_naked_reference.
func TestStateRef_NakedInPredicateFlagged(t *testing.T) {
	src := `name: bad
tasks:
  - name: t1
    where: "size(state.redis_users) > 0"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "state_naked_reference") {
		dump(t, diags)
		t.Fatalf("ожидаем state_naked_reference на голом state.* в where:")
	}
}

// TestStateRef_NakedInApplyInputFlagged — канонический кейс update_acl: голый
// `${ state.redis_users }` в apply.input (забыт префикс incarnation.) → ошибка.
func TestStateRef_NakedInApplyInputFlagged(t *testing.T) {
	src := `name: bad
tasks:
  - name: t1
    apply:
      destiny: redis
      input:
        current: "${ state.redis_users }"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "state_naked_reference", "$.tasks[0].apply.input") {
		dump(t, diags)
		t.Fatalf("ожидаем state_naked_reference на голом state.* в apply.input")
	}
}

// TestStateRef_NakedInParamsFlagged — голый state.* в params: (интерполяция).
func TestStateRef_NakedInParamsFlagged(t *testing.T) {
	src := `name: bad
tasks:
  - name: t1
    module: core.exec.run
    params:
      cmd: "echo ${ state.redis_users }"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "state_naked_reference") {
		dump(t, diags)
		t.Fatalf("ожидаем state_naked_reference на голом state.* в params:")
	}
}

// TestStateRef_NestedCELLiteralIgnored — `state.x` внутри строкового литерала CEL
// (данные, не ссылка) не флагается: literal вырезается перед извлечением.
func TestStateRef_NestedCELLiteralIgnored(t *testing.T) {
	src := `name: ok
tasks:
  - name: t1
    where: "incarnation.name == 'state.redis_users'"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "state_naked_reference") {
		dump(t, diags)
		t.Fatalf("ложно-позитивный state_naked_reference на state.x внутри строкового литерала")
	}
}

// TestStateRef_SubstringIdentNotFlagged — идентификаторы, в которые `state`
// входит как подстрока (`mystate.x`, `restate.y`) или как поле другого объекта
// (`foo.state.z`), НЕ корневой `state` — не флагаются.
func TestStateRef_SubstringIdentNotFlagged(t *testing.T) {
	src := `name: ok
tasks:
  - name: t1
    where: "incarnation.state_schema_version > 0"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "state_naked_reference") {
		dump(t, diags)
		t.Fatalf("ложно-позитивный state_naked_reference на incarnation.state_schema_version (state — подстрока, не корень)")
	}
}
