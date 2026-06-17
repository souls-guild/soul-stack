package voyage

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

// PG-коды (parity errandrun/crud.go).
const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество pgxpool.Pool, нужное CRUD-у (симметрично
// errandrun.ExecQueryRower). unit-тесты ходят через fake, production даёт
// реальный pool/Conn/Tx.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	// CopyFrom — batch-вставка единиц прогона (InsertTargets, S-med-3). pgx.Tx /
	// *pgx.Conn / *pgxpool.Pool реализуют нативно; fake в unit-тестах — стаб.
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
}

// Compile-time check.
var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

// inter_batch_interval пишется text-литералом через `$N::interval`, читается как
// float-секунды через EXTRACT(EPOCH FROM ...) — pgx не сканит PG interval в
// time.Duration напрямую, а тащить pgtype.Interval ради S4-фичи преждевременно.
const insertSQL = `
INSERT INTO voyages (
    voyage_id, kind, scenario_name, module, input,
    target_resolved, target_origin,
    batch_size, concurrency, batch_mode, dry_run,
    schedule_at, inter_batch_interval, on_failure,
    total_batches, status, started_by_aid,
    batch_percent, fail_threshold, inter_unit_interval, require_alive,
    cadence_id
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7,
    $8, $9, $10, $11,
    $12, $13::interval, $14,
    $15, $16, $17,
    $18, $19, $20::interval, $21,
    $22
)
RETURNING created_at
`

