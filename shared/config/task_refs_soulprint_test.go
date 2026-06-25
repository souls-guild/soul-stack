package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestSoulprintRef_OKPaths — каноническая форма soulprint.self.<top>.<sub> с
// валидными полями typed-схемы (ADR-018) не должна давать errors.
func TestSoulprintRef_OKPaths(t *testing.T) {
	src := `name: ok
tasks:
  - name: t1
    where: soulprint.self.os.family == "debian"
    module: core.exec.run
    params: { cmd: "true" }
  - name: t2
    where: soulprint.self.memory.total_mb > 1024
    module: core.exec.run
    params: { cmd: "true" }
  - name: t3
    where: soulprint.self.network.primary_ip != "127.0.0.1"
    module: core.exec.run
    params: { cmd: "true" }
  - name: t4
    where: soulprint.self.sid.startsWith("ta-")
    module: core.exec.run
    params: { cmd: "true" }
  - name: t5
    where: '"db" in soulprint.self.covens'
    module: core.exec.run
    params: { cmd: "true" }
  - name: t6
    where: '"alpha" in soulprint.self.choirs'
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("ожидаем ноль ошибок на валидных soulprint.self.* путях")
	}
	if hasCode(diags, "soulprint_unknown_path") || hasCode(diags, "soulprint_naked_reference") {
		dump(t, diags)
		t.Fatalf("ложно-позитивный soulprint diagnostic на валидных путях")
	}
}

// TestSoulprintRef_ChoirsTargeting — рекомендованный ADR-044 choir-таргетинг
// через `where: "'x' in soulprint.self.choirs"` не должен флагаться (choirs —
// registry-проекция list<string>, зеркало covens; cel_render.go её уже
// проецирует и в self, и в hosts[]). Регресс на латентный баг S-T4.
func TestSoulprintRef_ChoirsTargeting(t *testing.T) {
	src := `name: ok
tasks:
  - name: self-choir
    where: '"replicas" in soulprint.self.choirs'
    module: core.exec.run
    params: { cmd: "true" }
  - name: hosts-choir
    where: 'soulprint.hosts.where("\"replicas\" in choirs").size() > 0'
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "soulprint_unknown_path") || hasCode(diags, "soulprint_naked_reference") {
		dump(t, diags)
		t.Fatalf("choir-таргетинг через soulprint.self.choirs / soulprint.hosts[].choirs не должен флагаться (ADR-044)")
	}
}

// TestSoulprintRef_ChoirsTypoStillFlagged — рядом стоящая опечатка в проекции
// (`choir` без s) по-прежнему должна ловиться как unknown top-level path:
// добавление choirs не ослабляет линтер.
func TestSoulprintRef_ChoirsTypoStillFlagged(t *testing.T) {
	src := `name: bad
tasks:
  - name: typo
    where: '"x" in soulprint.self.choir'
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "soulprint_unknown_path") {
		dump(t, diags)
		t.Fatalf("ожидали soulprint_unknown_path на soulprint.self.choir (опечатка)")
	}
}

// TestSoulprintRef_TraitsTargeting — GUARD (ADR-060): operator-set traits
// таргетинг через `soulprint.self.traits.<key>` (скаляр и список) не флагается.
// Ключ traits динамичен (произвольное имя оператора) — третий сегмент НЕ
// статпроверяется (как у covens/choirs); soul-lint сверяет только что
// `traits` — известное top-level поле под soulprint.self.*.
func TestSoulprintRef_TraitsTargeting(t *testing.T) {
	src := `name: ok
tasks:
  - name: scalar-trait
    where: soulprint.self.traits.namespace == "dba-ns"
    module: core.exec.run
    params: { cmd: "true" }
  - name: list-trait
    where: '"alice" in soulprint.self.traits.owners'
    module: core.exec.run
    params: { cmd: "true" }
  - name: hosts-trait
    where: 'soulprint.hosts.where("traits.namespace == \"dba-ns\"").size() > 0'
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "soulprint_unknown_path") || hasCode(diags, "soulprint_naked_reference") {
		dump(t, diags)
		t.Fatalf("trait-таргетинг через soulprint.self.traits.<key> не должен флагаться (ADR-060)")
	}
}

