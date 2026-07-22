package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
)

// --- fake pgx.Tx / TxBeginner ----------------------------------------
//
// Transactional operations (Unlock / UpgradeStateSchema) go through
// TxBeginner.BeginTx → pgx.Tx. fakeTx implements pgx.Tx: Exec / QueryRow /
// Query + Commit / Rollback; other methods are stubs (panic) since they're
// unused in these code paths.

// scriptedRow — QueryRow response for FOR UPDATE: either Scan values or an
// error (ErrNoRows for not-found).
type scriptedRow struct {
	values []any
	err    error
}

func (r scriptedRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		assign(d, r.values[i])
	}
	return nil
}

type fakeTx struct {
	// FOR UPDATE response (one per upgrade/unlock transaction).
	selectRow scriptedRow

	// queryRows — sequence of QueryRow responses in call order (for operations
	// with multiple QueryRow calls in one tx, e.g. UnlockForRerun: FOR UPDATE +
	// last-scenario probe). If set, consumed by queryN index; exhausting the
	// sequence falls back to selectRow (single-QueryRow tests unaffected — they
	// leave queryRows nil).
	queryRows []scriptedRow
	queryN    int

	execErrAt int // index of the Exec call at which to return execErr (-1 = never)
	execErr   error
	committed bool
	rolled    bool

	// execTags — CommandTag by Exec call index (for checking RowsAffected, e.g.
	// single-winner DELETE with RowsAffected==0). nil / out of range → default
	// "UPDATE 1" (original behavior, existing tests unaffected).
	execTags []pgconn.CommandTag

	execSQLs []string
	execArgs [][]any
	execN    int
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	idx := f.execN
	f.execN++
	f.execSQLs = append(f.execSQLs, sql)
	f.execArgs = append(f.execArgs, args)
	if f.execErr != nil && idx == f.execErrAt {
		return pgconn.CommandTag{}, f.execErr
	}
	if idx < len(f.execTags) {
		return f.execTags[idx], nil
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (f *fakeTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	if f.queryN < len(f.queryRows) {
		row := f.queryRows[f.queryN]
		f.queryN++
		return row
	}
	return f.selectRow
}

func (f *fakeTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return &fakeRows{}, nil
}

func (f *fakeTx) Commit(_ context.Context) error   { f.committed = true; return nil }
func (f *fakeTx) Rollback(_ context.Context) error { f.rolled = true; return nil }

func (f *fakeTx) Begin(context.Context) (pgx.Tx, error) { panic("fakeTx.Begin: unused") }
func (f *fakeTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("fakeTx.CopyFrom: unused")
}
func (f *fakeTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("fakeTx.SendBatch: unused")
}
func (f *fakeTx) LargeObjects() pgx.LargeObjects { panic("fakeTx.LargeObjects: unused") }
func (f *fakeTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("fakeTx.Prepare: unused")
}
func (f *fakeTx) Conn() *pgx.Conn { return nil }

// fakePool — TxBeginner stub. Hands out pre-built transactions in BeginTx
// call order (upgrade-tx, then migration_failed-tx).
type fakePool struct {
	txs    []*fakeTx
	beginN int
}

func (p *fakePool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	tx := p.txs[p.beginN]
	p.beginN++
	return tx, nil
}

// compile-time check.
var _ TxBeginner = (*fakePool)(nil)

// newEvaluator — real migration-CEL engine (thin wrapper for tests).
func newEvaluator(t *testing.T) statemigrate.Evaluator {
	t.Helper()
	ev, err := statemigrate.NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return ev
}

// setStep builds a from→to migration with a single set state.v = <to>.
func setStep(from, to int) *statemigrate.Migration {
	return &statemigrate.Migration{
		FromVersion: from,
		ToVersion:   to,
		Transform: []statemigrate.Op{
			{Set: &statemigrate.SetOp{Path: "state.v", Value: to}},
		},
	}
}

// --- UpgradeStateSchema ----------------------------------------------

func TestUpgradeStateSchema_HappyMultiStep(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		// SELECT … FOR UPDATE: state={"v":1}, version=1, status=ready.
		selectRow: scriptedRow{values: []any{[]byte(`{"v":1}`), 1, "ready"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}
	aid := "archon-alice"

	res, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
		Name:             "redis-prod",
		TargetServiceVer: "v3.0.0",
		TargetSchemaVer:  3,
		Chain:            statemigrate.Chain{setStep(1, 2), setStep(2, 3)},
		Evaluator:        newEvaluator(t),
		ApplyID:          "01HUPGRADE0000000000000000",
		ChangedByAID:     &aid,
	})
	if err != nil {
		t.Fatalf("UpgradeStateSchema: %v", err)
	}
	if res.FromSchemaVer != 1 || res.ToSchemaVer != 3 || res.Steps != 2 {
		t.Errorf("res = %+v, want from=1 to=3 steps=2", res)
	}
	if !tx.committed {
		t.Error("tx not committed")
	}

	// 2 INSERT state_history (per-step) + 1 UPDATE incarnation + 1 INSERT
	// drift-transition history = 4 Exec.
	if tx.execN != 4 {
		t.Fatalf("Exec calls = %d, want 4 (2 step-history + update + drift-history)", tx.execN)
	}
	for i := 0; i < 2; i++ {
		if !strings.Contains(tx.execSQLs[i], "INSERT INTO state_history") {
			t.Errorf("Exec[%d] = %q, want state_history INSERT", i, tx.execSQLs[i])
		}
		// scenario label = "migration".
		if tx.execArgs[i][2] != migrationScenarioLabel {
			t.Errorf("Exec[%d] scenario = %v, want %q", i, tx.execArgs[i][2], migrationScenarioLabel)
		}
		// shared apply_id across all steps.
		if tx.execArgs[i][6] != "01HUPGRADE0000000000000000" {
			t.Errorf("Exec[%d] apply_id = %v, want shared ApplyID", i, tx.execArgs[i][6])
		}
	}
	// distinct history_id per step.
	if tx.execArgs[0][0] == tx.execArgs[1][0] {
		t.Errorf("history_id not unique per step: %v", tx.execArgs[0][0])
	}

	// UPDATE: state migrated (v=3), schema=3, service_version=v3.0.0.
	up := tx.execArgs[2]
	if !strings.Contains(tx.execSQLs[2], "UPDATE incarnation") {
		t.Fatalf("Exec[2] = %q, want incarnation UPDATE", tx.execSQLs[2])
	}
	var finalState map[string]any
	if err := json.Unmarshal(up[1].([]byte), &finalState); err != nil {
		t.Fatalf("final state not JSON: %v", err)
	}
	if finalState["v"] != float64(3) {
		t.Errorf("final state.v = %v, want 3", finalState["v"])
	}
	if up[2] != 3 {
		t.Errorf("UPDATE schema arg = %v, want 3", up[2])
	}
	if up[3] != "v3.0.0" {
		t.Errorf("UPDATE service_version arg = %v, want v3.0.0", up[3])
	}
	// Final status is drift, NOT ready (ADR-031 amendment): hosts are still
	// waiting to apply the new version. status is a SQL literal (not a
	// bind-arg), so check the UPDATE statement text.
	if !strings.Contains(tx.execSQLs[2], "status               = 'drift'") {
		t.Errorf("UPDATE statement = %q, want status='drift' (ADR-031 upgrade→drift)", tx.execSQLs[2])
	}

	// Drift-transition history (Exec[3]): scenario=upgrade-pending-apply,
	// zero-diff (state_before==state_after = post-migration final state),
	// shares apply_id with the step snapshots.
	if !strings.Contains(tx.execSQLs[3], "INSERT INTO state_history") {
		t.Fatalf("Exec[3] = %q, want drift-transition state_history INSERT", tx.execSQLs[3])
	}
	dh := tx.execArgs[3]
	if dh[2] != upgradeDriftScenarioLabel {
		t.Errorf("drift-history scenario = %v, want %q", dh[2], upgradeDriftScenarioLabel)
	}
	if string(dh[3].([]byte)) != string(up[1].([]byte)) {
		t.Errorf("drift-history state does not equal post-migration: %s vs %s", dh[3], up[1])
	}
	if dh[5] != "01HUPGRADE0000000000000000" {
		t.Errorf("drift-history apply_id = %v, want shared ApplyID", dh[5])
	}
}

// upgradeUPDATEStatus extracts the final status from the upgrade-tx's
// captured Exec calls: finds the UPDATE incarnation setting the status and
// returns the status='<x>' literal value. Status is a SQL literal (not a
// bind-arg), so we parse the statement text. Returns empty string if no
// UPDATE incarnation is found.
func upgradeUPDATEStatus(t *testing.T, tx *fakeTx) string {
	t.Helper()
	for _, sql := range tx.execSQLs {
		if !strings.Contains(sql, "UPDATE incarnation") || !strings.Contains(sql, "state_schema_version") {
			continue
		}
		const marker = "status               = '"
		i := strings.Index(sql, marker)
		if i < 0 {
			t.Fatalf("UPDATE incarnation without a status literal: %q", sql)
		}
		rest := sql[i+len(marker):]
		j := strings.IndexByte(rest, '\'')
		if j < 0 {
			t.Fatalf("unterminated status literal in UPDATE: %q", sql)
		}
		return rest[:j]
	}
	return ""
}

// TestUpgradeStateSchema_FinalStatusDrift — BEHAVIORAL INVARIANT (ADR-031
// amendment, catches regressions): on a successful upgrade the incarnation
// MUST transition to status=drift, NOT ready. Gap between upgrade and hosts:
// DB state changed but hosts are still on the old rollout — drift signals the
// operator to "apply to hosts". If a future change reverts to 'ready' in the
// final UPDATE, this test fails. Cases: migration from ready, migration from
// drift (drift→drift, repeated upgrade), no-op ref-bump.
func TestUpgradeStateSchema_FinalStatusDrift(t *testing.T) {
	cases := []struct {
		name      string
		startStat string
		chain     statemigrate.Chain
		targetVer int
		fromState string
	}{
		{"from_ready_migration", "ready", statemigrate.Chain{setStep(1, 2)}, 2, `{"v":1}`},
		{"from_drift_migration", "drift", statemigrate.Chain{setStep(1, 2)}, 2, `{"v":1}`},
		{"noop_ref_bump", "ready", statemigrate.Chain{}, 2, `{"v":2}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Starting schema version of the FOR UPDATE row: for a migration it's
			// chain[0].From, for a no-op it's target (equal to current).
			startVer := c.targetVer
			if len(c.chain) > 0 {
				startVer = c.chain[0].FromVersion
			}
			tx := &fakeTx{
				execErrAt: -1,
				selectRow: scriptedRow{values: []any{[]byte(c.fromState), startVer, c.startStat}},
			}
			pool := &fakePool{txs: []*fakeTx{tx}}

			_, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
				Name:             "redis-prod",
				TargetServiceVer: "v9.9.9",
				TargetSchemaVer:  c.targetVer,
				Chain:            c.chain,
				Evaluator:        newEvaluator(t),
				ApplyID:          "01HUPGRADEDRIFT00000000000",
			})
			if err != nil {
				t.Fatalf("UpgradeStateSchema: %v", err)
			}
			if !tx.committed {
				t.Fatal("upgrade tx not committed")
			}
			if got := upgradeUPDATEStatus(t, tx); got != string(StatusDrift) {
				t.Errorf("final status = %q, want drift (ADR-031: upgrade leaves hosts behind the DB state)", got)
			}
			// The transition reason is recorded as a separate history entry.
			var sawDriftHistory bool
			for i, sql := range tx.execSQLs {
				if strings.Contains(sql, "INSERT INTO state_history") && tx.execArgs[i][2] == upgradeDriftScenarioLabel {
					sawDriftHistory = true
				}
			}
			if !sawDriftHistory {
				t.Errorf("no state_history record of transition to drift (scenario=%q)", upgradeDriftScenarioLabel)
			}
		})
	}
}

// TestUpgradeStateSchema_LockedStatusNotOverwritten — GUARD: blocking statuses
// (error_locked / migration_failed / applying) are NOT overwritten by upgrade
// into drift. Rejected before any mutation (gate in upgradeTx): transaction is
// not committed, zero Exec calls. Protects the invariant "upgrade never
// silently clears a lock" — error_locked/migration_failed require an explicit
// unlock, applying means a run is in progress.
func TestUpgradeStateSchema_LockedStatusNotOverwritten(t *testing.T) {
	cases := []struct {
		status  string
		wantErr error
	}{
		{"error_locked", ErrIncarnationLocked},
		{"migration_failed", ErrIncarnationLocked},
		{"applying", ErrIncarnationBusy},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			tx := &fakeTx{
				execErrAt: -1,
				selectRow: scriptedRow{values: []any{[]byte(`{"v":1}`), 1, c.status}},
			}
			pool := &fakePool{txs: []*fakeTx{tx}}

			_, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
				Name:             "redis-prod",
				TargetServiceVer: "v2.0.0",
				TargetSchemaVer:  2,
				Chain:            statemigrate.Chain{setStep(1, 2)},
				Evaluator:        newEvaluator(t),
				ApplyID:          "01HUPGRADELOCKED0000000000",
			})
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("status=%s: err = %v, want %v", c.status, err, c.wantErr)
			}
			// No mutation, no commit: blocking status preserved as-is.
			if tx.committed {
				t.Errorf("status=%s: tx committed - blocking status overwritten", c.status)
			}
			if tx.execN != 0 {
				t.Errorf("status=%s: Exec = %d, want 0 (rejection BEFORE mutation, status untouched)", c.status, tx.execN)
			}
		})
	}
}

func TestUpgradeStateSchema_NoOpEmptyChain(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		// version=2, ref-bump without a schema change.
		selectRow: scriptedRow{values: []any{[]byte(`{"v":2}`), 2, "ready"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
		Name:             "redis-prod",
		TargetServiceVer: "v2.1.0",
		TargetSchemaVer:  2, // == current
		Chain:            statemigrate.Chain{},
		Evaluator:        newEvaluator(t),
		ApplyID:          "01HUPGRADE0000000000000001",
	})
	if err != nil {
		t.Fatalf("UpgradeStateSchema: %v", err)
	}
	if res.Steps != 0 || res.FromSchemaVer != 2 || res.ToSchemaVer != 2 {
		t.Errorf("res = %+v, want steps=0 from=to=2", res)
	}
	if !tx.committed {
		t.Error("tx not committed")
	}
	// 1 zero-diff migration-history + 1 UPDATE + 1 drift-transition history = 3 Exec.
	if tx.execN != 3 {
		t.Fatalf("Exec calls = %d, want 3 (zero-diff history + update + drift-history)", tx.execN)
	}
	// zero-diff: state_before == state_after.
	h := tx.execArgs[0]
	if string(h[3].([]byte)) != string(h[4].([]byte)) {
		t.Errorf("no-op history not zero-diff: before=%s after=%s", h[3], h[4])
	}
	// service_version is updated regardless.
	if tx.execArgs[1][3] != "v2.1.0" {
		t.Errorf("UPDATE service_version = %v, want v2.1.0", tx.execArgs[1][3])
	}
	// No-op ref-bump also transitions to drift (the new ref's rollout isn't on hosts yet).
	if !strings.Contains(tx.execSQLs[1], "status               = 'drift'") {
		t.Errorf("no-op UPDATE statement = %q, want status='drift'", tx.execSQLs[1])
	}
	if tx.execArgs[2][2] != upgradeDriftScenarioLabel {
		t.Errorf("no-op drift-history scenario = %v, want %q", tx.execArgs[2][2], upgradeDriftScenarioLabel)
	}
}

func TestUpgradeStateSchema_DowngradeReject(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{"v":3}`), 3, "ready"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
		Name:             "redis-prod",
		TargetServiceVer: "v1.0.0",
		TargetSchemaVer:  1, // < current 3
		Chain:            statemigrate.Chain{},
		Evaluator:        newEvaluator(t),
		ApplyID:          "01HUPGRADE0000000000000002",
	})
	if !errors.Is(err, ErrDowngradeUnsupported) {
		t.Fatalf("err = %v, want ErrDowngradeUnsupported", err)
	}
	if tx.committed {
		t.Error("downgrade reject must not commit")
	}
	if tx.execN != 0 {
		t.Errorf("Exec calls = %d, want 0 (rejected before write)", tx.execN)
	}
}

