package augur

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
)

// fakeDB is an ExecQueryRower stub. queryRowFunc receives SQL and call ordinal number.
// InsertRite makes two QueryRow calls: SelectOmenByID, then INSERT.
type fakeDB struct {
	queryRowSQL   string
	queryRowArgs  []any
	queryRowFunc  func(call int, sql string) pgx.Row
	queryRowCalls int

	querySQL   string
	queryArgs  []any
	queryFunc  func(sql string) (pgx.Rows, error)
	queryCalls int

	execTag   pgconn.CommandTag
	execErr   error
	execSQL   string
	execCalls int
}

func (f *fakeDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.execCalls++
	f.execSQL = sql
	return f.execTag, f.execErr
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.queryRowCalls++
	f.queryRowSQL = sql
	f.queryRowArgs = args
	if f.queryRowFunc != nil {
		return f.queryRowFunc(f.queryRowCalls, sql)
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
	case *bool:
		*d = src.(bool)
	case *time.Time:
		*d = src.(time.Time)
	case *[]byte:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]byte)
		}
	case *[]string:
		// The array-valued subject dimensions (sid, coven). A SQL NULL and an empty
		// array both arrive as a nil slice, which is what the domain reads as
		// "this dimension was not written".
		if src == nil {
			*d = nil
		} else {
			*d = src.([]string)
		}
	case **string:
		if src == nil {
			*d = nil
		} else {
			s := src.(string)
			*d = &s
		}
	case **int:
		if src == nil {
			*d = nil
		} else {
			n := src.(int)
			*d = &n
		}
	default:
		panic("staticRow.assign: unsupported dest type")
	}
}

type fakeRows struct {
	rows []staticRow
	idx  int
	err  error
}

