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

// writeUpgrade — хелпер для подкладывания upgrade/<slug>/main.yml внутрь тестового
// serviceRoot (зеркало writeScenario для второго канала дискавери, ADR-0068).
func writeUpgrade(t *testing.T, root, slug, body string) string {
	t.Helper()
	dir := filepath.Join(root, upgradeDir, slug)
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

// TestListScenarios_ResolvesCovenantInput — ★guard на UI-баг: сценарий наследует
// input через `extends: covenant`, своя input-дельта нулевая. ListScenarios обязан
// СЛИТЬ covenant.yml.input в InputSchema (тот же add-only merge, что runtime), иначе
// UI-форма create приходит пустой (covenant-поля отсутствуют). Красный до фикса:
// loadScenario парсил сырой main.yml без covenant-резолва → InputSchema пустой.
func TestListScenarios_ResolvesCovenantInput(t *testing.T) {
	root := t.TempDir()
	// covenant.yml несёт ВЕСЬ input-контракт (как redis covenant.yml).
	writeCovenant(t, root, "covenant", `input:
  version:
    type: string
    required: true
    enum: ["8.6.1", "6.2.21"]
  redis_type:
    type: string
    default: sentinel
    enum: [sentinel, cluster]
  memory_mb:
    type: integer
    min: 64
`)
	// Дельта сценария — НУЛЕВОЙ input (всё наследуется из covenant), как
	// create_from_souls/main.yml.
	writeScenario(t, root, "create_from_souls", `name: create_from_souls
create: true
extends: covenant
tasks: []
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1; got = %+v", len(got), got)
	}
	sc := got[0]
	// covenant-поля обязаны попасть в InputSchema (иначе UI-форма пустая).
	for _, field := range []string{"version", "redis_type", "memory_mb"} {
		if _, ok := sc.InputSchema[field]; !ok {
			t.Errorf("covenant-поле %q не подмержено в InputSchema: %#v", field, sc.InputSchema)
		}
	}
	// Форма поля сохранена raw (UI читает type/enum/required напрямую).
	ver, ok := sc.InputSchema["version"].(map[string]any)
	if !ok {
		t.Fatalf("version не raw-map: %T", sc.InputSchema["version"])
	}
	if ver["type"] != "string" {
		t.Errorf("version.type = %v, want string", ver["type"])
	}
	if _, ok := ver["enum"]; !ok {
		t.Errorf("version.enum потерян после merge: %#v", ver)
	}
}

// TestListScenarios_CovenantMergeAddOnly — covenant-input и СОБСТВЕННЫЙ input
// сценария оба попадают в InputSchema (add-only union, дельта дополняет covenant).
func TestListScenarios_CovenantMergeAddOnly(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "covenant", `input:
  shared_field:
    type: string
`)
	writeScenario(t, root, "create", `name: create
extends: covenant
tasks: []
input:
  local_field:
    type: integer
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	sc := got[0]
	if _, ok := sc.InputSchema["shared_field"]; !ok {
		t.Errorf("covenant-поле shared_field потеряно: %#v", sc.InputSchema)
	}
	if _, ok := sc.InputSchema["local_field"]; !ok {
		t.Errorf("local-поле local_field потеряно: %#v", sc.InputSchema)
	}
}

// TestListScenarios_NoExtendsUnaffected — сценарий БЕЗ extends резолвится как
// прежде (covenant-резолв no-op, forward-compat бит-в-бит).
func TestListScenarios_NoExtendsUnaffected(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
tasks: []
input:
  only_field:
    type: string
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if len(got[0].InputSchema) != 1 {
		t.Errorf("без extends InputSchema должен нести только своё поле, got %#v", got[0].InputSchema)
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
// секции с key/title/collapsed/show_when и полями name/label/show_when/placeholder/
// hint попадают в listing-проекцию.
func TestListScenarios_FormProjection(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: ok
input:
  tls_enabled: { type: boolean }
  tls_port: { type: integer }
  redis_password: { type: string }
form:
  sections:
    - key: connection
      title: "Подключение"
      collapsed: false
      show_when: "input.tls_enabled"
      fields:
        - { name: tls_enabled, label: "TLS" }
        - { name: tls_port, show_when: "input.tls_enabled", placeholder: "6379", hint: "TCP-порт" }
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
	if f.Sections[0].ShowWhen != "input.tls_enabled" {
		t.Errorf("section[0].show_when = %q, want input.tls_enabled", f.Sections[0].ShowWhen)
	}
	if f.Sections[1].Key != "secrets" || !f.Sections[1].Collapsed {
		t.Errorf("section[1] = %#v, want collapsed=true", f.Sections[1])
	}
	if f.Sections[0].Fields[0].Name != "tls_enabled" || f.Sections[0].Fields[0].Label != "TLS" {
		t.Errorf("field[0] = %#v", f.Sections[0].Fields[0])
	}
	f1 := f.Sections[0].Fields[1]
	if f1.ShowWhen != "input.tls_enabled" || f1.Placeholder != "6379" || f1.Hint != "TCP-порт" {
		t.Errorf("field[1] show_when/placeholder/hint = %#v", f1)
	}
}

// TestListScenarios_FormUXKeysOmitted — поле без show_when/placeholder/hint: ключи
// отсутствуют в JSON reply (omitempty, бит-в-бит как до фичи; forward-compat).
func TestListScenarios_FormUXKeysOmitted(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: ok
input:
  a: { type: string }
form:
  sections:
    - key: s1
      fields:
        - { name: a, label: "A" }
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	out, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{`"show_when"`, `"placeholder"`, `"hint"`} {
		if strings.Contains(string(out), key) {
			t.Errorf("ключ %s не должен присутствовать без значения (omitempty), got %s", key, out)
		}
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

// TestListScenarios_CreateFlag — top-level `create: true|false` парсится в
// Scenario.Create; отсутствие ключа → false (back-compat). Дискриминатор create-
// kind для UI-фильтра «выбрать стартовый сценарий».
func TestListScenarios_CreateFlag(t *testing.T) {
	root := t.TempDir()

	writeScenario(t, root, "create", `description: дефолтный bootstrap
create: true
`)
	writeScenario(t, root, "create_cluster", `description: cluster-bootstrap
create: true
`)
	writeScenario(t, root, "add_user", `description: day-2 операция
create: false
`)
	writeScenario(t, root, "restart", `description: рестарт без флага
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	want := map[string]bool{
		"create":         true,
		"create_cluster": true,
		"add_user":       false,
		"restart":        false,
	}
	for _, s := range got {
		exp, ok := want[s.Name]
		if !ok {
			t.Fatalf("unexpected scenario %q", s.Name)
		}
		if s.Create != exp {
			t.Errorf("%s.Create = %v, want %v", s.Name, s.Create, exp)
		}
	}
}

// TestListScenarios_CreateFlagJSONOmitempty — Scenario.Create сериализуется в JSON
// под ключом `create` и опускается при false (omitempty: бит-в-бит как до фичи для
// non-create сценариев).
func TestListScenarios_CreateFlagJSONOmitempty(t *testing.T) {
	withCreate, err := json.Marshal(Scenario{Name: "create", Create: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(withCreate), `"create":true`) {
		t.Errorf("create=true scenario JSON must carry \"create\":true, got %s", withCreate)
	}
	noCreate, err := json.Marshal(Scenario{Name: "restart", Create: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(noCreate), `"create"`) {
		t.Errorf("create=false scenario JSON must omit \"create\" (omitempty), got %s", noCreate)
	}
}

// --- ListUpgrades: второй канал авто-дискавери upgrade/<slug>/ (ADR-0068 §3) ---

// TestListUpgrades_FindsUpgradeWithFrom — upgrade/<slug>/main.yml с top-level `from:`
// находится и проецируется в Scenario.FromVersions; Path указывает на upgrade/.
func TestListUpgrades_FindsUpgradeWithFrom(t *testing.T) {
	root := t.TempDir()
	writeUpgrade(t, root, "v2", `description: sentinel→cluster на v2
from: ["v1.0.0", "v1.2.0"]
tasks: []
`)

	got, err := ListUpgrades(root, discardLogger())
	if err != nil {
		t.Fatalf("ListUpgrades: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1; got = %+v", len(got), got)
	}
	up := got[0]
	if up.Name != "v2" {
		t.Errorf("Name = %q, want v2", up.Name)
	}
	if up.Path != "upgrade/v2/main.yml" {
		t.Errorf("Path = %q, want upgrade/v2/main.yml", up.Path)
	}
	want := []string{"v1.0.0", "v1.2.0"}
	if len(up.FromVersions) != len(want) || up.FromVersions[0] != want[0] || up.FromVersions[1] != want[1] {
		t.Errorf("FromVersions = %+v, want %+v", up.FromVersions, want)
	}
}

// TestListUpgrades_IgnoresNonDirs — свободный файл под upgrade/ (не каталог)
// пропускается, как и у ListScenarios.
func TestListUpgrades_IgnoresNonDirs(t *testing.T) {
	root := t.TempDir()
	writeUpgrade(t, root, "v2", `from: ["v1.0.0"]
tasks: []
`)
	// Свободный файл рядом с каталогами upgrade/ — не должен попасть в listing.
	if err := os.WriteFile(filepath.Join(root, upgradeDir, "README.md"), []byte("noise"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ListUpgrades(root, discardLogger())
	if err != nil {
		t.Fatalf("ListUpgrades: %v", err)
	}
	if len(got) != 1 || got[0].Name != "v2" {
		t.Fatalf("got = %+v, want ровно [v2]", got)
	}
}

// TestListUpgrades_MissingDir_Empty — отсутствие каталога upgrade/ → пустой список,
// НЕ ошибка (сервис без upgrade-сценариев валиден, ADR-0068 §5 legacy-путь).
func TestListUpgrades_MissingDir_Empty(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", "description: x\ntasks: []\n")

	got, err := ListUpgrades(root, discardLogger())
	if err != nil {
		t.Fatalf("ListUpgrades без upgrade/ должен вернуть nil-ошибку, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %+v, want пустой список", got)
	}
}

// TestListScenarios_IgnoresUpgradeDir — РЕГРЕСС-ГУАРД изоляции (ADR-0068 §3):
// upgrade/<slug>/ НЕ должен просачиваться в day-2 scenario-список, а scenario/ —
// в upgrade-список. Каналы строго разделены.
func TestListScenarios_IgnoresUpgradeDir(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", "description: стартовый\ntasks: []\n")
	writeUpgrade(t, root, "v2", "from: [\"v1.0.0\"]\ntasks: []\n")

	scenarios, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(scenarios) != 1 || scenarios[0].Name != "create" {
		t.Fatalf("ListScenarios = %+v, want ровно [create] (upgrade/ не должен просачиваться)", scenarios)
	}

	upgrades, err := ListUpgrades(root, discardLogger())
	if err != nil {
		t.Fatalf("ListUpgrades: %v", err)
	}
	if len(upgrades) != 1 || upgrades[0].Name != "v2" {
		t.Fatalf("ListUpgrades = %+v, want ровно [v2] (scenario/ не должен просачиваться)", upgrades)
	}
}

// TestListScenarios_StrayFromNotProjected — ФИЗИЧЕСКИЙ гейт изоляции поля (ADR-0068
// §3): стрэй top-level `from:` в scenario/<name>/main.yml НЕ просачивается в day-2
// reply — FromVersions заполняется только на upgrade/-канале (dir==upgradeDir), а не
// по каталогу-косвенно. Регресс на случай, если оператор случайно напишет `from:` в
// обычном сценарии.
func TestListScenarios_StrayFromNotProjected(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: стартовый
from: ["v1.0.0"]
tasks: []
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].FromVersions != nil {
		t.Fatalf("scenario/-запись не должна нести FromVersions (стрэй from: просочился): %+v", got[0].FromVersions)
	}

	// Тот же стрэй-guard симметрично: upgrade/-канал ДОЛЖЕН нести from:.
	writeUpgrade(t, root, "v2", "from: [\"v1.0.0\"]\ntasks: []\n")
	ups, err := ListUpgrades(root, discardLogger())
	if err != nil {
		t.Fatalf("ListUpgrades: %v", err)
	}
	if len(ups) != 1 || len(ups[0].FromVersions) != 1 || ups[0].FromVersions[0] != "v1.0.0" {
		t.Fatalf("upgrade/-запись должна нести FromVersions=[v1.0.0], got %+v", ups)
	}
}
