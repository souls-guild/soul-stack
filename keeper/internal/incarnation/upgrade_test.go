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
// Транзакционные операции (Unlock / UpgradeStateSchema) ходят через
// TxBeginner.BeginTx → pgx.Tx. fakeTx реализует pgx.Tx: Exec / QueryRow /
// Query + Commit / Rollback, остальные методы — заглушки (panic), т.к. в
// этих code-path-ах не вызываются.

// scriptedRow — QueryRow-ответ для FOR UPDATE: либо значения на Scan, либо
// ошибка (ErrNoRows для not-found).
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
	// FOR UPDATE-ответ (один на транзакцию upgrade/unlock).
	selectRow scriptedRow

	// queryRows — последовательность QueryRow-ответов по порядку вызовов (для
	// операций с несколькими QueryRow в одной tx, напр. UnlockForRerun: FOR UPDATE
	// + last-scenario probe). Если задан, consumed по индексу queryN; исчерпание
	// последовательности падает обратно на selectRow (single-QueryRow тесты не
	// меняются — у них queryRows nil).
	queryRows []scriptedRow
	queryN    int

	execErrAt int // индекс Exec-вызова, на котором вернуть execErr (-1 = никогда)
	execErr   error
	committed bool
	rolled    bool

	// execTags — CommandTag по индексу Exec-вызова (для проверки RowsAffected,
	// напр. single-winner DELETE с RowsAffected==0). nil / за пределами длины →
	// дефолт "UPDATE 1" (исходное поведение, существующие тесты не меняются).
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

// fakePool — TxBeginner-stub. Раздаёт заранее заготовленные транзакции по
// порядку BeginTx-вызовов (upgrade-tx, затем migration_failed-tx).
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

// newEvaluator — реальный migration-CEL движок (тонкая обёртка для тестов).
func newEvaluator(t *testing.T) statemigrate.Evaluator {
	t.Helper()
	ev, err := statemigrate.NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return ev
}

// setStep собирает миграцию from→to с одним set state.v = <to>.
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
		// scenario-метка = "migration".
		if tx.execArgs[i][2] != migrationScenarioLabel {
			t.Errorf("Exec[%d] scenario = %v, want %q", i, tx.execArgs[i][2], migrationScenarioLabel)
		}
		// общий apply_id на все шаги.
		if tx.execArgs[i][6] != "01HUPGRADE0000000000000000" {
			t.Errorf("Exec[%d] apply_id = %v, want shared ApplyID", i, tx.execArgs[i][6])
		}
	}
	// разные history_id на шаг.
	if tx.execArgs[0][0] == tx.execArgs[1][0] {
		t.Errorf("history_id not unique per step: %v", tx.execArgs[0][0])
	}

	// UPDATE: state мигрирован (v=3), schema=3, service_version=v3.0.0.
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
	// Финальный статус — drift, НЕ ready (ADR-031 amendment): хосты ждут
	// применения новой версии. status — литерал в SQL (не bind-arg), проверяем
	// текст UPDATE-statement-а.
	if !strings.Contains(tx.execSQLs[2], "status               = 'drift'") {
		t.Errorf("UPDATE statement = %q, want status='drift' (ADR-031 upgrade→drift)", tx.execSQLs[2])
	}

	// Drift-transition history (Exec[3]): scenario=upgrade-pending-apply,
	// zero-diff (state_before==state_after = пост-миграционный final state),
	// общий apply_id со step-снимками.
	if !strings.Contains(tx.execSQLs[3], "INSERT INTO state_history") {
		t.Fatalf("Exec[3] = %q, want drift-transition state_history INSERT", tx.execSQLs[3])
	}
	dh := tx.execArgs[3]
	if dh[2] != upgradeDriftScenarioLabel {
		t.Errorf("drift-history scenario = %v, want %q", dh[2], upgradeDriftScenarioLabel)
	}
	if string(dh[3].([]byte)) != string(up[1].([]byte)) {
		t.Errorf("drift-history state не равен пост-миграционному: %s vs %s", dh[3], up[1])
	}
	if dh[5] != "01HUPGRADE0000000000000000" {
		t.Errorf("drift-history apply_id = %v, want shared ApplyID", dh[5])
	}
}

