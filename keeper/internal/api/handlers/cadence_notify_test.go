package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// Guard-тесты постоянного notify-блока формы Cadence (ADR-052 §m / ADR-046 §9).
// Покрывают: атомарность (rollback при сбое InsertTiding), инвалидацию TTL-
// снимка dispatcher-а (вызвана только при notify), RBAC-deny (herald.read на
// канал), ULID-привязку (Tiding.Cadence==c.ID, не name), генерацию имён
// (<name>-notify[-N]), регресс notify=пусто (поведение как у прежнего Create).
// Каскад FK (DELETE cadence сносит created_from_cadence_id-правила, не трогая
// NULL-маркер) — в integration под docker (cadence_notify_cascade_integration_test.go).

// notifyEnforcer — allow incarnation.run/errand.run + herald.read (notify-OK).
func notifyEnforcer() *fakeVoyageEnforcer {
	return &fakeVoyageEnforcer{allow: map[string]bool{
		"incarnation.run": true,
		"errand.run":      true,
		"herald.read":     true,
	}}
}

const cadenceNotifyBody = `{"name":"nightly","schedule_kind":"interval","interval_seconds":300,` +
	`"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},` +
	`"notify":[{"herald":"ops-webhook","on":["failed"]}]}`

// notify=пусто → поведение идентично прежнему Create: один Insert Cadence в tx,
// БЕЗ InsertTiding и БЕЗ инвалидации (регресс).
func TestCadenceCreate_NotifyEmpty_NoInvalidationNoTiding(t *testing.T) {
	store := &fakeCadenceStore{}
	inv := &fakeTidingInvalidator{}
	h := newCadenceHandlerNotify(store, notifyEnforcer(), inv)

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences",
		`{"name":"nightly","schedule_kind":"interval","interval_seconds":300,"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"}}`))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", store.insertCalls)
	}
	if store.insertTidingCalls != 0 {
		t.Errorf("insertTidingCalls = %d, want 0 (notify пуст)", store.insertTidingCalls)
	}
	if !store.committed {
		t.Error("tx должна быть закоммичена")
	}
	if inv.calls != 0 {
		t.Errorf("InvalidateTidings calls = %d, want 0 (notify пуст)", inv.calls)
	}
}

// notify[1] → постоянный Tiding в той же tx + инвалидация после commit. Проверяем
// ULID-привязку (Cadence + created_from_cadence_id == c.ID, не name) и имя.
func TestCadenceCreate_Notify_PersistsTidingAndInvalidates(t *testing.T) {
	store := &fakeCadenceStore{}
	inv := &fakeTidingInvalidator{}
	h := newCadenceHandlerNotify(store, notifyEnforcer(), inv)

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences", cadenceNotifyBody))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var reply cadenceCreateReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if store.insertCalls != 1 || store.insertTidingCalls != 1 {
		t.Fatalf("insertCalls=%d insertTidingCalls=%d, want 1/1", store.insertCalls, store.insertTidingCalls)
	}
	if !store.committed {
		t.Error("tx должна быть закоммичена (notify-OK)")
	}

	// Инвалидация вызвана РОВНО один раз, с ULID расписания (cadences.id).
	if inv.calls != 1 {
		t.Fatalf("InvalidateTidings calls = %d, want 1", inv.calls)
	}
	if inv.names[0] != reply.CadenceID {
		t.Errorf("InvalidateTidings(name) = %q, want cadence-id %q", inv.names[0], reply.CadenceID)
	}

	// ULID-привязка Tiding: herald, cadence-селектор, created_from_cadence_id, имя.
	// tidingInsertSQL-args (см. crud.go): name=$1 herald=$2 ... cadence=$7 task=$8
	// ephemeral=$9 voyage_id=$10 created_from_cadence_id=$11.
	args := store.insertTidingArgs[0]
	gotName, _ := args[0].(string)
	gotHerald, _ := args[1].(string)
	gotCadence, _ := args[6].(string)
	gotEphemeral, _ := args[8].(bool)
	gotOrigin, _ := args[10].(string)

	if gotName != "nightly-notify" {
		t.Errorf("Tiding.Name = %q, want nightly-notify", gotName)
	}
	if gotHerald != "ops-webhook" {
		t.Errorf("Tiding.Herald = %q, want ops-webhook", gotHerald)
	}
	if gotEphemeral {
		t.Error("Tiding.Ephemeral = true, want false (постоянное правило)")
	}
	if gotCadence != reply.CadenceID {
		t.Errorf("Tiding.Cadence(селектор) = %q, want ULID %q (не name)", gotCadence, reply.CadenceID)
	}
	if gotOrigin != reply.CadenceID {
		t.Errorf("Tiding.CreatedFromCadenceID = %q, want ULID %q (origin-маркер каскада)", gotOrigin, reply.CadenceID)
	}
	// Привязка по ULID, не по имени расписания: имя "nightly" не должно попасть в
	// селектор/маркер (rename-safe инвариант).
	if gotCadence == "nightly" || gotOrigin == "nightly" {
		t.Error("привязка по имени расписания, а не по ULID (нарушен rename-safe инвариант)")
	}
}

