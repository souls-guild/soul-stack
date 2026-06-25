package artifact

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeScenario — хелпер для подкладывания scenario/<name>/main.yml внутрь
// тестового serviceRoot. Возвращает абсолютный путь к main.yml.
func writeScenario(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, scenarioDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p := filepath.Join(dir, scenarioMainFile)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestListScenarios_ReadsAllValid — три валидных scenario, сортировка по имени,
// description / input_schema / tags вычитываются.
func TestListScenarios_ReadsAllValid(t *testing.T) {
	root := t.TempDir()

	writeScenario(t, root, "create", `description: Создаёт incarnation
input_schema:
  shards:
    type: integer
  replicas:
    type: integer
tags: [create]
`)
	writeScenario(t, root, "add_replicas", `description: Добавить реплики
input:
  count:
    type: integer
`)
	writeScenario(t, root, "rolling_restart", `description: Перезапуск по очереди
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3; got = %+v", len(got), got)
	}
	// Сортировка по имени (alphabetical asc): add_replicas, create, rolling_restart.
	wantOrder := []string{"add_replicas", "create", "rolling_restart"}
	for i, n := range wantOrder {
		if got[i].Name != n {
			t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, n)
		}
	}
	// create: input_schema подхвачен (приоритет input_schema над input), tags заполнен.
	create := got[1]
	if create.Description != "Создаёт incarnation" {
		t.Errorf("create.Description = %q", create.Description)
	}
	if len(create.InputSchema) != 2 {
		t.Errorf("create.InputSchema len = %d, want 2", len(create.InputSchema))
	}
	if len(create.Tags) != 1 || create.Tags[0] != "create" {
		t.Errorf("create.Tags = %+v", create.Tags)
	}
	if create.Path != "scenario/create/main.yml" {
		t.Errorf("create.Path = %q", create.Path)
	}
	// add_replicas: top-level `input` (без _schema) — должен попасть в InputSchema.
	add := got[0]
	if len(add.InputSchema) != 1 {
		t.Errorf("add_replicas.InputSchema len = %d (input fallback не сработал)", len(add.InputSchema))
	}
	// rolling_restart: только description, остальное — пустое.
	rr := got[2]
	if rr.Description == "" {
		t.Errorf("rolling_restart.Description пустое")
	}
	if rr.InputSchema != nil {
		t.Errorf("rolling_restart.InputSchema должен быть nil, got %+v", rr.InputSchema)
	}
}

// TestListScenarios_PreferInputSchemaOverInput — если заданы оба поля,
// input_schema побеждает (это нормативное имя; `input` — fallback для свежих
// примеров).
func TestListScenarios_PreferInputSchemaOverInput(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `input_schema:
  schema_key: 1
input:
  input_key: 2
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if _, ok := got[0].InputSchema["schema_key"]; !ok {
		t.Errorf("schema_key должен победить: %+v", got[0].InputSchema)
	}
	if _, ok := got[0].InputSchema["input_key"]; ok {
		t.Errorf("input_key не должен попасть: %+v", got[0].InputSchema)
	}
}

// TestListScenarios_MissingScenarioDir — каталога scenario/ нет; должен
// вернуться пустой список без ошибки (сервис без сценариев — валидный).
func TestListScenarios_MissingScenarioDir(t *testing.T) {
	root := t.TempDir()
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ожидался пустой список, got %+v", got)
	}
}

// TestListScenarios_SkipsBrokenYAML — невалидный YAML в одном scenario не
// должен ломать listing-у остальных (partial-success).
func TestListScenarios_SkipsBrokenYAML(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "good", `description: ok
`)
	writeScenario(t, root, "bad", "{ this is: not: valid yaml :::\n")

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("ожидался только good, got %+v", got)
	}
}

// TestListScenarios_SkipsFolderWithoutMain — каталог scenario/<n> без main.yml
// пропускается (warning, без ошибки).
func TestListScenarios_SkipsFolderWithoutMain(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "good", `description: ok
`)
	// «Голая» директория без main.yml.
	if err := os.MkdirAll(filepath.Join(root, scenarioDir, "empty"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("ожидался только good, got %+v", got)
	}
}

// TestListScenarios_IgnoresFilesAtTopLevel — файл `scenario/foo.txt` рядом с
// директориями игнорируется (только субдиректории).
func TestListScenarios_IgnoresFilesAtTopLevel(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", "description: ok\n")
	if err := os.WriteFile(filepath.Join(root, scenarioDir, "README.md"), []byte("docs"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 || got[0].Name != "create" {
		t.Errorf("ожидался только create, got %+v", got)
	}
}

// TestListScenarios_IgnoresUnknownTopLevelFields — нестандартные top-level
// поля YAML (`tasks:`, `state_changes:` и т.п.) парсер игнорирует — берёт
// только name/description/input/input_schema/tags.
func TestListScenarios_IgnoresUnknownTopLevelFields(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: ok
tasks:
  - name: foo
    module: core.exec.run
state_changes:
  sets:
    key: value
random_field: 123
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Description != "ok" {
		t.Errorf("Description = %q", got[0].Description)
	}
}

// TestListScenarios_FormProjection — top-level `form:` парсится в Scenario.Form:
// секции с key/title/collapsed и полями name/label попадают в listing-проекцию.
func TestListScenarios_FormProjection(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: ok
input:
  tls_enabled: { type: boolean }
  redis_password: { type: string }
form:
  sections:
    - key: connection
      title: "Подключение"
      collapsed: false
      fields:
        - { name: tls_enabled, label: "TLS" }
    - key: secrets
      title: "Секреты"
      collapsed: true
      fields:
        - { name: redis_password, label: "Пароль Redis" }
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 || got[0].Form == nil {
		t.Fatalf("Form не распарсен: %+v", got)
	}
	f := got[0].Form
	if len(f.Sections) != 2 {
		t.Fatalf("Sections len = %d, want 2", len(f.Sections))
	}
	if f.Sections[0].Key != "connection" || f.Sections[0].Title != "Подключение" || f.Sections[0].Collapsed {
		t.Errorf("section[0] = %#v", f.Sections[0])
	}
	if f.Sections[1].Key != "secrets" || !f.Sections[1].Collapsed {
		t.Errorf("section[1] = %#v, want collapsed=true", f.Sections[1])
	}
	if f.Sections[0].Fields[0].Name != "tls_enabled" || f.Sections[0].Fields[0].Label != "TLS" {
		t.Errorf("field[0] = %#v", f.Sections[0].Fields[0])
	}
}

// TestListScenarios_FormAbsentOmitted — нет `form:` → Form==nil И поле отсутствует
// в JSON reply (omitempty, бит-в-бит как до фичи; forward-compat).
func TestListScenarios_FormAbsentOmitted(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: ok
input:
  a: { type: string }
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 || got[0].Form != nil {
		t.Fatalf("Form должен быть nil без form:, got %+v", got)
	}
	out, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), `"form"`) {
		t.Errorf("ключ \"form\" не должен присутствовать в JSON при отсутствии form:, got %s", out)
	}
}

// TestScenarioListerFunc_CompileTime — компилируемая гарантия, что Scenario
// несёт ожидаемые exported-поля для JSON-сериализации (handler полагается на
// json-tags).
func TestScenarioListerFunc_CompileTime(t *testing.T) {
	s := Scenario{
		Name:        "create",
		Path:        "scenario/create/main.yml",
		Kind:        ScenarioKindLifecycle,
		Runnable:    true,
		Description: "d",
		InputSchema: map[string]any{"k": 1},
		Tags:        []string{"a"},
	}
	_ = s
}
