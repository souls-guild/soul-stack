package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// handleRunResult — обработчик payload-а [keeperv1.RunResult] (M2.4).
//
// PM-decision (4):
//   - SUCCESS  → UPDATE incarnation.state + state_history + status=ready.
//   - FAILED / CANCELLED / ERROR_LOCKED → UPDATE status=error_locked (state не
//     меняем — храним последний known-good snapshot).
//
// Атомарность — одна транзакция через [pgx.BeginFunc]:
//   - INSERT state_history;
//   - UPDATE incarnation.state/status/status_details.
//
// Audit `run.completed` пишется **после** commit-а — DB-консистентность не
// зависит от write-path-а аудита (паттерн идентичен Bootstrap).
//
// Адресация incarnation (M2.x): `apply_id` тащит сам Soul, имя incarnation
// в proto отсутствует (см. apply.proto: RunResult содержит только
// apply_id/status/state_changes). Correlation закрывает таблица `apply_runs`
// (миграция 018): scenario-runner пишет строку `(apply_id, sid)` при
// dispatch-е `ApplyRequest`, а этот handler читает её через
// [applyrun.SelectIncarnationByApplyID] и переводит в терминальный статус
// ([correlateRunResult]). apply_id, не найденный в `apply_runs` (ad-hoc push
// без scenario-runner-а) → log+skip.
//
// Коммит `incarnation.state` (применение `state_changes` по scenario-DSL) —
// зона scenario-runner-а (.g): он владеет cross-host final-barrier-ом
// (docs/scenario/orchestration.md §7), коммитит state один раз после
// безусловного барьера и вызывает [commitRunState] с уже merged-state-ом.
// Здесь state НЕ трогаем, чтобы не нарушить barrier-инвариант на multi-host.
func (h *eventStreamHandler) handleRunResult(ctx context.Context, sid, sessionID string, ev *keeperv1.RunResult) {
	if ev == nil {
		h.logger.Warn("eventstream: RunResult payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	payload := map[string]any{
		"sid":      sid,
		"apply_id": ev.GetApplyId(),
		"status":   ev.GetStatus().String(),
	}
	if sc := ev.GetStateChanges(); sc != nil {
		if b, err := protojson.Marshal(sc); err != nil {
			h.logger.Warn("eventstream: state_changes marshal failed",
				slog.String("sid", sid),
				slog.String("apply_id", ev.GetApplyId()),
				slog.Any("error", err),
			)
		} else {
			payload["state_changes"] = string(b)
		}
	}

	if err := h.deps.AuditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventRunCompleted,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: ev.GetApplyId(),
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		h.logger.Warn("eventstream: audit write run.completed failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Any("error", err),
		)
	}

	h.publishRunResult(sid, ev)
	h.correlateRunResult(ctx, sid, sessionID, ev)
}