func TestUpgradeStateSchema_VersionMismatchReject(t *testing.T) {
	// current=2, but chain[0].From=1 → someone upgraded between resolve and lock.
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{"v":2}`), 2, "ready"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
		Name:             "redis-prod",
		TargetServiceVer: "v3.0.0",
		TargetSchemaVer:  3,
		Chain:            statemigrate.Chain{setStep(1, 2), setStep(2, 3)},
		Evaluator:        newEvaluator(t),
		ApplyID:          "01HUPGRADE0000000000000003",
	})
	if !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("err = %v, want ErrSchemaVersionMismatch", err)
	}
	if tx.execN != 0 {
		t.Errorf("Exec calls = %d, want 0", tx.execN)
	}
}

func TestUpgradeStateSchema_GateBusyReject(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{"v":1}`), 1, "applying"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
		Name:             "redis-prod",
		TargetServiceVer: "v2.0.0",
		TargetSchemaVer:  2,
		Chain:            statemigrate.Chain{setStep(1, 2)},
		Evaluator:        newEvaluator(t),
		ApplyID:          "01HUPGRADE0000000000000004",
	})
	if !errors.Is(err, ErrIncarnationBusy) {
		t.Fatalf("err = %v, want ErrIncarnationBusy", err)
	}
}

func TestUpgradeStateSchema_GateLockedReject(t *testing.T) {
	for _, st := range []string{"error_locked", "migration_failed"} {
		tx := &fakeTx{
			execErrAt: -1,
			selectRow: scriptedRow{values: []any{[]byte(`{"v":1}`), 1, st}},
		}
		pool := &fakePool{txs: []*fakeTx{tx}}
		_, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
			Name:             "redis-prod",
			TargetServiceVer: "v2.0.0",
			TargetSchemaVer:  2,
			Chain:            statemigrate.Chain{setStep(1, 2)},
			Evaluator:        newEvaluator(t),
			ApplyID:          "01HUPGRADE0000000000000005",
		})
		if !errors.Is(err, ErrIncarnationLocked) {
			t.Fatalf("status %q: err = %v, want ErrIncarnationLocked", st, err)
		}
	}
}

