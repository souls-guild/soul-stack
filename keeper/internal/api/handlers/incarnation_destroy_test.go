package handlers

// HANDLER-NATIVE тесты DestroyTyped (T5d-2c-full): прямой вызов доменной функции вместо
// httptest+(w,r). Status-коды проверяются через problem.Type sentinel-а (wantProblem); 202 —
// err==nil + view.ApplyID. Bind-тесты (allow_destroy missing/non-bool → 400) СНЕСЕНЫ: huma биндит
// bool до DestroyTyped, доменная функция bool уже получает.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeDestroyer — мок [DestroyStarter]: фиксирует переданный RunSpec teardown-а
// и счётчик вызовов StartDestroy. err задаёт фейл старта прогона.
type fakeDestroyer struct {
	gotSpec scenario.RunSpec
	calls   int
	err     error
}

func (f *fakeDestroyer) StartDestroy(_ context.Context, spec scenario.RunSpec) error {
	f.calls++
	f.gotSpec = spec
	return f.err
}

// fakeAuditWriter — мок [audit.Writer]: копит записанные события для проверки
// destroy_started / destroy_completed (force в payload).
type fakeAuditWriter struct {
	events []*audit.Event
}

func (f *fakeAuditWriter) Write(_ context.Context, ev *audit.Event) error {
	f.events = append(f.events, ev)
	return nil
}

// newDestroyHandler собирает handler со всеми destroy-deps. hasScenario управляет
// fakeLoader (есть ли scenario `destroy` в снапшоте).
func newDestroyHandler(db *fakeIncDB, destroyer *fakeDestroyer, aw *fakeAuditWriter, hasScenario bool) *IncarnationHandler {
	loader := &fakeLoader{hasDestroyScenario: hasScenario}
	return NewIncarnationHandler(db, &fakeStarter{}, destroyer, nil, &fakeResolver{ok: true}, loader, aw, nil, nil)
}

// destroyDB конструирует fakeIncDB под полный destroy-поток: SelectByName
// (prepare/404) отдаёт строку статуса status; Destroy SELECT FOR UPDATE
// (state, status) — тот же status.
func destroyDB(name, status string) *fakeIncDB {
	return &fakeIncDB{
		selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, status) },
		unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow(status) },
	}
}

// --- 202 + teardown (scenario есть, allow_destroy=false) ---------------

func TestIncarnation_Destroy_Teardown_202(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, destroyer, aw, true)
	view, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false)
	if err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	if len(view.ApplyID) != 26 {
		t.Errorf("apply_id len = %d, want 26", len(view.ApplyID))
	}
	if destroyer.calls != 1 {
		t.Fatalf("StartDestroy calls = %d, want 1 (teardown)", destroyer.calls)
	}
	// applyID handler-а == applyID teardown-spec-а (общая корреляция).
	if destroyer.gotSpec.ApplyID != view.ApplyID {
		t.Errorf("teardown ApplyID = %q, want %q (handler apply_id)", destroyer.gotSpec.ApplyID, view.ApplyID)
	}
	if destroyer.gotSpec.IncarnationName != "redis-prod" {
		t.Errorf("teardown IncarnationName = %q", destroyer.gotSpec.IncarnationName)
	}
	if destroyer.gotSpec.StartedByAID != "archon-alice" {
		t.Errorf("teardown StartedByAID = %q", destroyer.gotSpec.StartedByAID)
	}
	// Teardown катится развёрнутой версией сервиса (inc.ServiceVersion = "v1").
	if destroyer.gotSpec.ServiceRef.Ref != "v1" {
		t.Errorf("teardown ServiceRef.Ref = %q, want v1 (deployed version)", destroyer.gotSpec.ServiceRef.Ref)
	}
	// force=false → НЕ должно быть прямого DELETE строки (его делает run.go S-D3).
	for _, sql := range db.execCalls {
		if strings.Contains(sql, "DELETE FROM incarnation") {
			t.Errorf("force=false не должен делать DELETE напрямую; got exec %q", sql)
		}
	}
	// audit destroy_started пишет service-слой Destroy (source=api), force=false.
	if !hasEvent(aw, audit.EventIncarnationDestroyStarted) {
		t.Errorf("ожидался audit destroy_started")
	}
	if hasEvent(aw, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("destroy_completed не должен писаться при teardown-пути (его пишет run.go)")
	}
}

// newDestroyHandlerLifecycle — как newDestroyHandler, но с lifecycle-блоком
// манифеста снапшота (S3: auto_destroy).
func newDestroyHandlerLifecycle(db *fakeIncDB, destroyer *fakeDestroyer, aw *fakeAuditWriter, hasScenario bool, lc *config.LifecycleConfig) *IncarnationHandler {
	loader := &fakeLoader{hasDestroyScenario: hasScenario, lifecycle: lc}
	return NewIncarnationHandler(db, &fakeStarter{}, destroyer, nil, &fakeResolver{ok: true}, loader, aw, nil, nil)
}

