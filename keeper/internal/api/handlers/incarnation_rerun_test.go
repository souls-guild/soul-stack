package handlers

// HANDLER-NATIVE тесты RerunLastTyped: прямой вызов доменной функции вместо
// httptest+(w,r). 202 → err==nil + view.{ApplyID,Incarnation,Scenario}; 404/409/422 → wantProblem.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// newRerunHandler собирает handler со всеми deps для rerun-last
// (runner + resolver + auditWriter). loader не нужен (rerun не валидирует input).
func newRerunHandler(db *fakeIncDB, starter *fakeStarter, aw *fakeAuditWriter) *IncarnationHandler {
	return NewIncarnationHandler(db, starter, nil, nil, &fakeResolver{ok: true}, nil, aw, nil, nil)
}

// rerunDB конструирует fakeIncDB под rerun-поток: SelectByName (status) +
// UnlockForRerun SELECT FOR UPDATE (state, status) тот же status. Дефолтный
// last-run probe → create (create-путь: последний упавший == created).
func rerunDB(status string) *fakeIncDB {
	return &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, status) },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow(status) },
	}
}

// TestRerunLast_202_FromErrorLocked — happy path: из error_locked снимается
// блок (UnlockForRerun: applying) и запускается ровно один scenario `create`
// с общим apply_id; ответ 202 {apply_id, incarnation, scenario}; audit rerun_last
// с reason + previous_status + scenario.
func TestRerunLast_202_FromErrorLocked(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	aw := &fakeAuditWriter{}
	h := newRerunHandler(db, starter, aw)

	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun bootstrap verified")
	if err != nil {
		t.Fatalf("RerunLastTyped err = %v", err)
	}
	if out.Incarnation != "redis-prod" {
		t.Errorf("incarnation = %q", out.Incarnation)
	}
	if out.Scenario != "create" {
		t.Errorf("reply scenario = %q, want create", out.Scenario)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	// Ровно один новый create-прогон с тем же apply_id.
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "create" {
		t.Errorf("ScenarioName = %q, want create", starter.gotSpec.ScenarioName)
	}
	if starter.gotSpec.ApplyID != out.ApplyID {
		t.Errorf("run apply_id = %q, want %q", starter.gotSpec.ApplyID, out.ApplyID)
	}
	if starter.gotSpec.ServiceRef.Ref != "v1" {
		t.Errorf("ServiceRef.Ref = %q, want v1 (развёрнутая версия)", starter.gotSpec.ServiceRef.Ref)
	}
	// audit rerun_last с reason + previous_status=error_locked + scenario.
	if !hasEvent(aw, audit.EventIncarnationRerunLast) {
		t.Fatalf("ожидался audit incarnation.rerun_last")
	}
	var ev *audit.Event
	for _, e := range aw.events {
		if e.EventType == audit.EventIncarnationRerunLast {
			ev = e
		}
	}
	if ev.Payload["reason"] != "rerun bootstrap verified" {
		t.Errorf("audit reason = %v", ev.Payload["reason"])
	}
	if ev.Payload["previous_status"] != "error_locked" {
		t.Errorf("audit previous_status = %v, want error_locked", ev.Payload["previous_status"])
	}
	if ev.Payload["scenario"] != "create" {
		t.Errorf("audit scenario = %v, want create", ev.Payload["scenario"])
	}
	if ev.Payload["apply_id"] != out.ApplyID {
		t.Errorf("audit apply_id = %v, want %q", ev.Payload["apply_id"], out.ApplyID)
	}
	// НЕ переиспользует incarnation.unlocked.
	if hasEvent(aw, audit.EventIncarnationUnlocked) {
		t.Errorf("rerun не должен писать incarnation.unlocked")
	}
}

// TestRerunLast_ReusesStoredInput_202 — create-путь GUARD: rerun-last инкарнации
// с сохранённым в spec.input оператор-input (redis cluster: version + shards) →
// этот input проброшен в RunSpec.Input перезапускаемого bootstrap-прогона (НЕ nil,
// НЕ дефолты). Регресс: RunSpec без Input → nil → перезапуск падает на required-
// валидации (version/shards) либо применяет дефолты.
func TestRerunLast_ReusesStoredInput_202(t *testing.T) {
	specJSON := []byte(`{"input":{"version":"8.6.1","shards":3,"connection_mode":"cluster"}}`)
	db := &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "error_locked") },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRowSpec("error_locked", specJSON) },
	}
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-cluster-prod", "rerun cluster bootstrap")
	if err != nil {
		t.Fatalf("RerunLastTyped err = %v", err)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	gotInput := starter.gotSpec.Input
	if gotInput == nil {
		t.Fatal("RunSpec.Input = nil — stored spec.input НЕ проброшен (create-путь регресс)")
	}
	if gotInput["version"] != "8.6.1" {
		t.Errorf("RunSpec.Input[version] = %v, want 8.6.1 (stored)", gotInput["version"])
	}
	// jsonb-числа десериализуются как float64 — сверяем по значению.
	if shards, ok := gotInput["shards"].(float64); !ok || shards != 3 {
		t.Errorf("RunSpec.Input[shards] = %v (%T), want 3", gotInput["shards"], gotInput["shards"])
	}
	if gotInput["connection_mode"] != "cluster" {
		t.Errorf("RunSpec.Input[connection_mode] = %v, want cluster (stored)", gotInput["connection_mode"])
	}
}

