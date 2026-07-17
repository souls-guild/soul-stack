package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB — execQueryRower stub for unit tests. Captures the last SQL
// and args, returns a configurable Row / Rows / Err.
type fakeDB struct {
	execCalls    int
	lastExecSQL  string
	lastExecArgs []any
	execErr      error
	execTag      pgconn.CommandTag

	queryRowSQL   string
	queryRowArgs  []any
	queryRowFunc  func(sql string) pgx.Row
	queryRowCalls int

	querySQL   string
	queryArgs  []any
	queryFunc  func(sql string) (pgx.Rows, error)
	queryCalls int
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls++
	f.lastExecSQL = sql
	f.lastExecArgs = args
	return f.execTag, f.execErr
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.queryRowCalls++
	f.queryRowSQL = sql
	f.queryRowArgs = args
	if f.queryRowFunc != nil {
		return f.queryRowFunc(sql)
	}
	return errRow{err: pgx.ErrNoRows}
}

func (f *fakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.queryCalls++
	f.querySQL = sql
	f.queryArgs = args
	if f.queryFunc != nil {
		return f.queryFunc(sql)
	}
	return &fakeRows{}, nil
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

type staticRow struct {
	values []any
	err    error
}

func (r staticRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("staticRow: len mismatch")
	}
	for i, d := range dest {
		assign(d, r.values[i])
	}
	return nil
}

func assign(dest, src any) {
	switch d := dest.(type) {
	case *string:
		*d = src.(string)
	case *int:
		*d = src.(int)
	case *int64:
		*d = src.(int64)
	case *time.Time:
		*d = src.(time.Time)
	case **string:
		if src == nil {
			*d = nil
		} else {
			s := src.(string)
			*d = &s
		}
	case *[]byte:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]byte)
		}
	case *[]string:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]string)
		}
	case **time.Time:
		if src == nil {
			*d = nil
		} else {
			t := src.(time.Time)
			*d = &t
		}
	default:
		panic("staticRow.assign: unsupported dest type")
	}
}

// fakeRows — pgx.Rows stub. Iterates staticRow by staticRow.
type fakeRows struct {
	rows []staticRow
	idx  int
	err  error
}

func (r *fakeRows) Next() bool {
	if r.err != nil {
		return false
	}
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error                       { return r.rows[r.idx-1].Scan(dest...) }
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) Close()                                       {}
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

// --- Create -----------------------------------------------------------

func TestCreate_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{now, now}}
		},
	}
	parent := "archon-alice"
	inc := &Incarnation{
		Name:               "redis-prod",
		Service:            "redis",
		ServiceVersion:     "v1.0.0",
		StateSchemaVersion: 1,
		Spec:               map[string]any{"replicas": 3},
		Status:             StatusReady,
		CreatedByAID:       &parent,
	}
	if err := Create(context.Background(), f, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !inc.CreatedAt.Equal(now) || !inc.UpdatedAt.Equal(now) {
		t.Errorf("RETURNING not assigned: %+v", inc)
	}
	if f.queryRowCalls != 1 {
		t.Errorf("queryRowCalls = %d, want 1", f.queryRowCalls)
	}
	if !strings.Contains(f.queryRowSQL, "INSERT INTO incarnation") {
		t.Errorf("SQL: %q", f.queryRowSQL)
	}
	if len(f.queryRowArgs) != 12 {
		t.Fatalf("args len = %d, want 12", len(f.queryRowArgs))
	}
	if f.queryRowArgs[0] != "redis-prod" {
		t.Errorf("args[0] name = %v", f.queryRowArgs[0])
	}
	// created_scenario (args[11]): caller left the field unset (nil *string) → NULL in DB
	// (migration 090, bare incarnation). No more ""→'create' normalization.
	if f.queryRowArgs[11] != nil {
		t.Errorf("args[11] created_scenario = %v, want nil (NULL for bare)", f.queryRowArgs[11])
	}
	if f.queryRowArgs[1] != "redis" {
		t.Errorf("args[1] service = %v", f.queryRowArgs[1])
	}
	if f.queryRowArgs[2] != "v1.0.0" {
		t.Errorf("args[2] service_version = %v", f.queryRowArgs[2])
	}
	if f.queryRowArgs[3] != 1 {
		t.Errorf("args[3] state_schema_version = %v", f.queryRowArgs[3])
	}
	if f.queryRowArgs[6] != "ready" {
		t.Errorf("args[6] status = %v", f.queryRowArgs[6])
	}
	if f.queryRowArgs[8] != "archon-alice" {
		t.Errorf("args[8] created_by_aid = %v", f.queryRowArgs[8])
	}
}

func TestCreate_RejectsInvalidName(t *testing.T) {
	f := &fakeDB{}
	inc := &Incarnation{
		Name: "Bad_Name", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
	}
	err := Create(context.Background(), f, inc)
	if err == nil {
		t.Fatal("Create with invalid name returned nil")
	}
	if !strings.Contains(err.Error(), "invalid name") {
		t.Errorf("err = %v", err)
	}
	if f.queryRowCalls != 0 {
		t.Errorf("queryRowCalls = %d on invalid name; want 0", f.queryRowCalls)
	}
}

func TestCreate_RejectsEmptyService(t *testing.T) {
	f := &fakeDB{}
	inc := &Incarnation{
		Name: "redis-x", Service: "", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
	}
	if err := Create(context.Background(), f, inc); err == nil {
		t.Fatal("Create with empty service returned nil")
	}
}

func TestCreate_RejectsInvalidStatus(t *testing.T) {
	f := &fakeDB{}
	inc := &Incarnation{
		Name: "redis-x", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: Status("hax"),
	}
	if err := Create(context.Background(), f, inc); err == nil {
		t.Fatal("Create with invalid status returned nil")
	}
}

func TestCreate_RejectsZeroSchemaVersion(t *testing.T) {
	f := &fakeDB{}
	inc := &Incarnation{
		Name: "redis-x", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 0, Status: StatusReady,
	}
	if err := Create(context.Background(), f, inc); err == nil {
		t.Fatal("Create with state_schema_version=0 returned nil")
	}
}

func TestCreate_RejectsNil(t *testing.T) {
	f := &fakeDB{}
	if err := Create(context.Background(), f, nil); err == nil {
		t.Fatal("Create(nil) returned nil")
	}
}

func TestCreate_MapsUniqueViolation(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeUniqueViolation,
				ConstraintName: "incarnation_pkey",
			}}
		},
	}
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
	}
	err := Create(context.Background(), f, inc)
	if !errors.Is(err, ErrIncarnationAlreadyExists) {
		t.Fatalf("err = %v, want errors.Is ErrIncarnationAlreadyExists", err)
	}
	if !strings.Contains(err.Error(), "incarnation_pkey") {
		t.Errorf("err = %v; expected constraint name in message", err)
	}
}