// Несколько notify → детерминированно-уникальные имена <name>-notify, -notify-2,…
func TestCadenceCreate_Notify_MultipleUniqueNames(t *testing.T) {
	store := &fakeCadenceStore{}
	inv := &fakeTidingInvalidator{}
	h := newCadenceHandlerNotify(store, notifyEnforcer(), inv)

	body := `{"name":"nightly","schedule_kind":"interval","interval_seconds":300,` +
		`"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},` +
		`"notify":[{"herald":"ops-webhook"},{"herald":"ops-webhook","on":["failed"]},{"herald":"sec-webhook"}]}`

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences", body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertTidingCalls != 3 {
		t.Fatalf("insertTidingCalls = %d, want 3", store.insertTidingCalls)
	}

	names := map[string]bool{}
	for i, args := range store.insertTidingArgs {
		n, _ := args[0].(string)
		if names[n] {
			t.Fatalf("дубликат имени Tiding %q (PK-коллизия недопустима)", n)
		}
		names[n] = true
		_ = i
	}
	for _, want := range []string{"nightly-notify", "nightly-notify-2", "nightly-notify-3"} {
		if !names[want] {
			t.Errorf("ожидаемое имя %q отсутствует; получены %v", want, names)
		}
	}
}

// Атомарность: сбой InsertTiding (например FK/коллизия) → ROLLBACK всей tx,
// Cadence НЕ создан (insertCalls=1, но commit не вызван, rollback вызван), 500.
func TestCadenceCreate_Notify_InsertTidingFails_Rollback(t *testing.T) {
	store := &fakeCadenceStore{insertTidingErr: pgx.ErrTxClosed}
	inv := &fakeTidingInvalidator{}
	h := newCadenceHandlerNotify(store, notifyEnforcer(), inv)

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences", cadenceNotifyBody))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (сбой InsertTiding); body=%s", rec.Code, rec.Body.String())
	}
	// Insert Cadence в tx был вызван, но НЕ закоммичен — откат уносит и его.
	if !store.rolledBack {
		t.Error("ожидался Rollback при сбое InsertTiding")
	}
	if store.committed {
		t.Error("tx закоммичена несмотря на сбой InsertTiding (атомарность нарушена)")
	}
	// Инвалидация НЕ вызвана (rollback → правил в БД нет, инвалидировать нечего).
	if inv.calls != 0 {
		t.Errorf("InvalidateTidings calls = %d, want 0 (rollback)", inv.calls)
	}
}

// RBAC-deny: создатель без herald.read на канал → 403, Cadence НЕ создан
// (InsertTiding не вызван, tx не открыта/закоммичена), нет инвалидации.
func TestCadenceCreate_Notify_NoHeraldRead_Denied(t *testing.T) {
	store := &fakeCadenceStore{}
	inv := &fakeTidingInvalidator{}
	// allowAll: есть incarnation.run/errand.run, но НЕТ herald.read.
	h := newCadenceHandlerNotify(store, allowAll(), inv)

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences", cadenceNotifyBody))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (нет herald.read); body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 || store.insertTidingCalls != 0 {
		t.Errorf("write вызван при RBAC-deny: insertCalls=%d insertTidingCalls=%d, want 0/0",
			store.insertCalls, store.insertTidingCalls)
	}
	if store.committed {
		t.Error("tx закоммичена при RBAC-deny (Cadence не должен создаваться)")
	}
	if inv.calls != 0 {
		t.Errorf("InvalidateTidings calls = %d, want 0 (deny)", inv.calls)
	}
}

// Cap: notify[] длиннее maxNotifyChannels → 422 ДО tx (а не мутный rollback-500
// от слишком длинного имени правила внутри транзакции), Cadence НЕ создан,
// инвалидации нет.
func TestCadenceCreate_Notify_TooMany_422(t *testing.T) {
	store := &fakeCadenceStore{}
	inv := &fakeTidingInvalidator{}
	h := newCadenceHandlerNotify(store, notifyEnforcer(), inv)

	entries := make([]string, maxNotifyChannels+1)
	for i := range entries {
		entries[i] = `{"herald":"ops-webhook","on":["failed"]}`
	}
	body := `{"name":"nightly","schedule_kind":"interval","interval_seconds":300,` +
		`"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},` +
		`"notify":[` + strings.Join(entries, ",") + `]}`

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences", body))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (notify > cap); body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 || store.insertTidingCalls != 0 || store.committed {
		t.Errorf("write вызван при превышении cap: insertCalls=%d insertTidingCalls=%d committed=%v",
			store.insertCalls, store.insertTidingCalls, store.committed)
	}
	if inv.calls != 0 {
		t.Errorf("InvalidateTidings calls = %d, want 0 (cap-deny)", inv.calls)
	}
}