// TestRerunLast_NoStoredInput_NilInput_202 — create-путь контраст: инкарнация БЕЗ
// сохранённого input (spec.input отсутствует) → RunSpec.Input nil (input не
// задавался), прогон стартует штатно. Регресс = пустой spec даёт `{}`-input или
// панику на извлечении.
func TestRerunLast_NoStoredInput_NilInput_202(t *testing.T) {
	db := rerunDB("error_locked") // makeUnlockSelectRow → spec=`{}` (нет input)
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun no-input bootstrap")
	if err != nil {
		t.Fatalf("RerunLastTyped err = %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.Input != nil {
		t.Errorf("RunSpec.Input = %v, want nil (spec без input)", starter.gotSpec.Input)
	}
}

// TestRerunLast_Ops_ReusesRecipeInput_202 — happy-path: последний упавший
// — add_user (≠ created `create`), его input берётся из recipe apply_run → 202,
// RunSpec.ScenarioName=="add_user", RunSpec.Input=={user:alice} (не spec.input),
// reply.Scenario=="add_user", audit scenario=="add_user".
func TestRerunLast_Ops_ReusesRecipeInput_202(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "error_locked") },
		// spec.input несёт version — НЕ должен просочиться на операционном пути.
		unlockSelectRow: func(_ string) pgx.Row {
			return makeUnlockSelectRowSpec("error_locked", []byte(`{"input":{"version":"8.6.1"}}`))
		},
		lastScenarioRow: func(_ string) pgx.Row {
			return staticRow{values: []any{"add_user", "01HFAILEDADDUSER0000000000"}}
		},
		recipeRow: func(_ string) pgx.Row {
			return staticRow{values: []any{[]byte(`{"scenario_name":"add_user","input":{"user":"alice"}}`)}}
		},
	}
	starter := &fakeStarter{}
	aw := &fakeAuditWriter{}
	h := newRerunHandler(db, starter, aw)

	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun add_user verified")
	if err != nil {
		t.Fatalf("RerunLastTyped operational err = %v", err)
	}
	if out.Scenario != "add_user" {
		t.Errorf("reply scenario = %q, want add_user", out.Scenario)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.ScenarioName != "add_user" {
		t.Errorf("ScenarioName = %q, want add_user (последний упавший операционный сценарий)", starter.gotSpec.ScenarioName)
	}
	gotInput := starter.gotSpec.Input
	if gotInput == nil || gotInput["user"] != "alice" {
		t.Fatalf("RunSpec.Input = %v, want {user:alice} (recipe.input)", gotInput)
	}
	if _, leaked := gotInput["version"]; leaked {
		t.Error("RunSpec.Input несёт spec.input[version] — операционный сценарий обязан брать recipe.input")
	}
	var ev *audit.Event
	for _, e := range aw.events {
		if e.EventType == audit.EventIncarnationRerunLast {
			ev = e
		}
	}
	if ev == nil || ev.Payload["scenario"] != "add_user" {
		t.Errorf("audit scenario = %v, want add_user", ev)
	}
	// recipe без from_upgrade → RunSpec.FromUpgrade=false (перезапуск из scenario/).
	if starter.gotSpec.FromUpgrade {
		t.Error("RunSpec.FromUpgrade = true, want false (recipe без from_upgrade)")
	}
}

