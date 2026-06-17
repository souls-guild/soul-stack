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

// TestMain включает SOUL_STACK_ALLOW_FILE_REPOS на всё время прогона пакета:
// unit/integration-тесты грузят локальные file://-репозитории, которые в проде
// запрещены scheme-allowlist-ом (security review L2). Тесты на сам allowlist
// сохраняют/восстанавливают флаг локально через t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(allowFileReposEnv, "1")
	os.Exit(m.Run())
}

// validManifest — минимальный валидный service.yml для тестовых репозиториев.
const validManifest = `name: web-app
state_schema_version: 1
state_schema:
  type: object
  properties:
    replicas:
      type: integer
`

// testRepo — рабочая обёртка над локальным git-репозиторием для тестов.
type testRepo struct {
	t    *testing.T
	dir  string
	repo *git.Repository
}

// newTestRepo создаёт непустой (non-bare) git-репозиторий во временном каталоге
// с одним начальным коммитом, содержащим service.yml.
func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	tr := &testRepo{t: t, dir: t.TempDir()}
	tr.initRepo()
	tr.writeFile("service.yml", validManifest)
	tr.commit("initial")
	return tr
}

// initRepo инициализирует git-репозиторий в tr.dir (для repo-обёрток, наполняемых
// нестандартным набором файлов — например, destiny-репо в destiny_test.go).
// Дефолтная ветка — `main` (`master` вне словаря Soul Stack); тесты ветки
// резолвят `Ref: "main"`.
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

// fileURL возвращает file://-URL репозитория для go-git.
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

// commit добавляет все изменения и создаёт коммит, возвращая его sha1.
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

// tag создаёт lightweight-тег на HEAD.
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
		t.Fatalf("manifest не распарсен корректно: %+v", art.Manifest)
	}
	if _, err := os.Stat(filepath.Join(art.LocalDir, "service.yml")); err != nil {
		t.Fatalf("снапшот не содержит service.yml: %v", err)
	}
	// Снапшот — чистое дерево без .git.
	if _, err := os.Stat(filepath.Join(art.LocalDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git не должен попадать в снапшот, stat err = %v", err)
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
		t.Fatalf("снапшот тега подхватил состояние после тега: desc=%q", art.Manifest.Description)
	}
}

func TestLoad_BranchAdvanceRefetched(t *testing.T) {
	tr := newTestRepo(t)
	loader := newLoader(t)

	first, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL(), Ref: "main"})
	if err != nil {
		t.Fatalf("Load #1: %v", err)
	}

	// Двигаем ветку main вперёд.
	tr.writeFile("CHANGELOG", "advance\n")
	newHead := tr.commit("advance")

	second, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL(), Ref: "main"})
	if err != nil {
		t.Fatalf("Load #2: %v", err)
	}
	if second.SHA1 == first.SHA1 {
		t.Fatalf("ветка не перефетчена: оба Load вернули %s", first.SHA1)
	}
	if second.SHA1 != newHead {
		t.Fatalf("второй Load = %s, want новый tip %s", second.SHA1, newHead)
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
		t.Fatalf("снапшот не переиспользован: %s != %s", a.LocalDir, b.LocalDir)
	}
	info2, _ := os.Stat(b.LocalDir)
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatalf("снапшот пересоздан (modtime изменился)")
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
		t.Fatalf("содержимое = %q", string(data))
	}
}

func TestReadFile_PathTraversalBlocked(t *testing.T) {
	tr := newTestRepo(t)
	loader := newLoader(t)
	art, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// securejoin клампит `..` к корню снапшота → попытка выйти наружу не читает
	// внешний файл, а упирается в несуществующий путь внутри снапшота.
	if _, err := loader.ReadFile(art, "../../../../etc/passwd"); err == nil {
		t.Fatalf("path traversal не заблокирован")
	}
}

func TestLoad_InvalidManifestRejected(t *testing.T) {
	tr := newTestRepo(t)
	tr.writeFile("service.yml", "name: web-app\n") // нет state_schema_version/state_schema
	tr.commit("break manifest")

	loader := newLoader(t)
	_, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL()})
	if err == nil {
		t.Fatalf("ожидалась ошибка на невалидный service.yml")
	}
}

func TestLoad_UnresolvableRef(t *testing.T) {
	tr := newTestRepo(t)
	loader := newLoader(t)
	_, err := loader.Load(context.Background(), ServiceRef{Name: "web-app", Git: tr.fileURL(), Ref: "no-such-ref"})
	if err == nil {
		t.Fatalf("ожидалась ошибка на несуществующий ref")
	}
}

func TestLoad_EmptyGitURL(t *testing.T) {
	loader := newLoader(t)
	if _, err := loader.Load(context.Background(), ServiceRef{Name: "web-app"}); err == nil {
		t.Fatalf("ожидалась ошибка на пустой Git URL")
	}
}

func TestLoad_NonKebabNameRejected(t *testing.T) {
	tr := newTestRepo(t)
	loader := newLoader(t)
	if _, err := loader.Load(context.Background(), ServiceRef{Name: "../escape", Git: tr.fileURL()}); err == nil {
		t.Fatalf("ожидалась ошибка на не-kebab имя сервиса")
	}
}
