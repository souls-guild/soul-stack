package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

// writeServiceManifest — хелпер: кладёт `service.yml` в корень тестового
// serviceRoot. parallel с writeScenario.
func writeServiceManifest(t *testing.T, root, body string) {
	t.Helper()
	p := filepath.Join(root, serviceManifestFile)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile service.yml: %v", err)
	}
}

// writeMigration — хелпер: кладёт `migrations/<NNN>_to_<MMM>.yml` с пустым
// телом (content не парсится — listing работает по metadata).
func writeMigration(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, migrationsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll migrations: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

const validManifestV2 = `name: redis-cluster
state_schema_version: 2
state_schema:
  type: object
  required: [master_host, replicas]
  properties:
    master_host:
      type: string
    replicas:
      type: integer
`

// TestListStateSchema_ReadsManifest — happy-path: версия + структура +
// migrations присутствуют, в ответе всё на месте, сортировка по `to` ASC.
func TestListStateSchema_ReadsManifest(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, validManifestV2)
	writeMigration(t, root, "001_to_002.yml", "ops: []\n")
	writeMigration(t, root, "002_to_003.yml", "ops: []\n")

	info, err := ListStateSchema(root, discardLogger())
	if err != nil {
		t.Fatalf("ListStateSchema: %v", err)
	}
	if info.Version != 2 {
		t.Errorf("Version = %d, want 2", info.Version)
	}
	if info.Schema == nil {
		t.Fatal("Schema=nil, ожидалась декларация state_schema")
	}
	if got, ok := info.Schema["type"].(string); !ok || got != "object" {
		t.Errorf("Schema.type = %v, want object", info.Schema["type"])
	}
	if len(info.Migrations) != 2 {
		t.Fatalf("Migrations len = %d, want 2; %+v", len(info.Migrations), info.Migrations)
	}
	if info.Migrations[0].From != 1 || info.Migrations[0].To != 2 {
		t.Errorf("Migrations[0] = %+v", info.Migrations[0])
	}
	if info.Migrations[1].From != 2 || info.Migrations[1].To != 3 {
		t.Errorf("Migrations[1] = %+v", info.Migrations[1])
	}
	if info.Migrations[0].Path != "migrations/001_to_002.yml" {
		t.Errorf("Migrations[0].Path = %q", info.Migrations[0].Path)
	}
}

// TestListStateSchema_NoMigrationsDir — каталога `migrations/` нет; должен
// вернуться пустой список без ошибки (parity со ListScenarios для scenario/).
func TestListStateSchema_NoMigrationsDir(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, validManifestV2)

	info, err := ListStateSchema(root, discardLogger())
	if err != nil {
		t.Fatalf("ListStateSchema: %v", err)
	}
	if info.Migrations == nil {
		t.Errorf("Migrations должен быть пустым slice, не nil")
	}
	if len(info.Migrations) != 0 {
		t.Errorf("ожидался пустой список, got %+v", info.Migrations)
	}
}

// TestListStateSchema_SortByToAsc — миграции отдаются отсортированными по `to`
// (граф цепочки растёт), независимо от os.ReadDir-порядка.
func TestListStateSchema_SortByToAsc(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, validManifestV2)
	// Кладём в обратном порядке имён, чтобы убедиться, что сортировка реально работает.
	writeMigration(t, root, "003_to_004.yml", "")
	writeMigration(t, root, "001_to_002.yml", "")
	writeMigration(t, root, "002_to_003.yml", "")

	info, err := ListStateSchema(root, discardLogger())
	if err != nil {
		t.Fatalf("ListStateSchema: %v", err)
	}
	wantTo := []int{2, 3, 4}
	if len(info.Migrations) != len(wantTo) {
		t.Fatalf("Migrations len = %d, want 3", len(info.Migrations))
	}
	for i, w := range wantTo {
		if info.Migrations[i].To != w {
			t.Errorf("Migrations[%d].To = %d, want %d", i, info.Migrations[i].To, w)
		}
	}
}

// TestListStateSchema_IgnoresNonCanonicalFiles — файлы в `migrations/`, не
// подпадающие под `<NNN>_to_<MMM>.yml`, должны игнорироваться (README, тестовый
// subdir, неверный pattern).
func TestListStateSchema_IgnoresNonCanonicalFiles(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, validManifestV2)
	writeMigration(t, root, "001_to_002.yml", "")
	writeMigration(t, root, "README.md", "docs")
	writeMigration(t, root, "1_to_2.yml", "no leading zeros")
	writeMigration(t, root, "001_to_002.yaml", "wrong ext")
	// тестовый subdir миграции (docs/migrations.md: tests/<case>.yml внутри
	// `migrations/<NNN_to_MMM>/`).
	if err := os.MkdirAll(filepath.Join(root, migrationsDir, "001_to_002", "tests"), 0o755); err != nil {
		t.Fatalf("MkdirAll tests: %v", err)
	}

	info, err := ListStateSchema(root, discardLogger())
	if err != nil {
		t.Fatalf("ListStateSchema: %v", err)
	}
	if len(info.Migrations) != 1 {
		t.Fatalf("len = %d, want 1; %+v", len(info.Migrations), info.Migrations)
	}
	if info.Migrations[0].From != 1 || info.Migrations[0].To != 2 {
		t.Errorf("Migrations[0] = %+v", info.Migrations[0])
	}
}

// TestListStateSchema_MissingManifest — `service.yml` нет → error (битый
// снапшот; caller отдаёт 502).
func TestListStateSchema_MissingManifest(t *testing.T) {
	root := t.TempDir()
	_, err := ListStateSchema(root, discardLogger())
	if err == nil {
		t.Fatalf("ожидалась ошибка при отсутствии service.yml")
	}
}

// TestListStateSchema_BrokenManifest — невалидный YAML → error.
func TestListStateSchema_BrokenManifest(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, "{ this is: not valid: yaml :::\n")
	_, err := ListStateSchema(root, discardLogger())
	if err == nil {
		t.Fatalf("ожидалась ошибка при невалидном service.yml")
	}
}

// TestListStateSchema_NoStateSchemaField — manifest без `state_schema:` →
// согласно нормативной валидации это ошибка (state_schema required в MVP).
// Гарантирует, что мы не маскируем drift между UI-ответом и нормативной
// схемой.
func TestListStateSchema_NoStateSchemaField(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, `name: redis-cluster
state_schema_version: 1
`)
	_, err := ListStateSchema(root, discardLogger())
	if err == nil {
		t.Fatalf("ожидалась ошибка валидации (state_schema required)")
	}
}