// TestRerunLast_Ops_FromUpgradeRecipe_202 — MAJOR-guard (ADR-0068): rerun-last
// прогона, чей recipe.from_upgrade=true (упавший found-автозапуск upgrade-сценария),
// обязан пробросить FromUpgrade=true в RunSpec — иначе перезапуск ищет scenario/<slug>/
// (которого нет, §3) и падает 500. Проверяет проводку UnlockResult.FromUpgrade →
// RunSpec.FromUpgrade на уровне ХЕНДЛЕРА (DB-слой это не ловит).
func TestRerunLast_Ops_FromUpgradeRecipe_202(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "error_locked") },
		unlockSelectRow: func(_ string) pgx.Row {
			return makeUnlockSelectRowSpec("error_locked", []byte(`{}`))
		},
		lastScenarioRow: func(_ string) pgx.Row {
			return staticRow{values: []any{"to_v2", "01HFAILEDUPGRADE0000000000"}}
		},
		recipeRow: func(_ string) pgx.Row {
			return staticRow{values: []any{[]byte(`{"scenario_name":"to_v2","from_upgrade":true,"input":{}}`)}}
		},
	}
	starter := &fakeStarter{}
	aw := &fakeAuditWriter{}
	h := newRerunHandler(db, starter, aw)

	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun upgrade verified")
	if err != nil {
		t.Fatalf("RerunLastTyped operational upgrade err = %v", err)
	}
	if out.Scenario != "to_v2" {
		t.Errorf("reply scenario = %q, want to_v2", out.Scenario)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if !starter.gotSpec.FromUpgrade {
		t.Error("RunSpec.FromUpgrade = false, want true (recipe.from_upgrade → перезапуск из upgrade/)")
	}
	if !starter.gotSpec.FromLocked {
		t.Error("RunSpec.FromLocked = false, want true (applying зарезервирован)")
	}
}

// TestRerunLast_Ops_BareIncarnation_202 — bare-инкарнация (created_scenario IS
// NULL) залочена операционным сценарием → rerun-last применим через recipe-путь (было:
// 409). ScenarioName из last-run, Input из recipe.
func TestRerunLast_Ops_BareIncarnation_202(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "error_locked") },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRowBare("error_locked") },
		lastScenarioRow: func(_ string) pgx.Row {
			return staticRow{values: []any{"update_acl", "01HFAILEDACL00000000000000"}}
		},
		recipeRow: func(_ string) pgx.Row {
			return staticRow{values: []any{[]byte(`{"input":{"acl":"readonly"}}`)}}
		},
	}
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-bare", "rerun bare operational")
	if err != nil {
		t.Fatalf("RerunLastTyped bare operational err = %v", err)
	}
	if out.Scenario != "update_acl" {
		t.Errorf("reply scenario = %q, want update_acl", out.Scenario)
	}
	if starter.calls != 1 {
		t.Fatalf("scenario start calls = %d, want 1", starter.calls)
	}
	if starter.gotSpec.Input["acl"] != "readonly" {
		t.Errorf("RunSpec.Input[acl] = %v, want readonly (recipe.input)", starter.gotSpec.Input["acl"])
	}
}

// TestRerunLast_Ops_RecipeUnavailable_409 — операционный сценарий, но recipe отсутствует (recipe
// IS NULL / apply_run вычищен → ErrNoRows): fail-closed 409 rerun-input-unavailable
// (отдельный problem-type от incarnation-locked — machine-readable отличие от
// «статус не error_locked»), прогон НЕ стартует (без silent bootstrap-input).
func TestRerunLast_Ops_RecipeUnavailable_409(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "error_locked") },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow("error_locked") },
		lastScenarioRow: func(_ string) pgx.Row {
			return staticRow{values: []any{"add_user", "01HFAILEDADDUSER0000000000"}}
		},
		// recipeRow nil → recipe-probe вернёт ErrNoRows (fail-closed).
	}
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "rerun add_user")
	wantProblem(t, err, problem.TypeRerunInputUnavailable)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (fail-closed recipe недоступен)", starter.calls)
	}
}