// Insert вставляет новый Voyage в статусе [StatusPending]. voyage_id (ULID) —
// генерация caller-а (API-handler S5), CRUD только проверяет непустоту и
// диспатчит UNIQUE-violation в [ErrVoyageExists].
//
// Pre-conditions: непустые VoyageID / StartedByAID; валидный Kind; для
// KindScenario — непустой ScenarioName, для KindCommand — непустой Module;
// непустой TargetResolved; OnFailure (если задан) валиден; Status пуст или
// StatusPending; BatchSize/Concurrency (если заданы) > 0. Input — caller
// сериализует заранее (пустой → CRUD подставит `{}` сам, parity DEFAULT).
func Insert(ctx context.Context, db ExecQueryRower, v *Voyage) error {
	if v == nil {
		return fmt.Errorf("voyage: nil voyage")
	}
	if v.VoyageID == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if v.StartedByAID == "" {
		return fmt.Errorf("voyage: empty started_by_aid")
	}
	if !ValidKind(v.Kind) {
		return fmt.Errorf("voyage: invalid kind %q", v.Kind)
	}
	switch v.Kind {
	case KindScenario:
		if v.ScenarioName == nil || *v.ScenarioName == "" {
			return fmt.Errorf("voyage: kind=scenario требует непустой scenario_name")
		}
		if v.Module != nil && *v.Module != "" {
			return fmt.Errorf("voyage: kind=scenario не должен нести module")
		}
	case KindCommand:
		if v.Module == nil || *v.Module == "" {
			return fmt.Errorf("voyage: kind=command требует непустой module")
		}
		if v.ScenarioName != nil && *v.ScenarioName != "" {
			return fmt.Errorf("voyage: kind=command не должен нести scenario_name")
		}
	}
	if len(v.TargetResolved) == 0 {
		return fmt.Errorf("voyage: empty target_resolved")
	}
	if v.BatchSize != nil && *v.BatchSize <= 0 {
		return fmt.Errorf("voyage: batch_size must be > 0, got %d", *v.BatchSize)
	}
	if v.Concurrency != nil && *v.Concurrency <= 0 {
		return fmt.Errorf("voyage: concurrency must be > 0, got %d", *v.Concurrency)
	}
	if v.BatchMode != nil && !ValidBatchMode(*v.BatchMode) {
		return fmt.Errorf("voyage: invalid batch_mode %q", *v.BatchMode)
	}
	if v.BatchPercent != nil && (*v.BatchPercent < 1 || *v.BatchPercent > 100) {
		return fmt.Errorf("voyage: batch_percent must be in [1, 100], got %d", *v.BatchPercent)
	}
	if v.FailThreshold != nil && *v.FailThreshold <= 0 {
		return fmt.Errorf("voyage: fail_threshold must be > 0, got %d", *v.FailThreshold)
	}
	if v.OnFailure != nil && !ValidOnFailure(*v.OnFailure) {
		return fmt.Errorf("voyage: invalid on_failure %q", *v.OnFailure)
	}
	if v.Status == "" {
		// Отложенный старт (S4): schedule_at в будущем → scheduled, иначе pending.
		// Worker подбирает scheduled только когда schedule_at <= NOW() (см.
		// [ClaimNext]). Сравнение по UTC-стенке против локального now —
		// ScheduleAt уже нормализуется к UTC в insertSQL-аргументе ниже;
		// для решения о ветке достаточно сопоставить с time.Now().
		if v.ScheduleAt != nil && v.ScheduleAt.After(time.Now()) {
			v.Status = StatusScheduled
		} else {
			v.Status = StatusPending
		}
	}
	if v.Status != StatusPending && v.Status != StatusScheduled {
		return fmt.Errorf("voyage: Insert требует status=pending|scheduled, got %q", v.Status)
	}

	input := v.Input
	if len(input) == 0 {
		input = []byte("{}")
	}

	var scenarioArg, moduleArg, onFailureArg any
	if v.ScenarioName != nil {
		scenarioArg = *v.ScenarioName
	}
	if v.Module != nil {
		moduleArg = *v.Module
	}
	if v.OnFailure != nil {
		onFailureArg = string(*v.OnFailure)
	}
	var originArg any
	if len(v.TargetOrigin) > 0 {
		originArg = []byte(v.TargetOrigin)
	}
	var batchSizeArg, concurrencyArg, batchModeArg any
	if v.BatchSize != nil {
		batchSizeArg = *v.BatchSize
	}
	if v.Concurrency != nil {
		concurrencyArg = *v.Concurrency
	}
	if v.BatchMode != nil {
		batchModeArg = string(*v.BatchMode)
	}
	var scheduleArg any
	if v.ScheduleAt != nil {
		scheduleArg = v.ScheduleAt.UTC()
	}
	var intervalArg any
	if v.InterBatchInterval != nil {
		intervalArg = pgutil.Interval(*v.InterBatchInterval)
	}
	var batchPercentArg, failThresholdArg, interUnitArg, requireAliveArg any
	if v.BatchPercent != nil {
		batchPercentArg = *v.BatchPercent
	}
	if v.FailThreshold != nil {
		failThresholdArg = *v.FailThreshold
	}
	if v.InterUnitInterval != nil {
		interUnitArg = pgutil.Interval(*v.InterUnitInterval)
	}
	if v.RequireAlive != nil {
		requireAliveArg = *v.RequireAlive
	}
	var cadenceIDArg any
	if v.CadenceID != nil {
		cadenceIDArg = *v.CadenceID
	}

	row := db.QueryRow(ctx, insertSQL,
		v.VoyageID,
		string(v.Kind),
		scenarioArg,
		moduleArg,
		input,
		[]byte(v.TargetResolved),
		originArg,
		batchSizeArg,
		concurrencyArg,
		batchModeArg,
		v.DryRun,
		scheduleArg,
		intervalArg,
		onFailureArg,
		v.TotalBatches,
		string(v.Status),
		v.StartedByAID,
		batchPercentArg,
		failThresholdArg,
		interUnitArg,
		requireAliveArg,
		cadenceIDArg,
	)
	if err := row.Scan(&v.CreatedAt); err != nil {
		return mapInsertError(err)
	}
	return nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w",
				ErrVoyageExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("voyage: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("voyage: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("voyage: insert: %w", err)
}

const selectColumns = `
    voyage_id, kind, scenario_name, module, input,
    target_resolved, target_origin,
    batch_size, concurrency, batch_mode, dry_run,
    schedule_at, EXTRACT(EPOCH FROM inter_batch_interval)::float8, on_failure,
    total_batches, current_batch_index, status,
    claimed_by_kid, last_renewed_at, claim_expires_at, attempt,
    started_by_aid, created_at, started_at, finished_at, summary,
    batch_percent, fail_threshold, EXTRACT(EPOCH FROM inter_unit_interval)::float8, require_alive,
    cadence_id
`

const selectByIDSQL = `SELECT ` + selectColumns + `
FROM voyages
WHERE voyage_id = $1
`

// SelectByID читает Voyage по PK. [ErrVoyageNotFound] при отсутствии.
func SelectByID(ctx context.Context, db ExecQueryRower, id string) (*Voyage, error) {
	if id == "" {
		return nil, fmt.Errorf("voyage: empty voyage_id")
	}
	row := db.QueryRow(ctx, selectByIDSQL, id)
	v, err := scanVoyage(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrVoyageNotFound
		}
		return nil, fmt.Errorf("voyage: select by id: %w", err)
	}
	return v, nil
}

