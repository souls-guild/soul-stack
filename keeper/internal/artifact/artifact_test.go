package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestMain enables SOUL_STACK_ALLOW_FILE_REPOS for the whole package run:
// unit/integration tests load local file:// repositories, which are forbidden
// in prod by the scheme allowlist (security review L2). Tests for the allowlist
// itself save/restore the flag locally through t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(allowFileReposEnv, "1")
	os.Exit(m.Run())
}

// validManifest is the minimal valid service.yml for test repositories.
const validManifest = `name: web-app
state_schema_version: 1
state_schema:
  type: object
  properties:
    replicas:
      type: integer
`

// testRepo is a working wrapper around a local git repository for tests.
type testRepo struct {
	t    *testing.T
	dir  string
	repo *git.Repository
}

// newTestRepo creates a non-empty (non-bare) git repository in a temp directory
// with one initial commit containing service.yml.
func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	tr := &testRepo{t: t, dir: t.TempDir()}
	tr.initRepo()
	tr.writeFile("service.yml", validManifest)
	tr.commit("initial")
	return tr
}

// initRepo initializes a git repository in tr.dir (for repo wrappers populated
// with a custom file set, for example a destiny repo in destiny_test.go).
// Default branch is `main` (`master` is outside the Soul Stack vocabulary);
// branch tests resolve `Ref: "main"`.
func (tr *testRepo) initRepo() {
	tr.t.Helper()
	repo, err := git.PlainInitWithOptions(tr.dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		tr.t.Fatalf("PlainInit: %v", err)
	}
	tr.repo = repo
}

// fileURL returns the repository file:// URL for go-git.
func (tr *testRepo) fileURL() string { return "file://" + tr.dir }

func (tr *testRepo) writeFile(path, content string) {
	tr.t.Helper()
	full := filepath.Join(tr.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tr.t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		tr.t.Fatalf("WriteFile: %v", err)
	}
}

// commit adds all changes and creates a commit, returning its sha1.
func (tr *testRepo) commit(msg string) string {
	tr.t.Helper()
	wt, err := tr.repo.Worktree()
	if err != nil {
		tr.t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		tr.t.Fatalf("AddGlob: %v", err)
	}
	h, err := wt.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	})
	if err != nil {
		tr.t.Fatalf("Commit: %v", err)
	}
	return h.String()
}

// tag creates a lightweight tag on HEAD.
func (tr *testRepo) tag(name string) {
	tr.t.Helper()
	head, err := tr.repo.Head()
	if err != nil {
		tr.t.Fatalf("Head: %v", err)
	}
	if _, err := tr.repo.CreateTag(name, head.Hash(), nil); err != nil {
		tr.t.Fatalf("CreateTag: %v", err)
	}
}

func newLoader(t *testing.T) *ServiceLoader {
	t.Helper()
	return NewServiceLoader(t.TempDir(), nil)
}

func (tr *testRepo) headSHA() string {
	tr.t.Helper()
	head, err := tr.repo.Head()
	if err != nil {
		tr.t.Fatalf("Head: %v", err)
	}
	return head.Hash().String()
}

func TestLoad_DefaultHEAD(t *testing.T) {
	tr := newTestRepo(t)
	want := tr.headSHA()

	loader := newLoader(t)
	art, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if art.SHA1 != want {
		t.Fatalf("SHA1 = %s, want HEAD %s", art.SHA1, want)
	}
	if art.Manifest == nil || art.Manifest.Name != "web-app" {
		t.Fatalf("manifest was not parsed correctly: %+v", art.Manifest)
	}
	if _, err := os.Stat(filepath.Join(art.LocalDir, "service.yml")); err != nil {
		t.Fatalf("snapshot does not contain service.yml: %v", err)
	}
	// Snapshot is a clean tree without .git.
	if _, err := os.Stat(filepath.Join(art.LocalDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git should not land in snapshot, stat err = %v", err)
	}
}

func TestLoad_Tag(t *testing.T) {
	tr := newTestRepo(t)
	tr.writeFile("VERSION", "1.0.0\n")
	tagged := tr.commit("for-tag")
	tr.tag("v1.0.0")
	tr.writeFile("service.yml", validManifest+"description: moved on\n")
	tr.commit("after-tag")

	loader := newLoader(t)
	art, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL(), Ref: "v1.0.0"})
	if err != nil {
		t.Fatalf("Load tag: %v", err)
	}
	if art.SHA1 != tagged {
		t.Fatalf("tag SHA1 = %s, want %s", art.SHA1, tagged)
	}
	if art.Manifest.Description != "" {
		t.Fatalf("tag snapshot picked up state after tag: desc=%q", art.Manifest.Description)
	}
}

