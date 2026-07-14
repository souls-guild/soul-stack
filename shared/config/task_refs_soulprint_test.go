package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestSoulprintRef_OKPaths — the canonical form soulprint.self.<top>.<sub> with
// valid typed-schema fields (ADR-018) must not produce errors.
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

// TestSoulprintRef_ChoirsTargeting — the ADR-044-recommended choir targeting
// via `where: "'x' in soulprint.self.choirs"` must not be flagged (choirs is a
// registry projection list<string>, a mirror of covens; cel_render.go already
// projects it into both self and hosts[]). Regression for latent bug S-T4.
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

// TestSoulprintRef_ChoirsTypoStillFlagged — an adjacent typo in the projection
// (`choir` without the s) must still be caught as an unknown top-level path:
// adding choirs does not weaken the linter.
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
// targeting via `soulprint.self.traits.<key>` (scalar and list) is not flagged.
// The traits key is dynamic (an arbitrary operator name) — the third segment is
// NOT statically checked (like covens/choirs); soul-lint only verifies that
// `traits` is a known top-level field under soulprint.self.*.
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

// TestSoulprintRef_TraitsTypoStillFlagged — a typo in the projection (`trait`
// without the s) is still caught as an unknown top-level path: adding traits
// does not weaken the linter (regression safety, symmetric to choirs).
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

// TestSoulprintRef_TypoFamilyFlagged — the typo `os.familly` (double l) does
// not validate as a top-level SoulprintFacts field, so soulprint_unknown_path
// must appear.
//
// Note: the current static check catches the first segment after
// `soulprint.self.` (top-level — os/kernel/cpu/...). A typo in the SECOND
// segment (`os.familly`) without going deeper is not flagged — that is deferred
// to a separate slice (checkSoulprintSubPath placeholder). To pin the linter's
// current working scope, the test checks a typo in the first segment.
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

// TestSoulprintRef_NakedFormFlagged — the bare form `soulprint.<x>` without
// .self/.hosts/.where is an error (docs/soul/soulprint.md "canonical form is required").
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
// pass without a flag (these are scenario-only accessors, checked by shared/cel
// in the render phase).
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
// 'primary'")` contains a nested CEL string — its contents must not be
// extracted as a `soulprint.<...>` typo.
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
	// apply.input is not a CEL predicate in the task_refs sense (it does not go
	// through checkSoulprintRefs), but even if it did, the nested literal string
	// must be stripped out. The guarantee is zero soulprint flags.
	if hasCode(diags, "soulprint_naked_reference") || hasCode(diags, "soulprint_unknown_path") {
		dump(t, diags)
		t.Fatalf("вложенный CEL-литерал-аргумент .where(...) не должен порождать soulprint-флаги")
	}
}

// TestSoulprintRef_RetryUntil — soulprint in retry.until is covered by the walk.
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

// TestCovenLabelValidatorHook_Active — the custom validator is swappable and
// returns coven_label_unknown with a meaningful message. After the test we
// restore the no-op (determinism for parallel tests).
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

// TestCovenLabelValidatorHook_Noop — by default (no-op) a valid coven-id passes
// without a flag.
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
