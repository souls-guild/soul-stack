package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// --- fakes ---

type fakePool struct {
	insertErr   error
	insertCalls int
	insertOp    *operator.Operator

	selectFn func(aid string) (*operator.Operator, error)
	revokeFn func(aid, reason string) error
	activeFn func() ([]string, error)

	// incFn — резолв incarnation по имени (SelectByName + existence-probe +
	// FOR UPDATE-select). nil → QueryRow по `FROM incarnation` отдаёт
	// pgx.ErrNoRows (→ not-found). FOR UPDATE-вариант (unlock/upgrade) читает
	// state/status/version из того же inc.
	incFn func(name string) (*incarnation.Incarnation, error)

	// incListFn — backing для keeper.incarnation.list (SelectAll). Возвращает
	// страницу + total. nil → пустой список, total=0.
	incListFn func(filter incarnation.ListFilter) ([]*incarnation.Incarnation, int)

	// historyFn — backing для keeper.incarnation.history (HistorySelectByName).
	// nil → пустой список, total=0.
	historyFn func(name string, filter incarnation.HistoryFilter) ([]*incarnation.HistoryEntry, int)

	// incInsertFn — backing для keeper.incarnation.create (Create insertSQL).
	// nil → INSERT успешен (created_at/updated_at = zero). Возврат ошибки —
	// эмуляция UNIQUE / FK / прочих сбоев.
	incInsertFn func(name, service string) error

	// insertIncArgs — захваченные args INSERT INTO incarnation (создаётся
	// create-tool-ом): [0]=name … [4]=spec(jsonb []byte) … [10]=traits(jsonb).
	// Позволяет тесту проверить прокидку spec.traits на create-пути (ADR-060
	// amend R1, паритет REST insertArgs).
	insertIncArgs []any

	// lastScenarioFn — backing для rerun-create probe `SELECT scenario FROM
	// state_history … ORDER BY history_id DESC LIMIT 1` (UnlockForRerun scope-
	// check). nil → отдаём имя создавшего сценария из incFn-строки (дефолт
	// `create`), что моделирует «последний упавший сценарий = create».
	lastScenarioFn func(name string) (string, error)

	// soulBulkCountFn — backing для souls-bulk-проекции traits (SyncTraitsToHosts
	// → CountBulkMatched: `SELECT COUNT(*) FROM souls …`). nil → 0 (0 хостов-членов,
	// sync-hook no-op — как обычно на create до онбординга). Ненулевое → столько
	// «членов», и тест может проверить, что проекция была вызвана.
	soulBulkCountFn func() int

	// deleteTag — RowsAffected для single-winner `DELETE FROM incarnation`
	// (destroy force-путь, DeleteAfterTeardown). zero-value → "DELETE 1".
	deleteTag pgconn.CommandTag

	beginErr error
}

