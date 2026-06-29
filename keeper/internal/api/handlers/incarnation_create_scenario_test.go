package handlers

// Guard-тесты механизма нескольких create-сценариев (Вариант A) на handler-слое:
// POST /v1/incarnations с полем `create_scenario` (CreateTyped). Покрывают стык,
// которого не было в *_test.go (составные куски — ResolveCreateScenarios /
// ValidateCreateScenarioChoice / ValidateInput — покрыты в scenario-пакете, e2e
// через handler с реальным снапшотом отсутствовал):
//
//   (a) валидный create-kind выбор → стартует ИМЕННО выбранный сценарий
//       (RunSpec.ScenarioName) + пишет incarnation.created_scenario (INSERT $12);
//   (b) невалидное / non-create имя → 422 create_scenario_invalid
//       (ErrCreateScenarioNotEligible), incarnation НЕ создаётся, прогон НЕ стартует;
//   (c) input валидируется против ВЫБРАННОГО сценария (его required-поля), а не
//       против дефолтного `create`.
//
// Снапшот сервиса материализуется на диск (temp): ResolveCreateScenarios сканирует
// art.LocalDir (artifact.ListScenarios), а ValidateInput читает scenario/<chosen>/
// main.yml — обе фазы видят один снапшот через fakeLoader{localDir}.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// createScenarioSnapshot пишет temp-снапшот сервиса с тремя сценариями и
// возвращает его корень (для fakeLoader.localDir):
//
//   - scenario/create/main.yml   — create: true, input БЕЗ required-полей;
//   - scenario/restore/main.yml  — create: true, input с required `backup_id`
//     (без default) — выбираемый стартовый сценарий с ОТЛИЧНОЙ от create схемой;
//   - scenario/add_user/main.yml — operational (нет create:) — НЕ eligible.
//
// Фаза 2: `create` тоже помечен `create: true` — имя больше не привилегировано,
// в набор попадают РОВНО `create: true`-сценарии (здесь {create, restore}).
//
// t.TempDir авто-чистится. Файлы минимальны (input + tasks: []), чтобы
// config.LoadScenarioManifestFromBytes парсил без diag-ошибок.
func createScenarioSnapshot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(name, yaml string) {
		dir := filepath.Join(root, "scenario", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte(yaml), 0o644); err != nil {
			t.Fatalf("write %s/main.yml: %v", name, err)
		}
	}
	write("create", `name: create
create: true
input:
  replicas:
    type: integer
    default: 1
tasks: []
`)
	write("restore", `name: restore
create: true
input:
  backup_id:
    type: string
    required: true
tasks: []
`)
	write("add_user", `name: add_user
input:
  user:
    type: string
    required: true
tasks: []
`)
	return root
}

// newCreateScenarioHandler собирает handler с runner+services+loader (полный
// create-путь: runScenario=true, loader!=nil). starter перехватывает RunSpec.
func newCreateScenarioHandler(t *testing.T, db *fakeIncDB, starter *fakeStarter) *IncarnationHandler {
	t.Helper()
	loader := &fakeLoader{localDir: createScenarioSnapshot(t)}
	return NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
}

// bareScenarioSnapshot пишет снапшот сервиса БЕЗ единого create-сценария (только
// operational restart) и возвращает корень. Набор create-сценариев пуст →
// bare-инкарнация.
func bareScenarioSnapshot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "scenario", "restart")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte("name: restart\ntasks: []\n"), 0o644); err != nil {
		t.Fatalf("write restart/main.yml: %v", err)
	}
	return root
}