// correlateRunResult резолвит incarnation по `(apply_id, sid)` через реестр
// `apply_runs` и переводит строку прогона в терминальный статус
// (`success`/`failed`/`cancelled`). Закрывает correlation-разрыв «Keeper не
// знает, к какой incarnation относится прогон».
//
// ApplyRunDB=nil (unit-сборка без PG / ad-hoc push) → no-op.
// apply_id не найден в `apply_runs` (push без scenario-runner-а) → log+skip.
func (h *eventStreamHandler) correlateRunResult(ctx context.Context, sid, sessionID string, ev *keeperv1.RunResult) {
	if h.deps.ApplyRunDB == nil {
		return
	}
	applyID := ev.GetApplyId()
	name, scenario, rowAttempt, err := applyrun.SelectIncarnationByApplyID(ctx, h.deps.ApplyRunDB, applyID, sid)
	if err != nil {
		if errors.Is(err, applyrun.ErrApplyRunNotFound) {
			h.logger.Info("eventstream: RunResult без apply_runs-строки — correlation пропущена",
				slog.String("sid", sid),
				slog.String("session_id", sessionID),
				slog.String("apply_id", applyID))
			return
		}
		h.logger.Warn("eventstream: resolve incarnation по apply_id failed",
			slog.String("sid", sid),
			slog.String("apply_id", applyID),
			slog.Any("error", err))
		return
	}

	// epoch-check (gate-1, ADR-027(g)): результат от устаревшей попытки не коммитим.
	//   recvAttempt == 0          → старый Soul без эхо (forward-compat) → коммитим;
	//   recvAttempt <  rowAttempt → stale: существует пере-claim с бОльшим epoch →
	//                               DROP (метрика keeper_runresult_stale_total),
	//                               state НЕ коммитим;
	//   recvAttempt == rowAttempt → актуально → коммитим;
	//   recvAttempt >  rowAttempt → невозможный инвариант (apply_runs.attempt растёт
	//                               только вверх при claim, RunResult.attempt — эхо
	//                               захваченного epoch) → defensive warn + всё равно
	//                               коммитим (fail-safe: не теряем результат живого
	//                               прогона из-за рассинхрона/аномалии чтения строки).
	recvAttempt := ev.GetAttempt()
	if recvAttempt != 0 && recvAttempt < rowAttempt {
		h.deps.Metrics.ObserveRunResultStale()
		h.logger.Info("eventstream: RunResult от устаревшей попытки — stale-drop (commit отвергнут)",
			slog.String("sid", sid),
			slog.String("apply_id", applyID),
			slog.Int("recv_attempt", int(recvAttempt)),
			slog.Int("row_attempt", int(rowAttempt)),
			slog.String("incarnation", name),
			slog.String("scenario", scenario))
		return
	}
	if recvAttempt > rowAttempt {
		// Инвариант «attempt только растёт» нарушен: keeper↔soul рассинхрон epoch-а
		// либо аномалия чтения строки. Не дропаем — коммитим (fail-safe), но
		// сигнализируем нарушение инварианта для триажа.
		h.logger.Warn("eventstream: RunResult.attempt больше attempt строки — нарушен инвариант «attempt только растёт» (commit fail-safe)",
			slog.String("sid", sid),
			slog.String("apply_id", applyID),
			slog.Int("recv_attempt", int(recvAttempt)),
			slog.Int("row_attempt", int(rowAttempt)))
	}

	status := runStatusToApplyStatus(ev.GetStatus())
	// error_summary НЕ перезаписываем здесь: при FAILED причина уже записана
	// per-task-ом ([recordTaskFailure] → RecordTaskFailure) и несёт idx+module+
	// message упавшей задачи (BUG-3). UpdateStatus COALESCE-ит — nil не затирает
	// уже записанное. Если TaskEvent-а с ошибкой не было (dispatch-level фейл),
	// error_summary остаётся NULL, и barrier classify подставит сам статус
	// (`failed`) — без бессмысленного `run_status=RUN_STATUS_FAILED`.
	if err := applyrun.UpdateStatus(ctx, h.deps.ApplyRunDB, applyID, sid, status, nil); err != nil {
		// Append-only single-winner (ADR-027(j)): строку уже перевёл в терминал
		// другой обработчик (recovery-перехват / повторный RunResult). НЕ ошибка
		// — первый победил, дубль-терминал не записываем. Логируем как no-op.
		if errors.Is(err, applyrun.ErrApplyRunAlreadyTerminal) {
			h.logger.Info("eventstream: apply_runs уже терминальна — correlation no-op (первый коммиттер победил)",
				slog.String("sid", sid),
				slog.String("apply_id", applyID),
				slog.String("incarnation", name),
				slog.String("scenario", scenario))
			return
		}
		h.logger.Warn("eventstream: update apply_runs status failed",
			slog.String("sid", sid),
			slog.String("apply_id", applyID),
			slog.String("incarnation", name),
			slog.String("scenario", scenario),
			slog.Any("error", err))
		return
	}
	h.logger.Info("eventstream: apply_runs correlated",
		slog.String("sid", sid),
		slog.String("apply_id", applyID),
		slog.String("incarnation", name),
		slog.String("scenario", scenario),
		slog.String("status", string(status)))
}