func TestUpgradeStateSchema_NotFound(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{err: pgx.ErrNoRows},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
		Name:             "ghost",
		TargetServiceVer: "v2.0.0",
		TargetSchemaVer:  2,
		Chain:            statemigrate.Chain{setStep(1, 2)},
		Evaluator:        newEvaluator(t),
		ApplyID:          "01HUPGRADE0000000000000006",
	})
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Fatalf("err = %v, want ErrIncarnationNotFound", err)
	}
}

// TestUpgradeStateSchema_WriteError_MigrationFailed: provoke a write failure
// (Exec on INSERT history) → upgrade-tx rolls back, a separate background-tx
// marks migration_failed; state is unchanged (rollback). We use a write
// failure instead of a CEL error: a statically built Chain is valid, making it
// simpler and more precise to verify failure-handling via the pool with two
// transactions.
func TestUpgradeStateSchema_WriteError_MigrationFailed(t *testing.T) {
	upTx := &fakeTx{
		// fail on the first Exec (INSERT state_history).
		execErrAt: 0,
		execErr:   errors.New("db gone"),
		selectRow: scriptedRow{values: []any{[]byte(`{"v":1}`), 1, "ready"}},
	}
	failTx := &fakeTx{execErrAt: -1}
	pool := &fakePool{txs: []*fakeTx{upTx, failTx}}

	_, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
		Name:             "redis-prod",
		TargetServiceVer: "v2.0.0",
		TargetSchemaVer:  2,
		Chain:            statemigrate.Chain{setStep(1, 2)},
		Evaluator:        newEvaluator(t),
		ApplyID:          "01HUPGRADE0000000000000007",
	})
	if err == nil {
		t.Fatal("write error returned nil")
	}
	if isUpgradeRejection(err) {
		t.Errorf("write error must not be a rejection sentinel: %v", err)
	}
	// upgrade-tx rolled back, not committed.
	if upTx.committed {
		t.Error("upgrade tx committed despite write error")
	}
	if !upTx.rolled {
		t.Error("upgrade tx not rolled back")
	}
	// migration_failed background-tx: BeginTx called twice, second-tx commits.
	if pool.beginN != 2 {
		t.Fatalf("BeginTx calls = %d, want 2 (upgrade + migration_failed)", pool.beginN)
	}
	if !failTx.committed {
		t.Error("migration_failed tx not committed")
	}
	if failTx.execN != 1 {
		t.Fatalf("migration_failed Exec = %d, want 1 (status UPDATE)", failTx.execN)
	}
	if !strings.Contains(failTx.execSQLs[0], "migration_failed") {
		t.Errorf("migration_failed SQL = %q", failTx.execSQLs[0])
	}
}