func TestCreate_MapsFKViolation(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return errRow{err: &pgconn.PgError{
				Code:           pgErrCodeForeignKeyViolation,
				ConstraintName: "incarnation_created_by_aid_fk",
			}}
		},
	}
	parent := "archon-ghost"
	inc := &Incarnation{
		Name: "redis-x", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &parent,
	}
	err := Create(context.Background(), f, inc)
	if err == nil {
		t.Fatal("Create with FK-violation returned nil")
	}
	if errors.Is(err, ErrIncarnationAlreadyExists) {
		t.Errorf("FK-violation should NOT be ErrIncarnationAlreadyExists; err = %v", err)
	}
	if !strings.Contains(err.Error(), "FK violation") {
		t.Errorf("err = %v; expected substring \"FK violation\"", err)
	}
}

func TestCreate_MarshalsSpecAsJSONB(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{time.Now(), time.Now()}}
		},
	}
	inc := &Incarnation{
		Name: "redis-x", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
		Spec: map[string]any{"replicas": 3, "version": "7.0"},
	}
	if err := Create(context.Background(), f, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, ok := f.queryRowArgs[4].([]byte)
	if !ok {
		t.Fatalf("args[4] = %T, want []byte", f.queryRowArgs[4])
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("spec not JSON: %v", err)
	}
	if got["version"] != "7.0" {
		t.Errorf("spec.version = %v", got["version"])
	}
}

func TestCreate_NilSpecBecomesEmptyObject(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{time.Now(), time.Now()}}
		},
	}
	inc := &Incarnation{
		Name: "redis-x", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
		// Spec / State nil
	}
	if err := Create(context.Background(), f, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s, _ := f.queryRowArgs[4].([]byte); string(s) != "{}" {
		t.Errorf("spec bytes = %s, want \"{}\"", s)
	}
	if s, _ := f.queryRowArgs[5].([]byte); string(s) != "{}" {
		t.Errorf("state bytes = %s, want \"{}\"", s)
	}
}

// TestCreate_NilCovensBecomesEmptySlice — covens=nil encodes as an empty
// array (NOT NULL DEFAULT '{}'): pgx would otherwise pass NULL → violation.
// Arg $10 (index 9) — covens.
func TestCreate_NilCovensBecomesEmptySlice(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{time.Now(), time.Now()}}
		},
	}
	inc := &Incarnation{
		Name: "redis-x", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
		// Covens nil
	}
	if err := Create(context.Background(), f, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	covens, ok := f.queryRowArgs[9].([]string)
	if !ok {
		t.Fatalf("args[9] = %T, want []string", f.queryRowArgs[9])
	}
	if covens == nil {
		t.Errorf("covens arg = nil, want non-nil empty slice")
	}
	if len(covens) != 0 {
		t.Errorf("covens arg = %v, want empty", covens)
	}
}

// TestCreate_CovensPassedThrough — explicitly set covens reach INSERT arg $10.
func TestCreate_CovensPassedThrough(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{time.Now(), time.Now()}}
		},
	}
	inc := &Incarnation{
		Name: "redis-x", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
		Covens: []string{"prod", "dc1"},
	}
	if err := Create(context.Background(), f, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	covens, _ := f.queryRowArgs[9].([]string)
	if len(covens) != 2 || covens[0] != "prod" || covens[1] != "dc1" {
		t.Errorf("covens arg = %v, want [prod dc1]", covens)
	}
}

// --- SelectByName -----------------------------------------------------

func TestSelectByName_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{
				"redis-prod", "redis", "v1.0.0", 1,
				[]byte(`{"replicas":3}`),
				[]byte(`{"ready":true}`),
				"ready",
				[]byte(nil),
				any("archon-alice"),
				now, now, []string{"prod"},
				[]byte(`{"team":"dba"}`), // traits
				any(nil), []byte(nil),    // last_drift_check_at, last_drift_summary
				"create", // created_scenario
				any(nil), // applying_apply_id
			}}
		},
	}
	inc, err := SelectByName(context.Background(), f, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if inc.Name != "redis-prod" {
		t.Errorf("Name = %q", inc.Name)
	}
	if inc.Status != StatusReady {
		t.Errorf("Status = %q", inc.Status)
	}
	if inc.CreatedByAID == nil || *inc.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", inc.CreatedByAID)
	}
	if inc.Spec["replicas"] != float64(3) { // JSON-decode → float64
		t.Errorf("Spec.replicas = %v", inc.Spec["replicas"])
	}
	if inc.State["ready"] != true {
		t.Errorf("State.ready = %v", inc.State["ready"])
	}
	if len(inc.Covens) != 1 || inc.Covens[0] != "prod" {
		t.Errorf("Covens = %v, want [prod]", inc.Covens)
	}
	// traits jsonb (ADR-060 amend R1) — round-trip scan into Incarnation.Traits.
	if inc.Traits["team"] != "dba" {
		t.Errorf("Traits = %v, want team=dba", inc.Traits)
	}
}

func TestSelectByName_NotFound(t *testing.T) {
	f := &fakeDB{} // default → ErrNoRows
	_, err := SelectByName(context.Background(), f, "missing")
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Fatalf("err = %v, want ErrIncarnationNotFound", err)
	}
}

// --- SelectAll --------------------------------------------------------

func TestSelectAll_NoFilter(t *testing.T) {
	now := time.Now()
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(2)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{rows: []staticRow{
				{values: []any{
					"a", "redis", "v1", 1,
					[]byte("{}"), []byte("{}"), "ready",
					[]byte(nil), any(nil), now, now, []string(nil),
					[]byte("{}"), // traits
					any(nil), []byte(nil),
					"create", // created_scenario
					any(nil), // applying_apply_id
				}},
				{values: []any{
					"b", "redis", "v1", 1,
					[]byte("{}"), []byte("{}"), "applying",
					[]byte(nil), any(nil), now, now, []string(nil),
					[]byte("{}"), // traits
					any(nil), []byte(nil),
					"create", // created_scenario
					any(nil), // applying_apply_id
				}},
			}}, nil
		},
	}
	out, total, err := SelectAll(context.Background(), f, ListFilter{}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	if out[0].Name != "a" || out[1].Name != "b" {
		t.Errorf("names = %s, %s", out[0].Name, out[1].Name)
	}
	if !strings.Contains(f.querySQL, "ORDER BY created_at DESC") {
		t.Errorf("ORDER missing in: %q", f.querySQL)
	}
	// args = [offset, limit] with no filters.
	if len(f.queryArgs) != 2 || f.queryArgs[0] != 0 || f.queryArgs[1] != 50 {
		t.Errorf("args = %v", f.queryArgs)
	}
}

func TestSelectAll_FilterByService(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(0)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{}, nil
		},
	}
	_, _, err := SelectAll(context.Background(), f,
		ListFilter{Service: "redis"}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "service = $1") {
		t.Errorf("filter SQL: %q", f.querySQL)
	}
	if f.queryArgs[0] != "redis" {
		t.Errorf("args[0] = %v", f.queryArgs[0])
	}
}

