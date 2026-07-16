package soul

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// historyFakeDB — a fake [ExecQueryRower] for SelectHistory: QueryRow serves the
// count SQL (returns total), Query — the page SQL (returns rows). Captures the SQL and
// args of both calls for checking filters/the UNION form.
type historyFakeDB struct {
	total int

	countSQL  string
	countArgs []any
	querySQL  string
	queryArgs []any

	pageRows []staticRow
	queryErr error
	rowsErr  error
}

func (f *historyFakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("historyFakeDB: Exec not expected")
}

func (f *historyFakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.countSQL = sql
	f.countArgs = args
	return staticRow{values: []any{f.total}}
}

func (f *historyFakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.querySQL = sql
	f.queryArgs = args
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return &fakeRows{rows: f.pageRows, err: f.rowsErr}, nil
}

// scenarioStaticRow / errandStaticRow — staticRow constructors for the
// 9-column SelectHistory projection (type,id,incarnation,scenario,module,
// status,started_at,finished_at,voyage_id). Nullable columns are
// either a string or nil (assign wraps it into **string), finished is either
// a time.Time or nil (assign wraps it into **time.Time). voyage_id is always
// present (nilStr → NULL outside a Voyage) — wrappers with an explicit voyage are
// scenarioVoyageStaticRow/errandVoyageStaticRow below.
func scenarioStaticRow(id, incarnation, scenario, status string, started time.Time, finished any) staticRow {
	return scenarioVoyageStaticRow(id, incarnation, scenario, status, started, finished, "")
}

func errandStaticRow(id, module, status string, started time.Time, finished any) staticRow {
	return errandVoyageStaticRow(id, module, status, started, finished, "")
}

// scenarioVoyageStaticRow / errandVoyageStaticRow — variants with an explicit voyage_id
// (an empty string → SQL NULL).
func scenarioVoyageStaticRow(id, incarnation, scenario, status string, started time.Time, finished any, voyageID string) staticRow {
	return staticRow{values: []any{
		"scenario", id, incarnation, scenario, nilStr(""),
		status, started, finished, nilStr(voyageID),
	}}
}

func errandVoyageStaticRow(id, module, status string, started time.Time, finished any, voyageID string) staticRow {
	return staticRow{values: []any{
		"errand", id, nilStr(""), nilStr(""), module,
		status, started, finished, nilStr(voyageID),
	}}
}

// nilStr returns nil (→ SQL NULL) for an empty string, otherwise the string itself —
// the assign-helper maps string→**string pointer, nil→nil pointer.
func nilStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestSelectHistory_RequiresSID(t *testing.T) {
	_, _, err := SelectHistory(context.Background(), &historyFakeDB{}, HistoryFilter{}, 0, 50)
	if err == nil {
		t.Fatal("expected error for empty sid")
	}
}

