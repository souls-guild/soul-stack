package voyage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// voyageTargetsTable / voyageTargetsColumns — таблица и порядок колонок для
// CopyFrom-вставки единиц прогона. Совпадают с прежним per-row INSERT (тот же
// набор и порядок); serial-PK в набор не входит — target_id составной из
// известных данных, CopyFrom не возвращает строки и они не нужны.
var (
	voyageTargetsTable   = pgx.Identifier{"voyage_targets"}
	voyageTargetsColumns = []string{"voyage_id", "target_kind", "target_id", "batch_index", "status"}
)

// InsertTargets вставляет единицы прогона (Leg-разбиение) одного Voyage в
// статусе [TargetStatusAwaiting]. Вызывается при создании Voyage (S5-handler /
// orchestrator pre-plan) — снапшот целей фиксируется сразу за INSERT-ом самого
// `voyages`-row (ADR-043: snapshot-scope не «дрожит» между Leg-ами).
//
// Все targets обязаны ссылаться на тот же voyageID, иметь валидный TargetKind и
// непустой TargetID. Caller обязан передавать db = pgx.Tx, если нужна атомарность
// с Insert самого Voyage (CRUD не открывает транзакцию сам).
//
// Вставка идёт одним COPY (pgx CopyFrom), а не циклом per-row INSERT (S-med-3):
// scope Voyage может достигать [config.DefaultVoyageMaxScope] единиц, и N
// отдельных round-trip-ов в одной транзакции упёрлись бы в INSERT-rate.
// Атомарность сохранена — CopyFrom идёт через ту же tx, что и Insert самого
// Voyage. Валидация (тот же voyageID / валидный TargetKind / непустой TargetID /
// неотрицательный BatchIndex) проходит ДО COPY: невалидный target → error без
// записи.
func InsertTargets(ctx context.Context, db ExecQueryRower, voyageID string, targets []VoyageTarget) error {
	if voyageID == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if len(targets) == 0 {
		return fmt.Errorf("voyage: empty targets")
	}
	for i := range targets {
		t := &targets[i]
		if t.VoyageID != "" && t.VoyageID != voyageID {
			return fmt.Errorf("voyage: target[%d] voyage_id %q != %q", i, t.VoyageID, voyageID)
		}
		if !ValidTargetKind(t.TargetKind) {
			return fmt.Errorf("voyage: target[%d] invalid target_kind %q", i, t.TargetKind)
		}
		if t.TargetID == "" {
			return fmt.Errorf("voyage: target[%d] empty target_id", i)
		}
		if t.BatchIndex < 0 {
			return fmt.Errorf("voyage: target[%d] negative batch_index %d", i, t.BatchIndex)
		}
	}

	src := pgx.CopyFromSlice(len(targets), func(i int) ([]any, error) {
		t := &targets[i]
		status := t.Status
		if status == "" {
			status = TargetStatusAwaiting
		}
		return []any{
			voyageID,
			string(t.TargetKind),
			t.TargetID,
			t.BatchIndex,
			string(status),
		}, nil
	})

	if _, err := db.CopyFrom(ctx, voyageTargetsTable, voyageTargetsColumns, src); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case pgErrCodeUniqueViolation:
				return fmt.Errorf("voyage: duplicate target on %s: %w", pgErr.ConstraintName, err)
			case pgErrCodeForeignKeyViolation:
				return fmt.Errorf("voyage: target FK violation on %s: %w", pgErr.ConstraintName, err)
			case pgErrCodeCheckViolation:
				return fmt.Errorf("voyage: target CHECK violation on %s: %w", pgErr.ConstraintName, err)
			}
		}
		return fmt.Errorf("voyage: copy targets: %w", err)
	}
	return nil
}

const selectTargetsSQL = `
SELECT voyage_id, target_kind, target_id, batch_index, status, apply_id, errand_id, finished_at
FROM voyage_targets
WHERE voyage_id = $1
ORDER BY batch_index ASC, target_kind ASC, target_id ASC
`