func TestSelectAll_FilterByServiceAndStatus(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(0)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{}, nil
		},
	}
	_, _, err := SelectAll(context.Background(), f,
		ListFilter{Service: "redis", Status: StatusReady}, ListScope{Unrestricted: true}, 10, 5)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "service = $1") ||
		!strings.Contains(f.querySQL, "status = $2") {
		t.Errorf("filter SQL: %q", f.querySQL)
	}
	// Args: [service, status, offset, limit].
	if len(f.queryArgs) != 4 {
		t.Fatalf("args len = %d, want 4", len(f.queryArgs))
	}
	if f.queryArgs[2] != 10 || f.queryArgs[3] != 5 {
		t.Errorf("offset/limit args = %v / %v", f.queryArgs[2], f.queryArgs[3])
	}
}

func TestSelectAll_FilterByCoven(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(0)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{}, nil
		},
	}
	_, _, err := SelectAll(context.Background(), f,
		ListFilter{Coven: "dev"}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "ANY(covens)") {
		t.Errorf("filter SQL: %q (want ANY(covens))", f.querySQL)
	}
	if f.queryArgs[0] != "dev" {
		t.Errorf("args[0] = %v, want dev", f.queryArgs[0])
	}
}

// --- SelectAll: RBAC scope (ADR-047 S3b-3) ----------------------------

// TestSelectAll_ScopeUnrestricted_NoClause — Unrestricted scope adds no
// scope predicate at all (full list, no narrowing). args = [offset, limit].
func TestSelectAll_ScopeUnrestricted_NoClause(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if strings.Contains(f.querySQL, "covens &&") || strings.Contains(f.querySQL, "FALSE") {
		t.Errorf("unrestricted scope added a predicate: %q", f.querySQL)
	}
	if len(f.queryArgs) != 2 {
		t.Errorf("args = %v, want only [offset, limit]", f.queryArgs)
	}
}

// TestSelectAll_ScopeEmpty_FailClosed — the KEY security invariant at the CRUD
// layer: a scope that's not Unrestricted and empty across dimensions (neither
// Covens nor StateNames) → FALSE predicate (no incarnation matches), NOT the
// full list. Defensive branch (the handler filters an empty scope earlier, but
// fail-closed must hold here too).
func TestSelectAll_ScopeEmpty_FailClosed(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{}, ListScope{}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "FALSE") {
		t.Errorf("empty scope must produce a FALSE predicate (fail-closed), got: %q", f.querySQL)
	}
	if !strings.Contains(f.queryRowSQL, "FALSE") {
		t.Errorf("FALSE must also appear in COUNT (total=0 in scope): %q", f.queryRowSQL)
	}
}

// TestSelectAll_ScopeCovens_CovenUnionName — coven∪{name} matcher (ADR-008,
// architect major): scope-coven must match an incarnation both by covens[]
// intersection (`covens && $n`) and by name equality (`name = ANY($n)`). One
// bind parameter serves both arms of the OR.
func TestSelectAll_ScopeCovens_CovenUnionName(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{},
		ListScope{Covens: []string{"redis-prod"}}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "covens && $1") {
		t.Errorf("coven intersection covens && $1 is missing: %q", f.querySQL)
	}
	if !strings.Contains(f.querySQL, "name = ANY($1)") {
		t.Errorf("coven∪{name}: name = ANY($1) is missing (incarnation with name=scope-coven should match): %q", f.querySQL)
	}
	// Both arms use the same bind ($1); value = scope-covens.
	if covs, ok := f.queryArgs[0].([]string); !ok || len(covs) != 1 || covs[0] != "redis-prod" {
		t.Errorf("scope-covens bind = %v, want [redis-prod]", f.queryArgs[0])
	}
}

// TestSelectAll_ScopeStateNames_PushdownByName — the state dimension arrives
// as a pre-resolved set of names → `name = ANY($n)` pushdown (CEL isn't
// duplicated in CRUD; names match via plain SQL, total/offset stay coherent).
func TestSelectAll_ScopeStateNames_PushdownByName(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{},
		ListScope{StateNames: []string{"redis-a", "redis-c"}}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "name = ANY($1)") {
		t.Errorf("state-names pushdown name = ANY($1) is missing: %q", f.querySQL)
	}
	if names, ok := f.queryArgs[0].([]string); !ok || len(names) != 2 {
		t.Errorf("state-names bind = %v, want [redis-a redis-c]", f.queryArgs[0])
	}
}

// TestSelectAll_ScopeOR_CovenAndState — OR of dimensions (architect): coven ∪
// state = union. The predicate is a single parenthesized block
// `(coven-arm OR name-state-arm)`, so the OR doesn't "leak" across the
// adjacent AND clause of the user filter.
func TestSelectAll_ScopeOR_CovenAndState(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{Service: "redis"},
		ListScope{Covens: []string{"prod"}, StateNames: []string{"redis-x"}}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	// service filter AND scope block; inside the scope block, coven OR state.
	if !strings.Contains(f.querySQL, "service = $1") {
		t.Errorf("service filter is missing: %q", f.querySQL)
	}
	// scope block is parenthesized and contains OR between dimensions.
	if !strings.Contains(f.querySQL, "((covens && $2 OR name = ANY($2)) OR name = ANY($3))") {
		t.Errorf("dimension OR-block is wrong: %q", f.querySQL)
	}
}

// TestSelectAll_ScopeWithUserFilter_AND — user filter AND scope: the filter
// narrows WITHIN the scope (doesn't widen it). service AND (scope-block).
func TestSelectAll_ScopeWithUserFilter_AND(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f,
		ListFilter{Status: StatusReady},
		ListScope{Covens: []string{"prod"}}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "status = $1") || !strings.Contains(f.querySQL, "AND") {
		t.Errorf("filter AND scope expected: %q", f.querySQL)
	}
	if !strings.Contains(f.querySQL, "covens && $2") {
		t.Errorf("scope-coven as $2 after status=$1: %q", f.querySQL)
	}
}

// --- SelectAll: trait-scope SQL form (BUG #1 fix — scalar, not @>) ---------

