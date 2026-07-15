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

// Guard tests for the persistent notify block of the Cadence form (ADR-052 §m /
// ADR-046 §9). Cover: atomicity (rollback on InsertTiding failure), invalidation
// of the dispatcher's TTL snapshot (called only on notify), RBAC-deny (herald.read
// on the channel), ULID binding (Tiding.Cadence==c.ID, not name), name generation
// (<name>-notify[-N]), the empty-notify regression (behaves like the former
// Create). FK cascade (DELETE cadence removes created_from_cadence_id rules without
// touching the NULL marker) is covered in integration under docker
// (cadence_notify_cascade_integration_test.go).

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

// empty notify → behavior identical to the former Create: a single Insert Cadence
// in the tx, no InsertTiding and no invalidation (regression).
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

// notify[1] → a persistent Tiding in the same tx + invalidation after commit. We
// check the ULID binding (Cadence + created_from_cadence_id == c.ID, not name) and
// the name.
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

	// Invalidation is called EXACTLY once, with the schedule's ULID (cadences.id).
	if inv.calls != 1 {
		t.Fatalf("InvalidateTidings calls = %d, want 1", inv.calls)
	}
	if inv.names[0] != reply.CadenceID {
		t.Errorf("InvalidateTidings(name) = %q, want cadence-id %q", inv.names[0], reply.CadenceID)
	}

	// Tiding ULID binding: herald, cadence selector, created_from_cadence_id, name.
	// tidingInsertSQL args (see crud.go): name=$1 herald=$2 ... cadence=$7 task=$8
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
	// Bind by ULID, not by the schedule name: the name "nightly" must not land in
	// the selector/marker (rename-safe invariant).
	if gotCadence == "nightly" || gotOrigin == "nightly" {
		t.Error("привязка по имени расписания, а не по ULID (нарушен rename-safe инвариант)")
	}
}

// Multiple notify entries → deterministically unique names <name>-notify, -notify-2,…
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

// Atomicity: an InsertTiding failure (e.g. FK/collision) → ROLLBACK of the whole
// tx, Cadence NOT created (insertCalls=1, but commit not called, rollback called),
// 500.
func TestCadenceCreate_Notify_InsertTidingFails_Rollback(t *testing.T) {
	store := &fakeCadenceStore{insertTidingErr: pgx.ErrTxClosed}
	inv := &fakeTidingInvalidator{}
	h := newCadenceHandlerNotify(store, notifyEnforcer(), inv)

	rec := httptest.NewRecorder()
	h.Create(rec, cadenceReq(http.MethodPost, "/v1/cadences", cadenceNotifyBody))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (сбой InsertTiding); body=%s", rec.Code, rec.Body.String())
	}
	// Insert Cadence in the tx was called but NOT committed — the rollback takes it too.
	if !store.rolledBack {
		t.Error("ожидался Rollback при сбое InsertTiding")
	}
	if store.committed {
		t.Error("tx закоммичена несмотря на сбой InsertTiding (атомарность нарушена)")
	}
	// Invalidation NOT called (rollback → no rules in the DB, nothing to invalidate).
	if inv.calls != 0 {
		t.Errorf("InvalidateTidings calls = %d, want 0 (rollback)", inv.calls)
	}
}

// RBAC-deny: a creator without herald.read on the channel → 403, Cadence NOT
// created (InsertTiding not called, tx not opened/committed), no invalidation.
func TestCadenceCreate_Notify_NoHeraldRead_Denied(t *testing.T) {
	store := &fakeCadenceStore{}
	inv := &fakeTidingInvalidator{}
	// allowAll: has incarnation.run/errand.run, but NO herald.read.
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

// Cap: notify[] longer than maxNotifyChannels → 422 BEFORE the tx (not a murky
// rollback-500 from an over-long rule name inside the transaction), Cadence NOT
// created, no invalidation.
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

// Cap boundary: exactly maxNotifyChannels entries — passes (201), not 422.
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

// A nonexistent herald → 422 BEFORE the tx (not an FK-500 on insert), Cadence NOT created.
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

// DELETE cadence → InvalidateTidings called EXACTLY once with the cadence-id
// (BUG-2). The FK cascade (created_from_cadence_id ON DELETE CASCADE, migration
// 074) removes the persistent notify rules at the DB level BYPASSING the
// herald.Service CRUD; without an explicit invalidation the dispatcher keeps the
// removed rule behind its TTL snapshot (15s) and no cross-keeper publish happens.
// Invalidation is unconditional (the handler does not know whether there were
// form-rules — the cascade is DB-side; it drops the whole snapshot, the id is just
// a label).
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

// Delete of a nonexistent cadence (404) → NO invalidation (the delete did not go
// through, nothing was removed in the DB — no reason to drop the snapshot; parity
// with the Create rollback branch, where invalidation is not called either).
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

// Delete without an invalidator (dev without herald) → nil-safe, 204 without a
// panic (parity with Create: nil tidingInvalidator → no-op).
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