func (r *fakeRows) Next() bool {
	if r.err != nil || r.idx >= len(r.rows) {
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

func ptr[T any](v T) *T { return &v }

var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// omenRow returns SELECT-row values for omens in omenColumns order. The trailing
// nil is `label` (ADR-0085): a display caption these fixtures do not set, which
// reads NULL and makes a consumer show the name.
func omenRow(name, src, endpoint, authRef string, aid any) []any {
	return []any{name, src, endpoint, authRef, aid, testNow, any(nil)}
}

// --- InsertOmen -------------------------------------------------------

func validOmen() *Omen {
	return &Omen{
		ID:           "vault-prod",
		SourceType:   SourceVault,
		Endpoint:     "https://vault.internal:8200",
		AuthRef:      "vault:secret/keeper/augur/vault-prod",
		CreatedByAID: ptr("archon-alice"),
	}
}

func TestInsertOmen_HappyPath(t *testing.T) {
	f := &fakeDB{queryRowFunc: func(_ int, _ string) pgx.Row {
		return staticRow{values: []any{testNow}}
	}}
	o := validOmen()
	if err := InsertOmen(context.Background(), f, o); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	if !o.CreatedAt.Equal(testNow) {
		t.Errorf("CreatedAt not filled: %v", o.CreatedAt)
	}
	if !strings.Contains(f.queryRowSQL, "INSERT INTO omens") {
		t.Errorf("SQL: %q", f.queryRowSQL)
	}
	if len(f.queryRowArgs) != 6 {
		t.Fatalf("args len = %d, want 6", len(f.queryRowArgs))
	}
	if f.queryRowArgs[1] != "vault" {
		t.Errorf("args[1] source_type = %v, want vault", f.queryRowArgs[1])
	}
}

func TestInsertOmen_NilCreatedBy(t *testing.T) {
	f := &fakeDB{queryRowFunc: func(_ int, _ string) pgx.Row { return staticRow{values: []any{testNow}} }}
	o := validOmen()
	o.CreatedByAID = nil
	if err := InsertOmen(context.Background(), f, o); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	if f.queryRowArgs[4] != nil {
		t.Errorf("args[4] = %v, want nil", f.queryRowArgs[4])
	}
}

func TestInsertOmen_RejectsBadName(t *testing.T) {
	f := &fakeDB{}
	o := validOmen()
	o.ID = "Vault_Prod"
	if err := InsertOmen(context.Background(), f, o); err == nil ||
		!strings.Contains(err.Error(), "invalid omen id") {
		t.Fatalf("err = %v, want invalid omen id", err)
	}
	if f.queryRowCalls != 0 {
		t.Errorf("queryRowCalls = %d on bad name; want 0", f.queryRowCalls)
	}
}

func TestInsertOmen_RejectsBadSourceType(t *testing.T) {
	f := &fakeDB{}
	o := validOmen()
	o.SourceType = "mysql"
	if err := InsertOmen(context.Background(), f, o); err == nil ||
		!strings.Contains(err.Error(), "invalid source_type") {
		t.Fatalf("err = %v, want invalid source_type", err)
	}
}

func TestInsertOmen_RejectsEmptyEndpoint(t *testing.T) {
	f := &fakeDB{}
	o := validOmen()
	o.Endpoint = ""
	if err := InsertOmen(context.Background(), f, o); err == nil {
		t.Fatal("InsertOmen with empty endpoint returned nil")
	}
}

func TestInsertOmen_RejectsBadAuthRef(t *testing.T) {
	f := &fakeDB{}
	for _, ref := range []string{"", "vault:", "secret/x", "env:FOO", "vault:onlymount", "vault:secret/../x"} {
		o := validOmen()
		o.AuthRef = ref
		if err := InsertOmen(context.Background(), f, o); err == nil {
			t.Errorf("InsertOmen with auth_ref %q returned nil", ref)
		}
	}
	if f.queryRowCalls != 0 {
		t.Errorf("queryRowCalls = %d on bad ref; want 0", f.queryRowCalls)
	}
}

func TestInsertOmen_MapsUniqueViolation(t *testing.T) {
	f := &fakeDB{queryRowFunc: func(_ int, _ string) pgx.Row {
		return errRow{err: &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "omens_pkey"}}
	}}
	err := InsertOmen(context.Background(), f, validOmen())
	if !errors.Is(err, ErrOmenAlreadyExists) {
		t.Fatalf("err = %v, want ErrOmenAlreadyExists", err)
	}
}

// --- SelectOmenByID / SelectAllOmens / DeleteOmen -------------------

func TestSelectOmenByName_HappyPath(t *testing.T) {
	f := &fakeDB{queryRowFunc: func(_ int, _ string) pgx.Row {
		return staticRow{values: omenRow("prom-main", "prometheus", "https://prom:9090", "vault:secret/k/prom", any("archon-alice"))}
	}}
	o, err := SelectOmenByID(context.Background(), f, "prom-main")
	if err != nil {
		t.Fatalf("SelectOmenByID: %v", err)
	}
	if o.SourceType != SourcePrometheus {
		t.Errorf("SourceType = %q", o.SourceType)
	}
	if o.CreatedByAID == nil || *o.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", o.CreatedByAID)
	}
}

func TestSelectOmenByName_NotFound(t *testing.T) {
	f := &fakeDB{}
	if _, err := SelectOmenByID(context.Background(), f, "missing"); !errors.Is(err, ErrOmenNotFound) {
		t.Fatalf("err = %v, want ErrOmenNotFound", err)
	}
}

func TestSelectAllOmens_HappyPath(t *testing.T) {
	f := &fakeDB{
		queryRowFunc: func(_ int, _ string) pgx.Row { return staticRow{values: []any{int(2)}} },
		queryFunc: func(_ string) (pgx.Rows, error) {
			return &fakeRows{rows: []staticRow{
				{values: omenRow("a", "vault", "e", "vault:secret/a", any(nil))},
				{values: omenRow("b", "elk", "e", "vault:secret/b", any("archon-alice"))},
			}}, nil
		},
	}
	out, total, err := SelectAllOmens(context.Background(), f, 0, 50)
	if err != nil {
		t.Fatalf("SelectAllOmens: %v", err)
	}
	if total != 2 || len(out) != 2 {
		t.Fatalf("total=%d len=%d, want 2/2", total, len(out))
	}
	if !strings.Contains(f.querySQL, "ORDER BY created_at DESC") {
		t.Errorf("ORDER missing: %q", f.querySQL)
	}
}