// TestSelectAll_ScopeTrait_ScalarEquality — the trait arm must be SCALAR
// equality `traits->>$1 = $2` (key+value as separate bind parameters), NOT
// jsonb containment `@>`: containment with a scalar RHS matches an array
// (list-Trait), diverging from the GET path (traitScalarEquals). Values never
// leak into the SQL text.
func TestSelectAll_ScopeTrait_ScalarEquality(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{},
		ListScope{Traits: []TraitPair{{Key: "env", Value: "prod"}}}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "traits->>$1 = $2") {
		t.Errorf("trait arm must be scalar `traits->>$1 = $2`, got: %q", f.querySQL)
	}
	// Regression guard: the old containment form must not return (that's exactly BUG #1).
	if strings.Contains(f.querySQL, "@>") {
		t.Errorf("trait arm uses containment @> (BUG #1 — matches list): %q", f.querySQL)
	}
	// Key and value are separate bind parameters (not concatenated into SQL text).
	if len(f.queryArgs) < 2 || f.queryArgs[0] != "env" || f.queryArgs[1] != "prod" {
		t.Errorf("trait bind-args = %v, want [env prod] separately", f.queryArgs)
	}
	if strings.Contains(f.querySQL, "env") || strings.Contains(f.querySQL, "prod") {
		t.Errorf("key/value leaked into SQL text (must be bind params): %q", f.querySQL)
	}
	// Same predicate in COUNT (otherwise total would diverge from items).
	if !strings.Contains(f.queryRowSQL, "traits->>$1 = $2") {
		t.Errorf("trait arm is missing from COUNT: %q", f.queryRowSQL)
	}
}

// TestAppendScopeClause_Table — table-driven overview of the scope-predicate
// shape across different ListScope dimension combos: trait-arm content,
// parenthesized OR union, fail-closed FALSE on an empty non-Unrestricted
// scope, no clause on Unrestricted. Checks the SQL via SelectAll (the only
// public path to appendScopeClause), comparing f.querySQL.
func TestAppendScopeClause_Table(t *testing.T) {
	tests := []struct {
		name       string
		scope      ListScope
		wantSubstr []string // substrings that must be present
		denySubstr []string // substrings that must be ABSENT
	}{
		{
			name:       "one trait -> scalar arm without OR",
			scope:      ListScope{Traits: []TraitPair{{Key: "owner", Value: "alice"}}},
			wantSubstr: []string{"traits->>$1 = $2"},
			denySubstr: []string{"@>", " OR ", "FALSE"},
		},
		{
			name: "two traits -> OR between arms inside common parens",
			scope: ListScope{Traits: []TraitPair{
				{Key: "owner", Value: "alice"},
				{Key: "team", Value: "dba"},
			}},
			wantSubstr: []string{"(traits->>$1 = $2 OR traits->>$3 = $4)"},
			denySubstr: []string{"@>", "FALSE"},
		},
		{
			name: "coven ∪ trait -> dimension OR, trait arm after the coven bind",
			scope: ListScope{
				Covens: []string{"prod"},
				Traits: []TraitPair{{Key: "owner", Value: "alice"}},
			},
			// coven takes $1, trait — $2(key)/$3(value).
			wantSubstr: []string{"((covens && $1 OR name = ANY($1)) OR traits->>$2 = $3)"},
			denySubstr: []string{"@>", "FALSE"},
		},
		{
			name:  "empty scope, not Unrestricted -> fail-closed FALSE",
			scope: ListScope{},
			// FALSE predicate in WHERE; no scope arm at all (column names
			// traits/covens in the SELECT list don't count — we check predicates).
			wantSubstr: []string{"WHERE FALSE"},
			denySubstr: []string{"traits->>", "covens &&"},
		},
		{
			name:       "Unrestricted -> no scope predicate at all",
			scope:      ListScope{Unrestricted: true},
			denySubstr: []string{"FALSE", "traits->>", "covens &&"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCountQueryFakeDB()
			if _, _, err := SelectAll(context.Background(), f, ListFilter{}, tc.scope, 0, 50); err != nil {
				t.Fatalf("SelectAll: %v", err)
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(f.querySQL, want) {
					t.Errorf("SQL does not contain %q: %q", want, f.querySQL)
				}
			}
			for _, deny := range tc.denySubstr {
				if strings.Contains(f.querySQL, deny) {
					t.Errorf("SQL contains forbidden %q: %q", deny, f.querySQL)
				}
			}
		})
	}
}

func TestSelectAll_RejectsNegativeOffset(t *testing.T) {
	f := &fakeDB{}
	_, _, err := SelectAll(context.Background(), f, ListFilter{}, ListScope{Unrestricted: true}, -1, 50)
	if err == nil {
		t.Fatal("expected error on negative offset")
	}
}

func TestSelectAll_RejectsZeroLimit(t *testing.T) {
	f := &fakeDB{}
	_, _, err := SelectAll(context.Background(), f, ListFilter{}, ListScope{Unrestricted: true}, 0, 0)
	if err == nil {
		t.Fatal("expected error on zero limit")
	}
}

// --- SelectAll: StatePredicates (jsonb-pushdown) ----------------------

func newCountQueryFakeDB() *fakeDB {
	return &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(0)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{}, nil
		},
	}
}

