package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ephemeralTidingsExecer — узкая поверхность pgxpool.Pool, нужная правилу
// `purge_orphan_ephemeral_tidings`. Сужение позволяет fake в unit-тестах без
// поднятия Postgres; реальный *pgxpool.Pool удовлетворяет автоматически
// (паттерн errandsExecer / orphanPurger).
type ephemeralTidingsExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// purgeOrphanEphemeralTidingsSQL — снос осиротевших ephemeral-Tiding-ов
// (ADR-052(g) amendment N2, очистка). Ephemeral-правило привязано к одному
// Voyage; терминал прогона должен унести его подписку. Это правило — СТРАХОВКА:
// сносит ephemeral-строку, если её прогон либо
//   - не существует (строка voyages была удалена / прогон так и не создался —
//     откат tx исключает осиротелость by construction, но защита от ручного
//     вмешательства / будущих путей);
//   - в ТЕРМИНАЛЕ дольше grace-периода ($1) — за grace tap гарантированно успел
//     сматчить терминальное событие против правила и заэнкьюить уведомление
//     (dispatcher работает асинхронно через bounded-канал; синхронный снос в
//     момент finalize опередил бы доставку — см. ADR-052(g) «Очистка»).
//
// Grace — обязательное условие корректности, а не косметика: без него правило,
// сработав ровно на терминал, удалило бы Tiding ДО того, как tap-consumer
// дочитает событие из буфера → уведомление о завершении не ушло бы.
//
// Использует partial-индекс `tidings_ephemeral_voyage_idx` (миграция 072,
// WHERE ephemeral): постоянные правила в скан не попадают. Один DELETE одним
// statement-ом (ephemeral-правил мало — десятки на in-flight прогоны).
const purgeOrphanEphemeralTidingsSQL = `
DELETE FROM tidings t
WHERE t.ephemeral
  AND (
        NOT EXISTS (
            SELECT 1 FROM voyages v WHERE v.voyage_id = t.voyage_id
        )
        OR EXISTS (
            SELECT 1 FROM voyages v
            WHERE v.voyage_id = t.voyage_id
              AND v.status IN ('succeeded', 'failed', 'partial_failed', 'cancelled')
              AND v.finished_at < NOW() - $1::interval
        )
      )`

// EphemeralTidingsPurger — реализация правила `purge_orphan_ephemeral_tidings`
// (ADR-052(g) amendment N2, docs/keeper/reaper.md). Один батч-проход = один
// DELETE по partial-индексу `tidings_ephemeral_voyage_idx`. Сигнатура Run
// совместима с runDurationRule-вызовом Runner-а (parity ErrandsPurger /
// orphanPurger).
//
// В отличие от `purge_old_errands` (TTL зашит в строку), здесь `maxAge` правила
// — это GRACE после терминала Voyage, ВХОДЯЩИЙ в предикат как интервал (parity
// `purge_apply_task_register`: max_age-as-grace). batchSize не используется
// (один DELETE-statement; ephemeral-правил мало).
type EphemeralTidingsPurger struct {
	pool   ephemeralTidingsExecer
	logger *slog.Logger
}

// NewEphemeralTidingsPurger конструирует purger. logger nil-safe.
func NewEphemeralTidingsPurger(pool *pgxpool.Pool, logger *slog.Logger) *EphemeralTidingsPurger {
	return &EphemeralTidingsPurger{pool: pool, logger: logger}
}

// newEphemeralTidingsPurgerFromExecer — внутренний конструктор для unit-тестов.
// Публичный [NewEphemeralTidingsPurger] фиксирует *pgxpool.Pool, чтобы caller-ы
// не цеплялись за расширение интерфейса.
func newEphemeralTidingsPurgerFromExecer(pool ephemeralTidingsExecer, logger *slog.Logger) *EphemeralTidingsPurger {
	return &EphemeralTidingsPurger{pool: pool, logger: logger}
}

// Run выполняет одну итерацию правила: снос осиротевших ephemeral-Tiding-ов
// (прогон не существует ИЛИ в терминале > grace). grace передаётся как интервал
// в предикат. Возвращает (affected, err): affected — число удалённых строк
// (Runner.runDurationRule сложит в keeper_reaper_*-метрики).
//
// Сигнатура совместима с runDurationRule (`(ctx, duration, batch) → (int64, error)`);
// аргумент batchSize игнорируется (см. doc-comment типа).
func (p *EphemeralTidingsPurger) Run(ctx context.Context, grace time.Duration, _ int) (int64, error) {
	// pgx принимает time.Duration как Postgres-interval напрямую (microsecond-точность).
	tag, err := p.pool.Exec(ctx, purgeOrphanEphemeralTidingsSQL, grace)
	if err != nil {
		return 0, fmt.Errorf("reaper.purge_orphan_ephemeral_tidings: %w", err)
	}
	return tag.RowsAffected(), nil
}