// --- Unlock (unit, via fakeTx) -------------------------------------

func TestUnlock_FromMigrationFailed(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{"primary":"redis-01"}`), "migration_failed"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := Unlock(context.Background(), pool, "redis-prod", "manual cleanup", "archon-alice", "01HUNLOCK0000000000000010")
	if err != nil {
		t.Fatalf("Unlock from migration_failed: %v", err)
	}
	if res.PreviousStatus != StatusMigrationFailed {
		t.Errorf("PreviousStatus = %q, want migration_failed", res.PreviousStatus)
	}
	if !tx.committed {
		t.Error("unlock tx not committed")
	}
	// INSERT history + UPDATE status → ready.
	if tx.execN != 2 {
		t.Fatalf("Exec = %d, want 2", tx.execN)
	}
	if tx.execArgs[1][1] != string(StatusReady) {
		t.Errorf("UPDATE status arg = %v, want ready", tx.execArgs[1][1])
	}
}

func TestUnlock_FromErrorLocked_Regression(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{}`), "error_locked"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := Unlock(context.Background(), pool, "redis-prod", "x", "archon-alice", "01HUNLOCK0000000000000011")
	if err != nil {
		t.Fatalf("Unlock from error_locked: %v", err)
	}
	if res.PreviousStatus != StatusErrorLocked {
		t.Errorf("PreviousStatus = %q, want error_locked", res.PreviousStatus)
	}
	if !tx.committed {
		t.Error("unlock tx not committed")
	}
}

