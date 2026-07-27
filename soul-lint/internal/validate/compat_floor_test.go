package validate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// The diagnostic must land on the file carrying the DECLARATION — for a scenario
// that is the service manifest two directories up, not the file being linted.
func TestFloorDiags_ReportedOnTheDeclaringFile(t *testing.T) {
	got := floorDiags(
		"/srv/redis/service.yml",
		&config.VersionWindow{Min: "1.0.0", Max: "3.0.0"},
		[]config.KeeperFeature{{ID: "core.file.present.params.selinux", IntroducedIn: "2.5.0", Where: "$.tasks[1]"}},
	)
	if len(got) != 1 {
		t.Fatalf("diags = %+v, want one compat_floor_too_low", got)
	}
	if got[0].Code != "compat_floor_too_low" || got[0].File != "/srv/redis/service.yml" {
		t.Fatalf("diag = %+v, want the code on the declaring file", got[0])
	}
}

func TestFloorDiags_HonestDeclarationIsSilent(t *testing.T) {
	got := floorDiags(
		"/srv/redis/service.yml",
		&config.VersionWindow{Min: "2.5.0"},
		[]config.KeeperFeature{{ID: "core.file.present.params.selinux", IntroducedIn: "2.5.0"}},
	)
	if len(got) != 0 {
		t.Fatalf("diags = %+v, want silence", got)
	}
}

// A destiny is weighed against its manifest grammar AND its task file. A task
// file that does not parse is skipped rather than fatal: its own errors are
// reported by the paths that own it, and half a definition yields a floor worse
// than none.
func TestDestinyKeeperFeatures_TaskFile(t *testing.T) {
	manifest := &config.DestinyManifest{
		Name:     "redis",
		Compat:   &config.CompatConfig{Keeper: &config.VersionWindow{Min: "0.1.0"}},
		Validate: []config.ValidateRule{{That: "input.port > 0"}},
	}

	for _, tc := range []struct {
		name  string
		tasks string
	}{
		{name: "valid task file", tasks: "- name: install\n  module: core.pkg.installed\n  params: {name: redis}\n"},
		{name: "unparseable task file", tasks: "- : : :\n"},
		{name: "no task file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "destiny.yml")
			if tc.tasks != "" {
				if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "tasks", "main.yml"), []byte(tc.tasks), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ids := map[string]bool{}
			for _, f := range destinyKeeperFeatures(path, manifest) {
				ids[f.ID] = true
			}
			if !ids[config.FeatureDestinyCompat] || !ids[config.FeatureDestinyValidate] {
				t.Fatalf("features = %v, want the manifest grammar to be collected regardless of the task file", ids)
			}
		})
	}
}

// A scenario linted outside a service tree has no window to weigh against —
// standalone linting must stay silent rather than invent one.
func TestScenarioCompatFloorDiags_NoServiceManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario", "create", "main.yml")
	if got := scenarioCompatFloorDiags(path, &config.ScenarioManifest{Name: "create"}); got != nil {
		t.Fatalf("diags = %+v, want nil without a service.yml", got)
	}
}

// The service manifest two directories up is the window a scenario renders
// under. A definition using nothing above the baseline stays silent.
func TestScenarioCompatFloorDiags_ReadsServiceWindow(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scenario", "create"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := "name: redis\nstate_schema_version: 1\ncompat:\n  keeper: {min: \"0.1.0\", max: \"0.3.0\"}\n"
	if err := os.WriteFile(filepath.Join(root, "service.yml"), []byte(service), 0o600); err != nil {
		t.Fatal(err)
	}
	scn := &config.ScenarioManifest{
		Name:  "create",
		Tasks: []config.Task{{Module: &config.ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "redis"}}}},
	}
	got := scenarioCompatFloorDiags(filepath.Join(root, "scenario", "create", "main.yml"), scn)
	if got != nil {
		t.Fatalf("diags = %+v, want silence for a baseline-only scenario", got)
	}
}
