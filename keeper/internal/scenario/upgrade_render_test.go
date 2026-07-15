package scenario

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// TestParseScenarioFromArtifact_FromUpgradeSelectsUpgradeDir covers where
// recipe.FromUpgrade is threaded through RenderForHost (render_host.go):
// fromUpgrade=true loads upgrade/<slug>/main.yml, false loads
// scenario/<slug>/main.yml (ADR-0068). Both dirs hold a scenario with the
// same name but a different description, so the test can tell which file
// was actually read.
func TestParseScenarioFromArtifact_FromUpgradeSelectsUpgradeDir(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("scenario/to_v2/main.yml", "name: to_v2\ndescription: from-scenario-dir\ntasks: []\n")
	write("upgrade/to_v2/main.yml", "name: to_v2\ndescription: from-upgrade-dir\nfrom: [\"v1\"]\ntasks: []\n")

	loader := artifact.NewServiceLoader(t.TempDir(), nil)
	art := &artifact.ServiceArtifact{LocalDir: dir}

	up, err := parseScenarioFromArtifact(loader, art, "to_v2", true)
	if err != nil {
		t.Fatalf("parse upgrade/: %v", err)
	}
	if up.Description != "from-upgrade-dir" {
		t.Errorf("fromUpgrade=true → description=%q, want from-upgrade-dir (upgrade/to_v2/main.yml)", up.Description)
	}

	sc, err := parseScenarioFromArtifact(loader, art, "to_v2", false)
	if err != nil {
		t.Fatalf("parse scenario/: %v", err)
	}
	if sc.Description != "from-scenario-dir" {
		t.Errorf("fromUpgrade=false → description=%q, want from-scenario-dir (scenario/to_v2/main.yml)", sc.Description)
	}
}
