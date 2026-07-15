package artifact

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// redisServiceRoot — the root of the real redis Service repo under examples/
// (relative to the keeper/internal/artifact package). The directive catalog is
// the feature's source of truth.
const redisServiceRoot = "../../../examples/service/redis"

// requireRedisExamples skips the test if the source tree has no examples/
// (custom build), otherwise returns the ABSOLUTE root of the redis service
// (securejoin requires an absolute base, like the production
// ServiceArtifact.LocalDir). Parity with the committed-spec-guard skip.
func requireRedisExamples(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(redisServiceRoot)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, essenceDefaultFile)); err != nil {
		t.Skipf("examples/service/redis/essence/_default.yaml недоступен (%v); guard пропущен", err)
	}
	return root
}

// TestLoadDirectiveCatalog_FullRealCatalog — guard #1: the real catalog
// carries all 6 series, known names are present, no typos.
func TestLoadDirectiveCatalog_FullRealCatalog(t *testing.T) {
	root := requireRedisExamples(t)
	cat, err := LoadDirectiveCatalog(root, "")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog: %v", err)
	}

	wantSeries := []string{"6.2", "7.0", "7.2", "7.4", "8.0", "8.2"}
	if len(cat) != len(wantSeries) {
		got := make([]string, 0, len(cat))
		for s := range cat {
			got = append(got, s)
		}
		sort.Strings(got)
		t.Fatalf("серий в каталоге = %d %v, want %d %v", len(cat), got, len(wantSeries), wantSeries)
	}
	for _, s := range wantSeries {
		if _, ok := cat[s]; !ok {
			t.Errorf("серия %q отсутствует в каталоге", s)
		}
	}

	// A known directive is present in 8.2; its typo variant is not (a guard on
	// the generator).
	if !containsStr(cat["8.2"], "maxmemory") {
		t.Errorf("директива maxmemory отсутствует в серии 8.2")
	}
	if containsStr(cat["8.2"], "maxmemoyr") {
		t.Errorf("опечатка maxmemoyr просочилась в серию 8.2")
	}
	// Every series is sorted.
	for s, names := range cat {
		if !sort.StringsAreSorted(names) {
			t.Errorf("серия %q не отсортирована", s)
		}
	}
}

// TestLoadDirectiveCatalog_VersionNarrows — guard #2: version narrows to the
// major.minor series; a foreign (8.2-only) directive is absent from 6.2.
func TestLoadDirectiveCatalog_VersionNarrows(t *testing.T) {
	root := requireRedisExamples(t)

	full, err := LoadDirectiveCatalog(root, "")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog(full): %v", err)
	}

	// 8.2.2 → exactly series 8.2.
	v82, err := LoadDirectiveCatalog(root, "8.2.2")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog(8.2.2): %v", err)
	}
	if len(v82) != 1 || v82["8.2"] == nil {
		t.Fatalf("version=8.2.2 → %v, want ровно {8.2}", keysOf(v82))
	}
	if !containsStr(v82["8.2"], "maxmemory") {
		t.Errorf("maxmemory отсутствует в сужении 8.2")
	}

	// 6.2.21 → exactly series 6.2.
	v62, err := LoadDirectiveCatalog(root, "6.2.21")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog(6.2.21): %v", err)
	}
	if len(v62) != 1 || v62["6.2"] == nil {
		t.Fatalf("version=6.2.21 → %v, want ровно {6.2}", keysOf(v62))
	}

	// A directive present in 8.2 but absent from 6.2 is missing from the 6.2
	// narrowing.
	only82 := firstOnlyIn(full["8.2"], full["6.2"])
	if only82 == "" {
		t.Skip("не нашлось 8.2-only директивы для cross-version-проверки")
	}
	if containsStr(v62["6.2"], only82) {
		t.Errorf("8.2-only директива %q протекла в сужение 6.2", only82)
	}
}

