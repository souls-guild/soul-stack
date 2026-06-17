package errand

import (
	"context"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// orphanGraceDuration — на сколько разрешается просрочить running-Errand-у
// этого keeper-инстанса до того, как Replay переведёт его в timed_out.
// Параметр выводится из server-cap × 5 (ADR-033 server-cap 300s × 5 = 25 мин)
// — заведомо больше любого реального TimeoutSec пользователя (потолок 300s),
// плюс запас на drift времени и slow-recovery. Значение per-Replay-вызов,
// не constants-пакетная (caller вправе подкрутить через ReplayOptions).
const orphanGraceDuration = 25 * time.Minute

// ReplayOptions — параметры однократного recovery-scan-а при старте keeper-а.
// Все поля опциональны; нулевое значение — sensible default.
type ReplayOptions struct {
	// Grace — на сколько running-Errand этого KID должен быть «просрочен»,
	// чтобы Replay перевёл его в timed_out. nil/0 → [orphanGraceDuration].
	// Симметрично pushorch.purge_orphan_push_runs (ADR-027(b)).
	Grace time.Duration

	// Reason — пометка в error_message переведённых строк. Опциональна;
	// дефолт — "keeper restart: orphan running errand".
	Reason string
}

// Replay переводит «осиротевшие» running-Errand-ы текущего keeper-инстанса
// в timed_out. Источник осиротевших — рестарт процесса: каждая running-
// строка с started_by_kid=self и started_at < now-grace точно не дождётся
// ErrandResult-а (background-горутина умерла вместе с процессом).
//
// Вызов — однократный в setupErrandDispatcher после Store.Insert/Reaper-
// зависимостей, ДО старта HTTP-слушателя (чтобы reaper-purge_old_errands
// и старые running-строки не конкурировали с новыми Dispatch).
//
// Возвращает число переведённых строк (для лога).
func (d *Dispatcher) Replay(ctx context.Context, opts ReplayOptions) (int, error) {
	grace := opts.Grace
	if grace <= 0 {
		grace = orphanGraceDuration
	}
	reason := opts.Reason
	if reason == "" {
		reason = "keeper restart: orphan running errand"
	}

	ids, err := d.deps.Store.SweepOrphanRunning(ctx, d.deps.KID, grace, reason)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	d.deps.Logger.Warn("errand: replay swept orphan running errands",
		slog.String("kid", d.deps.KID),
		slog.Int("count", len(ids)),
		slog.Duration("grace", grace))

	// Audit-events: одно `errand.timed_out` на каждую осиротевшую строку.
	// archon_aid в payload не кладём (orphan-purge — keeper-internal путь,
	// инициатора-Архонта на этом write-path-е больше нет; sourceSoulGRPC —
	// тот же канал, что live-timed_out-handler).
	if d.deps.Audit != nil {
		for _, id := range ids {
			payload := map[string]any{
				"errand_id": id,
				"reason":    reason,
				"orphan":    true,
			}
			ev := &audit.Event{
				EventType:     audit.EventTypeErrandTimedOut,
				Source:        audit.SourceSoulGRPC,
				CorrelationID: id,
				Payload:       payload,
			}
			if werr := d.deps.Audit.Write(ctx, ev); werr != nil {
				d.deps.Logger.Warn("errand: audit orphan-timed_out failed",
					slog.String("errand_id", id),
					slog.Any("error", werr))
			}
		}
	}
	return len(ids), nil
}
