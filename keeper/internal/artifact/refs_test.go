package artifact

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// TestListRefs_LocalRepo прогоняет ListRefs против локального file://-репо
// (testRepo даёт правильный init + main-branch + теги). Покрывает три
// инварианта:
//
//   - branches приходят и помечают main как is_default (HEAD-symref);
//   - tags приходят с правильными commit-hash-ами;
//   - tags идут перед branches в результирующем слайсе.
func TestListRefs_LocalRepo(t *testing.T) {
	tr := newTestRepo(t)
	// initial-commit уже есть; добавим коммит и теги поверх него.
	tr.writeFile("VERSION", "1.0\n")
	v1head := tr.commit("v1-base")
	tr.tag("v1.0.0")
	tr.writeFile("CHANGELOG", "v2\n")
	tr.commit("v2")
	tr.tag("v2.0.0")

	refs, err := ListRefs(context.Background(), tr.fileURL())
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if len(refs) < 3 {
		t.Fatalf("ожидалось >= 3 ref-а (2 тега + main), получено %d: %+v", len(refs), refs)
	}

	// Tags — первыми, semver-desc: v2.0.0 перед v1.0.0.
	if refs[0].Name != "v2.0.0" || refs[0].Type != GitRefTypeTag {
		t.Errorf("первая запись должна быть tag v2.0.0, получено %+v", refs[0])
	}
	if refs[1].Name != "v1.0.0" || refs[1].Type != GitRefTypeTag {
		t.Errorf("вторая запись должна быть tag v1.0.0, получено %+v", refs[1])
	}
	if refs[1].Commit != v1head {
		t.Errorf("tag v1.0.0 указывает на %s, ожидалось %s", refs[1].Commit, v1head)
	}

	// main — после тегов, IsDefault=true (HEAD-symref в file:// тоже виден go-git).
	var mainFound bool
	for _, r := range refs {
		if r.Name == "main" && r.Type == GitRefTypeBranch {
			mainFound = true
			if !r.IsDefault {
				t.Errorf("main branch не помечен is_default: %+v", r)
			}
			break
		}
	}
	if !mainFound {
		t.Fatalf("main branch не найден среди refs: %+v", refs)
	}
}

// TestListRefs_EmptyURL — sanity-check на ранний возврат без сетевых вызовов.
func TestListRefs_EmptyURL(t *testing.T) {
	if _, err := ListRefs(context.Background(), ""); err == nil {
		t.Fatalf("ожидалась ошибка на пустой gitURL")
	}
}

// TestListRefs_UnsupportedScheme — http:// не в allowlist-е.
func TestListRefs_UnsupportedScheme(t *testing.T) {
	t.Setenv(allowFileReposEnv, "0") // file:// тоже выключим, чтобы http не проскочил
	if _, err := ListRefs(context.Background(), "http://example.com/repo.git"); err == nil {
		t.Fatalf("ожидалась ошибка на http://")
	}
}

