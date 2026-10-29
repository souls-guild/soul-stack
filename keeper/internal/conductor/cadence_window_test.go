package conductor

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// valueRows is a pgx.Rows over prepared column values, assigned by type.
type valueRows struct {
	vals [][]any
	pos  int
}

func (r *valueRows) Next() bool { r.pos++; return r.pos <= len(r.vals) }
func (r *valueRows) Scan(dest ...any) error {
	for i, v := range r.vals[r.pos-1] {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(v))
	}
	return nil
}
func (r *valueRows) Close()                                       {}
func (r *valueRows) Err() error                                   { return nil }
func (r *valueRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *valueRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *valueRows) Values() ([]any, error)                       { return nil, nil }
func (r *valueRows) RawValues() [][]byte                          { return nil }
func (r *valueRows) Conn() *pgx.Conn                              { return nil }

var everyDay = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// 2026-11-02 is a Monday.
func nov(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC)
}

func window(inc string, days []string, start string, minutes int) maintenance.Window {
	return maintenance.Window{ID: inc + "-w", IncarnationID: inc, Days: days, StartTime: start,
		DurationMinutes: minutes, Timezone: "UTC"}
}

func freeze(inc string, from, to time.Time) maintenance.Freeze {
	return maintenance.Freeze{ID: inc + "-f", IncarnationID: inc, StartsAt: from, EndsAt: to, Comment: "sale"}
}

// cronScenarioCadence fires daily at 14:00 UTC and is due at that slot.
func cronScenarioCadence(policy cadence.OverlapPolicy, inWindow bool) *cadence.Cadence {
	expr := "0 14 * * *"
	next := nov(2, 14, 0)
	name := "converge"
	return &cadence.Cadence{
		ID:                  "01H000000000000000000CAD00",
		Name:                "daily",
		Enabled:             true,
		ScheduleKind:        cadence.ScheduleKindCron,
		CronExpr:            &expr,
		OverlapPolicy:       policy,
		Kind:                cadence.KindScenario,
		ScenarioName:        &name,
		Target:              json.RawMessage(`{"service":"redis"}`),
		NextRunAt:           &next,
		CreatedByAID:        "archon-alice",
		InMaintenanceWindow: inWindow,
	}
}

func spawnAt(t *testing.T, tx *spawnFakeTx, c *cadence.Cadence, resolved []string, now time.Time) (*spawnedRecord, bool) {
	t.Helper()
	s := newSpawnerFor(tx, stubResolver{out: resolved}, nil)
	rec, spawned, err := s.processOne(context.Background(), tx, c, now)
	if err != nil {
		t.Fatalf("processOne: %v", err)
	}
	return rec, spawned
}

// insertedStart returns the spawned Voyage's schedule_at and status, read from
// voyage.Insert's positional arguments.
func insertedStart(t *testing.T, tx *spawnFakeTx) (any, string) {
	t.Helper()
	if tx.insertCalls != 1 {
		t.Fatalf("voyage.Insert calls = %d, want 1", tx.insertCalls)
	}
	return tx.voyageInsertArgs[11], tx.voyageInsertArgs[15].(string)
}

func TestProcessOne_InMaintenanceWindow_CronAt1400IsScheduledAt0200(t *testing.T) {
	tx := &spawnFakeTx{windows: []maintenance.Window{window("redis-prod", everyDay, "02:00", 180)}}
	rec, spawned := spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicySkip, true), []string{"redis-prod"}, nov(2, 14, 0))
	if !spawned {
		t.Fatal("not spawned")
	}
	scheduleAt, status := insertedStart(t, tx)
	if scheduleAt != nov(3, 2, 0) || status != string(voyage.StatusScheduled) {
		t.Fatalf("Voyage schedule_at = %v status = %q; want %v scheduled", scheduleAt, status, nov(3, 2, 0))
	}
	if rec.scheduleAt == nil || !rec.scheduleAt.Equal(nov(3, 2, 0)) {
		t.Fatalf("record schedule_at = %v; want %v", rec.scheduleAt, nov(3, 2, 0))
	}
	// The claim asks the gate again from the row: it must still ask for the window.
	if got := tx.voyageInsertArgs[22]; got != true {
		t.Fatalf("Voyage in_maintenance_window = %v; want true", got)
	}

	ad := &spawnAudit{}
	newSpawnerFor(tx, stubResolver{}, ad).emit(context.Background(), rec)
	if got := ad.events[0].Payload["schedule_at"]; got != nov(3, 2, 0) {
		t.Fatalf("cadence.spawned schedule_at = %v; want %v", got, nov(3, 2, 0))
	}
}