func (f *fakePool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if contains(sql, "INSERT INTO operators") {
		f.insertCalls++
		op := &operator.Operator{
			AID:         args[0].(string),
			DisplayName: args[1].(string),
		}
		if args[4] != nil {
			s := args[4].(string)
			op.CreatedByAID = &s
		}
		f.insertOp = op
		return pgconn.NewCommandTag("INSERT 0 1"), f.insertErr
	}
	if contains(sql, "UPDATE operators") {
		if f.revokeFn != nil {
			aid := args[0].(string)
			reason := ""
			if len(args) > 1 {
				reason = args[1].(string)
			}
			if err := f.revokeFn(aid, reason); err != nil {
				return pgconn.NewCommandTag("UPDATE 0"), err
			}
			return pgconn.NewCommandTag("UPDATE 1"), nil
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	// destroy force-DELETE (DeleteAfterTeardown): archive-INSERT-ы + single-
	// winner DELETE FROM incarnation. deleteTag задаёт RowsAffected DELETE-а
	// (zero-value → "DELETE 1" = строка снесена; "DELETE 0" → no-op).
	if contains(sql, "DELETE FROM incarnation") {
		if f.deleteTag.String() == "" {
			return pgconn.NewCommandTag("DELETE 1"), nil
		}
		return f.deleteTag, nil
	}
	if contains(sql, "INSERT INTO incarnation_archive") || contains(sql, "INSERT INTO state_history_archive") {
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	// incarnation-mutations (unlock state_history-insert / status-update,
	// upgrade tx-write-ы, destroy state_history-insert). Эмулируем как успешный
	// no-op — фейлы инжектятся через incFn / beginErr, не через Exec.
	if contains(sql, "INSERT INTO state_history") || contains(sql, "UPDATE incarnation") {
		return pgconn.NewCommandTag("OK 1"), nil
	}
	return pgconn.CommandTag{}, errFakeUnexpected{sql: sql}
}

func (f *fakePool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	// INSERT INTO incarnation … RETURNING created_at, updated_at (Create).
	if contains(sql, "INSERT INTO incarnation") {
		f.insertIncArgs = args
		name := args[0].(string)
		service := args[1].(string)
		if f.incInsertFn != nil {
			if err := f.incInsertFn(name, service); err != nil {
				return errRow{err: err}
			}
		}
		return staticRow{values: []any{time.Time{}, time.Time{}}}
	}
	// SELECT COUNT(*) FROM souls … (SyncTraitsToHosts → CountBulkMatched). Без
	// этой ветки souls-bulk-count упал бы в errFakeUnexpected и sync-hook (best-
	// effort) лишь логировал бы warning; ветка делает проекцию наблюдаемой.
	if contains(sql, "COUNT(*) FROM souls") {
		n := 0
		if f.soulBulkCountFn != nil {
			n = f.soulBulkCountFn()
		}
		return countRow{n: n}
	}
	// COUNT(*) FROM incarnation (list total) — без name-arg.
	if contains(sql, "COUNT(*) FROM incarnation") {
		_, total := f.listItems(incarnation.ListFilter{})
		return countRow{n: total}
	}
	// COUNT(*) FROM state_history (history total).
	if contains(sql, "COUNT(*) FROM state_history") {
		name := args[0].(string)
		_, total := f.historyItems(name, incarnation.HistoryFilter{})
		return countRow{n: total}
	}
	// UPDATE incarnation … RETURNING updated_at (UpdateHosts/UpdateTraits day-2
	// мутации): отдаём свежий updated_at на Scan(*time.Time). Идёт ДО общего
	// `FROM incarnation`-матча (тот предикат стоит и в WHERE этого UPDATE).
	if contains(sql, "UPDATE incarnation") && contains(sql, "RETURNING updated_at") {
		return staticRow{values: []any{time.Now().UTC()}}
	}
	// FOR UPDATE-select из incarnation. ПОЛНАЯ строка (UpdateTraits:
	// `covens, traits` в проекции → scanIncarnation) → newIncRow. rerun-create
	// (state, status, created_scenario) → rerunForUpdateRow. Частичная
	// (unlock: state,status / upgrade: state,state_schema_version,status) →
	// forUpdateIncRow. args[0] = name.
	if contains(sql, "FROM incarnation") && contains(sql, "FOR UPDATE") {
		if f.incFn == nil {
			return errRow{err: pgx.ErrNoRows}
		}
		inc, err := f.incFn(args[0].(string))
		if err != nil {
			return errRow{err: err}
		}
		if contains(sql, "covens, traits") {
			return newIncRow(inc)
		}
		if contains(sql, "created_scenario") {
			return rerunForUpdateRow{inc: inc}
		}
		return forUpdateIncRow{inc: inc, withVersion: contains(sql, "state_schema_version")}
	}
	// rerun-create scope-probe: SELECT scenario FROM state_history … LIMIT 1.
	// Идёт ДО общего `FROM state_history`/COUNT (тот требует COUNT-токен).
	if contains(sql, "SELECT scenario") && contains(sql, "FROM state_history") {
		name := args[0].(string)
		if f.lastScenarioFn != nil {
			s, err := f.lastScenarioFn(name)
			if err != nil {
				return errRow{err: err}
			}
			return staticRow{values: []any{s}}
		}
		// Дефолт: «последний упавший сценарий = создавший». created_scenario
		// строки берём из incFn (если задан), иначе канонический `create`.
		last := "create"
		if f.incFn != nil {
			if inc, err := f.incFn(name); err == nil && inc.CreatedScenario != "" {
				last = inc.CreatedScenario
			}
		}
		return staticRow{values: []any{last}}
	}
	// SelectByName / existence-probe (полная строка incarnation).
	if contains(sql, "FROM incarnation") {
		if f.incFn == nil {
			return errRow{err: pgx.ErrNoRows}
		}
		inc, err := f.incFn(args[0].(string))
		if err != nil {
			return errRow{err: err}
		}
		return newIncRow(inc)
	}
	if contains(sql, "SELECT aid, display_name") {
		if f.selectFn != nil {
			op, err := f.selectFn(args[0].(string))
			if err != nil {
				return errRow{err: err}
			}
			var createdByPtr any
			var revokedPtr any
			if op.CreatedByAID != nil {
				createdByPtr = *op.CreatedByAID
			}
			if op.RevokedAt != nil {
				revokedPtr = *op.RevokedAt
			}
			createdVia := op.CreatedVia
			if createdVia == "" {
				createdVia = operator.CreatedViaUser
			}
			return staticRow{values: []any{
				op.AID, op.DisplayName, string(op.AuthMethod), op.CreatedAt,
				createdByPtr, createdVia, revokedPtr, []byte("{}"),
			}}
		}
		return errRow{err: pgx.ErrNoRows}
	}
	return errRow{err: errFakeUnexpected{sql: sql}}
}

func (f *fakePool) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	// list items: SELECT name, service, … FROM incarnation … OFFSET/LIMIT.
	if contains(sql, "FROM incarnation") {
		items, _ := f.listItems(incarnation.ListFilter{})
		rows := make([]incRow, 0, len(items))
		for _, inc := range items {
			rows = append(rows, newIncRow(inc))
		}
		return &incRows{rows: rows}, nil
	}
	// history items: SELECT history_id, scenario, … FROM state_history.
	if contains(sql, "FROM state_history") {
		name := args[0].(string)
		items, _ := f.historyItems(name, incarnation.HistoryFilter{})
		rows := make([]historyRow, 0, len(items))
		for _, e := range items {
			rows = append(rows, newHistoryRow(e))
		}
		return &historyRows{rows: rows}, nil
	}
	// Synod-ветка self-lockout-ядра (ADR-049(f), эпик Synod S2):
	// LockEffectiveClusterAdmins шлёт второй locking-запрос по synod_operators.
	// mcp-сценарии групповых админов не моделируют — пусто; их покрывают rbac
	// integration-guard-тесты. Проверяется ПЕРЕД прямой веткой.
	if contains(sql, "FROM synod_operators") {
		return &stringRows{}, nil
	}
	// Slice 3: operator.revoke lockout-probe идёт через
	// rbac.LockEffectiveClusterAdmins — SELECT ro.aid FROM rbac_role_operators
	// JOIN … FOR UPDATE OF ro,rp,o. activeFn возвращает уже-эффективный набор
	// активных `*`-admin-ов из БД (admin-set целиком из БД, без пересечения
	// с in-memory снимком).
	if contains(sql, "FROM rbac_role_operators") {
		var admins []string
		if f.activeFn != nil {
			a, err := f.activeFn()
			if err != nil {
				return nil, err
			}
			admins = a
		}
		return &stringRows{values: admins}, nil
	}
	return nil, errFakeUnexpected{sql: sql}
}

func (f *fakePool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return &fakeTx{pool: f}, nil
}

type fakeTx struct{ pool *fakePool }

func (t *fakeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return t.pool.BeginTx(ctx, pgx.TxOptions{})
}
func (t *fakeTx) BeginFunc(_ context.Context, fn func(pgx.Tx) error) error { return fn(t) }
func (t *fakeTx) Commit(_ context.Context) error                           { return nil }
func (t *fakeTx) Rollback(_ context.Context) error                         { return nil }
func (t *fakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	panic("fakeTx.CopyFrom: unexpected")
}
func (t *fakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("fakeTx.SendBatch: unexpected")
}
func (t *fakeTx) LargeObjects() pgx.LargeObjects { panic("fakeTx.LargeObjects: unexpected") }
func (t *fakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	panic("fakeTx.Prepare: unexpected")
}
func (t *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.pool.Exec(ctx, sql, args...)
}
func (t *fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.pool.Query(ctx, sql, args...)
}
func (t *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.pool.QueryRow(ctx, sql, args...)
}
func (t *fakeTx) Conn() *pgx.Conn { return nil }

type errFakeUnexpected struct{ sql string }

func (e errFakeUnexpected) Error() string { return "fake: unexpected SQL: " + e.sql }

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

// listItems / historyItems — общие backing-аксессоры, чтобы COUNT- и
// list-варианты SQL отдавали согласованные total/items из одного источника.
func (f *fakePool) listItems(filter incarnation.ListFilter) ([]*incarnation.Incarnation, int) {
	if f.incListFn == nil {
		return nil, 0
	}
	return f.incListFn(filter)
}

func (f *fakePool) historyItems(name string, filter incarnation.HistoryFilter) ([]*incarnation.HistoryEntry, int) {
	if f.historyFn == nil {
		return nil, 0
	}
	return f.historyFn(name, filter)
}

// countRow — pgx.Row для SELECT COUNT(*) (Scan в *int).
type countRow struct{ n int }

func (r countRow) Scan(dest ...any) error {
	if p, ok := dest[0].(*int); ok {
		*p = r.n
		return nil
	}
	return fmt.Errorf("countRow.Scan: unexpected dest type %T", dest[0])
}

// forUpdateIncRow — pgx.Row для FOR UPDATE-select-ов incarnation.
// unlock: Scan(state []byte, status string). upgrade: Scan(state []byte,
// state_schema_version int, status string) — withVersion=true.
type forUpdateIncRow struct {
	inc         *incarnation.Incarnation
	withVersion bool
}

func (r forUpdateIncRow) Scan(dest ...any) error {
	mustJSON := func(m map[string]any) []byte {
		if m == nil {
			return []byte("null")
		}
		b, _ := json.Marshal(m)
		return b
	}
	if r.withVersion {
		if len(dest) != 3 {
			return fmt.Errorf("forUpdateIncRow.Scan: want 3 dest, got %d", len(dest))
		}
		*dest[0].(*[]byte) = mustJSON(r.inc.State)
		*dest[1].(*int) = r.inc.StateSchemaVersion
		*dest[2].(*string) = string(r.inc.Status)
		return nil
	}
	if len(dest) != 2 {
		return fmt.Errorf("forUpdateIncRow.Scan: want 2 dest, got %d", len(dest))
	}
	*dest[0].(*[]byte) = mustJSON(r.inc.State)
	*dest[1].(*string) = string(r.inc.Status)
	return nil
}

// rerunForUpdateRow — pgx.Row для UnlockForRerun FOR UPDATE-select-а
// `SELECT state, status, created_scenario`. Scan(state []byte, status string,
// created_scenario string).
type rerunForUpdateRow struct{ inc *incarnation.Incarnation }

func (r rerunForUpdateRow) Scan(dest ...any) error {
	if len(dest) != 3 {
		return fmt.Errorf("rerunForUpdateRow.Scan: want 3 dest, got %d", len(dest))
	}
	state := []byte("null")
	if r.inc.State != nil {
		b, _ := json.Marshal(r.inc.State)
		state = b
	}
	*dest[0].(*[]byte) = state
	*dest[1].(*string) = string(r.inc.Status)
	*dest[2].(*string) = r.inc.CreatedScenario
	return nil
}

// incRows — pgx.Rows над срезом incRow (list items).
type incRows struct {
	rows []incRow
	idx  int
}

func (r *incRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}
func (r *incRows) Scan(dest ...any) error                       { return r.rows[r.idx-1].Scan(dest...) }
func (r *incRows) Err() error                                   { return nil }
func (r *incRows) Close()                                       {}
func (r *incRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *incRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *incRows) Values() ([]any, error)                       { return nil, nil }
func (r *incRows) RawValues() [][]byte                          { return nil }
func (r *incRows) Conn() *pgx.Conn                              { return nil }

// historyRow — pgx.Row для одной state_history-записи (scanHistoryEntry читает
// 7 колонок: history_id, scenario, state_before, state_after, changed_by_aid,
// apply_id, at).
type historyRow struct{ vals []any }

func newHistoryRow(e *incarnation.HistoryEntry) historyRow {
	mustJSON := func(m map[string]any) []byte {
		if m == nil {
			return []byte("null")
		}
		b, _ := json.Marshal(m)
		return b
	}
	return historyRow{vals: []any{
		e.HistoryID,
		e.Scenario,
		mustJSON(e.StateBefore),
		mustJSON(e.StateAfter),
		e.ChangedByAID,
		e.ApplyID,
		e.At,
	}}
}

func (r historyRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = r.vals[i].(string)
		case *[]byte:
			*d = r.vals[i].([]byte)
		case **string:
			*d = r.vals[i].(*string)
		case *time.Time:
			*d = r.vals[i].(time.Time)
		default:
			return fmt.Errorf("historyRow.Scan: unexpected dest type %T at %d", d, i)
		}
	}
	return nil
}

