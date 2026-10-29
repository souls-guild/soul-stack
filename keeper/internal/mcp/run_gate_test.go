package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// maintenanceRows answers the start gate's reads in windowCols / freezeCols
// order, assigned by reflection.
func maintenanceRows(windows []maintenance.Window, freezes []maintenance.Freeze) pgx.Rows {
	var rows [][]any
	for _, w := range windows {
		rows = append(rows, []any{w.ID, w.IncarnationID, w.Days, w.StartTime, w.DurationMinutes, w.Timezone, w.Comment, time.Now(), time.Now()})
	}
	for _, f := range freezes {
		rows = append(rows, []any{f.ID, f.IncarnationID, f.StartsAt, f.EndsAt, f.Comment, time.Now(), time.Now()})
	}
	return &reflectRows{rows: rows}
}

type reflectRows struct {
	rows [][]any
	i    int
}

func (r *reflectRows) Close()                                       {}
func (r *reflectRows) Err() error                                   { return nil }
func (r *reflectRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *reflectRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *reflectRows) Next() bool                                   { r.i++; return r.i <= len(r.rows) }
func (r *reflectRows) Values() ([]any, error)                       { return r.rows[r.i-1], nil }
func (r *reflectRows) RawValues() [][]byte                          { return nil }
func (r *reflectRows) Conn() *pgx.Conn                              { return nil }
func (r *reflectRows) Scan(dest ...any) error {
	for i, v := range r.rows[r.i-1] {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(v))
	}
	return nil
}

func mcpSale() maintenance.Freeze {
	now := time.Now().UTC()
	return maintenance.Freeze{ID: "11111111-1111-4111-8111-111111111111", IncarnationID: "redis-prod",
		StartsAt: now.Add(-time.Hour), EndsAt: now.Add(2 * time.Hour).Truncate(time.Second), Comment: "Распродажа"}
}

func wantMCPFreezeActive(t *testing.T, resp jsonRPCResponse) {
	t.Helper()
	if resp.Error == nil {
		t.Fatal("expected freeze-active, got a result")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeFreezeActive {
		t.Fatalf("data.code = %q, want %s", data.Code, mcpCodeFreezeActive)
	}
	if !strings.Contains(resp.Error.Message, "redis-prod") || !strings.Contains(resp.Error.Message, "Распродажа") {
		t.Errorf("message = %q, want the incarnation and the freeze's comment", resp.Error.Message)
	}
}

func TestToolsCall_IncarnationRun_FreezeNeedsConsent(t *testing.T) {
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady), freezes: []maintenance.Freeze{mcpSale()}}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, runnerRBAC(), starter, &mcpResolver{ok: true}, nil)

	wantMCPFreezeActive(t, callTool(t, h, "archon-alice", "keeper.incarnation.run", `{"id":"redis-prod","scenario":"rotate"}`))
	if starter.calls != 0 || len(rec.events) != 0 {
		t.Fatalf("refused run: starts=%d audit=%d, want neither", starter.calls, len(rec.events))
	}

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run", `{"id":"redis-prod","scenario":"rotate","confirm_freeze":true}`)
	if resp.Error != nil {
		t.Fatalf("run with consent: %+v", resp.Error)
	}
	if starter.calls != 1 || len(rec.events) != 1 || rec.events[0].Payload["confirm_freeze"] != true {
		t.Fatalf("starts=%d events=%v, want started with confirm_freeze audited", starter.calls, rec.events)
	}
}