func TestProcessOne_InMaintenanceWindow_OpenNowStartsAtOnce(t *testing.T) {
	tx := &spawnFakeTx{windows: []maintenance.Window{window("redis-prod", everyDay, "13:00", 180)}}
	_, spawned := spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicySkip, true), []string{"redis-prod"}, nov(2, 14, 0))
	if !spawned {
		t.Fatal("not spawned")
	}
	if scheduleAt, status := insertedStart(t, tx); scheduleAt != nil || status != string(voyage.StatusPending) {
		t.Fatalf("Voyage schedule_at = %v status = %q; want none, pending", scheduleAt, status)
	}
}

func TestProcessOne_InMaintenanceWindow_FreezePushesPastItsEnd(t *testing.T) {
	tx := &spawnFakeTx{
		windows: []maintenance.Window{window("redis-prod", everyDay, "02:00", 180)},
		freezes: []maintenance.Freeze{freeze("redis-prod", nov(3, 1, 0), nov(3, 3, 30))},
	}
	spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicySkip, true), []string{"redis-prod"}, nov(2, 14, 0))
	if scheduleAt, _ := insertedStart(t, tx); scheduleAt != nov(3, 3, 30) {
		t.Fatalf("Voyage schedule_at = %v; want the freeze's end inside the window, %v", scheduleAt, nov(3, 3, 30))
	}
}

// The guard of "an automatic run never starts during a freeze": a Cadence
// without in_maintenance_window, ticking inside a freeze, starts when the freeze
// ends — not at its tick, and not at the next window either.
func TestProcessOne_WithoutFlag_FreezeDefersToItsEnd_NotToTheWindow(t *testing.T) {
	tx := &spawnFakeTx{
		windows: []maintenance.Window{window("redis-prod", everyDay, "02:00", 180)},
		freezes: []maintenance.Freeze{
			freeze("redis-prod", nov(2, 10, 0), nov(2, 16, 0)),
			freeze("redis-prod", nov(2, 16, 0), nov(2, 18, 0)),
		},
	}
	_, spawned := spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicySkip, false), []string{"redis-prod"}, nov(2, 14, 0))
	if !spawned {
		t.Fatal("not spawned")
	}
	scheduleAt, status := insertedStart(t, tx)
	if scheduleAt != nov(2, 18, 0) || status != string(voyage.StatusScheduled) {
		t.Fatalf("Voyage schedule_at = %v status = %q; want %v scheduled", scheduleAt, status, nov(2, 18, 0))
	}
}

func TestProcessOne_WithoutFlag_NoFreeze_StartsAtItsTick(t *testing.T) {
	tx := &spawnFakeTx{windows: []maintenance.Window{window("redis-prod", everyDay, "02:00", 180)}}
	spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicySkip, false), []string{"redis-prod"}, nov(2, 14, 0))
	if scheduleAt, status := insertedStart(t, tx); scheduleAt != nil || status != string(voyage.StatusPending) {
		t.Fatalf("Voyage schedule_at = %v status = %q; want none, pending", scheduleAt, status)
	}
}

