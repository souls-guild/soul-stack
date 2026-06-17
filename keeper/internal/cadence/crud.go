package cadence

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

// PG-коды (parity voyage/crud.go).
const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество pgxpool.Pool, нужное CRUD-у (parity
// voyage.ExecQueryRower минус CopyFrom: у cadences нет batch-вставки единиц).
// unit-тесты ходят через fake, production даёт реальный pool/Conn/Tx.
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

// validate — общая проверка рецепта/расписания Cadence (Insert + Update). Чистая
// функция от полей, без PG. Инварианты:
//   - непустые ID / Name / CreatedByAID; валидные enum-ы (schedule_kind /
//     overlap_policy / kind / batch_mode / on_failure);
//   - schedule_kind ↔ interval/cron XOR (interval ⇒ только interval_seconds>0;
//     cron ⇒ только непустой cron_expr) — parity CHECK
//     `cadences_schedule_consistency`, но «дружелюбная» ошибка до PG;
//   - kind ↔ scenario_name/module (parity voyage.Insert);
//   - непустой Target; sane-bounds batch_size/percent/concurrency/fail_threshold.
func validate(c *Cadence) error {
	if c == nil {
		return fmt.Errorf("cadence: nil cadence")
	}
	if c.ID == "" {
		return fmt.Errorf("cadence: empty id")
	}
	if c.Name == "" {
		return fmt.Errorf("cadence: empty name")
	}
	if c.CreatedByAID == "" {
		return fmt.Errorf("cadence: empty created_by_aid")
	}
	if !ValidScheduleKind(c.ScheduleKind) {
		return fmt.Errorf("cadence: invalid schedule_kind %q", c.ScheduleKind)
	}
	switch c.ScheduleKind {
	case ScheduleKindInterval:
		if c.IntervalSeconds == nil {
			return fmt.Errorf("cadence: schedule_kind=interval требует interval_seconds")
		}
		if *c.IntervalSeconds <= 0 {
			return fmt.Errorf("cadence: interval_seconds must be > 0, got %d", *c.IntervalSeconds)
		}
		if c.CronExpr != nil && *c.CronExpr != "" {
			return fmt.Errorf("cadence: schedule_kind=interval не должен нести cron_expr")
		}
	case ScheduleKindCron:
		if c.CronExpr == nil || *c.CronExpr == "" {
			return fmt.Errorf("cadence: schedule_kind=cron требует непустой cron_expr")
		}
		if c.IntervalSeconds != nil {
			return fmt.Errorf("cadence: schedule_kind=cron не должен нести interval_seconds")
		}
		// Битый cron отвергаем здесь, до PG: CHECK-инвариант миграции 066 не парсит
		// cron-грамматику, а scheduler-у (NextRun) нужен валидный expr (ADR-046 §2).
		if _, err := ParseCron(*c.CronExpr); err != nil {
			return err
		}
	}
	if !ValidOverlapPolicy(c.OverlapPolicy) {
		return fmt.Errorf("cadence: invalid overlap_policy %q", c.OverlapPolicy)
	}
	if !ValidKind(c.Kind) {
		return fmt.Errorf("cadence: invalid kind %q", c.Kind)
	}
	switch c.Kind {
	case KindScenario:
		if c.ScenarioName == nil || *c.ScenarioName == "" {
			return fmt.Errorf("cadence: kind=scenario требует непустой scenario_name")
		}
		if c.Module != nil && *c.Module != "" {
			return fmt.Errorf("cadence: kind=scenario не должен нести module")
		}
	case KindCommand:
		if c.Module == nil || *c.Module == "" {
			return fmt.Errorf("cadence: kind=command требует непустой module")
		}
		if c.ScenarioName != nil && *c.ScenarioName != "" {
			return fmt.Errorf("cadence: kind=command не должен нести scenario_name")
		}
	}
	if len(c.Target) == 0 {
		return fmt.Errorf("cadence: empty target")
	}
	if c.BatchMode != nil && !ValidBatchMode(*c.BatchMode) {
		return fmt.Errorf("cadence: invalid batch_mode %q", *c.BatchMode)
	}
	if c.BatchSize != nil && *c.BatchSize <= 0 {
		return fmt.Errorf("cadence: batch_size must be > 0, got %d", *c.BatchSize)
	}
	if c.BatchPercent != nil && (*c.BatchPercent < 1 || *c.BatchPercent > 100) {
		return fmt.Errorf("cadence: batch_percent must be in [1, 100], got %d", *c.BatchPercent)
	}
	if c.Concurrency != nil && *c.Concurrency <= 0 {
		return fmt.Errorf("cadence: concurrency must be > 0, got %d", *c.Concurrency)
	}
	if c.FailThreshold != nil && *c.FailThreshold <= 0 {
		return fmt.Errorf("cadence: fail_threshold must be > 0, got %d", *c.FailThreshold)
	}
	if c.FailThresholdPercent != nil && (*c.FailThresholdPercent < 1 || *c.FailThresholdPercent > 100) {
		return fmt.Errorf("cadence: fail_threshold_percent must be in [1, 100], got %d", *c.FailThresholdPercent)
	}
	if c.FailThreshold != nil && c.FailThresholdPercent != nil {
		return fmt.Errorf("cadence: fail_threshold и fail_threshold_percent взаимоисключающи (задайте один)")
	}
	if c.OnFailure != nil && !ValidOnFailure(*c.OnFailure) {
		return fmt.Errorf("cadence: invalid on_failure %q", *c.OnFailure)
	}
	return nil
}

// ValidateIntervalFloor проверяет нижний предел периода interval-Cadence
// (floor-лимит, ADR-046 Pass B): `interval_seconds >= floorSeconds`. Источник
// floorSeconds — тот же config `cadence_scheduler.poll_floor`, что у адаптивного
// опроса Conductor (единый минимум, не хардкод 30 в двух местах), прокидывается
// write-path-handler-ом. floorSeconds <= 0 → проверка выключена (библиотечный
// Insert/Update и unit/integration-тесты, которым нужно вставить суб-floor строку
// для clamp-проверки, floor не задают). cron-Cadence (interval_seconds == NULL) и
// не-interval schedule_kind — не затрагиваются (cron-гранулярность минута ≥ floor).
//
// Возвращает голую `cadence: …`-ошибку (как [validate]): handler маппит её в 422
// тем же путём, что прочие validate-ошибки рецепта.
func ValidateIntervalFloor(c *Cadence, floorSeconds int) error {
	if floorSeconds <= 0 || c == nil {
		return nil
	}
	if c.ScheduleKind != ScheduleKindInterval || c.IntervalSeconds == nil {
		return nil
	}
	if *c.IntervalSeconds < floorSeconds {
		return fmt.Errorf(
			"cadence: минимальный период Cadence — %ds; для реакции быстрее %ds используйте Beacons (Vigil/Oracle, ADR-030), got interval_seconds=%d",
			floorSeconds, floorSeconds, *c.IntervalSeconds)
	}
	return nil
}

// recipeArgs распаковывает рецепт/расписание Cadence в позиционные SQL-аргументы
// для INSERT/UPDATE. nullable-поля → nil-интерфейс (NULL в БД); INTERVAL —
// text-литералом через pgutil.Interval (parity voyage). Возвращает 23 значения в
// порядке колонок начиная с `name` до `last_run_at`. id (PK) и created_by_aid
// (фиксирован, в UPDATE не пишется) caller добавляет отдельно: Insert — оба,
// Update — только id.
func recipeArgs(c *Cadence) []any {
	input := c.Input
	if len(input) == 0 {
		input = []byte("{}")
	}

	var intervalSecondsArg, cronExprArg any
	if c.IntervalSeconds != nil {
		intervalSecondsArg = *c.IntervalSeconds
	}
	if c.CronExpr != nil {
		cronExprArg = *c.CronExpr
	}
	var scenarioArg, moduleArg any
	if c.ScenarioName != nil {
		scenarioArg = *c.ScenarioName
	}
	if c.Module != nil {
		moduleArg = *c.Module
	}
	var batchModeArg, batchSizeArg, batchPercentArg, concurrencyArg, failThresholdArg, failThresholdPercentArg any
	if c.BatchMode != nil {
		batchModeArg = string(*c.BatchMode)
	}
	if c.BatchSize != nil {
		batchSizeArg = *c.BatchSize
	}
	if c.BatchPercent != nil {
		batchPercentArg = *c.BatchPercent
	}
	if c.Concurrency != nil {
		concurrencyArg = *c.Concurrency
	}
	if c.FailThreshold != nil {
		failThresholdArg = *c.FailThreshold
	}
	if c.FailThresholdPercent != nil {
		failThresholdPercentArg = *c.FailThresholdPercent
	}
	var interBatchArg, interUnitArg any
	if c.InterBatchInterval != nil {
		interBatchArg = pgutil.Interval(*c.InterBatchInterval)
	}
	if c.InterUnitInterval != nil {
		interUnitArg = pgutil.Interval(*c.InterUnitInterval)
	}
	var requireAliveArg, onFailureArg any
	if c.RequireAlive != nil {
		requireAliveArg = *c.RequireAlive
	}
	if c.OnFailure != nil {
		onFailureArg = string(*c.OnFailure)
	}
	var nextRunArg, lastRunArg any
	if c.NextRunAt != nil {
		nextRunArg = c.NextRunAt.UTC()
	}
	if c.LastRunAt != nil {
		lastRunArg = c.LastRunAt.UTC()
	}

	return []any{
		c.Name,
		c.Enabled,
		string(c.ScheduleKind),
		intervalSecondsArg,
		cronExprArg,
		string(c.OverlapPolicy),
		string(c.Kind),
		scenarioArg,
		moduleArg,
		[]byte(c.Target),
		input,
		batchModeArg,
		batchSizeArg,
		batchPercentArg,
		concurrencyArg,
		failThresholdArg,
		failThresholdPercentArg,
		interBatchArg,
		interUnitArg,
		requireAliveArg,
		onFailureArg,
		nextRunArg,
		lastRunArg,
	}
}

const insertSQL = `
INSERT INTO cadences (
    id, name, enabled,
    schedule_kind, interval_seconds, cron_expr, overlap_policy,
    kind, scenario_name, module, target, input,
    batch_mode, batch_size, batch_percent, concurrency, fail_threshold, fail_threshold_percent,
    inter_batch_interval, inter_unit_interval, require_alive, on_failure,
    next_run_at, last_run_at,
    created_by_aid
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7,
    $8, $9, $10, $11, $12,
    $13, $14, $15, $16, $17, $18,
    $19::interval, $20::interval, $21, $22,
    $23, $24,
    $25
)
RETURNING created_at, updated_at
`

// Insert вставляет новую Cadence. id (ULID) — генерация caller-а (API-handler
// S4), CRUD только проверяет непустоту и диспатчит UNIQUE-violation в
// [ErrCadenceExists]. Прогоняет [validate] до SQL. Input пустой → CRUD подставит
// `{}` (parity DEFAULT). created_at/updated_at читаются из RETURNING.
func Insert(ctx context.Context, db ExecQueryRower, c *Cadence) error {
	if err := validate(c); err != nil {
		return err
	}

	args := append([]any{c.ID}, recipeArgs(c)...)
	args = append(args, c.CreatedByAID)
	row := db.QueryRow(ctx, insertSQL, args...)
	if err := row.Scan(&c.CreatedAt, &c.UpdatedAt); err != nil {
		return mapWriteError(err)
	}
	return nil
}

func mapWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w",
				ErrCadenceExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("cadence: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("cadence: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("cadence: write: %w", err)
}

const selectColumns = `
    id, name, enabled,
    schedule_kind, interval_seconds, cron_expr, overlap_policy,
    kind, scenario_name, module, target, input,
    batch_mode, batch_size, batch_percent, concurrency, fail_threshold, fail_threshold_percent,
    EXTRACT(EPOCH FROM inter_batch_interval)::float8,
    EXTRACT(EPOCH FROM inter_unit_interval)::float8,
    require_alive, on_failure,
    next_run_at, last_run_at,
    created_by_aid, created_at, updated_at
`

const selectByIDSQL = `SELECT ` + selectColumns + `
FROM cadences
WHERE id = $1
`

// Get читает Cadence по PK. [ErrCadenceNotFound] при отсутствии.
func Get(ctx context.Context, db ExecQueryRower, id string) (*Cadence, error) {
	if id == "" {
		return nil, fmt.Errorf("cadence: empty id")
	}
	row := db.QueryRow(ctx, selectByIDSQL, id)
	c, err := scanCadence(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCadenceNotFound
		}
		return nil, fmt.Errorf("cadence: get: %w", err)
	}
	return c, nil
}