// historyRows — pgx.Rows над срезом historyRow.
type historyRows struct {
	rows []historyRow
	idx  int
}

func (r *historyRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}
func (r *historyRows) Scan(dest ...any) error                       { return r.rows[r.idx-1].Scan(dest...) }
func (r *historyRows) Err() error                                   { return nil }
func (r *historyRows) Close()                                       {}
func (r *historyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *historyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *historyRows) Values() ([]any, error)                       { return nil, nil }
func (r *historyRows) RawValues() [][]byte                          { return nil }
func (r *historyRows) Conn() *pgx.Conn                              { return nil }

type staticRow struct {
	values []any
}

func (r staticRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = r.values[i].(string)
		case *time.Time:
			*d = r.values[i].(time.Time)
		case **string:
			if r.values[i] == nil {
				*d = nil
			} else {
				s := r.values[i].(string)
				*d = &s
			}
		case **time.Time:
			if r.values[i] == nil {
				*d = nil
			} else {
				t := r.values[i].(time.Time)
				*d = &t
			}
		case *[]byte:
			*d = r.values[i].([]byte)
		}
	}
	return nil
}

type stringRows struct {
	values []string
	idx    int
}

func (r *stringRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}
func (r *stringRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.values[r.idx-1]
	return nil
}
func (r *stringRows) Err() error                                   { return nil }
func (r *stringRows) Close()                                       {}
func (r *stringRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *stringRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *stringRows) Values() ([]any, error)                       { return nil, nil }
func (r *stringRows) RawValues() [][]byte                          { return nil }
func (r *stringRows) Conn() *pgx.Conn                              { return nil }

