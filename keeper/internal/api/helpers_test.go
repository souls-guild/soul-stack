package api

// Shared test fixtures for the api_test package: fake-pool/row/rows stubs, an
// allow-all PermissionChecker, an audit.Writer capturer. Used by the huma
// guards of all domains (400/422 contract validation, wire shape,
// audit-on-write). They used to live in the strict test files; after the
// strict aggregate was torn down (teardown T2) they were moved here as
// neutral infrastructure — carries NO strict wiring (bridge/wrapper/server).

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- minimal fake CadenceStore (cadence huma guards) ---

// strictFakeCadenceStore serves INSERT (success → timestamps) and
// SELECT COUNT (list). Cadence huma guards additionally need selectByID (Get
// for PATCH/Runs), UPDATE...RETURNING (PatchTyped), UPDATE...enabled
// (SetEnabledTyped), and DELETE (DeleteTyped) — optional fields, defaulting to
// not-found (404). Any other SQL is an error.
type strictFakeCadenceStore struct {
	insertCalls int

	// selectByID — a configurable cadence row for Get (PATCH/Runs); nil → 404.
	selectByID func(id string) pgx.Row
	// not-found flags for cadence write operations.
	updateNoRows    bool
	setEnabledNoRow bool
	deleteNoRow     bool
}

func (f *strictFakeCadenceStore) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "UPDATE cadences SET\n    enabled"):
		if f.setEnabledNoRow {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "DELETE FROM cadences"):
		if f.deleteNoRow {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.CommandTag{}, errStrictUnexpectedSQL
}

func (f *strictFakeCadenceStore) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO cadences"):
		f.insertCalls++
		return strictScalarRow{vals: []any{time.Now().UTC(), time.Now().UTC()}}
	case strings.Contains(sql, "UPDATE cadences SET") && strings.Contains(sql, "RETURNING created_at, updated_at"):
		if f.updateNoRows {
			return strictErrRow{err: pgx.ErrNoRows}
		}
		return strictScalarRow{vals: []any{time.Now().UTC(), time.Now().UTC()}}
	case strings.Contains(sql, "FROM cadences\nWHERE id = $1"):
		if f.selectByID != nil {
			return f.selectByID(args[0].(string))
		}
		return strictErrRow{err: pgx.ErrNoRows}
	case strings.Contains(sql, "SELECT COUNT(*) FROM cadences"):
		return strictScalarRow{vals: []any{0}}
	case strings.Contains(sql, "SELECT COUNT(*) FROM voyages"):
		// GET /v1/cadences/{id}/runs — voyage.List COUNT (empty set of runs).
		return strictScalarRow{vals: []any{0}}
	}
	return strictErrRow{err: errStrictUnexpectedSQL}
}

func (f *strictFakeCadenceStore) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	// list select rows — an empty set (the list test only checks the success code).
	return &strictEmptyRows{}, nil
}

func (f *strictFakeCadenceStore) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errStrictUnexpectedSQL
}

// BeginTx — the Create tx (ADR-052 §m): INSERT INTO cadences goes through a tx
// wrapper that routes back to the store.
func (f *strictFakeCadenceStore) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return &strictCadenceTx{store: f}, nil
}

type strictCadenceTx struct{ store *strictFakeCadenceStore }

func (t *strictCadenceTx) Begin(context.Context) (pgx.Tx, error)                    { return t, nil }
func (t *strictCadenceTx) BeginFunc(_ context.Context, fn func(pgx.Tx) error) error { return fn(t) }
func (t *strictCadenceTx) Commit(context.Context) error                             { return nil }
func (t *strictCadenceTx) Rollback(context.Context) error                           { return nil }
func (t *strictCadenceTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errStrictUnexpectedSQL
}
func (t *strictCadenceTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("unexpected")
}
func (t *strictCadenceTx) LargeObjects() pgx.LargeObjects { panic("unexpected") }
func (t *strictCadenceTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected")
}
func (t *strictCadenceTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.store.Exec(ctx, sql, args...)
}
func (t *strictCadenceTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.store.Query(ctx, sql, args...)
}
func (t *strictCadenceTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.store.QueryRow(ctx, sql, args...)
}
func (t *strictCadenceTx) Conn() *pgx.Conn { return nil }

var errStrictUnexpectedSQL = errStrict("strictFakeCadenceStore: unexpected SQL")

type errStrict string

func (e errStrict) Error() string { return string(e) }