func TestProcessOne_InMaintenanceWindow_NoCommonWindow_IsARecordedSkip(t *testing.T) {
	tx := &spawnFakeTx{windows: []maintenance.Window{
		window("redis-b", []string{"tue"}, "02:00", 60),
		window("redis-a", []string{"mon"}, "02:00", 60),
	}}
	rec, spawned := spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicyParallel, true), []string{"redis-b", "redis-a"}, nov(2, 14, 0))
	if spawned || tx.insertCalls != 0 {
		t.Fatalf("spawned = %v, voyage inserts = %d; want a skip", spawned, tx.insertCalls)
	}
	if tx.execCalls != 1 {
		t.Fatalf("AdvanceSchedule calls = %d; want 1, the series must not wedge", tx.execCalls)
	}
	if rec == nil || rec.skipReason != skipReasonNoWindow {
		t.Fatalf("record = %+v; want a no_window skip", rec)
	}
	// The gate and the skip's explanation read one set of rules: the skip
	// recurs at every slot, inside the transaction that locks the batch.
	if tx.windowQueries != 1 {
		t.Fatalf("window reads = %d; want 1", tx.windowQueries)
	}

	ad := &spawnAudit{}
	newSpawnerFor(tx, stubResolver{}, ad).emit(context.Background(), rec)
	ev := ad.events[0]
	if ev.EventType != audit.EventCadenceSkippedNoWindow || ev.CorrelationID != rec.cadenceID {
		t.Fatalf("event = %s / %s; want %s / %s", ev.EventType, ev.CorrelationID, audit.EventCadenceSkippedNoWindow, rec.cadenceID)
	}
	want := []map[string]any{
		{"incarnation": "redis-a", "next_start": nov(9, 2, 0)},
		{"incarnation": "redis-b", "next_start": nov(3, 2, 0)},
	}
	if got := ev.Payload["next_starts"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("next_starts = %v; want %v", got, want)
	}
	if ev.Payload["incarnation_count"] != 2 || ev.Payload["reason"] != skipReasonNoWindow {
		t.Fatalf("payload = %v", ev.Payload)
	}
}

// An incarnation with neither windows nor freezes is always open: it is never
// the reason, so it takes no row from the ones that are.
func TestProcessOne_InMaintenanceWindow_NoCommonWindow_ListsOnlyIncarnationsWithRules(t *testing.T) {
	resolved := []string{"redis-a", "redis-b"}
	for i := range noWindowListMax + 5 {
		resolved = append(resolved, "free-"+string(rune('a'+i/26))+string(rune('a'+i%26)))
	}
	tx := &spawnFakeTx{windows: []maintenance.Window{
		window("redis-a", []string{"mon"}, "02:00", 60),
		window("redis-b", []string{"tue"}, "02:00", 60),
	}}
	rec, spawned := spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicySkip, true), resolved, nov(2, 14, 0))
	if spawned || rec == nil || rec.noWindow == nil {
		t.Fatalf("spawned = %v record = %+v; want a no_window skip", spawned, rec)
	}
	if len(rec.noWindow.rows) != 2 || rec.noWindow.incarnations != len(resolved) {
		t.Fatalf("rows = %v count = %d; want redis-a and redis-b of %d", rec.noWindow.rows, rec.noWindow.incarnations, len(resolved))
	}
}

func TestProcessOne_InMaintenanceWindow_UncompilableWindowIsNamedFirst(t *testing.T) {
	var windows []maintenance.Window
	var resolved []string
	for i := range noWindowListMax + 5 {
		inc := "a-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		resolved = append(resolved, inc)
		windows = append(windows, window(inc, everyDay, "02:00", 60))
	}
	broken := window("z-broken", everyDay, "02:00", 60)
	broken.Timezone = "Mars/Olympus_Mons"
	windows = append(windows, broken)
	resolved = append(resolved, "z-broken")

	tx := &spawnFakeTx{windows: windows}
	rec, spawned := spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicySkip, true), resolved, nov(2, 14, 0))
	if spawned || rec == nil || rec.noWindow == nil {
		t.Fatalf("spawned = %v record = %+v; want a no_window skip", spawned, rec)
	}
	if len(rec.noWindow.rows) != noWindowListMax || rec.noWindow.incarnations != len(resolved) {
		t.Fatalf("rows = %d count = %d; want %d, %d", len(rec.noWindow.rows), rec.noWindow.incarnations, noWindowListMax, len(resolved))
	}
	first := rec.noWindow.rows[0]
	if first["incarnation"] != "z-broken" || first["error"] == nil {
		t.Fatalf("first row = %v; want the broken incarnation with its error", first)
	}
}

