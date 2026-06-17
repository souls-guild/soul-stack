package grpc

import (
	"context"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// handleWardRoster — обработчик payload-а [keeperv1.WardRoster] (Soul-reconcile,
// ADR-027(g), S6). Soul на (re)connect-е объявляет ведомые apply_id (ReplaceAll);
// Keeper по набору терминалит осиротевшие `dispatched`-строки этого SID-а.
//
// Закрывает dispatched-orphan дыру «Keeper и Soul оба мертвы после отдачи»: строка
// иначе застряла бы в `dispatched` навсегда (reclaim сужен до `claimed`, Reaper
// dispatched-timeout сознательно не делаем). Sweep запускается ТОЛЬКО при приходе
// WardRoster — старый Soul без этого сообщения никогда его не шлёт, и для его
// dispatched-строк sweep не выполняется (fail-safe висяк, forward-compat).
//
// ApplyRunDB=nil (unit-сборка без PG / ad-hoc push без scenario-runner-а) → no-op.
// Авторитет — общая PG: reconnect на любой инстанс кластера сверяет с той же
// таблицей. Single-winner и гонка sweep ↔ RunResult разрешаются фильтром
// `status='dispatched'` внутри [applyrun.OrphanDispatched].
func (h *eventStreamHandler) handleWardRoster(ctx context.Context, sid, sessionID string, ev *keeperv1.WardRoster) {
	if ev == nil {
		h.logger.Warn("eventstream: WardRoster payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if h.deps.ApplyRunDB == nil {
		return
	}

	known := wardRosterToActive(ev)
	orphaned, err := applyrun.OrphanDispatched(ctx, h.deps.ApplyRunDB, sid, known)
	if err != nil {
		h.logger.Warn("eventstream: orphan dispatched sweep failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Int("known", len(known)),
			slog.Any("error", err))
		return
	}
	if orphaned > 0 {
		h.deps.Metrics.ObserveApplyOrphaned(orphaned)
		h.logger.Info("eventstream: dispatched-строки осиротены по WardRoster (Soul-reconcile)",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Int("known", len(known)),
			slog.Int64("orphaned", orphaned))
		return
	}
	h.logger.Debug("eventstream: WardRoster обработан, осиротевших dispatched нет",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.Int("known", len(known)))
}

// wardRosterToActive маппит proto-набор [keeperv1.WardRoster] в доменные
// [applyrun.ActiveApply], изолируя CRUD-слой applyrun от proto-генерации.
// Пустой/nil-набор → nil (явная декларация «ничего не ведётся»).
func wardRosterToActive(ev *keeperv1.WardRoster) []*applyrun.ActiveApply {
	src := ev.GetActive()
	if len(src) == 0 {
		return nil
	}
	out := make([]*applyrun.ActiveApply, 0, len(src))
	for _, a := range src {
		if a == nil {
			continue
		}
		out = append(out, &applyrun.ActiveApply{
			ApplyID: a.GetApplyId(),
			Attempt: a.GetAttempt(),
		})
	}
	return out
}
