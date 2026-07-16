//go:build e2e

package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Destiny fixture: materializes a standalone destiny from
// examples/destiny/<name> into a per-test file:// git repo + sets
// keeper_settings[default_destiny_source].
//
// Why separate from RegisterService (git.go): service composition via
// apply:destiny (ADR-009) — e.g. examples/service/redis — references a
// standalone destiny by name (`apply: { destiny: redis-single }`). The
// keeper-side scenario resolver pulls the git URL for such a destiny from
// keeper_settings[default_destiny_source] with a {name} substitution
// (ADR-029, keeper/internal/scenario/destiny.go::resolveGitURL). The service
// fixture repo does NOT contain these destinies — they live in separate
// directories under examples/destiny/. Without materialize+seed the resolve
// fails with "default_destiny_source not set".
//
// keeper_settings — a direct SQL upsert (the harness already works via
// pgxpool, as in AddSoulToCoven): there is no OpenAPI endpoint for this
// scalar (managed only through keeper_settings, ADR-029). The Holder rereads
// the snapshot via TTL poll (10s) OR pub/sub invalidation; to avoid waiting
// on the poll, the harness calls SeedDefaultDestinySource BEFORE
// RegisterService — the invalidate from POST /v1/services will pick up the
// already-written setting with the same snapshot (see holder.go).

// settingDefaultDestinySource — the scalar key in keeper_settings. Duplicates
// serviceregistry.SettingDefaultDestinySource as a literal: tests/e2e is a
// separate go module without a dependency on keeper/internal/* (Go internal
// rules, ADR-039).
const settingDefaultDestinySource = "default_destiny_source"

// destinyURLPlaceholder — the {name} marker in the default_destiny_source
// template, replaced with the destiny name during keeper-side resolution.
const destinyURLPlaceholder = "{name}"

// MaterializeDestinies materializes each destiny from
// examples/destiny/<name> into a separate file:// git repo
// ($TMP/destiny-repos/<name>) under a git tag ref and sets
// keeper_settings[default_destiny_source] = file://$TMP/destiny-repos/{name}.
//
// ref — the git tag under which each destiny is declared in
// service.yml::destiny[] (ADR-007: dependency version = git ref). For
// examples/service/redis all three destinies are declared ref:v1.0.0 — the
// artifact resolver takes the ref from the service.yml entry and checks out
// exactly that (NOT main). Without the tag the resolve fails with "ref <ref>
// does not resolve: reference not found".
//
// Call BEFORE RegisterService: the invalidate from registering the service
// will pick up keeper_settings[default_destiny_source] in the Holder without
// waiting for the 10s TTL poll (see the doc above). Destiny names are exactly
// those declared in service.yml::destiny[] and used in apply:destiny.
func (s *Stack) MaterializeDestinies(t *testing.T, ref string, names ...string) {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("MaterializeDestinies: empty destiny name list")
	}

	destinyRoot := filepath.Join(s.tmpDir, "destiny-repos")
	for _, name := range names {
		relPath := filepath.Join("examples", "destiny", name)
		// materializeRepoAt: init a working-tree repo + a deterministic commit +
		// the ref tag. The repo directory is destiny-repos/<name> so that the
		// {name} substitution in the template resolves to this path.
		s.materializeRepoAt(t, filepath.Join(destinyRoot, name), relPath, ref)
	}

	tmpl := "file://" + filepath.Join(destinyRoot, destinyURLPlaceholder)
	s.SeedDefaultDestinySource(t, tmpl)
}

// SeedDefaultDestinySource writes keeper_settings[default_destiny_source] via
// a direct SQL upsert. updated_by_aid = NULL (a system-level fixture setting;
// the FK allows NULL, same as for the first Archon). The Holder will pick it
// up on the next snapshot reread (TTL poll or invalidate).
func (s *Stack) SeedDefaultDestinySource(t *testing.T, template string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.db.Exec(ctx, `
		INSERT INTO keeper_settings (key, value, updated_by_aid, updated_at)
		VALUES ($1, $2, NULL, NOW())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = NOW()
	`, settingDefaultDestinySource, template)
	if err != nil {
		t.Fatalf("SeedDefaultDestinySource(%q): %v", template, err)
	}
}

// materializeRepoAt — like materializeServiceRepo (git.go), but into an
// arbitrary repoDir (not $TMP/repos/<name>) and with a git tag ref: destiny
// repos live under destiny-repos/<name> so that the {name} substitution in
// default_destiny_source points at them, and are resolved by the ref from
// service.yml::destiny[] (not main).
func (s *Stack) materializeRepoAt(t *testing.T, repoDir, relativePath, ref string) {
	t.Helper()
	srcDir := filepath.Join(repoRoot(t), relativePath)
	if _, err := os.Stat(srcDir); err != nil {
		t.Fatalf("materializeRepoAt: source %s: %v", srcDir, err)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("materializeRepoAt: mkdir %s: %v", repoDir, err)
	}
	runGit(t, "", "init", "-q", "-b", "main", repoDir)
	copyTree(t, srcDir, repoDir)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "e2e destiny snapshot from "+relativePath)
	// An annotated tag ref (-m is required): the artifact resolver checks out
	// the destiny by this ref. gpgsign=false — fixture hermeticity (see the
	// commit above).
	runGit(t, repoDir, "-c", "tag.gpgsign=false",
		"tag", "-a", ref, "-m", "e2e destiny ref "+ref)
}
