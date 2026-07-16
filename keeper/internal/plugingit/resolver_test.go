package plugingit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// TestMain enables SOUL_STACK_ALLOW_FILE_REPOS for entire package test run:
// tests resolve local file:// repositories which are forbidden in prod by
// scheme-allowlist ([validateGitScheme]). Test for allowlist itself
// (TestResolveEntry_FileSchemeRequiresFlag) saves/restores flag
// locally via t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(allowFileReposEnv, "1")
	os.Exit(m.Run())
}

const validCloudManifest = `kind: cloud_driver
protocol_version: 1
namespace: cloud
name: hetzner
spec:
  profile_schema:
    type: object
`

// fixtureRepo — working wrapper over local git repository plugin source
// (populated with manifest + dist/<binary>), used by go-git resolver
// via file:// URL. No system git and no git-egress outward.
type fixtureRepo struct {
	t    *testing.T
	dir  string
	repo *git.Repository
}

// newFixtureRepo initializes empty git repository in temp directory.
// Default branch — `main` (`master` outside Soul Stack dictionary).
func newFixtureRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	return &fixtureRepo{t: t, dir: dir, repo: repo}
}

func (fr *fixtureRepo) fileURL() string { return "file://" + fr.dir }

// writePlugin puts manifest.yaml and dist/<binName> with given content
// in working tree. Empty manifest/binName skips respective file (for
// ErrManifestNotFound / ErrArtifactNotFound).
func (fr *fixtureRepo) writePlugin(manifest, binName string, binary []byte) {
	fr.t.Helper()
	if manifest != "" {
		fr.writeFile(sharedplugin.FileName, []byte(manifest))
	}
	if binName != "" {
		fr.writeFile(filepath.Join(artifactSubdir, binName), binary)
	}
}

func (fr *fixtureRepo) writeFile(rel string, content []byte) {
	fr.t.Helper()
	full := filepath.Join(fr.dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		fr.t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		fr.t.Fatalf("WriteFile: %v", err)
	}
}

// commit stages all changes and creates commit, returning its sha1.
func (fr *fixtureRepo) commit(msg string) string {
	fr.t.Helper()
	wt, err := fr.repo.Worktree()
	if err != nil {
		fr.t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		fr.t.Fatalf("AddGlob: %v", err)
	}
	h, err := wt.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	})
	if err != nil {
		fr.t.Fatalf("Commit: %v", err)
	}
	return h.String()
}

// tag creates lightweight tag at HEAD.
func (fr *fixtureRepo) tag(name string) {
	fr.t.Helper()
	head, err := fr.repo.Head()
	if err != nil {
		fr.t.Fatalf("Head: %v", err)
	}
	if _, err := fr.repo.CreateTag(name, head.Hash(), nil); err != nil {
		fr.t.Fatalf("CreateTag: %v", err)
	}
}

// taggedPlugin — common setup: commit with valid cloud plugin + tag v1.0.0.
// Returns sha1 of commit under tag.
func taggedPlugin(fr *fixtureRepo, binName string, binary []byte) string {
	fr.writePlugin(validCloudManifest, binName, binary)
	sha := fr.commit("plugin")
	fr.tag("v1.0.0")
	return sha
}

func newTestResolver(t *testing.T) (*Resolver, string) {
	t.Helper()
	// 0/0 size limits → defaults (256 MiB / 1024 MiB), happy-path doesn't hit.
	return newTestResolverWithLimits(t, 0, 0)
}

// newTestResolverWithLimits — resolver with explicit byte-limits artifact/clone
// (for size-limit tests, ADR-026(g)). 0 → default of respective limit.
func newTestResolverWithLimits(t *testing.T, maxArtifact, maxClone int64) (*Resolver, string) {
	t.Helper()
	base := t.TempDir()
	cacheRoot := filepath.Join(base, "cache")
	workRoot := filepath.Join(base, "work")
	return NewResolver(cacheRoot, workRoot, 0, maxArtifact, maxClone, nil), cacheRoot
}

// entryFor builds catalog entry on file:// source fr with ref.
func entryFor(fr *fixtureRepo, ref string) config.PluginCatalogEntry {
	return config.PluginCatalogEntry{Name: "hetzner", Source: fr.fileURL(), Ref: ref}
}