// scanCadence — общий scan строки `cadences` в *Cadence. Используется Get и List
// (через rows.Next). Nullable-колонки биндим через указатели; target/input —
// jsonb-bytes; INTERVAL приходит float-секундами (EXTRACT EPOCH) → time.Duration.
func scanCadence(row pgx.Row) (*Cadence, error) {
	var (
		c                    Cadence
		scheduleKindStr      string
		intervalSeconds      *int
		cronExpr             *string
		overlapStr           string
		kindStr              string
		scenarioName         *string
		module               *string
		batchModeStr         *string
		batchSize            *int
		batchPercent         *int
		concurrency          *int
		failThreshold        *int
		failThresholdPercent *int
		interBatchSecs       *float64
		interUnitSecs        *float64
		requireAlive         *bool
		onFailureStr         *string
	)
	if err := row.Scan(
		&c.ID,
		&c.Name,
		&c.Enabled,
		&scheduleKindStr,
		&intervalSeconds,
		&cronExpr,
		&overlapStr,
		&kindStr,
		&scenarioName,
		&module,
		&c.Target,
		&c.Input,
		&batchModeStr,
		&batchSize,
		&batchPercent,
		&concurrency,
		&failThreshold,
		&failThresholdPercent,
		&interBatchSecs,
		&interUnitSecs,
		&requireAlive,
		&onFailureStr,
		&c.NextRunAt,
		&c.LastRunAt,
		&c.CreatedByAID,
		&c.CreatedAt,
		&c.UpdatedAt,
	); err != nil {
		return nil, err
	}
	c.ScheduleKind = ScheduleKind(scheduleKindStr)
	c.IntervalSeconds = intervalSeconds
	c.CronExpr = cronExpr
	c.OverlapPolicy = OverlapPolicy(overlapStr)
	c.Kind = Kind(kindStr)
	c.ScenarioName = scenarioName
	c.Module = module
	if batchModeStr != nil {
		m := BatchMode(*batchModeStr)
		c.BatchMode = &m
	}
	c.BatchSize = batchSize
	c.BatchPercent = batchPercent
	c.Concurrency = concurrency
	c.FailThreshold = failThreshold
	c.FailThresholdPercent = failThresholdPercent
	if interBatchSecs != nil {
		d := time.Duration(*interBatchSecs * float64(time.Second))
		c.InterBatchInterval = &d
	}
	if interUnitSecs != nil {
		d := time.Duration(*interUnitSecs * float64(time.Second))
		c.InterUnitInterval = &d
	}
	c.RequireAlive = requireAlive
	if onFailureStr != nil {
		f := OnFailure(*onFailureStr)
		c.OnFailure = &f
	}
	return &c, nil
}

