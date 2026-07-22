package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

	// incFn — resolves incarnation by name (SelectByName + existence-probe +
	// FOR UPDATE-select). nil → QueryRow on `FROM incarnation` returns
	// pgx.ErrNoRows (→ not-found). FOR UPDATE variant (unlock/upgrade) reads
	// state/status/version from the same inc.
	incFn func(name string) (*incarnation.Incarnation, error)

	// incListFn — backing for keeper.incarnation.list (SelectAll). Returns a
	// page + total. nil → empty list, total=0.
	incListFn func(filter incarnation.ListFilter) ([]*incarnation.Incarnation, int)

	// historyFn — backing for keeper.incarnation.history (HistorySelectByName).
	// nil → empty list, total=0.
	historyFn func(name string, filter incarnation.HistoryFilter) ([]*incarnation.HistoryEntry, int)

	// incInsertFn — backing for keeper.incarnation.create (Create insertSQL).
	// nil → INSERT succeeds (created_at/updated_at = zero). Returning an error
	// emulates UNIQUE / FK / other failures.
	incInsertFn func(name, service string) error

	// insertIncArgs — captured args of INSERT INTO incarnation (issued by the
	// create tool): [0]=name … [4]=spec(jsonb []byte) … [10]=traits(jsonb).
	// Lets a test assert spec.traits propagation on the create path (ADR-060
	// amend R1, parity with REST insertArgs).
	insertIncArgs []any

	// lastScenarioFn — backing for the rerun-last probe `SELECT scenario, apply_id FROM
	// state_history … ORDER BY history_id DESC LIMIT 1` (UnlockForRerun last-run).
	// nil → returns the creating scenario's name from the incFn row (default
	// `create`) — create path (last failed = created).
	lastScenarioFn func(name string) (string, error)

	// recipeFn — backing for the rerun-last day-2 recipe probe `SELECT recipe FROM
	// apply_runs WHERE apply_id = $1 AND recipe IS NOT NULL LIMIT 1`. nil →
	// ErrNoRows (fail-closed). Set for the day-2 happy path (recipe-jsonb with input).
	recipeFn func(applyID string) ([]byte, error)

	// soulBulkCountFn — backing for the souls-bulk traits projection (SyncTraitsToHosts
	// → CountBulkMatched: `SELECT COUNT(*) FROM souls …`). nil → 0 (0 member hosts,
	// sync-hook no-op — the usual case on create before onboarding). Nonzero → that
	// many "members", letting a test assert the projection was invoked.
	soulBulkCountFn func() int

	// deleteTag — RowsAffected for the single-winner `DELETE FROM incarnation`
	// (destroy force path, DeleteAfterTeardown). zero-value → "DELETE 1".
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
	// destroy force-DELETE (DeleteAfterTeardown): archive INSERTs + single-
	// winner DELETE FROM incarnation. deleteTag sets the DELETE's RowsAffected
	// (zero-value → "DELETE 1" = row removed; "DELETE 0" → no-op).
	if contains(sql, "DELETE FROM incarnation") {
		if f.deleteTag.String() == "" {
			return pgconn.NewCommandTag("DELETE 1"), nil
		}
		return f.deleteTag, nil
	}
	if contains(sql, "INSERT INTO incarnation_archive") || contains(sql, "INSERT INTO state_history_archive") {
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	// incarnation mutations (unlock state_history insert / status update,
	// upgrade tx writes, destroy state_history insert). Emulated as a successful
	// no-op — failures are injected via incFn / beginErr, not Exec.
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
	// SELECT COUNT(*) FROM souls … (SyncTraitsToHosts → CountBulkMatched). Without
	// this branch souls-bulk-count would hit errFakeUnexpected and the best-effort
	// sync-hook would just log a warning; this branch makes the projection observable.
	if contains(sql, "COUNT(*) FROM souls") {
		n := 0
		if f.soulBulkCountFn != nil {
			n = f.soulBulkCountFn()
		}
		return countRow{n: n}
	}
	// COUNT(*) FROM incarnation (list total) — no name arg.
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
	// mutations): returns a fresh updated_at on Scan(*time.Time). Checked BEFORE
	// the general `FROM incarnation` match (that predicate is also in this
	// UPDATE's WHERE).
	if contains(sql, "UPDATE incarnation") && contains(sql, "RETURNING updated_at") {
		return staticRow{values: []any{time.Now().UTC()}}
	}
	// FOR UPDATE-select on incarnation. FULL row (UpdateTraits:
	// `covens, traits` in the projection → scanIncarnation) → newIncRow. rerun-last
	// (state, status, created_scenario) → rerunForUpdateRow. Partial
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
	// rerun-last last-run probe: SELECT scenario, apply_id FROM state_history …
	// LIMIT 1. Checked BEFORE the general `FROM state_history`/COUNT (that one
	// requires a COUNT token).
	if contains(sql, "SELECT scenario") && contains(sql, "FROM state_history") {
		name := args[0].(string)
		const dummyApplyID = "01HFAILEDRUN00000000000000"
		if f.lastScenarioFn != nil {
			s, err := f.lastScenarioFn(name)
			if err != nil {
				return errRow{err: err}
			}
			return staticRow{values: []any{s, dummyApplyID}}
		}
		// Default: "last failed = creator". created_scenario is taken from the
		// incFn row (if set), else the canonical `create`.
		last := "create"
		if f.incFn != nil {
			if inc, err := f.incFn(name); err == nil && inc.CreatedScenario != nil {
				last = *inc.CreatedScenario
			}
		}
		return staticRow{values: []any{last, dummyApplyID}}
	}
	// rerun-last day-2 recipe probe: SELECT recipe FROM apply_runs WHERE apply_id …
	if contains(sql, "FROM apply_runs") && contains(sql, "recipe IS NOT NULL") {
		if f.recipeFn != nil {
			b, err := f.recipeFn(args[0].(string))
			if err != nil {
				return errRow{err: err}
			}
			return staticRow{values: []any{b}}
		}
		return errRow{err: pgx.ErrNoRows}
	}
	// SelectByName / existence-probe (full incarnation row).
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
	// Synod branch of the self-lockout core (ADR-049(f), Synod S2 epic):
	// LockEffectiveClusterAdmins sends a second locking query against
	// synod_operators. mcp scenarios don't model group admins — empty; covered
	// by rbac integration-guard tests instead. Checked BEFORE the direct branch.
	if contains(sql, "FROM synod_operators") {
		return &stringRows{}, nil
	}
	// Slice 3: operator.revoke's lockout-probe goes through
	// rbac.LockEffectiveClusterAdmins — SELECT ro.aid FROM rbac_role_operators
	// JOIN … FOR UPDATE OF ro,rp,o. activeFn returns the already-effective set
	// of active `*`-admins from the DB (the full admin set from the DB, no
	// intersection with the in-memory snapshot).
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

// listItems / historyItems — shared backing accessors so the COUNT- and
// list-variant SQL return consistent total/items from a single source.
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

// countRow — pgx.Row for SELECT COUNT(*) (Scan into *int).
type countRow struct{ n int }

func (r countRow) Scan(dest ...any) error {
	if p, ok := dest[0].(*int); ok {
		*p = r.n
		return nil
	}
	return fmt.Errorf("countRow.Scan: unexpected dest type %T", dest[0])
}

// forUpdateIncRow — pgx.Row for incarnation FOR UPDATE-selects.
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

// rerunForUpdateRow — pgx.Row for UnlockForRerun's FOR UPDATE-select
// `SELECT state, status, created_scenario, spec`. Scan(state []byte, status string,
// created_scenario *string — NULL=bare, spec []byte to propagate spec.input, B1).
type rerunForUpdateRow struct{ inc *incarnation.Incarnation }

func (r rerunForUpdateRow) Scan(dest ...any) error {
	if len(dest) != 4 {
		return fmt.Errorf("rerunForUpdateRow.Scan: want 4 dest, got %d", len(dest))
	}
	state := []byte("null")
	if r.inc.State != nil {
		b, _ := json.Marshal(r.inc.State)
		state = b
	}
	*dest[0].(*[]byte) = state
	*dest[1].(*string) = string(r.inc.Status)
	// created_scenario NULLABLE → **string (NULL=bare incarnation).
	*dest[2].(**string) = r.inc.CreatedScenario
	// spec jsonb (B1): serializes inc.Spec; nil → `{}` (incarnation.InputFromSpec
	// extracts spec.input when present).
	spec := []byte("{}")
	if r.inc.Spec != nil {
		b, _ := json.Marshal(r.inc.Spec)
		spec = b
	}
	*dest[3].(*[]byte) = spec
	return nil
}

// incRows — pgx.Rows over a slice of incRow (list items).
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

// historyRow — pgx.Row for a single state_history record (scanHistoryEntry reads
// 7 columns: history_id, scenario, state_before, state_after, changed_by_aid,
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

// historyRows — pgx.Rows over a slice of historyRow.
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

// contains — substring check without strings (avoids a dependency in the test fake).
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

// mcpStarter — mock of [handlers.ScenarioStarter]. Mirrors the REST fakeStarter:
// captures spec + call count, injects an error.
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

// mcpResolver — mock of [handlers.ServiceResolver]. ok=false → not-registered.
type mcpResolver struct{ ok bool }

func (f *mcpResolver) Resolve(service string) (artifact.ServiceRef, bool) {
	return artifact.ServiceRef{Name: service, Ref: "v1"}, f.ok
}

// mcpLoader — mock of [handlers.ServiceSnapshotLoader]. Mirrors the REST fakeLoader.
type mcpLoader struct {
	targetSchema int
	loadErr      error
	chain        statemigrate.Chain
	chainErr     error

	// destroy pre-check (ReadFile): hasDestroyScenario=true → scenario `destroy`
	// "exists" in the snapshot; false → os.ErrNotExist. readErr overrides (I/O failure).
	hasDestroyScenario bool
	readErr            error

	// scenarioYAML — for sync input validation (scenario.ValidateInput):
	// non-empty → ReadFile returns this YAML as scenario/<name>/main.yml.
	scenarioYAML string

	// localDir — snapshot root on disk (Phase 2). Non-empty → Load sets LocalDir
	// (ResolveCreateScenarios scans it), ReadFile reads from disk (path-aware),
	// taking precedence over scenarioYAML/hasDestroyScenario. Needed for the
	// multi create-scenario mechanism: a create scenario must live on disk with
	// `create: true`, else the set is empty → bare incarnation.
	localDir string
}

func (f *mcpLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return &artifact.ServiceArtifact{
		Ref:      ref,
		LocalDir: f.localDir,
		Manifest: &config.ServiceManifest{StateSchemaVersion: f.targetSchema},
	}, nil
}

func (f *mcpLoader) LoadMigrationChain(_ *artifact.ServiceArtifact, _, _ int) (statemigrate.Chain, error) {
	if f.chainErr != nil {
		return nil, f.chainErr
	}
	return f.chain, nil
}

func (f *mcpLoader) ListUpgrades(_ *artifact.ServiceArtifact) ([]artifact.Scenario, error) {
	return nil, nil
}

// ReadFile — for the destroy PrepareDestroy pre-check (scenario `destroy`
// presence) and sync input validation. localDir (if set) → path-aware disk read.
func (f *mcpLoader) ReadFile(_ *artifact.ServiceArtifact, file string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.localDir != "" {
		return os.ReadFile(filepath.Join(f.localDir, filepath.FromSlash(file)))
	}
	if f.scenarioYAML != "" {
		return []byte(f.scenarioYAML), nil
	}
	if f.hasDestroyScenario {
		return []byte("tasks: []\n"), nil
	}
	return nil, os.ErrNotExist
}

// mcpCreateSnapshot writes scenario/create/main.yml (yaml) under a temp root and
// returns it (for mcpLoader.localDir). Phase 2: ResolveCreateScenarios scans
// localDir, so the create scenario must live on disk. If yaml lacks
// `create: true`, prefix the flag (to land in the create set) while keeping the
// `create` name. t.TempDir auto-cleans.
func mcpCreateSnapshot(t *testing.T, yaml string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "scenario", "create")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if !contains(yaml, "create: true") {
		yaml = "create: true\n" + yaml
	}
	if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write create/main.yml: %v", err)
	}
	return root
}