// strictCadenceBody — a valid cadence.create body (happy-path huma guard).
const strictCadenceBody = `{"name":"hourly","schedule_kind":"cron","cron_expr":"0 * * * *","overlap_policy":"queue","kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`

// --- shared pgx.Row / pgx.Rows stubs ---

type strictScalarRow struct{ vals []any }

func (r strictScalarRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch p := d.(type) {
		case *time.Time:
			*p = r.vals[i].(time.Time)
		case *int:
			*p = r.vals[i].(int)
		}
	}
	return nil
}

type strictErrRow struct{ err error }

func (r strictErrRow) Scan(...any) error { return r.err }

// strictEmptyRows — an empty pgx.Rows (list with no rows).
type strictEmptyRows struct{}

func (*strictEmptyRows) Close()                        {}
func (*strictEmptyRows) Err() error                    { return nil }
func (*strictEmptyRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (*strictEmptyRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (*strictEmptyRows) Next() bool             { return false }
func (*strictEmptyRows) Scan(...any) error      { return nil }
func (*strictEmptyRows) Values() ([]any, error) { return nil, nil }
func (*strictEmptyRows) RawValues() [][]byte    { return nil }
func (*strictEmptyRows) Conn() *pgx.Conn        { return nil }

// strictAllowAll — an allow-all PermissionChecker (RBAC is not the subject of
// the guards where it must let the request through to the handler).
type strictAllowAll struct{}

func (strictAllowAll) Check(string, string, string, map[string]string) error { return nil }

// --- assignScan: a narrow assignment for scanHerald/scanTiding types ---

// assignScan covers the types that scanHerald/scanTiding read
// (time.Time / string / *string / bool / []byte / []string).
func assignScan(dest, val any) {
	switch d := dest.(type) {
	case *time.Time:
		if v, ok := val.(time.Time); ok {
			*d = v
		}
	case *string:
		if v, ok := val.(string); ok {
			*d = v
		}
	case **string:
		if v, ok := val.(*string); ok {
			*d = v
		}
	case *bool:
		if v, ok := val.(bool); ok {
			*d = v
		}
	case *[]byte:
		if v, ok := val.([]byte); ok {
			*d = v
		}
	case *[]string:
		if v, ok := val.([]string); ok {
			*d = v
		}
	}
}

// --- audit.Writer capturer and the role.create stub pool ---

// auditCaptureWriter — an audit.Writer stub: captures the events written (no
// mutex — the guard run is single-threaded).
type auditCaptureWriter struct {
	events []*audit.Event
}

func (c *auditCaptureWriter) Write(_ context.Context, ev *audit.Event) error {
	cp := *ev
	c.events = append(c.events, &cp)
	return nil
}

func (c *auditCaptureWriter) Events() []*audit.Event { return c.events }

// auditRolePool — a stub-PG for the role.create success path: CreateRole with an
// EMPTY set of permissions does one tx.Exec(insertRoleSQL) → success → Commit,
// without reading the DB (the subset check doesn't read anything when required
// is empty).
type auditRolePool struct{}

func (auditRolePool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (auditRolePool) QueryRow(context.Context, string, ...any) pgx.Row {
	return auditErrRow{err: pgx.ErrNoRows}
}
func (auditRolePool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errStrictUnexpectedSQL
}
func (auditRolePool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return auditOKTx{}, nil
}

type auditErrRow struct{ err error }

func (r auditErrRow) Scan(...any) error { return r.err }

// auditOKTx — a pgx.Tx with a success Exec/Commit/Rollback (embedding a nil
// pgx.Tx supplies the remaining methods — not called on the role.create
// success path).
type auditOKTx struct{ pgx.Tx }

func (auditOKTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (auditOKTx) Commit(context.Context) error   { return nil }
func (auditOKTx) Rollback(context.Context) error { return nil }

// q400ListPool — a fake pool for the list domain: COUNT → 0, SELECT → empty set.
// Implements handlers.OperatorPool (ExecQueryRower+BeginTx), auditpg-queryRower,
// and errand.ExecQueryRower simultaneously (structurally). BeginTx is never
// reached on the list paths this covers.
type q400ListPool struct{}

func (q400ListPool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "COUNT(*)") {
		return strictScalarRow{vals: []any{0}}
	}
	return strictErrRow{err: pgx.ErrNoRows}
}
func (q400ListPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &strictEmptyRows{}, nil
}
func (q400ListPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (q400ListPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errStrictUnexpectedSQL
}
