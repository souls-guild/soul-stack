package trial

import (
	"context"
	"strings"
	"testing"
)

// flowMain carries every field flowMismatches reads: a settings-level
// task_timeout, a task that overrides it and one that lifts it, a failed_when, a
// loop source (two plan indices behind one register) and two onfail tasks.
const flowMain = `name: create
settings:
  task_timeout: 10s
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params:
      cmd: deploy
  - name: copy each
    module: core.exec.run
    register: copies
    loop:
      items: "${ ['a', 'b'] }"
      as: item
    params:
      cmd: "${ item }"
  - name: slow step
    module: core.exec.run
    timeout: 60s
    failed_when: register.self.exit_code == 0
    params:
      cmd: slow
  - name: rollback deploy
    module: core.exec.run
    onfail: [deploy]
    params:
      cmd: rollback
  - name: rollback copies
    module: core.exec.run
    onfail: [copies, deploy]
    params:
      cmd: undo
  - name: unbounded
    module: core.exec.run
    timeout: "0"
    params:
      cmd: wait
`

func runFlowCase(t *testing.T, caseYML string) Result {
	t.Helper()
	results, err := Run(context.Background(), writeScenarioTree(t, flowMain, caseYML), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return results[0]
}

// The positive half: the effective timeout (inherited, overridden, lifted), the
// failed_when text and the onfail sources all match, in both forms. A timeout
// matches as a duration, not as text; the loop source is written once although
// it resolves to two indices. The entries on indices 3 and 4 name none of the
// fields those tasks carry, and must not be failed for it.
func TestRunCase_FlowFieldsMatch(t *testing.T) {
	res := runFlowCase(t, `name: flow fields match
assert:
  rendered_tasks:
    - {index: 0, module: core.exec.run, timeout: 10000ms}
    - {index: 3, module: core.exec.run, timeout: 60s, failed_when: register.self.exit_code == 0}
    - {index: 3, module: core.exec.run}
    - {index: 4, module: core.exec.run}
    - {index: 5, module: core.exec.run, onfail: [deploy, copies]}
    - {index: 6, module: core.exec.run, timeout: "0"}
  task_present:
    - module: core.exec.run
      timeout: 10s
      onfail: [deploy]
      params_subset: {cmd: rollback}
    - module: core.exec.run
      failed_when: register.self.exit_code == 0
    - module: core.exec.run
      onfail: [copies, deploy, copies]
`)
	if !res.Pass {
		t.Fatalf("expected PASS, got: %v", res.Failures)
	}
}

// Each field narrows the presence match: a wrong value is "not found" rather
// than a match on module and params alone.
func TestRunCase_FlowFieldsNarrowPresence(t *testing.T) {
	for _, tc := range []struct{ name, entry string }{
		{"timeout", "timeout: 60s\n      params_subset: {cmd: deploy}"},
		{"failed_when", "failed_when: register.self.exit_code != 0"},
		{"onfail", "onfail: [copies]\n      params_subset: {cmd: rollback}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := runFlowCase(t, "name: narrow "+tc.name+"\nassert:\n  task_present:\n    - module: core.exec.run\n      "+tc.entry+"\n")
			if res.Pass {
				t.Fatalf("expected FAIL: a wrong %s must not match", tc.name)
			}
			if got := strings.Join(res.Failures, "\n"); !strings.Contains(got, "not found") || !strings.Contains(got, tc.name+"=") {
				t.Fatalf("expected a \"not found\" naming %s, got: %v", tc.name, res.Failures)
			}
		})
	}
}

// task_absent with a flow field is satisfied when only the field differs, and
// violated when it agrees.
func TestRunCase_FlowFieldsInAbsence(t *testing.T) {
	if res := runFlowCase(t, `name: absent with other onfail
assert:
  task_absent:
    - module: core.exec.run
      onfail: [copies]
`); !res.Pass {
		t.Fatalf("expected PASS: no task has onfail exactly [copies], got: %v", res.Failures)
	}
	if res := runFlowCase(t, `name: absent violated
assert:
  task_absent:
    - module: core.exec.run
      onfail: [deploy]
`); res.Pass {
		t.Fatal("expected FAIL: the rollback deploy task has onfail [deploy]")
	}
}

// The positional form reports each field by name, with what the plan carries. A
// "0" against a limit and a limit against none both fail.
func TestRunCase_FlowFieldsPositionalMismatch(t *testing.T) {
	res := runFlowCase(t, `name: positional mismatch
assert:
  rendered_tasks:
    - {index: 0, module: core.exec.run, timeout: 30s}
    - {index: 1, module: core.exec.run, timeout: "0"}
    - {index: 3, module: core.exec.run, failed_when: "true"}
    - {index: 4, module: core.exec.run, onfail: [copies]}
    - {index: 6, module: core.exec.run, timeout: 10s}
`)
	if res.Pass {
		t.Fatal("expected FAIL on every entry")
	}
	got := strings.Join(res.Failures, "\n")
	for _, want := range []string{
		`task index 0: timeout = "10s", expected "30s"`,
		`task index 1: timeout = "10s", expected "0"`,
		`task index 3: failed_when = "register.self.exit_code == 0", expected "true"`,
		`task index 4: onfail = [deploy], expected [copies]`,
		`task index 6: timeout = "", expected "10s"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// A timeout that is no duration is refused at load, in every form, instead of
// reading as "0".
func TestLoadCase_RejectsTimeoutThatIsNoDuration(t *testing.T) {
	for _, section := range []string{"rendered_tasks", "task_present", "task_absent"} {
		t.Run(section, func(t *testing.T) {
			caseDir := writeScenarioTree(t, flowMain, "name: bad timeout\nassert:\n  "+section+":\n    - {index: 0, module: core.exec.run, timeout: 10 s}\n")
			_, _, err := LoadCase(caseDir)
			if err == nil {
				t.Fatal("expected a load error for timeout \"10 s\"")
			}
			if want := "assert." + section + "[0]: timeout \"10 s\" is not a duration"; !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not contain %q", err, want)
			}
		})
	}
}
