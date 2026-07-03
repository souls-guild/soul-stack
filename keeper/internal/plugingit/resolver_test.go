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

// TestMain включает SOUL_STACK_ALLOW_FILE_REPOS на всё время прогона пакета:
// тесты резолвят локальные file://-репозитории, которые в проде запрещены
// scheme-allowlist-ом ([validateGitScheme]). Тест на сам allowlist
// (TestResolveEntry_FileSchemeRequiresFlag) сохраняет/восстанавливает флаг
// локально через t.Setenv.
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

// fixtureRepo — рабочая обёртка над локальным git-репозиторием-источником
// плагина (наполняется manifest + dist/<binary>), используемым go-git-резолвером
// через file://-URL. Без системного git и без git-егресса наружу.
type fixtureRepo struct {
	t    *testing.T
	dir  string
	repo *git.Repository
}

// newFixtureRepo инициализирует пустой git-репозиторий во временном каталоге.
// Дефолтная ветка — `main` (`master` вне словаря Soul Stack).
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

// writePlugin кладёт в рабочее дерево manifest.yaml и dist/<binName> с заданным
// содержимым. Пустой manifest/binName пропускает соответствующий файл (для
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

// commit добавляет все изменения и создаёт коммит, возвращая его sha1.
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

// tag создаёт lightweight-тег на HEAD.
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

// taggedPlugin — частый сетап: коммит с валидным cloud-плагином + тег v1.0.0.
// Возвращает sha1 коммита под тегом.
func taggedPlugin(fr *fixtureRepo, binName string, binary []byte) string {
	fr.writePlugin(validCloudManifest, binName, binary)
	sha := fr.commit("plugin")
	fr.tag("v1.0.0")
	return sha
}

func newTestResolver(t *testing.T) (*Resolver, string) {
	t.Helper()
	// 0/0 size-лимиты → дефолты (256 MiB / 1024 MiB), happy-path не упирается.
	return newTestResolverWithLimits(t, 0, 0)
}

// newTestResolverWithLimits — резолвер с явными байт-лимитами артефакта/клона
// (для size-limit тестов, ADR-026(g)). 0 → дефолт соответствующего лимита.
func newTestResolverWithLimits(t *testing.T, maxArtifact, maxClone int64) (*Resolver, string) {
	t.Helper()
	base := t.TempDir()
	cacheRoot := filepath.Join(base, "cache")
	workRoot := filepath.Join(base, "work")
	return NewResolver(cacheRoot, workRoot, 0, maxArtifact, maxClone, nil), cacheRoot
}

// entryFor строит запись каталога на file://-источник fr с ref-ом.
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

	// Слот лёг по R-nested-раскладке + current → commit.
	wantSlot := filepath.Join(cacheRoot, "cloud-hetzner", wantSHA)
	if got.SlotDir != wantSlot {
		t.Errorf("SlotDir = %q, want %q", got.SlotDir, wantSlot)
	}
	if _, err := os.Stat(filepath.Join(wantSlot, sharedplugin.FileName)); err != nil {
		t.Errorf("manifest в слоте отсутствует: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wantSlot, "soul-cloud-hetzner")); err != nil {
		t.Errorf("бинарь в слоте отсутствует: %v", err)
	}
	link, err := os.Readlink(filepath.Join(cacheRoot, "cloud-hetzner", currentLink))
	if err != nil {
		t.Fatalf("readlink current: %v", err)
	}
	if link != wantSHA {
		t.Errorf("current → %q, want %q", link, wantSHA)
	}

	// Бинарь должен быть исполняемым (0755), manifest — 0644.
	if st, _ := os.Stat(filepath.Join(wantSlot, "soul-cloud-hetzner")); st.Mode().Perm()&0o111 == 0 {
		t.Errorf("бинарь не исполняемый: %v", st.Mode())
	}
}

