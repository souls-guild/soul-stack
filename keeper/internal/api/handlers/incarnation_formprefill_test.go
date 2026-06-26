package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// makeIncRowWithState — pgx.Row-stub под SelectByName с контролируемым jsonb-
// state (для form-prefill-тестов: значения, из которых резолвится prefill).
// Зеркалит makeIncarnationRow, но даёт контроль над state-колонкой (idx 5).
func makeIncRowWithState(name string, state map[string]any) pgx.Row {
	return makeIncRowWithStateVersion(name, "v1", state)
}

// makeIncRowWithStateVersion — как makeIncRowWithState, но с явной
// service_version (idx 2): version-pin-тесты сверяют, что снапшот грузится по
// ServiceVersion инкарнации, а не по дефолту резолвера.
func makeIncRowWithStateVersion(name, version string, state map[string]any) pgx.Row {
	stateBytes, _ := json.Marshal(state)
	now := time.Now()
	return staticRow{values: []any{
		name, "redis", version, int(1),
		[]byte("{}"), stateBytes, "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"), // traits
		any(nil), []byte(nil),
		"create", // created_scenario (миграция 089, NOT NULL DEFAULT)
	}}
}

// formPrefillHandler собирает IncarnationHandler с db (state-row) + loader
// (scenario YAML + state_schema для secret-исключения) + services.
func formPrefillHandler(state map[string]any, scenarioYAML string, stateSchema map[string]any) *IncarnationHandler {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row { return makeIncRowWithState(name, state) },
	}
	loader := &fakeLoader{scenarioYAML: scenarioYAML, stateSchema: stateSchema}
	return &IncarnationHandler{
		db:       db,
		loader:   loader,
		services: &fakeResolver{ok: true},
	}
}

// allowAllScope — inScope-предикат «инкарнация всегда в scope» (RBAC-граница
// проверяется отдельными тестами scope; здесь фокус на резолве prefill).
func allowAllScope(*incarnation.Incarnation) bool { return true }

// TestFormPrefill_ResolvesDeclaredPaths — базовый успех: объявленные
// prefill_from_state-пути резолвятся из incarnation.state в {values}.
func TestFormPrefill_ResolvesDeclaredPaths(t *testing.T) {
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n" +
		"  redis_version: { type: string, prefill_from_state: state.redis_version }\n" +
		"  max_memory: { type: string, prefill_from_state: state.config.max_memory }\n"
	state := map[string]any{
		"redis_version": "7.2.4",
		"config":        map[string]any{"max_memory": "2gb"},
	}
	h := formPrefillHandler(state, scenarioYAML, nil)

	res, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config", allowAllScope)
	if err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}
	if res.Values["redis_version"] != "7.2.4" {
		t.Errorf("redis_version = %#v, want 7.2.4", res.Values["redis_version"])
	}
	if res.Values["max_memory"] != "2gb" {
		t.Errorf("max_memory = %#v, want 2gb (nested state.config.max_memory)", res.Values["max_memory"])
	}
}

// TestFormPrefill_UncoveredPathOmitted — поле с prefill-путём, которого нет в
// текущем state, ОПУСКАЕТСЯ (не null-значение, не ошибка).
func TestFormPrefill_UncoveredPathOmitted(t *testing.T) {
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n" +
		"  redis_version: { type: string, prefill_from_state: state.redis_version }\n" +
		"  missing: { type: string, prefill_from_state: state.not_in_state }\n"
	state := map[string]any{"redis_version": "7.2.4"}
	h := formPrefillHandler(state, scenarioYAML, nil)

	res, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config", allowAllScope)
	if err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}
	if _, present := res.Values["missing"]; present {
		t.Errorf("поле с непокрытым state-путём не должно попадать в values: %#v", res.Values)
	}
	if res.Values["redis_version"] != "7.2.4" {
		t.Errorf("покрытое поле потеряно: %#v", res.Values)
	}
}

// TestFormPrefill_PathWhitelist — GUARD (инвариант b): резолвятся СТРОГО
// объявленные в схеме prefill-пути. Поле БЕЗ prefill_from_state — даже если его
// имя совпадает с state-ключом — в values НЕ попадает (клиент путь не задаёт,
// произвольный state-доступ невозможен).
func TestFormPrefill_PathWhitelist(t *testing.T) {
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n" +
		"  redis_version: { type: string, prefill_from_state: state.redis_version }\n" +
		"  secret_token: { type: string }\n" // НЕТ prefill_from_state → не whitelisted
	state := map[string]any{
		"redis_version": "7.2.4",
		"secret_token":  "super-secret", // присутствует в state, но поле его не объявило
	}
	h := formPrefillHandler(state, scenarioYAML, nil)

	res, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config", allowAllScope)
	if err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}
	if _, present := res.Values["secret_token"]; present {
		t.Errorf("поле без объявленного prefill_from_state утекло в values (path-whitelist пробит): %#v", res.Values)
	}
	if len(res.Values) != 1 || res.Values["redis_version"] != "7.2.4" {
		t.Errorf("ожидался только объявленный redis_version: %#v", res.Values)
	}
}

