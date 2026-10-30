package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// gateIncDB is a fakeIncDB that also holds one incarnation's windows and
// freezes, answers the start gate's reads from them, and records the
// scheduled Voyage a deferred run writes.
type gateIncDB struct {
	*fakeIncDB
	windows []maintenance.Window
	freezes []maintenance.Freeze

	voyageInsertArgs []any
	targetsCopied    int
	committed        bool
}

func (g *gateIncDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM incarnation_maintenance_windows"):
		rows := make([][]any, len(g.windows))
		for i, w := range g.windows {
			rows[i] = []any{w.ID, w.IncarnationID, w.Kind, w.Days, w.StartTime, w.DurationMinutes, w.Timezone,
				(*time.Time)(nil), (*time.Time)(nil), w.Comment, time.Now(), time.Now()}
		}
		return &gateRows{rows: rows}, nil
	case strings.Contains(sql, "FROM incarnation_freezes"):
		rows := make([][]any, len(g.freezes))
		for i, f := range g.freezes {
			rows[i] = []any{f.ID, f.IncarnationID, f.StartsAt, f.EndsAt, f.Comment, time.Now(), time.Now()}
		}
		return &gateRows{rows: rows}, nil
	}
	return g.fakeIncDB.Query(ctx, sql, args...)
}

func (g *gateIncDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return &gateTx{fakeIncTx: fakeIncTx{db: g.fakeIncDB}, g: g}, nil
}

type gateTx struct {
	fakeIncTx
	g *gateIncDB
}

func (t *gateTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO voyages") {
		t.g.voyageInsertArgs = args
		return voyageScalarRow{vals: []any{time.Now().UTC()}}
	}
	return t.fakeIncTx.QueryRow(ctx, sql, args...)
}

func (t *gateTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, src pgx.CopyFromSource) (int64, error) {
	for src.Next() {
		t.g.targetsCopied++
	}
	return int64(t.g.targetsCopied), nil
}

func (t *gateTx) Commit(context.Context) error { t.g.committed = true; return nil }

// gateRows is pgx.Rows over positional values, assigned by reflection.
type gateRows struct {
	rows [][]any
	i    int
}

func (r *gateRows) Close()                                       {}
func (r *gateRows) Err() error                                   { return nil }
func (r *gateRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *gateRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *gateRows) Next() bool                                   { r.i++; return r.i <= len(r.rows) }
func (r *gateRows) Values() ([]any, error)                       { return r.rows[r.i-1], nil }
func (r *gateRows) RawValues() [][]byte                          { return nil }
func (r *gateRows) Conn() *pgx.Conn                              { return nil }
func (r *gateRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	if len(dest) != len(row) {
		return errors.New("gateRows: dest/values length mismatch")
	}
	for i, v := range row {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(v))
	}
	return nil
}

func gateDB(windows []maintenance.Window, freezes []maintenance.Freeze) *gateIncDB {
	return &gateIncDB{
		fakeIncDB: &fakeIncDB{
			selectByNameRow: func(n string) pgx.Row { return makeIncStatusRow(n, "ready") },
			unlockSelectRow: func(_ string) pgx.Row { return makeUnlockSelectRow("error_locked") },
		},
		windows: windows,
		freezes: freezes,
	}
}

// activeSale is a freeze covering now, with a comment the refusal must quote.
func activeSale() maintenance.Freeze {
	now := time.Now().UTC()
	return maintenance.Freeze{ID: "11111111-1111-4111-8111-111111111111", IncarnationID: "redis-prod",
		StartsAt: now.Add(-time.Hour), EndsAt: now.Add(2 * time.Hour).Truncate(time.Second), Comment: "Распродажа"}
}

// slotAhead is a daily one-hour window six hours from now, and its next start.
func slotAhead() (maintenance.Window, time.Time) {
	slot := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Minute)
	return maintenance.Window{ID: "w1", IncarnationID: "redis-prod",
		Days:      []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
		StartTime: slot.Format("15:04"), DurationMinutes: 60, Timezone: "UTC"}, slot
}

func wantFreezeActive(t *testing.T, err error) {
	t.Helper()
	var pe *problemError
	if !errors.As(err, &pe) || pe.Details.Type != problem.TypeFreezeActive {
		t.Fatalf("err = %v, want problem %s", err, problem.TypeFreezeActive)
	}
	if !strings.Contains(pe.Details.Detail, "redis-prod") || !strings.Contains(pe.Details.Detail, "Распродажа") {
		t.Errorf("detail = %q, want the incarnation and the freeze's comment", pe.Details.Detail)
	}
}