// --- UnlockForRerun (unit, via fakeTx) ------------------------------

// TestUnlockForRerun_FromErrorLocked — allowed from error_locked: state
// unchanged (state_before == state_after), status → applying (NOT ready —
// race-free), under a single FOR UPDATE; snapshot in state_history labeled
// rerun-last with a shared apply_id; commit.
func TestUnlockForRerun_FromErrorLocked(t *testing.T) {
	const applyID = "01HRERUN00000000000000000A"
	tx := &fakeTx{
		execErrAt: -1,
		// #1 FOR UPDATE (state, status, created_scenario, spec); #2 last-run probe
		// (scenario, apply_id). create path (last==created) → input from
		// spec.input, no recipe-probe.
		queryRows: []scriptedRow{
			{values: []any{[]byte(`{"primary":"redis-01"}`), "error_locked", "create", []byte(`{"input":{"version":"8.6.1"}}`)}},
			{values: []any{"create", "01HFAILEDRUN0000000000000A"}},
		},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := UnlockForRerun(context.Background(), pool, "redis-prod", "rerun bootstrap", "archon-alice", "01HRERUNHIST000000000000A", applyID)
	if err != nil {
		t.Fatalf("UnlockForRerun from error_locked: %v", err)
	}
	if res.PreviousStatus != StatusErrorLocked {
		t.Errorf("PreviousStatus = %q, want error_locked", res.PreviousStatus)
	}
	if res.Scenario != "create" {
		t.Errorf("Scenario = %q, want create", res.Scenario)
	}
	// create path: stored spec.input is returned in UnlockResult.Input.
	if res.Input == nil {
		t.Fatal("UnlockResult.Input = nil - spec.input NOT read under FOR UPDATE")
	}
	if res.Input["version"] != "8.6.1" {
		t.Errorf("UnlockResult.Input[version] = %v, want 8.6.1 (stored spec.input)", res.Input["version"])
	}
	if !tx.committed {
		t.Error("rerun-last tx not committed")
	}
	// INSERT history + UPDATE status → applying (NOT ready).
	if tx.execN != 2 {
		t.Fatalf("Exec = %d, want 2 (history + update)", tx.execN)
	}
	hist := tx.execArgs[0]
	if hist[2] != rerunLastScenarioLabel {
		t.Errorf("history scenario = %v, want %q", hist[2], rerunLastScenarioLabel)
	}
	if hist[5] != applyID {
		t.Errorf("history apply_id = %v, want %q", hist[5], applyID)
	}
	if got := tx.execArgs[1][1]; got != string(StatusApplying) {
		t.Errorf("UPDATE status arg = %v, want applying (bypassing ready)", got)
	}
	if tx.execArgs[1][1] == string(StatusReady) {
		t.Error("rerun moved to ready - race window not closed (should be applying)")
	}
}

// TestUnlockForRerun_RejectNonErrorLocked — strictly allowed only from
// error_locked: ready / applying / migration_failed / destroy_failed →
// ErrIncarnationNotErrorLocked, transaction not committed (for those, use
// plain unlock + manual run).
func TestUnlockForRerun_RejectNonErrorLocked(t *testing.T) {
	for _, status := range []string{"ready", "applying", "migration_failed", "destroy_failed", "destroying", "drift"} {
		t.Run(status, func(t *testing.T) {
			tx := &fakeTx{
				execErrAt: -1,
				selectRow: scriptedRow{values: []any{[]byte(`{}`), status, "create", []byte("{}")}},
			}
			pool := &fakePool{txs: []*fakeTx{tx}}

			_, err := UnlockForRerun(context.Background(), pool, "redis-prod", "x", "archon-alice", "01HRERUNHIST000000000000B", "01HRERUN00000000000000000B")
			if !errors.Is(err, ErrIncarnationNotErrorLocked) {
				t.Fatalf("status=%s: err = %v, want ErrIncarnationNotErrorLocked", status, err)
			}
			if tx.committed {
				t.Errorf("status=%s: tx committed (should be rejected without mutation)", status)
			}
			if tx.execN != 0 {
				t.Errorf("status=%s: Exec = %d, want 0 (rejection BEFORE mutation)", status, tx.execN)
			}
		})
	}
}

// TestUnlockForRerun_Day2_ReusesRecipeInput — day-2 happy path: the last
// failed run was add_user (≠ created `create`), so its input comes from the
// recipe's apply_run (NOT spec.input) → allowed, Scenario=="add_user",
// Input=={user:alice}.
func TestUnlockForRerun_Day2_ReusesRecipeInput(t *testing.T) {
	const applyID = "01HRERUN00000000000000000E"
	tx := &fakeTx{
		execErrAt: -1,
		queryRows: []scriptedRow{
			// spec.input carries version — must NOT leak through (day-2 uses the recipe).
			{values: []any{[]byte(`{"primary":"redis-01"}`), "error_locked", "create", []byte(`{"input":{"version":"8.6.1"}}`)}},
			{values: []any{"add_user", "01HFAILEDRUN0000000000000E"}},
			{values: []any{[]byte(`{"scenario_name":"add_user","input":{"user":"alice"}}`)}},
		},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := UnlockForRerun(context.Background(), pool, "redis-prod", "rerun add_user", "archon-alice", "01HRERUNHIST000000000000E", applyID)
	if err != nil {
		t.Fatalf("UnlockForRerun day-2 add_user: %v", err)
	}
	if res.Scenario != "add_user" {
		t.Errorf("Scenario = %q, want add_user (last failed operational run)", res.Scenario)
	}
	if res.Input == nil {
		t.Fatal("UnlockResult.Input = nil - recipe.input NOT read (operational regression)")
	}
	if res.Input["user"] != "alice" {
		t.Errorf("UnlockResult.Input[user] = %v, want alice (recipe.input)", res.Input["user"])
	}
	if _, leaked := res.Input["version"]; leaked {
		t.Error("UnlockResult.Input carries spec.input[version] - operational must take recipe.input, not spec")
	}
	// recipe without from_upgrade → FromUpgrade=false (rerun from scenario/, ADR-0068).
	if res.FromUpgrade {
		t.Error("UnlockResult.FromUpgrade = true, want false (recipe without from_upgrade)")
	}
	if !tx.committed {
		t.Error("rerun-last day-2 tx not committed")
	}
	// history label is rerun-last, applied to the last failed day-2 scenario.
	if tx.execArgs[0][2] != rerunLastScenarioLabel {
		t.Errorf("history scenario = %v, want %q", tx.execArgs[0][2], rerunLastScenarioLabel)
	}
}

// TestUnlockForRerun_Day2_RecipeNull_FailClosed — day-2, but the recipe is
// missing (recipe IS NULL / apply_run purged → ErrNoRows): fail-closed with
// ErrRerunInputUnavailable, transaction not committed (no silent
// bootstrap-input).
func TestUnlockForRerun_Day2_RecipeNull_FailClosed(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		queryRows: []scriptedRow{
			{values: []any{[]byte(`{"primary":"redis-01"}`), "error_locked", "create", []byte(`{"input":{"version":"8.6.1"}}`)}},
			{values: []any{"add_user", "01HFAILEDRUN0000000000000F"}},
			{err: pgx.ErrNoRows}, // recipe-probe: no row (WHERE recipe IS NOT NULL)
		},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UnlockForRerun(context.Background(), pool, "redis-prod", "x", "archon-alice", "01HRERUNHIST000000000000F", "01HRERUN00000000000000000F")
	if !errors.Is(err, ErrRerunInputUnavailable) {
		t.Fatalf("err = %v, want ErrRerunInputUnavailable (recipe unavailable)", err)
	}
	if tx.committed {
		t.Error("tx committed (fail-closed: rejection without mutation)")
	}
	if tx.execN != 0 {
		t.Errorf("Exec = %d, want 0 (rejection BEFORE mutation)", tx.execN)
	}
}

// TestUnlockForRerun_Day2_BareIncarnation — bare incarnation (created_scenario
// IS NULL) locked by a day-2 scenario: rerun-last works via the recipe path
// (created==nil → day-2), Scenario=="add_user", Input from recipe.
func TestUnlockForRerun_Day2_BareIncarnation(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		queryRows: []scriptedRow{
			{values: []any{[]byte(`{}`), "error_locked", nil, []byte(`{}`)}}, // created_scenario = NULL
			{values: []any{"add_user", "01HFAILEDRUN0000000000000B"}},
			{values: []any{[]byte(`{"input":{"user":"bob"}}`)}},
		},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := UnlockForRerun(context.Background(), pool, "redis-bare", "rerun bare day-2", "archon-alice", "01HRERUNHIST00000000000B0", "01HRERUN0000000000000000B0")
	if err != nil {
		t.Fatalf("UnlockForRerun bare day-2: %v", err)
	}
	if res.Scenario != "add_user" {
		t.Errorf("Scenario = %q, want add_user", res.Scenario)
	}
	if res.Input == nil || res.Input["user"] != "bob" {
		t.Errorf("UnlockResult.Input = %v, want {user:bob} (recipe.input)", res.Input)
	}
	if !tx.committed {
		t.Error("rerun-last bare day-2 tx not committed")
	}
}

// TestUnlockForRerun_CustomCreateScenario — incarnation was CREATED via
// `create_cluster`, last failure = `create_cluster` → create path:
// Scenario=="create_cluster", input from spec.input (restart of the CREATING
// scenario with its own values).
func TestUnlockForRerun_CustomCreateScenario(t *testing.T) {
	const applyID = "01HRERUN00000000000000000C"
	tx := &fakeTx{
		execErrAt: -1,
		queryRows: []scriptedRow{
			{values: []any{[]byte(`{"shards":3}`), "error_locked", "create_cluster", []byte(`{"input":{"shards":3,"version":"8.6.1"}}`)}},
			{values: []any{"create_cluster", "01HFAILEDRUN0000000000000C"}},
		},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := UnlockForRerun(context.Background(), pool, "redis-prod", "rerun cluster", "archon-alice", "01HRERUNHIST000000000000C", applyID)
	if err != nil {
		t.Fatalf("UnlockForRerun created_scenario=create_cluster: %v", err)
	}
	if res.Scenario != "create_cluster" {
		t.Errorf("Scenario = %q, want create_cluster (restart of the CREATING scenario)", res.Scenario)
	}
	if res.Input == nil {
		t.Fatal("UnlockResult.Input = nil - spec.input cluster NOT read")
	}
	if shards, ok := res.Input["shards"].(float64); !ok || shards != 3 {
		t.Errorf("UnlockResult.Input[shards] = %v (%T), want 3", res.Input["shards"], res.Input["shards"])
	}
	if !tx.committed {
		t.Error("rerun-last tx not committed for a valid custom create scenario")
	}
}

// TestUnlockForRerun_NoStateHistory_FailClosed — error_locked without a
// single state_history snapshot (unreachable in normal operation) →
// fail-closed with ErrRerunInputUnavailable, transaction not committed.
func TestUnlockForRerun_NoStateHistory_FailClosed(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		queryRows: []scriptedRow{
			{values: []any{[]byte(`{}`), "error_locked", "create", []byte("{}")}},
			{err: pgx.ErrNoRows}, // last-run probe: no trace
		},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UnlockForRerun(context.Background(), pool, "redis-prod", "x", "archon-alice", "01HRERUNHIST00000000000G0", "01HRERUN0000000000000000G0")
	if !errors.Is(err, ErrRerunInputUnavailable) {
		t.Fatalf("err = %v, want ErrRerunInputUnavailable (no snapshot)", err)
	}
	if tx.committed {
		t.Error("tx committed (fail-closed without a snapshot)")
	}
}

// TestUnlockForRerun_NotFound — nonexistent incarnation → ErrIncarnationNotFound.
func TestUnlockForRerun_NotFound(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{err: pgx.ErrNoRows},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UnlockForRerun(context.Background(), pool, "ghost", "x", "archon-alice", "01HRERUNHIST000000000000C", "01HRERUN00000000000000000C")
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Fatalf("err = %v, want ErrIncarnationNotFound", err)
	}
	if tx.committed {
		t.Error("tx committed for missing incarnation")
	}
}

// TestUnlockForRerun_EmptyReason — empty reason is rejected BEFORE the
// transaction (explicit operator confirmation required).
func TestUnlockForRerun_EmptyReason(t *testing.T) {
	pool := &fakePool{txs: []*fakeTx{{execErrAt: -1}}}
	_, err := UnlockForRerun(context.Background(), pool, "redis-prod", "", "archon-alice", "01HRERUNHIST000000000000D", "01HRERUN00000000000000000D")
	if err == nil {
		t.Fatal("empty reason accepted, want error")
	}
}

func TestUnlock_FromDestroyFailed(t *testing.T) {
	// S-D2a: destroy_failed is cleared the same way as error_locked/migration_failed —
	// state unchanged, status → ready.
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{"primary":"redis-01"}`), "destroy_failed"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := Unlock(context.Background(), pool, "redis-prod", "operator canceled destroy", "archon-alice", "01HUNLOCK0000000000000020")
	if err != nil {
		t.Fatalf("Unlock from destroy_failed: %v", err)
	}
	if res.PreviousStatus != StatusDestroyFailed {
		t.Errorf("PreviousStatus = %q, want destroy_failed", res.PreviousStatus)
	}
	if !tx.committed {
		t.Error("unlock tx not committed")
	}
	if tx.execN != 2 {
		t.Fatalf("Exec = %d, want 2 (history + update)", tx.execN)
	}
	if tx.execArgs[1][1] != string(StatusReady) {
		t.Errorf("UPDATE status arg = %v, want ready", tx.execArgs[1][1])
	}
}

func TestUnlock_FromDestroying_Rejected(t *testing.T) {
	// destroying is not unlockable: teardown is in progress, not locked by a failure.
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{}`), "destroying"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := Unlock(context.Background(), pool, "redis-prod", "x", "archon-alice", "01HUNLOCK0000000000000021")
	if !errors.Is(err, ErrIncarnationNotLocked) {
		t.Fatalf("err = %v, want ErrIncarnationNotLocked", err)
	}
	if tx.execN != 0 {
		t.Errorf("Exec = %d, want 0 (rejected before write)", tx.execN)
	}
}

func TestUnlock_FromReady_Rejected(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{}`), "ready"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := Unlock(context.Background(), pool, "redis-prod", "x", "archon-alice", "01HUNLOCK0000000000000012")
	if !errors.Is(err, ErrIncarnationNotLocked) {
		t.Fatalf("err = %v, want ErrIncarnationNotLocked", err)
	}
	if tx.execN != 0 {
		t.Errorf("Exec = %d, want 0 (rejected before write)", tx.execN)
	}
}

func TestUnlock_FromApplying_Rejected(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{}`), "applying"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := Unlock(context.Background(), pool, "redis-prod", "x", "archon-alice", "01HUNLOCK0000000000000013")
	if !errors.Is(err, ErrIncarnationNotLocked) {
		t.Fatalf("err = %v, want ErrIncarnationNotLocked", err)
	}
}