func TestSelectAllOmens_RejectsBadPaging(t *testing.T) {
	f := &fakeDB{}
	if _, _, err := SelectAllOmens(context.Background(), f, -1, 50); err == nil {
		t.Error("negative offset accepted")
	}
	if _, _, err := SelectAllOmens(context.Background(), f, 0, 0); err == nil {
		t.Error("zero limit accepted")
	}
}

func TestDeleteOmen_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 1")}
	if err := DeleteOmen(context.Background(), f, "vault-prod"); err != nil {
		t.Fatalf("DeleteOmen: %v", err)
	}
	if !strings.Contains(f.execSQL, "DELETE FROM omens") {
		t.Errorf("SQL: %q", f.execSQL)
	}
}

func TestDeleteOmen_NotFound(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 0")}
	if err := DeleteOmen(context.Background(), f, "missing"); !errors.Is(err, ErrOmenNotFound) {
		t.Fatalf("err = %v, want ErrOmenNotFound", err)
	}
}

// --- InsertRite -------------------------------------------------------

func vaultRite() *Rite {
	return &Rite{
		Omen:         "vault-prod",
		Coven:        []string{"web"},
		Allow:        json.RawMessage(`{"paths":["secret/app/db"]}`),
		Delegate:     true,
		TokenTTL:     ptr("5m"),
		TokenNumUses: ptr(3),
		CreatedByAID: ptr("archon-alice"),
	}
}

// insertRiteFake returns a fakeDB where first QueryRow returns an Omen with given
// source_type, and the second (INSERT) returns id/created_at via RETURNING.
func insertRiteFake(src string) *fakeDB {
	return &fakeDB{queryRowFunc: func(call int, _ string) pgx.Row {
		if call == 1 {
			return staticRow{values: omenRow("vault-prod", src, "e", "vault:secret/k/x", any(nil))}
		}
		return staticRow{values: []any{int64(42), testNow}}
	}}
}

func TestInsertRite_VaultDelegate_HappyPath(t *testing.T) {
	f := insertRiteFake("vault")
	r := vaultRite()
	if err := InsertRite(context.Background(), f, r); err != nil {
		t.Fatalf("InsertRite: %v", err)
	}
	if r.ID != 42 || !r.CreatedAt.Equal(testNow) {
		t.Errorf("id/created_at not filled: %d / %v", r.ID, r.CreatedAt)
	}
	if f.queryRowCalls != 2 {
		t.Errorf("queryRowCalls = %d, want 2 (omen lookup + insert)", f.queryRowCalls)
	}
}

func TestInsertRite_OmenNotFound(t *testing.T) {
	f := &fakeDB{} // QueryRow default → ErrNoRows
	if err := InsertRite(context.Background(), f, vaultRite()); !errors.Is(err, ErrOmenNotFound) {
		t.Fatalf("err = %v, want ErrOmenNotFound", err)
	}
}

func TestInsertRite_RejectsSubjectNotExactlyOne(t *testing.T) {
	f := &fakeDB{}
	// two dimensions specified
	r := vaultRite()
	r.SID = []string{"host.example.com"}
	if err := InsertRite(context.Background(), f, r); err == nil ||
		!strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("two-dimension err = %v, want exactly-one-of", err)
	}
	// no subject
	r2 := vaultRite()
	r2.Coven = nil
	if err := InsertRite(context.Background(), f, r2); err == nil ||
		!strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("no-subject err = %v, want exactly-one-of", err)
	}
	if f.queryRowCalls != 0 {
		t.Errorf("queryRowCalls = %d on subject-fail; want 0 (no omen lookup)", f.queryRowCalls)
	}
}