type fakeIssuer struct{ called bool }

func (f *fakeIssuer) Issue(aid string, _ []string, _ time.Duration, _ bool) (string, error) {
	f.called = true
	return "fake-jwt-" + aid, nil
}

type recordingAudit struct {
	events []*audit.Event
}

func (r *recordingAudit) Write(_ context.Context, ev *audit.Event) error {
	r.events = append(r.events, ev)
	return nil
}

// contains — substring без strings (избегаем зависимости в тестовом fake-е).
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	if len(needle) == 0 {
		return 0
	}
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

// --- helpers ---

func newTestHandler(t *testing.T, pool *fakePool, rbacCfg *rbactest.Config) (*Handler, *fakeIssuer, *recordingAudit) {
	t.Helper()
	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	iss := &fakeIssuer{}
	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       pool,
		Issuer:     iss,
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	rec := &recordingAudit{}
	h, err := NewHandler(HandlerDeps{
		OperatorSvc:   svc,
		RBAC:          enf,
		AuditWriter:   rec,
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		IncarnationDB: pool,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, iss, rec
}

// --- incarnation-deps fakes (create/run/upgrade) ---

// mcpStarter — мок [handlers.ScenarioStarter]. Зеркалит REST fakeStarter:
// фиксирует spec + число вызовов, инжектит ошибку.
type mcpStarter struct {
	gotSpec scenario.RunSpec
	calls   int
	err     error
}

func (f *mcpStarter) Start(_ context.Context, spec scenario.RunSpec) error {
	f.calls++
	f.gotSpec = spec
	return f.err
}

// mcpResolver — мок [handlers.ServiceResolver]. ok=false → not-registered.
type mcpResolver struct{ ok bool }

func (f *mcpResolver) Resolve(service string) (artifact.ServiceRef, bool) {
	return artifact.ServiceRef{Name: service, Ref: "v1"}, f.ok
}

// mcpLoader — мок [handlers.ServiceSnapshotLoader]. Зеркалит REST fakeLoader.
type mcpLoader struct {
	targetSchema int
	loadErr      error
	chain        statemigrate.Chain
	chainErr     error

	// destroy pre-check (ReadFile): hasDestroyScenario=true → scenario `destroy`
	// «есть» в снапшоте; false → os.ErrNotExist. readErr перекрывает (I/O-сбой).
	hasDestroyScenario bool
	readErr            error

	// scenarioYAML — для sync input-валидации (scenario.ValidateInput):
	// непустое → ReadFile отдаёт этот YAML как scenario/<name>/main.yml.
	scenarioYAML string
}

func (f *mcpLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return &artifact.ServiceArtifact{
		Ref:      ref,
		Manifest: &config.ServiceManifest{StateSchemaVersion: f.targetSchema},
	}, nil
}

func (f *mcpLoader) LoadMigrationChain(_ *artifact.ServiceArtifact, _, _ int) (statemigrate.Chain, error) {
	if f.chainErr != nil {
		return nil, f.chainErr
	}
	return f.chain, nil
}

// ReadFile — для destroy PrepareDestroy pre-check (наличие scenario `destroy`).
func (f *mcpLoader) ReadFile(_ *artifact.ServiceArtifact, _ string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.scenarioYAML != "" {
		return []byte(f.scenarioYAML), nil
	}
	if f.hasDestroyScenario {
		return []byte("tasks: []\n"), nil
	}
	return nil, os.ErrNotExist
}

// newTestHandlerFull — как newTestHandler, но прокидывает incarnation-deps
// (runner / registry / loader) для create/run/upgrade-tools. nil-deps →
// tool отвечает not-configured (паритет REST 500).
func newTestHandlerFull(t *testing.T, pool *fakePool, rbacCfg *rbactest.Config, runner handlers.ScenarioStarter, registry handlers.ServiceResolver, loader handlers.ServiceSnapshotLoader) (*Handler, *recordingAudit) {
	t.Helper()
	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       pool,
		Issuer:     &fakeIssuer{},
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	rec := &recordingAudit{}
	h, err := NewHandler(HandlerDeps{
		OperatorSvc:     svc,
		RBAC:            enf,
		AuditWriter:     rec,
		Logger:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
		IncarnationDB:   pool,
		ScenarioRunner:  runner,
		ServiceRegistry: registry,
		ServiceLoader:   loader,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

// callTool — общий прогон любого incarnation-tool через реальный Dispatch.
func callTool(t *testing.T, h *Handler, aid, tool, argsJSON string) jsonRPCResponse {
	t.Helper()
	params, _ := json.Marshal(toolsCallParams{
		Name:      tool,
		Arguments: json.RawMessage(argsJSON),
	})
	req := jsonRPCRequest{JSONRPC: "2.0", ID: mustRawID(700), Method: "tools/call", Params: params}
	resp, isNot := h.Dispatch(context.Background(), claims(aid), req)
	if isNot {
		t.Fatal("tools/call must not be a notification")
	}
	return resp
}

func claims(aid string) *keeperjwt.Claims {
	return &keeperjwt.Claims{Subject: aid}
}

func mustRawID(id int) json.RawMessage {
	b, _ := json.Marshal(id)
	return b
}

// --- tests ---

func TestNewHandler_RequiresIncarnationDB(t *testing.T) {
	enf, err := rbactest.NewEnforcer(nil)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       &fakePool{},
		Issuer:     &fakeIssuer{},
		RBAC:       enf,
		TTLDefault: time.Hour,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	base := HandlerDeps{
		OperatorSvc: svc,
		RBAC:        enf,
		AuditWriter: &recordingAudit{},
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	// Без IncarnationDB → ошибка.
	if _, err := NewHandler(base); err == nil {
		t.Fatal("NewHandler must reject nil IncarnationDB")
	}
	// С IncarnationDB → ok.
	base.IncarnationDB = &fakePool{}
	if _, err := NewHandler(base); err != nil {
		t.Fatalf("NewHandler with IncarnationDB: %v", err)
	}
}

func TestDispatch_Initialize(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, nil)
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      mustRawID(1),
		Method:  "initialize",
	}
	resp, isNot := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if isNot {
		t.Fatal("initialize should not be a notification")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res initializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.ServerInfo.Name != serverInfoName {
		t.Errorf("ServerInfo.Name = %q", res.ServerInfo.Name)
	}
	if res.ProtocolVersion != mcpProtocolVersion {
		t.Errorf("ProtocolVersion = %q", res.ProtocolVersion)
	}
}

func TestDispatch_ToolsList_HasAllTools(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, nil)
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      mustRawID(2),
		Method:  "tools/list",
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsListResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(res.Tools) != 90 {
		t.Errorf("tool count = %d, want 90", len(res.Tools))
	}
	// Имена должны быть стабильны (spec — mcp-tools.md).
	names := map[string]bool{}
	for _, t := range res.Tools {
		names[t.Name] = true
	}
	must := []string{
		"keeper.operator.create", "keeper.operator.revoke", "keeper.operator.issue-token",
		"keeper.role.create", "keeper.role.delete", "keeper.role.list",
		"keeper.role.update", "keeper.role.grant-operator", "keeper.role.revoke-operator",
		"keeper.incarnation.create", "keeper.incarnation.run", "keeper.incarnation.get",
		"keeper.incarnation.list", "keeper.incarnation.history", "keeper.incarnation.unlock",
		"keeper.incarnation.upgrade", "keeper.incarnation.destroy", "keeper.incarnation.check-drift",
		"keeper.soul.create", "keeper.soul.issue-token", "keeper.soul.coven-assign", "keeper.soul.list",
		"keeper.plugin.allow", "keeper.plugin.revoke", "keeper.plugin.list",
		"keeper.sigil.key.introduce", "keeper.sigil.key.list", "keeper.sigil.key.set-primary", "keeper.sigil.key.retire",
		"keeper.service.register", "keeper.service.update", "keeper.service.list", "keeper.service.deregister",
		"keeper.augur.omen.create", "keeper.augur.omen.list", "keeper.augur.omen.delete",
		"keeper.augur.rite.create", "keeper.augur.rite.list", "keeper.augur.rite.delete",
		"keeper.soul.errand.run", "keeper.errand.list", "keeper.errand.get",
		"keeper.push.apply", "keeper.push.cleanup",
		"keeper.push-provider.create", "keeper.push-provider.update", "keeper.push-provider.delete",
		"keeper.push-provider.list", "keeper.push-provider.read",
		"keeper.provider.create", "keeper.provider.read", "keeper.provider.list", "keeper.provider.delete",
		"keeper.profile.create", "keeper.profile.read", "keeper.profile.list", "keeper.profile.delete",
	}
	for _, m := range must {
		if !names[m] {
			t.Errorf("missing tool: %s", m)
		}
	}
}

func TestDispatch_UnknownMethod(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, nil)
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      mustRawID(3),
		Method:  "no/such/method",
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error == nil || resp.Error.Code != rpcCodeMethodNotFound {
		t.Errorf("Error = %+v, want -32601", resp.Error)
	}
}

func TestDispatch_NotificationsAreIgnored(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, nil)
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}
	_, isNot := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if !isNot {
		t.Errorf("notifications/initialized must be treated as notification")
	}
}

func TestToolsCall_OperatorCreate_Success(t *testing.T) {
	now := time.Now().UTC()
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{
				AID:         aid,
				DisplayName: "Bob",
				AuthMethod:  operator.AuthMethodJWT,
				CreatedAt:   now,
			}, nil
		},
	}
	h, iss, rec := newTestHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"operator.create"}},
		},
	})

	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.operator.create",
		Arguments: json.RawMessage(`{"aid":"archon-bob","display_name":"Bob"}`),
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      mustRawID(10),
		Method:  "tools/call",
		Params:  params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(res.StructuredContent) == 0 {
		t.Fatal("structuredContent is empty")
	}
	var out operatorCreateOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if out.AID != "archon-bob" {
		t.Errorf("AID = %q", out.AID)
	}
	if out.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %q", out.CreatedByAID)
	}
	if out.JWT == "" {
		t.Errorf("JWT empty")
	}
	if !iss.called {
		t.Errorf("issuer not called")
	}
	if pool.insertCalls != 1 {
		t.Errorf("insertCalls = %d", pool.insertCalls)
	}
	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d", len(rec.events))
	}
	if rec.events[0].EventType != "operator.created" {
		t.Errorf("EventType = %q", rec.events[0].EventType)
	}
	if rec.events[0].ArchonAID != "archon-alice" {
		t.Errorf("ArchonAID = %q", rec.events[0].ArchonAID)
	}
	// ADR-022(b): MCP-handler пишет audit-event с Source=mcp (не api),
	// иначе теряется granular trail.
	if rec.events[0].Source != audit.SourceMCP {
		t.Errorf("Source = %q, want %q", rec.events[0].Source, audit.SourceMCP)
	}
}