func TestResolveEntry_HappyPath(t *testing.T) {
	binary := []byte("fake-built-cloud-binary")
	fr := newFixtureRepo(t)
	wantSHA := taggedPlugin(fr, "soul-cloud-hetzner", binary)
	r, cacheRoot := newTestResolver(t)

	got, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if err != nil {
		t.Fatalf("ResolveEntry: %v", err)
	}
	if got.CommitSHA != wantSHA {
		t.Errorf("CommitSHA = %q, want %q", got.CommitSHA, wantSHA)
	}
	if got.Namespace != "cloud" || got.Name != "hetzner" {
		t.Errorf("ns/name = %q/%q, want cloud/hetzner", got.Namespace, got.Name)
	}
	if got.Ref != "v1.0.0" {
		t.Errorf("Ref = %q, want v1.0.0", got.Ref)
	}
	wantDigest := sha256.Sum256(binary)
	if got.BinarySHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Errorf("BinarySHA256 mismatch")
	}

	// Slot laid in R-nested layout + current → commit.
	wantSlot := filepath.Join(cacheRoot, "cloud-hetzner", wantSHA)
	if got.SlotDir != wantSlot {
		t.Errorf("SlotDir = %q, want %q", got.SlotDir, wantSlot)
	}
	if _, err := os.Stat(filepath.Join(wantSlot, sharedplugin.FileName)); err != nil {
		t.Errorf("manifest in slot missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wantSlot, "soul-cloud-hetzner")); err != nil {
		t.Errorf("binary in slot missing: %v", err)
	}
	link, err := os.Readlink(filepath.Join(cacheRoot, "cloud-hetzner", currentLink))
	if err != nil {
		t.Fatalf("readlink current: %v", err)
	}
	if link != wantSHA {
		t.Errorf("current → %q, want %q", link, wantSHA)
	}

	// Binary must be executable (0755), manifest — 0644.
	if st, _ := os.Stat(filepath.Join(wantSlot, "soul-cloud-hetzner")); st.Mode().Perm()&0o111 == 0 {
		t.Errorf("binary not executable: %v", st.Mode())
	}
}

// TestResolveEntry_BranchRef checks resolve of ref-branch (`main`), not just tag.
func TestResolveEntry_BranchRef(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.writePlugin(validCloudManifest, "soul-cloud-hetzner", []byte("bin"))
	wantSHA := fr.commit("plugin on main")
	r, _ := newTestResolver(t)

	got, err := r.ResolveEntry(context.Background(), entryFor(fr, "main"))
	if err != nil {
		t.Fatalf("ResolveEntry branch: %v", err)
	}
	if got.CommitSHA != wantSHA {
		t.Errorf("CommitSHA = %q, want HEAD %q", got.CommitSHA, wantSHA)
	}
}

func TestResolveEntry_ErrRefNotResolved(t *testing.T) {
	fr := newFixtureRepo(t)
	taggedPlugin(fr, "soul-cloud-hetzner", []byte("bin"))
	r, _ := newTestResolver(t)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "no-such-ref"))
	if !errors.Is(err, ErrRefNotResolved) {
		t.Fatalf("err = %v, want ErrRefNotResolved", err)
	}
}

func TestResolveEntry_ErrManifestNotFound(t *testing.T) {
	// Commit without manifest.yaml.
	fr := newFixtureRepo(t)
	fr.writeFile("README", []byte("no manifest here"))
	fr.commit("empty")
	fr.tag("v1.0.0")
	r, _ := newTestResolver(t)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("err = %v, want ErrManifestNotFound", err)
	}
}

func TestResolveEntry_ErrArtifactNotFound(t *testing.T) {
	// manifest exists, dist/<binary> missing.
	fr := newFixtureRepo(t)
	fr.writePlugin(validCloudManifest, "", nil)
	fr.commit("manifest only")
	fr.tag("v1.0.0")
	r, _ := newTestResolver(t)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("err = %v, want ErrArtifactNotFound", err)
	}
}

func TestResolveEntry_ErrSourceUnavailable(t *testing.T) {
	// Nonexistent local repository → clone fails.
	r, _ := newTestResolver(t)
	e := config.PluginCatalogEntry{
		Name:   "hetzner",
		Source: "file://" + filepath.Join(t.TempDir(), "does-not-exist"),
		Ref:    "v1.0.0",
	}
	_, err := r.ResolveEntry(context.Background(), e)
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
}

// TestResolveEntry_FileSchemeRequiresFlag fixes scheme-allowlist: file://
// without env flag rejected ErrSourceUnavailable before git operations.
func TestResolveEntry_FileSchemeRequiresFlag(t *testing.T) {
	fr := newFixtureRepo(t)
	taggedPlugin(fr, "soul-cloud-hetzner", []byte("bin"))
	r, _ := newTestResolver(t)

	t.Setenv(allowFileReposEnv, "")
	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("file:// without flag: err = %v, want ErrSourceUnavailable", err)
	}
}

