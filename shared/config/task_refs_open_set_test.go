package config

import (
	"fmt"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/diag"
)

func openSetScenarioDiags(t *testing.T, tasks string) []diag.Diagnostic {
	t.Helper()
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte("name: probe\ntasks:\n"+tasks), ValidateOptions{})
	return diags
}

func diagsWithCode(ds []diag.Diagnostic, code string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, d := range ds {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}

// TestOpenSet_RegisterSelfIsReserved — ★ GUARD: `register: self` is refused at parse,
// as `hosts` is. The Keeper decides every register but register.self before dispatch
// (predicate-rendering.md §3), and a task registered as `self` would be both the
// decided register and the open one. The reservation sits in [validateTaskNode], which
// the destiny loader shares: on the Soul a destiny task's `self` is shadowed by the
// task's own result too.
//
// Mutation: drop the registerSelfName branch in [validateRegisterField].
func TestOpenSet_RegisterSelfIsReserved(t *testing.T) {
	diags := openSetScenarioDiags(t, `  - name: probe
    module: core.exec.run
    register: self
    params: { cmd: "true" }
`)
	d := diagWithCode(diags, "register_name_reserved")
	if d == nil || d.YAMLPath != "$.tasks[0].register" || d.Line == 0 {
		dump(t, diags)
		t.Fatalf("want register_name_reserved at $.tasks[0].register with a position, got %+v", d)
	}

	_, ddiags, _ := LoadDestinyTasksFromBytes("main.yml", []byte(`- name: probe
  module: core.exec.run
  register: self
  params: { cmd: "true" }
`), ValidateOptions{})
	if !hasCode(ddiags, "register_name_reserved") {
		dump(t, ddiags)
		t.Fatal("want register_name_reserved on a destiny task")
	}

	for _, name := range []string{"self_check", "myself", "selfie"} {
		diags := openSetScenarioDiags(t, fmt.Sprintf(`  - name: probe
    module: core.exec.run
    register: %s
    params: { cmd: "true" }
`, name))
		if hasCode(diags, "register_name_reserved") {
			t.Errorf("register: %s refused as reserved", name)
		}
	}
}

// TestOpenSet_WhenRegisterSelfRefused — ★ GUARD: `when:` runs before the module, so
// register.self — the task's own result — does not exist there; on the Soul today the
// predicate fails with no-such-key on every host. Refused in a scenario and a destiny
// alike, and only in `when:`.
//
// Mutation: drop the `when` arm in collectRefs.
func TestOpenSet_WhenRegisterSelfRefused(t *testing.T) {
	diags := openSetScenarioDiags(t, `  - name: probe
    module: core.exec.run
    params: { cmd: "true" }
    when: "register.self.rc == 0"
`)
	d := diagWithCode(diags, codeWhenRegisterSelfUnsupported)
	if d == nil || d.YAMLPath != "$.tasks[0].when" || d.Line == 0 {
		dump(t, diags)
		t.Fatalf("want %s at $.tasks[0].when, got %+v", codeWhenRegisterSelfUnsupported, d)
	}

	_, destiny, _ := LoadDestinyTasksFromBytes("main.yml", []byte(`- name: probe
  module: core.exec.run
  params: { cmd: "true" }
  when: "register.self.rc == 0"
`), ValidateOptions{})
	if !hasCode(destiny, codeWhenRegisterSelfUnsupported) {
		dump(t, destiny)
		t.Fatal("want the refusal on a destiny task")
	}

	passes := openSetScenarioDiags(t, `  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: "true" }
  - name: next
    module: core.exec.run
    params: { cmd: "true" }
    when: "register.probe.rc == 0 && 'register.self' != ''"
    changed_when: "register.self.rc == 0"
`)
	if hasCode(passes, codeWhenRegisterSelfUnsupported) {
		dump(t, passes)
		t.Error("refused a when: that does not read register.self, or a changed_when: that does")
	}
}

// TestOpenSet_ReservedNameIsTheOpenRegister — the name reserved at parse is the one the
// run leaves open when it renders a predicate (shared/cel).
func TestOpenSet_ReservedNameIsTheOpenRegister(t *testing.T) {
	if registerSelfName != cel.RegisterSelf {
		t.Errorf("parse reserves %q, the run leaves %q open", registerSelfName, cel.RegisterSelf)
	}
}
