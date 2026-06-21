// Package voyageorch — pool VoyageWorker-ов, исполняющих Voyage-прогоны
// (ADR-043, S1). Worker крутит claim-loop: атомарно подбирает один pending
// Voyage через [voyage.ClaimNext], запускает renewal-goroutine через
// [voyage.RenewLease], исполняет работу прогона и финализирует через
// [voyage.Finalize] под CAS-guard-ом ownership.
//
// Pattern copy из [tideorch]/[errandrunorch] — `claim → renew → execute →
// finalize-with-ownership`. execute ветвится по kind:
//   - kind=scenario (S2) — реальный батчевый прогон scenario поверх N
//     инкарнаций (см. scenario.go, executeScenarioVoyage); подход B1 ADR-043:
//     batch (Leg) = N инкарнаций, каждая = полноценный per-incarnation
//     scenario-run со своим state-commit-ом (делает сам scenario-runner);
//   - kind=command (S3) — пока NOOP-заглушка (фундамент S1): finalize succeeded.
//
// config-gated OFF по умолчанию (см. daemon.setupVoyageWorker); production
// wire-up Spawner/Awaiter для kind=scenario — S5.
//
// Failover-resilience: при смерти инстанса протухший claim возвращается
// Reaper-правилом обратно в `pending` (тираж — пост-S1); другой Keeper подбирает
// Voyage через ClaimNext.
//
// TODO(post-S1): claim+lease helpers дублируют tide/errandrun (architect-decision
// γ 2026-05-27 об extract в shared `claimlease/` отложен). Реальное исполнение —
// S2 (scenario fan-out per-incarnation) / S3 (command fan-out per-host).
package voyageorch

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// VoyageWorker — claim+execute loop для Voyage-прогонов. Один Worker — одна
// goroutine; в daemon (setupVoyageWorker) поднимается несколько (cfg.Voyage.Workers).
//
// Lifecycle:
//   - Run(ctx) крутит loop до отмены ctx.
//   - graceful-shutdown: cancel ctx → текущий executeVoyage добегает до конца
//     (на S1 — мгновенно: NOOP-finalize); renewLoop останавливается на ctx.Done.
type VoyageWorker struct {
	KID           string
	Pool          voyage.ExecQueryRower
	LeaseTTL      time.Duration
	RenewInterval time.Duration
	PollInterval  time.Duration
	Logger        *slog.Logger

	// ScenarioSpawner / ScenarioAwaiter — DI kind=scenario исполнения (S2):
	// спавн per-incarnation scenario-run-а + ожидание его терминала (parity
	// tideorch.SurgeSpawner/TerminalAwaiter). nil → claim-нутый scenario-Voyage
	// финализируется failed (fail-closed; production wire-up — S5). kind=command
	// (S3) их не использует.
	ScenarioSpawner ScenarioSpawner
	ScenarioAwaiter IncarnationAwaiter

	// OrphanReleaser — recovery-шов ADR-027(k): ПЕРЕД повторным спавном per-
	// incarnation scenario-run реклеймнутого Voyage снимает осиротевший
	// applying-lock инкарнации, оставшийся от scenario-run мёртвого прошлого
	// владельца (FENCED single-winner). nil → детект выключен (поведение как до
	// фикса; unit-сборка без recovery-шва). Только kind=scenario (S2).
	OrphanReleaser OrphanLockReleaser

	// CommandSpawner — DI kind=command исполнения (S3): блокирующий спавн
	// Errand-а на один SID (reuse errand-машинерии, parity
	// errandrunorch.ErrandSpawner). nil → claim-нутый command-Voyage
	// финализируется failed (fail-closed; production wire-up — S5). kind=scenario
	// (S2) его не использует.
	CommandSpawner CommandSpawner

	// Audit — writer finalize-audit-семейства (ADR-043, A3): per-Leg
	// (scenario_run.leg_*) + терминал (scenario_run/command_run.{completed|
	// partial_failed|failed}) + lease_lost (scenario_run.lease_lost) +
	// voyage.reclaimed пишет Reaper отдельно. nil-safe: dev-без-audit живёт,
	// эмит только при Audit != nil; validate() поле НЕ требует.
	Audit audit.Writer
}