// TestRerunLast_RejectNonErrorLocked — из ready/applying/migration_failed
// rerun отклонён (409 incarnation-locked), прогон НЕ стартует.
func TestRerunLast_RejectNonErrorLocked(t *testing.T) {
	for _, status := range []string{"ready", "applying", "migration_failed", "destroy_failed", "drift"} {
		t.Run(status, func(t *testing.T) {
			db := rerunDB(status)
			starter := &fakeStarter{}
			h := newRerunHandler(db, starter, &fakeAuditWriter{})

			_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "x")
			wantProblem(t, err, problem.TypeIncarnationLocked)
			if starter.calls != 0 {
				t.Errorf("status=%s: scenario start calls = %d, want 0", status, starter.calls)
			}
		})
	}
}

// TestRerunLast_NotFound_404 — несуществующая incarnation → 404.
func TestRerunLast_NotFound_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "ghost", "x")
	wantProblem(t, err, problem.TypeNotFound)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0", starter.calls)
	}
}

// TestRerunLast_EmptyReason_422 — пустой reason → 422 (явное подтверждение).
func TestRerunLast_EmptyReason_422(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "")
	wantProblem(t, err, problem.TypeValidationFailed)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (отказ до старта)", starter.calls)
	}
}

// TestRerunLast_InvalidName_422 — невалидное имя в path → 422.
func TestRerunLast_InvalidName_422(t *testing.T) {
	h := newRerunHandler(rerunDB("error_locked"), &fakeStarter{}, &fakeAuditWriter{})
	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "Bad_Name", "x")
	wantProblem(t, err, problem.TypeValidationFailed)
}

// TestRerunLast_ReasonAtMax_202 — reason ровно ReasonMaxLen символов проходит
// (граница включительно): rerun-last стартует, scenario start вызван.
func TestRerunLast_ReasonAtMax_202(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("a", incarnation.ReasonMaxLen)
	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	if err != nil {
		t.Fatalf("RerunLastTyped err = %v (reason ровно %d допустим)", err, incarnation.ReasonMaxLen)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// TestRerunLast_ReasonOverMax_422 — reason длиннее ReasonMaxLen → 422 ДО старта
// (верхняя граница reason, поведенческий инвариант).
func TestRerunLast_ReasonOverMax_422(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("a", incarnation.ReasonMaxLen+1)
	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	wantProblem(t, err, problem.TypeValidationFailed)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (reason over max → отказ до старта)", starter.calls)
	}
}

// TestRerunLast_ReasonMultibyteAtMax_202 — ЛОК рунной семантики (спека↔рантайм):
// reason из ReasonMaxLen кириллических рун — это 2*ReasonMaxLen БАЙТ, но ровно
// ReasonMaxLen рун. JSON-Schema maxLength считает руны, значит ДОЛЖЕН пройти, хотя
// по байтам это >maxLen. Ловит регресс len(reason)↔utf8.RuneCountInString.
func TestRerunLast_ReasonMultibyteAtMax_202(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("я", incarnation.ReasonMaxLen) // ReasonMaxLen рун, 2*ReasonMaxLen байт
	if len(reason) <= incarnation.ReasonMaxLen {
		t.Fatalf("предусловие теста нарушено: %d байт не превышает лимит %d — кейс не различает байты/руны",
			len(reason), incarnation.ReasonMaxLen)
	}
	out, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	if err != nil {
		t.Fatalf("RerunLastTyped err = %v (ReasonMaxLen рун кириллицей допустимо — считаем руны, не байты)", err)
	}
	if !audit.IsValidULID(out.ApplyID) {
		t.Errorf("apply_id not ULID: %q", out.ApplyID)
	}
	if starter.calls != 1 {
		t.Errorf("scenario start calls = %d, want 1", starter.calls)
	}
}

// TestRerunLast_ReasonMultibyteOverMax_422 — обратная граница рунной семантики:
// ReasonMaxLen+1 кириллических рун → 422 ДО старта (по рунам превышено).
func TestRerunLast_ReasonMultibyteOverMax_422(t *testing.T) {
	db := rerunDB("error_locked")
	starter := &fakeStarter{}
	h := newRerunHandler(db, starter, &fakeAuditWriter{})

	reason := strings.Repeat("я", incarnation.ReasonMaxLen+1)
	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", reason)
	wantProblem(t, err, problem.TypeValidationFailed)
	if starter.calls != 0 {
		t.Errorf("scenario start calls = %d, want 0 (reason over max рунами → отказ до старта)", starter.calls)
	}
}
