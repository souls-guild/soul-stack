package artifact

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// writeStep puts `migrations/<NNN>_<slug>/main.yml` into the snapshot dir. The slug
// is deliberately arbitrary and different per step: the loader must find a step by
// its NUMBER, and a test that names every directory the same way would pass against
// a loader that reconstructed the path instead.
func writeStep(t *testing.T, dir string, version int, slug, body string) {
	t.Helper()
	stepDir := filepath.Join(dir, config.MigrationsDirName, fmt.Sprintf("%03d_%s", version, slug))
	if err := os.MkdirAll(stepDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", stepDir, err)
	}
	if err := os.WriteFile(filepath.Join(stepDir, config.MigrationStepFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write step %d: %v", version, err)
	}
}

const migToTwo = `
transform:
  - set: { path: state.v, value: 2 }
`

const migToThree = `
transform:
  - set: { path: state.v, value: 3 }
`

func TestLoadMigrationChain_AssembleFromSteps(t *testing.T) {
	dir := t.TempDir()
	writeStep(t, dir, 2, "widen_users", migToTwo)
	writeStep(t, dir, 3, "drop_install", migToThree)

	l := NewServiceLoader(t.TempDir(), nil)
	art := &ServiceArtifact{Ref: ServiceRef{Name: "redis"}, LocalDir: dir}

	chain, err := l.LoadMigrationChain(art, 1, 3)
	if err != nil {
		t.Fatalf("LoadMigrationChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("len(chain) = %d, want 2", len(chain))
	}
	// The versions come from the directory names; neither document states one.
	if chain[0].FromVersion != 1 || chain[0].ToVersion != 2 {
		t.Errorf("chain[0] = %d→%d, want 1→2", chain[0].FromVersion, chain[0].ToVersion)
	}
	if chain[1].FromVersion != 2 || chain[1].ToVersion != 3 {
		t.Errorf("chain[1] = %d→%d, want 2→3", chain[1].FromVersion, chain[1].ToVersion)
	}
	// The step carries its own path, slug and all — the API reply reports it, and
	// it is not reconstructible from the numbers.
	if want := "migrations/002_widen_users/main.yml"; chain[0].Path != want {
		t.Errorf("chain[0].Path = %q, want %q", chain[0].Path, want)
	}
	if want := "migrations/003_drop_install/main.yml"; chain[1].Path != want {
		t.Errorf("chain[1].Path = %q, want %q", chain[1].Path, want)
	}
}

func TestLoadMigrationChain_FromEqualsTo_Empty(t *testing.T) {
	l := NewServiceLoader(t.TempDir(), nil)
	art := &ServiceArtifact{Ref: ServiceRef{Name: "redis"}, LocalDir: t.TempDir()}

	chain, err := l.LoadMigrationChain(art, 3, 3)
	if err != nil {
		t.Fatalf("LoadMigrationChain: %v", err)
	}
	if len(chain) != 0 {
		t.Errorf("len(chain) = %d, want 0 (no-op)", len(chain))
	}
}

func TestLoadMigrationChain_MissingStep_ChainBroken(t *testing.T) {
	dir := t.TempDir()
	// Only the first step; nothing leads to version 3.
	writeStep(t, dir, 2, "widen_users", migToTwo)

	l := NewServiceLoader(t.TempDir(), nil)
	art := &ServiceArtifact{Ref: ServiceRef{Name: "redis"}, LocalDir: dir}

	_, err := l.LoadMigrationChain(art, 1, 3)
	if !errors.Is(err, ErrMigrationChainBroken) {
		t.Fatalf("err = %v, want ErrMigrationChainBroken", err)
	}
	// The operator has to be told WHICH rung is missing; "chain broken" alone sends
	// them reading the whole ladder.
	if !strings.Contains(err.Error(), "003_*") {
		t.Errorf("error %q does not name the missing version", err)
	}
}

// TestLoadMigrationChain_UnrecognisedStepDir_ChainBroken — a step directory the
// engine cannot read as `<NNN>_<slug>` is a step that does not exist.
//
// This is the failure the derived version made possible and `soul-lint` exists to
// catch (`migration_step_name_invalid`): the file is right there, the author sees a
// full ladder, and the engine counts one rung fewer. The loader must not paper over
// it by falling back to a name match.
func TestLoadMigrationChain_UnrecognisedStepDir_ChainBroken(t *testing.T) {
	dir := t.TempDir()
	writeStep(t, dir, 2, "widen_users", migToTwo)
	// `3_drop_install` — two digits short of the rule.
	stepDir := filepath.Join(dir, config.MigrationsDirName, "3_drop_install")
	if err := os.MkdirAll(stepDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stepDir, config.MigrationStepFile), []byte(migToThree), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	l := NewServiceLoader(t.TempDir(), nil)
	art := &ServiceArtifact{Ref: ServiceRef{Name: "redis"}, LocalDir: dir}

	if _, err := l.LoadMigrationChain(art, 1, 3); !errors.Is(err, ErrMigrationChainBroken) {
		t.Fatalf("err = %v, want ErrMigrationChainBroken", err)
	}
}

func TestLoadMigrationChain_Downgrade_Rejected(t *testing.T) {
	l := NewServiceLoader(t.TempDir(), nil)
	art := &ServiceArtifact{Ref: ServiceRef{Name: "redis"}, LocalDir: t.TempDir()}

	if _, err := l.LoadMigrationChain(art, 3, 1); err == nil {
		t.Fatal("downgrade from>to returned nil error")
	}
}

// TestLoadMigrationChain_StepStatesItsOwnPlace — a step document still carrying the
// retired header is refused at load, not silently accepted with the header ignored.
func TestLoadMigrationChain_StepStatesItsOwnPlace(t *testing.T) {
	dir := t.TempDir()
	writeStep(t, dir, 2, "widen_users", "from_version: 1\nto_version: 2\ntransform: []\n")

	l := NewServiceLoader(t.TempDir(), nil)
	art := &ServiceArtifact{Ref: ServiceRef{Name: "redis"}, LocalDir: dir}

	_, err := l.LoadMigrationChain(art, 1, 2)
	if err == nil {
		t.Fatal("a step stating its own place returned nil error")
	}
	if !strings.Contains(err.Error(), "migration_version_key") {
		t.Errorf("error %q does not carry the migration_version_key code", err)
	}
}

func TestLoadMigrationChain_InvalidStepDocument(t *testing.T) {
	dir := t.TempDir()
	writeStep(t, dir, 2, "widen_users", "transform:\n  - frobnicate: {}\n")
	l := NewServiceLoader(t.TempDir(), nil)
	art := &ServiceArtifact{Ref: ServiceRef{Name: "redis"}, LocalDir: dir}

	if _, err := l.LoadMigrationChain(art, 1, 2); err == nil {
		t.Fatal("invalid step document returned nil error")
	}
}