// TestIncarnation_Create_BareNoScenario_ReadyNoRun — GUARD Фаза 2: сервис БЕЗ
// create-сценариев + пустой create_scenario → bare-инкарнация: 202, incarnation
// создана (insert), прогон НЕ стартует, apply_id ОТСУТСТВУЕТ, created_scenario col
// ($12) = NULL. Главный инвариант bare-ветки.
func TestIncarnation_Create_BareNoScenario_ReadyNoRun(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	loader := &fakeLoader{localDir: bareScenarioSnapshot(t)}
	h := NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-bare","service":"redis"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1 (bare создаётся)", db.insertCalls)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0 (bare без прогона)", starter.calls)
	}
	// created_scenario col ($12) = NULL (nil) — bare несёт NULL, не 'create'.
	if db.insertArgs[11] != nil {
		t.Errorf("INSERT created_scenario ($12) = %v, want nil (NULL для bare)", db.insertArgs[11])
	}
	// apply_id отсутствует в JSON (bare без прогона).
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, has := raw["apply_id"]; has {
		t.Errorf("apply_id присутствует для bare-инкарнации: %v", raw)
	}
}

// (a) валидный выбор не-дефолтного create-сценария --------------------------

// TestIncarnation_Create_ChosenScenario_Starts_AndPersisted — create_scenario:
// restore (create:true) → стартует restore (НЕ дефолтный create) + INSERT несёт
// created_scenario=restore ($12). Регресс = выбор оператора игнорируется, всегда
// запускается create.
func TestIncarnation_Create_ChosenScenario_Starts_AndPersisted(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"restore","input":{"backup_id":"b-001"}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Fatalf("starter.calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "restore" {
		t.Errorf("RunSpec.ScenarioName = %q, want restore (выбранный, НЕ дефолтный create)", starter.gotSpec.ScenarioName)
	}
	// created_scenario — $12 INSERT-а (insertArgs[11]); см. insertSQL crud.go.
	if len(db.insertArgs) < 12 {
		t.Fatalf("insertArgs len = %d, want ≥12", len(db.insertArgs))
	}
	if got, _ := db.insertArgs[11].(string); got != "restore" {
		t.Errorf("INSERT created_scenario ($12) = %q, want restore", got)
	}
}

// TestIncarnation_Create_EmptyChoice_HasScenarios_422 — Фаза 2: сервис ПРЕДЛАГАЕТ
// create-сценарии ({create, restore}), но выбор пуст → 422 create_scenario_required
// с перечислением годных; incarnation НЕ создаётся, прогон НЕ стартует. Регресс =
// вернулся back-compat-дефолт `create` при пустом выборе.
func TestIncarnation_Create_EmptyChoice_HasScenarios_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeValidationFailed {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeValidationFailed)
	}
	if !bytes.Contains([]byte(p.Detail), []byte("create_scenario_required")) {
		t.Errorf("detail = %q, want содержит create_scenario_required", p.Detail)
	}
	// Перечисление годных сценариев — оператору видно, что выбрать.
	if !bytes.Contains([]byte(p.Detail), []byte("create")) || !bytes.Contains([]byte(p.Detail), []byte("restore")) {
		t.Errorf("detail = %q, want список {create, restore}", p.Detail)
	}
	if db.insertCalls != 0 || starter.calls != 0 {
		t.Errorf("insertCalls/starter.calls = %d/%d, want 0/0", db.insertCalls, starter.calls)
	}
}

// TestIncarnation_Create_ExplicitCreate_Starts — явный create_scenario=create
// (помечен create:true) → стартует create, created_scenario=create. Контраст к
// _EmptyChoice_HasScenarios_422: имя `create` валидно как ЯВНЫЙ выбор.
func TestIncarnation_Create_ExplicitCreate_Starts(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"create"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	if starter.gotSpec.ScenarioName != "create" {
		t.Errorf("RunSpec.ScenarioName = %q, want create", starter.gotSpec.ScenarioName)
	}
	if got, _ := db.insertArgs[11].(string); got != "create" {
		t.Errorf("INSERT created_scenario ($12) = %q, want create", got)
	}
}

// (b) невалидное / non-create имя → 422 create_scenario_invalid ---------------