func TestLoad_BranchAdvanceRefetched(t *testing.T) {
	tr := newTestRepo(t)
	loader := newLoader(t)

	first, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL(), Ref: "main"})
	if err != nil {
		t.Fatalf("Load #1: %v", err)
	}

	// Move branch main forward.
	tr.writeFile("CHANGELOG", "advance\n")
	newHead := tr.commit("advance")

	second, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL(), Ref: "main"})
	if err != nil {
		t.Fatalf("Load #2: %v", err)
	}
	if second.SHA1 == first.SHA1 {
		t.Fatalf("branch was not refetched: both Load calls returned %s", first.SHA1)
	}
	if second.SHA1 != newHead {
		t.Fatalf("second Load = %s, want new tip %s", second.SHA1, newHead)
	}
}

func TestLoad_SnapshotReuse(t *testing.T) {
	tr := newTestRepo(t)
	loader := newLoader(t)
	ref := ServiceRef{Name: "web-app", Git: tr.fileURL(), Ref: "main"}

	a, err := loader.Load(context.Background(), ref)
	if err != nil {
		t.Fatalf("Load #1: %v", err)
	}
	info1, _ := os.Stat(a.LocalDir)

	b, err := loader.Load(context.Background(), ref)
	if err != nil {
		t.Fatalf("Load #2: %v", err)
	}
	if a.LocalDir != b.LocalDir {
		t.Fatalf("snapshot was not reused: %s != %s", a.LocalDir, b.LocalDir)
	}
	info2, _ := os.Stat(b.LocalDir)
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatalf("snapshot was recreated (modtime changed)")
	}
}

func TestReadFile_ArbitraryFile(t *testing.T) {
	tr := newTestRepo(t)
	tr.writeFile("scenario/deploy/main.yml", "on: keeper\n")
	tr.commit("add scenario")

	loader := newLoader(t)
	art, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	data, err := loader.ReadFile(art, "scenario/deploy/main.yml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "on: keeper\n" {
		t.Fatalf("content = %q", string(data))
	}
}

func TestReadFile_PathTraversalBlocked(t *testing.T) {
	tr := newTestRepo(t)
	loader := newLoader(t)
	art, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// securejoin clamps `..` to the snapshot root: an escape attempt does not
	// read the external file and instead hits a nonexistent path inside snapshot.
	if _, err := loader.ReadFile(art, "../../../../etc/passwd"); err == nil {
		t.Fatalf("path traversal was not blocked")
	}
}

func TestLoad_InvalidManifestRejected(t *testing.T) {
	tr := newTestRepo(t)
	tr.writeFile("service.yml", "name: web-app\n") // no state_schema_version/state_schema
	tr.commit("break manifest")

	loader := newLoader(t)
	_, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL()})
	if err == nil {
		t.Fatalf("want error for invalid service.yml")
	}
}

func TestLoad_UnresolvableRef(t *testing.T) {
	tr := newTestRepo(t)
	loader := newLoader(t)
	_, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL(), Ref: "no-such-ref"})
	if err == nil {
		t.Fatalf("want error for nonexistent ref")
	}
}

func TestLoad_EmptyGitURL(t *testing.T) {
	loader := newLoader(t)
	if _, err := loader.Load(context.Background(), ServiceRef{Name: "web-app"}); err == nil {
		t.Fatalf("want error for empty Git URL")
	}
}

func TestLoad_NonKebabNameRejected(t *testing.T) {
	tr := newTestRepo(t)
	loader := newLoader(t)
	if _, err := loader.Load(context.Background(), ServiceRef{Name: "../escape", Git: tr.fileURL()}); err == nil {
		t.Fatalf("want error for non-kebab service name")
	}
}