// TestFormPrefill_SecretExcluded — GUARD (инвариант c): поле, чей state-путь
// помечен secret в state_schema, ИСКЛЮЧАЕТСЯ из prefill полностью (pre-fill
// маски бесполезен). Несекретное поле остаётся.
func TestFormPrefill_SecretExcluded(t *testing.T) {
	scenarioYAML := "name: rotate\nstate_changes: {}\ntasks: []\n" +
		"input:\n" +
		"  admin_token: { type: string, secret: true, prefill_from_state: state.admin_token }\n" +
		"  redis_version: { type: string, prefill_from_state: state.redis_version }\n"
	state := map[string]any{
		"admin_token":   "s3cr3t-token",
		"redis_version": "7.2.4",
	}
	// state_schema помечает admin_token secret → secretSchemaForIncarnation.IsSecret("state.admin_token")=true.
	stateSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"admin_token":   map[string]any{"type": "string", "secret": true},
			"redis_version": map[string]any{"type": "string"},
		},
	}
	h := formPrefillHandler(state, scenarioYAML, stateSchema)

	res, err := h.FormPrefillTyped(context.Background(), "redis-prod", "rotate", allowAllScope)
	if err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}
	if _, present := res.Values["admin_token"]; present {
		t.Errorf("secret-поле утекло в prefill-ответ (инвариант c пробит): %#v", res.Values["admin_token"])
	}
	if res.Values["redis_version"] != "7.2.4" {
		t.Errorf("несекретное поле потеряно: %#v", res.Values)
	}
}

// TestFormPrefill_OutOfScope404 — вне RBAC-scope → 404 (не палим существование,
// parity GetTyped).
func TestFormPrefill_OutOfScope404(t *testing.T) {
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n  redis_version: { type: string, prefill_from_state: state.redis_version }\n"
	h := formPrefillHandler(map[string]any{"redis_version": "7.2.4"}, scenarioYAML, nil)

	_, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config",
		func(*incarnation.Incarnation) bool { return false })
	if err == nil {
		t.Fatal("вне scope ожидалась ошибка 404")
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("ошибка не problemError: %v", err)
	}
	if d.Status != 404 {
		t.Errorf("status = %d, want 404", d.Status)
	}
}

// TestFormPrefill_NilScope404 — nil-предикат → fail-closed 404.
func TestFormPrefill_NilScope404(t *testing.T) {
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n  redis_version: { type: string, prefill_from_state: state.redis_version }\n"
	h := formPrefillHandler(map[string]any{"redis_version": "7.2.4"}, scenarioYAML, nil)

	if _, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config", nil); err == nil {
		t.Fatal("nil-scope ожидалась ошибка 404 (fail-closed)")
	}
}

// TestFormPrefill_NoPrefillFields — схема без prefill_from_state → пустые values
// (не ошибка).
func TestFormPrefill_NoPrefillFields(t *testing.T) {
	scenarioYAML := "name: restart\nstate_changes: {}\ntasks: []\n" +
		"input:\n  reason: { type: string }\n"
	h := formPrefillHandler(map[string]any{"redis_version": "7.2.4"}, scenarioYAML, nil)

	res, err := h.FormPrefillTyped(context.Background(), "redis-prod", "restart", allowAllScope)
	if err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}
	if len(res.Values) != 0 {
		t.Errorf("ожидались пустые values (нет prefill-полей): %#v", res.Values)
	}
}

// TestFormPrefill_SchemaPinnedToServiceVersion — GUARD (анти version-craft):
// схема для prefill (И path-whitelist, И secret-set) грузится СТРОГО по
// inc.ServiceVersion, а не по произвольной клиентской версии. Клиент версию
// больше не задаёт; тест фиксирует, что обе материализации снапшота
// (prefillFieldsForScenario + secretSchemaForIncarnation) запросили ОДНУ
// авторитетную версию = ServiceVersion. Регрессия (рассинхрон whitelist/secret
// по разным версиям) → version-craft вектор возвращения sensitive-полей.
func TestFormPrefill_SchemaPinnedToServiceVersion(t *testing.T) {
	const wantVersion = "v2.0.0" // отлично от дефолта fakeResolver ("v1")
	scenarioYAML := "name: update_config\nstate_changes: {}\ntasks: []\n" +
		"input:\n  redis_version: { type: string, prefill_from_state: state.redis_version }\n"
	stateSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"redis_version": map[string]any{"type": "string"}},
	}
	state := map[string]any{"redis_version": "7.2.4"}

	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return makeIncRowWithStateVersion(name, wantVersion, state)
		},
	}
	loader := &fakeLoader{scenarioYAML: scenarioYAML, stateSchema: stateSchema}
	h := &IncarnationHandler{db: db, loader: loader, services: &fakeResolver{ok: true}}

	if _, err := h.FormPrefillTyped(context.Background(), "redis-prod", "update_config", allowAllScope); err != nil {
		t.Fatalf("FormPrefillTyped: %v", err)
	}

	if len(loader.loadedRefs) == 0 {
		t.Fatal("снапшот сервиса не загружался — version-pin недоказуем")
	}
	for i, ref := range loader.loadedRefs {
		if ref != wantVersion {
			t.Errorf("Load[%d] ref = %q, want %q (схема обязана пиниться на ServiceVersion, не на клиентскую/дефолтную)", i, ref, wantVersion)
		}
	}
}