// scanVoyage — общий scan строки `voyages` в *Voyage. Используется SelectByID,
// List (через rows.Next) и ClaimNext (RETURNING). Колонки nullable биндим через
// указатели; input/target_resolved/target_origin — jsonb-bytes;
// inter_batch_interval приходит как float-секунды (EXTRACT EPOCH) → time.Duration.
func scanVoyage(row pgx.Row) (*Voyage, error) {
	var (
		v             Voyage
		kindStr       string
		statusStr     string
		scenarioName  *string
		module        *string
		targetOrigin  []byte
		batchSize     *int
		concurrency   *int
		batchModeStr  *string
		scheduleAt    *time.Time
		intervalSecs  *float64
		onFailureStr  *string
		claimedByKID  *string
		lastRenewedAt *time.Time
		claimExpires  *time.Time
		startedAt     *time.Time
		finishedAt    *time.Time
		summaryJSON   []byte
		batchPercent  *int
		failThreshold *int
		interUnitSecs *float64
		requireAlive  *bool
		cadenceID     *string
	)
	if err := row.Scan(
		&v.VoyageID,
		&kindStr,
		&scenarioName,
		&module,
		&v.Input,
		&v.TargetResolved,
		&targetOrigin,
		&batchSize,
		&concurrency,
		&batchModeStr,
		&v.DryRun,
		&scheduleAt,
		&intervalSecs,
		&onFailureStr,
		&v.TotalBatches,
		&v.CurrentBatchIndex,
		&statusStr,
		&claimedByKID,
		&lastRenewedAt,
		&claimExpires,
		&v.Attempt,
		&v.StartedByAID,
		&v.CreatedAt,
		&startedAt,
		&finishedAt,
		&summaryJSON,
		&batchPercent,
		&failThreshold,
		&interUnitSecs,
		&requireAlive,
		&cadenceID,
	); err != nil {
		return nil, err
	}
	v.Kind = Kind(kindStr)
	v.Status = Status(statusStr)
	v.ScenarioName = scenarioName
	v.Module = module
	v.BatchSize = batchSize
	v.Concurrency = concurrency
	if batchModeStr != nil {
		m := BatchMode(*batchModeStr)
		v.BatchMode = &m
	}
	v.ScheduleAt = scheduleAt
	v.ClaimedByKID = claimedByKID
	v.LastRenewedAt = lastRenewedAt
	v.ClaimExpiresAt = claimExpires
	v.StartedAt = startedAt
	v.FinishedAt = finishedAt
	if onFailureStr != nil {
		f := OnFailure(*onFailureStr)
		v.OnFailure = &f
	}
	if intervalSecs != nil {
		d := time.Duration(*intervalSecs * float64(time.Second))
		v.InterBatchInterval = &d
	}
	v.BatchPercent = batchPercent
	v.FailThreshold = failThreshold
	if interUnitSecs != nil {
		d := time.Duration(*interUnitSecs * float64(time.Second))
		v.InterUnitInterval = &d
	}
	v.RequireAlive = requireAlive
	v.CadenceID = cadenceID
	if len(targetOrigin) > 0 {
		v.TargetOrigin = append([]byte(nil), targetOrigin...)
	}
	summary, err := unmarshalSummary(summaryJSON)
	if err != nil {
		return nil, fmt.Errorf("voyage: unmarshal summary: %w", err)
	}
	v.Summary = summary
	return &v, nil
}

// ListFilter — фильтры [List] (parity errandrun.AllFilter). Statuses —
// multi-value OR (пустой слайс — без фильтра); Kind — exact (пустая строка — без
// фильтра); CreatedAfter (zero-time → нет фильтра) — exclusive lower bound по
// `created_at`; CadenceID — exact (пустая строка — без фильтра): дочерние Voyage
// одного Cadence-расписания (`GET /v1/cadences/{id}/runs`, ADR-046 §6).
type ListFilter struct {
	Statuses     []Status
	Kind         Kind
	CreatedAfter time.Time
	CadenceID    string
}

const listSQL = `SELECT ` + selectColumns + `
FROM voyages
WHERE ($1::text[] IS NULL OR cardinality($1::text[]) = 0 OR status = ANY($1::text[]))
  AND ($2::text IS NULL OR kind = $2)
  AND ($3::timestamptz IS NULL OR created_at > $3::timestamptz)
  AND ($6::text IS NULL OR cadence_id = $6)
ORDER BY created_at DESC
LIMIT $4 OFFSET $5
`

const countSQL = `SELECT COUNT(*) FROM voyages
WHERE ($1::text[] IS NULL OR cardinality($1::text[]) = 0 OR status = ANY($1::text[]))
  AND ($2::text IS NULL OR kind = $2)
  AND ($3::timestamptz IS NULL OR created_at > $3::timestamptz)
  AND ($4::text IS NULL OR cadence_id = $4)
`