// --- upgrade found branch (ADR-0068 §5/B) ------------------------------

// TestUpgradeStateSchema_FoundModeApplyingRunHistory — found: UpgradeSlug + R
// set → final status applying (NOT drift), transition entry under R with
// scenario=slug (auto-run linkage), migration steps stay under M.
func TestUpgradeStateSchema_FoundModeApplyingRunHistory(t *testing.T) {
	const migM, runR = "01HUPGRADEMIGR00000000000M", "01HUPGRADERUN000000000000R"
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{"v":1}`), 1, "ready"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
		Name:             "redis-prod",
		TargetServiceVer: "v2.0.0",
		TargetSchemaVer:  2,
		Chain:            statemigrate.Chain{setStep(1, 2)},
		Evaluator:        newEvaluator(t),
		ApplyID:          migM,
		UpgradeSlug:      "to_v2",
		RunApplyID:       runR,
	})
	if err != nil {
		t.Fatalf("UpgradeStateSchema found: %v", err)
	}
	if !tx.committed {
		t.Fatal("found upgrade tx not committed")
	}
	if got := upgradeUPDATEStatus(t, tx); got != string(StatusApplying) {
		t.Errorf("final status = %q, want applying (found -> to the Runner, ADR-0068)", got)
	}
	var sawRunHistory, sawDriftHistory bool
	for i, sql := range tx.execSQLs {
		if !strings.Contains(sql, "INSERT INTO state_history") {
			continue
		}
		switch tx.execArgs[i][2] {
		case "to_v2":
			// run/drift-history INSERT: 6 args (state_before==state_after=$4),
			// apply_id at index 5 (migration-step has 7 args → index 6).
			sawRunHistory = true
			if tx.execArgs[i][5] != runR {
				t.Errorf("run-history apply_id = %v, want R=%s", tx.execArgs[i][5], runR)
			}
		case upgradeDriftScenarioLabel:
			sawDriftHistory = true
		case migrationScenarioLabel:
			if tx.execArgs[i][6] != migM {
				t.Errorf("migration-step apply_id = %v, want M=%s", tx.execArgs[i][6], migM)
			}
		}
	}
	if !sawRunHistory {
		t.Error("no linkage record under R (scenario=slug) - found did not write run-history")
	}
	if sawDriftHistory {
		t.Error("found wrote drift-pending-apply - should write run-history under R, not drift")
	}
}

// TestUpgradeStateSchema_SlugWithoutRunApplyID_Legacy — ANTI-STRANDING
// (ADR-0068 §5/B): slug found, but RunApplyID is empty (caller has no
// auto-run, e.g. MCP) → legacy drift, NOT applying. Otherwise the incarnation
// would get stuck in applying with no run.
func TestUpgradeStateSchema_SlugWithoutRunApplyID_Legacy(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{"v":1}`), 1, "ready"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UpgradeStateSchema(context.Background(), pool, UpgradeInput{
		Name:             "redis-prod",
		TargetServiceVer: "v2.0.0",
		TargetSchemaVer:  2,
		Chain:            statemigrate.Chain{setStep(1, 2)},
		Evaluator:        newEvaluator(t),
		ApplyID:          "01HUPGRADEMIGR00000000000N",
		UpgradeSlug:      "to_v2", // slug present, R empty → caller has no auto-run
	})
	if err != nil {
		t.Fatalf("UpgradeStateSchema: %v", err)
	}
	if got := upgradeUPDATEStatus(t, tx); got != string(StatusDrift) {
		t.Errorf("status = %q, want drift (slug without R = legacy, anti-stranding)", got)
	}
	var sawDrift bool
	for i, sql := range tx.execSQLs {
		if strings.Contains(sql, "INSERT INTO state_history") && tx.execArgs[i][2] == upgradeDriftScenarioLabel {
			sawDrift = true
		}
	}
	if !sawDrift {
		t.Error("slug-without-R must write legacy drift-pending-apply under M")
	}
}

