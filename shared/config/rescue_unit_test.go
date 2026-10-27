package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ADR-0091 amendment NIM-993: rescue units (rule 1), the refusals that read them
// (`onfail_names_rescue`, `group_multiple_rescue_units`) and the static rules of
// `core.task.failed` (rule 8).

// scenarioPlan loads a scenario main file and expands its includes the way the
// Keeper does before a run, returning the flat plan and every diagnostic of both
// phases.
func scenarioPlan(t *testing.T, src string, files map[string]string) (*ScenarioManifest, []Task, []diag.Diagnostic) {
	t.Helper()
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("the scenario itself does not load")
	}
	tasks, idiags := ExpandIncludes(m.Tasks, mapResolver(files))
	return m, tasks, append(diags, idiags...)
}

// destinyPlan is [scenarioPlan] for a destiny's `tasks/main.yml`.
func destinyPlan(t *testing.T, src string, files map[string]string) []diag.Diagnostic {
	t.Helper()
	tasks, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
	if diag.HasErrors(diags) {
		return diags
	}
	_, idiags := ExpandIncludesInDestiny(tasks, mapResolver(files))
	return append(diags, idiags...)
}

func withCode(diags []diag.Diagnostic, code string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, d := range diags {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}

func TestRescueUnits_ReckonedPerSource(t *testing.T) {
	_, tasks, diags := scenarioPlan(t, `name: x
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: "true" }
  - name: migrate
    module: core.exec.run
    register: migrate
    params: { cmd: "true" }
  - name: roll back
    onfail: [deploy]
    block:
      - name: restore
        module: core.exec.run
        register: restore
        onfail: [deploy, migrate]
        params: { cmd: "true" }
      - name: check
        module: core.exec.run
        register: health
        params: { cmd: "true" }
  - name: notify
    module: core.exec.run
    onfail: [deploy, deploy]
    params: { cmd: "true" }
  - name: record
    module: core.state.set
    onfail: [deploy]
    params: { field: owner, value: alice }
  - name: redeploy
    onfail: [migrate]
    register: redeploy
    apply: { destiny: app, input: {} }
`, nil)
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("the plan was refused")
	}

	type unit struct {
		source, path, name string
		registers          []string
	}
	var got []unit
	for _, u := range RescueUnits(tasks) {
		got = append(got, unit{u.Source, u.Path, u.Task.Name, u.Registers})
	}
	want := []unit{
		// The block names deploy; its child's own deploy merges into it.
		{"deploy", "2", "roll back", []string{"restore", "health"}},
		// The child's migrate has no enclosing unit for migrate: a unit of its own.
		{"migrate", "2/0", "restore", []string{"restore"}},
		// A second unit of deploy; a source named twice is one unit.
		{"deploy", "3", "notify", nil},
		// `record` is keeper-side: no rescue, no unit. An applier is a unit, and its
		// register is set inside it.
		{"migrate", "5", "redeploy", []string{"redeploy"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("units:\n got  %+v\n want %+v", got, want)
	}
}

func TestRescueUnit_Holds(t *testing.T) {
	u := RescueUnit{Path: "2"}
	for path, want := range map[string]bool{"2": true, "2/0": true, "2/1/3": true, "20": false, "1": false, "": false} {
		if got := u.Holds(path); got != want {
			t.Errorf("Holds(%q) = %v, want %v", path, got, want)
		}
	}
}

const rescueHead = `name: x
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: "true" }
  - name: roll back
    onfail: [deploy]
    block:
      - name: restore
        module: core.exec.run
        register: restore
        params: { cmd: "true" }
`

func TestOnfailNamesRescue_Refused(t *testing.T) {
	for name, tc := range map[string]struct {
		tail  string
		files map[string]string
		names string
	}{
		"a task rescuing a unit task": {tail: `  - name: rescue the rescue
    module: core.exec.run
    onfail: [restore]
    params: { cmd: "true" }
`, names: "restore"},
		"a block rescuing a unit task": {tail: `  - name: rescue block
    onfail: [restore]
    block:
      - module: core.exec.run
        params: { cmd: "true" }
`, names: "restore"},
		"a task rescuing an applier unit": {tail: `  - name: redeploy
    onfail: [deploy]
    register: redeploy
    apply: { destiny: app, input: {} }
  - name: rescue the applier
    module: core.exec.run
    onfail: [redeploy]
    params: { cmd: "true" }
`, names: "redeploy"},
		"a unit task rescuing a sibling of its own unit": {tail: `  - name: second unit
    onfail: [deploy]
    block:
      - name: probe
        module: core.exec.run
        register: probe
        params: { cmd: "true" }
      - name: on the probe
        module: core.exec.run
        onfail: [probe]
        params: { cmd: "true" }
`, names: "probe"},
		"an included body rescuing a task the includer's unit holds": {tail: `  - name: included unit
    onfail: [deploy]
    block:
      - include: body.yml
`, files: map[string]string{"body.yml": `- name: inner
  module: core.exec.run
  register: inner
  params: { cmd: "true" }
- name: on inner
  module: core.exec.run
  onfail: [inner]
  params: { cmd: "true" }
`}, names: "inner"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, diags := scenarioPlan(t, rescueHead+tc.tail, tc.files)
			got := withCode(diags, CodeOnfailNamesRescue)
			if len(got) != 1 {
				dump(t, diags)
				t.Fatalf("%d %s, want exactly one", len(got), CodeOnfailNamesRescue)
			}
			if got[0].Level != diag.LevelError || !strings.Contains(got[0].Message, `"`+tc.names+`"`) {
				t.Errorf("diagnostic %+v: want an error naming register %q", got[0], tc.names)
			}
		})
	}
}

func TestOnfailNamesRescue_NearMissesPass(t *testing.T) {
	for name, tail := range map[string]string{
		"when: reading a unit register": `  - name: report
    module: core.exec.run
    when: register.restore.failed
    params: { cmd: "true" }
`,
		"onchanges: naming a unit register": `  - name: on restore
    module: core.exec.run
    onchanges: [restore]
    params: { cmd: "true" }
`,
		"a second rescue of the same source": `  - name: notify
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: "true" }
`,
		"an applier unit applying a destiny": `  - name: redeploy
    onfail: [deploy]
    apply: { destiny: app, input: {} }
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, diags := scenarioPlan(t, rescueHead+tail, nil)
			if got := withCode(diags, CodeOnfailNamesRescue); len(got) != 0 {
				dump(t, diags)
				t.Fatalf("refused a legal plan")
			}
		})
	}
}

// The check reads one plan: a destiny's own `onfail:` naming the destiny's
// registers is judged in the destiny alone — its own source/rescue pair is legal,
// a rescue of its own unit task is not.
func TestOnfailNamesRescue_DestinyIsJudgedInItself(t *testing.T) {
	ownPair := `- name: migrate
  module: core.exec.run
  register: migrate
  params: { cmd: "true" }
- name: undo migrate
  module: core.exec.run
  register: undo
  onfail: [migrate]
  params: { cmd: "true" }
`
	if got := withCode(destinyPlan(t, ownPair, nil), CodeOnfailNamesRescue); len(got) != 0 {
		t.Fatalf("a destiny's own source/rescue pair was refused: %+v", got)
	}

	nested := destinyPlan(t, ownPair+`- name: rescue the rescue
  module: core.exec.run
  onfail: [undo]
  params: { cmd: "true" }
`, nil)
	if got := withCode(nested, CodeOnfailNamesRescue); len(got) != 1 {
		dump(t, nested)
		t.Fatalf("a destiny rescuing its own unit task: %d refusals, want 1", len(got))
	}
}

const groupHead = `name: x
settings:
  serial_scope: block
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: "true" }
  - name: migrate
    module: core.exec.run
    register: migrate
    params: { cmd: "true" }
`

// fillerTasks returns top-level tasks that only take the positions from..to.
func fillerTasks(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "  - name: filler %d\n    module: core.exec.run\n    params: { cmd: \"true\" }\n", i)
	}
	return b.String()
}

func TestGroupMultipleRescueUnits(t *testing.T) {
	for name, tc := range map[string]struct {
		src     string
		refused bool
	}{
		"two units for outside sources": {groupHead + `  - name: rescues
    serial: 1
    block:
      - name: undo deploy
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: "true" }
      - name: undo migrate
        module: core.exec.run
        onfail: [migrate]
        params: { cmd: "true" }
`, true},
		"two units of one outside source": {groupHead + `  - name: rescues
    serial: 1
    block:
      - name: first
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: "true" }
      - name: second
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: "true" }
`, true},
		"a unit enclosing the group and one inside it": {groupHead + `  - name: outer
    onfail: [deploy]
    block:
      - name: rescues
        serial: 1
        block:
          - name: undo migrate
            module: core.exec.run
            onfail: [migrate]
            params: { cmd: "true" }