// TestResolveEntry_UnsupportedScheme fixes http:// (unencrypted) and
// other schemes outside allowlist rejected without git-egress.
func TestResolveEntry_UnsupportedScheme(t *testing.T) {
	r, _ := newTestResolver(t)
	e := config.PluginCatalogEntry{Name: "hetzner", Source: "http://example.com/repo.git", Ref: "v1.0.0"}
	_, err := r.ResolveEntry(context.Background(), e)
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("http://: err = %v, want ErrSourceUnavailable", err)
	}
}

func TestResolveEntry_Idempotent(t *testing.T) {
	binary := []byte("idempotent-binary")
	fr := newFixtureRepo(t)
	sha := taggedPlugin(fr, "soul-cloud-hetzner", binary)
	r, cacheRoot := newTestResolver(t)

	first, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if err != nil {
		t.Fatalf("ResolveEntry #1: %v", err)
	}
	slotPath := filepath.Join(cacheRoot, "cloud-hetzner", sha, "soul-cloud-hetzner")
	st1, err := os.Stat(slotPath)
	if err != nil {
		t.Fatalf("stat slot binary: %v", err)
	}

	second, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if err != nil {
		t.Fatalf("ResolveEntry #2: %v", err)
	}
	st2, err := os.Stat(slotPath)
	if err != nil {
		t.Fatalf("stat slot binary #2: %v", err)
	}
	if first.BinarySHA256 != second.BinarySHA256 {
		t.Errorf("digest unstable between runs")
	}
	// Immutable slot not recreated — binary mtime unchanged.
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Errorf("slot recreated on re-resolve of same commit (mtime %v → %v)", st1.ModTime(), st2.ModTime())
	}
}

func TestResolveEntry_CurrentSymlinkAtomicSwap(t *testing.T) {
	// Resolve tag v1.0.0, then advance main branch with new plugin and
	// resolve main: current must switch to second commit atomically, both
	// slots — remain in cache.
	fr := newFixtureRepo(t)
	shaA := taggedPlugin(fr, "soul-cloud-hetzner", []byte("bin-a"))
	r, cacheRoot := newTestResolver(t)

	if _, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0")); err != nil {
		t.Fatalf("resolve A (tag): %v", err)
	}

	// Advance main with new plugin commit.
	fr.writePlugin(validCloudManifest, "soul-cloud-hetzner", []byte("bin-b"))
	shaB := fr.commit("advance main")
	if shaB == shaA {
		t.Fatal("commit B matched A — setup broken")
	}

	if _, err := r.ResolveEntry(context.Background(), entryFor(fr, "main")); err != nil {
		t.Fatalf("resolve B (branch): %v", err)
	}

	link, err := os.Readlink(filepath.Join(cacheRoot, "cloud-hetzner", currentLink))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if link != shaB {
		t.Errorf("current → %q, want %q (latest resolve)", link, shaB)
	}
	// Both commit slots in place (commit_sha immutable, old not deleted).
	for _, c := range []string{shaA, shaB} {
		if _, err := os.Stat(filepath.Join(cacheRoot, "cloud-hetzner", c, "soul-cloud-hetzner")); err != nil {
			t.Errorf("slot %s missing: %v", c, err)
		}
	}
}

