//go:build e2e_live

package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// L3b destiny fixture: materializes a standalone destiny from
// examples/destiny/<name> into a per-test file://-git repo under a git tag +
// sets keeper_settings[default_destiny_source].
//
// Why: service composition via apply:destiny (ADR-009) — e.g.
// examples/service/redis — references a standalone destiny by name; the
// keeper-side scenario resolver pulls the git URL from
// keeper_settings[default_destiny_source] with {name} substitution
// (ADR-029). The service fixture repo (git.go) doesn't contain these
// destinies — they live in separate examples/destiny/<name> directories.
// Without materialize+seed, apply:destiny fails with "default_destiny_source
// not set" or "ref does not resolve".
//
// keeper_settings — direct SQL upsert (the harness already talks to pgxpool,
// like AddSoulToCoven): there's no OpenAPI endpoint for this scalar
// (ADR-029). Call BEFORE registerExampleService — invalidation from POST
// /v1/services will pick up the already-written setting in the same
// snapshot without waiting on the Holder's 10s TTL poll.

const settingDefaultDestinySource = "default_destiny_source"

const destinyURLPlaceholder = "{name}"

// MaterializeDestinies materializes each destiny from
// examples/destiny/<name> into a separate file://-git repo
// ($TMP/destiny-repos/<name>) under the git tag ref, and sets
// keeper_settings[default_destiny_source] = file://$TMP/destiny-repos/{name}.
//
// ref is a git tag from service.yml::destiny[] (ADR-007: dependency version =
// git ref); the artifact resolver checks out the destiny by exactly that ref
// (NOT main). All three destinies of examples/service/redis are declared as
// ref:v1.0.0.
//
// Call BEFORE registerExampleService (NewStack does the registration itself
// — in L3b MaterializeDestinies is called by the test AFTER NewStack, so it
// relies on the TTL poll / a later invalidate; the first CreateIncarnation
// retries the transient "not registered", and default_destiny_source is
// resolved at render time of the create run — by then the Holder snapshot is
// already fresh).
func (s *Stack) MaterializeDestinies(t *testing.T, ref string, names ...string) {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("MaterializeDestinies: empty destiny name list")
	}

	destinyRoot := filepath.Join(s.tmpDir, "destiny-repos")
	for _, name := range names {
		relPath := filepath.Join("examples", "destiny", name)
		s.materializeRepoAt(t, filepath.Join(destinyRoot, name), relPath, ref)
	}

	tmpl := "file://" + filepath.Join(destinyRoot, destinyURLPlaceholder)
	s.SeedDefaultDestinySource(t, tmpl)
}

// holderRefreshGrace — wait window for the TTL re-read of the
// serviceregistry.Holder snapshot after the keeper_settings SQL write. =
// serviceregistry.DefaultRefreshInterval(10s) + buffer. In L3b
// registerExampleService already ran inside NewStack (its invalidate
// happened BEFORE the seed), and the harness has no access to the Redis
// pub/sub channel `service:invalidate` (the harness only talks to PG/Vault).
// So we rely on the TTL poll: wait until the Holder has definitely re-read
// the snapshot with the new default_destiny_source. Acceptable for L3b (slow
// tier, the test already runs for minutes anyway).
const holderRefreshGrace = 12 * time.Second

// SeedDefaultDestinySource writes keeper_settings[default_destiny_source]
// via a direct SQL upsert and blocks for holderRefreshGrace so the Holder is
// guaranteed to have picked up the value via the TTL poll BEFORE the first
// create render (apply:destiny resolves the template at render time).
// updated_by_aid = NULL (system-installed fixture).
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
	time.Sleep(holderRefreshGrace)
}

// materializeRepoAt — like materializeServiceRepo (git.go), but into an
// arbitrary repoDir and with a git tag ref: destiny repos live under
// destiny-repos/<name> so the {name} substitution in
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
		"commit", "-q", "-m", "e2e-live destiny snapshot from "+relativePath)
	runGit(t, repoDir, "-c", "tag.gpgsign=false",
		"tag", "-a", ref, "-m", "e2e-live destiny ref "+ref)
}
