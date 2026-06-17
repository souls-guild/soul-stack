package voyageorch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// kind=command исполнение (ADR-043 S3). Поглощает паттерн ErrandRun: батчевый
// прогон whitelisted-модуля (ADR-033 whitelist + ErrandReadSafe) по набору
// ХОСТОВ. `incarnation.state` НЕ трогается — нет state-commit-а (ADR-033/043).
//
// Batch (Leg) = N ХОСТОВ. В отличие от kind=scenario (Leg = N инкарнаций,
// per-incarnation scenario-run со своим state-commit-ом) здесь Leg — плоский
// fan-out одиночного Errand-а на каждый SID, без barrier/state. Алгоритм
// симметричен executeScenarioVoyage:
//   1. target_resolved (kind=command) → []sid (JSONB-массив SID-ов).
//   2. chunkSIDs(sids, batch_size) → Leg-и (NULL/0 → один Leg = все).
//   3. Per Leg: параллельный fan-out по хостам под semaphore-cap (concurrency).
//      На каждый SID: SpawnCommand (= блокирующий Errand-dispatch до терминала,
//      reuse errand-машинерии) → MarkTargetRunning(errand_id) → MarkTargetTerminal.
//   4. on_failure=abort → первый провалившийся Leg останавливает переход к
//      следующему; continue → идём до конца.
//   5. Между Leg-ами — пауза inter_batch_interval (контролируемая выкатка).
//   6. После всех Leg-ов — Finalize voyage (succeeded / partial_failed / failed).
//
// Переиспользование errand-машинерии: errandrunorch обернул блокирующий
// errand.Dispatcher в единый DI-вызов ErrandSpawner.SpawnErrand (spawn+await
// схлопнуты, т.к. Dispatch синхронен до терминала). Здесь повторяем ровно этот
// boundary под именем [CommandSpawner.SpawnCommand] (production wire-up S5
// подставит адаптер поверх того же Dispatcher-а). S2-форма «Spawner+Awaiter»
// для command вырождается в один интерфейс — у Errand-а нет async-spawn-фазы,
// которую нужно отдельно ждать (в отличие от scenario-runner-а).

// CommandSpawner — спавн одного Errand-а на один SID, блокирующий до терминала.
// Изолирует voyageorch от errand.Dispatcher / Outbound / ApplyBus (parity
// errandrunorch.ErrandSpawner): production wire-up (S5) подставит адаптер поверх
// existing Dispatcher.Dispatch, unit-тесты — fake без зависимостей.
//
// Контракт (совпадает с errandrunorch.ErrandSpawner.SpawnErrand минус Cancel —
// voyage-level cancel-all отложен до S5):
//
//  1. SpawnCommand блокируется до достижения terminal-статуса Errand-а либо до
//     отмены ctx (caller передаёт fanCtx, который cancel-ится при leaseLost /
//     on_failure=abort).
//  2. Возвращаемый status — строковая проекция errand.Status: success / failed /
//     timed_out / cancelled / module_not_allowed. Whitelist (ADR-033) проверяет
//     Soul-side — module_not_allowed прилетает как обычный терминал, voyageorch
//     его не дублирует.
//  3. errandID — back-link на errands-строку (для voyage_targets.errand_id и
//     drill-а S5). Пустая строка возможна, если Spawner упал до Insert-а — тогда
//     caller зачитывает target как failed без back-link-а.
//  4. err != nil — внутренняя ошибка orchestrator-вызова (не failed-Errand);
//     caller считает Errand failed.
//
// module — voyages.module (whitelisted, NOT NULL для kind=command). input —
// voyages.input (jsonb, прокидывается в errands.input без изменений).
// startedByAID — AID Архонта-инициатора Voyage-а (FK errands.started_by_aid).
type CommandSpawner interface {
	SpawnCommand(ctx context.Context, voyageID, sid, module, startedByAID string, input []byte) (errandID, status string, err error)
}

