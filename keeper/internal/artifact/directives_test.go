package artifact

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// redisServiceRoot — корень реального redis-Service-репо в examples/ (относительно
// пакета keeper/internal/artifact). Каталог директив = source of truth фичи.
const redisServiceRoot = "../../../examples/service/redis"

// requireRedisExamples скипает тест, если source-tree без examples/ (custom-сборка),
// иначе возвращает АБСОЛЮТНЫЙ корень redis-сервиса (securejoin требует абсолютный
// base, как production ServiceArtifact.LocalDir). Parity со skip committed-spec-guard-а.
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

// TestLoadDirectiveCatalog_FullRealCatalog — guard #1: реальный каталог несёт все 6
// серий, известные имена присутствуют, опечатки нет.
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

	// Известная директива есть в 8.2; её опечатка — нет (guard на генератор).
	if !containsStr(cat["8.2"], "maxmemory") {
		t.Errorf("директива maxmemory отсутствует в серии 8.2")
	}
	if containsStr(cat["8.2"], "maxmemoyr") {
		t.Errorf("опечатка maxmemoyr просочилась в серию 8.2")
	}
	// Каждая серия отсортирована.
	for s, names := range cat {
		if !sort.StringsAreSorted(names) {
			t.Errorf("серия %q не отсортирована", s)
		}
	}
}

// TestLoadDirectiveCatalog_VersionNarrows — guard #2: version сужает до серии
// major.minor; чужая (8.2-only) директива отсутствует в 6.2.
func TestLoadDirectiveCatalog_VersionNarrows(t *testing.T) {
	root := requireRedisExamples(t)

	full, err := LoadDirectiveCatalog(root, "")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog(full): %v", err)
	}

	// 8.2.2 → ровно серия 8.2.
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

	// 6.2.21 → ровно серия 6.2.
	v62, err := LoadDirectiveCatalog(root, "6.2.21")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog(6.2.21): %v", err)
	}
	if len(v62) != 1 || v62["6.2"] == nil {
		t.Fatalf("version=6.2.21 → %v, want ровно {6.2}", keysOf(v62))
	}

	// Директива, что есть в 8.2, но нет в 6.2, — отсутствует в сужении 6.2.
	only82 := firstOnlyIn(full["8.2"], full["6.2"])
	if only82 == "" {
		t.Skip("не нашлось 8.2-only директивы для cross-version-проверки")
	}
	if containsStr(v62["6.2"], only82) {
		t.Errorf("8.2-only директива %q протекла в сужение 6.2", only82)
	}
}

// TestLoadDirectiveCatalog_NoCatalog — guard #3 (loader-половина): сервис без
// redis_directives (и без essence-файла) → пустой non-nil map + nil-ошибка.
func TestLoadDirectiveCatalog_NoCatalog(t *testing.T) {
	// (а) essence/_default.yaml есть, но без redis_directives.
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

	// (б) essence-файла нет вовсе → тоже пустой каталог, без ошибки.
	empty := t.TempDir()
	cat2, err := LoadDirectiveCatalog(empty, "8.2.2")
	if err != nil {
		t.Fatalf("LoadDirectiveCatalog(no file): %v", err)
	}
	if cat2 == nil || len(cat2) != 0 {
		t.Errorf("каталог без essence-файла = %v, want пустой non-nil", cat2)
	}
}

// TestLoadDirectiveCatalog_SortsNames — имена внутри серии сортируются (defensive:
// генератор мог отдать неотсортированный список).
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

// TestFilterDirectivesByVersion — юнит правила сужения (эмуляция assert-regex).
func TestFilterDirectivesByVersion(t *testing.T) {
	cat := map[string][]string{
		"6.2": {"a"},
		"7.4": {"b"},
		"8.2": {"c"},
	}
	// version="" → каталог целиком (тот же map).
	if got := FilterDirectivesByVersion(cat, ""); len(got) != 3 {
		t.Errorf("version='' → %d серий, want 3", len(got))
	}
	// distro-пин с epoch-префиксом матчит серию.
	if got := FilterDirectivesByVersion(cat, "5:7.4.1-1~deb12u7"); len(got) != 1 || got["7.4"] == nil {
		t.Errorf("epoch-пин 5:7.4.1 → %v, want {7.4}", keysOf(got))
	}
	// 7.4 не цепляет 7.04-подобное (граница серии — трейлинг-точка).
	if got := FilterDirectivesByVersion(cat, "7.42.0"); len(got) != 0 {
		t.Errorf("7.42.0 → %v, want пусто (7.4 не матчит 7.42)", keysOf(got))
	}
	// Неизвестная версия → пустой non-nil map (assert-skip-семантика).
	got := FilterDirectivesByVersion(cat, "9.9.9")
	if got == nil || len(got) != 0 {
		t.Errorf("9.9.9 → %v, want пустой non-nil", got)
	}
}

// TestListScenarios_RedisSettingsHasDirectivesAnnotation — guard #5: метка
// x-directives:redis долетает в DTO input_schema.redis_settings для create /
// create_from_souls (через covenant extends) И update_config (прямо в input).
// Иначе фронт молча перестанет валидировать директивы.
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

// firstOnlyIn возвращает первый (по сортировке) элемент a, отсутствующий в b.
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