func TestToolsCall_OperatorCreate_RBACForbidden(t *testing.T) {
	pool := &fakePool{}
	h, _, _ := newTestHandler(t, pool, nil) // empty RBAC → deny

	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.operator.create",
		Arguments: json.RawMessage(`{"aid":"archon-bob","display_name":"Bob"}`),
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(11), Method: "tools/call", Params: params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
	if pool.insertCalls != 0 {
		t.Errorf("insertCalls = %d (RBAC must reject before service)", pool.insertCalls)
	}
}

func TestToolsCall_OperatorCreate_InvalidAID(t *testing.T) {
	pool := &fakePool{}
	h, _, _ := newTestHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"operator.create"}},
		},
	})
	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.operator.create",
		Arguments: json.RawMessage(`{"aid":"BOB","display_name":"Bob"}`),
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(12), Method: "tools/call", Params: params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q", data.Code)
	}
}

func TestToolsCall_OperatorCreate_DuplicateAID(t *testing.T) {
	pool := &fakePool{insertErr: operator.ErrOperatorAlreadyExists}
	h, _, _ := newTestHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"operator.create"}},
		},
	})
	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.operator.create",
		Arguments: json.RawMessage(`{"aid":"archon-bob","display_name":"Bob"}`),
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(13), Method: "tools/call", Params: params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeOperatorExists {
		t.Errorf("data.code = %q", data.Code)
	}
}

