package config

// The `id_template:` → `id:` window (ADR-0079 amendment, NIM-899) at the load
// boundary: which spelling is accepted, what is said about it, what every rule
// downstream sees — and that the PREVIOUS window, `name_template:`, is now shut.
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

// windowInput is the tail every window scenario shares: one declared component and
// an empty task list, so nothing but the id keys is under test.
const windowInput = `input:
  name:
    type: string
tasks: []
`

// blockScenario / scalarScenario write the SAME template under the two spellings the
// window covers. The block form puts the template on line 4, the scalar on line 3 —
// the diagnostics below assert on those lines, because a warning without a place is
// a warning an author cannot act on.
const blockScenario = `name: create
create: true
id:
  template: "${input.name}-redis"
` + windowInput

const scalarScenario = `name: create
create: true
id_template: "${input.name}-redis"
` + windowInput

// TestIDTemplateWindow_BlockIsSilent — the migrated spelling has nothing to say, or
// every migrated repository carries a warning it cannot act on.
func TestIDTemplateWindow_BlockIsSilent(t *testing.T) {
	m, diags := loadScenarioForWindow(t, blockScenario)
	if m.ID.Template != "${input.name}-redis" {
		t.Fatalf("ID.Template = %q, want the template as written", m.ID.Template)
	}
	if m.LegacyIDTemplate != "" {
		t.Errorf("LegacyIDTemplate = %q on a file that never wrote the key", m.LegacyIDTemplate)
	}
	for _, d := range diags {
		if d.Code == "id_template_legacy_spelling" || d.Code == "id_template_conflict" {
			t.Errorf("unexpected window diagnostic on the current spelling: %+v", d)
		}
	}
}

// TestIDTemplateWindow_ScalarLoadsAndWarns is the acceptance: the retired key fills the
// SAME field every reader uses, and the file is told with an address and a replacement.
func TestIDTemplateWindow_ScalarLoadsAndWarns(t *testing.T) {
	m, diags := loadScenarioForWindow(t, scalarScenario)

	if m.ID.Template != "${input.name}-redis" {
		t.Fatalf("ID.Template = %q — the retired key did not reach the field every reader uses, "+
			"so the create composes nothing", m.ID.Template)
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
			t.Errorf("Line = %d, want 3 (the `id_template:` key) — the warning has to name a place", d.Line)
		}
		if d.YAMLPath != "$.id_template" {
			t.Errorf("YAMLPath = %q, want the key the FILE wrote, not the canonical one", d.YAMLPath)
		}
		if !strings.Contains(d.Hint, "template:") {
			t.Errorf("hint = %q, want the replacement in it", d.Hint)
		}
	}
	if !found {
		t.Fatalf("no id_template_legacy_spelling warning; diags=%v", diags)
	}
}

// TestIDTemplateWindow_ScalarCarriesNoBound is why the key became a block: a scalar has
// nowhere to say what its composed id must fit. Inventing one would make a migrated
// file and an unmigrated one enforce different bounds from the same template.
func TestIDTemplateWindow_ScalarCarriesNoBound(t *testing.T) {
	m, _ := loadScenarioForWindow(t, scalarScenario)
	if m.ID.MaxLength != 0 {
		t.Errorf("ID.MaxLength = %d on the scalar spelling, want 0 (unset)", m.ID.MaxLength)
	}
	if got := m.ID.Ceiling(); got != IncarnationIDMaxLen {
		t.Errorf("Ceiling() = %d, want the platform %d", got, IncarnationIDMaxLen)
	}
}

// TestIDTemplateWindow_DiagnosticsNameTheKeyTheFileWrote — telling an author to fix
// `id.template` in a file that has no such key is an address going nowhere.
func TestIDTemplateWindow_DiagnosticsNameTheKeyTheFileWrote(t *testing.T) {
	_, diags := loadScenarioForWindow(t, `name: create
create: true
id_template: "${input.name}-${input.nope}"
`+windowInput)
	var found bool
	for _, d := range diags {
		if d.Code != "id_template_input_unknown" {
			continue
		}
		found = true
		if !strings.HasPrefix(d.Message, "id_template ") {
			t.Errorf("message = %q, want it to name the key the file wrote", d.Message)
		}
		if d.YAMLPath != "$.id_template" || d.Line == 0 {
			t.Errorf("address = %s:%d, want $.id_template with a line", d.YAMLPath, d.Line)
		}
	}
	if !found {
		t.Fatalf("the undeclared-component rule did not run over the retired key; diags=%v", diags)
	}
}

