package plugingit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginsource"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestMain enables SOUL_STACK_ALLOW_FILE_REPOS for the whole package run: these tests
// resolve local file:// repositories, which the scheme-allowlist ([validateGitScheme])
// forbids in production. The test for the allowlist itself
// (TestResolveEntry_FileSchemeRequiresFlag) saves/restores the flag locally via
// t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(allowFileReposEnv, "1")
	os.Exit(m.Run())
}

// sshDoc / moduleDoc are the two documents the fixtures stamp. Neither carries a
// name, a namespace or a publisher — there is nowhere in the format left to put one.
func sshDoc() schema.Document {
	return schema.Document{
		Kind:            schema.KindSSHProvider,
		ProtocolVersion: 1,
		ProviderKind:    "static_key",
	}
}

func moduleDoc(names ...string) schema.Document {
	mods := make([]schema.Module, 0, len(names))
	for _, n := range names {
		mods = append(mods, schema.Module{
			Name:   n,
			States: map[string]schema.State{"present": {Description: "the resource exists"}},
		})
	}
	return schema.Document{Kind: schema.KindSoulModule, ProtocolVersion: 1, Modules: mods}
}

// stamped returns body with doc appended as a schema trailer — the bytes
// `soul-mod stamp` would leave in `dist/`.
func stamped(t *testing.T, doc schema.Document, body []byte) []byte {
	t.Helper()
	payload, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return schema.AppendTrailer(body, payload)
}

// fixtureRepo — a working wrapper over a local git repository acting as a plugin
// source (populated with `dist/`), reached by the go-git resolver through a file://
// URL. No system git and no git-egress outward.
type fixtureRepo struct {
	t    *testing.T
	dir  string
	repo *git.Repository
}

// newFixtureRepo initializes an empty git repository in a temp directory. Default
// branch — `main` (`master` is outside the Soul Stack dictionary).
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

// writeArtifact puts an EXECUTABLE `dist/<binName>` carrying body plus doc's trailer.
// The executable bit matters: `dist/` also holds the published schema.json, and what
// separates the artifact from it is exactly that bit.
func (fr *fixtureRepo) writeArtifact(binName string, doc schema.Document, body []byte) {
	fr.t.Helper()
	fr.writeFileMode(filepath.Join(artifactSubdir, binName), stamped(fr.t, doc, body), 0o755)
}

// writeUnstampedArtifact puts an executable with NO trailer — an artifact whose build
// forgot `soul-mod stamp`.
func (fr *fixtureRepo) writeUnstampedArtifact(binName string, body []byte) {
	fr.t.Helper()
	fr.writeFileMode(filepath.Join(artifactSubdir, binName), body, 0o755)
}

func (fr *fixtureRepo) writeFile(rel string, content []byte) {
	fr.t.Helper()
	fr.writeFileMode(rel, content, 0o644)
}