// TestResolveEntry_BranchRef проверяет резолв ref-ветки (`main`), не только тега.
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
	// Коммит без manifest.yaml.
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
	// manifest есть, dist/<binary> нет.
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
	// Несуществующий локальный репозиторий → clone провалится.
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

// TestResolveEntry_FileSchemeRequiresFlag фиксирует scheme-allowlist: file://
// без env-флага отвергается ErrSourceUnavailable ещё до git-операций.
func TestResolveEntry_FileSchemeRequiresFlag(t *testing.T) {
	fr := newFixtureRepo(t)
	taggedPlugin(fr, "soul-cloud-hetzner", []byte("bin"))
	r, _ := newTestResolver(t)

	t.Setenv(allowFileReposEnv, "")
	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("file:// без флага: err = %v, want ErrSourceUnavailable", err)
	}
}

// TestResolveEntry_UnsupportedScheme фиксирует, что http:// (незашифрованный) и
// прочие схемы вне allowlist отвергаются без git-егресса.
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
		t.Errorf("digest нестабилен между прогонами")
	}
	// Иммутабельный слот не пересоздавался — mtime бинаря не изменился.
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Errorf("слот пересоздан при повторном резолве того же commit (mtime %v → %v)", st1.ModTime(), st2.ModTime())
	}
}

func TestResolveEntry_CurrentSymlinkAtomicSwap(t *testing.T) {
	// Резолв тега v1.0.0, затем продвижение ветки main с новым плагином и
	// резолв main: current должен переключиться на второй commit атомарно, оба
	// слота — остаться в кеше.
	fr := newFixtureRepo(t)
	shaA := taggedPlugin(fr, "soul-cloud-hetzner", []byte("bin-a"))
	r, cacheRoot := newTestResolver(t)

	if _, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0")); err != nil {
		t.Fatalf("resolve A (tag): %v", err)
	}

	// Двигаем main вперёд новым коммитом плагина.
	fr.writePlugin(validCloudManifest, "soul-cloud-hetzner", []byte("bin-b"))
	shaB := fr.commit("advance main")
	if shaB == shaA {
		t.Fatal("commit B совпал с A — сетап сломан")
	}

	if _, err := r.ResolveEntry(context.Background(), entryFor(fr, "main")); err != nil {
		t.Fatalf("resolve B (branch): %v", err)
	}

	link, err := os.Readlink(filepath.Join(cacheRoot, "cloud-hetzner", currentLink))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if link != shaB {
		t.Errorf("current → %q, want %q (последний резолв)", link, shaB)
	}
	// Оба commit-слота на месте (commit_sha иммутабелен, старый не удаляется).
	for _, c := range []string{shaA, shaB} {
		if _, err := os.Stat(filepath.Join(cacheRoot, "cloud-hetzner", c, "soul-cloud-hetzner")); err != nil {
			t.Errorf("слот %s отсутствует: %v", c, err)
		}
	}
}

func TestResolveCatalog_CollectsWarningsAndDoesNotFail(t *testing.T) {
	// Годные entry: cloud + soul_module; сломанный (нет manifest в checkout-е).
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
		t.Fatalf("ResolveCatalog вернул fatal: %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("slots = %d, want 2 (hetzner + community.redis): %v", len(slots), slots)
	}
	byName := map[string]ResolvedSlot{}
	for _, s := range slots {
		byName[s.Name] = s
	}
	if _, ok := byName["hetzner"]; !ok {
		t.Errorf("нет слота hetzner: %v", byName)
	}
	if mod, ok := byName["redis"]; !ok || mod.Namespace != "community" {
		t.Errorf("нет слота community.redis: %v", byName)
	}
	if len(warns) != 1 {
		t.Fatalf("warns = %d, want 1 (broken entry): %v", len(warns), warns)
	}
}

