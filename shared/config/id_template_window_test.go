package config

// The `name_template:` → `id_template:` window ([ADR-0085], NIM-730) at the load
// boundary: which spelling is accepted, what is said about it, and what every
// rule downstream sees.
//
// The create path's own half — that the retired key actually composes an id end
// to end — is guarded in keeper/internal/scenario/id_template_test.go, where
// there is a plan to inspect.

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func loadScenarioForWindow(t *testing.T, body string) (*ScenarioManifest, []diag.Diagnostic) {
	t.Helper()
	m, _, diags, err := LoadScenarioManifestFromBytes("scenario/create/main.yml", []byte(body), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	return m, diags
}

const windowScenario = `name: create
create: true
%s: "${input.name}-redis"
input:
  name:
    type: string
tasks: []
`

// TestIDTemplateWindow_NewSpellingIsSilent — the migrated spelling loads with
// nothing to say. If this ever produced the deprecation warning, every migrated
// repository would carry a warning it cannot act on.
func TestIDTemplateWindow_NewSpellingIsSilent(t *testing.T) {
	m, diags := loadScenarioForWindow(t, strings.Replace(windowScenario, "%s", "id_template", 1))
	if m.IDTemplate != "${input.name}-redis" {
		t.Fatalf("IDTemplate = %q, want the template as written", m.IDTemplate)
	}
	if m.LegacyNameTemplate != "" {
		t.Errorf("LegacyNameTemplate = %q on a file that never wrote the key", m.LegacyNameTemplate)
	}
	for _, d := range diags {
		if d.Code == "id_template_legacy_spelling" || d.Code == "id_template_conflict" {
			t.Errorf("unexpected window diagnostic on the current spelling: %+v", d)
		}
	}
}

// TestIDTemplateWindow_LegacySpellingLoadsAndWarns is the acceptance: the retired
// key still fills the SAME field every downstream reader uses — so the scenario
// keeps working — and the file is told, with an address and a replacement.
func TestIDTemplateWindow_LegacySpellingLoadsAndWarns(t *testing.T) {
	m, diags := loadScenarioForWindow(t, strings.Replace(windowScenario, "%s", "name_template", 1))

	if m.IDTemplate != "${input.name}-redis" {
		t.Fatalf("IDTemplate = %q — the retired key did not reach the field every reader uses, "+
			"so the create composes nothing", m.IDTemplate)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("the retired key must not be an ERROR while the window is open; diags=%v", diags)
	}

	var found bool
	for _, d := range diags {
		if d.Code != "id_template_legacy_spelling" {
			continue
		}
		found = true
		if d.Level != diag.LevelWarning {
			t.Errorf("level = %v, want a WARNING", d.Level)
		}
		if d.Line != 3 {
			t.Errorf("Line = %d, want 3 (the `name_template:` key) — the warning has to name a place", d.Line)
		}
		if d.YAMLPath != "$.name_template" {
			t.Errorf("YAMLPath = %q, want the key the FILE wrote, not the canonical one", d.YAMLPath)
		}
		if !strings.Contains(d.Hint, "id_template") {
			t.Errorf("hint = %q, want the replacement in it", d.Hint)
		}
	}
	if !found {
		t.Fatalf("no id_template_legacy_spelling warning; diags=%v", diags)
	}
}

// TestIDTemplateWindow_DiagnosticsNameTheKeyTheFileWrote — the template's own
// rules address the retired key too. Telling an author to fix `id_template` in a
// file that has no such key is an address that goes nowhere.
func TestIDTemplateWindow_DiagnosticsNameTheKeyTheFileWrote(t *testing.T) {
	_, diags := loadScenarioForWindow(t, `name: create
create: true
name_template: "${input.name}-${input.nope}"
input:
  name:
    type: string
tasks: []
`)
	var found bool
	for _, d := range diags {
		if d.Code != "id_template_input_unknown" {
			continue
		}
		found = true
		if !strings.HasPrefix(d.Message, "name_template ") {
			t.Errorf("message = %q, want it to name the key the file wrote", d.Message)
		}
		if d.YAMLPath != "$.name_template" || d.Line == 0 {
			t.Errorf("address = %s:%d, want $.name_template with a line", d.YAMLPath, d.Line)
		}
	}
	if !found {
		t.Fatalf("the undeclared-component rule did not run over the retired key; diags=%v", diags)
	}
}

// TestIDTemplateWindow_BothSpellingsIsAnError — two templates composing one id is
// an authoring mistake whichever value a loader picked, so it is refused rather
// than resolved by precedence. Silently preferring one would make the run
// disagree with half the file.
func TestIDTemplateWindow_BothSpellingsIsAnError(t *testing.T) {
	_, diags := loadScenarioForWindow(t, `name: create
create: true
id_template: "${input.name}-redis"
name_template: "${input.name}-valkey"
input:
  name:
    type: string
tasks: []
`)
	var found bool
	for _, d := range diags {
		if d.Code != "id_template_conflict" {
			continue
		}
		found = true
		if d.Level != diag.LevelError {
			t.Errorf("level = %v, want an ERROR: a silent winner here is a file that lies", d.Level)
		}
		if d.Line == 0 {
			t.Error("the conflict carries no address")
		}
	}
	if !found {
		t.Fatalf("declaring both spellings was accepted; diags=%v", diags)
	}
}

// TestIDTemplateWindow_FeatureFloorCitesTheWrittenKey — the compat-floor
// collector reports one feature id for both spellings (they are one grammar) but
// points at the key that is actually in the file.
func TestIDTemplateWindow_FeatureFloorCitesTheWrittenKey(t *testing.T) {
	for key, want := range map[string]string{"id_template": "$.id_template", "name_template": "$.name_template"} {
		m, _ := loadScenarioForWindow(t, strings.Replace(windowScenario, "%s", key, 1))
		var got string
		for _, f := range KeeperFeaturesOfScenario(m) {
			if f.ID == FeatureScenarioIDTemplate {
				got = f.Where
			}
		}
		if got != want {
			t.Errorf("%s: feature location = %q, want %q", key, got, want)
		}
	}
}
