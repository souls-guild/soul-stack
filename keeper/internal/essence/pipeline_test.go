package essence

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeLayers материализует слои в tmp service-dir по РЕАЛЬНОЙ раскладке
// (docs/service/manifest.md): ключ — relative-path внутри `essence/`
// (`_default.yaml`, `os/<family>.yaml`, `coven/<метка>.yaml`), значение —
// содержимое YAML. Файлы кладутся под `<dir>/essence/<rel>` — резолвер обязан
// читать из поддиректории essence/, а не из корня сервиса (BUG 2).
func writeLayers(t *testing.T, layers map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range layers {
		full := filepath.Join(dir, "essence", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// TestResolve_ReadsFromEssenceSubdir — регрессия BUG 2: резолвер обязан читать
// слои из `<ServiceDir>/essence/`, НЕ из корня снапшота. Кладём _default.yaml в
// ОБА места с разным содержимым; ожидаем значение из essence/ (root-файл
// игнорируется). До фикса резолвер читал root и тест бы провалился.
func TestResolve_ReadsFromEssenceSubdir(t *testing.T) {
	dir := t.TempDir()
	// Корень сервиса — НЕ источник essence: значение-ловушка.
	if err := os.WriteFile(filepath.Join(dir, "_default.yaml"), []byte("key: ROOT_TRAP\n"), 0o644); err != nil {
		t.Fatalf("write root trap: %v", err)
	}
	// Реальная раскладка — essence/_default.yaml.
	if err := os.MkdirAll(filepath.Join(dir, "essence"), 0o755); err != nil {
		t.Fatalf("mkdir essence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "essence", "_default.yaml"), []byte("key: from_essence\n"), 0o644); err != nil {
		t.Fatalf("write essence/_default.yaml: %v", err)
	}

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["key"] != "from_essence" {
		t.Fatalf("essence читается не из essence/: got=%#v (root-файл не должен учитываться)", got["key"])
	}
}

func TestResolve_FourLayerPrecedence(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "key: default\nonly_default: 1\n",
		"os/debian.yaml": "key: os\nonly_os: 2\n",
		"coven/web.yaml": "key: coven\nonly_coven: 3\n",
	})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{
		ServiceDir:      dir,
		OSFamily:        "debian",
		Covens:          []string{"web"},
		IncarnationSpec: map[string]any{"key": "spec", "only_spec": 4},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := map[string]any{
		"key":          "spec", // самый сильный слой
		"only_default": uint64(1),
		"only_os":      uint64(2),
		"only_coven":   uint64(3),
		"only_spec":    4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("precedence mismatch:\n got=%#v\nwant=%#v", got, want)
	}
}

func TestResolve_DeepMergeMaps(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml": "redis:\n  maxmemory: 100mb\n  appendonly: true\n",
		"os/rhel.yaml":  "redis:\n  maxmemory: 200mb\n  bind: 0.0.0.0\n",
	})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, OSFamily: "rhel"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	redis, ok := got["redis"].(map[string]any)
	if !ok {
		t.Fatalf("redis не map: %#v", got["redis"])
	}
	want := map[string]any{
		"maxmemory":  "200mb", // override
		"appendonly": true,    // сохранён из default
		"bind":       "0.0.0.0",
	}
	if !reflect.DeepEqual(redis, want) {
		t.Fatalf("deep-merge mismatch:\n got=%#v\nwant=%#v", redis, want)
	}
}

func TestResolve_ListsReplaceNotAppend(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "ports:\n  - 6379\n  - 16379\n",
		"os/debian.yaml": "ports:\n  - 7000\n",
	})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, OSFamily: "debian"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ports, ok := got["ports"].([]any)
	if !ok {
		t.Fatalf("ports не список: %#v", got["ports"])
	}
	want := []any{uint64(7000)}
	if !reflect.DeepEqual(ports, want) {
		t.Fatalf("список должен заменяться, а не дополняться:\n got=%#v\nwant=%#v", ports, want)
	}
}

func TestResolve_ScalarOverMapAndViceVersa(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "a:\n  nested: 1\nb: scalar\n",
		"os/debian.yaml": "a: now_scalar\nb:\n  now: map\n",
	})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, OSFamily: "debian"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["a"] != "now_scalar" {
		t.Fatalf("map должен замениться скаляром: %#v", got["a"])
	}
	bm, ok := got["b"].(map[string]any)
	if !ok || bm["now"] != "map" {
		t.Fatalf("скаляр должен замениться map: %#v", got["b"])
	}
}