func TestSelectAll_StatePredicate_EqString(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{
		StatePredicates: []StateEq{{Path: "redis_version", Op: StateOpEq, Value: "8.0"}},
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	// jsonb pushdown: text extraction via ->>, value through bind ($1).
	if !strings.Contains(f.querySQL, "state->>'redis_version' = $1") {
		t.Errorf("jsonb eq SQL missing in: %q", f.querySQL)
	}
	// Same predicate must appear in COUNT too — otherwise total would diverge.
	if !strings.Contains(f.queryRowSQL, "state->>'redis_version' = $1") {
		t.Errorf("jsonb eq SQL missing in COUNT: %q", f.queryRowSQL)
	}
	if f.queryArgs[0] != "8.0" {
		t.Errorf("args[0] = %v, want 8.0", f.queryArgs[0])
	}
	// Value is always a bind parameter, never concatenated into SQL text.
	if strings.Contains(f.querySQL, "8.0") {
		t.Errorf("value leaked into SQL text (must be bind): %q", f.querySQL)
	}
}

func TestSelectAll_StatePredicate_NumericGt(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{
		StatePredicates: []StateEq{{Path: "memory_mb", Op: StateOpGt, Value: "1000"}},
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	// Numeric comparison: cast (->>)::numeric, value also via bind+cast.
	if !strings.Contains(f.querySQL, "(state->>'memory_mb')::numeric > $1::numeric") {
		t.Errorf("jsonb numeric gt SQL missing in: %q", f.querySQL)
	}
	if f.queryArgs[0] != "1000" {
		t.Errorf("args[0] = %v, want 1000", f.queryArgs[0])
	}
}

func TestSelectAll_StatePredicate_RejectsInjectionPath(t *testing.T) {
	for _, bad := range []string{
		"redis_version' OR '1'='1",
		"redis_version'; DROP TABLE incarnation; --",
		"foo) OR (1=1",
		"UPPER",      // not lowercase
		"with-dash",  // hyphen outside whitelist [a-z0-9_]
		"with.dot",   // dot outside whitelist
		"",           // empty path
		"with space", // space
	} {
		f := newCountQueryFakeDB()
		_, _, err := SelectAll(context.Background(), f, ListFilter{
			StatePredicates: []StateEq{{Path: bad, Op: StateOpEq, Value: "x"}},
		}, ListScope{Unrestricted: true}, 0, 50)
		if !errors.Is(err, ErrInvalidStatePath) {
			t.Errorf("path %q: err = %v, want ErrInvalidStatePath", bad, err)
		}
		// Injection must not reach the DB: no queries on reject.
		if f.queryRowCalls != 0 || f.queryCalls != 0 {
			t.Errorf("path %q: query reached the DB despite the reject", bad)
		}
	}
}

func TestSelectAll_StatePredicate_AcceptsWhitelistPath(t *testing.T) {
	for _, ok := range []string{"redis_version", "memory_mb", "a", "a1", "x_y_z", "tier2"} {
		f := newCountQueryFakeDB()
		_, _, err := SelectAll(context.Background(), f, ListFilter{
			StatePredicates: []StateEq{{Path: ok, Op: StateOpEq, Value: "v"}},
		}, ListScope{Unrestricted: true}, 0, 50)
		if err != nil {
			t.Errorf("path %q rejected unexpectedly: %v", ok, err)
		}
	}
}

func TestSelectAll_StatePredicate_RejectsUnknownOp(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{
		StatePredicates: []StateEq{{Path: "redis_version", Op: "like", Value: "8%"}},
	}, ListScope{Unrestricted: true}, 0, 50)
	if !errors.Is(err, ErrInvalidStateOp) {
		t.Errorf("err = %v, want ErrInvalidStateOp", err)
	}
}

// TestSelectAll_StatePredicate_NumericOp_RejectsNonNumericValue — for numeric
// operators (gt/gte/lt/lte) a non-numeric value must be rejected BEFORE the DB
// query ([ErrInvalidStateValue]), not fail with a 22P02 cast error on the PG side.
func TestSelectAll_StatePredicate_NumericOp_RejectsNonNumericValue(t *testing.T) {
	for _, op := range []StateOp{StateOpGt, StateOpGte, StateOpLt, StateOpLte} {
		for _, bad := range []string{"abc", "1.2.3", "", "10x", "0x10", " "} {
			f := newCountQueryFakeDB()
			_, _, err := SelectAll(context.Background(), f, ListFilter{
				StatePredicates: []StateEq{{Path: "memory_mb", Op: op, Value: bad}},
			}, ListScope{Unrestricted: true}, 0, 50)
			if !errors.Is(err, ErrInvalidStateValue) {
				t.Errorf("op %q value %q: err = %v, want ErrInvalidStateValue", op, bad, err)
			}
			// An invalid value must not reach the DB (otherwise 22P02 → 500).
			if f.queryRowCalls != 0 || f.queryCalls != 0 {
				t.Errorf("op %q value %q: query reached the DB despite the reject", op, bad)
			}
		}
	}
}

// TestSelectAll_StatePredicate_NumericOp_AcceptsNumericValue — valid numeric
// forms (integer/decimal/negative/exponent) pass validation.
func TestSelectAll_StatePredicate_NumericOp_AcceptsNumericValue(t *testing.T) {
	for _, ok := range []string{"1000", "0", "-5", "3.14", "1e3", "-0.5"} {
		f := newCountQueryFakeDB()
		_, _, err := SelectAll(context.Background(), f, ListFilter{
			StatePredicates: []StateEq{{Path: "memory_mb", Op: StateOpGt, Value: ok}},
		}, ListScope{Unrestricted: true}, 0, 50)
		if err != nil {
			t.Errorf("value %q rejected unexpectedly: %v", ok, err)
		}
	}
}

// TestSelectAll_StatePredicate_TextOp_AllowsNonNumericValue — for text
// operators (eq/ne) a non-numeric value stays valid (numeric validation
// does NOT apply).
func TestSelectAll_StatePredicate_TextOp_AllowsNonNumericValue(t *testing.T) {
	for _, op := range []StateOp{StateOpEq, StateOpNe} {
		f := newCountQueryFakeDB()
		_, _, err := SelectAll(context.Background(), f, ListFilter{
			StatePredicates: []StateEq{{Path: "redis_version", Op: op, Value: "abc"}},
		}, ListScope{Unrestricted: true}, 0, 50)
		if err != nil {
			t.Errorf("op %q: text value was rejected: %v", op, err)
		}
	}
}

// TestSelectAll_StatePredicate_CombinesWithBaseFilter — state predicate
// AND base filter (service/coven): bind placeholder numbering is shared.
func TestSelectAll_StatePredicate_CombinesWithBaseFilter(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{
		Service:         "redis",
		Coven:           "prod",
		StatePredicates: []StateEq{{Path: "redis_version", Op: StateOpEq, Value: "8.0"}},
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "service = $1") ||
		!strings.Contains(f.querySQL, "$2 = ANY(covens)") ||
		!strings.Contains(f.querySQL, "state->>'redis_version' = $3") {
		t.Errorf("combined WHERE SQL: %q", f.querySQL)
	}
	// Args: [service, coven, state-value, offset, limit].
	if len(f.queryArgs) != 5 {
		t.Fatalf("args len = %d, want 5: %v", len(f.queryArgs), f.queryArgs)
	}
	if f.queryArgs[0] != "redis" || f.queryArgs[1] != "prod" || f.queryArgs[2] != "8.0" {
		t.Errorf("args = %v", f.queryArgs)
	}
}

// --- SelectAll: SortBy / SortDir --------------------------------------

func TestSelectAll_SortByName_Asc(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{
		SortBy: "name", SortDir: SortAsc,
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "ORDER BY name ASC, name ASC") {
		t.Errorf("ORDER BY name asc missing in: %q", f.querySQL)
	}
}

func TestSelectAll_SortByStatus_Desc(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{
		SortBy: "status", SortDir: SortDesc,
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	// tie-break on name ASC is kept for stable pagination.
	if !strings.Contains(f.querySQL, "ORDER BY status DESC, name ASC") {
		t.Errorf("ORDER BY status desc missing in: %q", f.querySQL)
	}
}

// TestSelectAll_SortByStateField — sort by a state field via jsonb ->>.
// The jsonb path is validated by the same whitelist as StatePredicate.Path.
func TestSelectAll_SortByStateField(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{
		SortBy: "state.redis_version", SortDir: SortAsc,
	}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "ORDER BY state->>'redis_version' ASC, name ASC") {
		t.Errorf("ORDER BY state-field missing in: %q", f.querySQL)
	}
}

func TestSelectAll_SortBy_RejectsUnknownField(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{SortBy: "spec"}, ListScope{Unrestricted: true}, 0, 50)
	if !errors.Is(err, ErrInvalidSortField) {
		t.Errorf("err = %v, want ErrInvalidSortField", err)
	}
	if f.queryRowCalls != 0 || f.queryCalls != 0 {
		t.Error("query reached the DB despite the sort-field reject")
	}
}