// validate проверяет обязательные поля. Вызывается на старте Run; ошибка —
// программная (caller setupVoyageWorker должен был передать все deps).
func (w *VoyageWorker) validate() error {
	if w.KID == "" {
		return errors.New("voyageorch: KID is required")
	}
	if w.Pool == nil {
		return errors.New("voyageorch: Pool is required")
	}
	if w.LeaseTTL <= 0 {
		return errors.New("voyageorch: LeaseTTL must be > 0")
	}
	if w.RenewInterval <= 0 {
		return errors.New("voyageorch: RenewInterval must be > 0")
	}
	if w.PollInterval <= 0 {
		return errors.New("voyageorch: PollInterval must be > 0")
	}
	if w.Logger == nil {
		return errors.New("voyageorch: Logger is required")
	}
	return nil
}

// Run крутит claim-loop до отмены ctx. Возвращает на ctx.Done без ошибки —
// штатный graceful-shutdown. На invalid-config (validate fail) — error для
// caller-а (программная ошибка setup-а).
func (w *VoyageWorker) Run(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}

	w.Logger.Info("voyageorch: worker started",
		slog.String("kid", w.KID),
		slog.Duration("lease_ttl", w.LeaseTTL),
		slog.Duration("renew_interval", w.RenewInterval),
		slog.Duration("poll_interval", w.PollInterval),
	)

	for {
		select {
		case <-ctx.Done():
			w.Logger.Info("voyageorch: worker stopped",
				slog.String("kid", w.KID),
				slog.Any("reason", ctx.Err()),
			)
			return nil
		default:
		}

		run, err := voyage.ClaimNext(ctx, w.Pool, w.KID, w.LeaseTTL)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			w.Logger.Error("voyageorch: ClaimNext failed",
				slog.String("kid", w.KID),
				slog.Any("error", err),
			)
			if !w.sleep(ctx, w.PollInterval) {
				return nil
			}
			continue
		}
		if run == nil {
			if !w.sleep(ctx, w.PollInterval) {
				return nil
			}
			continue
		}

		w.executeVoyage(ctx, run)
	}
}