func TestToolsCall_IncarnationRun_ClosedWindowDefers(t *testing.T) {
	slot := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Minute)
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady), windows: []maintenance.Window{{
		ID: "w1", IncarnationID: "redis-prod", Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
		StartTime: slot.Format("15:04"), DurationMinutes: 60, Timezone: "UTC",
	}}}
	starter := &mcpStarter{}
	h, rec := newTestHandlerFull(t, pool, runnerRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"id":"redis-prod","scenario":"rotate","in_maintenance_window":true}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out incarnationRunOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ApplyID != "" || out.VoyageID == "" || out.ScheduleAt == nil || !out.ScheduleAt.Equal(slot) {
		t.Fatalf("output = %+v, want voyage_id + schedule_at %s and no _apply_id", out, slot)
	}
	if starter.calls != 0 || pool.voyageTargets != 1 {
		t.Fatalf("starts=%d targets=%d, want no start and one scheduled target", starter.calls, pool.voyageTargets)
	}
	if a := pool.voyageInsertArgs; a[15] != string(voyage.StatusScheduled) || a[22] != true {
		t.Errorf("voyage row status=%v in_maintenance_window=%v, want scheduled true", a[15], a[22])
	}
	if len(rec.events) != 1 || rec.events[0].Payload["voyage_id"] != out.VoyageID {
		t.Errorf("audit = %v, want one event with the voyage_id", rec.events)
	}
}

// The pre-flight assert answers alike whether the window is open now or not
// (parity REST TestRunGate_AFailingAssertIsRefusedBeforeTheDeferral).
func TestToolsCall_IncarnationRun_FailingAssertIsRefusedBeforeTheDeferral(t *testing.T) {
	slot := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Minute)
	pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady), windows: []maintenance.Window{{
		ID: "w1", IncarnationID: "redis-prod", Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
		StartTime: slot.Format("15:04"), DurationMinutes: 60, Timezone: "UTC",
	}}}
	starter := &mcpStarterAssert{assertErr: fmt.Errorf("%w: roster has 1 host, want 3", scenario.ErrAssertFailed)}
	h, _ := newTestHandlerFull(t, pool, runnerRBAC(), starter, &mcpResolver{ok: true}, nil)
	resp := callTool(t, h, "archon-alice", "keeper.incarnation.run",
		`{"id":"redis-prod","scenario":"rotate","in_maintenance_window":true}`)
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "assert_failed") {
		t.Fatalf("resp = %+v, want assert_failed", resp)
	}
	if pool.voyageInsertArgs != nil || starter.calls != 0 {
		t.Fatal("a run whose assert failed was deferred or started")
	}
}

func TestToolsCall_IncarnationRerunLast_FreezeNeedsConsent(t *testing.T) {
	pool := &fakePool{incFn: incLocked(nil, "create"), freezes: []maintenance.Freeze{mcpSale()}}
	starter := &mcpStarter{}
	h, _ := newTestHandlerFull(t, pool, rerunRBAC(), starter, &mcpResolver{ok: true}, nil)

	resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last", `{"id":"redis-prod","reason":"retry"}`)
	wantMCPFreezeActive(t, resp)
	if strings.Contains(resp.Error.Message, "in_maintenance_window") {
		t.Errorf("message offers a field rerun-last refuses: %q", resp.Error.Message)
	}
	if starter.calls != 0 {
		t.Fatal("a refused rerun started")
	}
	if resp := callTool(t, h, "archon-alice", "keeper.incarnation.rerun-last",
		`{"id":"redis-prod","reason":"retry","confirm_freeze":true}`); resp.Error != nil || starter.calls != 1 {
		t.Fatalf("rerun with consent: err=%+v starts=%d", resp.Error, starter.calls)
	}
}

func TestToolsCall_IncarnationDestroy_FreezeNeedsConsent(t *testing.T) {
	for _, force := range []string{"false", "true"} {
		pool := &fakePool{incFn: incWithStatus(incarnation.StatusReady), freezes: []maintenance.Freeze{mcpSale()}}
		destroyer := &mcpDestroyer{}
		h, rec := newTestHandlerDestroy(t, pool, destroyerRBAC(), destroyer, true)

		wantMCPFreezeActive(t, callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
			`{"id":"redis-prod","allow_destroy":`+force+`}`))
		if destroyer.calls != 0 || len(rec.events) != 0 {
			t.Fatalf("allow_destroy=%s: refused destroy left teardown=%d audit=%d", force, destroyer.calls, len(rec.events))
		}
		if resp := callTool(t, h, "archon-alice", "keeper.incarnation.destroy",
			`{"id":"redis-prod","allow_destroy":`+force+`,"confirm_freeze":true}`); resp.Error != nil {
			t.Fatalf("allow_destroy=%s: destroy with consent: %+v", force, resp.Error)
		}
	}
}