// upgradeUPDATEStatus извлекает финальный статус из перехвата Exec-вызовов
// upgrade-tx: ищет UPDATE incarnation, ставящий статус, и возвращает значение
// литерала status='<x>'. Статус — литерал в SQL (не bind-arg), поэтому парсим
// текст statement-а. Пустая строка, если UPDATE incarnation не найден.
func upgradeUPDATEStatus(t *testing.T, tx *fakeTx) string {
	t.Helper()
	for _, sql := range tx.execSQLs {
		if !strings.Contains(sql, "UPDATE incarnation") || !strings.Contains(sql, "state_schema_version") {
			continue
		}
		const marker = "status               = '"
		i := strings.Index(sql, marker)
		if i < 0 {
			t.Fatalf("UPDATE incarnation без status-литерала: %q", sql)
		}
		rest := sql[i+len(marker):]
		j := strings.IndexByte(rest, '\'')
		if j < 0 {
			t.Fatalf("незакрытый status-литерал в UPDATE: %q", sql)
		}
		return rest[:j]
	}
	return ""
}

// TestUpgradeStateSchema_FinalStatusDrift — ПОВЕДЕНЧЕСКИЙ ИНВАРИАНТ (ADR-031
// amendment, ловит регресс): по успешному upgrade incarnation ОБЯЗАНА уйти в
// status=drift, НЕ ready. Дыра upgrade↔хосты: БД-state сменился, но хосты
// остались на старой раскатке — drift сигналит оператору «накати на хосты».
// Если будущая правка вернёт 'ready' в финальный UPDATE — тест краснеет. Кейсы:
// миграция из ready, миграция из drift (drift→drift, повторный upgrade), no-op
// ref-bump.
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
			// Стартовая schema-версия FOR UPDATE-строки: для миграции =
			// chain[0].From, для no-op = target (равна current).
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
				t.Errorf("финальный статус = %q, want drift (ADR-031: upgrade оставляет хосты позади БД-state)", got)
			}
			// Причина перехода зафиксирована в истории отдельной записью.
			var sawDriftHistory bool
			for i, sql := range tx.execSQLs {
				if strings.Contains(sql, "INSERT INTO state_history") && tx.execArgs[i][2] == upgradeDriftScenarioLabel {
					sawDriftHistory = true
				}
			}
			if !sawDriftHistory {
				t.Errorf("нет state_history-записи перехода в drift (scenario=%q)", upgradeDriftScenarioLabel)
			}
		})
	}
}

// TestUpgradeStateSchema_LockedStatusNotOverwritten — GUARD: блокирующие
// статусы (error_locked / migration_failed / applying) НЕ перетираются upgrade-ом
// в drift. Отказ до любой мутации (gate в upgradeTx): транзакция НЕ коммитится,
// ни одного Exec. Защищает инвариант «upgrade не сбрасывает блокировку молча» —
// error_locked/migration_failed требуют явного unlock, applying = идёт прогон.
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
			// Ни мутации, ни коммита: блокирующий статус сохранён as-is.
			if tx.committed {
				t.Errorf("status=%s: tx committed — блокирующий статус перетёрт", c.status)
			}
			if tx.execN != 0 {
				t.Errorf("status=%s: Exec = %d, want 0 (отказ ДО мутации, статус не тронут)", c.status, tx.execN)
			}
		})
	}
}