func TestSelectHistory_MergeOrder(t *testing.T) {
	t0 := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	// Return rows already in DESC order (as ORDER BY in the DB would) —
	// SelectHistory does not re-sort, it only maps. Checks that the interleaved
	// scenario+errand merge is preserved and fields are split out correctly.
	db := &historyFakeDB{
		total: 3,
		pageRows: []staticRow{
			errandStaticRow("e2", "core.exec.run", "success", t0.Add(3*time.Hour), t0.Add(3*time.Hour+time.Minute)),
			scenarioStaticRow("a1", "web-prod", "deploy", "running", t0.Add(2*time.Hour), nil),
			errandStaticRow("e1", "core.pkg.installed", "failed", t0.Add(time.Hour), t0.Add(time.Hour+time.Minute)),
		},
	}
	items, total, err := SelectHistory(context.Background(), db, HistoryFilter{SID: "h.example.com"}, 0, 50)
	if err != nil {
		t.Fatalf("SelectHistory: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}

	// item[0] — errand e2.
	if items[0].Type != HistoryTypeErrand || items[0].ID != "e2" || items[0].Module != "core.exec.run" {
		t.Errorf("item0 = %+v", items[0])
	}
	if items[0].Incarnation != "" || items[0].Scenario != "" {
		t.Errorf("item0 scenario-only fields leaked: %+v", items[0])
	}
	// item[1] — scenario a1.
	if items[1].Type != HistoryTypeScenario || items[1].ID != "a1" ||
		items[1].Incarnation != "web-prod" || items[1].Scenario != "deploy" {
		t.Errorf("item1 = %+v", items[1])
	}
	if items[1].Module != "" {
		t.Errorf("item1 errand-only fields leaked: %+v", items[1])
	}
	if items[1].FinishedAt != nil {
		t.Errorf("item1 running должен иметь FinishedAt=nil, got %v", items[1].FinishedAt)
	}
	// item[2] — errand e1.
	if items[2].ID != "e1" || items[2].Status != "failed" {
		t.Errorf("item2 = %+v", items[2])
	}
	if items[2].FinishedAt == nil {
		t.Errorf("item2 terminal должен иметь FinishedAt")
	}

	// UNION ALL of both sources + ORDER BY started_at DESC in the page SQL.
	if !strings.Contains(db.querySQL, "FROM apply_runs") || !strings.Contains(db.querySQL, "FROM errands") {
		t.Errorf("page SQL должен включать оба источника: %s", db.querySQL)
	}
	if !strings.Contains(db.querySQL, "UNION ALL") {
		t.Errorf("page SQL должен быть UNION ALL: %s", db.querySQL)
	}
	if !strings.Contains(db.querySQL, "ORDER BY started_at DESC") {
		t.Errorf("page SQL должен сортировать started_at DESC: %s", db.querySQL)
	}
}

func TestSelectHistory_FilterTypeScenarioOnly(t *testing.T) {
	db := &historyFakeDB{total: 1, pageRows: []staticRow{
		scenarioStaticRow("a1", "web", "deploy", "success", time.Now().UTC(), nil),
	}}
	_, _, err := SelectHistory(context.Background(), db,
		HistoryFilter{SID: "h.example.com", Types: []HistoryType{HistoryTypeScenario}}, 0, 50)
	if err != nil {
		t.Fatalf("SelectHistory: %v", err)
	}
	if strings.Contains(db.querySQL, "FROM errands") {
		t.Errorf("type=scenario не должен включать errands: %s", db.querySQL)
	}
	if !strings.Contains(db.querySQL, "FROM apply_runs") {
		t.Errorf("type=scenario должен включать apply_runs: %s", db.querySQL)
	}
	if strings.Contains(db.querySQL, "UNION ALL") {
		t.Errorf("один источник — без UNION ALL: %s", db.querySQL)
	}
}

func TestSelectHistory_FilterTypeErrandOnly(t *testing.T) {
	db := &historyFakeDB{total: 1, pageRows: []staticRow{
		errandStaticRow("e1", "core.exec.run", "success", time.Now().UTC(), nil),
	}}
	_, _, err := SelectHistory(context.Background(), db,
		HistoryFilter{SID: "h.example.com", Types: []HistoryType{HistoryTypeErrand}}, 0, 50)
	if err != nil {
		t.Fatalf("SelectHistory: %v", err)
	}
	if strings.Contains(db.querySQL, "FROM apply_runs") {
		t.Errorf("type=errand не должен включать apply_runs: %s", db.querySQL)
	}
	if !strings.Contains(db.querySQL, "FROM errands") {
		t.Errorf("type=errand должен включать errands: %s", db.querySQL)
	}
}

func TestSelectHistory_SinceFilter(t *testing.T) {
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	db := &historyFakeDB{total: 0}
	_, _, err := SelectHistory(context.Background(), db,
		HistoryFilter{SID: "h.example.com", Since: since}, 0, 50)
	if err != nil {
		t.Fatalf("SelectHistory: %v", err)
	}
	if !strings.Contains(db.querySQL, "started_at > $2") {
		t.Errorf("since должен добавить started_at > $2: %s", db.querySQL)
	}
	// args: $1=sid, $2=since, $3=limit, $4=offset.
	if len(db.queryArgs) != 4 {
		t.Fatalf("queryArgs len = %d, want 4: %v", len(db.queryArgs), db.queryArgs)
	}
	if db.queryArgs[1] != since {
		t.Errorf("queryArgs[1] = %v, want %v", db.queryArgs[1], since)
	}
}

func TestSelectHistory_Pagination(t *testing.T) {
	db := &historyFakeDB{total: 137}
	_, total, err := SelectHistory(context.Background(), db,
		HistoryFilter{SID: "h.example.com"}, 50, 25)
	if err != nil {
		t.Fatalf("SelectHistory: %v", err)
	}
	if total != 137 {
		t.Errorf("total = %d, want 137", total)
	}
	// args without since: $1=sid, $2=limit, $3=offset.
	if len(db.queryArgs) != 3 {
		t.Fatalf("queryArgs len = %d, want 3: %v", len(db.queryArgs), db.queryArgs)
	}
	if db.queryArgs[1] != 25 || db.queryArgs[2] != 50 {
		t.Errorf("LIMIT/OFFSET args = %v, want [_,25,50]", db.queryArgs)
	}
	if !strings.Contains(db.querySQL, "LIMIT $2 OFFSET $3") {
		t.Errorf("page SQL LIMIT/OFFSET плейсхолдеры: %s", db.querySQL)
	}
}

func TestSelectHistory_Empty(t *testing.T) {
	db := &historyFakeDB{total: 0, pageRows: nil}
	items, total, err := SelectHistory(context.Background(), db,
		HistoryFilter{SID: "h.example.com"}, 0, 50)
	if err != nil {
		t.Fatalf("SelectHistory: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Errorf("empty: total=%d len=%d, want 0/0", total, len(items))
	}
}

func TestSelectHistory_QueryError(t *testing.T) {
	db := &historyFakeDB{total: 5, queryErr: errors.New("boom")}
	_, _, err := SelectHistory(context.Background(), db,
		HistoryFilter{SID: "h.example.com"}, 0, 50)
	if err == nil {
		t.Fatal("expected error on Query failure")
	}
}

func TestValidHistoryType(t *testing.T) {
	for _, ok := range []HistoryType{HistoryTypeScenario, HistoryTypeErrand} {
		if !ValidHistoryType(ok) {
			t.Errorf("ValidHistoryType(%q) = false, want true", ok)
		}
	}
	for _, bad := range []HistoryType{"push", "", "Scenario"} {
		if ValidHistoryType(bad) {
			t.Errorf("ValidHistoryType(%q) = true, want false", bad)
		}
	}
}

// TestSelectHistory_VoyageBacklink — voyage_id is resolved via voyage_targets:
// scenario (via apply_id), command/errand (via errand_id), and NULL for a direct
// run without a Voyage. Checks both the SQL form (LEFT JOIN voyage_targets) and the
// mapping of the column into HistoryItem.VoyageID.
func TestSelectHistory_VoyageBacklink(t *testing.T) {
	t0 := time.Date(2026, 5, 30, 9, 0, 0, 0, time.UTC)
	db := &historyFakeDB{
		total: 3,
		pageRows: []staticRow{
			// scenario with a Voyage (vt.apply_id match).
			scenarioVoyageStaticRow("a1", "web-prod", "deploy", "success", t0.Add(2*time.Hour), t0.Add(2*time.Hour+time.Minute), "vy-1"),
			// errand with a Voyage (vt.errand_id match).
			errandVoyageStaticRow("e1", "core.cmd.run", "success", t0.Add(time.Hour), t0.Add(time.Hour+time.Minute), "vy-2"),
			// a direct incarnation.run without a Voyage → NULL.
			scenarioVoyageStaticRow("a2", "db-prod", "restart", "success", t0, t0.Add(time.Minute), ""),
		},
	}
	items, _, err := SelectHistory(context.Background(), db, HistoryFilter{SID: "h.example.com"}, 0, 50)
	if err != nil {
		t.Fatalf("SelectHistory: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}

	if items[0].VoyageID == nil || *items[0].VoyageID != "vy-1" {
		t.Errorf("item0 (scenario) VoyageID = %v, want vy-1", items[0].VoyageID)
	}
	if items[1].VoyageID == nil || *items[1].VoyageID != "vy-2" {
		t.Errorf("item1 (errand) VoyageID = %v, want vy-2", items[1].VoyageID)
	}
	if items[2].VoyageID != nil {
		t.Errorf("item2 (direct run) VoyageID = %v, want nil", items[2].VoyageID)
	}

	// SQL form: both arms LEFT JOIN voyage_targets on their own dispatch references.
	if !strings.Contains(db.querySQL, "LEFT JOIN voyage_targets vt ON vt.apply_id = apply_runs.apply_id") {
		t.Errorf("scenario-плечо должно LEFT JOIN-ить voyage_targets по apply_id: %s", db.querySQL)
	}
	if !strings.Contains(db.querySQL, "LEFT JOIN voyage_targets vt ON vt.errand_id = errands.errand_id") {
		t.Errorf("errand-плечо должно LEFT JOIN-ить voyage_targets по errand_id: %s", db.querySQL)
	}
}