func TestProcessOne_Parallel_DeferredChildHoldsTheSpawn(t *testing.T) {
	tx := &spawnFakeTx{deferred: true, live: true}
	rec, spawned := spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicyParallel, true), []string{"redis-prod"}, nov(2, 15, 0))
	if spawned || tx.insertCalls != 0 {
		t.Fatalf("spawned = %v inserts = %d; want the spawn held back", spawned, tx.insertCalls)
	}
	if rec == nil || rec.skipReason != skipReasonOverlap || tx.execCalls != 1 {
		t.Fatalf("record = %+v advance = %d; want an overlap skip that advances", rec, tx.execCalls)
	}
	if tx.hasLiveCalls != 0 {
		t.Fatalf("HasLiveChild calls = %d; parallel must not ask about running children", tx.hasLiveCalls)
	}
}

// A child waiting out a freeze holds parallel back as well: without the flag
// the spawn is still deferred, and the ticks of the freeze would pile up.
func TestProcessOne_Parallel_ChildWaitingOutAFreezeHoldsTheSpawn(t *testing.T) {
	tx := &spawnFakeTx{deferred: true}
	rec, spawned := spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicyParallel, false), []string{"redis-prod"}, nov(2, 15, 0))
	if spawned || tx.insertCalls != 0 || rec == nil || rec.skipReason != skipReasonOverlap {
		t.Fatalf("spawned = %v inserts = %d record = %+v; want an overlap skip", spawned, tx.insertCalls, rec)
	}
}

func TestProcessOne_Parallel_RunningChildDoesNotHold(t *testing.T) {
	tx := &spawnFakeTx{live: true}
	_, spawned := spawnAt(t, tx, cronScenarioCadence(cadence.OverlapPolicyParallel, true), []string{"redis-prod"}, nov(2, 15, 0))
	if !spawned || tx.deferredCalls != 1 {
		t.Fatalf("spawned = %v deferred checks = %d; want a spawn after one check", spawned, tx.deferredCalls)
	}
}

func TestProcessOne_Command_ReadsNoWindowsAndNoDeferredChild(t *testing.T) {
	c := cronScenarioCadence(cadence.OverlapPolicyParallel, false)
	mod := "core.pkg.present"
	c.Kind, c.ScenarioName, c.Module = cadence.KindCommand, nil, &mod
	// A freeze on a name the command target happens to share must not matter:
	// a host has no window.
	tx := &spawnFakeTx{freezes: []maintenance.Freeze{freeze("web-01", nov(2, 10, 0), nov(2, 18, 0))}}
	_, spawned := spawnAt(t, tx, c, []string{"web-01"}, nov(2, 14, 0))
	if !spawned || tx.deferredCalls != 0 {
		t.Fatalf("spawned = %v deferred checks = %d; want a spawn and no check", spawned, tx.deferredCalls)
	}
	if scheduleAt, status := insertedStart(t, tx); scheduleAt != nil || status != string(voyage.StatusPending) {
		t.Fatalf("Voyage schedule_at = %v status = %q; want none, pending", scheduleAt, status)
	}
}

func TestSpawner_Run_UsesItsClock(t *testing.T) {
	c := cronScenarioCadence(cadence.OverlapPolicySkip, true)
	tx := &spawnFakeTx{
		dueRows: []*cadence.Cadence{c},
		windows: []maintenance.Window{window("redis-prod", everyDay, "02:00", 180)},
	}
	s := newCadenceSpawnerFromBeginner(spawnOneBeginner{tx: tx}, stubResolver{out: []string{"redis-prod"}}, stubResolver{}, nil, nil)
	s.now = func() time.Time { return nov(2, 14, 0) }
	if _, err := s.Run(context.Background(), 0, 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if scheduleAt, _ := insertedStart(t, tx); scheduleAt != nov(3, 2, 0) {
		t.Fatalf("Voyage schedule_at = %v; want %v", scheduleAt, nov(3, 2, 0))
	}
}