// TestIncarnation_Create_NonCreateScenario_422 — operational-сценарий (add_user,
// нет create:true) как create_scenario → 422 create_scenario_invalid; incarnation
// НЕ создаётся, прогон НЕ стартует. Регресс = можно «создать» инкарнацию
// operational-сценарием в обход bootstrap.
func TestIncarnation_Create_NonCreateScenario_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"add_user","input":{"user":"bob"}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if p.Type != problem.TypeValidationFailed {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeValidationFailed)
	}
	// detail несёт маркер create_scenario_invalid (handler-маппинг ErrCreateScenarioNotEligible).
	if !bytes.Contains([]byte(p.Detail), []byte("create_scenario_invalid")) {
		t.Errorf("detail = %q, want содержит create_scenario_invalid", p.Detail)
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (incarnation НЕ создаётся)", db.insertCalls)
	}
	if starter.calls != 0 {
		t.Errorf("starter.calls = %d, want 0 (прогон НЕ стартует)", starter.calls)
	}
}

// TestIncarnation_Create_UnknownScenario_422 — несуществующий create_scenario →
// 422 (не в наборе): тот же класс отказа, что и operational.
func TestIncarnation_Create_UnknownScenario_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"nonexistent"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 || starter.calls != 0 {
		t.Errorf("insertCalls/starter.calls = %d/%d, want 0/0", db.insertCalls, starter.calls)
	}
}

// TestIncarnation_Create_TraversalScenario_422 — мусорное имя (path-traversal по
// ScenarioNamePattern) отбивается ДО резолва набора (не подставляем в путь) → 422.
func TestIncarnation_Create_TraversalScenario_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"../../etc/passwd"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 || starter.calls != 0 {
		t.Errorf("insertCalls/starter.calls = %d/%d, want 0/0", db.insertCalls, starter.calls)
	}
}

// (c) input валидируется против ВЫБРАННОГО сценария --------------------------

// TestIncarnation_Create_InputValidatedAgainstChosen_Missing_422 — выбран restore
// (required backup_id), но input пуст → 422 input_invalid. Доказывает, что схема
// берётся у ВЫБРАННОГО сценария (у дефолтного create такого required-поля нет).
func TestIncarnation_Create_InputValidatedAgainstChosen_Missing_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"restore"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422 (restore требует backup_id), body=%s", rec.Code, rec.Body.String())
	}
	var p problem.Details
	_ = json.NewDecoder(rec.Body).Decode(&p)
	if !bytes.Contains([]byte(p.Detail), []byte("input_invalid")) {
		t.Errorf("detail = %q, want содержит input_invalid", p.Detail)
	}
	if db.insertCalls != 0 || starter.calls != 0 {
		t.Errorf("insertCalls/starter.calls = %d/%d, want 0/0 (отказ ДО мутации)", db.insertCalls, starter.calls)
	}
}

// TestIncarnation_Create_ChosenCreate_EmptyInputOK — КОНТРАСТ к
// _InputValidatedAgainstChosen_Missing_422: тот же пустой input, но выбран create
// (его схема НЕ требует backup_id) → 202. Пара тестов фиксирует: схема резолвится
// по ВЫБРАННОМУ сценарию, не по статике.
func TestIncarnation_Create_ChosenCreate_EmptyInputOK(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"create"}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Code = %d, want 202 (create НЕ требует backup_id), body=%s", rec.Code, rec.Body.String())
	}
}

// TestIncarnation_Create_ChosenScenario_BadInputType_422 — выбран restore,
// backup_id передан числом (схема: string) → 422 input_invalid (type-mismatch у
// схемы выбранного сценария).
func TestIncarnation_Create_ChosenScenario_BadInputType_422(t *testing.T) {
	db := &fakeIncDB{}
	starter := &fakeStarter{}
	h := newCreateScenarioHandler(t, db, starter)

	req := httptest.NewRequest(http.MethodPost, "/v1/incarnations",
		bytes.NewReader([]byte(`{"name":"redis-prod","service":"redis","create_scenario":"restore","input":{"backup_id":123}}`)))
	req = withClaims(req, "archon-alice")
	rec := incCreate(h, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Code = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if db.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0", db.insertCalls)
	}
}
