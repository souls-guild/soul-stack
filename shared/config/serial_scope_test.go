package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// groupChildDiags runs the warning on the plan the way soul-lint does.
func groupChildDiags(t *testing.T, src string) []diag.Diagnostic {
	t.Helper()
	m, diags := loadSettingsScenario(t, src)
	requireNoErrors(t, diags)
	return GroupChildKeysUnread(m.Settings, m.Tasks)
}

func TestSerialScope_GroupChildKeyUnread(t *testing.T) {
	ds := groupChildDiags(t, `name: s
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
`)
	var got []string
	for _, d := range ds {
		if d.Code != CodeGroupChildKeyUnread {
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

// A serial: block that writes no serial_scope rolls by task, so its own child's
// serial: is read, and a group nested in it is warned about like any other.
func TestSerialScope_GroupChildKeyUnreadUnderTheDefault(t *testing.T) {
	ds := groupChildDiags(t, `name: s
tasks:
  - name: rolling
    serial: 2
    block:
      - { name: own width, module: core.exec.run, serial: 1, params: { cmd: "true" } }
      - name: group
        settings: { serial_scope: block }
        block:
          - { name: grouped child, module: core.exec.run, serial: 1, params: { cmd: "true" } }
`)
	if len(ds) != 1 || !strings.HasPrefix(ds[0].Message, `serial: on "grouped child" is not read`) {
		t.Fatalf("diagnostics = %+v, want one warning, for the grouped child", ds)
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