// TestClassifyRefs_Sorting проверяет чистую функцию-сортировщик без сети.
//
// Покрывает:
//   - semver-desc для тегов с pre-release; не-semver идёт после semver-валидных;
//   - peeled-форма annotated-тега подменяет голый sha тег-объекта на commit;
//   - default-branch первым; остальные ветки lex-asc;
//   - fallback main/master, если HEAD-symref не пришёл.
func TestClassifyRefs_Sorting(t *testing.T) {
	const (
		commitA = "1111111111111111111111111111111111111111"
		commitB = "2222222222222222222222222222222222222222"
		commitC = "3333333333333333333333333333333333333333"
		commitD = "4444444444444444444444444444444444444444"
		tagObj  = "9999999999999999999999999999999999999999"
	)

	in := []*plumbing.Reference{
		plumbing.NewHashReference("refs/tags/v1.0.0", plumbing.NewHash(commitA)),
		plumbing.NewHashReference("refs/tags/v2.0.0", plumbing.NewHash(tagObj)),
		plumbing.NewHashReference("refs/tags/v2.0.0^{}", plumbing.NewHash(commitB)), // peeled
		plumbing.NewHashReference("refs/tags/v2.0.0-rc.1", plumbing.NewHash(commitC)),
		plumbing.NewHashReference("refs/tags/release-old", plumbing.NewHash(commitD)),
		plumbing.NewHashReference("refs/heads/develop", plumbing.NewHash(commitA)),
		plumbing.NewHashReference("refs/heads/main", plumbing.NewHash(commitB)),
		plumbing.NewHashReference("refs/heads/feature/x", plumbing.NewHash(commitC)),
		plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main"),
	}

	got := classifyRefs(in)

	// Ожидаемый порядок:
	//   tag v2.0.0  (release > rc)
	//   tag v2.0.0-rc.1
	//   tag v1.0.0
	//   tag release-old (не-semver, идёт после semver)
	//   branch main (default)
	//   branch develop
	//   branch feature/x
	want := []GitRef{
		{Name: "v2.0.0", Type: GitRefTypeTag, Commit: commitB}, // peeled подменил sha
		{Name: "v2.0.0-rc.1", Type: GitRefTypeTag, Commit: commitC},
		{Name: "v1.0.0", Type: GitRefTypeTag, Commit: commitA},
		{Name: "release-old", Type: GitRefTypeTag, Commit: commitD},
		{Name: "main", Type: GitRefTypeBranch, Commit: commitB, IsDefault: true},
		{Name: "develop", Type: GitRefTypeBranch, Commit: commitA},
		{Name: "feature/x", Type: GitRefTypeBranch, Commit: commitC},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d; got=%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ref[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestClassifyRefs_FallbackDefault — без HEAD-symref main помечается через
// fallback (старые серверы без HEAD-листинга / некоторые file://-форматы).
func TestClassifyRefs_FallbackDefault(t *testing.T) {
	in := []*plumbing.Reference{
		plumbing.NewHashReference("refs/heads/develop", plumbing.NewHash("1111111111111111111111111111111111111111")),
		plumbing.NewHashReference("refs/heads/main", plumbing.NewHash("2222222222222222222222222222222222222222")),
	}
	got := classifyRefs(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "main" || !got[0].IsDefault {
		t.Errorf("main должен быть первым с IsDefault=true, got %+v", got[0])
	}
}

// TestClassifyRefs_FallbackMaster — fallback берёт master, если main нет.
func TestClassifyRefs_FallbackMaster(t *testing.T) {
	in := []*plumbing.Reference{
		plumbing.NewHashReference("refs/heads/feat", plumbing.NewHash("1111111111111111111111111111111111111111")),
		plumbing.NewHashReference("refs/heads/master", plumbing.NewHash("2222222222222222222222222222222222222222")),
	}
	got := classifyRefs(in)
	if got[0].Name != "master" || !got[0].IsDefault {
		t.Errorf("master должен быть первым с IsDefault=true, got %+v", got[0])
	}
}

// TestParseSemver проверяет граничные случаи парсера: v-prefix, pre-release,
// невалидные строки уходят в not-ok.
func TestParseSemver(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		major int
		pre   string
	}{
		{"v1.2.3", true, 1, ""},
		{"1.2.3", true, 1, ""},
		{"v1.2.3-rc.1", true, 1, "rc.1"},
		{"v1.0", false, 0, ""},
		{"release-2025", false, 0, ""},
		{"v1.2.x", false, 0, ""},
	}
	for _, c := range cases {
		s, ok := parseSemver(c.in)
		if ok != c.ok {
			t.Errorf("parseSemver(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && (s.Major != c.major || s.Pre != c.pre) {
			t.Errorf("parseSemver(%q) = %+v, want major=%d pre=%q", c.in, s, c.major, c.pre)
		}
	}
}