// mcpEmptyCreateSnapshot writes a snapshot with no create scenario at all (only
// an operational restart) and returns the root. For bare incarnations: an empty
// create-scenario set → creation without a run.
func mcpEmptyCreateSnapshot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "scenario", "restart")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte("name: restart\ntasks: []\n"), 0o644); err != nil {
		t.Fatalf("write restart/main.yml: %v", err)
	}
	return root
}

// newTestHandlerFull — like newTestHandler, but wires incarnation deps
// (runner / registry / loader) for the create/run/upgrade tools. nil deps →
// tool responds not-configured (parity with REST 500).
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

// callTool — generic runner for any incarnation tool through the real Dispatch.
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
	// No IncarnationDB → error.
	if _, err := NewHandler(base); err == nil {
		t.Fatal("NewHandler must reject nil IncarnationDB")
	}
	// With IncarnationDB → ok.
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
	// Names must stay stable (spec — mcp-tools.md).
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
	// ADR-022(b): the MCP handler writes audit events with Source=mcp (not api),
	// otherwise the granular trail is lost.
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
	// keeper.push.cleanup remains a stub (toolStatusStub) — awaiting the
	// SshDispatcher Cleanup wire-up (separate slice). Used as a representative
	// stub tool (push.apply is implemented in the Variant C orchestrator slice;
	// cloud-tools are also stubs).
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

// TestDispatch_AllImplementedToolsDispatchable — every tool with
// status=Implemented in catalogManifest must be reachable from tools/call
// (the switch dispatcher in handleToolsCall must return something other than
// "tool declared implemented but dispatch missing"). Guards the switch
// dispatcher against lost branches when new implemented tools are added.
func TestDispatch_AllImplementedToolsDispatchable(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, &rbactest.Config{
		Roles: []rbactest.Role{
			// a valid permission isn't needed — RBAC fires first, but what
			// matters here is not falling into "dispatch missing".
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
				return // success branch is fine too (means dispatch worked)
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

// mustToolErrorData decodes error.Data into mcpToolError. JSON-RPC unmarshal
// turns any → map[string]any, so we re-marshal through json to coerce it into
// a typed struct.
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
