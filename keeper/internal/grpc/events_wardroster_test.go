package grpc

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// TestHandleWardRoster_SweepsOrphans — a SID's dispatched rows outside the set
// are terminated into orphaned (OrphanDispatched is called, the metric grows by the number
// of orphans). The point of S6: "Keeper and Soul both die after dispatch".
func TestHandleWardRoster_SweepsOrphans(t *testing.T) {
	aw := &recordingAudit{}
	// Exec returns UPDATE 2 — two dispatched rows were orphaned.
	ardb := &fakeApplyRunDB{execTag: pgconn.NewCommandTag("UPDATE 2")}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)

	h.handleWardRoster(context.Background(), "host.example.com", "session-1", &keeperv1.WardRoster{
		Active: []*keeperv1.ActiveApply{{ApplyId: "apply-live", Attempt: 3}},
	})

	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (OrphanDispatched)", ardb.execCalls)
	}
	if !strings.Contains(ardb.execSQL, "status        = 'orphaned'") {
		t.Errorf("sweep SQL does not set orphaned: %q", ardb.execSQL)
	}
	// $2 — known apply_ids: a "live" one protects its own row from being orphaned.
	known, ok := ardb.execArgs[1].([]string)
	if !ok || len(known) != 1 || known[0] != "apply-live" {
		t.Errorf("known set = %v, want [apply-live]", ardb.execArgs[1])
	}
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "keeper_apply_orphaned_total 2") {
		t.Errorf("keeper_apply_orphaned_total must be 2; got=\n%s", body)
	}
}

// TestHandleWardRoster_InSet_NotTouched — when the set covers all dispatched
// rows (Exec affects 0 rows), the metric does NOT grow: rows in the set aren't orphaned.
func TestHandleWardRoster_InSet_NotTouched(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)

	h.handleWardRoster(context.Background(), "host.example.com", "session-1", &keeperv1.WardRoster{
		Active: []*keeperv1.ActiveApply{{ApplyId: "apply-live", Attempt: 3}},
	})

	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (sweep still happens, filter cut rows)", ardb.execCalls)
	}
	if body := obstest.Scrape(t, reg.Gatherer()); strings.Contains(body, "keeper_apply_orphaned_total 1") ||
		strings.Contains(body, "keeper_apply_orphaned_total 2") {
		t.Errorf("orphaned metric should not grow with 0 affected; got=\n%s", body)
	}
}

// TestHandleWardRoster_EmptySet_OrphansAll — an empty WardRoster (Soul restart):
// the sweep runs with an empty known set → all dispatched rows are terminated.
func TestHandleWardRoster_EmptySet_OrphansAll(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{execTag: pgconn.NewCommandTag("UPDATE 5")}
	h, reg := newTestHandlerWithApplyRunMetrics(t, aw, ardb)

	h.handleWardRoster(context.Background(), "host.example.com", "session-1", &keeperv1.WardRoster{Active: nil})

	if ardb.execCalls != 1 {
		t.Fatalf("execCalls = %d, want 1 (empty set -> sweep all dispatched)", ardb.execCalls)
	}
	known, ok := ardb.execArgs[1].([]string)
	if !ok || len(known) != 0 {
		t.Errorf("known set = %v, want empty []string", ardb.execArgs[1])
	}
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "keeper_apply_orphaned_total 5") {
		t.Errorf("keeper_apply_orphaned_total must be 5; got=\n%s", body)
	}
}

// TestHandleWardRoster_NilApplyRunDB_NoSweep — without ApplyRunDB (unit/ad-hoc), the sweep
// doesn't happen (no-op, no panic).
func TestHandleWardRoster_NilApplyRunDB_NoSweep(t *testing.T) {
	h := newTestHandler(t, &recordingAudit{}) // ApplyRunDB not set
	h.handleWardRoster(context.Background(), "host.example.com", "session-1", &keeperv1.WardRoster{
		Active: []*keeperv1.ActiveApply{{ApplyId: "x"}},
	})
	// Got here without a panic — ApplyRunDB=nil → early return.
}

// TestHandleWardRoster_NilPayload_NoSweep — a nil payload doesn't crash the handler and doesn't
// touch the DB.
func TestHandleWardRoster_NilPayload_NoSweep(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	h, _ := newTestHandlerWithApplyRunMetrics(t, aw, ardb)
	h.handleWardRoster(context.Background(), "host.example.com", "session-1", nil)
	if ardb.execCalls != 0 {
		t.Errorf("execCalls = %d, want 0 (nil payload → no sweep)", ardb.execCalls)
	}
}

// TestWardRosterToActive_MapsAndFilters — proto → domain: nil entries are dropped,
// apply_id/attempt are carried through; an empty set → nil.
func TestWardRosterToActive_MapsAndFilters(t *testing.T) {
	got := wardRosterToActive(&keeperv1.WardRoster{Active: []*keeperv1.ActiveApply{
		{ApplyId: "a", Attempt: 2},
		nil,
		{ApplyId: "b", Attempt: 0},
	}})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (nil dropped)", len(got))
	}
	if got[0].ApplyID != "a" || got[0].Attempt != 2 {
		t.Errorf("got[0] = %+v, want {a 2}", got[0])
	}

	if wardRosterToActive(&keeperv1.WardRoster{Active: nil}) != nil {
		t.Error("empty set should map to nil")
	}
}

// TestDispatch_OldSoulNoWardRoster_NoSweep — forward-compat: an old Soul without
// WardRoster never sends this message. We drive it through dispatch payloads that don't
// carry ward semantics (TaskEvent / SoulprintReport): OrphanDispatched (UPDATE
// 'orphaned') is NOT invoked for them — the sweep is unreachable, dispatched rows stay
// hanging (fail-safe). handleWardRoster is the only sweep entry point.
func TestDispatch_OldSoulNoWardRoster_NoSweep(t *testing.T) {
	aw := &recordingAudit{}
	ardb := &fakeApplyRunDB{execTag: pgconn.NewCommandTag("UPDATE 0")}
	h, _ := newTestHandlerWithApplyRunMetrics(t, aw, ardb)

	// TaskEvent OK — writes neither failure nor sweep (see handleTaskEvent).
	h.dispatch(context.Background(), "host.example.com", "session-1", &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_TaskEvent{TaskEvent: &keeperv1.TaskEvent{
			ApplyId: "01HAPPLY", Status: keeperv1.TaskStatus_TASK_STATUS_OK,
		}},
	})

	for _, sql := range []string{ardb.execSQL} {
		if strings.Contains(sql, "'orphaned'") {
			t.Fatalf("sweep fired without WardRoster (old Soul) - forward-compat violated: %q", sql)
		}
	}
}
