package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errandsExecer — узкая поверхность pgxpool.Pool, нужная правилу
// `purge_old_errands`. Сужение позволяет fake в unit-тестах без поднятия
// Postgres; реальный *pgxpool.Pool удовлетворяет интерфейсу автоматически
// (паттерн queryRower / PushRunCanceller).
type errandsExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// purgeOldErrandsSQL — `DELETE FROM errands WHERE ttl_at < NOW()`. `ttl_at`
// заполняется при INSERT-е (`started_at + reaper.errands.ttl`, по умолчанию
// 7д — см. errand.TTLDefault), индекс `errands_ttl_idx` (миграция 052)
// делает условие cheap-scanable. Параметр `max_age` правила в предикате
// НЕ участвует (TTL зашит в строку при создании); параметр остаётся в
// конфиге для совместимости с runDurationRule-runner-ом и как documented
// override для будущих миграций ttl-логики.
const purgeOldErrandsSQL = `DELETE FROM errands WHERE ttl_at < NOW()`

// ErrandsPurger — реализация правила `purge_old_errands` (ADR-033,
// docs/keeper/reaper.md → §purge_old_errands). Один батч-проход = один
// DELETE по индексу `errands_ttl_idx`. Сигнатура Run совместима с
// runDurationRule-вызовом Runner-а.
//
// Параметр `maxAge` правила в предикат НЕ входит: TTL зашит в `ttl_at`
// при INSERT-е dispatcher-ом (`started_at + errand.TTLDefault`). Сохраняем
// аргумент в сигнатуре для совместимости с общим duration-runner-ом, но
// тело его игнорирует. `batchSize` тоже не используется (`DELETE` режет
// одним SQL-statement-ом; для миллионов строк TTL-индекса это не проблема,
// для миллиардов потребуется partitioning — отдельная задача).
type ErrandsPurger struct {
	pool   errandsExecer
	logger *slog.Logger
}

// NewErrandsPurger конструирует purger. logger nil-safe (warn-ы
// подавляются).
func NewErrandsPurger(pool *pgxpool.Pool, logger *slog.Logger) *ErrandsPurger {
	return &ErrandsPurger{pool: pool, logger: logger}
}

// newErrandsPurgerFromExecer — внутренний конструктор для unit-тестов.
// Публичный [NewErrandsPurger] фиксирует *pgxpool.Pool, чтобы caller-ы не
// цеплялись за расширение интерфейса.
func newErrandsPurgerFromExecer(pool errandsExecer, logger *slog.Logger) *ErrandsPurger {
	return &ErrandsPurger{pool: pool, logger: logger}
}

// Run выполняет одну итерацию правила: `DELETE FROM errands WHERE ttl_at <
// NOW()`. Возвращает (affected, err): affected — число фактически удалённых
// строк. callers (Runner.runDurationRule) сложат это в keeper_reaper_*-метрики.
//
// Сигнатура совместима с runDurationRule (`(ctx, duration, batch) → (int64, error)`),
// аргументы maxAge/batchSize игнорируются (см. doc-comment типа).
func (p *ErrandsPurger) Run(ctx context.Context, _ time.Duration, _ int) (int64, error) {
	tag, err := p.pool.Exec(ctx, purgeOldErrandsSQL)
	if err != nil {
		return 0, fmt.Errorf("reaper.purge_old_errands: %w", err)
	}
	return tag.RowsAffected(), nil
}
