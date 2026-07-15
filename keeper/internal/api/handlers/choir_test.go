package handlers

// T5d-2c handler-native: choir/voice (w,r) wrappers removed — HTTP is served by huma
// full-typed (huma_choir_test.go: golden-wire / unknown-field-400 / bad-name-422 /
// RBAC-deny-403 / self-audit on the real huma wiring). These unit tests cover what
// huma-integration does NOT: the DOMAIN error classification of *Typed functions
// (sentinel→problem.Type) + created_by_aid/added_by_aid from JWT (NOT from the body).
// They call *Typed directly, without httptest(w,r).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// choirClaims builds keeperjwt.Claims for calling *Typed directly.
func choirClaims(subject string) *keeperjwt.Claims { return &keeperjwt.Claims{Subject: subject} }

// --- fake ChoirDB ------------------------------------------------------

// fakeChoirDB is a configurable [ChoirDB] mock for the ChoirHandler unit test.
// Dispatches by SQL substring (insert/select/list/delete/membership). Each field
// sets the outcome of the matching operation; nil outcome → success by default.
// fakeChoirDB implements pgx.Tx itself (via delegate methods), so BeginTx returns
// itself.
type fakeChoirDB struct {
	// CreateChoir: QueryRow(insertChoirSQL).Scan(&CreatedAt).
	insertChoirErr error
	// AddVoice: QueryRow(selectChoirForUpdateSQL).Scan(&dummy).
	choirLockRows pgx.Row // nil → success (returns 1); errRow{pgx.ErrNoRows} → ErrChoirNotFound
	// AddVoice: Query(membershipSQL) — which SIDs are actually members of the incarnation.
	memberSIDs []string
	// AddVoice: QueryRow(insertVoiceSQL).Scan(&AddedAt).
	insertVoiceErr error
	// ListChoirs: Query(listChoirsSQL).
	listChoirs [][]any
	// ListVoices: Query(listVoicesSQL).
	listVoices [][]any
	// DeleteChoir / RemoveVoice: Exec → RowsAffected.
	deleteChoirRows int
	deleteVoiceRows int
}

func (f *fakeChoirDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "DELETE FROM incarnation_choirs"):
		return pgconn.NewCommandTag("DELETE " + itoa(int64(f.deleteChoirRows))), nil
	case strings.Contains(sql, "DELETE FROM incarnation_choir_voices"):
		return pgconn.NewCommandTag("DELETE " + itoa(int64(f.deleteVoiceRows))), nil
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeChoirDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO incarnation_choirs"):
		if f.insertChoirErr != nil {
			return errRow{err: f.insertChoirErr}
		}
		return staticRow{values: []any{time.Now()}}
	case strings.Contains(sql, "FOR UPDATE"):
		if f.choirLockRows != nil {
			return f.choirLockRows
		}
		return staticRow{values: []any{1}}
	case strings.Contains(sql, "INSERT INTO incarnation_choir_voices"):
		if f.insertVoiceErr != nil {
			return errRow{err: f.insertVoiceErr}
		}
		return staticRow{values: []any{time.Now()}}
	}
	return errRow{err: pgx.ErrNoRows}
}

func (f *fakeChoirDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM souls"):
		rows := make([][]any, 0, len(f.memberSIDs))
		for _, s := range f.memberSIDs {
			rows = append(rows, []any{s})
		}
		return &fakeChoirRows{rows: rows}, nil
	case strings.Contains(sql, "FROM incarnation_choirs"):
		return &fakeChoirRows{rows: f.listChoirs}, nil
	case strings.Contains(sql, "FROM incarnation_choir_voices"):
		return &fakeChoirRows{rows: f.listVoices}, nil
	}
	return &fakeChoirRows{}, nil
}

// BeginTx returns the fakeChoirDB itself as pgx.Tx (delegate Exec/Query/QueryRow).
func (f *fakeChoirDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &fakeChoirTx{db: f}, nil
}

// fakeChoirTx is a pgx.Tx wrapper over fakeChoirDB.
type fakeChoirTx struct{ db *fakeChoirDB }

func (t *fakeChoirTx) Begin(ctx context.Context) (pgx.Tx, error) { return t, nil }
func (t *fakeChoirTx) Commit(_ context.Context) error            { return nil }
func (t *fakeChoirTx) Rollback(_ context.Context) error          { return nil }
func (t *fakeChoirTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("fakeChoirTx.CopyFrom: unexpected")
}
func (t *fakeChoirTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("fakeChoirTx.SendBatch: unexpected")
}
func (t *fakeChoirTx) LargeObjects() pgx.LargeObjects { panic("fakeChoirTx.LargeObjects: unexpected") }
func (t *fakeChoirTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("fakeChoirTx.Prepare: unexpected")
}
func (t *fakeChoirTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}
func (t *fakeChoirTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}
func (t *fakeChoirTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (t *fakeChoirTx) Conn() *pgx.Conn { return nil }

// fakeChoirRows is a pgx.Rows over [][]any (scan by dest types).
type fakeChoirRows struct {
	rows [][]any
	idx  int
}

func (r *fakeChoirRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeChoirRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		switch dst := d.(type) {
		case *string:
			*dst = row[i].(string)
		case *time.Time:
			*dst = row[i].(time.Time)
		case **string:
			if row[i] == nil {
				*dst = nil
				continue
			}
			s := row[i].(string)
			*dst = &s
		case **int:
			if row[i] == nil {
				*dst = nil
				continue
			}
			n := row[i].(int)
			*dst = &n
		}
	}
	return nil
}

