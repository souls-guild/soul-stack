package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// writeCovenant кладёт `<name>.yml` covenant-фрагмент в корень тестового
// serviceRoot (сиблинг types.yml/scenario/), как его ищет config.ResolveScenarioCovenant.
func writeCovenant(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name+".yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile covenant %s: %v", name, err)
	}
}

// loadResolved — обёртка: читает scenario/<name>/main.yml из снапшота root через
// тот же путь, что runtime-callers (ReadFile → LoadScenarioManifestResolved).
func loadResolved(t *testing.T, root, scenario string) (*config.ScenarioManifest, []diag.Diagnostic) {
	t.Helper()
	rel := filepath.ToSlash(filepath.Join(scenarioDir, scenario, scenarioMainFile))
	art := &ServiceArtifact{LocalDir: root}
	data, err := readSnapshotFile(root, rel)
	if err != nil {
		t.Fatalf("readSnapshotFile %s: %v", rel, err)
	}
	scn, _, diags, err := LoadScenarioManifestResolved(art, rel, data)
	if err != nil {
		t.Fatalf("LoadScenarioManifestResolved %s: %v", scenario, err)
	}
	return scn, diags
}

// diagCodes собирает коды error-уровня диагностик для проверок.
func diagCodes(ds []diag.Diagnostic) []string {
	var out []string
	for _, d := range ds {
		if d.Level == diag.LevelError {
			out = append(out, d.Code)
		}
	}
	return out
}

func hasCode(ds []diag.Diagnostic, code string) bool {
	for _, c := range diagCodes(ds) {
		if c == code {
			return true
		}
	}
	return false
}

// extends → covenant.yml резолвится и сливается: смерженный manifest несёт И
// covenant-поля (input/compute), И собственные поля сценария.
func TestResolveCovenant_MergesInputAndCompute(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  cluster_name:
    type: string
compute:
  shared_prefix: "${ input.cluster_name }"
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  replicas:
    type: integer
compute:
  local_label: "local"
`)

	scn, diags := loadResolved(t, root, "create")
	if diag.HasErrors(diags) {
		t.Fatalf("неожиданные ошибки резолва: %v", diagCodes(diags))
	}
	if _, ok := scn.Input["cluster_name"]; !ok {
		t.Errorf("covenant-input cluster_name не подмержен: %#v", scn.Input)
	}
	if _, ok := scn.Input["replicas"]; !ok {
		t.Errorf("local-input replicas потерян: %#v", scn.Input)
	}
	if len(scn.Compute) != 2 {
		t.Fatalf("ожидалось 2 compute (covenant+local), got %d: %#v", len(scn.Compute), scn.Compute)
	}
	// covenant-compute идёт ПЕРВЫМ (общий контракт предшествует дельте).
	if scn.Compute[0].Name != "shared_prefix" {
		t.Errorf("covenant-compute должен быть первым, got %q", scn.Compute[0].Name)
	}
	if scn.Compute[1].Name != "local_label" {
		t.Errorf("local-compute должен быть вторым, got %q", scn.Compute[1].Name)
	}
}

// covenant-input, несущий $type-ссылку, резолвится после merge (covenant ДО
// $type-резолва): смерженное поле получает тело типа + x-type, не остаётся сырым.
func TestResolveCovenant_CovenantInputTypeRefResolved(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
`)
	writeCovenant(t, root, "base", `input:
  target:
    $type: Endpoint
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
`)

	scn, diags := loadResolved(t, root, "create")
	if diag.HasErrors(diags) {
		t.Fatalf("неожиданные ошибки: %v", diagCodes(diags))
	}
	target, ok := scn.Input["target"]
	if !ok {
		t.Fatalf("covenant-input target не подмержен: %#v", scn.Input)
	}
	// covenant-input прошёл $type-резолв ПОСЛЕ merge: узел типизирован
	// (Type=object + Properties), а не остался untyped ссылкой ($type → пустой
	// Type, проверка формы submitted-input пропускалась бы молча).
	if target.Type != "object" {
		t.Errorf("covenant-поле target.Type = %q, ожидался object (резолв $type после merge не сработал)", target.Type)
	}
	if _, ok := target.Properties["host"]; !ok {
		t.Errorf("covenant-поле target.Properties.host отсутствует после резолва: %#v", target.Properties)
	}
}