func TestRunGate_OpenStartsNow(t *testing.T) {
	db := gateDB(nil, nil)
	starter := &fakeStarter{}
	h := NewIncarnationHandler(db, starter, nil, &fakeResolver{ok: true}, nil, nil, nil, nil)
	reply, err := h.RunTyped(context.Background(), claims("archon-alice"), "redis-prod", "restart", nil,
		RunGate{InMaintenanceWindow: true})
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if starter.calls != 1 || len(reply.Body.ApplyID) != 26 || reply.Body.VoyageID != "" {
		t.Fatalf("reply = %+v, starts = %d; want the run started now", reply.Body, starter.calls)
	}
	if db.voyageInsertArgs != nil {
		t.Error("an open incarnation's run was written as a Voyage")
	}
	if reply.AuditPayload["in_maintenance_window"] != true || reply.AuditPayload["confirm_freeze"] != false {
		t.Errorf("audit payload = %v, want both request fields", reply.AuditPayload)
	}
}

func TestRunGate_ClosedDefersToTheNextWindow(t *testing.T) {
	w, slot := slotAhead()
	db := gateDB([]maintenance.Window{w}, nil)
	starter := &fakeStarter{}
	h := NewIncarnationHandler(db, starter, nil, &fakeResolver{ok: true}, nil, nil, nil, nil)
	reply, err := h.RunTyped(context.Background(), claims("archon-alice"), "redis-prod", "restart",
		map[string]any{"force": true}, RunGate{InMaintenanceWindow: true})
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if starter.calls != 0 {
		t.Fatal("a run outside its window was started")
	}
	if reply.Body.ApplyID != "" || len(reply.Body.VoyageID) != 26 || reply.Body.ScheduleAt == nil || !reply.Body.ScheduleAt.Equal(slot) {
		t.Fatalf("reply = %+v, want voyage_id + schedule_at %s and no apply_id", reply.Body, slot)
	}
	if !db.committed || db.targetsCopied != 1 {
		t.Fatalf("committed=%v targets=%d, want one scheduled Voyage over one target", db.committed, db.targetsCopied)
	}
	// insertSQL: $12 schedule_at, $16 status, $23 in_maintenance_window.
	a := db.voyageInsertArgs
	if at, _ := a[11].(time.Time); !at.Equal(slot) || a[15] != string(voyage.StatusScheduled) || a[22] != true {
		t.Errorf("voyage row: schedule_at=%v status=%v in_maintenance_window=%v, want %s scheduled true", a[11], a[15], a[22], slot)
	}
	if !strings.Contains(string(a[4].([]byte)), `"force":true`) {
		t.Errorf("voyage input = %s, want the request's input", a[4])
	}
	if reply.AuditPayload["voyage_id"] != reply.Body.VoyageID || reply.AuditPayload["in_maintenance_window"] != true {
		t.Errorf("audit payload = %v, want voyage_id and the request fields", reply.AuditPayload)
	}
}

// assertFailingStarter is a runner whose pre-flight assert refuses the run.
type assertFailingStarter struct{ fakeStarter }

func (s *assertFailingStarter) PreflightAssert(context.Context, scenario.RunSpec) error {
	return fmt.Errorf("%w: roster has 1 host, want 3", scenario.ErrAssertFailed)
}

// TestRunGate_AFailingAssertIsRefusedBeforeTheDeferral — the pre-flight assert
// answers alike whether the window is open now or not: a deferred run is not a
// way to postpone a 422 into an error_locked.
func TestRunGate_AFailingAssertIsRefusedBeforeTheDeferral(t *testing.T) {
	w, _ := slotAhead()
	db := gateDB([]maintenance.Window{w}, nil)
	starter := &assertFailingStarter{}
	h := NewIncarnationHandler(db, starter, nil, &fakeResolver{ok: true}, nil, nil, nil, nil)
	_, err := h.RunTyped(context.Background(), claims("archon-alice"), "redis-prod", "restart", nil,
		RunGate{InMaintenanceWindow: true})
	var pe *problemError
	if !errors.As(err, &pe) || pe.Details.Type != problem.TypeAssertFailed {
		t.Fatalf("err = %v, want %s", err, problem.TypeAssertFailed)
	}
	if db.voyageInsertArgs != nil || starter.calls != 0 {
		t.Fatal("a run whose assert failed was deferred or started")
	}
}

