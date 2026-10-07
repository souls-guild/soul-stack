package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// serialScopeDiags runs both serial_scope checks the way their callers do: on the
// plan after include expansion, the across-tasks one over its stratification.
func serialScopeDiags(t *testing.T, src string, includes map[string]string) []diag.Diagnostic {
	t.Helper()
	m, diags := loadSettingsScenario(t, src)
	requireNoErrors(t, diags)
	tasks, xdiags := ExpandIncludes(m.Tasks, mapResolver(includes))
	requireNoErrors(t, xdiags)
	plan, err := Stratify(tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	return append(ImplicitSerialScope(m.Settings, tasks), ImplicitSerialScopeAcrossTasks(m.Settings, tasks, plan)...)
}

func diagCodes(ds []diag.Diagnostic) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

const (
	implicit = CodeSerialScopeImplicit
	across   = CodeSerialScopeImplicitAcrossTasks
	unread   = CodeGroupChildKeyUnread
)

func TestSerialScope_Codes(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		includes map[string]string
		want     []string
	}{
		{
			name: "a serial block over two host tasks with no line is refused",
			src: `name: s
tasks:
  - name: roll
    serial: 1
    block:
      - { name: change, module: core.exec.run, params: { cmd: "true" } }
      - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{implicit},
		},
		{
			name: "the default does not count as written, beside other scenario settings",
			src: `name: s
settings: { on_failure: host }
tasks:
  - name: roll
    serial: 1
    block:
      - { name: change, module: core.exec.run, params: { cmd: "true" } }
      - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{implicit},
		},
		{
			name: "settings: on the block without serial_scope is not a written line",
			src: `name: s
tasks:
  - name: roll
    serial: 1
    settings: { on_failure: host }
    block:
      - { name: change, module: core.exec.run, params: { cmd: "true" } }
      - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{implicit},
		},
		{
			name: "serial_scope: block on the block",
			src: `name: s
tasks:
  - name: roll
    serial: 1
    settings: { serial_scope: block }
    block:
      - { name: change, module: core.exec.run, params: { cmd: "true" } }
      - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{},
		},
		{
			name: "serial_scope: task on the block opts out too",
			src: `name: s
tasks:
  - name: roll
    serial: 1
    settings: { serial_scope: task }
    block:
      - { name: change, module: core.exec.run, params: { cmd: "true" } }
      - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{},
		},
		{
			name: "a line at the scenario level",
			src: `name: s
settings: { serial_scope: task }
tasks:
  - name: roll
    serial: 1
    block:
      - { name: change, module: core.exec.run, params: { cmd: "true" } }
      - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{},
		},
		{
			name: "a line on an enclosing block without serial:",
			src: `name: s
tasks:
  - name: outer
    settings: { serial_scope: block }
    block:
      - name: roll
        serial: 1
        block:
          - { name: change, module: core.exec.run, params: { cmd: "true" } }
          - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{},
		},
		{
			name: "one host task under serial: is not a change",
			src: `name: s
tasks:
  - name: roll
    serial: 1
    block:
      - { name: change, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{},
		},
		{
			name: "host tasks are counted through a nested block",
			src: `name: s
tasks:
  - name: roll
    serial: 1
    block:
      - name: inner
        block:
          - { name: change, module: core.exec.run, params: { cmd: "true" } }
      - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{implicit},
		},
		{
			name: "a nested block is not reported again under the outermost",
			src: `name: s
tasks:
  - name: roll
    serial: 2
    block:
      - name: inner
        block:
          - { name: change, module: core.exec.run, params: { cmd: "true" } }
          - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{implicit},
		},
		{
			name: "a block without a name is reached too",
			src: `name: s
tasks:
  - serial: 2
    block:
      - { name: change, module: core.exec.run, params: { cmd: "true" } }
      - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{implicit},
		},
		{
			name: "a serial block whose hosts sit only in a nested block rolls nothing today, so it shares no wave",
			src: `name: s
tasks:
  - name: roll
    serial: 2
    block:
      - name: inner
        block:
          - { name: change, module: core.exec.run, params: { cmd: "true" } }
          - { name: check, module: core.exec.run, params: { cmd: "true" } }
  - { name: after, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{implicit},
		},
		{
			name: "a serial block rolls today through a nested block with its own serial:",
			src: `name: s
tasks:
  - name: roll
    serial: 2
    block:
      - name: inner
        serial: 1
        block:
          - { name: change, module: core.exec.run, params: { cmd: "true" } }
  - { name: after, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{across},
		},
		{
			name: "the second host task arrives through include:",
			src: `name: s
tasks:
  - name: roll
    serial: 1
    block:
      - { name: change, module: core.exec.run, params: { cmd: "true" } }
      - include: check.yml
`,
			includes: map[string]string{"check.yml": `
- { name: check, module: core.exec.run, params: { cmd: "true" } }
`},
			want: []string{implicit},
		},
		{
			name: "two top-level host tasks in one Passage, serial first",
			src: `name: s
tasks:
  - { name: change, module: core.exec.run, serial: 2, params: { cmd: "true" } }
  - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{across},
		},
		{
			name: "two top-level host tasks in one Passage, serial last",
			src: `name: s
tasks:
  - { name: stop, module: core.exec.run, params: { cmd: "true" } }
  - { name: change, module: core.exec.run, serial: 2, params: { cmd: "true" } }
`,
			want: []string{across},
		},
		{
			name: "keeper-side tasks do not share a wave (the serial_waves shape)",
			src: `name: s
tasks:
  - { name: wave, module: core.cmd.shell, serial: 3, params: { cmd: "true" } }
  - { name: record, module: core.state.set, params: { field: last_run, value: x } }
  - { name: record more, module: core.state.set, params: { field: marker, value: y } }
`,
			want: []string{},
		},
		{
			name: "an assert: does not share a wave",
			src: `name: s
tasks:
  - name: precondition
    assert:
      that: ["true"]
      message: never
  - { name: change, module: core.exec.run, serial: 2, params: { cmd: "true" } }
`,
			want: []string{},
		},
		{
			name: "serial tasks in different Passages do not share a wave",
			src: `name: s
tasks:
  - { name: probe, module: core.exec.run, register: role, params: { cmd: "true" } }
  - name: change
    module: core.exec.run
    serial: 1
    where: register.role.stdout == 'slave'
    params: { cmd: "true" }
`,
			want: []string{},
		},
		{
			name: "serial_scope: task on the serial task opts out",
			src: `name: s
tasks:
  - name: change
    module: core.exec.run
    serial: 2
    settings: { serial_scope: task }
    params: { cmd: "true" }
  - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{},
		},
		{
			name: "serial_scope: block at the scenario level opts out too",
			src: `name: s
settings: { serial_scope: block }
tasks:
  - { name: change, module: core.exec.run, serial: 2, params: { cmd: "true" } }
  - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{},
		},
		{
			name: "a block without serial: splits into its children",
			src: `name: s
tasks:
  - name: outer
    block:
      - name: roll
        serial: 2
        block:
          - { name: change, module: core.exec.run, params: { cmd: "true" } }
      - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{across},
		},
		{
			name: "a module child's own serial: is not read today, so it carries none",
			src: `name: s
tasks:
  - name: outer
    block:
      - { name: change, module: core.exec.run, serial: 2, params: { cmd: "true" } }
      - { name: check, module: core.exec.run, params: { cmd: "true" } }
`,
			want: []string{},
		},
		{
			name: "the dragonfly restart shape trips both",
			src: `name: s
tasks:
  - { name: probe, module: core.exec.run, register: role, params: { cmd: "true" } }
  - name: roll replicas
    where: register.role.stdout == 'slave'
    serial: 1
    block:
      - { name: restart, module: core.service.restarted, params: { name: df } }
      - { name: wait, module: core.exec.run, params: { cmd: "true" } }
  - name: restart master last
    module: core.service.restarted
    where: register.role.stdout == 'master'
    run_once: true
    params: { name: df }
`,
			want: []string{implicit, across},
		},
		{
			name: "the dragonfly restart shape with its line",
			src: `name: s
tasks:
  - { name: probe, module: core.exec.run, register: role, params: { cmd: "true" } }
  - name: roll replicas
    where: register.role.stdout == 'slave'
    serial: 1
    settings: { serial_scope: block }
    block:
      - { name: restart, module: core.service.restarted, params: { name: df } }
      - { name: wait, module: core.exec.run, params: { cmd: "true" } }
  - name: restart master last
    module: core.service.restarted
    where: register.role.stdout == 'master'
    run_once: true
    params: { name: df }
`,
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := diagCodes(serialScopeDiags(t, tc.src, tc.includes))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("codes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSerialScope_Messages(t *testing.T) {
	ds := serialScopeDiags(t, `name: s
tasks:
  - name: roll
    serial: 2
    settings: { serial_scope: task }
    block:
      - { name: one, module: core.exec.run, params: { cmd: "true" } }
  - name: outer
    serial: 1
    block:
      - name: inner
        block:
          - { name: a, module: core.exec.run, params: { cmd: "true" } }
      - { name: b, module: core.exec.run, params: { cmd: "true" } }
`, nil)
	if len(ds) != 2 {
		t.Fatalf("diagnostics = %+v, want one implicit and one across-tasks", ds)
	}
	if ds[0].Code != implicit || !strings.Contains(ds[0].Message, `block "outer" carries serial: 1 over 2 host tasks`) {
		t.Errorf("implicit message = %q", ds[0].Message)
	}
	if ds[1].Code != across || !strings.Contains(ds[1].Message, `"outer" carries serial: 1 and shares Passage 0 of the derived plan with host task "roll"`) {
		t.Errorf("across-tasks message = %q", ds[1].Message)
	}
	for _, d := range ds {
		if d.Level != diag.LevelError {
			t.Errorf("%s level = %v, want error", d.Code, d.Level)
		}
	}
}

func TestSerialScope_GroupChildKeyUnread(t *testing.T) {
	ds := serialScopeDiags(t, `name: s
settings: { serial_scope: block }
tasks:
  - name: canary
    serial: 2
    settings: { tolerate: 1 }
    block:
      - name: change
        module: core.exec.run
        serial: 1
        settings: { on_failure: host, task_timeout: 1m }
        params: { cmd: "true" }
      - name: nested
        block:
          - name: check
            module: core.exec.run
            settings: { tolerate: "10%" }
            params: { cmd: "true" }
  - name: overlay
    settings: { serial_scope: block }
    block:
      - name: not in a group
        module: core.exec.run
        serial: 1
        settings: { tolerate: 1 }
        params: { cmd: "true" }
  - name: rolling overlay
    serial: 2
    settings: { serial_scope: task }
    block:
      - name: inherits serial and groups
        settings: { serial_scope: block }
        block:
          - name: grouped child
            module: core.exec.run
            settings: { tolerate: 1 }
            params: { cmd: "true" }
  - serial: 1
    settings: { serial_scope: block }
    block:
      - { module: core.exec.run, serial: 1, params: { cmd: "true" } }
`, nil)
	var got []string
	for _, d := range ds {
		if d.Code != unread {
			t.Fatalf("unexpected %s: %s", d.Code, d.Message)
		}
		if d.Level != diag.LevelWarning {
			t.Errorf("%s level = %v, want warning", d.Code, d.Level)
		}
		got = append(got, strings.SplitN(d.Message, " is not read", 2)[0])
	}
	want := []string{`serial: on "change"`, `settings.tolerate on "check"`, `settings.tolerate on "grouped child"`, `serial: on "core.exec.run"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("warned keys = %q, want %q", got, want)
	}
}

// Each key's written reports that key and no other, so a barrier-kind key added
// to the table is warned about on a group's child without a second list to keep.
func TestSettingKeys_WrittenNamesItsOwnKey(t *testing.T) {
	samples := map[string]string{
		"on_failure":       "host",
		"tolerate":         "1",
		"soul_timeout":     "1m",
		"task_timeout":     "1m",
		"scenario_timeout": "1h",
		"serial_scope":     "task",
	}
	for _, k := range settingKeys {
		sample, ok := samples[k.name]
		if !ok {
			t.Fatalf("no sample value for settings key %q: add one", k.name)
		}
		var s ScenarioSettings
		if err := yaml.Unmarshal([]byte(k.name+": "+sample), &s); err != nil {
			t.Fatalf("%s: %v", k.name, err)
		}
		for _, other := range settingKeys {
			if got := other.written(s); got != (other.name == k.name) {
				t.Errorf("settings {%s: %s}: %s.written = %v", k.name, sample, other.name, got)
			}
		}
	}
}
