//go:build e2e_live

package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// L3b destiny fixture: materializes a standalone destiny from
// examples/destiny/<name> — or, for a destiny that left this tree, from its pinned
// commit (scripts/destiny-source.sh, NIM-1062) — into a per-test file://-git repo
// under a git tag + sets keeper_settings[default_destiny_source].
//
// Why: service composition via apply:destiny (ADR-009) — e.g.
// examples/service/dragonfly — references a standalone destiny by name; the
// keeper-side scenario resolver pulls the git URL from
// keeper_settings[default_destiny_source] with {name} substitution
// (ADR-029). The service fixture repo (git.go) doesn't contain these
// destinies — they live in separate examples/destiny/<name> directories.
// Without materialize+seed, apply:destiny fails with "default_destiny_source
// not set" or "ref does not resolve".
//
// keeper_settings — direct SQL upsert (the harness already talks to pgxpool,
// like AddMember): there's no OpenAPI endpoint for this scalar
// (ADR-029). Call BEFORE registerExampleService — invalidation from POST
// /v1/services will pick up the already-written setting in the same
// snapshot without waiting on the Holder's 10s TTL poll.

const settingDefaultDestinySource = "default_destiny_source"

const destinyURLPlaceholder = "{name}"

// MaterializeDestinies materializes each destiny from
// [destinySourceDir] into a separate file://-git repo
// ($TMP/destiny-repos/<name>) under the git tag ref, and sets
// keeper_settings[default_destiny_source] = file://$TMP/destiny-repos/{name}.
//
// ref is a git tag from service.yml::destiny[] (ADR-007: dependency version =
// git ref); the artifact resolver checks out the destiny by exactly that ref
// (NOT main). Every corpus service declares its destinies as ref:v1.0.0.
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
		s.materializeRepoAt(t, filepath.Join(destinyRoot, name), destinySourceDir(t, name, ref), name, ref)
	}

	tmpl := "file://" + filepath.Join(destinyRoot, destinyURLPlaceholder)
	s.SeedDefaultDestinySource(t, tmpl)
}

// destinySourceDir — the tree a destiny is snapshotted from. A name in the catalog of
// scripts/destiny-source.sh is taken at its pin and nowhere else: the catalog is asked
// first so that a copy reappearing under examples/destiny cannot quietly become the
// subject again. The pin stands for one tag, and a test asking for another ref would be
// proving a tree its service does not declare.
func destinySourceDir(t *testing.T, name, ref string) string {
	t.Helper()
	if !slices.Contains(destinySourceScript(t, "aliases"), name) {
		return filepath.Join(repoRoot(t), "examples", "destiny", name)
	}
	if tag := destinySourceScript(t, "tag", name); len(tag) != 1 || tag[0] != ref {
		t.Fatalf("destiny %s is pinned at %v in scripts/destiny-source.sh, but the test asks for ref %s", name, tag, ref)
	}
	dir := destinySourceScript(t, "dir", name)
	if len(dir) != 1 {
		t.Fatalf("scripts/destiny-source.sh dir %s printed %q, want one path", name, dir)
	}
	return dir[0]
}

// destinySourceScript runs scripts/destiny-source.sh and returns its stdout lines. Lines,
// not fields: a cache or override path may contain a space.
// Outside the bring-up region at every call site, like redisPluginSourceDir: a pin that
// cannot be materialised is a fact about this repository and its cache, not the stand.
func destinySourceScript(t *testing.T, args ...string) []string {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts", "destiny-source.sh"), args...)
	cmd.Dir = root
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("scripts/destiny-source.sh %s: %v\n%s\n"+
			"\t`make destiny-sources` fills the cache; SOUL_STACK_DESTINY_OFFLINE=1 with a cold cache fails here by design.",
			strings.Join(args, " "), err, stderr.String())
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
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
//
// The commit message names the destiny and the ref, never srcDir: a pinned source lives
// in a per-machine cache, and its path in the message would make the snapshot SHA differ
// from one machine to the next.
func (s *Stack) materializeRepoAt(t *testing.T, repoDir, srcDir, name, ref string) {
	t.Helper()
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
		"commit", "-q", "-m", "e2e-live destiny snapshot of "+name+" at "+ref)
	runGit(t, repoDir, "-c", "tag.gpgsign=false",
		"tag", "-a", ref, "-m", "e2e-live destiny ref "+ref)
}