// List возвращает страницу Voyage-ов (parity errandrun.SelectAll): фильтры по
// статусам (OR), kind (exact), `created_after` (exclusive) и cadence_id (exact).
// Сортировка — `created_at DESC`. Total — общее число строк под фильтром
// (отдельный COUNT). limit/offset не валидируются — caller прогнал через
// page-парсер.
func List(ctx context.Context, db ExecQueryRower, filter ListFilter, offset, limit int) ([]*Voyage, int, error) {
	var statusesArg any
	if len(filter.Statuses) > 0 {
		ss := make([]string, len(filter.Statuses))
		for i, s := range filter.Statuses {
			ss[i] = string(s)
		}
		statusesArg = ss
	}
	var kindArg any
	if filter.Kind != "" {
		kindArg = string(filter.Kind)
	}
	var createdAfterArg any
	if !filter.CreatedAfter.IsZero() {
		createdAfterArg = filter.CreatedAfter.UTC()
	}
	var cadenceIDArg any
	if filter.CadenceID != "" {
		cadenceIDArg = filter.CadenceID
	}

	var total int
	if err := db.QueryRow(ctx, countSQL, statusesArg, kindArg, createdAfterArg, cadenceIDArg).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("voyage: count: %w", err)
	}

	rows, err := db.Query(ctx, listSQL, statusesArg, kindArg, createdAfterArg, limit, offset, cadenceIDArg)
	if err != nil {
		return nil, 0, fmt.Errorf("voyage: list: %w", err)
	}
	defer rows.Close()

	out := make([]*Voyage, 0, limit)
	for rows.Next() {
		v, scanErr := scanVoyage(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("voyage: list scan: %w", scanErr)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("voyage: list iter: %w", err)
	}
	return out, total, nil
}

const finalizeWithOwnershipSQL = `
UPDATE voyages
SET status           = $3,
    finished_at      = NOW(),
    summary          = COALESCE($4, summary),
    claimed_by_kid   = NULL,
    claim_expires_at = NULL,
    last_renewed_at  = NULL
WHERE voyage_id      = $1
  AND claimed_by_kid = $2
  AND status         = 'running'
`

// Finalize переводит running Voyage в терминальный статус ПОД CAS-guard-ом
// ownership: UPDATE срабатывает только при `claimed_by_kid = $kid AND
// status = 'running'`. Если lease ушёл между ClaimNext и финализацией
// (Reaper-reclaim → другой Keeper подобрал) — 0 строк → [ErrLeaseLost] (parity
// errandrun.FinalizeWithOwnership). Caller (VoyageWorker), получив ErrLeaseLost,
// логирует WARN и не повторяет.
//
// finished_at = NOW() (PG-side clock). summary опционален (nil → COALESCE
// сохранит текущее значение). status обязан быть терминальным; non-terminal →
// [ErrInvalidStatus].
func Finalize(ctx context.Context, db ExecQueryRower, id, kid string, status Status, summary *Summary) error {
	if id == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if kid == "" {
		return fmt.Errorf("voyage: empty kid")
	}
	if !ValidStatus(status) {
		return fmt.Errorf("%w: %q", ErrInvalidStatus, status)
	}
	if !IsTerminal(status) {
		return fmt.Errorf("%w: %q is not terminal", ErrInvalidStatus, status)
	}

	summaryJSON, err := marshalSummary(summary)
	if err != nil {
		return fmt.Errorf("voyage: marshal summary: %w", err)
	}
	var summaryArg any
	if summaryJSON != nil {
		summaryArg = summaryJSON
	}

	tag, err := db.Exec(ctx, finalizeWithOwnershipSQL, id, kid, string(status), summaryArg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeCheckViolation {
			return fmt.Errorf("voyage: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("voyage: finalize: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

const verifyOwnershipSQL = `
SELECT 1
FROM voyages
WHERE voyage_id      = $1
  AND claimed_by_kid = $2
  AND attempt        = $3
  AND status         = 'running'
`

// VerifyOwnership — fencing-проверка «я всё ещё владелец Voyage с моим attempt».
// Параллель CAS-предиката [Finalize] (`claimed_by_kid = $kid AND status =
// 'running'`), но с attempt-epoch вместо записи: если Voyage реклеймнут другим
// Keeper-ом (Reaper вернул в pending → ClaimNext инкрементил attempt → новый
// владелец), строка не сматчится — [ErrLeaseLost]. attempt отличает «свой claim»
// от пере-захвата того же KID-а после реклейма (ADR-027(g) fencing-epoch).
//
// Вызывается перед side-effect-ом, который НЕ имеет своего ownership-guard-а
// (command-leg-spawn dispatch-ит Errand до MarkTargetRunning-CAS; без fencing —
// дубль-Errand при реклейме посреди Leg-а, S-med-2). nil — владелец;
// [ErrLeaseLost] — lease потеряна; иная ошибка — CRUD-сбой PG.
func VerifyOwnership(ctx context.Context, db ExecQueryRower, id, kid string, attempt int) error {
	if id == "" {
		return fmt.Errorf("voyage: empty voyage_id")
	}
	if kid == "" {
		return fmt.Errorf("voyage: empty kid")
	}
	var one int
	err := db.QueryRow(ctx, verifyOwnershipSQL, id, kid, attempt).Scan(&one)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		return fmt.Errorf("voyage: verify ownership: %w", err)
	}
	return nil
}