func (fr *fixtureRepo) writeFileMode(rel string, content []byte, mode os.FileMode) {
	fr.t.Helper()
	full := filepath.Join(fr.dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		fr.t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, content, mode); err != nil {
		fr.t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(full, mode); err != nil {
		fr.t.Fatalf("Chmod: %v", err)
	}
}

// commit stages all changes and creates a commit, returning its sha1.
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

// tag creates a lightweight tag at HEAD.
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

// taggedPlugin — the common setup: a commit holding one stamped ssh_provider artifact
// in `dist/`, tagged v1.0.0. Returns the sha1 under the tag.
func taggedPlugin(t *testing.T, fr *fixtureRepo, binName string, body []byte) string {
	t.Helper()
	fr.writeArtifact(binName, sshDoc(), body)
	sha := fr.commit("plugin")
	fr.tag("v1.0.0")
	return sha
}

func newTestResolver(t *testing.T) (*Resolver, string) {
	t.Helper()
	// 0/0 size limits → defaults (256 MiB / 1024 MiB); the happy path never hits them.
	return newTestResolverWithLimits(t, 0, 0)
}

// newTestResolverWithLimits — a resolver with explicit artifact/clone byte-limits (for
// the size-limit tests, ADR-026(g)). 0 → the respective default.
func newTestResolverWithLimits(t *testing.T, maxArtifact, maxClone int64) (*Resolver, string) {
	t.Helper()
	base := t.TempDir()
	cacheRoot := filepath.Join(base, "cache")
	workRoot := filepath.Join(base, "work")
	return NewResolver(cacheRoot, workRoot, 0, maxArtifact, maxClone, nil), cacheRoot
}

// entryFor builds a catalog entry over fr at ref, registered under alias `hetzner`.
func entryFor(fr *fixtureRepo, ref string) config.PluginCatalogEntry {
	return config.PluginCatalogEntry{Name: "hetzner", Source: fr.fileURL(), Ref: ref}
}

func TestResolveEntry_HappyPath(t *testing.T) {
	body := []byte("fake-built-cloud-binary")
	fr := newFixtureRepo(t)
	wantSHA := taggedPlugin(t, fr, "soul-cloud-hetzner", body)
	r, cacheRoot := newTestResolver(t)

	got, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if err != nil {
		t.Fatalf("ResolveEntry: %v", err)
	}
	if got.CommitSHA != wantSHA {
		t.Errorf("CommitSHA = %q, want %q", got.CommitSHA, wantSHA)
	}
	// The identity comes from the CATALOG, not from the checkout: alias from `name:`,
	// source from `source:`. The repository has no say in either.
	if got.Alias != "hetzner" {
		t.Errorf("Alias = %q, want hetzner (the catalog's registration)", got.Alias)
	}
	if got.Source != fr.fileURL() {
		t.Errorf("Source = %q, want %q", got.Source, fr.fileURL())
	}
	if got.Ref != "v1.0.0" {
		t.Errorf("Ref = %q, want v1.0.0", got.Ref)
	}
	if got.Doc == nil || got.Doc.Kind != schema.KindSSHProvider {
		t.Errorf("Doc = %+v, want a parsed ssh_provider document", got.Doc)
	}
	// SchemaBytes must be the trailer payload byte for byte: they are what gets hashed
	// and signed at allow.
	wantSchema, err := schema.Marshal(sshDoc())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got.SchemaBytes) != string(wantSchema) {
		t.Errorf("SchemaBytes are not the canonical trailer payload")
	}

	// The slot is laid out R-nested under the ALIAS + current → commit.
	wantSlot := filepath.Join(cacheRoot, "hetzner", wantSHA)
	if got.SlotDir != wantSlot {
		t.Errorf("SlotDir = %q, want %q", got.SlotDir, wantSlot)
	}
	// The artifact is named by the alias — it has no name of its own to keep.
	slotBin := filepath.Join(wantSlot, "hetzner")
	if got.BinaryPath != slotBin {
		t.Errorf("BinaryPath = %q, want %q", got.BinaryPath, slotBin)
	}
	raw, err := os.ReadFile(slotBin)
	if err != nil {
		t.Fatalf("artifact in slot missing: %v", err)
	}
	wantDigest := sha256.Sum256(raw)
	if got.BinarySHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Errorf("BinarySHA256 mismatch")
	}
	link, err := os.Readlink(filepath.Join(cacheRoot, "hetzner", currentLink))
	if err != nil {
		t.Fatalf("readlink current: %v", err)
	}
	if link != wantSHA {
		t.Errorf("current → %q, want %q", link, wantSHA)
	}
	if st, _ := os.Stat(slotBin); st.Mode().Perm()&0o111 == 0 {
		t.Errorf("artifact not executable: %v", st.Mode())
	}

	// The slot holds the artifact and NOTHING else: no manifest, no schema.json. A
	// second copy of the schema beside the bytes could only ever disagree with the
	// trailer that gets signed.
	entries, err := os.ReadDir(wantSlot)
	if err != nil {
		t.Fatalf("read slot: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "hetzner" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("slot holds %v, want only the artifact", names)
	}
}

// TestResolveEntry_ArtifactNameIsIrrelevant pins the removal of the filename
// convention: whatever `dist/` calls its single executable, it is the artifact.
func TestResolveEntry_ArtifactNameIsIrrelevant(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.writeArtifact("anything-at-all", sshDoc(), []byte("bin"))
	fr.commit("plugin")
	fr.tag("v1.0.0")
	r, _ := newTestResolver(t)

	got, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if err != nil {
		t.Fatalf("ResolveEntry: %v", err)
	}
	if filepath.Base(got.BinaryPath) != "hetzner" {
		t.Errorf("slot artifact = %q, want it renamed to the alias", got.BinaryPath)
	}
}