func TestResolve_MultipleCovensDeterministicOrder(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "tier: base\n",
		"coven/aaa.yaml": "tier: aaa\nfrom_aaa: 1\n",
		"coven/zzz.yaml": "tier: zzz\nfrom_zzz: 1\n",
	})

	r := NewResolver(nil)
	// Передаём covens в обратном порядке — результат должен зависеть от
	// сортировки имён, не от порядка во входе.
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, Covens: []string{"zzz", "aaa"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// zzz сортируется после aaa → zzz wins.
	if got["tier"] != "zzz" {
		t.Fatalf("ожидался zzz (последний по сортировке), got=%#v", got["tier"])
	}
	if got["from_aaa"] != uint64(1) || got["from_zzz"] != uint64(1) {
		t.Fatalf("оба coven-слоя должны примениться: %#v", got)
	}
}

func TestResolve_MissingLayersOK(t *testing.T) {
	// Нет ни одного файла-слоя, есть только spec.
	dir := writeLayers(t, map[string]string{})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{
		ServiceDir:      dir,
		OSFamily:        "debian",           // os/debian.yaml нет
		Covens:          []string{"absent"}, // coven/absent.yaml нет
		IncarnationSpec: map[string]any{"only": "spec"},
	})
	if err != nil {
		t.Fatalf("отсутствие слоёв не должно быть ошибкой: %v", err)
	}
	want := map[string]any{"only": "spec"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
}

func TestResolve_EmptyEverything(t *testing.T) {
	dir := writeLayers(t, map[string]string{})
	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ожидался пустой essence, got=%#v", got)
	}
}

func TestResolve_EmptyDefaultFile(t *testing.T) {
	// Пустой _default.yaml (yaml.Unmarshal → nil map) не должен ломать merge.
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "",
		"os/debian.yaml": "key: os\n",
	})
	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, OSFamily: "debian"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["key"] != "os" {
		t.Fatalf("got=%#v", got)
	}
}

func TestResolve_InvalidYAMLErrors(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml": "key: : : broken\n  - bad",
	})
	r := NewResolver(nil)
	if _, err := r.Resolve(ResolveInput{ServiceDir: dir}); err == nil {
		t.Fatal("ожидалась ошибка парсинга невалидного YAML")
	}
}

func TestResolve_EmptyOSFamilySkipsOSLayer(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"_default.yaml":  "key: default\n",
		"os/debian.yaml": "key: os\n",
	})
	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{ServiceDir: dir, OSFamily: ""})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["key"] != "default" {
		t.Fatalf("пустой OSFamily должен пропускать os-слой, got=%#v", got["key"])
	}
}

func TestResolve_SecureJoinClampsTraversal(t *testing.T) {
	// Файл вне serviceDir не должен читаться: securejoin клампит `..` к корню,
	// итоговый путь несуществующий → слой пропускается без ошибки.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.yaml"), []byte("leaked: true\n"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	dir := writeLayers(t, map[string]string{"_default.yaml": "key: default\n"})

	r := NewResolver(nil)
	got, err := r.Resolve(ResolveInput{
		ServiceDir: dir,
		Covens:     []string{"../../" + filepath.Base(outside) + "/secret"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, leaked := got["leaked"]; leaked {
		t.Fatal("securejoin не сдержал traversal: прочитан файл вне serviceDir")
	}
	if got["key"] != "default" {
		t.Fatalf("got=%#v", got)
	}
}

func TestResolve_DoesNotMutateIncarnationSpecNested(t *testing.T) {
	// Spec оператора хранится в БД отдельно (architecture.md): Resolve не должен
	// мутировать переданный IncarnationSpec.
	dir := writeLayers(t, map[string]string{
		"_default.yaml": "redis:\n  maxmemory: 100mb\n  bind: 127.0.0.1\n",
	})
	spec := map[string]any{"redis": map[string]any{"maxmemory": "500mb"}}

	r := NewResolver(nil)
	if _, err := r.Resolve(ResolveInput{ServiceDir: dir, IncarnationSpec: spec}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	nested := spec["redis"].(map[string]any)
	if _, polluted := nested["bind"]; polluted {
		t.Fatalf("Resolve протёк base-ключ в IncarnationSpec: %#v", spec)
	}
	if len(nested) != 1 || nested["maxmemory"] != "500mb" {
		t.Fatalf("IncarnationSpec изменён: %#v", spec)
	}
}