`, true},
		// Inside the group's message both units hold the same plan, the whole group:
		// the same as one onfail: [deploy, migrate] on it.
		"the group's own unit and an enclosing one": {groupHead + `  - name: outer
    onfail: [deploy]
    block:
      - name: rescues
        serial: 1
        onfail: [migrate]
        block:
          - name: undo both
            module: core.exec.run
            params: { cmd: "true" }
`, false},
		"the group's own unit and an enclosing one, beside a unit inside it": {groupHead + `  - name: seed
    module: core.exec.run
    register: seed
    params: { cmd: "true" }
  - name: outer
    onfail: [deploy]
    block:
      - name: rescues
        serial: 1
        onfail: [migrate]
        block:
          - name: undo seed
            module: core.exec.run
            onfail: [seed]
            params: { cmd: "true" }
`, true},
		"one unit for an outside source": {groupHead + `  - name: rescues
    serial: 1
    block:
      - name: undo deploy
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: "true" }
      - name: report
        module: core.exec.run
        params: { cmd: "true" }
`, false},
		"one construct naming two outside sources": {groupHead + `  - name: rescues
    serial: 1
    block:
      - name: undo both
        module: core.exec.run
        onfail: [deploy, migrate]
        params: { cmd: "true" }
`, false},
		"an in-group pair beside one outside unit": {groupHead + `  - name: rescues
    serial: 1
    block:
      - name: probe
        module: core.exec.run
        register: probe
        params: { cmd: "true" }
      - name: on the probe
        module: core.exec.run
        onfail: [probe]
        params: { cmd: "true" }
      - name: undo deploy
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: "true" }
`, false},
		"two units in a default block": {`name: x
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: "true" }
  - name: migrate
    module: core.exec.run
    register: migrate
    params: { cmd: "true" }
  - name: rescues
    serial: 1
    block:
      - name: undo deploy
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: "true" }
      - name: undo migrate
        module: core.exec.run
        onfail: [migrate]
        params: { cmd: "true" }
