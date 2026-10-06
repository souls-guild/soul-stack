package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// NIM-939: a destiny is rendered by the Keeper in one pass, before any of its tasks
// runs, against an empty `register` root. A Keeper-rendered key reading a register of
// the same destiny can never resolve — a plain read fails the run at render, a
// has()-guarded one silently takes its fallback. Before this check nothing refused it
// earlier: the declaring task is in the plan, so `unknown_register_reference` is quiet.

const destinyProbeTask = `- name: probe
  module: core.exec.run
  register: probe
  params:
    cmd: /usr/bin/redis-cli
    args: ["role"]
`

// loadDestinyPlan is the keeper loader's sequence (artifact.DestinyLoader.parseTasks)
// over in-memory files: the entry file parsed as a destiny, then the destiny
// expansion, which carries the flat post-pass under test.
func loadDestinyPlan(t *testing.T, files map[string]string) []diag.Diagnostic {
	t.Helper()
	tasks, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(files["main.yml"]), ValidateOptions{DestinyTasks: true})
	if diag.HasErrors(diags) {
		t.Fatalf("entry file is invalid on its own, the case proves nothing: %v", diags)
	}
	_, idiags := ExpandIncludesInDestiny(tasks, mapResolver(files))
	return idiags
}

func withinDestinyDiags(diags []diag.Diagnostic) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, d := range diags {
		if d.Code == CodeWithinDestinyRegisterDependency {
			out = append(out, d)
		}
	}
	return out
}

func TestWithinDestinyRegister_KeeperRenderedReadsAreRefused(t *testing.T) {
	cases := map[string]string{
		"params": `- name: write role
  module: core.file.present
  params:
    path: /etc/role
    content: "${ register.probe.stdout }"
`,
		// The silent shape: renders without error and takes 'none' on every run.
		"params guarded by has()": `- name: write role
  module: core.file.present
  params:
    path: /etc/role
    content: "${ has(register.probe) ? register.probe.stdout : 'none' }"
`,
		// The per-file AST walk is blind inside a block scalar; the decoded plan is not.
		"params in a folded scalar": `- name: write role
  module: core.file.present
  params:
    path: /etc/role
    content: >-
      ${ register.probe.stdout }
`,
		"vars": `- name: write role
  module: core.file.present
  vars:
    role: "${ register.probe.stdout }"
  params:
    path: /etc/role
    content: "${ vars.role }"
`,
		"where": `- name: write role
  module: core.file.present
  where: register.probe.stdout == "master"
  params:
    path: /etc/role
    content: master
`,
		"loop.items": `- name: write role
  module: core.file.present
  loop:
    items: "${ [register.probe.stdout] }"
  params:
    path: /etc/role
    content: "${ item }"
`,
		"on": `- name: write role
  module: core.file.present
  on: ["${ has(register.probe) ? 'primary' : 'replica' }"]
  params:
    path: /etc/role
    content: master
`,
		"loop.when": `- name: write role
  module: core.file.present
  loop:
    items: ["master", "slave"]
    when: register.probe.stdout == item
  params:
    path: /etc/role
    content: "${ item }"
`,
	}
	for name, consumer := range cases {
		t.Run(name, func(t *testing.T) {
			got := withinDestinyDiags(loadDestinyPlan(t, map[string]string{"main.yml": destinyProbeTask + consumer}))
			if len(got) != 1 {
				t.Fatalf("got %d %s diagnostics, want 1: %v", len(got), CodeWithinDestinyRegisterDependency, got)
			}
			if got[0].Level != diag.LevelError {
				t.Errorf("level = %v, want error", got[0].Level)
			}
			for _, want := range []string{`"write role"`, `register "probe"`, `task "probe"`} {
				if !strings.Contains(got[0].Message, want) {
					t.Errorf("message does not name %s: %q", want, got[0].Message)
				}
			}
		})
	}
}

func TestWithinDestinyRegister_OwnRegisterReadIsRefused(t *testing.T) {
	plan := `- name: probe
  module: core.exec.run
  register: probe
  params:
    cmd: /bin/echo
    args: ["${ register.probe.stdout }"]
`
	got := withinDestinyDiags(loadDestinyPlan(t, map[string]string{"main.yml": plan}))
	if len(got) != 1 || !strings.Contains(got[0].Message, `its own register "probe"`) {
		t.Fatalf("a task reading its own register by name in params was not refused as such: %v", got)
	}
}

func TestWithinDestinyRegister_BlockChildReadingASiblingChild(t *testing.T) {
	plan := `- name: group
  block:
    - name: probe
      module: core.exec.run
      register: probe
      params:
        cmd: /bin/true
    - name: write role
      module: core.file.present
      params:
        path: /etc/role
        content: "${ register.probe.stdout }"
`
	got := withinDestinyDiags(loadDestinyPlan(t, map[string]string{"main.yml": plan}))
	if len(got) != 1 || !strings.Contains(got[0].Message, `destiny task "write role"`) {
		t.Fatalf("a block child reading its sibling's register was not refused against the child: %v", got)
	}
}

// The per-file pass seeds an included body with its includer's registers, so it
// accepts this read; only the flat plan sees it.
func TestWithinDestinyRegister_DeclaredInTheIncluderReadInTheBody(t *testing.T) {
	files := map[string]string{
		"main.yml": destinyProbeTask + "- include: body.yml\n",
		"body.yml": `- name: write role
  module: core.file.present
  params:
    path: /etc/role
    content: "${ register.probe.stdout }"
`,
	}
	if got := withinDestinyDiags(loadDestinyPlan(t, files)); len(got) != 1 {
		t.Fatalf("a read one include away was not refused: %v", got)
	}
}

// Flow control and requisites are evaluated by the Soul inside the message — the reads
// a destiny legitimately makes.
func TestWithinDestinyRegister_SoulEvaluatedReadsStayLegal(t *testing.T) {
	plan := `- name: probe
  module: core.exec.run
  register: probe
  changed_when: register.self.stdout != ""
  params:
    cmd: /usr/bin/redis-cli
    args: ["role"]
- name: write role
  module: core.file.present
  when: register.probe.stdout == "master"
  changed_when: register.probe.changed
  failed_when: register.probe.failed
  retry:
    count: 2
    until: register.probe.stdout != ""
  onchanges: [probe]
  params:
    path: /etc/role
    content: master
`
	diags := loadDestinyPlan(t, map[string]string{"main.yml": plan})
	if got := withinDestinyDiags(diags); len(got) != 0 {
		t.Fatalf("a Soul-evaluated read was refused: %v", got)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("the legal plan has other errors, the case proves nothing: %v", diags)
	}
}

// In a scenario the same read is how a probe feeds a later task: stratification puts
// the reader in a later Passage, rendered against the earlier one's registers.
func TestWithinDestinyRegister_ScenarioExpansionIsNotChecked(t *testing.T) {
	plan := destinyProbeTask + `- name: write role
  module: core.file.present
  params:
    path: /etc/role
    content: "${ register.probe.stdout }"
`
	tasks, diags, _ := LoadDestinyTasksFromBytes("main.yml", []byte(plan), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("plan invalid on its own: %v", diags)
	}
	_, idiags := ExpandIncludes(tasks, mapResolver(nil))
	if got := withinDestinyDiags(idiags); len(got) != 0 {
		t.Fatalf("a scenario's probe→consumer read was refused as a destiny one: %v", got)
	}
}