// TestResolveEntry_DistWithSchemaFileIsNotAmbiguous — the published `schema.json` sits
// beside the artifact in a real build and must not make `dist/` ambiguous: it is not
// executable.
func TestResolveEntry_DistWithSchemaFileIsNotAmbiguous(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.writeArtifact("soul-cloud-hetzner", sshDoc(), []byte("bin"))
	payload, err := schema.Marshal(sshDoc())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fr.writeFile(filepath.Join(artifactSubdir, schema.SchemaFileName), payload)
	fr.commit("plugin with published schema")
	fr.tag("v1.0.0")
	r, _ := newTestResolver(t)

	if _, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0")); err != nil {
		t.Fatalf("dist/ with schema.json beside the artifact must resolve: %v", err)
	}
}

// TestResolveEntry_BranchRef checks resolving a branch ref (`main`), not just a tag.
func TestResolveEntry_BranchRef(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.writeArtifact("soul-cloud-hetzner", sshDoc(), []byte("bin"))
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
	taggedPlugin(t, fr, "soul-cloud-hetzner", []byte("bin"))
	r, _ := newTestResolver(t)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "no-such-ref"))
	if !errors.Is(err, ErrRefNotResolved) {
		t.Fatalf("err = %v, want ErrRefNotResolved", err)
	}
}

// TestResolveEntry_ErrArtifactNotFound_Empty — `dist/` with no executable. Not "nothing
// to fetch": a catalog entry that resolves to no code is a broken entry.
func TestResolveEntry_ErrArtifactNotFound_Empty(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.writeFile("README", []byte("built nothing"))
	fr.commit("no dist")
	fr.tag("v1.0.0")
	r, _ := newTestResolver(t)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("err = %v, want ErrArtifactNotFound", err)
	}
}

// TestResolveEntry_ErrArtifactNotFound_TwoExecutables is the guard that replaces the
// old `dist/<binary-name>` lookup. With no name to look up, taking the first match
// would let a repository's directory listing order decide which bytes an operator ends
// up approving. Two executables must stop the entry.
func TestResolveEntry_ErrArtifactNotFound_TwoExecutables(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.writeArtifact("soul-cloud-hetzner", sshDoc(), []byte("bin-a"))
	fr.writeArtifact("soul-cloud-hetzner-debug", sshDoc(), []byte("bin-b"))
	fr.commit("two artifacts")
	fr.tag("v1.0.0")
	r, cacheRoot := newTestResolver(t)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("err = %v, want ErrArtifactNotFound", err)
	}
	if !strings.Contains(err.Error(), "2 executables") {
		t.Errorf("error should say what was ambiguous: %v", err)
	}
	// Fail-closed: nothing was cached.
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "hetzner")); !os.IsNotExist(statErr) {
		t.Errorf("slot created despite an ambiguous dist/ (stat err=%v)", statErr)
	}
}

// TestResolveEntry_ErrSchemaUnreadable_NoTrailer — an unstamped artifact has no
// disclosure to approve, so it never reaches the cache.
func TestResolveEntry_ErrSchemaUnreadable_NoTrailer(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.writeUnstampedArtifact("soul-cloud-hetzner", []byte("bin without a trailer"))
	fr.commit("unstamped")
	fr.tag("v1.0.0")
	r, cacheRoot := newTestResolver(t)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrSchemaUnreadable) {
		t.Fatalf("err = %v, want ErrSchemaUnreadable", err)
	}
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "hetzner")); !os.IsNotExist(statErr) {
		t.Errorf("slot created for an unstamped artifact (stat err=%v)", statErr)
	}
}

// TestResolveEntry_ErrSchemaUnreadable_CorruptTrailer — a truncated trailer fails
// closed for the same reason a missing one does.
func TestResolveEntry_ErrSchemaUnreadable_CorruptTrailer(t *testing.T) {
	fr := newFixtureRepo(t)
	full := stamped(t, sshDoc(), []byte("bin"))
	fr.writeUnstampedArtifact("soul-cloud-hetzner", full[:len(full)-1])
	fr.commit("corrupt trailer")
	fr.tag("v1.0.0")
	r, _ := newTestResolver(t)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrSchemaUnreadable) {
		t.Fatalf("err = %v, want ErrSchemaUnreadable", err)
	}
}