// section_key_conflict: одно имя input-поля и в covenant, и в сценарии → load-
// диагностика с кодом section_key_conflict и понятным текстом (covenant-имя + ключ).
func TestResolveCovenant_SectionKeyConflict(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  replicas:
    type: integer
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  replicas:
    type: string
`)

	_, diags := loadResolved(t, root, "create")
	if !hasCode(diags, "section_key_conflict") {
		t.Fatalf("ожидался section_key_conflict, коды: %v", diagCodes(diags))
	}
	var msg string
	for _, d := range diags {
		if d.Code == "section_key_conflict" {
			msg = d.Message
		}
	}
	if !strings.Contains(msg, "base") || !strings.Contains(msg, "replicas") {
		t.Errorf("текст конфликта должен нести covenant-имя и ключ, got %q", msg)
	}
}

// extends на отсутствующий covenant-файл → covenant_extends_target_not_found.
func TestResolveCovenant_TargetNotFound(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
extends: nope
tasks: []
`)

	_, diags := loadResolved(t, root, "create")
	if !hasCode(diags, "covenant_extends_target_not_found") {
		t.Fatalf("ожидался covenant_extends_target_not_found, коды: %v", diagCodes(diags))
	}
}

// extends с недопустимым именем (path-traversal/разделитель) → covenant_extends_invalid,
// файловый резолв не делается (имя не проходит грамматику).
func TestResolveCovenant_InvalidExtendsName(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
extends: "../escape"
tasks: []
`)

	_, diags := loadResolved(t, root, "create")
	// config-валидатор сам поднимает diag на невалидный extends в schema-фазе;
	// резолвер дополнительно фейл-клоузит covenant_extends_invalid. Достаточно,
	// что есть ошибка и НЕТ попытки «не найден» (имя отвергнуто до файла).
	if !diag.HasErrors(diags) {
		t.Fatalf("невалидный extends должен дать ошибку, коды: %v", diagCodes(diags))
	}
	if hasCode(diags, "covenant_extends_target_not_found") {
		t.Errorf("невалидное имя не должно доходить до файлового резолва: %v", diagCodes(diags))
	}
}

// Cross-form state_changes: covenant в list-форме, сценарий в map-форме (или
// наоборот) → state_changes_form_mismatch (S1-merge кросс-форменно не детектит).
func TestResolveCovenant_StateChangesFormMismatch(t *testing.T) {
	root := t.TempDir()
	// covenant — list-форма state_changes.
	writeCovenant(t, root, "base", `state_changes:
  - set: shared_field
    value: "${ input.x }"
`)
	// сценарий — map-форма (deprecated Sets).
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  x:
    type: string
state_changes:
  sets:
    local_field: "${ input.x }"
`)

	_, diags := loadResolved(t, root, "create")
	if !hasCode(diags, "state_changes_form_mismatch") {
		t.Fatalf("ожидался state_changes_form_mismatch, коды: %v", diagCodes(diags))
	}
}

// Сценарий БЕЗ extends → forward-compat: covenant-резолв no-op, manifest бит-в-бит
// как без фичи (диагностик covenant нет, input/compute остаются собственными).
func TestResolveCovenant_NoExtendsForwardCompat(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
tasks: []
input:
  replicas:
    type: integer
compute:
  local_label: "local"
`)

	scn, diags := loadResolved(t, root, "create")
	if diag.HasErrors(diags) {
		t.Fatalf("без extends не должно быть ошибок, коды: %v", diagCodes(diags))
	}
	for _, c := range diagCodes(diags) {
		if strings.HasPrefix(c, "covenant") || c == "section_key_conflict" || c == "state_changes_form_mismatch" {
			t.Errorf("covenant-диагностика без extends недопустима: %s", c)
		}
	}
	if len(scn.Input) != 1 {
		t.Errorf("input без extends должен нести только своё поле, got %#v", scn.Input)
	}
	if len(scn.Compute) != 1 {
		t.Errorf("compute без extends должен нести только своё, got %#v", scn.Compute)
	}
}

// ★Свежий-декод: повторный LoadScenarioManifestResolved одного scenario НЕ
// накапливает covenant-секции. Мутация смерженного manifest не липнет к кэшу,
// т.к. manifest рождается из байтов на каждый вызов (кэш держит файлы снапшота).
func TestResolveCovenant_RepeatedLoadNoAccumulation(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  cluster_name:
    type: string
compute:
  shared_prefix: "p"
validate:
  - that: "true"
    message: "shared invariant"
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  replicas:
    type: integer
compute:
  local_label: "local"
`)

	first, d1 := loadResolved(t, root, "create")
	if diag.HasErrors(d1) {
		t.Fatalf("первый Load: %v", diagCodes(d1))
	}
	second, d2 := loadResolved(t, root, "create")
	if diag.HasErrors(d2) {
		t.Fatalf("второй Load: %v", diagCodes(d2))
	}

	if len(first.Input) != len(second.Input) {
		t.Errorf("input накопился между загрузками: 1st=%d 2nd=%d", len(first.Input), len(second.Input))
	}
	if len(first.Compute) != len(second.Compute) {
		t.Errorf("compute накопился: 1st=%d 2nd=%d", len(first.Compute), len(second.Compute))
	}
	if len(first.Validate) != len(second.Validate) {
		t.Errorf("validate накопился: 1st=%d 2nd=%d", len(first.Validate), len(second.Validate))
	}
	// Абсолютные ожидания (ловит даже синхронное удвоение в ОБОИХ).
	if len(second.Compute) != 2 {
		t.Errorf("compute должен быть covenant(1)+local(1)=2, got %d", len(second.Compute))
	}
	if len(second.Validate) != 1 {
		t.Errorf("validate должен быть ровно covenant(1), got %d", len(second.Validate))
	}
}