func TestToolsCall_OperatorCreate_UnknownArg(t *testing.T) {
	pool := &fakePool{}
	h, _, _ := newTestHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"operator.create"}},
		},
	})
	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.operator.create",
		Arguments: json.RawMessage(`{"aid":"archon-bob","display_name":"Bob","extra":1}`),
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(14), Method: "tools/call", Params: params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeMalformedRequest {
		t.Errorf("data.code = %q, want malformed-request", data.Code)
	}
}

func TestToolsCall_OperatorRevoke_Success(t *testing.T) {
	pool := &fakePool{
		activeFn: func() ([]string, error) {
			return []string{"archon-alice", "archon-bob"}, nil
		},
	}
	h, _, rec := newTestHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "admin", Operators: []string{"archon-alice", "archon-bob"}, Permissions: []string{"*"}},
		},
	})
	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.operator.revoke",
		Arguments: json.RawMessage(`{"aid":"archon-bob","reason":"left team"}`),
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(20), Method: "tools/call", Params: params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if len(rec.events) != 1 || rec.events[0].EventType != "operator.revoked" {
		t.Errorf("audit events = %+v", rec.events)
	}
	if rec.events[0].Source != audit.SourceMCP {
		t.Errorf("Source = %q, want %q", rec.events[0].Source, audit.SourceMCP)
	}
}

