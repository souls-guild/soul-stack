package config

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// specExample is docs/scenario/settings.md §1, with the params a module task
// needs to load.
const specExample = `name: converge

settings:
  on_failure: scenario
  tolerate: "10%"
  soul_timeout: 1m
  task_timeout: 0
  scenario_timeout: 24h

tasks:
  - name: probe replication lag
    module: core.exec.run
    params: { cmd: "true" }
    register: lag
    settings:
      on_failure: host

  - name: rolling config update
    settings:
      on_failure: host
    block:
      - name: render config
        module: core.file.rendered
        params: { path: /etc/app.conf, template: templates/app.conf.tmpl }
      - name: migrate schema
        module: core.exec.run
        params: { cmd: "true" }
        settings:
          on_failure: scenario
          task_timeout: 30m
`

func loadSettingsScenario(t *testing.T, src string) (*ScenarioManifest, []diag.Diagnostic) {
	t.Helper()
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return m, diags
}

func codesAt(diags []diag.Diagnostic) map[string]string {
	out := map[string]string{}
	for _, d := range diags {
		out[d.YAMLPath] = d.Code
	}
	return out
}

func requireNoErrors(t *testing.T, diags []diag.Diagnostic) {
	t.Helper()
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %+v", diags)
	}
}

// effectiveOf resolves a task the way render will: defaults, then the scenario
// level, then the task's own level after block inheritance.
func effectiveOf(m *ScenarioManifest, task Task) ScenarioSettings {
	top := DefaultScenarioSettings()
	if m.Settings != nil {
		top = MergeSettings(top, *m.Settings)
	}
	return MergeSettings(top, task.OwnSettings())
}

// inheritChild is the settings row of keeper/internal/render.mergeBlockInheritance.
func inheritChild(block, child Task) Task {
	child.Settings = InheritSettings(block.Settings, child)
	return child
}

func TestScenarioSettings_SpecExampleMergesPerTask(t *testing.T) {
	m, diags := loadSettingsScenario(t, specExample)
	requireNoErrors(t, diags)

	probe, group := m.Tasks[0], m.Tasks[1]
	render := inheritChild(group, group.Block.Block[0])
	migrate := inheritChild(group, group.Block.Block[1])

	tenPercent := &Tolerate{N: 10, Percent: true}
	want := map[string]ScenarioSettings{
		"probe replication lag": {OnFailure: new(OnFailureHost), Tolerate: tenPercent, SoulTimeout: new(time.Minute), TaskTimeout: new(time.Duration(0)), ScenarioTimeout: new(24 * time.Hour), SerialScope: new(SerialScopeTask)},
		"rolling config update": {OnFailure: new(OnFailureHost), Tolerate: tenPercent, SoulTimeout: new(time.Minute), TaskTimeout: new(time.Duration(0)), ScenarioTimeout: new(24 * time.Hour), SerialScope: new(SerialScopeTask)},
		"render config":         {OnFailure: new(OnFailureHost), Tolerate: tenPercent, SoulTimeout: new(time.Minute), TaskTimeout: new(time.Duration(0)), ScenarioTimeout: new(24 * time.Hour), SerialScope: new(SerialScopeTask)},
		"migrate schema":        {OnFailure: new(OnFailureScenario), Tolerate: tenPercent, SoulTimeout: new(time.Minute), TaskTimeout: new(30 * time.Minute), ScenarioTimeout: new(24 * time.Hour), SerialScope: new(SerialScopeTask)},
	}
	for _, task := range []Task{probe, group, render, migrate} {
		if got := effectiveOf(m, task); !reflect.DeepEqual(got, want[task.Name]) {
			t.Errorf("%s: effective settings\n got  %s\n want %s", task.Name, settingsString(got), settingsString(want[task.Name]))
		}
	}
}

