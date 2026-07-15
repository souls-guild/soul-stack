package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/statepredicate"
)

// Slice S2: StateLister — a production adapter of statepredicate.IncarnationStateLister
// over SelectAll. Unit tests via a fake-db (without PG): verify (1) the
// BaseFilter→ListFilter mapping (service/coven pushdown reaches SQL args), (2) real
// page-by-page draining (a set > one page → all rows visited via offset/limit,
// none lost), (3) error propagation. Integration with a live PG is a separate
// integration test (build-tag).

// statePageDB — a fake-db modeling SelectAll for StateLister: COUNT returns
// total, SELECT — a slice of rows by offset/limit from args. Captures COUNT args
// (to check service/coven pushdown). Each row is 14 columns of scanIncarnation.
type statePageDB struct {
	rows       []*Incarnation // full set (simulates an already-narrowed SQL result)
	countArgs  []any          // args of the COUNT query (= base-pushdown bind params)
	queryArgs  [][]any        // args of each SELECT (to check the offset/limit loop)
	queryCalls int
	selectErr  error // if set — SELECT returns an error
}

func (d *statePageDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("statePageDB: Exec не ожидается")
}

func (d *statePageDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "COUNT(*)") {
		d.countArgs = args
		return stateCountRow{total: len(d.rows)}
	}
	return errRow{err: pgx.ErrNoRows}
}

func (d *statePageDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "SELECT name") {
		return nil, fmt.Errorf("statePageDB: неожиданный Query: %s", sql)
	}
	d.queryCalls++
	d.queryArgs = append(d.queryArgs, args)
	if d.selectErr != nil {
		return nil, d.selectErr
	}
	// The last two bind params of SelectAll are OFFSET, LIMIT.
	offset := args[len(args)-2].(int)
	limit := args[len(args)-1].(int)
	end := offset + limit
	if end > len(d.rows) {
		end = len(d.rows)
	}
	var page []staticRow
	if offset < len(d.rows) {
		for _, inc := range d.rows[offset:end] {
			page = append(page, incToStaticRow(inc))
		}
	}
	return &fakeRows{rows: page}, nil
}

// stateCountRow — a Row for COUNT(*): scans total into *int.
type stateCountRow struct{ total int }

func (r stateCountRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("stateCountRow: ожидался один dest")
	}
	*dest[0].(*int) = r.total
	return nil
}

// incToStaticRow packs an Incarnation into a 16-column staticRow in
// scanIncarnation order (name, service, service_version, state_schema_version, spec,
// state, status, status_details, created_by_aid, created_at, updated_at, covens,
// traits, last_drift_check_at, last_drift_summary, created_scenario).
func incToStaticRow(inc *Incarnation) staticRow {
	stateBytes, _ := json.Marshal(inc.State)
	specBytes := []byte("{}")
	now := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	// nil columns are passed as an untyped nil: assign distinguishes NULL by
	// `src == nil` (a typed nil pointer in interface{} != nil).
	return staticRow{values: []any{
		inc.Name,
		inc.Service,
		"v1",
		1,
		specBytes,
		stateBytes,
		string(StatusReady),
		nil, // status_details
		nil, // created_by_aid
		now,
		now,
		nil,          // covens
		[]byte("{}"), // traits
		nil,          // last_drift_check_at
		nil,          // last_drift_summary
		"create",     // created_scenario
		nil,          // applying_apply_id
	}}
}

func newStated(name string, state map[string]any) *Incarnation {
	return &Incarnation{Name: name, Service: "redis", State: state}
}

func collectPages(t *testing.T, l *StateLister, base statepredicate.BaseFilter) []statepredicate.Stated {
	t.Helper()
	var all []statepredicate.Stated
	err := l.ListStatePages(context.Background(), base, func(page []statepredicate.Stated) error {
		all = append(all, page...)
		return nil
	})
	if err != nil {
		t.Fatalf("ListStatePages: %v", err)
	}
	return all
}

// --- pushdown: BaseFilter (service/coven) reaches SQL as bind-args ---

func TestStateLister_PushdownToSQL(t *testing.T) {
	db := &statePageDB{rows: []*Incarnation{
		newStated("a", map[string]any{"k": "v"}),
	}}
	l := NewStateLister(db)

	_ = collectPages(t, l, statepredicate.BaseFilter{Service: "redis", Coven: "prod"})

	// buildListWhere for Service+Coven binds two parameters: service, coven.
	if len(db.countArgs) != 2 {
		t.Fatalf("COUNT получил %d args, want 2 (service+coven pushdown)", len(db.countArgs))
	}
	if db.countArgs[0] != "redis" {
		t.Errorf("первый bind=%v, want redis (service pushdown)", db.countArgs[0])
	}
	if db.countArgs[1] != "prod" {
		t.Errorf("второй bind=%v, want prod (coven pushdown)", db.countArgs[1])
	}
}

// --- BaseFilter.Covens (ADDITIVE multi-coven) → coven∪{name} scope in SQL ---

// TestStateLister_MultiCovenScope_PushdownCovenUnionName — the ADDITIVE field
// BaseFilter.Covens (S3b-3) maps into ListScope coven∪{name}: both the covens[]
// intersection and the name equality reach SQL as one bind. The old path
// (single Coven via ListFilter) is NOT engaged here.
func TestStateLister_MultiCovenScope_PushdownCovenUnionName(t *testing.T) {
	db := &statePageDB{rows: []*Incarnation{newStated("a", map[string]any{"k": "v"})}}
	l := NewStateLister(db)

	_ = collectPages(t, l, statepredicate.BaseFilter{Covens: []string{"redis-prod"}})

	// countArgs = [scope-covens] (one bind for both arms of coven∪{name}).
	if len(db.countArgs) != 1 {
		t.Fatalf("COUNT получил %d args, want 1 (coven∪{name} один bind)", len(db.countArgs))
	}
	covs, ok := db.countArgs[0].([]string)
	if !ok || len(covs) != 1 || covs[0] != "redis-prod" {
		t.Errorf("scope-covens bind = %v, want [redis-prod]", db.countArgs[0])
	}
}

