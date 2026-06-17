package applyrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/pgutil"
)

// Sentinel-ошибки CRUD-слоя.
//   - ErrApplyRunAlreadyExists — UNIQUE по composite PK (apply_id, sid):
//     повторный Insert той же пары (программная ошибка scenario-runner-а).
//   - ErrApplyRunNotFound      — нет строки по запрошенному ключу.
//   - ErrApplyRunNotClaimed    — [MarkDispatched] вызван на строке, которой
//     нет в статусе `claimed` (уже dispatched / planned / терминал либо отсутствует).
var (
	ErrApplyRunAlreadyExists = errors.New("applyrun: (apply_id, sid) already exists")
	ErrApplyRunNotFound      = errors.New("applyrun: not found")
	ErrApplyRunNotClaimed    = errors.New("applyrun: row is not in claimed state")
	// ErrApplyRunAlreadyTerminal — терминал append-only single-winner (ADR-027(j),
	// amend GATE-1): [UpdateStatus] вызван на строке, которая уже в терминальном
	// статусе (success/failed/cancelled). Терминал НЕ перезаписывается терминалом
	// другого исполнения (оригинальный RunResult vs recovery-перехват) — первый
	// победил. НЕ ошибка: caller трактует как no-op (логирует, не валит барьер).
	ErrApplyRunAlreadyTerminal = errors.New("applyrun: row is already in a terminal status")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество интерфейса pgxpool.Pool, нужное CRUD-у.
// Симметрично [incarnation.ExecQueryRower] / [operator.ExecQueryRower]:
// unit-тесты ходят через fake без подъёма PG, production даёт реальный
// pool / Conn / Tx.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check.
var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

const insertSQL = `
INSERT INTO apply_runs (
    apply_id, sid, incarnation_name, scenario, task_idx, status,
    error_summary, started_by_aid
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING started_at
`

// Insert вставляет строку прогона. [StatusRunning] здесь остался ТОЛЬКО для
// старого синхронного пути (dispatchWave при acolytes:0): он рендерит и шлёт
// `ApplyRequest` сразу, поэтому пишет строку как `running`. Acolyte-путь сюда
// не ходит — он пишет `planned` через [InsertPlanned], а затем переводит
// `claimed → dispatched` ([MarkDispatched]) перед SendApply.
//
// Pre-conditions: непустые ApplyID / SID / IncarnationName / Scenario;
// валидный Status.
//
// Возврат:
//   - [ErrApplyRunAlreadyExists] на UNIQUE по PK.
//   - wrapped fmt.Errorf на FK-violation (incarnation_name / started_by_aid
//     ссылаются на несуществующую строку) и CHECK-violation (status).
func Insert(ctx context.Context, db ExecQueryRower, run *ApplyRun) error {
	if run == nil {
		return fmt.Errorf("applyrun: nil run")
	}
	if run.ApplyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if run.SID == "" {
		return fmt.Errorf("applyrun: empty sid")
	}
	if run.IncarnationName == "" {
		return fmt.Errorf("applyrun: empty incarnation_name")
	}
	if run.Scenario == "" {
		return fmt.Errorf("applyrun: empty scenario")
	}
	if !ValidStatus(run.Status) {
		return fmt.Errorf("applyrun: invalid status %q", run.Status)
	}

	var taskIdxArg any
	if run.TaskIdx != nil {
		taskIdxArg = *run.TaskIdx
	}
	var errorSummaryArg any
	if run.ErrorSummary != nil {
		errorSummaryArg = *run.ErrorSummary
	}
	var startedByArg any
	if run.StartedByAID != nil {
		startedByArg = *run.StartedByAID
	}

	row := db.QueryRow(ctx, insertSQL,
		run.ApplyID,
		run.SID,
		run.IncarnationName,
		run.Scenario,
		taskIdxArg,
		string(run.Status),
		errorSummaryArg,
		startedByArg,
	)
	if err := row.Scan(&run.StartedAt); err != nil {
		return mapInsertError(err)
	}
	return nil
}

const insertPlannedSQL = `
INSERT INTO apply_runs (
    apply_id, sid, incarnation_name, scenario, status, started_by_aid, recipe
) VALUES ($1, $2, $3, $4, 'planned', $5, $6)
RETURNING started_at
`

// InsertPlanned вставляет planned-задание под Acolyte-claim (ADR-027, Phase
// 1.4.2): строка в статусе `planned` с persisted [Recipe] (render-инструкция,
// колонка recipe миграции 029). attempt остаётся DEFAULT 0 — fencing-epoch
// инкрементит [ClaimNext] при захвате. task_idx/error_summary не пишутся (нет
// задачи на dispatch-е), Ward-claim-колонки — NULL до claim.
//
// Отличие от [Insert]: тот пишет `running` сразу (старый путь, dispatch
// рендерит и шлёт ApplyRequest синхронно); InsertPlanned пишет `planned` —
// рендер/SendApply отложены на Acolyte при claim. Инвариант A (ADR-027): recipe
// несёт vault-ref КАК ЕСТЬ, секреты в PG не оседают.
//
// Pre-conditions: непустые ApplyID / SID / IncarnationName / Scenario;
// non-nil run.Recipe (planned-задание без рецепта Acolyte не отрендерит).
//
// Возврат:
//   - [ErrApplyRunAlreadyExists] на UNIQUE по PK.
//   - wrapped fmt.Errorf на FK-violation / recipe-marshal-фейл.
func InsertPlanned(ctx context.Context, db ExecQueryRower, run *ApplyRun) error {
	if run == nil {
		return fmt.Errorf("applyrun: nil run")
	}
	if run.ApplyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if run.SID == "" {
		return fmt.Errorf("applyrun: empty sid")
	}
	if run.IncarnationName == "" {
		return fmt.Errorf("applyrun: empty incarnation_name")
	}
	if run.Scenario == "" {
		return fmt.Errorf("applyrun: empty scenario")
	}
	if run.Recipe == nil {
		return fmt.Errorf("applyrun: planned-задание без recipe")
	}

	recipeJSON, err := MarshalRecipe(run.Recipe)
	if err != nil {
		return err
	}

	var startedByArg any
	if run.StartedByAID != nil {
		startedByArg = *run.StartedByAID
	}

	row := db.QueryRow(ctx, insertPlannedSQL,
		run.ApplyID,
		run.SID,
		run.IncarnationName,
		run.Scenario,
		startedByArg,
		recipeJSON,
	)
	if err := row.Scan(&run.StartedAt); err != nil {
		return mapInsertError(err)
	}
	run.Status = StatusPlanned
	return nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w",
				ErrApplyRunAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("applyrun: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("applyrun: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("applyrun: insert: %w", err)
}

// updateStatusSQL переводит строку в терминальный (или иной) статус с
// append-only single-winner guard (ADR-027(j), amend GATE-1): фильтр исходного
// статуса `status IN ('planned','claimed','running','dispatched')` гарантирует,
// что терминал НЕ перезаписывается терминалом другого исполнения — переход в
// терминал возможен ТОЛЬКО из не-терминального состояния. `dispatched` входит в
// guard (ADR-027 amend S3): после MarkDispatched строка к приходу RunResult
// именно dispatched, и терминал обязан коммититься из неё (dispatched →
// success/failed/cancelled). Уже-терминальная строка → RowsAffected==0 (первый
// победил), [UpdateStatus] различает это от not-found добором статуса.
//
// `finished_at = NOW()` проставляется при переходе в любой статус, кроме
// `running` (терминал фиксируется фактическим временем завершения); для
// `running` остаётся NULL. error_summary — COALESCE: NULL-аргумент не
// затирает уже записанное значение.
const updateStatusSQL = `
UPDATE apply_runs
SET status        = $3,
    error_summary = COALESCE($4, error_summary),
    finished_at   = CASE WHEN $3 = 'running' THEN finished_at ELSE NOW() END
WHERE apply_id = $1 AND sid = $2
  AND status IN ('planned', 'claimed', 'running', 'dispatched')
`

const probeStatusSQL = `
SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2
`

// UpdateStatus переводит строку `(applyID, sid)` в новый status. На
// терминальном статусе (всё кроме [StatusRunning]) проставляет
// `finished_at = NOW()`. errorSummary != nil → пишется в error_summary
// (nil не затирает существующее).
//
// Append-only single-winner (ADR-027(j)): переход разрешён ТОЛЬКО из
// не-терминального исходного статуса (planned/claimed/running/dispatched).
// Терминал не перезаписывается терминалом — защита от гонки
// оригинальный-RunResult vs recovery-перехват ([correlateRunResult] /
// barrier-классификатор).
//
// Возврат:
//   - [ErrApplyRunNotFound]        — строки нет вовсе.
//   - [ErrApplyRunAlreadyTerminal] — строка есть, но уже терминальна (первый
//     коммиттер победил): caller трактует как no-op (логирует, не валит
//     барьер), НЕ ошибка консистентности.
func UpdateStatus(ctx context.Context, db ExecQueryRower, applyID, sid string, status Status, errorSummary *string) error {
	if applyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if sid == "" {
		return fmt.Errorf("applyrun: empty sid")
	}
	if !ValidStatus(status) {
		return fmt.Errorf("applyrun: invalid status %q", status)
	}

	var errorSummaryArg any
	if errorSummary != nil {
		errorSummaryArg = *errorSummary
	}

	tag, err := db.Exec(ctx, updateStatusSQL, applyID, sid, string(status), errorSummaryArg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeCheckViolation {
			return fmt.Errorf("applyrun: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("applyrun: update status: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// 0 строк: либо строки нет, либо она уже терминальна (append-only guard
	// отсёк перезапись). Различаем добором статуса (паттерн MarkDispatched).
	var statusStr string
	if perr := db.QueryRow(ctx, probeStatusSQL, applyID, sid).Scan(&statusStr); perr != nil {
		if errors.Is(perr, pgx.ErrNoRows) {
			return ErrApplyRunNotFound
		}
		return fmt.Errorf("applyrun: update status probe: %w", perr)
	}
	return fmt.Errorf("%w (status=%s)", ErrApplyRunAlreadyTerminal, statusStr)
}

const selectByApplyIDSQL = `
SELECT apply_id, sid, incarnation_name, scenario, task_idx, status,
       error_summary, started_at, finished_at, started_by_aid,
       claim_by_kid, claim_at, claim_expires_at, attempt, recipe
FROM apply_runs
WHERE apply_id = $1 AND sid = $2
`

// SelectByApplyID читает строку прогона по composite PK, включая Ward-claim
// колонки (claim_by_kid/claim_at/claim_expires_at/attempt, миграция 025) и
// recipe (миграция 029). [ErrApplyRunNotFound] при pgx.ErrNoRows.
func SelectByApplyID(ctx context.Context, db ExecQueryRower, applyID, sid string) (*ApplyRun, error) {
	row := db.QueryRow(ctx, selectByApplyIDSQL, applyID, sid)
	var (
		run          ApplyRun
		statusStr    string
		taskIdx      *int
		errorSummary *string
		startedBy    *string
		recipeJSON   []byte
	)
	err := row.Scan(
		&run.ApplyID,
		&run.SID,
		&run.IncarnationName,
		&run.Scenario,
		&taskIdx,
		&statusStr,
		&errorSummary,
		&run.StartedAt,
		&run.FinishedAt,
		&startedBy,
		&run.ClaimByKID,
		&run.ClaimAt,
		&run.ClaimExpiresAt,
		&run.Attempt,
		&recipeJSON,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrApplyRunNotFound
		}
		return nil, fmt.Errorf("applyrun: scan: %w", err)
	}
	run.Status = Status(statusStr)
	run.TaskIdx = taskIdx
	run.ErrorSummary = errorSummary
	run.StartedByAID = startedBy
	recipe, err := UnmarshalRecipe(recipeJSON)
	if err != nil {
		return nil, err
	}
	run.Recipe = recipe
	return &run, nil
}

// recordTaskFailureSQL фиксирует данные первой упавшей задачи хоста
// (BUG-3): task_idx и error_summary пишутся только если ещё не заполнены
// (COALESCE с уже хранимым значением → first-failure-wins). Статус строки
// не трогаем — терминал проставит RunResult ([UpdateStatus]); до него строка
// остаётся `running`, но уже несёт причину падения для последующей агрегации.
const recordTaskFailureSQL = `
UPDATE apply_runs
SET task_idx      = COALESCE(task_idx, $3),
    error_summary = COALESCE(error_summary, $4)
WHERE apply_id = $1 AND sid = $2
`

// RecordTaskFailure записывает индекс и краткое описание первой упавшей
// задачи хоста в строку `(applyID, sid)`. Идемпотентна по first-failure-wins:
// повторный вызов (вторая упавшая задача / retry) не затирает уже записанные
// task_idx/error_summary (COALESCE). Вызывается RunResult-pipeline-ом из
// handleTaskEvent при TaskEvent со status FAILED/TIMED_OUT — так причина
// падения переживает cross-Keeper-роутинг (TaskEvent и run-goroutine могут
// быть на разных инстансах, ADR-002) до агрегации в error_summary прогона.
//
// summary — уже скомпонованная и пропущенная через MaskSecrets строка
// (`task <idx> <module>: <message>`); CRUD-слой её не интерпретирует.
//
// Возврат [ErrApplyRunNotFound], если строки нет (TaskEvent опередил Insert
// либо ad-hoc push без scenario-runner-а).
func RecordTaskFailure(ctx context.Context, db ExecQueryRower, applyID, sid string, taskIdx int, summary string) error {
	if applyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if sid == "" {
		return fmt.Errorf("applyrun: empty sid")
	}
	if taskIdx < 0 {
		return fmt.Errorf("applyrun: negative task_idx %d", taskIdx)
	}

	tag, err := db.Exec(ctx, recordTaskFailureSQL, applyID, sid, taskIdx, summary)
	if err != nil {
		return fmt.Errorf("applyrun: record task failure: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrApplyRunNotFound
	}
	return nil
}

const selectStatusesByApplyIDSQL = `
SELECT sid, status, task_idx, error_summary, cancel_requested
FROM apply_runs
WHERE apply_id = $1
ORDER BY sid ASC
`

// HostStatus — узкая проекция строки apply_runs для fan-in-опроса
// scenario-runner-ом: scenario-runner поллит [SelectStatusesByApplyID]
// до тех пор, пока статусы всех SID-ов прогона не станут терминальными.
//
// TaskIdx — индекс упавшей задачи (заполнен [RecordTaskFailure] на failed-
// хосте; nil на success / ещё-running / dispatch-level фейле без TaskEvent-а).
//
// CancelRequested — флаг cluster-wide Cancel (G1, миграция 024): любой Keeper
// при Cancel ставит его через [RequestCancel]; run-goroutine-владелец видит
// его на ближайшем тике барьера и отменяет прогон. Флаг одинаков для всех
// строк прогона (RequestCancel пишет по apply_id), но проекция несёт его
// per-host — barrier-у достаточно увидеть true на любой строке.
type HostStatus struct {
	SID             string
	Status          Status
	TaskIdx         *int
	ErrorSummary    *string
	CancelRequested bool
}

// SelectStatusesByApplyID возвращает статусы всех хостов одного прогона
// (один `apply_id`, разные `sid`), отсортированные по SID. Используется
// scenario-runner-ом для cross-host barrier-fan-in (poll до терминала всех
// хостов, orchestration.md §7). Индекс `apply_runs_apply_idx` (миграция 018)
// покрывает запрос. Несёт `cancel_requested` (G1) тем же запросом — barrier
// читает флаг отмены без второго round-trip-а.
//
// Пустой результат — apply без единой строки (программная ошибка
// scenario-runner-а: poll до Insert-а); caller трактует как «ещё ничего не
// диспатчено».
func SelectStatusesByApplyID(ctx context.Context, db ExecQueryRower, applyID string) ([]HostStatus, error) {
	rows, err := db.Query(ctx, selectStatusesByApplyIDSQL, applyID)
	if err != nil {
		return nil, fmt.Errorf("applyrun: statuses query: %w", err)
	}
	defer rows.Close()

	var out []HostStatus
	for rows.Next() {
		var (
			hs        HostStatus
			statusStr string
		)
		if err := rows.Scan(&hs.SID, &statusStr, &hs.TaskIdx, &hs.ErrorSummary, &hs.CancelRequested); err != nil {
			return nil, fmt.Errorf("applyrun: statuses scan: %w", err)
		}
		hs.Status = Status(statusStr)
		out = append(out, hs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: statuses iter: %w", err)
	}
	return out, nil
}

const requestCancelSQL = `
UPDATE apply_runs
SET cancel_requested = true
WHERE apply_id = $1 AND status IN ('planned', 'claimed', 'running')
`

// RequestCancel ставит флаг cluster-wide Cancel (G1) на ВСЕ ещё-нетерминальные
// строки прогона `apply_id` (planned/claimed/running). Любой Keeper-инстанс
// может вызвать её; инстанс, держащий run-goroutine, увидит флаг в
// barrier-поллинге ([SelectStatusesByApplyID]) и отменит прогон через тот же
// путь, что и локальный Cancel.
//
// Фильтр включает planned/claimed (ADR-027 cutover, minor-фикс «Cancel в
// planned/claimed-окне»): отмена ДО SendApply безопасна — Acolyte проверяет флаг
// перед отправкой ApplyRequest ([ClaimRunner.execute] → [SelectCancelRequested])
// и не шлёт apply, если Cancel запрошен. Без этого Cancel в planned-окне (между
// dispatch-ем и claim-ом) не трогал бы ни одной строки и прогон уезжал бы на
// Soul-ы.
//
// Идемпотентна: терминальные строки (success/failed/cancelled) исключены —
// Cancel уже завершённого прогона не трогает ни одной строки. Повторный вызов на
// ещё-нетерминальном прогоне просто переставляет true в true. Возвращает
// affected — число затронутых строк (0 → прогона нет либо он уже терминален:
// caller трактует как no-op).
func RequestCancel(ctx context.Context, db ExecQueryRower, applyID string) (int64, error) {
	if applyID == "" {
		return 0, fmt.Errorf("applyrun: empty apply_id")
	}
	tag, err := db.Exec(ctx, requestCancelSQL, applyID)
	if err != nil {
		return 0, fmt.Errorf("applyrun: request cancel: %w", err)
	}
	return tag.RowsAffected(), nil
}

const selectCancelRequestedSQL = `
SELECT cancel_requested
FROM apply_runs
WHERE apply_id = $1 AND sid = $2
`

// SelectCancelRequested читает свежий флаг `cancel_requested` строки
// `(applyID, sid)`. Acolyte вызывает его в claim-execute ПЕРЕД SendApply
// (ADR-027 cutover): если Cancel запрошен между claim-ом и отправкой, apply на
// Soul НЕ уходит. Отдельный узкий read (а не флаг из [ClaimNext]-RETURNING) —
// потому что окно claim→SendApply шире claim-транзакции: флаг мог встать уже
// после захвата Ward-а.
//
// Возврат [ErrApplyRunNotFound], если строки нет.
func SelectCancelRequested(ctx context.Context, db ExecQueryRower, applyID, sid string) (bool, error) {
	var cancelRequested bool
	err := db.QueryRow(ctx, selectCancelRequestedSQL, applyID, sid).Scan(&cancelRequested)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrApplyRunNotFound
		}
		return false, fmt.Errorf("applyrun: select cancel_requested: %w", err)
	}
	return cancelRequested, nil
}

// claimNextSQL — атомарный захват пачки planned-заданий (Ward-claim, ADR-027(d)).
// Идиома work-queue: внутренний SELECT … FOR UPDATE SKIP LOCKED блокирует
// выбранные planned-строки и ПРОПУСКАЕТ уже заблокированные конкурентами —
// два Acolyte-а разных инстансов никогда не захватывают одну строку. Внешний
// UPDATE переводит их в `claimed`, проставляет владельца/lease и инкрементит
// `attempt` (fencing-epoch, ADR-027(g)). RETURNING отдаёт захваченные строки
// именно этому Acolyte-у.
//
//	$1 claim_by_kid   — KID Acolyte-владельца
//	$2 lease          — interval до claim_expires_at (NOW() + $2)
//	$3 batch          — LIMIT захватываемой пачки
const claimNextSQL = `
UPDATE apply_runs AS r
SET status           = 'claimed',
    claim_by_kid     = $1,
    claim_at         = NOW(),
    claim_expires_at = NOW() + $2::interval,
    attempt          = r.attempt + 1
WHERE (r.apply_id, r.sid) IN (
    SELECT c.apply_id, c.sid
    FROM apply_runs AS c
    WHERE c.status = 'planned'
    ORDER BY c.started_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT $3
)
RETURNING apply_id, sid, incarnation_name, scenario, task_idx, status,
          error_summary, started_at, finished_at, started_by_aid,
          claim_by_kid, claim_at, claim_expires_at, attempt, recipe
`

// ClaimNext атомарно захватывает до batch planned-заданий для Acolyte-а kid,
// переводя их planned → claimed: проставляет claim_by_kid/claim_at,
// claim_expires_at = NOW()+lease и инкрементит attempt (fencing-epoch).
// Гарантия отсутствия гонки между конкурирующими Acolyte-ами разных инстансов —
// `FOR UPDATE SKIP LOCKED` (занятые строки пропускаются). FIFO по started_at.
//
// Возвращает захваченные строки (уже в статусе `claimed`, attempt
// инкрементирован). Пустой срез (не ошибка) — planned-заданий нет либо все
// уже заклеймлены конкурентами.
func ClaimNext(ctx context.Context, db ExecQueryRower, kid string, lease time.Duration, batch int) ([]*ApplyRun, error) {
	if kid == "" {
		return nil, fmt.Errorf("applyrun: empty kid")
	}
	if lease <= 0 {
		return nil, fmt.Errorf("applyrun: non-positive lease %s", lease)
	}
	if batch <= 0 {
		return nil, fmt.Errorf("applyrun: non-positive batch %d", batch)
	}

	rows, err := db.Query(ctx, claimNextSQL, kid, pgutil.Interval(lease), batch)
	if err != nil {
		return nil, fmt.Errorf("applyrun: claim next: %w", err)
	}
	defer rows.Close()

	var out []*ApplyRun
	for rows.Next() {
		run, scanErr := scanClaimedRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("applyrun: claim scan: %w", scanErr)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: claim iter: %w", err)
	}
	return out, nil
}

// scanClaimedRow читает полную строку apply_runs (включая Ward-claim колонки и
// recipe) из RETURNING [claimNextSQL] в *ApplyRun. recipe (jsonb, миграция 029)
// парсится в run.Recipe — Acolyte рендерит задание по нему ([RenderForHost]);
// NULL (старый путь, если бы попал в claim) → nil-Recipe.
func scanClaimedRow(row pgx.Row) (*ApplyRun, error) {
	var (
		run        ApplyRun
		statusStr  string
		recipeJSON []byte
	)
	if err := row.Scan(
		&run.ApplyID,
		&run.SID,
		&run.IncarnationName,
		&run.Scenario,
		&run.TaskIdx,
		&statusStr,
		&run.ErrorSummary,
		&run.StartedAt,
		&run.FinishedAt,
		&run.StartedByAID,
		&run.ClaimByKID,
		&run.ClaimAt,
		&run.ClaimExpiresAt,
		&run.Attempt,
		&recipeJSON,
	); err != nil {
		return nil, err
	}
	run.Status = Status(statusStr)
	recipe, err := UnmarshalRecipe(recipeJSON)
	if err != nil {
		return nil, err
	}
	run.Recipe = recipe
	return &run, nil
}

const markDispatchedSQL = `
UPDATE apply_runs
SET status = 'dispatched'
WHERE apply_id = $1 AND sid = $2 AND status = 'claimed'
`

// MarkDispatched переводит заклеймленное задание `(applyID, sid)` из
// claimed → dispatched — Acolyte вызывает И КОММИТИТ В PG СТРОГО ПЕРЕД SendApply
// (deliver-once intent-маркер, ADR-027 amend S3). Как только строка dispatched,
// recovery-reclaim её НЕ трогает (reclaim сужен до `status='claimed'`, S4):
// строка перестаёт быть «недо-доставленной», прогон отдаётся Soul-у, повторный
// SendApply = двойной apply. Это сердце инварианта против двойного apply.
//
// Фильтр `status = 'claimed'` — guard: переход возможен ТОЛЬКО из claimed;
// dispatched→dispatched / planned→dispatched / терминал→dispatched не проходят
// (idempotency-защита от повторного / гонкового перевода). Ward-колонки
// (claim_by_kid/lease/attempt) не трогаются — fencing-epoch уже зафиксирован
// ClaimNext-ом и едет в ApplyRequest.attempt.
//
// Возврат:
//   - [ErrApplyRunNotFound]   — строки нет вовсе.
//   - [ErrApplyRunNotClaimed] — строка есть, но не в статусе `claimed`.
func MarkDispatched(ctx context.Context, db ExecQueryRower, applyID, sid string) error {
	if applyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if sid == "" {
		return fmt.Errorf("applyrun: empty sid")
	}

	tag, err := db.Exec(ctx, markDispatchedSQL, applyID, sid)
	if err != nil {
		return fmt.Errorf("applyrun: mark dispatched: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// 0 строк затронуто: либо строки нет, либо она не в `claimed`. Различаем
	// для информативной ошибки (guard vs not-found) одним добором статуса.
	var statusStr string
	scanErr := db.QueryRow(ctx,
		`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2`,
		applyID, sid).Scan(&statusStr)
	if scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return ErrApplyRunNotFound
		}
		return fmt.Errorf("applyrun: mark dispatched probe: %w", scanErr)
	}
	return fmt.Errorf("%w (status=%s)", ErrApplyRunNotClaimed, statusStr)
}

// orphanDispatchedErrorSummary — фиксированная причина терминала `orphaned`.
// Записывается в error_summary осиротевших строк, чтобы триаж видел, что это
// Soul-reconcile, а не провал прогона.
const orphanDispatchedErrorSummary = "orphaned: RunResult lost, Soul does not track apply_id"

// orphanDispatchedSQL терминалит осиротевшие dispatched-строки SID-а
// (Soul-reconcile, ADR-027(g), S6). Single-winner: фильтр `status='dispatched'`
// гарантирует, что терминал НЕ перезаписывает терминал другого исполнения и не
// трогает не-dispatched-фазы; RowsAffected → сколько реально осиротили.
//
// epoch-fenced: строка осиротляется, ТОЛЬКО если её apply_id отсутствует в
// объявленном Soul-ом наборе `$2` (ARRAY known apply_ids). Любой apply_id из
// набора (с любым attempt) защищает строку — Soul декларирует, что прогон
// ведётся. Этим закрыт и attempt-разъезд: если набор несёт тот же apply_id, но с
// другим (большим) attempt (идёт пере-claim), строка НЕ терминалится — orphan
// безопаснее НЕ делать, чем сделать ложно.
//
//	$1 sid   — хост, чьи dispatched-строки сверяем
//	$2 known — ARRAY apply_id, объявленных живыми в WardRoster (может быть пустым)
const orphanDispatchedSQL = `
UPDATE apply_runs
SET status        = 'orphaned',
    error_summary = $3,
    finished_at   = NOW()
WHERE sid = $1
  AND status = 'dispatched'
  AND apply_id != ALL($2)
`

// OrphanDispatched терминалит dispatched-строки `sid`, чьи apply_id Soul НЕ
// объявил живыми в [WardRoster] (Soul-reconcile, ADR-027(g), S6). Закрывает
// dispatched-orphan дыру «Keeper и Soul оба мертвы после отдачи»: строка иначе
// застряла бы в `dispatched` навсегда (reclaim сужен до `claimed`, Reaper
// dispatched-timeout не делаем).
//
// known — apply_id, объявленные Soul-ом ведомыми (из WardRoster.active). Пустой
// набор = явная декларация «ничего не ведётся» → терминалятся ВСЕ dispatched-
// строки SID-а (корректно: после рестарта Soul in-flight физически нет). nil-набор
// трактуется как пустой.
//
// Single-winner (фильтр `status='dispatched'`, RowsAffected): гонка sweep ↔
// приходящий RunResult безопасна — кто первым переведёт строку из `dispatched`,
// тот и победил, второй увидит 0 строк. Авторитет — общая PG (reconnect на любой
// инстанс кластера сверяется с той же таблицей).
//
// Возвращает число осиротевших строк (для метрики/лога). НЕ ошибка при 0 —
// нечего сиротить (всё уже терминально либо всё объявлено живым).
func OrphanDispatched(ctx context.Context, db ExecQueryRower, sid string, known []*ActiveApply) (int64, error) {
	if sid == "" {
		return 0, fmt.Errorf("applyrun: empty sid")
	}

	// ARRAY known apply_id для PG `!= ALL($2)`. Пустой/nil → пустой массив:
	// `apply_id != ALL('{}')` истинно для всех → терминалятся все dispatched.
	knownIDs := make([]string, 0, len(known))
	for _, a := range known {
		if a != nil && a.ApplyID != "" {
			knownIDs = append(knownIDs, a.ApplyID)
		}
	}

	tag, err := db.Exec(ctx, orphanDispatchedSQL, sid, knownIDs, orphanDispatchedErrorSummary)
	if err != nil {
		return 0, fmt.Errorf("applyrun: orphan dispatched: %w", err)
	}
	return tag.RowsAffected(), nil
}

const selectAccessByApplyIDSQL = `
SELECT incarnation_name, started_by_aid
FROM apply_runs
WHERE apply_id = $1
ORDER BY started_at ASC
LIMIT 1
`

// Access — узкая проекция для SSE-RBAC: владелец прогона и его incarnation.
// StartedByAID — `NULL` для прогонов без identity Архонта (Soul-инициированные
// / system); caller трактует nil как «нет owner-а» (доступ только по
// incarnation-permission).
type Access struct {
	IncarnationName string
	StartedByAID    *string
}

// SelectAccessByApplyID резолвит `apply_id → (incarnation, started_by_aid)`
// по любой строке прогона (apply_id может иметь несколько SID-ов fan-out-а;
// incarnation и started_by_aid одинаковы для всех — берём первую по
// started_at). Используется SSE-handler-ом для RBAC-проверки подписки.
//
// Возврат [ErrApplyRunNotFound], если прогона нет (SSE-handler трактует как
// 403 — anti-enum: тот же ответ, что при отказе доступа).
func SelectAccessByApplyID(ctx context.Context, db ExecQueryRower, applyID string) (*Access, error) {
	row := db.QueryRow(ctx, selectAccessByApplyIDSQL, applyID)
	var (
		acc       Access
		startedBy *string
	)
	if err := row.Scan(&acc.IncarnationName, &startedBy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrApplyRunNotFound
		}
		return nil, fmt.Errorf("applyrun: resolve access: %w", err)
	}
	acc.StartedByAID = startedBy
	return &acc, nil
}

const selectIncarnationByApplyIDSQL = `
SELECT incarnation_name, scenario, attempt
FROM apply_runs
WHERE apply_id = $1 AND sid = $2
`

// SelectIncarnationByApplyID — узкий резолв `(apply_id, sid) → (incarnation,
// scenario, attempt)` для RunResult-correlation. Возвращает только поля, нужные
// state-commit-у и epoch-check-у на приёме результата (ADR-027(g), gate-1);
// полную строку даёт [SelectByApplyID].
//
// attempt — текущий fencing-epoch строки (инкрементится [ClaimNext] при захвате
// Ward): correlateRunResult сверяет его с RunResult.attempt и отвергает результат
// устаревшей попытки (recvAttempt < row.attempt → существует пере-claim с бОльшим
// epoch → stale-drop).
//
// Возврат [ErrApplyRunNotFound], если строки нет (apply без scenario-runner-а
// — ad-hoc push / standalone apply; caller обрабатывает как log+skip).
func SelectIncarnationByApplyID(ctx context.Context, db ExecQueryRower, applyID, sid string) (incarnationName, scenario string, attempt int32, err error) {
	row := db.QueryRow(ctx, selectIncarnationByApplyIDSQL, applyID, sid)
	if scanErr := row.Scan(&incarnationName, &scenario, &attempt); scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return "", "", 0, ErrApplyRunNotFound
		}
		return "", "", 0, fmt.Errorf("applyrun: resolve incarnation: %w", scanErr)
	}
	return incarnationName, scenario, attempt, nil
}