func TestUpgradeStateSchema_NoOpEmptyChain(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		// version=2, ref-bump без смены schema.
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
	// service_version всё равно обновлён.
	if tx.execArgs[1][3] != "v2.1.0" {
		t.Errorf("UPDATE service_version = %v, want v2.1.0", tx.execArgs[1][3])
	}
	// No-op ref-bump тоже уходит в drift (раскатка нового ref ещё не на хостах).
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
	// current=2, но chain[0].From=1 → кто-то проапгрейдил между resolve и lock.
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

// TestUpgradeStateSchema_ApplyError_MigrationFailed: provoке-фейл в write
// (Exec на INSERT history) → upgrade-tx rollback, отдельная background-tx
// помечает migration_failed; state НЕ менялся (rollback). Используем write-
// фейл вместо CEL-ошибки: статически собранная Chain валидна, проще и точнее
// проверить failure-handling по pool-у с двумя транзакциями.
func TestUpgradeStateSchema_WriteError_MigrationFailed(t *testing.T) {
	upTx := &fakeTx{
		// fail на первом Exec (INSERT state_history).
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
	// upgrade-tx откатился, не закоммитился.
	if upTx.committed {
		t.Error("upgrade tx committed despite write error")
	}
	if !upTx.rolled {
		t.Error("upgrade tx not rolled back")
	}
	// migration_failed background-tx: BeginTx вызвался дважды, second-tx commit.
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

// --- Unlock (unit, через fakeTx) -------------------------------------

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

// --- UnlockForRerun (unit, через fakeTx) ------------------------------

// TestUnlockForRerun_FromErrorLocked — допуск из error_locked: state не меняется
// (state_before == state_after), статус → applying (НЕ ready — race-free), под
// одним FOR UPDATE; snapshot в state_history с меткой rerun-create и общим
// apply_id; commit.
func TestUnlockForRerun_FromErrorLocked(t *testing.T) {
	const applyID = "01HRERUN00000000000000000A"
	tx := &fakeTx{
		execErrAt: -1,
		// QueryRow #1 — FOR UPDATE (state, status, created_scenario, spec); #2 —
		// last-scenario probe (scope=created-scenario: последний упавший = создавший
		// `create` → допуск). spec несёт оператор-input (B1: проброс в RunSpec.Input).
		queryRows: []scriptedRow{
			{values: []any{[]byte(`{"primary":"redis-01"}`), "error_locked", "create", []byte(`{"input":{"version":"8.6.1"}}`)}},
			{values: []any{"create"}},
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
	if res.CreatedScenario != "create" {
		t.Errorf("CreatedScenario = %q, want create", res.CreatedScenario)
	}
	// B1: stored spec.input возвращён в UnlockResult.Input (caller пробрасывает в
	// RunSpec.Input перезапускаемого bootstrap-прогона).
	if res.Input == nil {
		t.Fatal("UnlockResult.Input = nil — spec.input НЕ прочитан под FOR UPDATE (B1 регресс)")
	}
	if res.Input["version"] != "8.6.1" {
		t.Errorf("UnlockResult.Input[version] = %v, want 8.6.1 (stored spec.input)", res.Input["version"])
	}
	if !tx.committed {
		t.Error("rerun-create tx not committed")
	}
	// INSERT history + UPDATE status → applying (НЕ ready).
	if tx.execN != 2 {
		t.Fatalf("Exec = %d, want 2 (history + update)", tx.execN)
	}
	// history-INSERT: scenario=rerun-create, state_before==state_after ($4),
	// apply_id ($6) = переданный applyID.
	hist := tx.execArgs[0]
	if hist[2] != rerunCreateScenarioLabel {
		t.Errorf("history scenario = %v, want %q", hist[2], rerunCreateScenarioLabel)
	}
	if hist[5] != applyID {
		t.Errorf("history apply_id = %v, want %q", hist[5], applyID)
	}
	// UPDATE: статус → applying, НЕ ready (race-free, минуя ready).
	if got := tx.execArgs[1][1]; got != string(StatusApplying) {
		t.Errorf("UPDATE status arg = %v, want applying (минуя ready)", got)
	}
	if tx.execArgs[1][1] == string(StatusReady) {
		t.Error("rerun перевёл в ready — race-window не закрыт (должно быть applying)")
	}
}

// TestUnlockForRerun_RejectNonErrorLocked — допуск ЖЁСТКО из error_locked:
// ready / applying / migration_failed / destroy_failed → ErrIncarnationNotErrorLocked,
// транзакция НЕ коммитится (для них — обычный unlock + ручной run).
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
				t.Errorf("status=%s: tx committed (должен быть отказ без мутации)", status)
			}
			if tx.execN != 0 {
				t.Errorf("status=%s: Exec = %d, want 0 (отказ ДО мутации)", status, tx.execN)
			}
		})
	}
}

// TestUnlockForRerun_RejectNonCreateScenario — scope=create (GUARD): error_locked,
// но последний упавший сценарий — НЕ create (add_user) → ErrRerunScenarioNotCreate,
// транзакция НЕ коммитится. rerun-create перезапускает строго bootstrap `create`.
func TestUnlockForRerun_RejectNonCreateScenario(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		queryRows: []scriptedRow{
			{values: []any{[]byte(`{"primary":"redis-01"}`), "error_locked", "create", []byte("{}")}},
			{values: []any{"add_user"}},
		},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UnlockForRerun(context.Background(), pool, "redis-prod", "x", "archon-alice", "01HRERUNHIST000000000000E", "01HRERUN00000000000000000E")
	if !errors.Is(err, ErrRerunScenarioNotCreate) {
		t.Fatalf("err = %v, want ErrRerunScenarioNotCreate", err)
	}
	if tx.committed {
		t.Error("tx committed (должен быть отказ scope=create без мутации)")
	}
	if tx.execN != 0 {
		t.Errorf("Exec = %d, want 0 (отказ ДО мутации)", tx.execN)
	}
}