// TestResolveEntry_ErrSchemaUnreadable_InvalidDocument — a trailer that parses but does
// not validate is refused too.
func TestResolveEntry_ErrSchemaUnreadable_InvalidDocument(t *testing.T) {
	fr := newFixtureRepo(t)
	// kind=soul_module with no modules: the validator rejects it.
	bad := schema.Document{Kind: schema.KindSoulModule, ProtocolVersion: 1}
	fr.writeArtifact("soul-mod-redis", bad, []byte("bin"))
	fr.commit("invalid document")
	fr.tag("v1.0.0")
	r, _ := newTestResolver(t)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrSchemaUnreadable) {
		t.Fatalf("err = %v, want ErrSchemaUnreadable", err)
	}
}

// TestResolveEntry_ReservedAliasRejected — an operator cannot register a plugin as
// `core`: the address `core.file.present` must keep meaning the engine. Checked BEFORE
// any git egress, so a reserved entry never reaches the network.
func TestResolveEntry_ReservedAliasRejected(t *testing.T) {
	r, cacheRoot := newTestResolver(t)
	for _, alias := range []string{"core", "keeper", "soul", "destiny", "soul-stack", "Core"} {
		e := config.PluginCatalogEntry{
			Name:   alias,
			Source: "https://example.com/never-reached.git",
			Ref:    "v1.0.0",
		}
		_, err := r.ResolveEntry(context.Background(), e)
		if !errors.Is(err, ErrAliasInvalid) {
			t.Errorf("alias %q: err = %v, want ErrAliasInvalid", alias, err)
		}
	}
	if _, statErr := os.Stat(cacheRoot); !os.IsNotExist(statErr) {
		t.Errorf("a reserved alias must not touch the cache (stat err=%v)", statErr)
	}
}

// TestResolveEntry_MalformedAliasRejected — the alias names a directory and an address
// level, so path-shaped and uppercase names are refused before anything is created.
func TestResolveEntry_MalformedAliasRejected(t *testing.T) {
	r, _ := newTestResolver(t)
	for _, alias := range []string{"", "../escape", "with/slash", "Redis", "redis.acl", "9lives"} {
		e := config.PluginCatalogEntry{
			Name:   alias,
			Source: "https://example.com/never-reached.git",
			Ref:    "v1.0.0",
		}
		if _, err := r.ResolveEntry(context.Background(), e); !errors.Is(err, ErrAliasInvalid) {
			t.Errorf("alias %q: err = %v, want ErrAliasInvalid", alias, err)
		}
	}
}

func TestResolveEntry_ErrSourceUnavailable(t *testing.T) {
	// A nonexistent local repository → clone fails.
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

// TestResolveEntry_FileSchemeRequiresFlag pins the scheme-allowlist: file:// without
// the env flag is rejected as ErrSourceUnavailable before any git operation.
func TestResolveEntry_FileSchemeRequiresFlag(t *testing.T) {
	fr := newFixtureRepo(t)
	taggedPlugin(t, fr, "soul-cloud-hetzner", []byte("bin"))
	r, _ := newTestResolver(t)

	t.Setenv(allowFileReposEnv, "")
	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("file:// without flag: err = %v, want ErrSourceUnavailable", err)
	}
}

// TestResolveEntry_UnsupportedScheme pins that http:// (unencrypted) and other schemes
// outside the allowlist are rejected without git-egress.
func TestResolveEntry_UnsupportedScheme(t *testing.T) {
	r, _ := newTestResolver(t)
	e := config.PluginCatalogEntry{Name: "hetzner", Source: "http://example.com/repo.git", Ref: "v1.0.0"}
	_, err := r.ResolveEntry(context.Background(), e)
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("http://: err = %v, want ErrSourceUnavailable", err)
	}
}