// sleep ждёт duration или ctx.Done. Возвращает false, если вышли по ctx.
func (w *VoyageWorker) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// renewLoop CAS-продлевает lease каждые RenewInterval. На [voyage.ErrLeaseLost]
// закрывает leaseLost-канал — executeVoyage увидит, что lease ушёл, и не будет
// финализировать Voyage (его подбирает другой Keeper). Parity errandrunorch.
func (w *VoyageWorker) renewLoop(ctx context.Context, runID string, leaseLost chan<- struct{}) {
	ticker := time.NewTicker(w.RenewInterval)
	defer ticker.Stop()

	var closed bool
	closeOnce := func() {
		if !closed {
			close(leaseLost)
			closed = true
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := voyage.RenewLease(ctx, w.Pool, runID, w.KID, w.LeaseTTL)
			if err == nil {
				continue
			}
			if errors.Is(err, voyage.ErrLeaseLost) {
				w.Logger.Warn("voyageorch: lease lost — другой Keeper подобрал Voyage",
					slog.String("voyage_id", runID),
					slog.String("kid", w.KID),
				)
				closeOnce()
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			w.Logger.Warn("voyageorch: RenewLease failed (will retry on next tick)",
				slog.String("voyage_id", runID),
				slog.String("kid", w.KID),
				slog.Any("error", err),
			)
		}
	}
}

// executeVoyage — исполнитель одного Voyage-захвата. Ветвится по kind:
// scenario (S2, реальный батчевый прогон) / command (S3, пока NOOP-stub).
// Финализирует Voyage под ownership-guard-ом; при потере lease посреди прогона
// (executeScenarioVoyage вернул "") — НЕ финализирует (другой Keeper подберёт).
//
// renewCtx производный от ctx — на ctx.Done renewal остановится. Defer-обёртка
// гарантирует ПОРЯДОК: сначала cancelRenew() (renewLoop увидит ctx.Done и
// выйдет), затем renewWG.Wait() (дождёмся выхода renewLoop). Голый
// `defer renewWG.Wait()` без cancel дал бы deadlock (parity errandrunorch).
func (w *VoyageWorker) executeVoyage(ctx context.Context, run *voyage.Voyage) {
	renewCtx, cancelRenew := context.WithCancel(ctx)
	var renewWG sync.WaitGroup
	leaseLost := make(chan struct{})
	renewWG.Add(1)
	go func() {
		defer renewWG.Done()
		w.renewLoop(renewCtx, run.VoyageID, leaseLost)
	}()
	defer func() {
		cancelRenew()
		renewWG.Wait()
	}()

	w.Logger.Info("voyageorch: voyage claimed",
		slog.String("kid", w.KID),
		slog.String("voyage_id", run.VoyageID),
		slog.String("kind", string(run.Kind)),
		slog.Int("total_batches", run.TotalBatches),
		slog.Int("attempt", run.Attempt),
	)

	// Ветвление по kind:
	//   - scenario (S2): реальный батчевый прогон scenario поверх N инкарнаций.
	//   - command (S3): реальный батчевый прогон whitelisted-модуля поверх N
	//     хостов (поглощает ErrandRun, ADR-043).
	//
	// Оба executeXxxVoyage возвращают ("", nil) при потере lease / ctx.Done
	// посреди прогона — тогда finalize НЕ делаем (Reaper-reclaim вернёт в
	// pending, подберёт другой Keeper).
	var (
		finalStatus voyage.Status
		summary     *voyage.Summary
		// errCode — машинный код причины fail-closed-провала (до старта
		// единиц): spawner_not_configured / empty_scenario_name / empty_module /
		// target_resolve_failed. Пуст для happy-path и для «все единицы failed»
		// (там провал — фактический исход прогона, а не setup-ошибка). Кладётся
		// в payload терминального failed-события (см. emitFinalized).
		errCode string
	)
	switch run.Kind {
	case voyage.KindScenario:
		finalStatus, summary, errCode = w.executeScenarioVoyage(ctx, run, leaseLost)
	case voyage.KindCommand:
		finalStatus, summary, errCode = w.executeCommandVoyage(ctx, run, leaseLost)
	default:
		w.Logger.Error("voyageorch: unknown voyage kind",
			slog.String("voyage_id", run.VoyageID), slog.String("kind", string(run.Kind)))
		finalStatus = voyage.StatusFailed
		summary = &voyage.Summary{Total: run.TotalBatches}
		errCode = "unknown_kind"
	}
	if finalStatus == "" {
		// lease lost / прерывание посреди прогона — не финализируем.
		return
	}

	// Перед финализацией проверяем, не ушёл ли lease (renewLoop успел закрыть
	// канал) — тогда другой Keeper уже владеет Voyage, финализировать нельзя.
	select {
	case <-leaseLost:
		w.Logger.Warn("voyageorch: lease lost до финализации — пропускаем finalize",
			slog.String("voyage_id", run.VoyageID),
			slog.String("kid", w.KID),
		)
		w.emitLeaseLost(run, "finalize")
		return
	default:
	}

	err := voyage.Finalize(ctx, w.Pool, run.VoyageID, w.KID, finalStatus, summary)
	if err != nil {
		if errors.Is(err, voyage.ErrLeaseLost) {
			w.Logger.Warn("voyageorch: finalize — lease lost, финализирует новый владелец",
				slog.String("voyage_id", run.VoyageID),
				slog.String("kid", w.KID),
			)
			w.emitLeaseLost(run, "finalize")
			return
		}
		w.Logger.Error("voyageorch: finalize failed",
			slog.String("voyage_id", run.VoyageID),
			slog.String("kid", w.KID),
			slog.Any("error", err),
		)
		return
	}

	w.Logger.Info("voyageorch: voyage finalized",
		slog.String("voyage_id", run.VoyageID),
		slog.String("kid", w.KID),
		slog.String("status", string(finalStatus)),
	)

	w.emitFinalized(run, finalStatus, summary, errCode)
}

// emitFinalized пишет терминальное finalize-событие прогона по kind+status
// (ADR-043, A3). source=keeper_internal, archon_aid="" (NULL), correlation_id=
// voyage_id. nil-safe: эмит только при Audit != nil. scenario → scenario_run.*,
// command → command_run.*; payload-форма различается (scenario несёт
// total_batches+summary, command — total+succeeded из Summary, parity
// errand_run.*). errCode кладётся только в failed-событие fail-closed-путей.
func (w *VoyageWorker) emitFinalized(run *voyage.Voyage, status voyage.Status, summary *voyage.Summary, errCode string) {
	if w.Audit == nil {
		return
	}
	if summary == nil {
		summary = &voyage.Summary{}
	}

	var eventType audit.EventType
	payload := map[string]any{
		"voyage_id": run.VoyageID,
		"kind":      string(run.Kind),
	}
	// cadence_id на Voyage-терминале (ADR-052 §l amend): Voyage, спавненный
	// расписанием (run.CadenceID != nil, claim селектит cadence_id —
	// voyage.ClaimNext), несёт cadence_id в payload терминала прогона, чтобы
	// cadence-селектор Tiding поймал ОДНО агрегированное уведомление на спавн.
	// nil-guarded симметрично scenario/run.go emitRunCompleted: ручной Voyage
	// (CadenceID nil) поля не несёт → cadence-селектор не матчит.
	if run.CadenceID != nil {
		payload["cadence_id"] = *run.CadenceID
	}

	if run.Kind == voyage.KindCommand {
		payload["total"] = summary.Total
		payload["succeeded"] = summary.Succeeded
		switch status {
		case voyage.StatusSucceeded:
			eventType = audit.EventCommandRunCompleted
		case voyage.StatusPartialFailed:
			eventType = audit.EventCommandRunPartialFailed
			payload["failed"] = summary.Failed
			payload["cancelled"] = summary.Cancelled
			payload["on_failure"] = derefOnFailure(run.OnFailure)
		default: // StatusFailed
			eventType = audit.EventCommandRunFailed
			if errCode != "" {
				payload["error_code"] = errCode
			}
		}
	} else {
		payload["total_batches"] = run.TotalBatches
		payload["summary"] = summaryPayload(summary)
		switch status {
		case voyage.StatusSucceeded:
			eventType = audit.EventScenarioRunCompleted
		case voyage.StatusPartialFailed:
			eventType = audit.EventScenarioRunPartialFailed
			payload["on_failure"] = derefOnFailure(run.OnFailure)
		default: // StatusFailed
			eventType = audit.EventScenarioRunFailed
			if errCode != "" {
				payload["error_code"] = errCode
			}
		}
	}

	w.writeAudit(eventType, run.VoyageID, payload)
}

// emitLeaseLost пишет scenario_run.lease_lost (ADR-043, A3): VoyageWorker
// потерял lease посреди прогона / перед finalize. Только для kind=scenario
// (parity tide.lease_lost); command-семейство lease_lost НЕ имеет (parity
// errand_run.*) — прогон молча подберёт другой Keeper. phase ∈ leg/finalize.
func (w *VoyageWorker) emitLeaseLost(run *voyage.Voyage, phase string) {
	if w.Audit == nil || run.Kind != voyage.KindScenario {
		return
	}
	w.writeAudit(audit.EventScenarioRunLeaseLost, run.VoyageID, map[string]any{
		"voyage_id":    run.VoyageID,
		"kind":         string(run.Kind),
		"kid_who_lost": w.KID,
		"phase":        phase,
	})
}

// emitLegStarted пишет scenario_run.leg_started ПЕРЕД fan-out-ом Leg-а
// kind=scenario (ADR-043, A3, parity tide.surge_started). command-семейство
// leg-событий НЕ имеет (плоский fan-out).
func (w *VoyageWorker) emitLegStarted(run *voyage.Voyage, legIndex, incarnationsInLeg int) {
	if w.Audit == nil {
		return
	}
	w.writeAudit(audit.EventScenarioRunLegStarted, run.VoyageID, map[string]any{
		"voyage_id":           run.VoyageID,
		"kind":                string(run.Kind),
		"leg_index":           legIndex,
		"incarnations_in_leg": incarnationsInLeg,
	})
}

// emitLegCompleted пишет scenario_run.leg_completed после терминала всех
// инкарнаций Leg-а + агрегации Summary-дельты (ADR-043, A3, parity
// tide.surge_completed). terminal — терминальный статус Leg-а из исходов.
func (w *VoyageWorker) emitLegCompleted(run *voyage.Voyage, legIndex int, leg legOutcome) {
	if w.Audit == nil {
		return
	}
	w.writeAudit(audit.EventScenarioRunLegCompleted, run.VoyageID, map[string]any{
		"voyage_id": run.VoyageID,
		"kind":      string(run.Kind),
		"leg_index": legIndex,
		"terminal":  leg.terminal(),
		"total":     leg.total,
		"succeeded": leg.succeeded,
		"failed":    leg.failed,
		"cancelled": leg.cancelled,
	})
}

// advanceBatchProgress продвигает current_batch_index Voyage-а до числа
// завершённых Leg-ов (completedBatches) — UI-индикатор «Batch N/total».
// Barrier-only: window-путь (runSlidingWindow/runScenarioSlidingWindow) этот
// метод НЕ зовёт (там батчей нет, total_batches=1, прогресс по targets).
//
// Best-effort (voyage.UpdateBatchProgress, ownership-guarded): ошибка/0-rows
// (lease потеряна, Reaper-reclaim) логируется warn и НЕ валит прогон — источник
// правды о ходе прогона — voyage_targets, прогресс лишь подсказка для UI.
func (w *VoyageWorker) advanceBatchProgress(ctx context.Context, run *voyage.Voyage, completedBatches int) {
	if err := voyage.UpdateBatchProgress(ctx, w.Pool, run.VoyageID, w.KID, run.Attempt, completedBatches); err != nil {
		w.Logger.Warn("voyageorch: не удалось обновить current_batch_index (best-effort)",
			slog.String("voyage_id", run.VoyageID), slog.Int("completed_batches", completedBatches),
			slog.Any("error", err))
	}
}

// writeAudit — общий best-effort эмит keeper_internal-события прогона.
// Background-ctx: эмит вне apply-ctx, чтобы запись прошла даже при отмене
// исходного ctx (graceful-shutdown). Ошибка PG логируется warn — finalize уже
// зафиксирован в БД, audit-trail вторичен.
func (w *VoyageWorker) writeAudit(eventType audit.EventType, voyageID string, payload map[string]any) {
	ev := &audit.Event{
		EventType:     eventType,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: voyageID,
		Payload:       payload,
	}
	if err := w.Audit.Write(context.Background(), ev); err != nil {
		w.Logger.Warn("voyageorch: finalize audit write failed",
			slog.String("voyage_id", voyageID),
			slog.String("event_type", string(eventType)),
			slog.Any("error", err))
	}
}

// summaryPayload проецирует [voyage.Summary] в audit-payload-форму (no_match
// omitempty parity voyageSummaryDTO).
func summaryPayload(s *voyage.Summary) map[string]any {
	out := map[string]any{
		"total":     s.Total,
		"succeeded": s.Succeeded,
		"failed":    s.Failed,
		"cancelled": s.Cancelled,
	}
	if s.NoMatch > 0 {
		out["no_match"] = s.NoMatch
	}
	return out
}

// derefOnFailure возвращает строковую форму on_failure (пусто при nil).
func derefOnFailure(p *voyage.OnFailure) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// legOutcome — агрегаты исходов одного Leg-а для scenario_run.leg_completed.
type legOutcome struct {
	total     int
	succeeded int
	failed    int
	cancelled int
}

// terminal классифицирует терминал Leg-а (parity SurgeRecord.Terminal):
// failed (был провал, успеха нет) / partial (провал + успех) / cancelled
// (только cancelled, без провала и успеха) / success.
func (l legOutcome) terminal() string {
	if l.failed > 0 {
		if l.succeeded > 0 {
			return "partial"
		}
		return "failed"
	}
	if l.cancelled > 0 && l.succeeded == 0 {
		return "cancelled"
	}
	return "success"
}