// commandResult — runtime-исход одного хоста в Leg-е, собирается в per-SID
// goroutine и складывается под mutex. ErrandID пуст, если spawn упал до создания
// errand-строки (тогда Status=failed без back-link-а).
type commandResult struct {
	SID      string
	ErrandID string
	Outcome  TargetOutcome
}

// summarize агрегирует исходы хостов в [voyage.Summary] (Total = полный scope
// прогона, передаётся отдельно — results может быть короче при abort). Возвращает
// также anyFailure для выбора финального статуса. Единый агрегатор для обоих
// control-flow-каркасов command-исполнителя (barrier-цикл executeCommandVoyage и
// окно runSlidingWindow) — сами каркасы разные и НЕ сливаются, общая только эта
// чистая редукция.
func summarize(results []commandResult, total int) (*voyage.Summary, bool) {
	summary := &voyage.Summary{Total: total}
	var anyFailure bool
	for _, res := range results {
		switch res.Outcome {
		case OutcomeSucceeded, OutcomeNoMatch:
			summary.Succeeded++
		case OutcomeCancelled:
			summary.Cancelled++
		default:
			summary.Failed++
		}
		if res.Outcome == OutcomeNoMatch {
			summary.NoMatch++
		}
		if res.Outcome.isFailure() {
			anyFailure = true
		}
	}
	return summary, anyFailure
}

// commandStatusToOutcome переводит строковый errand-статус в [TargetOutcome].
// success → succeeded; cancelled → cancelled; всё прочее (failed / timed_out /
// module_not_allowed / неизвестное) → failed (fail-closed, parity
// errandrunorch.isFailureStatus + buildSummary).
func commandStatusToOutcome(status string) TargetOutcome {
	switch status {
	case "success":
		return OutcomeSucceeded
	case "cancelled":
		return OutcomeCancelled
	default:
		return OutcomeFailed
	}
}

// parseSIDTargets разбирает target_resolved (kind=command): JSONB-массив SID-ов
// (snapshot набора хостов от старта прогона, ADR-043). Пустой массив / невалидный
// JSON / пустой SID / дубликат — ошибка (parity parseIncarnationTargets: Insert
// требует непустой валидный target_resolved, дубль ломает UNIQUE PK
// voyage_targets).
func parseSIDTargets(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("voyageorch: empty target_resolved")
	}
	var sids []string
	if err := json.Unmarshal(raw, &sids); err != nil {
		return nil, fmt.Errorf("voyageorch: target_resolved (kind=command) не JSON-массив SID-ов: %w", err)
	}
	if len(sids) == 0 {
		return nil, fmt.Errorf("voyageorch: target_resolved пуст (нет хостов)")
	}
	seen := make(map[string]struct{}, len(sids))
	for i, s := range sids {
		if s == "" {
			return nil, fmt.Errorf("voyageorch: target_resolved[%d] пустой SID", i)
		}
		if _, dup := seen[s]; dup {
			return nil, fmt.Errorf("voyageorch: target_resolved содержит дубликат SID %q", s)
		}
		seen[s] = struct{}{}
	}
	return sids, nil
}

// chunkSIDs режет плоский список хостов на последовательные Leg-и размером не
// более batchSize. batchSize <= 0 → один Leg на всё (NULL/0 batch_size = «весь
// прогон одним Leg», ADR-043). Семантика идентична chunkIncarnations (kind=scenario):
// command-вариант держим отдельно во избежание правки S2-кода.
func chunkSIDs(sids []string, batchSize int) [][]string {
	if len(sids) == 0 {
		return nil
	}
	if batchSize <= 0 {
		return [][]string{append([]string(nil), sids...)}
	}
	legs := make([][]string, 0, (len(sids)+batchSize-1)/batchSize)
	for off := 0; off < len(sids); off += batchSize {
		end := off + batchSize
		if end > len(sids) {
			end = len(sids)
		}
		legs = append(legs, append([]string(nil), sids[off:end]...))
	}
	return legs
}