// SelectTargets читает все единицы прогона Voyage по voyage_id, отсортированные
// по (batch_index, target_kind, target_id) — порядок two-level drill для
// All-runs вида (S5). Пустой результат — не ошибка (Voyage без targets либо чужой
// id; вызывающий проверяет существование самого Voyage через SelectByID).
func SelectTargets(ctx context.Context, db ExecQueryRower, voyageID string) ([]VoyageTarget, error) {
	if voyageID == "" {
		return nil, fmt.Errorf("voyage: empty voyage_id")
	}
	rows, err := db.Query(ctx, selectTargetsSQL, voyageID)
	if err != nil {
		return nil, fmt.Errorf("voyage: select targets: %w", err)
	}
	defer rows.Close()

	var out []VoyageTarget
	for rows.Next() {
		t, scanErr := scanTarget(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("voyage: select targets scan: %w", scanErr)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("voyage: select targets iter: %w", err)
	}
	return out, nil
}

// updateTargetRunningApplySQL — back-link на дочерний scenario-run
// (kind=incarnation, S2): пишем apply_id. JOIN на voyages.attempt фенсит CAS по
// epoch захвата (см. [MarkTargetRunning]).
const updateTargetRunningApplySQL = `
UPDATE voyage_targets AS vt
SET status   = 'running',
    apply_id = $4
FROM voyages AS v
WHERE vt.voyage_id   = $1
  AND vt.target_kind = $2
  AND vt.target_id   = $3
  AND vt.status       = 'awaiting'
  AND v.voyage_id     = vt.voyage_id
  AND v.attempt       = $5
`

// updateTargetRunningErrandSQL — back-link на дочерний Errand (kind=sid, S3):
// пишем errand_id. Колонка отличается от scenario-варианта, потому voyage_targets
// несёт отдельные nullable apply_id / errand_id (миграция 059): один и тот же
// перевод awaiting→running, но другой back-link-столбец по target_kind. JOIN на
// voyages.attempt фенсит CAS по epoch захвата (см. [MarkTargetRunning]).
const updateTargetRunningErrandSQL = `
UPDATE voyage_targets AS vt
SET status    = 'running',
    errand_id = $4
FROM voyages AS v
WHERE vt.voyage_id   = $1
  AND vt.target_kind = $2
  AND vt.target_id   = $3
  AND vt.status       = 'awaiting'
  AND v.voyage_id     = vt.voyage_id
  AND v.attempt       = $5
`

// MarkTargetRunning переводит единицу прогона awaiting→running и проставляет
// back-link на дочерний прогон. По target_kind выбирается столбец back-link-а:
//   - [TargetKindIncarnation] → apply_id (per-incarnation scenario-run, S2);
//   - [TargetKindSID]         → errand_id (per-host Errand, S3).
//
// backlinkID — applyID (scenario) либо errandID (command). attempt — claim-epoch
// воркера (voyages.attempt из ClaimNext). Вызывается оркестратором сразу после
// спавна дочернего прогона, ДО ожидания его терминала, чтобы All-runs-вид (S5)
// показывал «в работе» с корректным drill-ом.
//
// WHERE сужено до status='awaiting' + JOIN voyages.attempt=$attempt (idempotent +
// fencing guard, S-med-2): повторный вызов после failover-re-claim — no-op
// RowsAffected=0 (target уже running ЛИБО attempt сдвинулся при реклейме). Это
// детерминированно отличает «свой running» от осиротевшего: воркер прежнего
// claim-epoch (attempt=N) не перепишет running после реклейма (voyages.attempt=
// N+1). CRUD-ошибка PG поднимается caller-у; «строки нет» — caller трактует как
// уже-обработанную (recovery), не как fatal.
func MarkTargetRunning(ctx context.Context, db ExecQueryRower, voyageID string, kind TargetKind, targetID, backlinkID string, attempt int) error {
	if voyageID == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if !ValidTargetKind(kind) {
		return fmt.Errorf("voyage: invalid target_kind %q", kind)
	}
	if targetID == "" {
		return fmt.Errorf("voyage: empty target_id")
	}
	if backlinkID == "" {
		return fmt.Errorf("voyage: empty back-link id")
	}
	sql := updateTargetRunningApplySQL
	if kind == TargetKindSID {
		sql = updateTargetRunningErrandSQL
	}
	if _, err := db.Exec(ctx, sql, voyageID, string(kind), targetID, backlinkID, attempt); err != nil {
		return fmt.Errorf("voyage: mark target running (%s/%s): %w", kind, targetID, err)
	}
	return nil
}

const updateTargetTerminalSQL = `
UPDATE voyage_targets
SET status      = $4,
    finished_at = NOW()
WHERE voyage_id   = $1
  AND target_kind = $2
  AND target_id   = $3
  AND status NOT IN ('succeeded', 'failed', 'cancelled', 'no_match')
`

// MarkTargetTerminal фиксирует терминал единицы прогона (succeeded / failed /
// cancelled / no_match) + finished_at = NOW(). Вызывается оркестратором после
// достижения терминала дочернего прогона target-а.
//
// WHERE исключает уже-терминальные строки (idempotent guard, parity
// MarkTargetRunning): повторный финал после failover — no-op RowsAffected=0.
// status обязан быть терминальным TargetStatus; awaiting/running → [ErrInvalidStatus]
// (программная ошибка caller-а).
func MarkTargetTerminal(ctx context.Context, db ExecQueryRower, voyageID string, kind TargetKind, targetID string, status TargetStatus) error {
	if voyageID == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if !ValidTargetKind(kind) {
		return fmt.Errorf("voyage: invalid target_kind %q", kind)
	}
	if targetID == "" {
		return fmt.Errorf("voyage: empty target_id")
	}
	if !isTerminalTargetStatus(status) {
		return fmt.Errorf("%w: target status %q is not terminal", ErrInvalidStatus, status)
	}
	if _, err := db.Exec(ctx, updateTargetTerminalSQL, voyageID, string(kind), targetID, string(status)); err != nil {
		return fmt.Errorf("voyage: mark target terminal (%s/%s=%s): %w", kind, targetID, status, err)
	}
	return nil
}

// isTerminalTargetStatus — terminal-подмножество [TargetStatus] (succeeded /
// failed / cancelled / no_match). awaiting/running — не терминал.
func isTerminalTargetStatus(s TargetStatus) bool {
	switch s {
	case TargetStatusSucceeded, TargetStatusFailed, TargetStatusCancelled, TargetStatusNoMatch:
		return true
	}
	return false
}

func scanTarget(row pgx.Row) (*VoyageTarget, error) {
	var (
		t          VoyageTarget
		kindStr    string
		statusStr  string
		applyID    *string
		errandID   *string
		finishedAt *time.Time
	)
	if err := row.Scan(
		&t.VoyageID,
		&kindStr,
		&t.TargetID,
		&t.BatchIndex,
		&statusStr,
		&applyID,
		&errandID,
		&finishedAt,
	); err != nil {
		return nil, err
	}
	t.TargetKind = TargetKind(kindStr)
	t.Status = TargetStatus(statusStr)
	t.ApplyID = applyID
	t.ErrandID = errandID
	t.FinishedAt = finishedAt
	return &t, nil
}