// TestStateLister_NoCovens_UnrestrictedScope — without BaseFilter.Covens the list
// drains WITHOUT scope narrowing (Unrestricted): state-CEL resolves over the whole
// service-narrowed set (a typical S3b-3 List, where coven and state are
// independent OR dimensions). countArgs is empty (no service, no coven, no scope).
func TestStateLister_NoCovens_UnrestrictedScope(t *testing.T) {
	db := &statePageDB{rows: []*Incarnation{newStated("a", map[string]any{"k": "v"})}}
	l := NewStateLister(db)

	_ = collectPages(t, l, statepredicate.BaseFilter{})

	if len(db.countArgs) != 0 {
		t.Errorf("без Covens scope обязан быть Unrestricted (без bind-args), got %v", db.countArgs)
	}
}

// --- page-by-page: a set larger than one page → several SELECTs, all rows visited ---

func TestStateLister_PageByPage(t *testing.T) {
	// 2.5 pages (statePageSize=1000) — check multi-page draining and that
	// no row is lost at the boundaries. Each incarnation is unique by name.
	const n = statePageSize*2 + 17
	rows := make([]*Incarnation, n)
	for i := range rows {
		rows[i] = newStated(fmt.Sprintf("inc-%05d", i), map[string]any{"idx": i})
	}
	db := &statePageDB{rows: rows}
	l := NewStateLister(db)

	got := collectPages(t, l, statepredicate.BaseFilter{Service: "redis"})

	if len(got) != n {
		t.Fatalf("обойдено %d строк, want %d (ни одна не потеряна)", len(got), n)
	}
	// Three pages (1000 + 1000 + 17).
	if db.queryCalls != 3 {
		t.Fatalf("SELECT-ов %d, want 3 (offset/limit-цикл по страницам)", db.queryCalls)
	}
	// Offsets increase by the page size.
	wantOffsets := []int{0, statePageSize, statePageSize * 2}
	for i, args := range db.queryArgs {
		off := args[len(args)-2].(int)
		if off != wantOffsets[i] {
			t.Errorf("страница %d: offset=%d, want %d", i, off, wantOffsets[i])
		}
	}
	// All names are unique and present.
	seen := make(map[string]bool, n)
	for _, s := range got {
		if seen[s.Name] {
			t.Fatalf("дубликат %q (страница перечитана)", s.Name)
		}
		seen[s.Name] = true
		if s.State == nil {
			t.Fatalf("%q: state не пробросился", s.Name)
		}
	}
}

// --- exactly one full page → one SELECT, no extra request ---

func TestStateLister_ExactPage(t *testing.T) {
	rows := make([]*Incarnation, statePageSize)
	for i := range rows {
		rows[i] = newStated(fmt.Sprintf("inc-%05d", i), map[string]any{"idx": i})
	}
	db := &statePageDB{rows: rows}
	l := NewStateLister(db)

	got := collectPages(t, l, statepredicate.BaseFilter{})
	if len(got) != statePageSize {
		t.Fatalf("got %d, want %d", len(got), statePageSize)
	}
	// total==statePageSize: offset+len >= total on the first page → a second SELECT
	// is not needed (offset+1000 >= 1000).
	if db.queryCalls != 1 {
		t.Errorf("SELECT-ов %d, want 1 (ровно страница, без лишнего round-trip)", db.queryCalls)
	}
}

// --- empty set → one SELECT, empty yield (callback doesn't fail) ---

func TestStateLister_EmptySet(t *testing.T) {
	db := &statePageDB{rows: nil}
	l := NewStateLister(db)

	yields := 0
	err := l.ListStatePages(context.Background(), statepredicate.BaseFilter{}, func(page []statepredicate.Stated) error {
		yields++
		return nil
	})
	if err != nil {
		t.Fatalf("ListStatePages: %v", err)
	}
	if yields != 0 {
		t.Errorf("пустой набор: yield вызван %d раз, want 0 (пустые страницы не отдаются)", yields)
	}
}

// --- SELECT error propagates ---

func TestStateLister_SelectError(t *testing.T) {
	sentinel := errors.New("db down")
	db := &statePageDB{rows: []*Incarnation{newStated("a", map[string]any{"k": 1})}, selectErr: sentinel}
	l := NewStateLister(db)

	err := l.ListStatePages(context.Background(), statepredicate.BaseFilter{}, func([]statepredicate.Stated) error { return nil })
	if !errors.Is(err, sentinel) {
		t.Fatalf("ошибка SELECT должна проброситься: got %v", err)
	}
}

// --- yield error stops draining and propagates (lazy: extra pages aren't fetched) ---

func TestStateLister_YieldErrorStops(t *testing.T) {
	const n = statePageSize + 10 // two pages
	rows := make([]*Incarnation, n)
	for i := range rows {
		rows[i] = newStated(fmt.Sprintf("inc-%05d", i), map[string]any{"idx": i})
	}
	db := &statePageDB{rows: rows}
	l := NewStateLister(db)

	sentinel := errors.New("not-bool on full state")
	err := l.ListStatePages(context.Background(), statepredicate.BaseFilter{}, func([]statepredicate.Stated) error {
		return sentinel // fail on the very first page
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ошибка yield должна проброситься: got %v", err)
	}
	if db.queryCalls != 1 {
		t.Errorf("после ошибки yield SELECT-ов %d, want 1 (вторая страница не тянется)", db.queryCalls)
	}
}