func TestRunGate_AWindowRunWaitsOutAFreezeWithoutConsent(t *testing.T) {
	sale := activeSale()
	db := gateDB(nil, []maintenance.Freeze{sale})
	starter := &fakeStarter{}
	h := NewIncarnationHandler(db, starter, nil, &fakeResolver{ok: true}, nil, nil, nil, nil)
	reply, err := h.RunTyped(context.Background(), claims("archon-alice"), "redis-prod", "restart", nil,
		RunGate{InMaintenanceWindow: true})
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if starter.calls != 0 || reply.Body.ScheduleAt == nil || !reply.Body.ScheduleAt.Equal(sale.EndsAt) {
		t.Fatalf("reply = %+v, want deferred to the freeze's end %s", reply.Body, sale.EndsAt)
	}
}

func TestRunGate_FreezeWithoutConsentIsRefused(t *testing.T) {
	db := gateDB(nil, []maintenance.Freeze{activeSale()})
	starter := &fakeStarter{}
	h := NewIncarnationHandler(db, starter, nil, &fakeResolver{ok: true}, nil, nil, nil, nil)
	_, err := h.RunTyped(context.Background(), claims("archon-alice"), "redis-prod", "restart", nil, RunGate{})
	wantFreezeActive(t, err)
	if starter.calls != 0 || db.voyageInsertArgs != nil {
		t.Fatal("a refused run started or was written")
	}
}

func TestRunGate_FreezeWithConsentStartsAndIsAudited(t *testing.T) {
	db := gateDB(nil, []maintenance.Freeze{activeSale()})
	starter := &fakeStarter{}
	h := NewIncarnationHandler(db, starter, nil, &fakeResolver{ok: true}, nil, nil, nil, nil)
	reply, err := h.RunTyped(context.Background(), claims("archon-alice"), "redis-prod", "restart", nil,
		RunGate{ConfirmFreeze: true})
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if starter.calls != 1 || reply.AuditPayload["confirm_freeze"] != true {
		t.Fatalf("starts = %d, payload = %v; want started with confirm_freeze recorded", starter.calls, reply.AuditPayload)
	}
}

func TestRunGate_RerunLastDuringAFreezeNeedsConsentBeforeTheUnlock(t *testing.T) {
	db := gateDB(nil, []maintenance.Freeze{activeSale()})
	starter := &fakeStarter{}
	h := newRerunHandler(db.fakeIncDB, starter, &fakeAuditWriter{})
	h.db = db
	_, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "retry", nil, false)
	wantFreezeActive(t, err)
	if strings.Contains(err.Error(), "in_maintenance_window") {
		t.Errorf("rerun-last refusal offers a field rerun-last does not accept: %v", err)
	}
	if starter.calls != 0 || len(db.execCalls) != 0 {
		t.Fatalf("starts=%d execs=%v: the refused rerun unlocked or started", starter.calls, db.execCalls)
	}
	if _, err := h.RerunLastTyped(context.Background(), claims("archon-alice"), "redis-prod", "retry", nil, true); err != nil {
		t.Fatalf("rerun with consent: %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("starts = %d after consent, want 1", starter.calls)
	}
}

// TestRunGate_DestroyDuringAFreezeNeedsConsent — NIM-1085: a destroy is an
// operator's act, refused during a freeze without confirm_freeze on both paths,
// before anything commits.
func TestRunGate_DestroyDuringAFreezeNeedsConsent(t *testing.T) {
	for _, force := range []bool{false, true} {
		db := gateDB(nil, []maintenance.Freeze{activeSale()})
		db.fakeIncDB.unlockSelectRow = func(_ string) pgx.Row { return makeUnlockSelectRow("ready") }
		destroyer := &fakeDestroyer{}
		aw := &fakeAuditWriter{}
		h := newDestroyHandler(db.fakeIncDB, destroyer, aw, true)
		h.db = db
		_, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", force, false)
		wantFreezeActive(t, err)
		if destroyer.calls != 0 || len(db.execCalls) != 0 || len(aw.events) != 0 {
			t.Fatalf("force=%v: the refused destroy left a trace: teardown=%d execs=%v audit=%d",
				force, destroyer.calls, db.execCalls, len(aw.events))
		}
		if _, err := h.DestroyTyped(context.Background(), claims("archon-alice"), "redis-prod", force, true); err != nil {
			t.Fatalf("force=%v: destroy with consent: %v", force, err)
		}
	}
}

