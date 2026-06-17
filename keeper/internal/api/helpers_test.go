package api

// Общие тест-фикстуры пакета api_test: fake-pool/row/rows-стабы, allow-all
// PermissionChecker, audit.Writer-захватчик. Используются huma-guard-ами всех
// доменов (валидация контракта 400/422, wire-форма, audit-on-write). Раньше жили
// в strict-тест-файлах; после сноса strict-агрегата (teardown T2) вынесены сюда
// как нейтральная инфраструктура — strict-обвязки (bridge/wrapper/server) НЕ несут.

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- минимальный fake CadenceStore (cadence huma-guard-ы) ---

// strictFakeCadenceStore обслуживает INSERT (success → timestamps) и
// SELECT COUNT (list). Cadence huma-guard-ам дополнительно нужны selectByID (Get
// для PATCH/Runs), UPDATE...RETURNING (PatchTyped), UPDATE...enabled
// (SetEnabledTyped) и DELETE (DeleteTyped) — опциональные поля, по умолчанию
// not-found (404). Остальной SQL — ошибка.
type strictFakeCadenceStore struct {
	insertCalls int

	// selectByID — настраиваемая строка cadence для Get (PATCH/Runs); nil → 404.
	selectByID func(id string) pgx.Row
	// флаги not-found для write-операций cadence.
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
		// GET /v1/cadences/{id}/runs — voyage.List COUNT (пустой набор прогонов).
		return strictScalarRow{vals: []any{0}}
	}
	return strictErrRow{err: errStrictUnexpectedSQL}
}

func (f *strictFakeCadenceStore) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	// list select rows — пустой набор (тест list проверяет лишь успешный код).
	return &strictEmptyRows{}, nil
}

func (f *strictFakeCadenceStore) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errStrictUnexpectedSQL
}

// BeginTx — Create-tx (ADR-052 §m): INSERT INTO cadences идёт через tx-обёртку,
// маршрутизирующую обратно в store.
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

// strictCadenceBody — валидное тело cadence.create (happy-path huma-guard).
const strictCadenceBody = `{"name":"hourly","schedule_kind":"cron","cron_expr":"0 * * * *","overlap_policy":"queue","kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`

// --- общие pgx.Row / pgx.Rows стабы ---

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

// strictEmptyRows — пустой pgx.Rows (list без строк).
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

// strictAllowAll — allow-all PermissionChecker (RBAC не предмет guard-ов, где он
// должен пропустить запрос к handler-у).
type strictAllowAll struct{}

func (strictAllowAll) Check(string, string, string, map[string]string) error { return nil }

// --- assignScan: узкое присваивание для scanHerald/scanTiding-типов ---

// assignScan покрывает типы, которые scanHerald/scanTiding читают
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

// --- audit.Writer-захватчик и role.create stub-pool ---

// auditCaptureWriter — audit.Writer-stub: захватывает записанные события (без
// mutex — guard-прогон однопоточный).
type auditCaptureWriter struct {
	events []*audit.Event
}

func (c *auditCaptureWriter) Write(_ context.Context, ev *audit.Event) error {
	cp := *ev
	c.events = append(c.events, &cp)
	return nil
}

func (c *auditCaptureWriter) Events() []*audit.Event { return c.events }

// auditRolePool — stub-PG для role.create success-path: CreateRole с ПУСТЫМ
// набором permissions делает один tx.Exec(insertRoleSQL) → success → Commit, без
// чтения БД (subset-check при пустом required не читает).
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

// auditOKTx — pgx.Tx с success Exec/Commit/Rollback (встраивание nil-pgx.Tx даёт
// остальные методы — на role.create success-пути не вызываются).
type auditOKTx struct{ pgx.Tx }

func (auditOKTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (auditOKTx) Commit(context.Context) error   { return nil }
func (auditOKTx) Rollback(context.Context) error { return nil }

// q400ListPool — fake pool для list-домена: COUNT → 0, SELECT → пустой набор.
// Реализует handlers.OperatorPool (ExecQueryRower+BeginTx), auditpg-queryRower и
// errand.ExecQueryRower одновременно (структурно). На покрываемых list-путях
// BeginTx не достигается.
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