func (r *fakeChoirRows) Err() error                                   { return nil }
func (r *fakeChoirRows) Close()                                       {}
func (r *fakeChoirRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeChoirRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeChoirRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeChoirRows) RawValues() [][]byte                          { return nil }
func (r *fakeChoirRows) Conn() *pgx.Conn                              { return nil }

// wantChoirProblem checks that err is a domain *problemError with the expected problem.Type.
func wantChoirProblem(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ожидалась ошибка %q, получено nil", want)
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("ошибка не *problemError: %v", err)
	}
	if d.Type != want {
		t.Errorf("problem.Type = %q, want %q", d.Type, want)
	}
}

// --- Create ------------------------------------------------------------

func TestChoir_CreateTyped_201(t *testing.T) {
	h := NewChoirHandler(&fakeChoirDB{}, nil, nil)
	view, err := h.CreateTyped(context.Background(), choirClaims("archon-alice"), "redis", ChoirCreateInput{ChoirName: "redis_primary"})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
	if view.ChoirName != "redis_primary" || view.IncarnationName != "redis" {
		t.Fatalf("view = %+v", view)
	}
	if view.CreatedByAID == nil || *view.CreatedByAID != "archon-alice" {
		t.Fatalf("created_by_aid = %v, want archon-alice (from JWT)", view.CreatedByAID)
	}
}

func TestChoir_CreateTyped_InvalidName_422(t *testing.T) {
	h := NewChoirHandler(&fakeChoirDB{}, nil, nil)
	_, err := h.CreateTyped(context.Background(), choirClaims("archon-alice"), "redis", ChoirCreateInput{ChoirName: "Bad-Name!"})
	wantChoirProblem(t, err, problem.TypeValidationFailed)
}

func TestChoir_CreateTyped_Exists_409(t *testing.T) {
	db := &fakeChoirDB{insertChoirErr: &pgconn.PgError{Code: "23505", ConstraintName: "incarnation_choirs_pkey"}}
	h := NewChoirHandler(db, nil, nil)
	_, err := h.CreateTyped(context.Background(), choirClaims("archon-alice"), "redis", ChoirCreateInput{ChoirName: "redis_primary"})
	wantChoirProblem(t, err, problem.TypeChoirExists)
}

func TestChoir_CreateTyped_IncarnationNotFound_404(t *testing.T) {
	db := &fakeChoirDB{insertChoirErr: &pgconn.PgError{Code: "23503", ConstraintName: "incarnation_choirs_incarnation_fk"}}
	h := NewChoirHandler(db, nil, nil)
	_, err := h.CreateTyped(context.Background(), choirClaims("archon-alice"), "ghost", ChoirCreateInput{ChoirName: "redis_primary"})
	wantChoirProblem(t, err, problem.TypeNotFound)
}

// --- List --------------------------------------------------------------