// Guard: a child naming one key inherits every other. Reflect-driven so a field
// added to ScenarioSettings and forgotten in MergeSettings fails here too.
func TestMergeSettings_ChildOverridesOnlyTheKeysItNames(t *testing.T) {
	parent := ScenarioSettings{
		OnFailure: new(OnFailureHost), Tolerate: &Tolerate{N: 3},
		SoulTimeout: new(5 * time.Minute), TaskTimeout: new(time.Hour),
		ScenarioTimeout: new(2 * time.Hour), SerialScope: new(SerialScopeBlock),
	}
	other := ScenarioSettings{
		OnFailure: new(OnFailureScenario), Tolerate: &Tolerate{N: 7, Percent: true},
		SoulTimeout: new(time.Second), TaskTimeout: new(time.Duration(0)),
		ScenarioTimeout: new(time.Minute), SerialScope: new(SerialScopeTask),
	}
	pv, ov := reflect.ValueOf(parent), reflect.ValueOf(other)
	for i := range pv.NumField() {
		if pv.Field(i).IsNil() || ov.Field(i).IsNil() {
			t.Fatalf("fixture leaves %s unset — set it on both sides", pv.Type().Field(i).Name)
		}
		var child ScenarioSettings
		reflect.ValueOf(&child).Elem().Field(i).Set(ov.Field(i))

		want := parent
		reflect.ValueOf(&want).Elem().Field(i).Set(ov.Field(i))
		if got := MergeSettings(parent, child); !reflect.DeepEqual(got, want) {
			t.Errorf("child naming only %s:\n got  %s\n want %s", pv.Type().Field(i).Name, settingsString(got), settingsString(want))
		}
	}
	if got := MergeSettings(parent, ScenarioSettings{}); !reflect.DeepEqual(got, parent) {
		t.Errorf("an empty child changed the parent: %s", settingsString(got))
	}
}

// Every field is reachable from YAML: a field without a settingKeys row would
// be dead, and a row without a field cannot store anything.
func TestSettingKeys_CoverEveryField(t *testing.T) {
	typ := reflect.TypeOf(ScenarioSettings{})
	if len(settingKeys) != typ.NumField() {
		t.Fatalf("%d settingKeys rows for %d ScenarioSettings fields", len(settingKeys), typ.NumField())
	}
	samples := map[string]struct {
		yaml string
		want ScenarioSettings
	}{
		"on_failure":       {"host", ScenarioSettings{OnFailure: new(OnFailureHost)}},
		"tolerate":         {"2", ScenarioSettings{Tolerate: &Tolerate{N: 2}}},
		"soul_timeout":     {"1m", ScenarioSettings{SoulTimeout: new(time.Minute)}},
		"task_timeout":     {"2m", ScenarioSettings{TaskTimeout: new(2 * time.Minute)}},
		"scenario_timeout": {"1h", ScenarioSettings{ScenarioTimeout: new(time.Hour)}},
		"serial_scope":     {"block", ScenarioSettings{SerialScope: new(SerialScopeBlock)}},
	}
	for _, k := range settingKeys {
		sample, ok := samples[k.name]
		if !ok {
			t.Errorf("row %q has no sample here — add one", k.name)
			continue
		}
		m, diags := loadSettingsScenario(t, "name: s\nsettings: {"+k.name+": "+sample.yaml+"}\ntasks: []\n")
		requireNoErrors(t, diags)
		if got := deref(m.Settings); !reflect.DeepEqual(got, sample.want) {
			t.Errorf("settings: {%s: %s} decoded to %s, want %s", k.name, sample.yaml, settingsString(got), settingsString(sample.want))
		}
	}
}

func deref(s *ScenarioSettings) ScenarioSettings {
	if s == nil {
		return ScenarioSettings{}
	}
	return *s
}

func settingsString(s ScenarioSettings) string {
	out := "{"
	v := reflect.ValueOf(s)
	for i := range v.NumField() {
		out += " " + v.Type().Field(i).Name + "="
		if v.Field(i).IsNil() {
			out += "nil"
			continue
		}
		out += fmt.Sprint(v.Field(i).Elem().Interface())
	}
	return out + " }"
}