`, false},
		// Positions "1" and "10" share a prefix string; the unit at 10 is not inside
		// the group at 1.
		"a unit at a later index sharing the group's prefix": {`name: x
settings:
  serial_scope: block
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: "true" }
  - name: rescues
    serial: 1
    block:
      - name: undo deploy
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: "true" }
  - name: migrate
    module: core.exec.run
    register: migrate
    params: { cmd: "true" }
` + fillerTasks(3, 9) + `  - name: undo migrate
    module: core.exec.run
    onfail: [migrate]
    params: { cmd: "true" }
`, false},
	} {
		t.Run(name, func(t *testing.T) {
			m, tasks, diags := scenarioPlan(t, tc.src, nil)
			if diag.HasErrors(diags) {
				dump(t, diags)
				t.Fatal("the plan was refused before the group check")
			}
			got := GroupMultipleRescueUnits(m.Settings, tasks)
			if !tc.refused {
				if len(got) != 0 {
					dump(t, got)
					t.Fatal("refused a group the rule allows")
				}
				return
			}
			if len(got) != 1 || got[0].Code != CodeGroupMultipleRescueUnits || got[0].Level != diag.LevelError {
				dump(t, got)
				t.Fatalf("want exactly one %s error", CodeGroupMultipleRescueUnits)
			}
			if !strings.Contains(got[0].Message, `"rescues"`) {
				t.Errorf("message %q does not name the group", got[0].Message)
			}
		})
	}
}

func TestTaskFailed_MessageRequiredOutsideAUnit(t *testing.T) {
	for name, tc := range map[string]struct {
		tail    string
		refused bool
	}{
		"outside a unit, no message": {`  - name: give up
    module: core.task.failed
    params: {}
`, true},
		"outside a unit, an empty message": {`  - name: give up
    module: core.task.failed
    params: { message: "" }
`, true},
		"outside a unit, with a message": {`  - name: give up
    module: core.task.failed
    params: { message: the release is broken }
`, false},
		"its own onfail:, no message": {`  - name: not handled
    module: core.task.failed
    onfail: [deploy]
    params: {}
`, false},
		"inside a block with onfail:, no message": {`  - name: unit
    onfail: [deploy]
    block:
      - name: not handled
        module: core.task.failed
        when: register.deploy.failed
        params: {}
`, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, diags := scenarioPlan(t, `name: x
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: "true" }
`+tc.tail, nil)
			got := withCode(diags, "missing_required_field")
			if !tc.refused {
				if len(got) != 0 {
					dump(t, diags)
					t.Fatal("refused a core.task.failed the rule allows")
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0].Message, "message") {
				dump(t, diags)
				t.Fatal("want exactly one missing_required_field naming message")
			}
		})
	}
}

// "Inside a unit" is judged where the task is written: a destiny task that is a
// rescue only through its applier's `onfail:` still needs `message:`.
func TestTaskFailed_DestinyJudgesItsOwnUnits(t *testing.T) {
	bare := withCode(destinyPlan(t, `- name: give up
  module: core.task.failed
  params: {}
