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

// PG error codes (parity voyage/crud.go).
const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower is the narrow subset of pgxpool.Pool the CRUD needs (parity
// voyage.ExecQueryRower minus CopyFrom: cadences has no batch unit insert).
// Unit tests go through a fake; production supplies a real pool/Conn/Tx.
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

// validate is the common Cadence recipe/schedule check (Insert + Update). A
// pure function of the fields, no PG. Invariants:
//   - non-empty ID / Name / CreatedByAID; valid enums (schedule_kind /
//     overlap_policy / kind / batch_mode / on_failure);
//   - schedule_kind ↔ interval/cron XOR (interval ⇒ only interval_seconds>0;
//     cron ⇒ only non-empty cron_expr) — parity CHECK
//     `cadences_schedule_consistency`, but a "friendly" error ahead of PG;
//   - kind ↔ scenario_name/module (parity voyage.Insert);
//   - non-empty Target; sane bounds on batch_size/percent/concurrency/fail_threshold.
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
		// Reject a broken cron here, ahead of PG: the migration 066 CHECK
		// invariant doesn't parse cron grammar, and the scheduler (NextRun)
		// needs a valid expr (ADR-046 §2).
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

// ValidateIntervalFloor checks the lower bound on an interval-Cadence period
// (floor limit, ADR-046 Pass B): `interval_seconds >= floorSeconds`.
// floorSeconds comes from the same `cadence_scheduler.poll_floor` config as
// Conductor's adaptive polling (a single minimum, not a 30 hardcoded in two
// places), passed in by the write-path handler. floorSeconds <= 0 → check
// disabled (the library Insert/Update and unit/integration tests that need
// to insert a sub-floor row for clamp testing don't set a floor).
// cron-Cadence (interval_seconds == NULL) and non-interval schedule_kind are
// unaffected (cron granularity of a minute ≥ floor).
//
// Returns a plain `cadence: …` error (like [validate]): the handler maps it
// to 422 the same way as other recipe validate errors.
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

// recipeArgs unpacks the Cadence recipe/schedule into positional SQL
// arguments for INSERT/UPDATE. Nullable fields → nil interface (NULL in the
// DB); INTERVAL is a text literal via pgutil.Interval (parity voyage).
// Returns 23 values in column order from `name` through `last_run_at`. id
// (PK) and created_by_aid (fixed, not written on UPDATE) are added
// separately by the caller: Insert adds both, Update only id.
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

// Insert inserts a new Cadence. id (ULID) is generated by the caller
// (API handler S4); CRUD only checks non-emptiness and dispatches a UNIQUE
// violation to [ErrCadenceExists]. Runs [validate] before SQL. Empty Input →
// CRUD substitutes `{}` (parity DEFAULT). created_at/updated_at are read
// from RETURNING.
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

// Get reads a Cadence by PK. [ErrCadenceNotFound] if absent.
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

// scanCadence is the common scan of a `cadences` row into *Cadence. Used by
// Get and List (via rows.Next). Nullable columns bind through pointers;
// target/input are jsonb bytes; INTERVAL arrives as float seconds (EXTRACT
// EPOCH) → time.Duration.
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

// ListFilter holds the [List] filters. EnabledOnly restricts to enabled
// schedules (false → no enabled filter); Kind is exact match (empty string →
// no filter).
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

// List returns a page of Cadences: filters by enabled and kind (exact).
// Sorted by `created_at DESC`. Total is the row count under the filter
// (separate COUNT). limit/offset are not validated — the caller has already
// run them through the page parser (parity voyage.List).
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

// Update fully rewrites a Cadence (full-replace, parity Insert's recipe
// fields). Runs [validate]. created_by_aid doesn't change (owner is fixed).
// [ErrCadenceNotFound] if the row is missing (0 rows RETURNING).
// created_at/updated_at are re-read.
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

// SetEnabled toggles the enabled flag (pause/resume the schedule) without a
// full recipe rewrite. [ErrCadenceNotFound] if the row is missing (0 rows).
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

// SelectDueForUpdate reads due schedules (enabled AND next_run_at <= NOW())
// under FOR UPDATE SKIP LOCKED — the basis of the Reaper rule
// spawn_due_cadence spawn-tx (ADR-046 §4). SKIP LOCKED gives single-executor
// semantics even if several Keeper instances happen to enter spawn at once
// (though normally only the Reaper leader spawns): a row locked by one tx is
// skipped by another.
//
// db MUST be a pgx.Tx (the FOR UPDATE lock is held until the end of the
// transaction that does the spawn + schedule advance). limit caps the number
// of schedules per tick (batch guard against a storm after long downtime).
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

// cronGranularity is the fixed contribution of cron rules to
// derivedMinPeriod. cron schedules have interval_seconds = NULL (excluded
// from MIN), but cron granularity is a minute: to not miss the nearest cron
// slot, Conductor polls at least once a minute whenever at least one enabled
// cron rule exists (ADR-048 "Adaptive interval").
const cronGranularity = 60 * time.Second

// MinPeriod is the raw result of [SelectMinPeriod]: aggregates over the
// enabled registry, from which the caller derives derivedMinPeriod (see
// [DerivedMinPeriod]).
type MinPeriod struct {
	// MinIntervalSeconds is MIN(interval_seconds) over enabled interval
	// rules. nil if there's no enabled interval rule (all cron, or the
	// registry is empty).
	MinIntervalSeconds *int
	// HasCron reports whether at least one enabled cron rule exists
	// (contributes cronGranularity).
	HasCron bool
}

// Empty reports that the enabled registry is empty: no interval and no cron
// rules. In this case derivedMinPeriod is undefined, and Conductor polls at
// poll_idle (ADR-048 idle variant (a): the same MIN query carries the
// "empty" signal, no new Redis channel).
func (p MinPeriod) Empty() bool {
	return p.MinIntervalSeconds == nil && !p.HasCron
}

// DerivedMinPeriod computes derivedMinPeriod (ADR-048): the minimum
// "needed" poll step over the enabled registry. ok=false → registry is
// empty (caller falls back to poll_idle). Otherwise:
//   - p = MIN(interval_seconds), if there are enabled interval rules;
//   - if an enabled cron rule exists — p = min(p, 60s) (cron granularity is
//     a minute; if there are no interval rules — p = 60s).
//
// The floor (interval_seconds ≥ 30) is NOT applied here — that's the poll
// clamp lower bound (see [Clamp]) and a separate creation-time validation
// (Pass B / ADR-046).
func (p MinPeriod) DerivedMinPeriod() (d time.Duration, ok bool) {
	if p.Empty() {
		return 0, false
	}
	if p.MinIntervalSeconds == nil {
		// Only cron rules (interval_seconds NULL is excluded from MIN): the
		// step is cron granularity (a minute).
		return cronGranularity, true
	}
	d = time.Duration(*p.MinIntervalSeconds) * time.Second
	if p.HasCron && cronGranularity < d {
		// Both interval and cron rules exist: take the more frequent of
		// the two.
		d = cronGranularity
	}
	return d, true
}

const selectMinPeriodSQL = `
SELECT MIN(interval_seconds), bool_or(schedule_kind = 'cron')
FROM cadences
WHERE enabled
`

// SelectMinPeriod aggregates the enabled registry for Conductor's adaptive
// poll step (ADR-048 "Adaptive interval"): MIN(interval_seconds) over
// enabled interval rules + a flag for enabled cron rules. A single light
// aggregate SELECT without FOR UPDATE — only called from the Conductor
// leader's IntervalFn (non-leaders don't call IntervalFn), so no extra load
// on PG.
//
// Empty registry (no enabled rows): MIN → NULL, bool_or → NULL; both reduce
// to "empty" ([MinPeriod.Empty]). Stateless by construction — a new
// Conductor leader after failover recomputes the step from PG, carrying no
// in-memory poll state.
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

// Clamp bounds the poll step to the [floor, ceiling] corridor ("Calm"
// profile: 30s/60s, ADR-048). The floor clamp is a defence-in-depth backstop
// in case a sub-floor row bypassed the write-path floor reject and the
// DB CHECK (Pass B rejects those, but clamp still keeps polling no lower
// than floor): derivedMinPeriod < floor (e.g. interval=10 with floor=30)
// polls every 30s. The ceiling cap stops rare schedules (interval=1h) from
// stretching the poll interval so far that NextRunAnchored's missed-slot
// handling becomes the only safety net.
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

// AdvanceSchedule advances the schedule after due processing: recalculated
// next_run_at + last_run_at (spawn moment; nil for an overlap-skip with no
// spawn — the caller passes the NextRun result as next, but last_run may
// stay unchanged). Called in the same spawn-tx as the spawned Voyage's
// Insert (atomicity against double-spawn, ADR-046 §4). [ErrCadenceNotFound]
// on 0 rows.
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

// HasLiveChild reports whether the Cadence has a non-terminal spawned
// Voyage (pending/scheduled/running) — the overlap check for skip/queue
// policies (ADR-046 §5). The terminal set matches voyage.IsTerminal. Called
// within the spawn-tx (same transaction as Insert/AdvanceSchedule), so it
// sees rows freshly created in this tick and doesn't double-spawn.
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

// Delete removes the schedule. Spawned Voyages remain (FK
// voyages.cadence_id ON DELETE SET NULL — orphaned children, ADR-046 §9).
// [ErrCadenceNotFound] if the row is missing (0 rows).
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