// TestResolveEntry_ErrArtifactTooLarge: бинарь больше max_artifact_size →
// ErrArtifactTooLarge, слот НЕ создаётся (fail-closed, ADR-026(g)).
func TestResolveEntry_ErrArtifactTooLarge(t *testing.T) {
	fr := newFixtureRepo(t)
	oversized := make([]byte, 4096) // > лимита 1024 байт
	sha := taggedPlugin(fr, "soul-cloud-hetzner", oversized)
	r, cacheRoot := newTestResolverWithLimits(t, 1024, 0)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("err = %v, want ErrArtifactTooLarge", err)
	}
	// Fail-closed: слот под commit_sha не материализован, current не создан.
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "cloud-hetzner", sha)); !os.IsNotExist(statErr) {
		t.Errorf("слот создан несмотря на превышение лимита (stat err=%v)", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(cacheRoot, "cloud-hetzner", currentLink)); !os.IsNotExist(statErr) {
		t.Errorf("current symlink создан несмотря на превышение лимита (stat err=%v)", statErr)
	}
}

// TestResolveEntry_ErrCloneTooLarge: суммарное рабочее дерево больше
// max_clone_size → ErrCloneTooLarge + cleanup workdir (fail-closed, ADR-026(g)).
func TestResolveEntry_ErrCloneTooLarge(t *testing.T) {
	fr := newFixtureRepo(t)
	// Дерево раздувается мусорным файлом помимо валидного плагина.
	fr.writePlugin(validCloudManifest, "soul-cloud-hetzner", []byte("bin"))
	fr.writeFile("bloat.dat", make([]byte, 8192))
	fr.commit("bloated plugin")
	fr.tag("v1.0.0")
	// Лимит клона ниже размера дерева; artifact-лимит дефолтный (мимо).
	r, cacheRoot := newTestResolverWithLimits(t, 0, 2048)

	_, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if !errors.Is(err, ErrCloneTooLarge) {
		t.Fatalf("err = %v, want ErrCloneTooLarge", err)
	}
	// Cleanup: workdir удалён (имя workdir = sanitizeSegment(name) под workRoot).
	workdir := filepath.Join(filepath.Dir(cacheRoot), "work", "hetzner")
	if _, statErr := os.Stat(workdir); !os.IsNotExist(statErr) {
		t.Errorf("workdir не вычищен после ErrCloneTooLarge (stat err=%v)", statErr)
	}
	// Слот не создан.
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "cloud-hetzner")); !os.IsNotExist(statErr) {
		t.Errorf("слот создан несмотря на ErrCloneTooLarge (stat err=%v)", statErr)
	}
}

// TestResolveEntry_WithinSizeLimits: нормальный размер при заданных (не
// дефолтных) лимитах резолвится без ошибки — happy-path не ломается hardening-ом.
func TestResolveEntry_WithinSizeLimits(t *testing.T) {
	binary := []byte("small-binary")
	fr := newFixtureRepo(t)
	wantSHA := taggedPlugin(fr, "soul-cloud-hetzner", binary)
	// Лимиты с запасом над реальным размером бинаря и дерева.
	r, cacheRoot := newTestResolverWithLimits(t, 1<<20, 16<<20)

	got, err := r.ResolveEntry(context.Background(), entryFor(fr, "v1.0.0"))
	if err != nil {
		t.Fatalf("ResolveEntry within limits: %v", err)
	}
	if got.CommitSHA != wantSHA {
		t.Errorf("CommitSHA = %q, want %q", got.CommitSHA, wantSHA)
	}
	if _, statErr := os.Stat(filepath.Join(cacheRoot, "cloud-hetzner", wantSHA, "soul-cloud-hetzner")); statErr != nil {
		t.Errorf("слот не создан при размере в пределах лимита: %v", statErr)
	}
}

func TestResolveCatalog_NilPlugins(t *testing.T) {
	r, _ := newTestResolver(t)
	slots, warns, err := r.ResolveCatalog(context.Background(), nil)
	if err != nil || slots != nil || warns != nil {
		t.Errorf("nil plugins: ожидали пустой результат, got slots=%v warns=%v err=%v", slots, warns, err)
	}
}