// The timeout fold happens on the child's level, before the merge: a block's
// task_timeout must not beat the child's own `timeout:`.
func TestInheritSettings_ChildTimeoutBeatsBlockTaskTimeout(t *testing.T) {
	block := Task{Settings: &ScenarioSettings{TaskTimeout: new(30 * time.Minute)}}
	child := Task{Timeout: "5m"}
	got := InheritSettings(block.Settings, child)
	if got == nil || got.TaskTimeout == nil || *got.TaskTimeout != 5*time.Minute {
		t.Fatalf("child timeout: 5m under a block task_timeout: 30m resolved to %s, want 5m", settingsString(deref(got)))
	}
}

func TestInheritSettings_BlockWithoutSettingsKeepsTheChildsOwn(t *testing.T) {
	own := &ScenarioSettings{OnFailure: new(OnFailureHost)}
	child := Task{Timeout: "5m", Settings: own}
	if got := InheritSettings(nil, child); got != own {
		t.Fatalf("a block with no settings changed the child's settings: %s, want %s", settingsString(deref(got)), settingsString(*own))
	}
	if got := InheritSettings(nil, Task{Timeout: "5m"}); got != nil {
		t.Errorf("a block with no settings gave a child with none %s", settingsString(*got))
	}
}

// A scenario's included body is loaded by the destiny-task loader without the
// destiny flag, and accepts `settings:`; a destiny body refuses it.
func TestSettings_IncludedBodyAcceptsADestinyRefuses(t *testing.T) {
	body := []byte("- name: t\n  module: core.exec.run\n  params: { cmd: \"true\" }\n  settings: { on_failure: host }\n")
	tasks, diags, err := LoadDestinyTasksFromBytes("_shared/deploy.yml", body, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requireNoErrors(t, diags)
	if s := tasks[0].OwnSettings(); s.OnFailure == nil || *s.OnFailure != OnFailureHost {
		t.Errorf("included body: on_failure not decoded, got %s", settingsString(s))
	}
	_, diags, err = LoadDestinyTasksFromBytes("tasks/main.yml", body, ValidateOptions{DestinyTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	if codesAt(diags)["$[0].settings"] != "unknown_key" {
		t.Errorf("destiny body: want unknown_key at $[0].settings, got %+v", diags)
	}
}

// A destiny's `settings:` gets the one diagnostic that is true there, not the
// scenario-placement codes and hints the shared task validator would add.
func TestSettings_DestinyGetsOneDiagnostic(t *testing.T) {
	_, diags, err := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(`- name: t
  module: core.exec.run
  params: { cmd: "true" }
  timeout: 5m
  settings: { scenario_timeout: 1h, task_timeout: 10m, bogus: 1 }
`), ValidateOptions{DestinyTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, d := range diags {
		if d.Level != diag.LevelHint {
			codes = append(codes, d.YAMLPath+" "+d.Code)
		}
	}
	if !slices.Equal(codes, []string{"$[0].settings unknown_key"}) {
		t.Errorf("destiny settings: got %v, want only the unknown_key on the key", codes)
	}
}

func TestTaskOwnSettings_ExplicitTaskTimeoutBeatsTimeout(t *testing.T) {
	task := Task{Timeout: "5m", Settings: &ScenarioSettings{TaskTimeout: new(time.Hour)}}
	if got := task.OwnSettings().TaskTimeout; got == nil || *got != time.Hour {
		t.Fatalf("task_timeout: 1h beside timeout: 5m resolved to %v, want 1h", got)
	}
	if got := (Task{Timeout: "5m"}).OwnSettings().TaskTimeout; got == nil || *got != 5*time.Minute {
		t.Fatalf("timeout: 5m alone resolved to %v, want 5m", got)
	}
}

// Guard: a run-kind key is refused on every level below the scenario, and
// accepted at the scenario level.
func TestSettings_RunKindKeyBelowScenarioIsRefused(t *testing.T) {
	m, diags := loadSettingsScenario(t, `name: s
settings:
  scenario_timeout: 2h
tasks:
  - name: on a task
    module: core.exec.run
    params: { cmd: "true" }
    settings: { scenario_timeout: 1h }
  - name: on a block
    settings: { scenario_timeout: 1h }
    block:
      - name: on a child
        module: core.exec.run
        params: { cmd: "true" }
        settings: { scenario_timeout: 1h }
  - name: on an applier
    apply: { destiny: d }
    settings: { scenario_timeout: 1h }
`)
	got := codesAt(diags)
	for _, path := range []string{
		"$.tasks[0].settings.scenario_timeout",
		"$.tasks[1].settings.scenario_timeout",
		"$.tasks[1].block[0].settings.scenario_timeout",
		"$.tasks[2].settings.scenario_timeout",
	} {
		if got[path] != "settings_run_key_below_scenario" {
			t.Errorf("%s: code %q, want settings_run_key_below_scenario (all: %v)", path, got[path], got)
		}
	}
	if code, refused := got["$.settings.scenario_timeout"]; refused {
		t.Errorf("scenario-level scenario_timeout refused with %s", code)
	}
	if m.Settings == nil || m.Settings.ScenarioTimeout == nil || *m.Settings.ScenarioTimeout != 2*time.Hour {
		t.Errorf("scenario-level scenario_timeout not decoded: %+v", m.Settings)
	}
}

// Guard: an unknown key is an error at every level, never skipped.
func TestSettings_UnknownKeyIsAnError(t *testing.T) {
	_, diags := loadSettingsScenario(t, `name: s
settings: { on_failur: host }
tasks:
  - name: t
    module: core.exec.run
    params: { cmd: "true" }
    settings: { retries: 3 }
`)
	got := codesAt(diags)
	for _, path := range []string{"$.settings.on_failur", "$.tasks[0].settings.retries"} {
		if got[path] != "unknown_key" {
			t.Errorf("%s: code %q, want unknown_key (all: %v)", path, got[path], got)
		}
	}
	if !diag.HasErrors(diags) {
		t.Errorf("an unknown settings key must be an error")
	}
}

func TestSettings_RefusedWhereTheBlockCannotBeWritten(t *testing.T) {
	_, diags := loadSettingsScenario(t, `name: s
tasks:
  - name: a keeper-side core module
    module: core.soul.registered
    params: { sid: a.example.com }
    settings: { on_failure: host }
  - name: a keeper-side plugin
    module: acme.thing.made
    on: keeper
    params: {}
    settings: { task_timeout: 5m }
  - name: an assert
    assert: { that: ["true"] }
    settings: { on_failure: host }
  - name: an include
    include: other.yml
    settings: { on_failure: host }
`)
	got := codesAt(diags)
	for path, want := range map[string]string{
		"$.tasks[0].settings": "settings_on_keeper_invalid",
		"$.tasks[1].settings": "settings_on_keeper_invalid",
		"$.tasks[2].settings": "settings_on_assert_invalid",
		"$.tasks[3].settings": "settings_on_include_invalid",
	} {
		if got[path] != want {
			t.Errorf("%s: code %q, want %s (all: %v)", path, got[path], want, got)
		}
	}
}

func TestSettings_AllowedOnEveryOtherLevel(t *testing.T) {
	_, diags := loadSettingsScenario(t, `name: s
settings: { serial_scope: block }
tasks:
  - name: module task
    module: core.exec.run
    params: { cmd: "true" }
    settings: { on_failure: host, tolerate: 2, soul_timeout: 5m, task_timeout: 1h, serial_scope: task }
  - name: applier
    apply: { destiny: d }
    settings: { task_timeout: 10m }
  - name: block
    serial: 2
    settings: { serial_scope: block, tolerate: "0%" }
    block:
      - name: nested block
        settings: { on_failure: scenario }
        block:
          - name: child
            module: core.exec.run
            params: { cmd: "true" }
            settings: { task_timeout: 0 }
  - name: loop
    module: core.exec.run
    params: { cmd: "true" }
    loop: { items: [1, 2] }
    settings: { soul_timeout: 2m }
`)
	requireNoErrors(t, diags)
}

func TestSettings_DestinyHasNoSettingsKey(t *testing.T) {
	_, diags, err := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(`- name: top
  module: core.exec.run
  params: { cmd: "true" }
  settings: { task_timeout: 5m }
- name: group
  block:
    - name: child
      module: core.exec.run
      params: { cmd: "true" }
      settings: { on_failure: host }
`), ValidateOptions{DestinyTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	got := codesAt(diags)
	for _, path := range []string{"$[0].settings", "$[1].block[0].settings"} {
		if got[path] != "unknown_key" {
			t.Errorf("%s: code %q, want unknown_key (all: %v)", path, got[path], got)
		}
	}
}

func TestSettings_ValueErrors(t *testing.T) {
	cases := map[string]struct{ yaml, code string }{
		"on_failure enum":            {`on_failure: abort`, "enum_invalid"},
		"serial_scope enum":          {`serial_scope: group`, "enum_invalid"},
		"on_failure type":            {`on_failure: 1`, "type_mismatch"},
		"tolerate negative":          {`tolerate: -1`, "value_out_of_range"},
		"tolerate percent too large": {`tolerate: "101%"`, "value_out_of_range"},
		"tolerate bare string":       {`tolerate: "3"`, "value_out_of_range"},
		"tolerate type":              {`tolerate: [1]`, "type_mismatch"},
		"duration garbage":           {`task_timeout: soon`, "duration_invalid"},
		"duration unitless":          {`soul_timeout: 30`, "type_mismatch"},
		"soul_timeout zero":          {`soul_timeout: 0`, "value_out_of_range"},
		"scenario_timeout zero":      {`scenario_timeout: "0s"`, "value_out_of_range"},
		"task_timeout negative":      {`task_timeout: -5m`, "value_out_of_range"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, diags := loadSettingsScenario(t, "name: s\nsettings: {"+c.yaml+"}\ntasks: []\n")
			found := false
			for _, d := range diags {
				found = found || d.Code == c.code
			}
			if !found {
				t.Errorf("settings: {%s}: want %s, got %+v", c.yaml, c.code, diags)
			}
		})
	}
	_, diags := loadSettingsScenario(t, "name: s\nsettings: 5\ntasks: []\n")
	if codesAt(diags)["$.settings"] != "type_mismatch" {
		t.Errorf("settings: 5 — want type_mismatch at $.settings, got %+v", diags)
	}
}

func TestSettings_TimeoutShadowedByTaskTimeoutWarns(t *testing.T) {
	_, diags := loadSettingsScenario(t, `name: s
tasks:
  - name: differ
    module: core.exec.run
    params: { cmd: "true" }
    timeout: 5m
    settings: { task_timeout: 10m }
  - name: same value, other spelling
    module: core.exec.run
    params: { cmd: "true" }
    timeout: 1h
    settings: { task_timeout: 60m }
`)
	requireNoErrors(t, diags)
	var warned []string
	for _, d := range diags {
		if d.Code == "timeout_shadowed_by_task_timeout" {
			if d.Level != diag.LevelWarning {
				t.Errorf("%s is %s, want a warning", d.Code, d.Level)
			}
			warned = append(warned, d.YAMLPath)
		}
	}
	if !slices.Equal(warned, []string{"$.tasks[0].timeout"}) {
		t.Errorf("warned at %v, want only $.tasks[0].timeout", warned)
	}
}