func TestChoir_ListChoirsTyped_200(t *testing.T) {
	now := time.Now()
	db := &fakeChoirDB{listChoirs: [][]any{
		{"redis", "redis_primary", nil, nil, nil, now, "archon-alice"},
		{"redis", "redis_replica", nil, nil, nil, now, nil},
	}}
	h := NewChoirHandler(db, nil, nil)
	page, err := h.ListChoirsTyped(context.Background(), "redis")
	if err != nil {
		t.Fatalf("ListChoirsTyped: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].ChoirName != "redis_primary" {
		t.Fatalf("items = %+v", page.Items)
	}
}

func TestChoir_ListChoirsTyped_Empty_NonNil(t *testing.T) {
	h := NewChoirHandler(&fakeChoirDB{}, nil, nil)
	page, err := h.ListChoirsTyped(context.Background(), "redis")
	if err != nil {
		t.Fatalf("ListChoirsTyped: %v", err)
	}
	if page.Items == nil {
		t.Errorf("Items должен быть non-nil пустым срезом")
	}
}

// --- Delete ------------------------------------------------------------

func TestChoir_DeleteTyped_204(t *testing.T) {
	db := &fakeChoirDB{deleteChoirRows: 1}
	h := NewChoirHandler(db, nil, nil)
	if err := h.DeleteTyped(context.Background(), choirClaims("archon-alice"), "redis", "redis_primary"); err != nil {
		t.Fatalf("DeleteTyped: %v", err)
	}
}

func TestChoir_DeleteTyped_NotFound_404(t *testing.T) {
	db := &fakeChoirDB{deleteChoirRows: 0}
	h := NewChoirHandler(db, nil, nil)
	err := h.DeleteTyped(context.Background(), choirClaims("archon-alice"), "redis", "ghost")
	wantChoirProblem(t, err, problem.TypeNotFound)
}

// --- AddVoice ----------------------------------------------------------

func TestChoir_AddVoiceTyped_201(t *testing.T) {
	db := &fakeChoirDB{memberSIDs: []string{"node-1.example.com"}}
	h := NewChoirHandler(db, nil, nil)
	view, err := h.AddVoiceTyped(context.Background(), choirClaims("archon-alice"), "redis", "redis_primary", VoiceAddInput{SID: "node-1.example.com"})
	if err != nil {
		t.Fatalf("AddVoiceTyped: %v", err)
	}
	if view.SID != "node-1.example.com" {
		t.Fatalf("sid = %q", view.SID)
	}
	if view.AddedByAID == nil || *view.AddedByAID != "archon-alice" {
		t.Fatalf("added_by_aid = %v, want archon-alice (from JWT)", view.AddedByAID)
	}
}

func TestChoir_AddVoiceTyped_ChoirNotFound_404(t *testing.T) {
	db := &fakeChoirDB{choirLockRows: errRow{err: pgx.ErrNoRows}}
	h := NewChoirHandler(db, nil, nil)
	_, err := h.AddVoiceTyped(context.Background(), choirClaims("archon-alice"), "redis", "ghost", VoiceAddInput{SID: "node-1.example.com"})
	wantChoirProblem(t, err, problem.TypeNotFound)
}

// TestChoir_AddVoiceTyped_NotMembers_422 — SID is not a member of the incarnation
// (membership query didn't return it) → ErrNotMembers → 422 (invariant ADR-044 item 3).
func TestChoir_AddVoiceTyped_NotMembers_422(t *testing.T) {
	db := &fakeChoirDB{memberSIDs: nil} // nobody is a member
	h := NewChoirHandler(db, nil, nil)
	_, err := h.AddVoiceTyped(context.Background(), choirClaims("archon-alice"), "redis", "redis_primary", VoiceAddInput{SID: "stranger.example.com"})
	wantChoirProblem(t, err, problem.TypeValidationFailed)
	d, _ := AsProblemDetails(err)
	if !strings.Contains(d.Detail, "stranger.example.com") {
		t.Fatalf("detail should name the offending SID; got %s", d.Detail)
	}
}

func TestChoir_AddVoiceTyped_Exists_409(t *testing.T) {
	db := &fakeChoirDB{
		memberSIDs:     []string{"node-1.example.com"},
		insertVoiceErr: &pgconn.PgError{Code: "23505", ConstraintName: "incarnation_choir_voices_pkey"},
	}
	h := NewChoirHandler(db, nil, nil)
	_, err := h.AddVoiceTyped(context.Background(), choirClaims("archon-alice"), "redis", "redis_primary", VoiceAddInput{SID: "node-1.example.com"})
	wantChoirProblem(t, err, problem.TypeVoiceExists)
}

func TestChoir_AddVoiceTyped_InvalidSID_422(t *testing.T) {
	h := NewChoirHandler(&fakeChoirDB{}, nil, nil)
	_, err := h.AddVoiceTyped(context.Background(), choirClaims("archon-alice"), "redis", "redis_primary", VoiceAddInput{SID: "BAD SID"})
	wantChoirProblem(t, err, problem.TypeValidationFailed)
}

// --- ListVoices / RemoveVoice -----------------------------------------

func TestChoir_ListVoicesTyped_200(t *testing.T) {
	now := time.Now()
	db := &fakeChoirDB{listVoices: [][]any{
		{"redis", "redis_primary", "node-1.example.com", "primary", 0, now, "archon-alice"},
	}}
	h := NewChoirHandler(db, nil, nil)
	page, err := h.ListVoicesTyped(context.Background(), "redis", "redis_primary")
	if err != nil {
		t.Fatalf("ListVoicesTyped: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].SID != "node-1.example.com" {
		t.Fatalf("items = %+v", page.Items)
	}
}

func TestChoir_RemoveVoiceTyped_204(t *testing.T) {
	db := &fakeChoirDB{deleteVoiceRows: 1}
	h := NewChoirHandler(db, nil, nil)
	if err := h.RemoveVoiceTyped(context.Background(), choirClaims("archon-alice"), "redis", "redis_primary", "node-1.example.com"); err != nil {
		t.Fatalf("RemoveVoiceTyped: %v", err)
	}
}

func TestChoir_RemoveVoiceTyped_NotFound_404(t *testing.T) {
	db := &fakeChoirDB{deleteVoiceRows: 0}
	h := NewChoirHandler(db, nil, nil)
	err := h.RemoveVoiceTyped(context.Background(), choirClaims("archon-alice"), "redis", "redis_primary", "ghost.example.com")
	wantChoirProblem(t, err, problem.TypeNotFound)
}
