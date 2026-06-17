package errand

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// CancelRequest — вход [Dispatcher.Cancel]. SID не указывается — dispatcher
// читает строку errand-а по ID и берёт SID оттуда.
type CancelRequest struct {
	ErrandID    string
	RequestedBy string // archon AID инициатора (для audit).
}

// Cancel — slice E5 ADR-033: отменить in-flight Errand. Семантика — best-effort
// signal: Keeper отправляет CancelErrand в EventStream-канал Soul-а
// (local/remote по holder-у lease-а), Soul-side errandrunner отменяет ctx
// активной Run-горутины → она возвращает ErrandResult{status: CANCELLED}
// через тот же EventStream, и applybus-receiver (events_errand.go) уже сам
// переводит строку errands в status='cancelled' через MarkTerminal.
//
// Cancel НЕ блокируется и НЕ ждёт ErrandResult: HTTP-handler отдаёт 204
// сразу. Финальный статус оператор увидит через GET /v1/errands/{id} (poll).
// Если Soul не отвечает за полный TimeoutSec (300s max) — purge_old_errands
// или sweep-on-restart переведут строку в timed_out по обычному пути.
//
// Шаги:
//  1. Lookup row по errand_id → 404 если нет.
//  2. Check status='running' → 409 (ErrErrandTerminal) если уже терминал.
//  3. Resolve holder lease, send CancelErrand local/remote.
//  4. Write audit `errand.cancelled` (event_type зафиксирован, см. event_types.go).
//
// Audit пишется СРАЗУ при успешной отправке CancelErrand — даже если Soul
// проигнорирует (race с собственным завершением). Это намеренно: audit
// фиксирует «оператор инициировал cancel», не «Soul действительно отменил».
// Финальный исход видно по GET (status=cancelled/success/failed/timed_out).
func (d *Dispatcher) Cancel(ctx context.Context, req CancelRequest) error {
	if req.ErrandID == "" {
		return ErrEmptyErrandID
	}

	row, err := d.deps.Store.Get(ctx, req.ErrandID)
	if err != nil {
		return fmt.Errorf("errand: cancel get: %w", err)
	}
	if row.Status != StatusRunning {
		// Terminal Errand — отменять нечего. Идемпотентно: дубль cancel того же
		// errand_id после успешной отмены тоже придёт сюда (status=cancelled),
		// 409 — корректный ответ (а не «ok, уже cancelled»).
		return fmt.Errorf("%w: status=%s", ErrErrandTerminal, row.Status)
	}

	if err := d.sendCancel(ctx, row.SID, row.ErrandID); err != nil {
		return err
	}

	d.writeCancelInitiated(ctx, row.ErrandID, row.SID, row.Module, req.RequestedBy)
	return nil
}

// sendCancel выбирает путь доставки CancelErrand: local (Outbound.SendCancelErrand)
// либо remote (Publisher.PublishCancelErrand) по holder-у lease-а.
// Алгоритм идентичен [Dispatcher.send] (для ErrandRequest); вынесен отдельной
// функцией, чтобы Cancel не вызывал buildProtoRequest (другой proto-тип).
func (d *Dispatcher) sendCancel(ctx context.Context, sid, errandID string) error {
	if d.deps.LeaseLookup == nil || d.deps.Publisher == nil {
		if err := d.deps.Outbound.SendCancelErrand(ctx, sid, errandID); err != nil {
			d.deps.Logger.Warn("errand: local-only cancel send failed",
				slog.String("sid", sid),
				slog.String("errand_id", errandID),
				slog.Any("error", err))
			return ErrSoulNotConnected
		}
		return nil
	}

	holder, err := d.deps.LeaseLookup.ReadHolder(ctx, sid)
	if err != nil {
		d.deps.Logger.Warn("errand: cancel lease lookup failed, fallback to local",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
			slog.Any("error", err))
		if sendErr := d.deps.Outbound.SendCancelErrand(ctx, sid, errandID); sendErr != nil {
			return ErrSoulNotConnected
		}
		return nil
	}
	if holder == "" {
		return ErrSoulNotConnected
	}
	if holder == d.deps.KID {
		if err := d.deps.Outbound.SendCancelErrand(ctx, sid, errandID); err != nil {
			d.deps.Logger.Warn("errand: cancel local send (holder=self) failed",
				slog.String("sid", sid),
				slog.String("errand_id", errandID),
				slog.Any("error", err))
			return ErrSoulNotConnected
		}
		return nil
	}
	if err := d.deps.Publisher.PublishCancelErrand(ctx, sid, errandID); err != nil {
		d.deps.Logger.Warn("errand: cancel remote publish failed",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
			slog.String("holder", holder),
			slog.Any("error", err))
		return ErrSoulNotConnected
	}
	return nil
}

// writeCancelInitiated пишет audit-event `errand.cancelled` от инициатора-Архонта
// (source=api). Терминальный `errand.cancelled` от applybus-receiver-а (Soul
// прислал ErrandResult{CANCELLED}) тоже пишется в writeTerminal — для UI это
// будут два разных события: «оператор отменил» + «Soul подтвердил отмену».
// Compromise vs дубля: разные source (api vs soul_grpc), correlation_id
// одинаковый — UI группирует по нему.
//
// Если audit-writer nil (тестовая сборка) — drop.
func (d *Dispatcher) writeCancelInitiated(ctx context.Context, errandID, sid, module, aid string) {
	if d.deps.Audit == nil {
		return
	}
	ev := &audit.Event{
		EventType:     audit.EventTypeErrandCancelled,
		Source:        audit.SourceAPI,
		ArchonAID:     aid,
		CorrelationID: errandID,
		Payload: map[string]any{
			"sid":       sid,
			"module":    module,
			"errand_id": errandID,
		},
	}
	if err := d.deps.Audit.Write(ctx, ev); err != nil {
		d.deps.Logger.Warn("errand: audit cancelled (initiated) failed",
			slog.String("errand_id", errandID),
			slog.Any("error", err))
	}
}