func TestSelectAll_SortBy_RejectsInjectionStatePath(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{
		SortBy: "state.foo' OR '1'='1",
	}, ListScope{Unrestricted: true}, 0, 50)
	if !errors.Is(err, ErrInvalidStatePath) {
		t.Errorf("err = %v, want ErrInvalidStatePath", err)
	}
}

func TestSelectAll_SortDir_RejectsUnknown(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{
		SortBy: "name", SortDir: "sideways",
	}, ListScope{Unrestricted: true}, 0, 50)
	if !errors.Is(err, ErrInvalidSortDir) {
		t.Errorf("err = %v, want ErrInvalidSortDir", err)
	}
}

// TestSelectAll_DefaultSortUnchanged — without SortBy the legacy order
// created_at DESC, name ASC is preserved (backward compatibility).
func TestSelectAll_DefaultSortUnchanged(t *testing.T) {
	f := newCountQueryFakeDB()
	_, _, err := SelectAll(context.Background(), f, ListFilter{}, ListScope{Unrestricted: true}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if !strings.Contains(f.querySQL, "ORDER BY created_at DESC, name ASC") {
		t.Errorf("default ORDER BY changed: %q", f.querySQL)
	}
}

// --- HistorySelectByName ---------------------------------------------

func TestHistorySelectByName_HappyPath(t *testing.T) {
	now := time.Now()
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(1)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{rows: []staticRow{
				{values: []any{
					"01HABC", "create",
					[]byte(`{"v":1}`),
					[]byte(`{"v":2}`),
					any("archon-alice"),
					"01HXYZ",
					now,
				}},
			}}, nil
		},
	}
	out, total, err := HistorySelectByName(context.Background(), f, "redis-prod", HistoryFilter{}, 0, 50)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d", total)
	}
	if len(out) != 1 || out[0].HistoryID != "01HABC" {
		t.Errorf("got = %+v", out)
	}
	if out[0].ChangedByAID == nil || *out[0].ChangedByAID != "archon-alice" {
		t.Errorf("ChangedByAID = %v", out[0].ChangedByAID)
	}
	if out[0].StateBefore["v"] != float64(1) {
		t.Errorf("StateBefore = %v", out[0].StateBefore)
	}
	// No filter — only-args [name, offset, limit].
	if len(f.queryArgs) != 3 {
		t.Errorf("queryArgs len = %d, want 3 (name, offset, limit)", len(f.queryArgs))
	}
	if strings.Contains(f.querySQL, "apply_id = $") {
		t.Errorf("empty filter must not produce apply_id WHERE; SQL=%q", f.querySQL)
	}
}

func TestHistorySelectByName_EmptyOK(t *testing.T) {
	// Existing incarnation with no history records → total=0, items=[].
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(0)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{}, nil
		},
	}
	out, total, err := HistorySelectByName(context.Background(), f, "redis-prod", HistoryFilter{}, 0, 50)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 0 || len(out) != 0 {
		t.Errorf("total = %d, len(out) = %d", total, len(out))
	}
}

func TestHistorySelectByName_FilterByApplyID(t *testing.T) {
	now := time.Now()
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(1)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{rows: []staticRow{
				{values: []any{
					"01HABC", "scale",
					[]byte(`{"v":1}`),
					[]byte(`{"v":2}`),
					any(nil),
					"01HAPPYBBBBBBBBBBBBBBBBB00",
					now,
				}},
			}}, nil
		},
	}
	out, total, err := HistorySelectByName(context.Background(), f,
		"redis-prod", HistoryFilter{ApplyID: "01HAPPYBBBBBBBBBBBBBBBBB00"}, 0, 50)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 1 || len(out) != 1 {
		t.Errorf("total = %d, len(out) = %d", total, len(out))
	}
	if !strings.Contains(f.querySQL, "apply_id = $2") {
		t.Errorf("filter SQL: %q", f.querySQL)
	}
	// Args: [name, apply_id, offset, limit].
	if len(f.queryArgs) != 4 {
		t.Fatalf("queryArgs len = %d, want 4", len(f.queryArgs))
	}
	if f.queryArgs[0] != "redis-prod" || f.queryArgs[1] != "01HAPPYBBBBBBBBBBBBBBBBB00" {
		t.Errorf("queryArgs head = %v / %v", f.queryArgs[0], f.queryArgs[1])
	}
	if f.queryArgs[2] != 0 || f.queryArgs[3] != 50 {
		t.Errorf("offset/limit args = %v / %v", f.queryArgs[2], f.queryArgs[3])
	}
}

// TestHistorySelectByName_DefaultsToActiveOnly — by default (HistoryFilter{}
// = IncludeArchived:false) the WHERE clause includes `archived_at IS NULL`,
// so soft-deleted snapshots from the Reaper archive_state_history rule don't
// leak into the Operator API / MCP history feed (ADR-Q19 retention).
func TestHistorySelectByName_DefaultsToActiveOnly(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(0)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{}, nil
		},
	}
	if _, _, err := HistorySelectByName(context.Background(), f, "redis-prod",
		HistoryFilter{}, 0, 50); err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if !strings.Contains(f.querySQL, "archived_at IS NULL") {
		t.Errorf("default filter must add archived_at IS NULL; SQL=%q", f.querySQL)
	}
}

// TestHistorySelectByName_IncludeArchived — with IncludeArchived=true the
// `archived_at IS NULL` filter does NOT apply: all snapshots are returned,
// including soft-deleted ones. Operator API uses this to investigate "where
// did the snapshot from N days ago go".
func TestHistorySelectByName_IncludeArchived(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(0)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{}, nil
		},
	}
	if _, _, err := HistorySelectByName(context.Background(), f, "redis-prod",
		HistoryFilter{IncludeArchived: true}, 0, 50); err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if strings.Contains(f.querySQL, "archived_at IS NULL") {
		t.Errorf("IncludeArchived=true must NOT add archived_at filter; SQL=%q", f.querySQL)
	}
}

func TestHistorySelectByName_FilterApplyID_NoMatch(t *testing.T) {
	// A row exists, but the apply_id filter doesn't match → COUNT=0, items=[].
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{int(0)}}
		},
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{}, nil
		},
	}
	out, total, err := HistorySelectByName(context.Background(), f,
		"redis-prod", HistoryFilter{ApplyID: "01HGHST000000000000000000Z"}, 0, 50)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 0 || len(out) != 0 {
		t.Errorf("total = %d, len(out) = %d (want 0/0)", total, len(out))
	}
}

func TestHistorySelectByName_RejectsBadOffsetLimit(t *testing.T) {
	f := &fakeDB{}
	if _, _, err := HistorySelectByName(context.Background(), f, "x", HistoryFilter{}, -1, 50); err == nil {
		t.Errorf("expected error on negative offset")
	}
	if _, _, err := HistorySelectByName(context.Background(), f, "x", HistoryFilter{}, 0, 0); err == nil {
		t.Errorf("expected error on zero limit")
	}
}