// countCode — число error-диагностик с данным кодом (для точного «ровно один»).
func countCode(ds []diag.Diagnostic, code string) int {
	n := 0
	for _, d := range ds {
		if d.Level == diag.LevelError && d.Code == code {
			n++
		}
	}
	return n
}

// ★Пост-merge form ВИДИТ covenant-input (guard на центральный новый код
// resolveCovenantFormDiags): covenant несёт input-поле, сценарий объявляет
// form-field на него → ноль form-ОШИБОК. До переноса на пост-merge стадию form
// гейтился в semantic-фазе ДО merge — covenant-поля там не было, и этот же form
// дал бы ложный form_field_unknown. Тест доказывает, что проверка идёт на
// СМЕРЖЕННОМ input.
func TestResolveCovenant_FormSeesCovenantInput(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  cluster_name:
    type: string
`)
	// form-секция ссылается на covenant-поле cluster_name; собственного input у
	// сценария нет — единственный form-field покрыт ровно covenant-полем.
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
form:
  sections:
    - key: main
      title: Main
      fields:
        - name: cluster_name
`)

	_, diags := loadResolved(t, root, "create")
	if hasCode(diags, "form_field_unknown") {
		t.Errorf("form_field_unknown на covenant-поле — пост-merge form не увидел смерженный input: %v", diagCodes(diags))
	}
	if diag.HasErrors(diags) {
		t.Errorf("ноль form-ошибок ожидалось, коды: %v", diagCodes(diags))
	}
}

// ★Реверс-guard «валидатор НЕ ослаб после переноса на пост-merge стадию»: form-field
// на имя, которого НЕТ ни в covenant, ни в local input → РОВНО ОДИН form_field_unknown
// пост-merge. Второе поле формы ссылается на covenant-input (cluster_name) и обязано
// пройти — так доказывается, что unknown эмитится именно на реально-отсутствующее
// поле, а не на covenant-поле (т.е. валидатор и видит covenant-input, и продолжает
// ловить мусор).
func TestResolveCovenant_FormFieldUnknownPostMerge(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  cluster_name:
    type: string
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  replicas:
    type: integer
form:
  sections:
    - key: main
      title: Main
      fields:
        - name: cluster_name
        - name: replicas
        - name: ghost_field
`)

	_, diags := loadResolved(t, root, "create")
	if n := countCode(diags, "form_field_unknown"); n != 1 {
		t.Fatalf("ожидался ровно один form_field_unknown (на ghost_field), got %d; коды: %v", n, diagCodes(diags))
	}
	// Доказываем адресность: covenant-поле НЕ дало ложный unknown (иначе их было бы >1,
	// но проверим явно — текст несёт ghost_field, не cluster_name/replicas).
	for _, d := range diags {
		if d.Code == "form_field_unknown" && !strings.Contains(d.Message, "ghost_field") {
			t.Errorf("form_field_unknown указал не на ghost_field: %q", d.Message)
		}
	}
}

// ★Cross-scenario независимость (S1-review finding): два РАЗНЫХ сценария с одним
// covenant.yml резолвятся независимо — резолв scenario-A не влияет на scenario-B
// (свежий fragment per-scenario, read-only fragment-контракт).
func TestResolveCovenant_TwoScenariosOneCovenantIndependent(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "base", `input:
  cluster_name:
    type: string
`)
	writeScenario(t, root, "create", `name: create
extends: base
tasks: []
input:
  a_only:
    type: integer
`)
	writeScenario(t, root, "migrate_cluster", `name: migrate_cluster
extends: base
tasks: []
input:
  b_only:
    type: integer
`)

	a, da := loadResolved(t, root, "create")
	b, db := loadResolved(t, root, "migrate_cluster")
	if diag.HasErrors(da) || diag.HasErrors(db) {
		t.Fatalf("ошибки резолва: A=%v B=%v", diagCodes(da), diagCodes(db))
	}
	// Каждый видит covenant + только своё поле, чужое НЕ протекло.
	if _, ok := a.Input["a_only"]; !ok {
		t.Errorf("scenario A потерял своё поле: %#v", a.Input)
	}
	if _, leaked := a.Input["b_only"]; leaked {
		t.Errorf("в scenario A протекло поле scenario B (cross-scenario aliasing): %#v", a.Input)
	}
	if _, ok := b.Input["b_only"]; !ok {
		t.Errorf("scenario B потерял своё поле: %#v", b.Input)
	}
	if _, leaked := b.Input["a_only"]; leaked {
		t.Errorf("в scenario B протекло поле scenario A (cross-scenario aliasing): %#v", b.Input)
	}
}