func TestRunGate_UpgradeScenarioDuringAFreezeNeedsConsentBeforeTheSchema(t *testing.T) {
	mig, err := statemigrate.Parse([]byte("transform:\n  - set:\n      path: state.foo\n      value: bar\n"), 2, "migrations/002_set_foo/main.yml")
	if err != nil {
		t.Fatalf("parse migration: %v", err)
	}
	upgradeSelected := false
	db := gateDB(nil, []maintenance.Freeze{activeSale()})
	db.fakeIncDB.selectByNameRow = func(name string) pgx.Row { return makeIncRowVer(name, "v1", 1) }
	db.fakeIncDB.upgradeSelectRow = func(_ string) pgx.Row { upgradeSelected = true; return makeUpgradeSelectRow(1, "ready") }
	loader := &fakeLoader{
		targetSchema: 2,
		chain:        statemigrate.Chain{mig},
		upgrades:     []artifact.Scenario{{Name: "to_v2", FromVersions: []string{"v1"}}},
	}
	starter := &fakeStarter{}
	h := NewIncarnationHandler(db, starter, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	_, err = h.UpgradeTyped(context.Background(), claims("archon-alice"), "redis-prod", "v2", false)
	wantFreezeActive(t, err)
	if upgradeSelected || starter.calls != 0 {
		t.Fatalf("upgradeTx reached=%v starts=%d: the refused upgrade committed or started", upgradeSelected, starter.calls)
	}
	reply, err := h.UpgradeTyped(context.Background(), claims("archon-alice"), "redis-prod", "v2", true)
	if err != nil || starter.calls != 1 || reply.AuditPayload["confirm_freeze"] != true {
		t.Fatalf("upgrade with consent: err=%v starts=%d payload=%v", err, starter.calls, reply.AuditPayload)
	}
}

func TestPlanRunStart_RecordsEveryCoveringFreezeOnConsent(t *testing.T) {
	sale := activeSale()
	incident := sale
	incident.ID = "22222222-2222-4222-8222-222222222222"
	incident.EndsAt = sale.EndsAt.Add(time.Hour)
	db := gateDB(nil, []maintenance.Freeze{sale, incident})
	start, err := PlanRunStart(context.Background(), db, []string{"redis-prod"}, time.Now(), RunGate{ConfirmFreeze: true})
	if err != nil {
		t.Fatalf("PlanRunStart: %v", err)
	}
	got, _ := json.Marshal(start.ConfirmedFreezeIDs)
	if want, _ := json.Marshal([]string{sale.ID, incident.ID}); string(got) != string(want) {
		t.Errorf("ConfirmedFreezeIDs = %s, want %s", got, want)
	}
}

func TestFreezeActiveDetail_GroupsTargetsByFreeze(t *testing.T) {
	end := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	var frozen []FrozenIncarnation
	for i := range 12 {
		frozen = append(frozen, FrozenIncarnation{ID: "df-" + string(rune('a'+i)),
			Freeze: maintenance.Freeze{EndsAt: end, Comment: "sale"}})
	}
	frozen = append(frozen, FrozenIncarnation{ID: "pg-1", Freeze: maintenance.Freeze{EndsAt: end.Add(time.Hour), Comment: "incident"}})
	got := freezeActiveDetail(frozen, "")
	for _, want := range []string{
		"13 target incarnations",
		"df-a, df-b, df-c, df-d, df-e, df-f, df-g, df-h, df-i, df-j and 2 more frozen until 2026-12-02T00:00:00Z (\"sale\")",
		"pg-1 frozen until 2026-12-02T01:00:00Z (\"incident\")",
		"confirm_freeze: true to run during the freeze, or in_maintenance_window: true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail = %q\nmissing %q", got, want)
		}
	}
}

func TestFreezeActiveDetail_CapsTheFreezesListed(t *testing.T) {
	base := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	var frozen []FrozenIncarnation
	for i := range 25 {
		frozen = append(frozen, FrozenIncarnation{ID: fmt.Sprintf("df-%02d", i),
			Freeze: maintenance.Freeze{EndsAt: base.Add(time.Duration(i) * time.Hour)}})
	}
	got := freezeActiveDetail(frozen, FreezeActDestroy)
	if n := strings.Count(got, "frozen until"); n != maxFreezesNamed {
		t.Errorf("detail lists %d freezes, want %d: %q", n, maxFreezesNamed, got)
	}
	if !strings.Contains(got, "and 15 more under 15 more freezes") {
		t.Errorf("detail = %q, want the hidden count", got)
	}
	// An act that is never deferred offers consent alone.
	if !strings.HasSuffix(got, "send confirm_freeze: true to destroy during the freeze") || strings.Contains(got, "in_maintenance_window") {
		t.Errorf("destroy remedy = %q", got)
	}
}