// sawDeleteIncarnation — был ли прямой DELETE строки incarnation среди exec-ов.
func sawDeleteIncarnation(db *fakeIncDB) bool {
	for _, sql := range db.execCalls {
		if strings.Contains(sql, "DELETE FROM incarnation") {
			return true
		}
	}
	return false
}

// TestIncarnation_Destroy_AutoDestroyFalse_DirectDelete — lifecycle.auto_destroy=
// false: удаление ВСЕГДА прямое (DELETE без teardown), приоритет над
// allow_destroy=false — даже при наличии scenario `destroy`.
func TestIncarnation_Destroy_AutoDestroyFalse_DirectDelete(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	// scenario `destroy` ЕСТЬ, но auto_destroy=false → teardown пропускается.
	h := newDestroyHandlerLifecycle(db, destroyer, aw, true, &config.LifecycleConfig{AutoDestroy: boolPtr(false)})
	// allow_destroy=false — auto_destroy=false должен иметь приоритет.
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false); err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0 (auto_destroy=false → без teardown)", destroyer.calls)
	}
	if !sawDeleteIncarnation(db) {
		t.Errorf("auto_destroy=false должен делать прямой DELETE; execCalls=%v", db.execCalls)
	}
}

// TestIncarnation_Destroy_AutoDestroyFalse_NoScenario_DirectDelete — auto_destroy=
// false + НЕТ scenario `destroy` + allow_destroy=false: всё равно прямой DELETE
// (приоритет над scenario-missing-gate, который иначе вернул бы 422).
func TestIncarnation_Destroy_AutoDestroyFalse_NoScenario_DirectDelete(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandlerLifecycle(db, destroyer, aw, false, &config.LifecycleConfig{AutoDestroy: boolPtr(false)})
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false); err != nil {
		t.Fatalf("DestroyTyped err = %v (auto_destroy=false минует scenario-gate)", err)
	}
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0", destroyer.calls)
	}
	if !sawDeleteIncarnation(db) {
		t.Errorf("ожидался прямой DELETE; execCalls=%v", db.execCalls)
	}
}

// TestIncarnation_Destroy_AutoDestroyDefault_Teardown — манифест без lifecycle
// (auto_destroy дефолтно true) + allow_destroy=false + scenario есть → teardown
// как сегодня (backcompat).
func TestIncarnation_Destroy_AutoDestroyDefault_Teardown(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandlerLifecycle(db, destroyer, aw, true, nil) // lifecycle nil
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false); err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	if destroyer.calls != 1 {
		t.Errorf("StartDestroy calls = %d, want 1 (default auto_destroy=true → teardown)", destroyer.calls)
	}
	if sawDeleteIncarnation(db) {
		t.Errorf("teardown-путь не должен делать прямой DELETE")
	}
}

// --- 422: allow_destroy=false и нет scenario `destroy` ----------------

func TestIncarnation_Destroy_NoScenario_NoForce_422(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, destroyer, aw, false) // нет scenario
	_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false)
	wantProblem(t, err, problem.TypeValidationFailed)
	// incarnation НЕ тронут: ни перехода в destroying, ни teardown.
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0 (отказ ДО destroying)", destroyer.calls)
	}
	if len(db.execCalls) != 0 {
		t.Errorf("execCalls = %d, want 0 (incarnation не должен мутировать)", len(db.execCalls))
	}
	if hasEvent(aw, audit.EventIncarnationDestroyStarted) {
		t.Errorf("destroy_started не должен писаться при отказе pre-check")
	}
}

// --- allow_destroy=true без scenario → force-DELETE -------------------

func TestIncarnation_Destroy_Force_NoScenario_202_Delete(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, destroyer, aw, false) // нет scenario, но force=true
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", true); err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	// force-путь: teardown пропущен.
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0 (force пропускает teardown)", destroyer.calls)
	}
	// force-путь делает DELETE строки напрямую (DeleteAfterTeardown).
	if !sawDeleteIncarnation(db) {
		t.Errorf("force-путь должен делать DELETE FROM incarnation; execCalls=%v", db.execCalls)
	}
	// audit: destroy_started (force=true) + destroy_completed (force=true).
	if !hasEvent(aw, audit.EventIncarnationDestroyStarted) {
		t.Errorf("ожидался destroy_started")
	}
	if !hasEvent(aw, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("ожидался destroy_completed (force-DELETE)")
	}
	if got := eventForce(aw, audit.EventIncarnationDestroyStarted); got != true {
		t.Errorf("destroy_started force payload = %v, want true", got)
	}
}