func TestToolsCall_OperatorRevoke_WouldLockOut(t *testing.T) {
	pool := &fakePool{
		activeFn: func() ([]string, error) { return []string{"archon-alice"}, nil },
	}
	h, _, _ := newTestHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "admin", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
		},
	})
	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.operator.revoke",
		Arguments: json.RawMessage(`{"aid":"archon-alice"}`),
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(21), Method: "tools/call", Params: params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeWouldLockOutCluster {
		t.Errorf("data.code = %q", data.Code)
	}
}

func TestToolsCall_OperatorIssueToken_Success(t *testing.T) {
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{
				AID:         aid,
				DisplayName: aid,
				AuthMethod:  operator.AuthMethodJWT,
				CreatedAt:   time.Now(),
			}, nil
		},
	}
	h, iss, _ := newTestHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "issuer", Operators: []string{"archon-alice"}, Permissions: []string{"operator.issue-token"}},
		},
	})
	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.operator.issue-token",
		Arguments: json.RawMessage(`{"aid":"archon-bob"}`),
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(30), Method: "tools/call", Params: params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if !iss.called {
		t.Errorf("issuer not called")
	}
}

func TestToolsCall_OperatorIssueToken_Revoked(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{
				AID:        aid,
				AuthMethod: operator.AuthMethodJWT,
				RevokedAt:  &now,
			}, nil
		},
	}
	h, _, _ := newTestHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "issuer", Operators: []string{"archon-alice"}, Permissions: []string{"operator.issue-token"}},
		},
	})
	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.operator.issue-token",
		Arguments: json.RawMessage(`{"aid":"archon-bob"}`),
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(31), Method: "tools/call", Params: params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeOperatorRevoked {
		t.Errorf("data.code = %q", data.Code)
	}
}