func TestResolveCatalog_CollectsWarningsAndDoesNotFail(t *testing.T) {
	// Good entries: cloud + soul_module; broken (no manifest in checkout).
	okRepo := newFixtureRepo(t)
	taggedPlugin(okRepo, "soul-cloud-hetzner", []byte("bin"))

	modRepo := newFixtureRepo(t)
	modRepo.writePlugin(`kind: soul_module
protocol_version: 1
namespace: community
name: redis
spec: { states: { pinged: {} } }
`, "soul-mod-redis", []byte("modbin"))
	modRepo.commit("soul module plugin")
	modRepo.tag("v1.0.0")

	brokenRepo := newFixtureRepo(t)
	brokenRepo.writeFile("README", []byte("no manifest"))
	brokenRepo.commit("empty")
	brokenRepo.tag("v9.9.9")

	plugins := &config.KeeperPlugins{
		CloudDrivers: []config.PluginCatalogEntry{
			{Name: "hetzner", Source: okRepo.fileURL(), Ref: "v1.0.0"},
		},
		SSHProviders: []config.PluginCatalogEntry{
			{Name: "broken", Source: brokenRepo.fileURL(), Ref: "v9.9.9"},
		},
		SoulModules: []config.PluginCatalogEntry{
			{Name: "redis", Source: modRepo.fileURL(), Ref: "v1.0.0"},
		},
	}
	r, _ := newTestResolver(t)

	slots, warns, err := r.ResolveCatalog(context.Background(), plugins)
	if err != nil {
		t.Fatalf("ResolveCatalog returned fatal: %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("slots = %d, want 2 (hetzner + community.redis): %v", len(slots), slots)
	}
	byName := map[string]ResolvedSlot{}
	for _, s := range slots {
		byName[s.Name] = s
	}
	if _, ok := byName["hetzner"]; !ok {
		t.Errorf("no slot hetzner: %v", byName)
	}
	if mod, ok := byName["redis"]; !ok || mod.Namespace != "community" {
		t.Errorf("no slot community.redis: %v", byName)
	}
	if len(warns) != 1 {
		t.Fatalf("warns = %d, want 1 (broken entry): %v", len(warns), warns)
	}
}

// TestResolveEntry_ErrArtifactTooLarge: binary larger than max_artifact_size →
// ErrArtifactTooLarge, slot NOT created (fail-closed, ADR-026(g)).
func TestResolveEntry_ErrArtifactTooLarge(t *testing.T) {
	fr := newFixtureRepo(t)
	oversized := make([]byte, 4096) // > limit 1024 bytes
	sha := taggedPlugin(fr, "soul-cloud-hetzner", oversized)
	r, cacheRoot := newTestResolverWithLimits(t, 1024, 0)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("err = %v, want ErrArtifactTooLarge", err)
	}
	// Fail-closed: slot at commit_sha not materialized, current not created.
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "cloud-hetzner", sha)); !os.IsNotExist(statErr) {
		t.Errorf("slot created despite limit exceeded (stat err=%v)", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(cacheRoot, "cloud-hetzner", currentLink)); !os.IsNotExist(statErr) {
		t.Errorf("current symlink created despite limit exceeded (stat err=%v)", statErr)
	}
}

// TestResolveEntry_ErrCloneTooLarge: total working tree larger than
// max_clone_size → ErrCloneTooLarge + cleanup workdir (fail-closed, ADR-026(g)).
func TestResolveEntry_ErrCloneTooLarge(t *testing.T) {
	fr := newFixtureRepo(t)
	// Tree bloated with junk file besides valid plugin.
	fr.writePlugin(validCloudManifest, "soul-cloud-hetzner", []byte("bin"))
	fr.writeFile("bloat.dat", make([]byte, 8192))
	fr.commit("bloated plugin")
	fr.tag("v1.0.0")
	// Clone limit below tree size; artifact limit default (miss).
	r, cacheRoot := newTestResolverWithLimits(t, 0, 2048)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrCloneTooLarge) {
		t.Fatalf("err = %v, want ErrCloneTooLarge", err)
	}
	// Cleanup: workdir deleted (workdir name = sanitizeSegment(name) under workRoot).
	workdir := filepath.Join(filepath.Dir(cacheRoot), "work", "hetzner")
	if _, statErr := os.Stat(workdir); !os.IsNotExist(statErr) {
		t.Errorf("workdir not cleaned after ErrCloneTooLarge (stat err=%v)", statErr)
	}
	// Slot not created.
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "cloud-hetzner")); !os.IsNotExist(statErr) {
		t.Errorf("slot created despite ErrCloneTooLarge (stat err=%v)", statErr)
	}
}

// TestResolveEntry_WithinSizeLimits: normal size with given (not
// default) limits resolves without error — happy-path not broken by hardening.
func TestResolveEntry_WithinSizeLimits(t *testing.T) {
	binary := []byte("small-binary")
	fr := newFixtureRepo(t)
	wantSHA := taggedPlugin(fr, "soul-cloud-hetzner", binary)
	// Limits with margin above real binary and tree size.
	r, cacheRoot := newTestResolverWithLimits(t, 1<<20, 16<<20)

	got, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if err != nil {
		t.Fatalf("ResolveEntry within limits: %v", err)
	}
	if got.CommitSHA != wantSHA {
		t.Errorf("CommitSHA = %q, want %q", got.CommitSHA, wantSHA)
	}
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "cloud-hetzner", wantSHA, "soul-cloud-hetzner")); statErr != nil {
		t.Errorf("slot not created with size within limit: %v", statErr)
	}
}

func TestResolveCatalog_NilPlugins(t *testing.T) {
	r, _ := newTestResolver(t)
	slots, warns, err := r.ResolveCatalog(context.Background(), nil)
	if err != nil || slots != nil || warns != nil {
		t.Errorf("nil plugins: expected empty result, got slots=%v warns=%v err=%v", slots, warns, err)
	}
}