func TestResolveEntry_Idempotent(t *testing.T) {
	fr := newFixtureRepo(t)
	sha := taggedPlugin(t, fr, "soul-cloud-hetzner", []byte("idempotent-binary"))
	r, cacheRoot := newTestResolver(t)

	first, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if err != nil {
		t.Fatalf("ResolveEntry #1: %v", err)
	}
	slotPath := filepath.Join(cacheRoot, "hetzner", sha, "hetzner")
	st1, err := os.Stat(slotPath)
	if err != nil {
		t.Fatalf("stat slot artifact: %v", err)
	}

	second, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if err != nil {
		t.Fatalf("ResolveEntry #2: %v", err)
	}
	st2, err := os.Stat(slotPath)
	if err != nil {
		t.Fatalf("stat slot artifact #2: %v", err)
	}
	if first.BinarySHA256 != second.BinarySHA256 {
		t.Errorf("digest unstable between runs")
	}
	// Immutable slot not recreated — artifact mtime unchanged.
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Errorf("slot recreated on re-resolve of same commit (mtime %v → %v)", st1.ModTime(), st2.ModTime())
	}
}

func TestResolveEntry_CurrentSymlinkAtomicSwap(t *testing.T) {
	// Resolve tag v1.0.0, then advance main with a new build and resolve main: current
	// must switch to the second commit atomically, and both slots must remain.
	fr := newFixtureRepo(t)
	shaA := taggedPlugin(t, fr, "soul-cloud-hetzner", []byte("bin-a"))
	r, cacheRoot := newTestResolver(t)

	if _, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0")); err != nil {
		t.Fatalf("resolve A (tag): %v", err)
	}

	fr.writeArtifact("soul-cloud-hetzner", sshDoc(), []byte("bin-b"))
	shaB := fr.commit("advance main")
	if shaB == shaA {
		t.Fatal("commit B matched A — setup broken")
	}

	if _, err := r.ResolveEntry(context.Background(), entryFor(fr, "main")); err != nil {
		t.Fatalf("resolve B (branch): %v", err)
	}

	link, err := os.Readlink(filepath.Join(cacheRoot, "hetzner", currentLink))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if link != shaB {
		t.Errorf("current → %q, want %q (latest resolve)", link, shaB)
	}
	// Both commit slots in place (commit_sha immutable, the old one is not deleted).
	for _, c := range []string{shaA, shaB} {
		if _, err := os.Stat(filepath.Join(cacheRoot, "hetzner", c, "hetzner")); err != nil {
			t.Errorf("slot %s missing: %v", c, err)
		}
	}
}

func TestResolveCatalog_CollectsWarningsAndDoesNotFail(t *testing.T) {
	// Good entries: cloud + soul_module; a broken one (no artifact in the checkout).
	okRepo := newFixtureRepo(t)
	taggedPlugin(t, okRepo, "soul-cloud-hetzner", []byte("bin"))

	modRepo := newFixtureRepo(t)
	modRepo.writeArtifact("soul-mod-redis", moduleDoc("acl", "config"), []byte("modbin"))
	modRepo.commit("soul module plugin")
	modRepo.tag("v1.0.0")

	brokenRepo := newFixtureRepo(t)
	brokenRepo.writeFile("README", []byte("no dist"))
	brokenRepo.commit("empty")
	brokenRepo.tag("v9.9.9")

	plugins := &config.KeeperPlugins{
		SSHProviders: []config.PluginCatalogEntry{
			{Name: "hetzner", Source: okRepo.fileURL(), Ref: "v1.0.0"},
			{Name: "broken", Source: brokenRepo.fileURL(), Ref: "v9.9.9"},
		},
		SoulModules: []config.PluginCatalogEntry{
			{Name: "redis", Source: modRepo.fileURL(), Ref: "v1.0.0"},
		},
	}
	r, _ := newTestResolver(t)
	catalog, err := pluginsource.NewCatalog(nil, NewProvider(r))
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	slots, warns, err := catalog.ResolveCatalog(context.Background(), plugins)
	if err != nil {
		t.Fatalf("ResolveCatalog returned fatal: %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("slots = %d, want 2 (hetzner + redis): %v", len(slots), slots)
	}
	byAlias := map[string]pluginsource.Resolved{}
	for _, s := range slots {
		byAlias[s.Alias] = s
	}
	if _, ok := byAlias["hetzner"]; !ok {
		t.Errorf("no slot hetzner: %v", byAlias)
	}
	mod, ok := byAlias["redis"]
	if !ok {
		t.Fatalf("no slot redis: %v", byAlias)
	}
	// The alias came from the catalog and the modules from the artifact; neither side
	// could have produced the pair alone.
	if mod.Contents == nil || mod.Contents.Doc == nil || len(mod.Contents.Doc.Modules) != 2 {
		t.Errorf("redis slot should carry the artifact's two modules: %+v", mod.Contents)
	}
	if len(warns) != 1 {
		t.Fatalf("warns = %d, want 1 (broken entry): %v", len(warns), warns)
	}
}

// TestResolveEntry_ErrArtifactTooLarge: an artifact larger than max_artifact_size →
// ErrArtifactTooLarge, slot NOT created (fail-closed, ADR-026(g)).
func TestResolveEntry_ErrArtifactTooLarge(t *testing.T) {
	fr := newFixtureRepo(t)
	oversized := make([]byte, 4096) // > the 1024-byte limit
	sha := taggedPlugin(t, fr, "soul-cloud-hetzner", oversized)
	r, cacheRoot := newTestResolverWithLimits(t, 1024, 0)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("err = %v, want ErrArtifactTooLarge", err)
	}
	// Fail-closed: the commit_sha slot is not materialized and current is not created.
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "hetzner", sha)); !os.IsNotExist(statErr) {
		t.Errorf("slot created despite limit exceeded (stat err=%v)", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(cacheRoot, "hetzner", currentLink)); !os.IsNotExist(statErr) {
		t.Errorf("current symlink created despite limit exceeded (stat err=%v)", statErr)
	}
}