// runStatusToApplyStatus маппит [keeperv1.RunStatus] на [applyrun.Status].
// FAILED / ERROR_LOCKED / прочее → failed (terminal lock); CANCELLED →
// cancelled; SUCCESS → success.
func runStatusToApplyStatus(rs keeperv1.RunStatus) applyrun.Status {
	switch rs {
	case keeperv1.RunStatus_RUN_STATUS_SUCCESS:
		return applyrun.StatusSuccess
	case keeperv1.RunStatus_RUN_STATUS_CANCELLED:
		return applyrun.StatusCancelled
	default:
		return applyrun.StatusFailed
	}
}

// publishRunResult транслирует RunResult в SSE-канал через applybus,
// классифицируя статус прогона:
//
//   - RUN_STATUS_SUCCESS          → apply.completed
//   - RUN_STATUS_CANCELLED        → apply.cancelled
//   - RUN_STATUS_FAILED/ERROR_LOCKED/прочее → apply.failed
//
// ApplyBus=nil (dev без SSE) → no-op.
func (h *eventStreamHandler) publishRunResult(sid string, ev *keeperv1.RunResult) {
	if h.deps.ApplyBus == nil {
		return
	}
	var kind applybus.EventKind
	switch ev.GetStatus() {
	case keeperv1.RunStatus_RUN_STATUS_SUCCESS:
		kind = applybus.KindApplyCompleted
	case keeperv1.RunStatus_RUN_STATUS_CANCELLED:
		kind = applybus.KindApplyCancelled
	default:
		kind = applybus.KindApplyFailed
	}

	payload := map[string]any{
		"apply_id":   ev.GetApplyId(),
		"kind":       string(kind),
		"sid":        sid,
		"run_status": ev.GetStatus().String(),
	}
	if sc := ev.GetStateChanges(); sc != nil {
		if b, err := protojson.Marshal(sc); err == nil {
			var asMap map[string]any
			if jerr := json.Unmarshal(b, &asMap); jerr == nil {
				payload["state_changes"] = asMap
			} else {
				payload["state_changes"] = string(b)
			}
		}
	}

	h.deps.ApplyBus.Publish(applybus.Event{
		ApplyID: ev.GetApplyId(),
		Kind:    kind,
		Payload: payload,
	})
}

// commitRunState — атомарный commit результатов прогона в incarnation.
// Вынесен публично для будущего scenario-runner-а (M0.6c-2), который владеет
// mapping-ом apply_id ↔ incarnation и оркестрирует apply от Operator API
// до RunResult.
//
// pool — *pgxpool.Pool или совместимый. scenario / name / applyID берутся
// у caller-а из incarnation-state-таблицы. stateBefore — текущее значение
// `incarnation.state` (читается под SELECT FOR UPDATE внутри транзакции);
// stateAfter — результат merge `stateBefore + RunResult.state_changes`
// (caller сам делает merge, потому что грамматика state_changes — scenario-DSL,
// не gRPC-контракт).
//
// На RUN_STATUS_SUCCESS статус становится `ready`; на остальные —
// `error_locked` со status_details, чтобы триаж видел причину.
//
// Single-winner (ADR-027(j) W1): [incarnation.UpdateStateFromRun] коммитит под
// guard `status IN ('applying','destroying')`. Если строку уже вывел из
// applying другой коммиттер (recovery-перехват), вернётся
// [incarnation.ErrAlreadyFinalized] — caller обязан трактовать его как no-op
// (логировать, не валить путь), а не как ошибку консистентности.
func commitRunState(
	ctx context.Context,
	pool TxBeginner,
	name, scenario, applyID, historyID string,
	stateBefore, stateAfter map[string]any,
	runStatus keeperv1.RunStatus,
) error {
	status := incarnation.StatusReady
	var details map[string]any
	switch runStatus {
	case keeperv1.RunStatus_RUN_STATUS_SUCCESS:
		// stateAfter уже учитывает state_changes.
	default:
		status = incarnation.StatusErrorLocked
		details = map[string]any{
			"reason":     "run_failed",
			"run_status": runStatus.String(),
			"apply_id":   applyID,
		}
		// На ошибку state НЕ перезаписываем — оставляем stateBefore.
		stateAfter = stateBefore
	}

	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return incarnation.UpdateStateFromRun(
			ctx, tx,
			name, scenario, applyID,
			stateBefore, stateAfter,
			status, details,
			nil, // soul_grpc — без AID Архонта.
			historyID,
		)
	})
}
