package reaper

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// fakeMaintenanceDB answers the two expired-row deletes with one prepared row
// each and records the retention and the limit each was asked with.
type fakeMaintenanceDB struct {
	mu        sync.Mutex
	retention map[string]float64
	limit     map[string]int
}

func (f *fakeMaintenanceDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retention == nil {
		f.retention, f.limit = map[string]float64{}, map[string]int{}
	}
	end := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	start := end.Add(-time.Hour)
	switch {
	case strings.Contains(sql, "DELETE FROM incarnation_maintenance_windows"):
		f.retention["windows"], f.limit["windows"] = args[0].(float64), args[1].(int)
		return &fakeRows{rows: [][]any{{"w1", "redis-prod", "once", []string(nil), "", 0, "",
			&start, &end, "hotfix", start, start}}}, nil
	case strings.Contains(sql, "DELETE FROM incarnation_freezes"):
		f.retention["freezes"], f.limit["freezes"] = args[0].(float64), args[1].(int)
		return &fakeRows{rows: [][]any{{"f1", "redis-prod", start, end, "sale", start, start}}}, nil
	}
	panic("unexpected query: " + sql)
}

func (f *fakeMaintenanceDB) QueryRow(context.Context, string, ...any) pgx.Row { panic("unused") }
func (f *fakeMaintenanceDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unused")
}

func (f *fakeMaintenanceDB) asked() (map[string]float64, map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.retention, f.limit
}

type fakeRows struct {
	rows [][]any
	i    int
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return nil }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Next() bool                                   { r.i++; return r.i <= len(r.rows) }
func (r *fakeRows) Values() ([]any, error)                       { return r.rows[r.i-1], nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeRows) Scan(dest ...any) error {
	for i, v := range r.rows[r.i-1] {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(v))
	}
	return nil
}

func newMaintenanceRunner(t *testing.T, yamlTail string) (*Runner, *fakeMaintenanceDB, *fakeAuditWriter) {
	t.Helper()
	db, aud := &fakeMaintenanceDB{}, &fakeAuditWriter{}
	rn, err := NewRunner(Deps{
		Purger: &fakePurger{}, Redis: newTestRedis(t), Store: newTestStore(t, testKeeperYAML+yamlTail),
		Holder: "keeper-test-01", Logger: silentLogger(),
		ExpiredMaintenance: &MaintenancePurger{pool: db, audit: aud},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return rn, db, aud
}

func TestPurgeExpiredMaintenance_DefaultRetentionIs72h(t *testing.T) {
	rn, db, aud := newMaintenanceRunner(t, "")
	rn.dispatch(context.Background(), rn.currentCfg.Load())

	retention, limit := db.asked()
	if retention["windows"] != (72*time.Hour).Seconds() || retention["freezes"] != (72*time.Hour).Seconds() {
		t.Errorf("retention asked = %v, want 72h for both", retention)
	}
	if limit["windows"] != 200 || limit["freezes"] != 200 {
		t.Errorf("limit asked = %v, want reaper.batch_size 200 for both", limit)
	}
	if len(aud.events) != 2 {
		t.Fatalf("audit = %v, want one event per deleted row", aud.events)
	}
	for i, want := range []audit.EventType{audit.EventMaintenanceWindowDeleted, audit.EventFreezeDeleted} {
		ev := aud.events[i]
		if ev.EventType != want || ev.Source != audit.SourceKeeperInternal || ev.ArchonAID != "" ||
			ev.Payload["reason"] != "expired" || ev.Payload["incarnation_id"] != "redis-prod" {
			t.Errorf("audit[%d] = %+v, want %s by the Reaper with reason expired", i, ev, want)
		}
	}
	if p := aud.events[0].Payload; p["kind"] != "once" || p["ends_at"] != "2026-10-01T03:00:00Z" {
		t.Errorf("maintenance_window.deleted payload = %v, want the once window's fields", p)
	}
}

// TestPurgeExpiredMaintenance_ZeroKeepsEverything: a retention of 0 means the
// rule does not run at all, and each key switches only its own rule.
func TestPurgeExpiredMaintenance_ZeroKeepsEverything(t *testing.T) {
	rn, db, aud := newMaintenanceRunner(t, "\nmaintenance:\n  window_retention: 0s\n  freeze_retention: 90m\n")
	rn.dispatch(context.Background(), rn.currentCfg.Load())

	retention, _ := db.asked()
	if _, asked := retention["windows"]; asked {
		t.Errorf("window_retention 0 still deleted windows (retention %v)", retention["windows"])
	}
	if retention["freezes"] != (90 * time.Minute).Seconds() {
		t.Errorf("freeze retention asked = %v, want 90m", retention["freezes"])
	}
	if len(aud.events) != 1 || aud.events[0].EventType != audit.EventFreezeDeleted {
		t.Errorf("audit = %v, want the freeze alone", aud.events)
	}
}

func TestPurgeExpiredMaintenance_DryRunDeletesNothing(t *testing.T) {
	rn, db, aud := newMaintenanceRunner(t, "")
	cfg := *rn.currentCfg.Load()
	reaperCfg := *cfg.Reaper
	reaperCfg.DryRun = true
	cfg.Reaper = &reaperCfg
	rn.dispatch(context.Background(), &cfg)
	if retention, _ := db.asked(); len(retention) != 0 || len(aud.events) != 0 {
		t.Errorf("dry_run asked %v and audited %v, want nothing", retention, aud.events)
	}
}
