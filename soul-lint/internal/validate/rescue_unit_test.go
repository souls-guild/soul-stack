package validate

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-0091 amendment NIM-993. The refusals are the `scenario-broken/` fixtures
// `scenario-onfail-names-rescue`, `scenario-group-multiple-rescue-units`,
// `scenario-task-failed-message-required` and `scenario-*-on-task-failed`. This is
// the other half: the near misses pass, and a destiny is judged in itself.

var rescueCodes = []string{
	"onfail_names_rescue", "group_multiple_rescue_units", "missing_required_field",
	"failed_when_on_task_failed_invalid", "changed_when_on_task_failed_invalid",
	"retry_on_task_failed_invalid", "timeout_on_task_failed_invalid",
}

func TestScenario_RescueUnitNearMissesPass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.yml")
	stageWrite(t, path, `name: near-misses
settings:
  serial_scope: block
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: /opt/app/deploy.sh }
  - name: one outside unit in a group
    serial: 1
    block:
      - name: roll back
        onfail: [deploy]
        block:
          - name: restore
            module: core.exec.run
            register: rollback
            failed_when: "false"
            params: { cmd: /opt/app/rollback.sh }
          - name: not handled while the rollback failed
            module: core.task.failed
            onfail: [deploy]
            when: register.rollback.exit_code != 0
            params: {}
  - name: report the rollback
    module: core.exec.run
    when: register.rollback.failed
    params: { cmd: /opt/app/report.sh }
  - name: refuse a legacy release
    module: core.task.failed
    when: register.deploy.changed
    params: { message: the legacy release is not supported }
`)
	var out, errOut bytes.Buffer
	code := Run(Options{Path: path, Kind: KindScenario}, &out, &errOut)
	if errOut.Len() > 0 {
		t.Fatalf("soul-lint failed to run: %s", errOut.String())
	}
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out.String())
	}
	for _, c := range rescueCodes {
		if strings.Contains(out.String(), "["+c+"]") {
			t.Errorf("a legal scenario drew %s\n%s", c, out.String())
		}
	}
}

// A destiny's own source/rescue pair is judged in the destiny, so it is legal
// however the destiny is applied; a rescue of one of its unit tasks, and a bare
// core.task.failed that only an applier's onfail: would put in a unit, are not.
func TestDestinyTasks_RescueUnitsAreJudgedInTheDestiny(t *testing.T) {
	pair := "- name: migrate\n  module: core.exec.run\n  register: migrate\n  params:\n    cmd: /bin/true\n" +
		"- name: undo\n  module: core.exec.run\n  register: undo\n  onfail: [migrate]\n  params:\n    cmd: /bin/true\n"
	for name, tc := range map[string]struct {
		tasks string
		want  string
	}{
		"its own pair":                 {pair, ""},
		"a rescue of its unit task":    {pair + "- name: page\n  module: core.exec.run\n  onfail: [undo]\n  params:\n    cmd: /bin/true\n", "onfail_names_rescue"},
		"a bare core.task.failed":      {"- name: give up\n  module: core.task.failed\n  params: {}\n", "missing_required_field"},
		"core.task.failed in its unit": {pair + "- name: unit\n  onfail: [migrate]\n  block:\n    - name: give up\n      module: core.task.failed\n      params: {}\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			manifest := filepath.Join(root, "destiny.yml")
			stageWrite(t, manifest, "name: app\ndescription: a destiny with rescues\n")
			stageWrite(t, filepath.Join(root, "tasks", "main.yml"), tc.tasks)

			code, out := runDestiny(t, manifest, nil)

			if tc.want == "" {
				if code != ExitOK {
					t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
				}
				return
			}
			if code != ExitHasErrors || !containsDiagFor(out, filepath.Join("tasks", "main.yml"), tc.want) {
				t.Fatalf("exit = %d, want %d with %s on tasks/main.yml\n%s", code, ExitHasErrors, tc.want, out)
			}
		})
	}
}