// TestUnlockForRerun_CustomCreateScenario — инкарнация СОЗДАНА `create_cluster`
// (incarnation.created_scenario), последний упавший = `create_cluster` → допуск
// + UnlockResult.CreatedScenario == "create_cluster" (механизм нескольких create:
// rerun перезапускает СОЗДАВШИЙ сценарий, не хардкод `create`). GUARD на пункт 4 ТЗ.
func TestUnlockForRerun_CustomCreateScenario(t *testing.T) {
	const applyID = "01HRERUN00000000000000000F"
	tx := &fakeTx{
		execErrAt: -1,
		queryRows: []scriptedRow{
			{values: []any{[]byte(`{"shards":3}`), "error_locked", "create_cluster", []byte(`{"input":{"shards":3,"version":"8.6.1"}}`)}},
			{values: []any{"create_cluster"}},
		},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := UnlockForRerun(context.Background(), pool, "redis-prod", "rerun cluster", "archon-alice", "01HRERUNHIST000000000000F", applyID)
	if err != nil {
		t.Fatalf("UnlockForRerun created_scenario=create_cluster: %v", err)
	}
	if res.CreatedScenario != "create_cluster" {
		t.Errorf("CreatedScenario = %q, want create_cluster (рестарт СОЗДАВШЕГО сценария)", res.CreatedScenario)
	}
	// B1: stored spec.input cluster-сценария проброшен (shards/version), а не дефолты.
	if res.Input == nil {
		t.Fatal("UnlockResult.Input = nil — spec.input cluster НЕ прочитан (B1 регресс)")
	}
	if shards, ok := res.Input["shards"].(float64); !ok || shards != 3 {
		t.Errorf("UnlockResult.Input[shards] = %v (%T), want 3", res.Input["shards"], res.Input["shards"])
	}
	if !tx.committed {
		t.Error("rerun-create tx not committed для валидного custom create-сценария")
	}
}

// TestUnlockForRerun_LastScenarioNotCreatedOne — инкарнация создана `create_cluster`,
// но последний упавший — day-2 `add_user` (тоже залочил) → ErrRerunScenarioNotCreate:
// rerun перезапускает СОЗДАВШИЙ bootstrap, а не любую упавшую операцию (guard сохранён
// при нескольких create).
func TestUnlockForRerun_LastScenarioNotCreatedOne(t *testing.T) {
	tx := &fakeTx{
		execErrAt: -1,
		queryRows: []scriptedRow{
			{values: []any{[]byte(`{"shards":3}`), "error_locked", "create_cluster", []byte("{}")}},
			{values: []any{"add_user"}},
		},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	_, err := UnlockForRerun(context.Background(), pool, "redis-prod", "x", "archon-alice", "01HRERUNHIST00000000000G", "01HRERUN0000000000000000G")
	if !errors.Is(err, ErrRerunScenarioNotCreate) {
		t.Fatalf("err = %v, want ErrRerunScenarioNotCreate (last=add_user ≠ created=create_cluster)", err)
	}
	if tx.committed {
		t.Error("tx committed (должен быть отказ без мутации)")
	}
}

// TestUnlockForRerun_NotFound — несуществующая incarnation → ErrIncarnationNotFound.
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

// TestUnlockForRerun_EmptyReason — пустой reason отвергается ДО транзакции
// (явное подтверждение оператора обязательно).
func TestUnlockForRerun_EmptyReason(t *testing.T) {
	pool := &fakePool{txs: []*fakeTx{{execErrAt: -1}}}
	_, err := UnlockForRerun(context.Background(), pool, "redis-prod", "", "archon-alice", "01HRERUNHIST000000000000D", "01HRERUN00000000000000000D")
	if err == nil {
		t.Fatal("empty reason accepted, want error")
	}
}

func TestUnlock_FromDestroyFailed(t *testing.T) {
	// S-D2a: destroy_failed снимается так же, как error_locked/migration_failed —
	// state не меняется, статус → ready.
	tx := &fakeTx{
		execErrAt: -1,
		selectRow: scriptedRow{values: []any{[]byte(`{"primary":"redis-01"}`), "destroy_failed"}},
	}
	pool := &fakePool{txs: []*fakeTx{tx}}

	res, err := Unlock(context.Background(), pool, "redis-prod", "оператор отменил destroy", "archon-alice", "01HUNLOCK0000000000000020")
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
	// destroying — не unlockable: идёт teardown, не залоченный фейлом.
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