func TestInsertRite_RejectsBadCovenFormat(t *testing.T) {
	f := &fakeDB{}
	r := vaultRite()
	r.Coven = []string{"-bad-start"}
	if err := InsertRite(context.Background(), f, r); err == nil ||
		!strings.Contains(err.Error(), "invalid subject coven") {
		t.Fatalf("err = %v, want invalid subject coven", err)
	}
}

func TestInsertRite_TokenFieldsRequireVault(t *testing.T) {
	// Omen is prometheus, but Rite carries token-fields → service layer catches this as vault-only.
	f := insertRiteFake("prometheus")
	r := &Rite{
		Omen:     "vault-prod",
		Coven:    []string{"web"},
		Allow:    json.RawMessage(`{"queries":["up"]}`),
		Delegate: true,
		TokenTTL: ptr("5m"),
	}
	if err := InsertRite(context.Background(), f, r); err == nil ||
		!strings.Contains(err.Error(), "vault-only") {
		t.Fatalf("err = %v, want vault-only", err)
	}
}

func TestInsertRite_TokenFieldsRequireDelegate(t *testing.T) {
	f := insertRiteFake("vault")
	r := &Rite{
		Omen:     "vault-prod",
		Coven:    []string{"web"},
		Allow:    json.RawMessage(`{"paths":["secret/x"]}`),
		Delegate: false,
		TokenTTL: ptr("5m"),
	}
	if err := InsertRite(context.Background(), f, r); err == nil ||
		!strings.Contains(err.Error(), "delegate=true") {
		t.Fatalf("err = %v, want delegate=true", err)
	}
}

func TestInsertRite_RejectsBadTokenTTL(t *testing.T) {
	f := insertRiteFake("vault")
	r := vaultRite()
	r.TokenTTL = ptr("5banana")
	if err := InsertRite(context.Background(), f, r); err == nil ||
		!strings.Contains(err.Error(), "token_ttl") {
		t.Fatalf("err = %v, want token_ttl", err)
	}
}

func TestInsertRite_RejectsAllowShapeMismatch(t *testing.T) {
	// Omen is vault, but allow is in prometheus form → ValidateAllow catches this.
	f := insertRiteFake("vault")
	r := &Rite{
		Omen:  "vault-prod",
		Coven: []string{"web"},
		Allow: json.RawMessage(`{"queries":["up"]}`),
	}
	if err := InsertRite(context.Background(), f, r); err == nil ||
		!strings.Contains(err.Error(), "vault") {
		t.Fatalf("err = %v, want vault allow-shape", err)
	}
}

func TestInsertRite_SIDSubject(t *testing.T) {
	f := insertRiteFake("elk")
	r := &Rite{
		Omen:  "vault-prod",
		SID:   []string{"host.example.com"},
		Allow: json.RawMessage(`{"indices":["logs-*"]}`),
	}
	if err := InsertRite(context.Background(), f, r); err != nil {
		t.Fatalf("InsertRite (sid/elk): %v", err)
	}
	// args: omen, sid, service, incarnation, coven, trait_key, trait_value, allow, …
	// Every dimension the Rite was NOT written with must go to the DB as NULL: a
	// leftover value would widen the grant through a dimension nobody wrote.
	if sids, ok := f.queryRowArgs[1].([]string); !ok || len(sids) != 1 || sids[0] != "host.example.com" {
		t.Errorf("sid arg = %v", f.queryRowArgs[1])
	}
	for i, name := range []string{"service", "incarnation", "coven", "trait_key", "trait_value"} {
		if v := f.queryRowArgs[2+i]; v != nil && !isNilPtr(v) {
			t.Errorf("%s arg = %v, want NULL", name, v)
		}
	}
}

// isNilPtr — a typed nil pointer (*string) still reads as non-nil through `any`;
// pgx sends it as NULL, so the assertion above must see it the same way.
func isNilPtr(v any) bool {
	switch t := v.(type) {
	case *string:
		return t == nil
	case []string:
		return t == nil
	default:
		return false
	}
}

// --- SelectRitesByOmen / BySubject / DeleteRite ----------------------