// ListFilter — фильтры [List]. EnabledOnly — только enabled-расписания (false →
// без фильтра по enabled); Kind — exact (пустая строка — без фильтра).
type ListFilter struct {
	EnabledOnly bool
	Kind        Kind
}

const listSQL = `SELECT ` + selectColumns + `
FROM cadences
WHERE (NOT $1::bool OR enabled)
  AND ($2::text IS NULL OR kind = $2)
ORDER BY created_at DESC
LIMIT $3 OFFSET $4
`

const countSQL = `SELECT COUNT(*) FROM cadences
WHERE (NOT $1::bool OR enabled)
  AND ($2::text IS NULL OR kind = $2)
`

// List возвращает страницу Cadence-ов: фильтры по enabled и kind (exact).
// Сортировка — `created_at DESC`. Total — общее число строк под фильтром
// (отдельный COUNT). limit/offset не валидируются — caller прогнал через
// page-парсер (parity voyage.List).
func List(ctx context.Context, db ExecQueryRower, filter ListFilter, offset, limit int) ([]*Cadence, int, error) {
	var kindArg any
	if filter.Kind != "" {
		kindArg = string(filter.Kind)
	}

	var total int
	if err := db.QueryRow(ctx, countSQL, filter.EnabledOnly, kindArg).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("cadence: count: %w", err)
	}

	rows, err := db.Query(ctx, listSQL, filter.EnabledOnly, kindArg, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("cadence: list: %w", err)
	}
	defer rows.Close()

	out := make([]*Cadence, 0, limit)
	for rows.Next() {
		c, scanErr := scanCadence(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("cadence: list scan: %w", scanErr)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("cadence: list iter: %w", err)
	}
	return out, total, nil
}