// TestUnlockForRerun_Day2_FromUpgradeRecipe — the failed day-2 run was an
// upgrade scenario (recipe.from_upgrade=true, ADR-0068): rerun-last returns
// FromUpgrade=true so RunSpec restarts it from upgrade/, not scenario/.
func TestUnlockForRerun_Day2_FromUpgradeRecipe(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		queryRows: []scriptedRow{
			{values: []any{[]byte(`{"v":2}`), "error_locked", "create", []byte(`{}`)}},
			{values: []any{"to_v2", "01HFAILEDUPGRADE0000000000"}},
			{values: []any{[]byte(`{"scenario_name":"to_v2","from_upgrade":true,"input":{}}`)}},
		},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := UnlockForRerun(context.Background(), pool, "redis-prod", "rerun upgrade", "archon-alice", "01HRERUNHIST0000000000UPG", "01HRERUN000000000000000UPG")
	if err != nil {
		t.Fatalf("UnlockForRerun day-2 upgrade: %v", err)
	}
	if !res.FromUpgrade {
		t.Error("UnlockResult.FromUpgrade = false, want true (recipe.from_upgrade -> rerun from upgrade/)")
	}
	if res.Scenario != "to_v2" {
		t.Errorf("Scenario = %q, want to_v2", res.Scenario)
	}
}