// executeCommandVoyage — S3-исполнитель kind=command: батчевый прогон
// whitelisted-модуля поверх N хостов. Вызывается из [VoyageWorker.executeVoyage]
// после claim-а; renewal-goroutine уже держит lease (leaseLost закрывается при
// потере).
//
// Возвращает финальный [voyage.Status] + [*voyage.Summary] + error_code для
// Finalize/finalize-audit. error_code непуст только для fail-closed-путей до
// старта хостов (spawner_not_configured / empty_module / target_resolve_failed);
// для happy-path и «все хосты failed» — пуст. На потерю lease / ctx.Done посреди
// прогона возвращает ("", nil, "") — caller НЕ финализирует (Reaper-reclaim
// вернёт Voyage в pending, другой Keeper подберёт). Симметрично
// executeScenarioVoyage; command leg-событий и lease_lost НЕ эмитит (parity
// errand_run.*).
func (w *VoyageWorker) executeCommandVoyage(ctx context.Context, run *voyage.Voyage, leaseLost <-chan struct{}) (voyage.Status, *voyage.Summary, string) {
	if w.CommandSpawner == nil {
		// Production wire-up (S5) обязан передать Spawner; его отсутствие при
		// claim-нутом command-Voyage — программная ошибка setup-а. Fail-closed.
		w.Logger.Error("voyageorch: command execution requested but CommandSpawner not configured",
			slog.String("voyage_id", run.VoyageID),
		)
		return voyage.StatusFailed, &voyage.Summary{Total: run.TotalBatches}, "spawner_not_configured"
	}
	if run.Module == nil || *run.Module == "" {
		w.Logger.Error("voyageorch: kind=command без module", slog.String("voyage_id", run.VoyageID))
		return voyage.StatusFailed, &voyage.Summary{}, "empty_module"
	}

	sids, err := parseSIDTargets(run.TargetResolved)
	if err != nil {
		w.Logger.Error("voyageorch: parse target_resolved failed",
			slog.String("voyage_id", run.VoyageID), slog.Any("error", err))
		return voyage.StatusFailed, &voyage.Summary{}, "target_resolve_failed"
	}

	concurrency := 1
	if run.Concurrency != nil && *run.Concurrency > 0 {
		concurrency = *run.Concurrency
	}

	// failThreshold — обобщённый abort-gate (ADR-043 amendment §3): порог
	// абсолютного числа провалов, при котором прогон останавливается. abort ≡ 1
	// (backcompat: первый провал → стоп); continue/nil без fail_threshold ≡ 0
	// (без порога — до конца); явный fail_threshold N → N. 0 = «без порога».
	failThreshold := voyage.ResolveFailThreshold(run.FailThreshold, run.OnFailure)

	// batch_mode=window → скользящее окно по хостам (ADR-043 amendment §1):
	// один общий пул concurrency воркеров из единой очереди SID-ов, без Leg-ов
	// и барьеров. barrier (NULL) → существующий chunk+runCommandLeg путь ниже.
	if voyage.ResolveBatchMode(run.BatchMode) == voyage.BatchModeWindow {
		return w.runSlidingWindow(ctx, run, sids, concurrency, failThreshold, leaseLost)
	}

	batchSize := 0
	if run.BatchSize != nil {
		batchSize = *run.BatchSize
	}
	legs := chunkSIDs(sids, batchSize)

	var (
		summary    = &voyage.Summary{Total: len(sids)}
		anyFailure bool
	)

	for legIdx, leg := range legs {
		// Раннее обнаружение потери lease / отмены — до спавна нового Leg-а.
		select {
		case <-leaseLost:
			w.Logger.Warn("voyageorch: lease lost между Leg-ами — другой Keeper подберёт Voyage",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
			return "", nil, ""
		case <-ctx.Done():
			w.Logger.Info("voyageorch: command-loop прерван ctx.Done",
				slog.String("voyage_id", run.VoyageID), slog.Any("reason", ctx.Err()))
			return "", nil, ""
		default:
		}

		// Пауза перед каждым Leg-ом, кроме первого (inter_batch_interval).
		if legIdx > 0 && run.InterBatchInterval != nil && *run.InterBatchInterval > 0 {
			if !w.interBatchPause(ctx, *run.InterBatchInterval, leaseLost) {
				w.Logger.Warn("voyageorch: прерван на inter_batch_interval — не финализируем",
					slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
				return "", nil, ""
			}
		}

		results, fenceLost := w.runCommandLeg(ctx, run, legIdx, leg, concurrency, leaseLost)

		// Потеря lease ПОСРЕДИ Leg-а: runCommandLeg остановил spawn-loop и прервал
		// in-flight через отменённый fanCtx, оставшиеся хосты помечены cancelled.
		// НЕ финализируем (Reaper-reclaim). Два детектора: renewLoop-канал leaseLost
		// (по тику) И fenceLost — fencing-CAS перед dispatch-ем поймал потерю раньше
		// renew-тика (S-med-2). Реклеймнувший Keeper владеет Voyage — финализировать
		// нельзя.
		select {
		case <-leaseLost:
			w.Logger.Warn("voyageorch: lease lost посреди Leg-а — другой Keeper подберёт Voyage",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
			return "", nil, ""
		default:
		}
		if fenceLost {
			w.Logger.Warn("voyageorch: ownership-fence потерян посреди Leg-а — другой Keeper подберёт Voyage",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
			return "", nil, ""
		}

		legSummary, legFailure := summarize(results, 0)
		summary.Succeeded += legSummary.Succeeded
		summary.Failed += legSummary.Failed
		summary.Cancelled += legSummary.Cancelled
		summary.NoMatch += legSummary.NoMatch
		anyFailure = anyFailure || legFailure

		// Прогресс батчей для UI: Leg legIdx ЗАВЕРШЁН → current_batch_index =
		// legIdx+1 (best-effort, ownership-guarded; симметрия со scenario-путём).
		w.advanceBatchProgress(ctx, run, legIdx+1)

		// Обобщённый abort-gate (ADR-043 amendment §3): достигнут порог числа
		// провалов → прекращаем переход к следующему Leg. threshold=0 → без
		// порога (continue). summary.Failed — кумулятив провалов по всем Leg-ам.
		if failThreshold > 0 && summary.Failed >= failThreshold {
			w.Logger.Info("voyageorch: достигнут fail_threshold — оставшиеся Leg-и пропущены",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx),
				slog.Int("failed", summary.Failed), slog.Int("fail_threshold", failThreshold))
			break
		}
	}

	return scenarioFinalStatus(summary, anyFailure), summary, ""
}

// runSlidingWindow — S-W1 исполнитель batch_mode=window для kind=command (ADR-043
// amendment §1). Полное скользящее окно по хостам: пул `concurrency` воркеров
// тянет SID-ы из ОДНОЙ общей очереди прогона; вернулся воркер → берёт следующий
// SID. Постоянно держится ≤ concurrency активных. Нет Leg-ов / барьеров /
// chunk-цикла — прогон плоский (batch_index=0 у всех целей зафиксирован при
// Insert-е, ADR-043 amendment §7).
//
// Переиспользование barrier-машинерии без изменений:
//   - [runOneCommand] — per-unit логика (VerifyOwnership-fencing перед dispatch,
//     SpawnCommand, MarkTargetRunning/Terminal, fail-closed маппинг);
//   - fanCtx + leaseLost-watcher — отмена окна при потере lease / ctx.Done
//     (воркеры перестают тянуть очередь, in-flight прерываются отменённым fanCtx);
//   - fenceLost — VerifyOwnership-CAS поймал реклейм раньше renew-тика;
//   - [summarize] — общая агрегация исходов в [voyage.Summary];
//   - [VoyageWorker.cancelRemaining] — пометка недоспавненного хоста cancelled
//     (parity barrier-markRemainingCancelled), вызывается при abort на остатке очереди.
//
// failThreshold (ADR-043 amendment §3, обобщённый abort-gate): порог абсолютного
// числа провалов, при котором прекращаем СПАВН новых из очереди (cancelFan
// останавливает выборку, текущие активные доработают); оставшиеся в очереди хосты
// помечаются cancelled (parity barrier: «оставшиеся Leg-и пропущены» →
// недоспавненные cancelled, Total = succeeded+failed+cancelled в обоих режимах).
// threshold=0 → без порога (окно вырабатывает очередь до конца, continue);
// threshold=1 → первый провал → стоп (backcompat abort); N>1 — толерантность.
//
// inter_unit_interval (ADR-043 amendment §4): per-unit пауза перед спавном каждой
// следующей единицы окна — мягкий throttle скользящего окна (parity
// inter_batch_interval между Leg-ами в barrier). Прерывается fanCtx.
//
// Возврат симметричен [executeCommandVoyage]: ("", nil, "") при потере lease /
// ctx.Done посреди окна (caller не финализирует — Reaper-reclaim); иначе
// финальный статус + summary.
func (w *VoyageWorker) runSlidingWindow(
	ctx context.Context, run *voyage.Voyage, sids []string, concurrency int,
	failThreshold int, leaseLost <-chan struct{},
) (voyage.Status, *voyage.Summary, string) {
	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()

	// watcher: потеря lease (renew-тик) отменяет fanCtx → воркеры перестают тянуть
	// очередь и in-flight прерываются. На happy-path выходит через fanCtx.Done.
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-leaseLost:
			cancelFan()
		case <-fanCtx.Done():
		}
	}()

	// queue — единая очередь хостов окна; буфер на всё, чтобы заполнить без
	// блокировки, далее воркеры тянут из неё (вернулся → следующий).
	queue := make(chan string, len(sids))
	for _, sid := range sids {
		queue <- sid
	}
	close(queue)

	var (
		mu        sync.Mutex
		results   = make([]commandResult, 0, len(sids))
		wg        sync.WaitGroup
		fenceLost atomic.Bool
		failCount atomic.Int64 // кумулятив провалов окна (обобщённый abort-gate §3).
	)

	appendResult := func(res commandResult) {
		mu.Lock()
		results = append(results, res)
		mu.Unlock()
	}

	// interUnit — per-unit пауза throttle (ADR-043 amendment §4). 0 → нет паузы.
	var interUnit time.Duration
	if run.InterUnitInterval != nil && *run.InterUnitInterval > 0 {
		interUnit = *run.InterUnitInterval
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// Приоритетный pre-check отмены: при одновременной готовности
				// queue-recv и fanCtx.Done Go выбрал бы ветку случайно и заспавнил
				// бы лишний Errand после потери lease / abort. Явная проверка делает
				// остановку детерминированной (parity runCommandLeg).
				select {
				case <-fanCtx.Done():
					return
				default:
				}

				var (
					sid string
					ok  bool
				)
				select {
				case <-fanCtx.Done():
					return
				case sid, ok = <-queue:
					if !ok {
						return // очередь выработана
					}
				}

				// inter_unit_interval (§4): per-unit throttle перед спавном единицы.
				// Прерывается fanCtx (lease lost / abort / ctx.Done) — тогда выходим,
				// не спавня (единица остаётся в очереди, дренируется как cancelled).
				if interUnit > 0 {
					t := time.NewTimer(interUnit)
					select {
					case <-t.C:
					case <-fanCtx.Done():
						t.Stop()
						// Хост уже снят с очереди — пометим cancelled (не успел стартовать).
						appendResult(w.cancelRemaining(ctx, run.VoyageID, sid))
						return
					}
				}

				res := w.runOneCommand(fanCtx, run, sid, cancelFan, &fenceLost)
				appendResult(res)

				// Обобщённый abort-gate (§3): достигнут порог провалов → прекращаем
				// СПАВН новых из очереди (cancelFan останавливает выборку у всех
				// воркеров); текущие активные доработают свой runOneCommand.
				// threshold=0 → без порога. fenceLost-путь уже дёрнул cancelFan
				// внутри runOneCommand — здесь дублируется безопасно (idempotent).
				if res.Outcome.isFailure() {
					n := failCount.Add(1)
					if failThreshold > 0 && n >= int64(failThreshold) {
						cancelFan()
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	cancelFan() // happy-path: разбудить watcher через fanCtx.Done.
	<-watcherDone

	// Потеря lease посреди окна → НЕ финализируем (Reaper-reclaim). Два детектора,
	// как в barrier-пути: renewLoop-канал leaseLost И fenceLost (VerifyOwnership-CAS
	// поймал реклейм раньше renew-тика). Здесь — раньше пометки cancelled остатка
	// очереди: при reclaim воладелец сменился, мы не пишем в его voyage_targets.
	select {
	case <-leaseLost:
		w.Logger.Warn("voyageorch: lease lost посреди window-окна — другой Keeper подберёт Voyage",
			slog.String("voyage_id", run.VoyageID))
		return "", nil, ""
	default:
	}
	if fenceLost.Load() {
		w.Logger.Warn("voyageorch: ownership-fence потерян посреди window-окна — другой Keeper подберёт Voyage",
			slog.String("voyage_id", run.VoyageID))
		return "", nil, ""
	}

	// abort мог оборвать окно до выработки очереди (cancelFan остановил выборку):
	// хосты, оставшиеся в буфере queue, никто не тянул. Помечаем их cancelled — так
	// же, как barrier-путь (runCommandLeg.markRemainingCancelled), чтобы Total =
	// succeeded+failed+cancelled совпадал в обоих режимах (drill UI одинаков).
	// Воркеры уже завершены (wg.Wait), дренаж closed-буфера безопасен; на
	// happy-path очередь пуста — цикл no-op.
	for sid := range queue {
		results = append(results, w.cancelRemaining(ctx, run.VoyageID, sid))
	}

	summary, anyFailure := summarize(results, len(sids))
	return scenarioFinalStatus(summary, anyFailure), summary, ""
}

// runCommandLeg исполняет один Leg: параллельный fan-out по хостам под
// semaphore-cap (concurrency). Per-host: SpawnCommand → MarkTargetRunning →
// MarkTargetTerminal. Возвращает срез исходов.
//
// leaseLost (КАК в S2-фиксе runLeg): renewal-goroutine может потерять lease
// ПОСРЕДИ длинного серийного Leg-а (batch_size=NULL → один Leg = все N хостов,
// concurrency=1). Продолжать spawn нельзя — Voyage уже мог быть переподобран
// другим Keeper-ом (runaway-спавн + дубль-Errand-ы). Производный fanCtx
// отменяется goroutine-наблюдателем при закрытии leaseLost:
//   - spawn-loop останавливается (acquire-select ловит fanCtx.Done);
//   - оставшиеся хосты помечаются cancelled (отчётность «не успели»);
//   - in-flight SpawnCommand прерываются через отменённый fanCtx.
//
// Финализацию при потере lease НЕ делаем — это решает caller
// (executeCommandVoyage проверяет leaseLost / fenceLost после runCommandLeg,
// Reaper-reclaim). Возвращает исходы + fenceLost: true, если ownership-fence
// (VerifyOwnership перед dispatch-ем, S-med-2) поймал потерю lease раньше
// renewLoop-тика — caller тогда тоже не финализирует.
func (w *VoyageWorker) runCommandLeg(ctx context.Context, run *voyage.Voyage, batchIndex int, leg []string, concurrency int, leaseLost <-chan struct{}) ([]commandResult, bool) {
	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()

	// goroutine-наблюдатель за leaseLost: на сигнал отменяет fanCtx, чтобы
	// остановить spawn-loop до старта оставшихся хостов. На happy-path
	// (wg.Wait → defer cancelFan) выходит через fanCtx.Done.
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-leaseLost:
			cancelFan()
		case <-fanCtx.Done():
		}
	}()

	sem := make(chan struct{}, concurrency)
	var (
		mu        sync.Mutex
		results   = make([]commandResult, 0, len(leg))
		wg        sync.WaitGroup
		fenceLost atomic.Bool // set: fencing-CAS перед dispatch-ем поймал потерю lease.
	)

	// markRemainingCancelled — оставшийся хост не стартует (fanCtx отменён:
	// lease lost / ctx.Done). Помечаем cancelled; MarkTargetTerminal под parent
	// ctx, чтобы запись прошла даже при graceful-shutdown отмене fanCtx.
	markRemainingCancelled := func(sid string) {
		mu.Lock()
		results = append(results, w.cancelRemaining(ctx, run.VoyageID, sid))
		mu.Unlock()
	}

	for _, sid := range leg {
		// Приоритетный pre-check отмены (КАК в S2 runLeg): при одновременной
		// готовности sem-acquire и fanCtx.Done Go выбрал бы ветку случайно и
		// заспавнил бы лишний Errand после потери lease. Явная проверка делает
		// остановку детерминированной.
		select {
		case <-fanCtx.Done():
			markRemainingCancelled(sid)
			continue
		default:
		}

		select {
		case sem <- struct{}{}:
		case <-fanCtx.Done():
			markRemainingCancelled(sid)
			continue
		}

		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			defer func() { <-sem }()

			res := w.runOneCommand(fanCtx, run, sid, cancelFan, &fenceLost)
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(sid)
	}

	wg.Wait()
	cancelFan() // happy-path: разбудить watcher через fanCtx.Done.
	<-watcherDone
	_ = batchIndex // batch_index уже зафиксирован в voyage_targets при Insert (S5).
	return results, fenceLost.Load()
}

// runOneCommand спавнит Errand на один хост (блокирующий до терминала) и
// обновляет voyage_targets. Ошибки трекинга (MarkTarget*) логируются, но не валят
// исход хоста — авторитет = статус, вернувшийся из SpawnCommand.
//
// Lease-fencing (S-med-2): dispatchErrand НЕ имеет своего ownership-guard-а (он
// шлёт Errand на Soul-сторону, parity top-level Finalize-CAS отсутствует на
// уровне leg-spawn). Поэтому ПЕРЕД спавном проверяем VerifyOwnership: воркер всё
// ещё владелец Voyage с моим claim-epoch (run.Attempt). Если lease потеряна
// (Reaper-reclaim → другой Keeper подобрал Voyage, attempt++) — НЕ шлём Errand
// (иначе дубль-исполнение дочернего Errand-а), помечаем хост cancelled, поднимаем
// fenceLost и через cancelFan останавливаем остаток Leg-а (встраиваемся в
// существующий fanCtx-механизм). Транзиентная PG-ошибка проверки (не ErrLeaseLost)
// — не подтверждённый реклейм: fail-closed по одному хосту (не шлём Errand,
// failed), без abort всего Leg-а.
func (w *VoyageWorker) runOneCommand(ctx context.Context, run *voyage.Voyage, sid string, cancelFan context.CancelFunc, fenceLost *atomic.Bool) commandResult {
	if err := voyage.VerifyOwnership(ctx, w.Pool, run.VoyageID, w.KID, run.Attempt); err != nil {
		if errors.Is(err, voyage.ErrLeaseLost) {
			// Lease потеряна посреди Leg-а — Errand НЕ отправляем (fencing).
			w.Logger.Warn("voyageorch: ownership-fence lost перед dispatch — Errand не отправлен",
				slog.String("voyage_id", run.VoyageID), slog.String("sid", sid),
				slog.String("kid", w.KID), slog.Int("attempt", run.Attempt))
			fenceLost.Store(true)
			cancelFan()
			w.trackCommandTerminal(ctx, run.VoyageID, sid, OutcomeCancelled)
			return commandResult{SID: sid, Outcome: OutcomeCancelled}
		}
		// Транзиентная PG-ошибка: не можем подтвердить владение — не шлём Errand,
		// fail-closed по этому хосту (без abort всего Leg-а).
		w.Logger.Warn("voyageorch: ownership-fence check failed (transient) — Errand не отправлен",
			slog.String("voyage_id", run.VoyageID), slog.String("sid", sid), slog.Any("error", err))
		w.trackCommandTerminal(ctx, run.VoyageID, sid, OutcomeFailed)
		return commandResult{SID: sid, Outcome: OutcomeFailed}
	}

	errandID, status, err := w.CommandSpawner.SpawnCommand(ctx, run.VoyageID, sid, *run.Module, run.StartedByAID, run.Input)
	if err != nil {
		// Внутренняя ошибка orchestrator-вызова (не failed-Errand). На отмену
		// ctx (leaseLost / abort) — cancelled, иначе failed (не молча success).
		var outcome TargetOutcome
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = OutcomeCancelled
		} else {
			w.Logger.Warn("voyageorch: spawn command failed",
				slog.String("voyage_id", run.VoyageID), slog.String("sid", sid), slog.Any("error", err))
			outcome = OutcomeFailed
		}
		// errandID может быть непуст (Errand создан, но dispatch вернул ошибку) —
		// проставляем back-link, если есть.
		if errandID != "" {
			w.markCommandRunning(ctx, run.VoyageID, sid, errandID, run.Attempt)
		}
		w.trackCommandTerminal(ctx, run.VoyageID, sid, outcome)
		return commandResult{SID: sid, ErrandID: errandID, Outcome: outcome}
	}

	if errandID != "" {
		w.markCommandRunning(ctx, run.VoyageID, sid, errandID, run.Attempt)
	}

	outcome := commandStatusToOutcome(status)
	w.trackCommandTerminal(ctx, run.VoyageID, sid, outcome)
	return commandResult{SID: sid, ErrandID: errandID, Outcome: outcome}
}

// markCommandRunning best-effort проставляет back-link errand_id (awaiting→running)
// под attempt-fenced CAS. Ошибка PG логируется warn — исход уже зафиксирован
// caller-ом.
func (w *VoyageWorker) markCommandRunning(ctx context.Context, voyageID, sid, errandID string, attempt int) {
	if err := voyage.MarkTargetRunning(ctx, w.Pool, voyageID, voyage.TargetKindSID, sid, errandID, attempt); err != nil {
		w.Logger.Warn("voyageorch: MarkTargetRunning failed (best-effort)",
			slog.String("voyage_id", voyageID), slog.String("sid", sid), slog.Any("error", err))
	}
}

// cancelRemaining помечает недоспавненный хост cancelled: пишет терминал в
// voyage_targets (best-effort) и возвращает соответствующий [commandResult].
// Единый механизм «оставшийся хост не успел стартовать» для обоих каркасов —
// barrier-Leg (fanCtx отменён посреди серийного Leg-а) и окно (abort оборвал
// выборку из очереди). MarkTargetTerminal — под parent ctx, чтобы запись прошла
// даже при отменённом fanCtx (graceful-shutdown / abort).
func (w *VoyageWorker) cancelRemaining(ctx context.Context, voyageID, sid string) commandResult {
	w.trackCommandTerminal(ctx, voyageID, sid, OutcomeCancelled)
	return commandResult{SID: sid, Outcome: OutcomeCancelled}
}

// trackCommandTerminal best-effort фиксирует терминал хоста в voyage_targets.
// Ошибка PG логируется warn — исход уже в commandResult (авторитет finalize).
func (w *VoyageWorker) trackCommandTerminal(ctx context.Context, voyageID, sid string, outcome TargetOutcome) {
	if err := voyage.MarkTargetTerminal(ctx, w.Pool, voyageID, voyage.TargetKindSID, sid, outcome.toTargetStatus()); err != nil {
		w.Logger.Warn("voyageorch: MarkTargetTerminal failed (best-effort)",
			slog.String("voyage_id", voyageID), slog.String("sid", sid),
			slog.String("outcome", string(outcome)), slog.Any("error", err))
	}
}
