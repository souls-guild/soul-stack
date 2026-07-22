package artifact

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeMigrationFile puts `migrations/<NNN>_to_<MMM>.yml` into the snapshot dir.
func writeMigrationFile(t *testing.T, dir string, from, to int, body string) {
	t.Helper()
	migDir := filepath.Join(dir, migrationsDir)
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatalf("mkdir migrations: %v", err)
	}
	name := fmt.Sprintf("%03d_to_%03d.yml", from, to)
	if err := os.WriteFile(filepath.Join(migDir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const migOneToTwo = `
from_version: 1
to_version: 2
transform:
  - set: { path: state.v, value: 2 }
`

const migTwoToThree = `
from_version: 2
to_version: 3
transform:
  - set: { path: state.v, value: 3 }
`

func TestLoadMigrationChain_AssembleFromFiles(t *testing.T) {
	dir := t.TempDir()
	writeMigrationFile(t, dir, 1, 2, migOneToTwo)
	writeMigrationFile(t, dir, 2, 3, migTwoToThree)

	l := NewServiceLoader(t.TempDir(), nil)
	art := &ServiceArtifact{Ref: ServiceRef{Name: "redis"}, LocalDir: dir}

	chain, err := l.LoadMigrationChain(art, 1, 3)
	if err != nil {
		t.Fatalf("LoadMigrationChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("len(chain) = %d, want 2", len(chain))
	}
	if chain[0].FromVersion != 1 || chain[0].ToVersion != 2 {
		t.Errorf("chain[0] = %d→%d, want 1→2", chain[0].FromVersion, chain[0].ToVersion)
	}
	if chain[1].FromVersion != 2 || chain[1].ToVersion != 3 {
		t.Errorf("chain[1] = %d→%d, want 2→3", chain[1].FromVersion, chain[1].ToVersion)
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
	// Only the first step; the second (002_to_003) is missing.
	writeMigrationFile(t, dir, 1, 2, migOneToTwo)

	l := NewServiceLoader(t.TempDir(), nil)
	art := &ServiceArtifact{Ref: ServiceRef{Name: "redis"}, LocalDir: dir}

	_, err := l.LoadMigrationChain(art, 1, 3)
	if !errors.Is(err, ErrMigrationChainBroken) {
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

func TestLoadMigrationChain_InvalidMigrationFile(t *testing.T) {
	dir := t.TempDir()
	writeMigrationFile(t, dir, 1, 2, "from_version: 1\nto_version: 5\n") // to != from+1
	l := NewServiceLoader(t.TempDir(), nil)
	art := &ServiceArtifact{Ref: ServiceRef{Name: "redis"}, LocalDir: dir}

	if _, err := l.LoadMigrationChain(art, 1, 2); err == nil {
		t.Fatal("invalid migration file returned nil error")
	}
}