func TestToolsCall_StubToolReturnsNotImplemented(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, nil)
	// keeper.push.cleanup остаётся stub (toolStatusStub) — ждёт SshDispatcher
	// Cleanup-wire-up-а (отдельный slice). Берём как репрезентативный stub-tool
	// (push.apply реализован в Variant C orchestrator slice; cloud-tools тоже stub).
	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.push.cleanup",
		Arguments: json.RawMessage(`{"inventory":["web-01"]}`),
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(40), Method: "tools/call", Params: params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeNotImplemented {
		t.Errorf("data.code = %q, want not-implemented", data.Code)
	}
}

func TestToolsCall_UnknownTool(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, nil)
	params, _ := json.Marshal(toolsCallParams{
		Name: "keeper.no.such",
	})
	req := jsonRPCRequest{
		JSONRPC: "2.0", ID: mustRawID(41), Method: "tools/call", Params: params,
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeNotFound {
		t.Errorf("data.code = %q", data.Code)
	}
}

func TestDispatch_BadJSONRPCVersion(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, nil)
	req := jsonRPCRequest{
		JSONRPC: "1.0",
		ID:      mustRawID(50),
		Method:  "tools/list",
	}
	resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
	if resp.Error == nil || resp.Error.Code != rpcCodeInvalidRequest {
		t.Errorf("Error = %+v", resp.Error)
	}
}

// TestDispatch_AllImplementedToolsDispatchable — каждый tool со
// status=Implemented в catalogManifest должен быть достижим из tools/call
// (диспетчер switch в handleToolsCall возвращает что-то отличное от
// «tool declared implemented but dispatch missing»). Это страхует
// switch-диспатчер от потерянных веток при добавлении новых implemented
// tools.
func TestDispatch_AllImplementedToolsDispatchable(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, &rbactest.Config{
		Roles: []rbactest.Role{
			// валидное permission не нужно — RBAC сработает раньше, но
			// нам важно не свалиться на «dispatch missing».
		},
	})
	for _, entry := range catalogManifest {
		if entry.status != toolStatusImplemented {
			continue
		}
		entry := entry
		t.Run(entry.decl.Name, func(t *testing.T) {
			params, _ := json.Marshal(toolsCallParams{
				Name:      entry.decl.Name,
				Arguments: json.RawMessage(`{}`),
			})
			req := jsonRPCRequest{
				JSONRPC: "2.0", ID: mustRawID(999), Method: "tools/call", Params: params,
			}
			resp, _ := h.Dispatch(context.Background(), claims("archon-alice"), req)
			if resp.Error == nil {
				return // success-ветка тоже OK (значит dispatch отработал)
			}
			data := mustToolErrorData(t, resp.Error.Data)
			if resp.Error.Message == "tool declared implemented but dispatch missing" {
				t.Fatalf("tool %q has no dispatch branch", entry.decl.Name)
			}
			if data.Code == mcpCodeNotImplemented {
				t.Fatalf("tool %q is declared Implemented but returned not-implemented", entry.decl.Name)
			}
		})
	}
}

// mustToolErrorData декодирует error.Data в mcpToolError. JSON-RPC сериализация
// any → map[string]any при unmarshal, поэтому делаем re-marshal через json
// для приведения к typed struct.
func mustToolErrorData(t *testing.T, data any) mcpToolError {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	var out mcpToolError
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	return out
}