// --- ValidName --------------------------------------------------------

// --- UpdateStateFromRun ---

// multiExecFake — fakeDB variant for single-winner [UpdateStateFromRun]
// (ADR-027(j) W1): Exec serves the state_history INSERT, QueryRow serves
// the final single-winner UPDATE … RETURNING (guard WHERE status IN
// (applying,destroying)) plus an optional probe-SELECT of status.
//
//   - updateRow  — what the final UPDATE … RETURNING name returns. nil →
//     pgx.ErrNoRows (guard didn't match: 0 rows).
//   - probeRow   — what the status probe-SELECT returns when UPDATE affects
//     0 rows. nil → pgx.ErrNoRows (no row at all → ErrIncarnationNotFound).
type multiExecFake struct {
	calls   int
	sqls    []string
	args    [][]any
	tags    []pgconn.CommandTag
	execErr error

	queryRowSQLs []string
	updateRow    pgx.Row // RETURNING name of the final UPDATE (nil → ErrNoRows)
	probeRow     pgx.Row // status probe-SELECT (nil → ErrNoRows)
}

func (f *multiExecFake) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	idx := f.calls
	f.calls++
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, args)
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	if idx < len(f.tags) {
		return f.tags[idx], nil
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// QueryRow distinguishes the final UPDATE … RETURNING (carries "UPDATE
// incarnation") from the status probe-SELECT by SQL text.
func (f *multiExecFake) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	f.queryRowSQLs = append(f.queryRowSQLs, sql)
	if strings.Contains(sql, "UPDATE incarnation") {
		if f.updateRow != nil {
			return f.updateRow
		}
		return errRow{err: pgx.ErrNoRows}
	}
	// probe SELECT status FROM incarnation
	if f.probeRow != nil {
		return f.probeRow
	}
	return errRow{err: pgx.ErrNoRows}
}
func (f *multiExecFake) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return &fakeRows{}, nil
}

func TestUpdateStateFromRun_HappyPath(t *testing.T) {
	f := &multiExecFake{
		tags:      []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")},
		updateRow: staticRow{values: []any{"redis-prod"}}, // RETURNING name — winner
	}
	err := UpdateStateFromRun(context.Background(), f,
		"redis-prod", "scale", "01HABCDEF000000000000000",
		map[string]any{"replicas": 1.0},
		map[string]any{"replicas": 3.0},
		StatusReady, nil, nil,
		"01HHIST00000000000000000",
	)
	if err != nil {
		t.Fatalf("UpdateStateFromRun: %v", err)
	}
	// Exec — state_history INSERT only; the final UPDATE went through QueryRow.
	if f.calls != 1 {
		t.Fatalf("Exec calls = %d, want 1 (state_history INSERT)", f.calls)
	}
	if !strings.Contains(f.sqls[0], "INSERT INTO state_history") {
		t.Errorf("first Exec SQL = %q, want state_history INSERT", f.sqls[0])
	}
	if len(f.queryRowSQLs) != 1 || !strings.Contains(f.queryRowSQLs[0], "UPDATE incarnation") {
		t.Errorf("QueryRow SQLs = %v, want single UPDATE incarnation … RETURNING", f.queryRowSQLs)
	}
	if !strings.Contains(f.queryRowSQLs[0], "status IN ('applying', 'destroying')") {
		t.Errorf("UPDATE missing the single-winner guard: %q", f.queryRowSQLs[0])
	}
}

// applying → success: single-winner commit succeeds (RETURNING returned a row).
func TestUpdateStateFromRun_SingleWinner_CommitsFromApplying(t *testing.T) {
	f := &multiExecFake{
		tags:      []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")},
		updateRow: staticRow{values: []any{"redis-prod"}},
	}
	err := UpdateStateFromRun(context.Background(), f,
		"redis-prod", "scale", "apply-id",
		map[string]any{"replicas": 1.0},
		map[string]any{"replicas": 3.0},
		StatusReady, nil, nil, "hist-id")
	if err != nil {
		t.Fatalf("commit from applying: %v", err)
	}
	// probe must not be called — UPDATE won the row immediately.
	if len(f.queryRowSQLs) != 1 {
		t.Errorf("QueryRow calls = %d, want 1 (UPDATE without probe)", len(f.queryRowSQLs))
	}
}

// Status is already terminal (ready/error_locked) → UPDATE affects 0 rows,
// probe sees a non-finalizable status → ErrAlreadyFinalized (single-winner
// no-op, NOT an error).
func TestUpdateStateFromRun_SingleWinner_AlreadyFinalized(t *testing.T) {
	for _, st := range []string{"ready", "error_locked", "destroy_failed"} {
		f := &multiExecFake{
			tags:      []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")},
			updateRow: errRow{err: pgx.ErrNoRows}, // guard didn't match — 0 rows
			probeRow:  staticRow{values: []any{st}},
		}
		err := UpdateStateFromRun(context.Background(), f,
			"redis-prod", "scale", "apply-id",
			nil, nil, StatusReady, nil, nil, "hist-id")
		if !errors.Is(err, ErrAlreadyFinalized) {
			t.Errorf("status=%s: err = %v, want ErrAlreadyFinalized", st, err)
		}
		// must have fetched status via probe-SELECT (UPDATE + probe = 2 QueryRow).
		if len(f.queryRowSQLs) != 2 {
			t.Errorf("status=%s: QueryRow calls = %d, want 2 (UPDATE + probe)", st, len(f.queryRowSQLs))
		}
	}
}

// No row exists at all: UPDATE 0 rows, probe → ErrNoRows → ErrIncarnationNotFound
// (caller contract preserved — don't confuse with the already-finalized no-op).
func TestUpdateStateFromRun_NotFound(t *testing.T) {
	f := &multiExecFake{
		tags:      []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")},
		updateRow: errRow{err: pgx.ErrNoRows},
		probeRow:  errRow{err: pgx.ErrNoRows},
	}
	err := UpdateStateFromRun(context.Background(), f,
		"ghost", "noop", "apply-id",
		nil, nil, StatusReady, nil, nil, "hist-id")
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Errorf("err = %v, want ErrIncarnationNotFound", err)
	}
}

func TestUpdateStateFromRun_RejectsBadName(t *testing.T) {
	f := &multiExecFake{}
	err := UpdateStateFromRun(context.Background(), f,
		"BAD_NAME", "s", "a", nil, nil, StatusReady, nil, nil, "h")
	if err == nil {
		t.Fatal("invalid name returned nil err")
	}
	if f.calls != 0 {
		t.Errorf("calls = %d, want 0 (validation before round-trip)", f.calls)
	}
}

func TestUpdateStateFromRun_RejectsBadStatus(t *testing.T) {
	f := &multiExecFake{}
	err := UpdateStateFromRun(context.Background(), f,
		"redis-prod", "s", "a", nil, nil, Status("frobnicated"), nil, nil, "h")
	if err == nil {
		t.Fatal("invalid status returned nil err")
	}
}