// riteRow builds one row in riteColumns order:
// id, omen, sid, service, incarnation, coven, trait_key, trait_value,
// allow, delegate, token_ttl, token_num_uses, created_by_aid, created_at.
func riteRow(id int64, omen string, coven, sid any, allow []byte, delegate bool) []any {
	return []any{
		id, omen,
		sid, any(nil), any(nil), coven, any(nil), any(nil),
		allow, delegate, any(nil), any(nil), any(nil), testNow,
	}
}

func TestSelectRitesByOmen(t *testing.T) {
	f := &fakeDB{queryFunc: func(_ string) (pgx.Rows, error) {
		return &fakeRows{rows: []staticRow{
			{values: riteRow(1, "vault-prod", any([]string{"web"}), any(nil), []byte(`{"paths":["x"]}`), false)},
		}}, nil
	}}
	out, err := SelectRitesByOmen(context.Background(), f, "vault-prod")
	if err != nil {
		t.Fatalf("SelectRitesByOmen: %v", err)
	}
	if len(out) != 1 || out[0].ID != 1 {
		t.Fatalf("got %+v", out)
	}
	if len(out[0].Coven) != 1 || out[0].Coven[0] != "web" {
		t.Errorf("coven = %v", out[0].Coven)
	}
	if string(out[0].Allow) != `{"paths":["x"]}` {
		t.Errorf("allow = %s", out[0].Allow)
	}
	if f.queryArgs[0] != "vault-prod" {
		t.Errorf("arg = %v", f.queryArgs[0])
	}
}

func TestSelectRitesBySubject(t *testing.T) {
	f := &fakeDB{queryFunc: func(_ string) (pgx.Rows, error) {
		return &fakeRows{rows: []staticRow{
			{values: riteRow(2, "vault-prod", any(nil), any([]string{"host.example.com"}), []byte(`{"paths":["x"]}`), true)},
		}}, nil
	}}
	host := subject.Host{
		SID:    "host.example.com",
		Covens: []string{"web", "db"},
		Traits: map[string]any{"tier": "gold"},
		Member: []subject.Incarnation{{Service: "redis", Name: "redis-prod", Covens: []string{"prod"}}},
	}
	out, err := SelectRitesBySubject(context.Background(), f, host)
	if err != nil {
		t.Fatalf("SelectRitesBySubject: %v", err)
	}
	if len(out) != 1 || len(out[0].SID) != 1 || out[0].SID[0] != "host.example.com" {
		t.Fatalf("got %+v", out)
	}
	// The predicate consumes the ALREADY-RESOLVED host as flat value arrays — no
	// joins — which is what keeps the SQL prefilter and the Go matcher from
	// drifting apart. The host's own covens and its incarnations' covens arrive
	// in one array: the two levels are unioned before the query, not inside it.
	if sid, ok := f.queryArgs[0].(string); !ok || sid != "host.example.com" {
		t.Errorf("sid arg = %v", f.queryArgs[0])
	}
	covens, ok := f.queryArgs[1].([]string)
	if !ok {
		t.Fatalf("coven arg is %T, want []string", f.queryArgs[1])
	}
	for _, want := range []string{"web", "db", "prod"} {
		if !slices.Contains(covens, want) {
			t.Errorf("coven arg %v is missing %q (both levels must be unioned)", covens, want)
		}
	}
}

func TestDeleteRite_HappyPath(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 1")}
	if err := DeleteRite(context.Background(), f, 42); err != nil {
		t.Fatalf("DeleteRite: %v", err)
	}
	if !strings.Contains(f.execSQL, "DELETE FROM rites") {
		t.Errorf("SQL: %q", f.execSQL)
	}
}

func TestDeleteRite_NotFound(t *testing.T) {
	f := &fakeDB{execTag: pgconn.NewCommandTag("DELETE 0")}
	if err := DeleteRite(context.Background(), f, 99); !errors.Is(err, ErrRiteNotFound) {
		t.Fatalf("err = %v, want ErrRiteNotFound", err)
	}
}
