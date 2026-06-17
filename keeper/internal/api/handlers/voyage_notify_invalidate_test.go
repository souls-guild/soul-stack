package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// spyTidingInvalidator — счётчик двухуровневой инвалидации (in-process +
// cross-keeper). Реализует [TidingInvalidator]. Имитирует
// herald.Service.InvalidateTidings, который под капотом дёргает оба уровня;
// тест проверяет именно факт вызова из persist после commit (race-fix
// ADR-052(g)). Раздельные in-process / publish счётчики держим, чтобы guard
// был осмыслен и при возможном будущем расщеплении интерфейса.
type spyTidingInvalidator struct {
	mu          sync.Mutex
	inProcess   int // эквивалент InvalidateRules
	crossKeeper int // эквивалент PublishHeraldInvalidate
	lastName    string
}

func (s *spyTidingInvalidator) InvalidateTidings(_ context.Context, name string) {
	s.mu.Lock()
	s.inProcess++
	s.crossKeeper++
	s.lastName = name
	s.mu.Unlock()
}

func (s *spyTidingInvalidator) counts() (inProc, cross int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inProcess, s.crossKeeper
}

// newVoyageHandlerWithInvalidator — handler со spy-инвалидатором + enforcer,
// разрешающим incarnation.run/errand.run/herald.read (notify требует herald.read
// на канал, ADR-052(g)). heraldExists=true выставляет вызывающий тест
// (notify-existence-check).
func newVoyageHandlerWithInvalidator(store *fakeVoyageStore, sc VoyageScenarioResolver, cmd VoyageCommandResolver, inv TidingInvalidator) *VoyageHandler {
	enf := &fakeVoyageEnforcer{allow: map[string]bool{
		"incarnation.run": true,
		"errand.run":      true,
		"herald.read":     true,
	}}
	return NewVoyageHandler(store, sc, cmd, nil /*incReader*/, enf, nil /*scoper*/, nil /*auditW*/, inv, 0, 0, nil)
}

// TestVoyageNotify_InvalidatesAfterCommit — guard A (race-fix ADR-052(g)):
// ephemeral-Tiding из notify вставляется прямым InsertTiding в voyage-tx, в
// обход herald.Service-инвалидации; persist обязан ЯВНО инвалидировать снимок
// dispatcher-а ПОСЛЕ commit, иначе быстрый прогон диспетчит терминал против
// устаревшего TTL-снимка и уведомление молча промахивается.
//
// Этот тест КРАСНЫЙ без фикса (handler не звал инвалидацию вовсе).
func TestVoyageNotify_InvalidatesAfterCommit(t *testing.T) {
	store := &fakeVoyageStore{heraldExists: true}
	inv := &spyTidingInvalidator{}
	h := newVoyageHandlerWithInvalidator(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, inv)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"},`+
			`"notify":[{"herald":"ops-webhook","on":["completed"]}]}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if !store.committed {
		t.Fatal("tx не закоммичена — инвариант теста сломан")
	}
	if store.insertTidings != 1 {
		t.Fatalf("insertTidings = %d, want 1 (один notify-элемент)", store.insertTidings)
	}
	inProc, cross := inv.counts()
	if inProc != 1 {
		t.Errorf("in-process InvalidateRules вызван %d раз, want ровно 1", inProc)
	}
	if cross != 1 {
		t.Errorf("cross-keeper publish вызван %d раз, want ровно 1", cross)
	}
}

// TestVoyageNotify_NoNotify_NoInvalidate — negative-guard: без notify-блока
// инвалидировать нечего (ephemeral-правил не создано) — invalidate НЕ зовётся.
func TestVoyageNotify_NoNotify_NoInvalidate(t *testing.T) {
	store := &fakeVoyageStore{}
	inv := &spyTidingInvalidator{}
	h := newVoyageHandlerWithInvalidator(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, inv)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"}}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if store.insertTidings != 0 {
		t.Fatalf("insertTidings = %d, want 0", store.insertTidings)
	}
	if inProc, cross := inv.counts(); inProc != 0 || cross != 0 {
		t.Errorf("invalidate вызван (in=%d cross=%d), want 0/0 — без notify правил нет", inProc, cross)
	}
}

// TestVoyageNotify_TidingInsertFails_NoInvalidate — ключевой инвариант
// «только после commit»: сбой INSERT INTO tidings откатывает tx (Voyage не
// создан), invalidate НЕ зовётся (нечего инвалидировать — правила в БД нет).
func TestVoyageNotify_TidingInsertFails_NoInvalidate(t *testing.T) {
	store := &fakeVoyageStore{heraldExists: true, insertTidingErr: context.DeadlineExceeded}
	inv := &spyTidingInvalidator{}
	h := newVoyageHandlerWithInvalidator(store, &fakeVoyageScenarioResolver{out: []string{"inc-a"}}, &fakeVoyageCommandResolver{}, inv)

	rec := httptest.NewRecorder()
	h.Create(rec, voyageReq(http.MethodPost, "/v1/voyages",
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"web"},`+
			`"notify":[{"herald":"ops-webhook","on":["completed"]}]}`))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (insert tiding failed); body=%s", rec.Code, rec.Body.String())
	}
	if store.committed {
		t.Fatal("tx закоммичена при сбое InsertTiding — нарушена атомарность")
	}
	if inProc, cross := inv.counts(); inProc != 0 || cross != 0 {
		t.Errorf("invalidate вызван при rollback (in=%d cross=%d), want 0/0 — инвариант «только после commit» нарушен", inProc, cross)
	}
}