// TestSoulprintRef_TraitsTypoStillFlagged — опечатка в проекции (`trait` без s)
// по-прежнему ловится как unknown top-level path: добавление traits не ослабляет
// линтер (регресс-страховка симметрично choirs).
func TestSoulprintRef_TraitsTypoStillFlagged(t *testing.T) {
	src := `name: bad
tasks:
  - name: typo
    where: soulprint.self.trait.namespace == "dba-ns"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "soulprint_unknown_path") {
		dump(t, diags)
		t.Fatalf("ожидали soulprint_unknown_path на soulprint.self.trait (опечатка)")
	}
}

// TestSoulprintRef_TypoFamilyFlagged — опечатка `os.familly` (двойная l)
// не валидируется как top-level поле SoulprintFacts, поэтому
// soulprint_unknown_path должен быть.
//
// Замечание: текущая статпроверка ловит первый сегмент после `soulprint.self.`
// (top-level — os/kernel/cpu/...). Опечатка во ВТОРОМ сегменте (`os.familly`)
// без перехода глубже не флагается — это отложено отдельным слайсом
// (checkSoulprintSubPath placeholder). Чтобы зафиксировать сейчас рабочий
// scope линтера, тест проверяет опечатку именно в первом сегменте.
func TestSoulprintRef_UnknownTopFieldFlagged(t *testing.T) {
	src := `name: bad
tasks:
  - name: typo
    where: soulprint.self.memmory.total_mb > 0
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "soulprint_unknown_path") {
		dump(t, diags)
		t.Fatalf("ожидали soulprint_unknown_path на soulprint.self.memmory")
	}
}

// TestSoulprintRef_NakedFormFlagged — голая форма `soulprint.<x>` без .self/
// .hosts/.where — ошибка (docs/soul/soulprint.md «Каноническая форма обязательна»).
func TestSoulprintRef_NakedFormFlagged(t *testing.T) {
	src := `name: bad
tasks:
  - name: bare
    where: soulprint.os.family == "debian"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "soulprint_naked_reference") {
		dump(t, diags)
		t.Fatalf("ожидали soulprint_naked_reference на soulprint.os")
	}
}

// TestSoulprintRef_HostsAccessorAllowed — soulprint.hosts / soulprint.where(...)
// проходят без флага (это scenario-only аксессоры, проверяются shared/cel в
// render-фазе).
func TestSoulprintRef_HostsAccessorAllowed(t *testing.T) {
	src := `name: ok
tasks:
  - name: probe
    where: 'soulprint.hosts.where("role == \"primary\"").size() == 1'
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "soulprint_naked_reference") || hasCode(diags, "soulprint_unknown_path") {
		dump(t, diags)
		t.Fatalf("soulprint.hosts.where(...) не должен флагаться")
	}
}

// TestSoulprintRef_NestedCELLiteralIgnored — `soulprint.hosts.where("role ==
// 'primary'")` содержит вложенную CEL-строку — её содержимое не должно
// извлекаться как `soulprint.<...>` опечатка.
func TestSoulprintRef_NestedCELLiteralIgnored(t *testing.T) {
	src := `name: ok
tasks:
  - name: master
    apply:
      destiny: redis
      input:
        master_addr: '${ soulprint.hosts.where("role == ''primary''")[0].network.primary_ip }'
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	// apply.input — не CEL-предикат в смысле task_refs (он не проходит через
	// checkSoulprintRefs), но даже если бы проходил, вложенная литерал-строка
	// должна быть вырезана. Гарантия — ноль soulprint-флагов.
	if hasCode(diags, "soulprint_naked_reference") || hasCode(diags, "soulprint_unknown_path") {
		dump(t, diags)
		t.Fatalf("вложенный CEL-литерал-аргумент .where(...) не должен порождать soulprint-флаги")
	}
}

// TestSoulprintRef_RetryUntil — soulprint в retry.until покрыт обходом.
func TestSoulprintRef_RetryUntil(t *testing.T) {
	src := `name: bad
tasks:
  - name: t
    module: core.exec.run
    params: { cmd: "true" }
    retry:
      count: 3
      until: soulprint.self.notafield == 1
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "soulprint_unknown_path") {
		dump(t, diags)
		t.Fatalf("ожидали soulprint_unknown_path на retry.until")
	}
}

// TestCovenLabelValidatorHook_Active — кастомный validator подменяемый, и
// возвращает coven_label_unknown с осмысленным сообщением. После теста
// возвращаем no-op обратно (детерминизм для параллельных тестов).
func TestCovenLabelValidatorHook_Active(t *testing.T) {
	prev := SetCovenLabelValidator(rejectAllCovenValidator{})
	t.Cleanup(func() { SetCovenLabelValidator(prev) })

	src := `name: x
tasks:
  - on: [prod]
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "coven_label_unknown") {
		dump(t, diags)
		t.Fatalf("ожидали coven_label_unknown при активном reject-validator")
	}
}

// TestCovenLabelValidatorHook_Noop — по умолчанию (no-op) валидный coven-id
// проходит без флага.
func TestCovenLabelValidatorHook_Noop(t *testing.T) {
	src := `name: x
tasks:
  - on: [prod, redis]
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "coven_label_unknown") {
		dump(t, diags)
		t.Fatalf("no-op CovenLabelValidator не должен флагать валидные coven-id")
	}
}

type rejectAllCovenValidator struct{}

func (rejectAllCovenValidator) Validate(label string) error {
	return errors.New("test: unknown coven " + strings.ToLower(label))
}