// TestIDTemplateWindow_BlockDiagnosticsAddressTheLeaf — a file with several keys under
// `id:` needs the line of the one that is wrong.
func TestIDTemplateWindow_BlockDiagnosticsAddressTheLeaf(t *testing.T) {
	_, diags := loadScenarioForWindow(t, `name: create
create: true
id:
  template: "${input.name}-${input.nope}"
  max_length: 40
`+windowInput)
	var found bool
	for _, d := range diags {
		if d.Code != "id_template_input_unknown" {
			continue
		}
		found = true
		if !strings.HasPrefix(d.Message, "id.template ") {
			t.Errorf("message = %q, want it to name id.template", d.Message)
		}
		if d.YAMLPath != "$.id.template" || d.Line != 4 {
			t.Errorf("address = %s:%d, want $.id.template on line 4", d.YAMLPath, d.Line)
		}
	}
	if !found {
		t.Fatalf("the undeclared-component rule did not run over the block; diags=%v", diags)
	}
}

// TestIDTemplateWindow_BothSpellingsIsAnError — refused rather than resolved by
// precedence: silently preferring one makes the run disagree with half the file.
func TestIDTemplateWindow_BothSpellingsIsAnError(t *testing.T) {
	_, diags := loadScenarioForWindow(t, `name: create
create: true
id:
  template: "${input.name}-redis"
id_template: "${input.name}-valkey"
`+windowInput)
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

// TestIDTemplateWindow_NameTemplateIsRefused — the PREVIOUS window is shut (NIM-899).
// The refusal carries the replacement, and there is exactly ONE of it: the reflect
// walker's bare twin is suppressed for every key in deprecatedScenarioKeys.
func TestIDTemplateWindow_NameTemplateIsRefused(t *testing.T) {
	m, diags := loadScenarioForWindow(t, `name: create
create: true
name_template: "${input.name}-redis"
`+windowInput)

	if m.ID.Template != "" {
		t.Errorf("ID.Template = %q — the retired key must not compose anything", m.ID.Template)
	}
	var found bool
	for _, d := range diags {
		if d.Code != "unknown_key" || d.YAMLPath != "$.name_template" {
			continue
		}
		found = true
		if d.Level != diag.LevelError {
			t.Errorf("level = %v, want an ERROR — the key is gone, not deprecated", d.Level)
		}
		if !strings.Contains(d.Hint, "id:") || !strings.Contains(d.Hint, "template:") {
			t.Errorf("hint = %q, want the `id:`/`template:` replacement in it", d.Hint)
		}
		if d.Line != 3 {
			t.Errorf("Line = %d, want 3", d.Line)
		}
	}
	if !found {
		t.Fatalf("name_template was not refused; diags=%v", diags)
	}
	// One diagnostic, not two: the reflect walker's bare `unknown_key` is suppressed
	// for every key in deprecatedScenarioKeys precisely so the hinted one stands alone.
	var n int
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.name_template" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d unknown_key diagnostics for name_template, want 1 (the hinted one)", n)
	}
}

func TestIDTemplateWindow_FeatureFloorCitesTheWrittenKey(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{blockScenario, "$.id.template"},
		{scalarScenario, "$.id_template"},
	} {
		m, _ := loadScenarioForWindow(t, tc.body)
		var got string
		for _, f := range KeeperFeaturesOfScenario(m) {
			if f.ID == FeatureScenarioIDTemplate {
				got = f.Where
			}
		}
		if got != tc.want {
			t.Errorf("feature location = %q, want %q", got, tc.want)
		}
	}
}
