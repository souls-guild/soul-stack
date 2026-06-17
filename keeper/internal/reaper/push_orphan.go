package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// PushRunCanceller — узкая поверхность [pushorch.Store] для reaper-purger-а.
// Сужено до двух методов (ListOrphans/CancelOrphan), нужных правилу
// `purge_orphan_push_runs`. Реальная реализация — *pushorch.Store; fake в
// unit-тестах. Имена и сигнатуры — те же, что в pushorch.Store, чтобы wire-up
// в daemon.setupReaper передавал её напрямую без адаптера.
//
// Вынесено в reaper-пакет (а НЕ pushorch), чтобы reaper не получал
// перекрёстный импорт pushorch (drift-vector через сlojure-связь модулей).
type PushRunCanceller interface {
	ListOrphans(ctx context.Context, maxAge time.Duration, batchSize int) ([]string, error)
	CancelOrphan(ctx context.Context, applyID, reason string) (bool, error)
}

// purgeOrphanPushRunsReason — фиксированная причина, попадающая в
// push_runs.summary.reason для всех осиротевших прогонов. Reaper не имеет
// контекста почему именно Keeper-инстанс умер (KID погас, ctx отменён, OOM);
// важно лишь то, что прогон находился в in-flight-статусе дольше TTL.
const purgeOrphanPushRunsReason = "orphan_purged_by_reaper"

// PurgeOrphanPushRuns — реализация правила `purge_orphan_push_runs`
// (docs/keeper/reaper.md, registered в Runner.dispatch). Находит in-flight
// push-прогоны (status IN pending/running) старше `maxAge` (Keeper, начавший
// прогон, либо умер, либо застрял), переводит каждый в `cancelled` с пометкой
// `orphan_purged: true` в summary.
//
// Один batch == один LIST + per-row UPDATE. Каждый UPDATE — guard по
// status IN (pending,running): single-winner-гонка с реальным MarkTerminal
// проигрывает (RowsAffected==0, считается не-purged).
//
// Возвращает (affected, err): affected — число фактически переведённых
// записей. callers (Runner.runDurationRule) сложат это в keeper_reaper_*-метрики.
type orphanPurger struct {
	store  PushRunCanceller
	logger *slog.Logger
}

// NewOrphanPushRunsPurger конструирует purger. logger nil-safe (warn-ы
// подавляются).
func NewOrphanPushRunsPurger(store PushRunCanceller, logger *slog.Logger) *orphanPurger {
	return &orphanPurger{store: store, logger: logger}
}

// Run выполняет одну итерацию правила. Сигнатура совместима с
// runDurationRule-вызовом (Runner.dispatch::case "purge_orphan_push_runs").
func (p *orphanPurger) Run(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	ids, err := p.store.ListOrphans(ctx, maxAge, batchSize)
	if err != nil {
		return 0, fmt.Errorf("reaper.purge_orphan_push_runs: list: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	var affected int64
	for _, id := range ids {
		ok, cerr := p.store.CancelOrphan(ctx, id, purgeOrphanPushRunsReason)
		if cerr != nil {
			// Сбой одного per-row UPDATE-а: продолжаем по списку (best-effort,
			// как в Purger.PurgeApplyRuns batch-цикле). Каждый сбой логируем —
			// агрегация ошибок в одно сообщение запутывает наблюдаемость.
			if p.logger != nil {
				p.logger.Warn("reaper: purge_orphan_push_runs cancel failed",
					slog.String("apply_id", id),
					slog.Any("error", cerr))
			}
			continue
		}
		if ok {
			affected++
		}
		// ok=false — single-winner-гонка с реальным MarkTerminal (orchestrator
		// успел финализировать запись между ListOrphans и CancelOrphan).
		// Это норма, не ошибка.
	}
	return affected, nil
}