// TestLoadDirectiveCatalog_NoCatalog — guard #3 (the loader half): a service
// without redis_directives (and without an essence file) → an empty non-nil
// map + nil error.
func TestLoadDirectiveCatalog_NoCatalog(t *testing.T) {
	// (a) essence/_default.yaml exists, but without redis_directives.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "essence", "_default.yaml"), "conf_dir: /etc/redis\nmemory_reserve_percent: 75\n")
	cat, err := LoadDirectiveCatalog(root, "")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog: %v", err)
	}
	if cat == nil {
		t.Fatalf("каталог nil, want непустой пустой map")
	}
	if len(cat) != 0 {
		t.Errorf("каталог %v, want пустой", keysOf(cat))
	}

	// (b) essence file is absent entirely → also an empty catalog, no error.
	empty := t.TempDir()
	cat2, err := LoadDirectiveCatalog(empty, "8.2.2")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog(no file): %v", err)
	}
	if cat2 == nil || len(cat2) != 0 {
		t.Errorf("каталог без essence-файла = %v, want пустой non-nil", cat2)
	}
}

// TestLoadDirectiveCatalog_SortsNames — names within a series are sorted
// (defensive: the generator might have returned an unsorted list).
func TestLoadDirectiveCatalog_SortsNames(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "essence", "_default.yaml"),
		"redis_directives:\n  \"8.2\":\n    - zebra\n    - alpha\n    - maxmemory\n")
	cat, err := LoadDirectiveCatalog(root, "")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog: %v", err)
	}
	want := []string{"alpha", "maxmemory", "zebra"}
	if !equalStr(cat["8.2"], want) {
		t.Errorf("8.2 = %v, want отсортированный %v", cat["8.2"], want)
	}
}

// TestFilterDirectivesByVersion — a unit test of the narrowing rule (emulates
// the assert regex).
func TestFilterDirectivesByVersion(t *testing.T) {
	cat := map[string][]string{
		"6.2": {"a"},
		"7.4": {"b"},
		"8.2": {"c"},
	}
	// version="" → the whole catalog (the same map).
	if got := FilterDirectivesByVersion(cat, ""); len(got) != 3 {
		t.Errorf("version='' → %d серий, want 3", len(got))
	}
	// A distro pin with an epoch prefix matches the series.
	if got := FilterDirectivesByVersion(cat, "5:7.4.1-1~deb12u7"); len(got) != 1 || got["7.4"] == nil {
		t.Errorf("epoch-пин 5:7.4.1 → %v, want {7.4}", keysOf(got))
	}
	// 7.4 does not catch a 7.04-like series (the series boundary is the
	// trailing dot).
	if got := FilterDirectivesByVersion(cat, "7.42.0"); len(got) != 0 {
		t.Errorf("7.42.0 → %v, want пусто (7.4 не матчит 7.42)", keysOf(got))
	}
	// An unknown version → an empty non-nil map (assert-skip semantics).
	got := FilterDirectivesByVersion(cat, "9.9.9")
	if got == nil || len(got) != 0 {
		t.Errorf("9.9.9 → %v, want пустой non-nil", got)
	}
}

// TestListScenarios_RedisSettingsHasDirectivesAnnotation — guard #5: the
// x-directives:redis marker makes it into the DTO input_schema.redis_settings
// for create / create_from_souls (via covenant extends) AND update_config
// (directly in input). Otherwise the frontend silently stops validating
// directives.
func TestListScenarios_RedisSettingsHasDirectivesAnnotation(t *testing.T) {
	root := requireRedisExamples(t)
	scenarios, err := ListScenarios(root, nil)
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	byName := make(map[string]Scenario, len(scenarios))
	for _, s := range scenarios {
		byName[s.Name] = s
	}

	for _, name := range []string{"create", "create_from_souls", "update_config"} {
		sc, ok := byName[name]
		if !ok {
			t.Errorf("сценарий %q не найден в listing-е", name)
			continue
		}
		rs, ok := sc.InputSchema["redis_settings"].(map[string]any)
		if !ok {
			t.Errorf("%s: redis_settings отсутствует/не map в input_schema", name)
			continue
		}
		if got := rs["x-directives"]; got != "redis" {
			t.Errorf("%s: input_schema.redis_settings[x-directives] = %v, want \"redis\"", name, got)
		}
	}
}

// --- helpers ---

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// firstOnlyIn returns the first (in sort order) element of a that is absent
// from b.
func firstOnlyIn(a, b []string) string {
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[s] = true
	}
	cand := make([]string, 0)
	for _, s := range a {
		if !set[s] {
			cand = append(cand, s)
		}
	}
	sort.Strings(cand)
	if len(cand) == 0 {
		return ""
	}
	return cand[0]
}