func TestUpdateStateFromRun_RejectsEmptyApplyID(t *testing.T) {
	f := &multiExecFake{}
	if err := UpdateStateFromRun(context.Background(), f,
		"redis-prod", "s", "", nil, nil, StatusReady, nil, nil, "h"); err == nil {
		t.Fatal("empty apply_id returned nil err")
	}
}

func TestUpdateStateFromRun_ErrorLockedWithDetails(t *testing.T) {
	f := &multiExecFake{
		tags:      []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")},
		updateRow: staticRow{values: []any{"redis-prod"}},
	}
	details := map[string]any{"reason": "run_failed"}
	err := UpdateStateFromRun(context.Background(), f,
		"redis-prod", "scale", "apply-id",
		map[string]any{"replicas": 1.0},
		map[string]any{"replicas": 1.0},
		StatusErrorLocked, details, nil, "hist-id")
	if err != nil {
		t.Fatalf("UpdateStateFromRun: %v", err)
	}
	// status_details travels as an arg of the final UPDATE … RETURNING (QueryRow):
	// we don't capture the final UPDATE's args in the QueryRow fake, so we
	// just verify the commit happened (err==nil) + that the guard is in the SQL.
	if len(f.queryRowSQLs) != 1 || !strings.Contains(f.queryRowSQLs[0], "UPDATE incarnation") {
		t.Fatalf("QueryRow SQLs = %v, want single UPDATE incarnation", f.queryRowSQLs)
	}
}

func TestValidName(t *testing.T) {
	good := []string{"a", "redis", "redis-prod", "redis-prod-01", "1redis"}
	bad := []string{"", "-leading", "Upper", "with_underscore", "x:colon",
		strings.Repeat("a", 64)}
	for _, n := range good {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

func TestValidStatus(t *testing.T) {
	valid := []Status{
		StatusReady, StatusApplying, StatusErrorLocked, StatusMigrationFailed,
		StatusDestroying, StatusDestroyFailed, StatusDrift,
	}
	for _, s := range valid {
		if !ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = false, want true", s)
		}
	}
	invalid := []Status{"", "provisioning", "DESTROY_FAILED", "destroyed"}
	for _, s := range invalid {
		if ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = true, want false", s)
		}
	}
}

// TestCreate_CreatedScenarioPassedThrough — an explicit CreatedScenario is
// written as an insert argument (multi-create mechanism: the incarnation
// remembers which start scenario created it).
func TestCreate_CreatedScenarioPassedThrough(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{time.Now(), time.Now()}}
		},
	}
	cs := "create_cluster"
	inc := &Incarnation{
		Name: "redis-cluster", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
		CreatedScenario: &cs,
	}
	if err := Create(context.Background(), f, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.queryRowArgs[11] != "create_cluster" {
		t.Errorf("args[11] created_scenario = %v, want create_cluster", f.queryRowArgs[11])
	}
}

// TestCreate_BareCreatedScenarioNull — a bare incarnation (CreatedScenario
// nil) → created_scenario is written as NULL insert argument (migration 090),
// not 'create'.
func TestCreate_BareCreatedScenarioNull(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ string) pgx.Row {
			return staticRow{values: []any{time.Now(), time.Now()}}
		},
	}
	inc := &Incarnation{
		Name: "redis-bare", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
		CreatedScenario: nil,
	}
	if err := Create(context.Background(), f, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.queryRowArgs[11] != nil {
		t.Errorf("args[11] created_scenario = %v, want nil (NULL for bare)", f.queryRowArgs[11])
	}
}

// TestSelectByName_ReadsCreatedScenario — created_scenario is read into
// Incarnation.CreatedScenario (round-trip scan), NULL → nil.
func TestSelectByName_ReadsCreatedScenario(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	makeF := func(createdScenario any) *fakeDB {
		return &fakeDB{
			queryRowFunc: func(_ string) pgx.Row {
				return staticRow{values: []any{
					"redis-cluster", "redis", "v1", 1,
					[]byte("{}"), []byte("{}"), "ready",
					[]byte(nil), any(nil), now, now, []string(nil),
					[]byte("{}"), any(nil), []byte(nil),
					createdScenario, // created_scenario (string | nil=NULL)
					any(nil),        // applying_apply_id
				}}
			},
		}
	}

	inc, err := SelectByName(context.Background(), makeF("create_cluster"), "redis-cluster")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if inc.CreatedScenario == nil || *inc.CreatedScenario != "create_cluster" {
		t.Errorf("CreatedScenario = %v, want create_cluster", inc.CreatedScenario)
	}

	// NULL → nil (bare incarnation).
	bare, err := SelectByName(context.Background(), makeF(nil), "redis-cluster")
	if err != nil {
		t.Fatalf("SelectByName(bare): %v", err)
	}
	if bare.CreatedScenario != nil {
		t.Errorf("bare CreatedScenario = %v, want nil (NULL)", bare.CreatedScenario)
	}
}

// TestSelectByName_ReadsApplyingApplyID — guard ADR-068 §A1: SelectByName reads
// the applying_apply_id column into Incarnation.ApplyingApplyID — non-null while
// a run is in progress (applying), nil at terminal (NULL). Read-source for the
// incarnation→live-run link.
func TestSelectByName_ReadsApplyingApplyID(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	makeF := func(applyingApplyID any) *fakeDB {
		return &fakeDB{
			queryRowFunc: func(_ string) pgx.Row {
				return staticRow{values: []any{
					"redis-cluster", "redis", "v1", 1,
					[]byte("{}"), []byte("{}"), "applying",
					[]byte(nil), any(nil), now, now, []string(nil),
					[]byte("{}"), any(nil), []byte(nil),
					"create",        // created_scenario
					applyingApplyID, // applying_apply_id (string=applying | nil=terminal)
				}}
			},
		}
	}

	// Run in progress → non-null apply_id.
	applying, err := SelectByName(context.Background(), makeF("01HAPPLYINGRUN000000000000"), "redis-cluster")
	if err != nil {
		t.Fatalf("SelectByName(applying): %v", err)
	}
	if applying.ApplyingApplyID == nil || *applying.ApplyingApplyID != "01HAPPLYINGRUN000000000000" {
		t.Errorf("ApplyingApplyID = %v, want 01HAPPLYINGRUN000000000000 (non-null while applying)", applying.ApplyingApplyID)
	}

	// Terminal → NULL → nil.
	terminal, err := SelectByName(context.Background(), makeF(nil), "redis-cluster")
	if err != nil {
		t.Fatalf("SelectByName(terminal): %v", err)
	}
	if terminal.ApplyingApplyID != nil {
		t.Errorf("terminal ApplyingApplyID = %v, want nil (NULL at terminal)", terminal.ApplyingApplyID)
	}
}