// Граница cap: ровно maxNotifyChannels элементов — проходит (201), не 422.
func TestCadenceCreate_Notify_AtCapBoundary_OK(t *testing.T) {
	store := &fakeCadenceStore{}
	inv := &fakeTidingInvalidator{}
	h := newCadenceHandlerNotify(store, notifyEnforcer(), inv)

	entries := make([]string, maxNotifyChannels)
	for i := range entries {
		entries[i] = `{"herald":"ops-webhook","on":["failed"]}`
	}
	body := `{"name":"nightly","schedule_kind":"interval","interval_seconds":300,` +
		`"overlap_policy":"skip","kind":"scenario","scenario_name":"converge","target":{"service":"web"},` +
		`"notify":[` + strings.Join(entries, ",") + `]}`

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences", body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (ровно cap); body=%s", rec.Code, rec.Body.String())
	}
	if store.insertTidingCalls != maxNotifyChannels {
		t.Errorf("insertTidingCalls = %d, want %d", store.insertTidingCalls, maxNotifyChannels)
	}
}

// Несуществующий herald → 422 ДО tx (а не FK-500 при insert), Cadence НЕ создан.
func TestCadenceCreate_Notify_UnknownHerald_422(t *testing.T) {
	store := &fakeCadenceStore{heraldKnown: []string{"other-webhook"}}
	inv := &fakeTidingInvalidator{}
	h := newCadenceHandlerNotify(store, notifyEnforcer(), inv)

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences", cadenceNotifyBody))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (несуществующий herald); body=%s", rec.Code, rec.Body.String())
	}
	if store.insertCalls != 0 || store.committed {
		t.Error("Cadence создан несмотря на несуществующий herald")
	}
}

// DELETE cadence → InvalidateTidings вызвана РОВНО один раз с cadence-id (BUG-2).
// Каскад FK (created_from_cadence_id ON DELETE CASCADE, миграция 074) сносит
// постоянные notify-правила на БД-уровне В ОБХОД herald.Service-CRUD; без явной
// инвалидации dispatcher держит снесённое правило за TTL-снимком (15s) и
// cross-keeper publish не происходит. Инвалидация безусловна (handler не знает,
// были ли form-rules — каскад БД-side; сброс — всего снимка, id лишь лейбл).
func TestCadenceDelete_InvalidatesTidings(t *testing.T) {
	store := &fakeCadenceStore{}
	inv := &fakeTidingInvalidator{}
	h := newCadenceHandlerNotify(store, notifyEnforcer(), inv)
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Delete(rec, cadenceReqID(http.MethodDelete, "/v1/cadences/"+id, id, ""))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if store.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", store.deleteCalls)
	}
	if inv.calls != 1 {
		t.Fatalf("InvalidateTidings calls = %d, want 1 (безусловно при delete)", inv.calls)
	}
	if inv.names[0] != id {
		t.Errorf("InvalidateTidings(name) = %q, want cadence-id %q", inv.names[0], id)
	}
}

// Delete не-существующего cadence (404) → инвалидации НЕТ (delete не прошёл,
// в БД ничего не снесено — сбрасывать снимок незачем; parity с rollback-веткой
// Create, где инвалидация тоже не зовётся).
func TestCadenceDelete_NotFound_NoInvalidation(t *testing.T) {
	store := &fakeCadenceStore{deleteNoRow: true}
	inv := &fakeTidingInvalidator{}
	h := newCadenceHandlerNotify(store, notifyEnforcer(), inv)
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Delete(rec, cadenceReqID(http.MethodDelete, "/v1/cadences/"+id, id, ""))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if inv.calls != 0 {
		t.Errorf("InvalidateTidings calls = %d, want 0 (delete не прошёл)", inv.calls)
	}
}

// Delete без инвалидатора (dev без herald) → nil-safe, 204 без паники
// (parity с Create: nil tidingInvalidator → no-op).
func TestCadenceDelete_NilInvalidator_OK(t *testing.T) {
	store := &fakeCadenceStore{}
	h := newCadenceHandler(store, allowAll())
	id := audit.NewULID()

	rec := httptest.NewRecorder()
	h.Delete(rec, cadenceReqID(http.MethodDelete, "/v1/cadences/"+id, id, ""))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (nil-safe); body=%s", rec.Code, rec.Body.String())
	}
}