const updateSQL = `
UPDATE cadences SET
    name                 = $2,
    enabled              = $3,
    schedule_kind        = $4,
    interval_seconds     = $5,
    cron_expr            = $6,
    overlap_policy       = $7,
    kind                 = $8,
    scenario_name        = $9,
    module               = $10,
    target               = $11,
    input                = $12,
    batch_mode             = $13,
    batch_size             = $14,
    batch_percent          = $15,
    concurrency            = $16,
    fail_threshold         = $17,
    fail_threshold_percent = $18,
    inter_batch_interval   = $19::interval,
    inter_unit_interval    = $20::interval,
    require_alive          = $21,
    on_failure             = $22,
    next_run_at            = $23,
    last_run_at            = $24,
    updated_at             = NOW()
WHERE id = $1
RETURNING created_at, updated_at
`

// Update переписывает Cadence целиком (full-replace, parity рецепт-полей Insert).
// Прогоняет [validate]. created_by_aid не меняется (владелец фиксирован).
// [ErrCadenceNotFound] если строки нет (0 строк RETURNING). created_at/updated_at
// перечитываются.
func Update(ctx context.Context, db ExecQueryRower, c *Cadence) error {
	if err := validate(c); err != nil {
		return err
	}

	args := append([]any{c.ID}, recipeArgs(c)...)
	row := db.QueryRow(ctx, updateSQL, args...)
	if err := row.Scan(&c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCadenceNotFound
		}
		return mapWriteError(err)
	}
	return nil
}

