package artifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// The `settings:` refusals are reached from the Keeper's own runtime parse, on
// the very files soul-lint is tested against — service repositories are not
// linted, so a check living only in soul-lint would let the Keeper run them.

const lintFixtures = "../../../soul-lint/testdata"

func loadRuntimeScenario(t *testing.T, path string) []diag.Diagnostic {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	art := &ServiceArtifact{Ref: ServiceRef{Name: "svc"}, LocalDir: t.TempDir()}
	_, _, diags, err := LoadScenarioManifestResolved(art, "scenario/s/main.yml", data, nil)
	if err != nil {
		t.Fatalf("LoadScenarioManifestResolved(%s): %v", path, err)
	}
	return diags
}

func TestLoadScenarioManifestResolved_SettingsRefusalsMatchSoulLint(t *testing.T) {
	matches, err := filepath.Glob(lintFixtures + "/scenario-broken/scenario-settings-*.yml")
	if err != nil || len(matches) == 0 {
		t.Fatalf("no settings fixtures found (err=%v)", err)
	}
	for _, p := range matches {
		t.Run(filepath.Base(p), func(t *testing.T) {
			raw, err := os.ReadFile(strings.TrimSuffix(p, ".yml") + ".expected.json")
			if err != nil {
				t.Fatal(err)
			}
			var exp struct{ Codes []string }
			if err := json.Unmarshal(raw, &exp); err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			diags := loadRuntimeScenario(t, p)
			for _, d := range diags {
				if d.Level == diag.LevelError {
					got[d.Code] = true
				}
			}
			for _, c := range exp.Codes {
				if !got[c] {
					t.Errorf("the Keeper did not refuse with %s; got %+v", c, diags)
				}
			}
		})
	}
}

func TestLoadScenarioManifestResolved_SettingsGoldenLoads(t *testing.T) {
	for _, name := range []string{"settings.yml", "settings-timeout-shadowed.yml"} {
		if diags := loadRuntimeScenario(t, lintFixtures+"/scenario-golden/"+name); diag.HasErrors(diags) {
			t.Errorf("%s was refused: %+v", name, diags)
		}
	}
}