// --- allow_destroy=true с scenario → всё равно force-DELETE -----------
//
// force означает «снести без teardown» независимо от наличия scenario
// (decisions.md: force-путь destroying→немедленный DELETE).

func TestIncarnation_Destroy_Force_WithScenario_SkipsTeardown(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	destroyer := &fakeDestroyer{}
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, destroyer, aw, true) // scenario есть, но force=true
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", true); err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0 (force пропускает teardown даже со scenario)", destroyer.calls)
	}
	if !hasEvent(aw, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("ожидался destroy_completed (force-DELETE)")
	}
}

// --- allow_destroy маппится в force (status_details + audit payload) --

func TestIncarnation_Destroy_AllowDestroyMapsToForce(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, &fakeDestroyer{}, aw, false)
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", true); err != nil {
		t.Fatalf("DestroyTyped err = %v", err)
	}
	// status_details.force=true пишется в UPDATE-Exec Destroy-tx — проверяем,
	// что переход в destroying произошёл (history INSERT + status UPDATE).
	var sawStatusUpdate bool
	for _, sql := range db.execCalls {
		if strings.Contains(sql, "UPDATE incarnation") && strings.Contains(sql, "status") {
			sawStatusUpdate = true
		}
	}
	if !sawStatusUpdate {
		t.Errorf("ожидался UPDATE incarnation status (переход в destroying)")
	}
	if got := eventForce(aw, audit.EventIncarnationDestroyStarted); got != true {
		t.Errorf("allow_destroy=true → force=true в audit, got %v", got)
	}
}

// --- 404: incarnation не существует ------------------------------------

func TestIncarnation_Destroy_NotFound_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} },
	}
	h := newDestroyHandler(db, &fakeDestroyer{}, &fakeAuditWriter{}, true)
	_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "ghost", false)
	wantProblem(t, err, problem.TypeNotFound)
}

// --- 409: статус не допускает destroy (applying) ----------------------

func TestIncarnation_Destroy_NotDestroyable_409(t *testing.T) {
	db := destroyDB("redis-prod", "applying")
	destroyer := &fakeDestroyer{}
	h := newDestroyHandler(db, destroyer, &fakeAuditWriter{}, true)
	_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false)
	wantProblem(t, err, problem.TypeIncarnationLocked)
	if destroyer.calls != 0 {
		t.Errorf("StartDestroy calls = %d, want 0 (applying не запускает teardown)", destroyer.calls)
	}
}

// --- 500: destroyer не сконфигурирован --------------------------------

func TestIncarnation_Destroy_NotConfigured_500(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	// destroyer/services/loader nil → endpoint не сконфигурирован.
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", false)
	wantProblem(t, err, problem.TypeInternalError)
	if len(db.execCalls) != 0 {
		t.Errorf("execCalls = %d, want 0 (не сконфигурирован → нет мутаций)", len(db.execCalls))
	}
}

// --- 422: невалидное path-name ----------------------------------------

func TestIncarnation_Destroy_InvalidName_422(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	h := newDestroyHandler(db, &fakeDestroyer{}, &fakeAuditWriter{}, true)
	_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "Bad_Name", false)
	wantProblem(t, err, problem.TypeValidationFailed)
}

// --- force-DELETE no-op (гонка: строка уже снесена) → 202 -------------

func TestIncarnation_Destroy_Force_DeleteNoOp_202(t *testing.T) {
	db := destroyDB("redis-prod", "ready")
	db.deleteTag = pgconn.NewCommandTag("DELETE 0") // RowsAffected==0 → Deleted=false
	aw := &fakeAuditWriter{}
	h := newDestroyHandler(db, &fakeDestroyer{}, aw, false)
	// no-op DELETE — не ошибка (идемпотентность S-D3): handler возвращает 202.
	if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", true); err != nil {
		t.Fatalf("DestroyTyped err = %v (no-op DELETE не ошибка)", err)
	}
	// destroy_completed НЕ пишется при no-op (Deleted=false).
	if hasEvent(aw, audit.EventIncarnationDestroyCompleted) {
		t.Errorf("destroy_completed не должен писаться при no-op DELETE")
	}
}

// --- helpers ----------------------------------------------------------

func hasEvent(aw *fakeAuditWriter, et audit.EventType) bool {
	for _, ev := range aw.events {
		if ev.EventType == et {
			return true
		}
	}
	return false
}

// eventForce возвращает значение payload["force"] первого события типа et
// (false если события нет / поля нет).
func eventForce(aw *fakeAuditWriter, et audit.EventType) bool {
	for _, ev := range aw.events {
		if ev.EventType == et {
			if v, ok := ev.Payload["force"].(bool); ok {
				return v
			}
		}
	}
	return false
}