const setEnabledSQL = `
UPDATE cadences SET
    enabled    = $2,
    updated_at = NOW()
WHERE id = $1
`

// SetEnabled переключает enabled-флаг (пауза/возобновление расписания) без полной
// перезаписи рецепта. [ErrCadenceNotFound] если строки нет (0 строк).
func SetEnabled(ctx context.Context, db ExecQueryRower, id string, enabled bool) error {
	if id == "" {
		return fmt.Errorf("cadence: empty id")
	}
	tag, err := db.Exec(ctx, setEnabledSQL, id, enabled)
	if err != nil {
		return fmt.Errorf("cadence: set enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCadenceNotFound
	}
	return nil
}

const selectDueForUpdateSQL = `SELECT ` + selectColumns + `
FROM cadences
WHERE enabled AND next_run_at IS NOT NULL AND next_run_at <= NOW()
ORDER BY next_run_at ASC
FOR UPDATE SKIP LOCKED
LIMIT $1
`

// SelectDueForUpdate читает due-расписания (enabled И next_run_at <= NOW()) под
// FOR UPDATE SKIP LOCKED — основа spawn-tx Reaper-правила spawn_due_cadence
// (ADR-046 §4). SKIP LOCKED отдаёт single-executor-семантику даже если несколько
// Keeper-инстансов случайно вошли в spawn одновременно (хотя нормально спавнит
// только Reaper-лидер): строка, залоченная одной tx, пропускается другой.
//
// db ОБЯЗАН быть pgx.Tx (FOR UPDATE-лок держится до конца транзакции, в которой
// идёт спавн + advance расписания). limit — потолок числа расписаний за один тик
// (batch-guard против лавины при долгом downtime).
func SelectDueForUpdate(ctx context.Context, db ExecQueryRower, limit int) ([]*Cadence, error) {
	rows, err := db.Query(ctx, selectDueForUpdateSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("cadence: select due: %w", err)
	}
	defer rows.Close()

	var out []*Cadence
	for rows.Next() {
		c, scanErr := scanCadence(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("cadence: select due scan: %w", scanErr)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cadence: select due iter: %w", err)
	}
	return out, nil
}

// cronGranularity — фиксированный вклад cron-правил в derivedMinPeriod. cron-
// расписания имеют interval_seconds = NULL (в MIN не попадают), но cron-
// гранулярность — минута: чтобы не промахнуться мимо ближайшего cron-слота,
// при наличии хоть одного enabled cron-правила Conductor опрашивается не реже
// раза в минуту (ADR-048 «Adaptive interval»).
const cronGranularity = 60 * time.Second

// MinPeriod — сырой результат [SelectMinPeriod]: агрегаты по enabled-реестру,
// из которых вызывающий выводит derivedMinPeriod (см. [DerivedMinPeriod]).
type MinPeriod struct {
	// MinIntervalSeconds — MIN(interval_seconds) по enabled interval-правилам.
	// nil, если ни одного enabled interval-правила нет (все cron или реестр пуст).
	MinIntervalSeconds *int
	// HasCron — есть ли хоть одно enabled cron-правило (вклад cronGranularity).
	HasCron bool
}

// Empty сообщает, что enabled-реестр пуст: ни interval-, ни cron-правил. В этом
// случае derivedMinPeriod не определён, и Conductor опрашивается с poll_idle
// (ADR-048 idle-вариант (a): тот же MIN-запрос несёт сигнал «пусто», нового
// Redis-канала нет).
func (p MinPeriod) Empty() bool {
	return p.MinIntervalSeconds == nil && !p.HasCron
}

// DerivedMinPeriod вычисляет derivedMinPeriod (ADR-048): минимальный «нужный»
// шаг опроса по enabled-реестру. ok=false → реестр пуст (вызывающий берёт
// poll_idle). Иначе:
//   - p = MIN(interval_seconds), если есть enabled interval-правила;
//   - при наличии enabled cron-правила — p = min(p, 60s) (cron-гранулярность
//     минута; если interval-правил нет — p = 60s).
//
// Floor (interval_seconds ≥ 30) тут НЕ применяется — это clamp-нижняя-граница
// опроса (см. [Clamp]) и отдельная валидация создания (Pass B / ADR-046).
func (p MinPeriod) DerivedMinPeriod() (d time.Duration, ok bool) {
	if p.Empty() {
		return 0, false
	}
	if p.MinIntervalSeconds == nil {
		// Только cron-правила (interval_seconds NULL не попадают в MIN): шаг —
		// cron-гранулярность (минута).
		return cronGranularity, true
	}
	d = time.Duration(*p.MinIntervalSeconds) * time.Second
	if p.HasCron && cronGranularity < d {
		// Есть и interval-, и cron-правила: берём более частый из двух.
		d = cronGranularity
	}
	return d, true
}

const selectMinPeriodSQL = `
SELECT MIN(interval_seconds), bool_or(schedule_kind = 'cron')
FROM cadences
WHERE enabled
`

// SelectMinPeriod агрегирует enabled-реестр для адаптивного шага опроса Conductor
// (ADR-048 «Adaptive interval»): MIN(interval_seconds) по enabled interval-
// правилам + флаг наличия enabled cron-правил. Один лёгкий aggregate-SELECT без
// FOR UPDATE — летит только с Conductor-лидера в IntervalFn (не-лидеры IntervalFn
// не зовут), лишней нагрузки на PG нет.
//
// Пустой реестр (нет enabled-строк): MIN → NULL, bool_or → NULL; обе сводятся к
// «пусто» ([MinPeriod.Empty]). Stateless by construction — новый Conductor-лидер
// после failover пересчитывает шаг из PG, не неся in-memory состояния опроса.
func SelectMinPeriod(ctx context.Context, db ExecQueryRower) (MinPeriod, error) {
	var (
		minIv   *int
		hasCron *bool
	)
	if err := db.QueryRow(ctx, selectMinPeriodSQL).Scan(&minIv, &hasCron); err != nil {
		return MinPeriod{}, fmt.Errorf("cadence: select min period: %w", err)
	}
	mp := MinPeriod{MinIntervalSeconds: minIv}
	if hasCron != nil {
		mp.HasCron = *hasCron
	}
	return mp, nil
}

// Clamp ограничивает шаг опроса коридором [floor, ceiling] (профиль «Спокойный»:
// 30s/60s, ADR-048). Floor-clamp — defence-in-depth backstop на случай, если
// суб-floor строка обошла write-path floor-reject и DB-CHECK (Pass B их отвергает,
// но clamp всё равно держит опрос не ниже floor): derivedMinPeriod < floor
// (например interval=10 при floor=30) опрашивается раз в 30s. Ceiling-cap не даёт
// редким расписаниям (interval=1h) растягивать опрос настолько, что
// NextRunAnchored missed-slot становится единственной страховкой.
func Clamp(d, floor, ceiling time.Duration) time.Duration {
	if d < floor {
		return floor
	}
	if d > ceiling {
		return ceiling
	}
	return d
}

const advanceScheduleSQL = `
UPDATE cadences SET
    next_run_at = $2,
    last_run_at = $3,
    updated_at  = NOW()
WHERE id = $1
`

// AdvanceSchedule двигает расписание после due-обработки: пересчитанный
// next_run_at + last_run_at (момент спавна; nil для overlap-skip без спавна —
// caller передаёт NextRun-результат как next, но last_run может остаться прежним).
// Вызывается в той же spawn-tx, что Insert порождённого Voyage (атомарность
// против задвоения, ADR-046 §4). [ErrCadenceNotFound] при 0 строк.
func AdvanceSchedule(ctx context.Context, db ExecQueryRower, id string, nextRunAt time.Time, lastRunAt *time.Time) error {
	if id == "" {
		return fmt.Errorf("cadence: empty id")
	}
	var lastArg any
	if lastRunAt != nil {
		lastArg = lastRunAt.UTC()
	}
	tag, err := db.Exec(ctx, advanceScheduleSQL, id, nextRunAt.UTC(), lastArg)
	if err != nil {
		return fmt.Errorf("cadence: advance schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCadenceNotFound
	}
	return nil
}

const hasLiveChildSQL = `
SELECT EXISTS(
    SELECT 1 FROM voyages
    WHERE cadence_id = $1
      AND status NOT IN ('succeeded', 'failed', 'partial_failed', 'cancelled')
)
`

// HasLiveChild сообщает, есть ли у Cadence НЕтерминальный порождённый Voyage
// (pending/scheduled/running) — overlap-проверка для skip/queue-политик (ADR-046
// §5). Терминальный набор совпадает с voyage.IsTerminal. Вызывается в spawn-tx
// (та же транзакция, что Insert/AdvanceSchedule), поэтому видит свежесозданные в
// этом тике строки и не задваивает спавн.
func HasLiveChild(ctx context.Context, db ExecQueryRower, cadenceID string) (bool, error) {
	if cadenceID == "" {
		return false, fmt.Errorf("cadence: empty cadence_id")
	}
	var live bool
	if err := db.QueryRow(ctx, hasLiveChildSQL, cadenceID).Scan(&live); err != nil {
		return false, fmt.Errorf("cadence: has live child: %w", err)
	}
	return live, nil
}

const deleteSQL = `DELETE FROM cadences WHERE id = $1`

// Delete снимает расписание. Порождённые Voyage остаются (FK
// voyages.cadence_id ON DELETE SET NULL — дети-сироты, ADR-046 §9).
// [ErrCadenceNotFound] если строки нет (0 строк).
func Delete(ctx context.Context, db ExecQueryRower, id string) error {
	if id == "" {
		return fmt.Errorf("cadence: empty id")
	}
	tag, err := db.Exec(ctx, deleteSQL, id)
	if err != nil {
		return fmt.Errorf("cadence: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCadenceNotFound
	}
	return nil
}
