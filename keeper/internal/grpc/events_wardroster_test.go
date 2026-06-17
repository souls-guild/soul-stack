package grpc

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// TestHandleWardRoster_SweepsOrphans — dispatched-строки SID-а вне набора
// терминалятся в orphaned (OrphanDispatched вызывается, метрика растёт на число
// осиротевших). Соль S6: «Keeper и Soul оба мертвы после отдачи».
func TestHandleWardRoster_SweepsOrphans(t *testing.T) {
	aw := &recordingAudit{}
	// Exec вернёт UPDATE 2 — две dispatched-строки осиротели.
	ardb := &fakeApplyRunDB{execTag: pgconn.NewCommandTag("UPDATE 2")}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)

	h.handleWardRoster(context.Background(), "host.example.com", "session-1", &keeperv1.WardRoster{
		Active: []*keeperv1.ActiveApply{{ApplyId: "apply-live", Attempt: 3}},
	})

	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (OrphanDispatched)", ardb.execCalls)
	}
	if !strings.Contains(ardb.execSQL, "status        = 'orphaned'") {
		t.Errorf("sweep SQL не ставит orphaned: %q", ardb.execSQL)
	}
	// $2 — known apply_ids: «живой» защищает свою строку от orphan.
	known, ok := ardb.execArgs[1].([]string)
	if !ok || len(known) != 1 || known[0] != "apply-live" {
		t.Errorf("known-набор = %v, want [apply-live]", ardb.execArgs[1])
	}
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "keeper_apply_orphaned_total 2") {
		t.Errorf("keeper_apply_orphaned_total must be 2; got=\n%s", body)
	}
}

// TestHandleWardRoster_InSet_NotTouched — когда набор покрывает все dispatched
// (Exec затронул 0 строк), метрика НЕ растёт: строки в наборе не осиротляются.
func TestHandleWardRoster_InSet_NotTouched(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)

	h.handleWardRoster(context.Background(), "host.example.com", "session-1", &keeperv1.WardRoster{
		Active: []*keeperv1.ActiveApply{{ApplyId: "apply-live", Attempt: 3}},
	})

	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (sweep всё равно делается, фильтр отсёк строки)", ardb.execCalls)
	}
	if body := obstest.Scrape(t, reg.Gatherer()); strings.Contains(body, "keeper_apply_orphaned_total 1") ||
		strings.Contains(body, "keeper_apply_orphaned_total 2") {
		t.Errorf("orphaned-метрика не должна расти при 0 затронутых; got=\n%s", body)
	}
}

// TestHandleWardRoster_EmptySet_OrphansAll — пустой WardRoster (рестарт Soul):
// sweep запускается с пустым known-набором → терминалятся все dispatched-строки.
func TestHandleWardRoster_EmptySet_OrphansAll(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{execTag: pgconn.NewCommandTag("UPDATE 5")}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)

	h.handleWardRoster(context.Background(), "host.example.com", "session-1", &keeperv1.WardRoster{Active: nil})

	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (пустой набор → sweep всех dispatched)", ardb.execCalls)
	}
	known, ok := ardb.execArgs[1].([]string)
	if !ok || len(known) != 0 {
		t.Errorf("known-набор = %v, want пустой []string", ardb.execArgs[1])
	}
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "keeper_apply_orphaned_total 5") {
		t.Errorf("keeper_apply_orphaned_total must be 5; got=\n%s", body)
	}
}

// TestHandleWardRoster_NilApplyRunDB_NoSweep — без ApplyRunDB (unit/ad-hoc) sweep
// не делается (no-op, без паники).
func TestHandleWardRoster_NilApplyRunDB_NoSweep(t *testing.T) {
	h := newTestHandler(t, &recordingAudit{}) // ApplyRunDB не задан
	h.handleWardRoster(context.Background(), "host.example.com", "session-1", &keeperv1.WardRoster{
		Active: []*keeperv1.ActiveApply{{ApplyId: "x"}},
	})
	// Дошли без паники — ApplyRunDB=nil → ранний возврат.
}

// TestHandleWardRoster_NilPayload_NoSweep — nil payload не валит handler и не
// дёргает DB.
func TestHandleWardRoster_NilPayload_NoSweep(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	h, _ := newTestHandlerWithApplyRunMetrics(t, aw, ardb)
	h.handleWardRoster(context.Background(), "host.example.com", "session-1", nil)
	if ardb.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (nil payload → no sweep)", ardb.execCalls)
	}
}

// TestWardRosterToActive_MapsAndFilters — proto → домен: nil-записи отбрасываются,
// apply_id/attempt протягиваются; пустой набор → nil.
func TestWardRosterToActive_MapsAndFilters(t *testing.T) {
	got := wardRosterToActive(&keeperv1.WardRoster{Active: []*keeperv1.ActiveApply{
		{ApplyId: "a", Attempt: 2},
		nil,
		{ApplyId: "b", Attempt: 0},
	}})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (nil отброшен)", len(got))
	}
	if got[0].ApplyID != "a" || got[0].Attempt != 2 {
		t.Errorf("got[0] = %+v, want {a 2}", got[0])
	}

	if wardRosterToActive(&keeperv1.WardRoster{Active: nil}) != nil {
		t.Error("пустой набор должен маппиться в nil")
	}
}

// TestDispatch_OldSoulNoWardRoster_NoSweep — forward-compat: старый Soul без
// WardRoster никогда не шлёт это сообщение. Гоним через dispatch payload-ы, не
// несущие ward-семантику (TaskEvent / SoulprintReport): OrphanDispatched (UPDATE
// 'orphaned') по ним НЕ вызывается — sweep недостижим, dispatched-строки остаются
// висеть (fail-safe). handleWardRoster — единственная точка sweep-а.
func TestDispatch_OldSoulNoWardRoster_NoSweep(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	h, _ := newTestHandlerWithApplyRunMetrics(t, aw, ardb)

	// TaskEvent OK — не пишет ни failure, ни sweep (см. handleTaskEvent).
	h.dispatch(context.Background(), "host.example.com", "session-1", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_TaskEvent{TaskEvent: &keeperv1.TaskEvent{
			ApplyId: "01HAPPLY", Status: keeperv1.TaskStatus_TASK_STATUS_OK,
		}},
	})

	for _, sql := range []string{ardb.execSQL} {
		if strings.Contains(sql, "'orphaned'") {
			t.Fatalf("sweep сработал без WardRoster (старый Soul) — нарушен forward-compat: %q", sql)
		}
	}
}