// TestResolveEntry_ErrCloneTooLarge: a working tree larger than max_clone_size →
// ErrCloneTooLarge + workdir cleanup (fail-closed, ADR-026(g)).
func TestResolveEntry_ErrCloneTooLarge(t *testing.T) {
	fr := newFixtureRepo(t)
	// A tree bloated with a junk file beside a valid plugin.
	fr.writeArtifact("soul-cloud-hetzner", sshDoc(), []byte("bin"))
	fr.writeFile("bloat.dat", make([]byte, 8192))
	fr.commit("bloated plugin")
	fr.tag("v1.0.0")
	// Clone limit below the tree size; artifact limit default (missed).
	r, cacheRoot := newTestResolverWithLimits(t, 0, 2048)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrCloneTooLarge) {
		t.Fatalf("err = %v, want ErrCloneTooLarge", err)
	}
	// Cleanup: the workdir is deleted (its name is the alias, under workRoot).
	workdir := filepath.Join(filepath.Dir(cacheRoot), "work", "hetzner")
	if _, statErr := os.Stat(workdir); !os.IsNotExist(statErr) {
		t.Errorf("workdir not cleaned after ErrCloneTooLarge (stat err=%v)", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "hetzner")); !os.IsNotExist(statErr) {
		t.Errorf("slot created despite ErrCloneTooLarge (stat err=%v)", statErr)
	}
}

// TestResolveEntry_WithinSizeLimits: a normal size under explicit (non-default) limits
// resolves without error — the happy path is not broken by the hardening.
func TestResolveEntry_WithinSizeLimits(t *testing.T) {
	fr := newFixtureRepo(t)
	wantSHA := taggedPlugin(t, fr, "soul-cloud-hetzner", []byte("small-binary"))
	r, cacheRoot := newTestResolverWithLimits(t, 1<<20, 16<<20)

	got, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if err != nil {
		t.Fatalf("ResolveEntry within limits: %v", err)
	}
	if got.CommitSHA != wantSHA {
		t.Errorf("CommitSHA = %q, want %q", got.CommitSHA, wantSHA)
	}
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "hetzner", wantSHA, "hetzner")); statErr != nil {
		t.Errorf("slot not created with size within limit: %v", statErr)
	}
}

func TestResolveCatalog_NilPlugins(t *testing.T) {
	r, _ := newTestResolver(t)
	catalog, err := pluginsource.NewCatalog(nil, NewProvider(r))
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	slots, warns, err := catalog.ResolveCatalog(context.Background(), nil)
	if err != nil || slots != nil || warns != nil {
		t.Errorf("nil plugins: expected empty result, got slots=%v warns=%v err=%v", slots, warns, err)
	}
}