`, nil), "missing_required_field")
	if len(bare) != 1 {
		t.Fatalf("a destiny's bare core.task.failed: %d missing_required_field, want 1", len(bare))
	}
	inUnit := destinyPlan(t, `- name: migrate
  module: core.exec.run
  register: migrate
  params: { cmd: "true" }
- name: unit
  onfail: [migrate]
  block:
    - name: not handled
      module: core.task.failed
      params: {}
`, nil)
	if got := withCode(inUnit, "missing_required_field"); len(got) != 0 {
		dump(t, inUnit)
		t.Fatal("a destiny's core.task.failed inside its own unit was refused")
	}
}

func TestTaskFailed_KeysSettledByTheKeeperAreRefused(t *testing.T) {
	for key, line := range map[string]string{
		"failed_when":  `failed_when: "false"`,
		"changed_when": `changed_when: "true"`,
		"retry":        `retry: { count: 2 }`,
		"timeout":      `timeout: 30s`,
	} {
		t.Run(key, func(t *testing.T) {
			src := `name: x
tasks:
  - name: give up
    module: core.task.failed
    ` + line + `
    params: { message: broken }
  - name: probe
    module: core.exec.run
    ` + line + `
    params: { cmd: "true" }
`
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			code := key + "_on_task_failed_invalid"
			if !hasCodeAt(diags, code, "$.tasks[0]."+key) {
				dump(t, diags)
				t.Fatalf("want %s at $.tasks[0].%s", code, key)
			}
			if got := countCodes(diags, code); got != 1 {
				dump(t, diags)
				t.Fatalf("%d %s, want 1: the same key on another module is legal", got, code)
			}
		})
	}

	src := `name: x
tasks:
  - name: give up
    module: core.task.failed
    when: input.strict
    params: { message: broken }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	for _, d := range diags {
		if strings.HasSuffix(d.Code, "_on_task_failed_invalid") {
			dump(t, diags)
			t.Fatal("when: on core.task.failed was refused")
		}
	}
}

// `settings.task_timeout` is the long form of `timeout:` (settings.md §2.3), so on
// the core.task.failed task itself it is refused the same; one it inherits from an
// enclosing level is that level's, not the task's.
func TestTaskFailed_TaskTimeoutLongForm(t *testing.T) {
	const code = "timeout_on_task_failed_invalid"
	for name, tc := range map[string]struct {
		src     string
		refused bool
	}{
		"written on the task": {`name: x
tasks:
  - name: give up
    module: core.task.failed
    settings: { task_timeout: 30s }
    params: { message: broken }
  - name: probe
    module: core.exec.run
    settings: { task_timeout: 30s }
    params: { cmd: "true" }
`, true},
		"written on the task, block style": {`name: x
tasks:
  - name: give up
    module: core.task.failed
    settings:
      task_timeout: 30s
    params: { message: broken }
`, true},
		"inherited from the scenario": {`name: x
settings: { task_timeout: 30s }
tasks:
  - name: give up
    module: core.task.failed
    params: { message: broken }
`, false},
		"inherited from a block": {`name: x
tasks:
  - name: guarded
    settings: { task_timeout: 30s }
    block:
      - name: give up
        module: core.task.failed
        params: { message: broken }
`, false},
		"another setting on the task": {`name: x
tasks:
  - name: give up
    module: core.task.failed
    settings: { on_failure: host }
    params: { message: broken }
`, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(tc.src), ValidateOptions{})
			if !tc.refused {
				if countCodes(diags, code) != 0 {
					dump(t, diags)
					t.Fatalf("%s raised for a task_timeout the task does not write itself", code)
				}
				return
			}
			if !hasCodeAt(diags, code, "$.tasks[0].settings.task_timeout") || countCodes(diags, code) != 1 {
				dump(t, diags)
				t.Fatalf("want exactly one %s at $.tasks[0].settings.task_timeout", code)
			}
		})
	}

	// A destiny task has no `settings:` at all: the key itself is the one refusal.
	for name, tc := range map[string]struct{ src, at string }{
		"destiny, top level": {`- name: give up
  module: core.task.failed
  settings: { task_timeout: 30s }
  params: { message: broken }
`, "$[0].settings"},
		"destiny, under block": {`- name: guarded
  block:
    - name: give up
      module: core.task.failed
      settings: { task_timeout: 30s }
      params: { message: broken }
`, "$[0].block[0].settings"},
	} {
		t.Run(name, func(t *testing.T) {
			diags := destinyPlan(t, tc.src, nil)
			if countCodes(diags, code) != 0 || countCodes(diags, "unknown_key") != 1 || !hasCodeAt(diags, "unknown_key", tc.at) {
				dump(t, diags)
				t.Fatalf("want zero %s and exactly one unknown_key, at %s", code, tc.at)
			}
		})
	}
}
