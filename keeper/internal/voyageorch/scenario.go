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

// kind=scenario исполнение (ADR-043 S2, подход B1).
//
// Batch (Leg) = N ИНКАРНАЦИЙ. Каждая инкарнация в Leg-е — полноценный
// scenario-run со своим cross-host barrier и per-incarnation state-commit; этот
// commit делает САМ scenario-runner (ADR-009 §7), voyageorch его НЕ дублирует —
// он лишь оркестрирует N независимых scenario-run-ов и трекает их через
// `voyage_targets`.
//
// Алгоритм (executeScenarioVoyage):
//  1. target_resolved → []incarnationName (JSONB-массив имён).
//  2. chunkIncarnations(names, batch_size) → Leg-и (NULL/0 → один Leg = все).
//  3. Per Leg: параллельный fan-out по инкарнациям под semaphore-cap
//     (concurrency, parity errandrunorch.runFanOut). На каждую инкарнацию:
//     SpawnScenarioRun (= scenario-runner per-incarnation) → MarkTargetRunning →
//     Await terminal → MarkTargetTerminal.
//  4. on_failure=abort → первый провалившийся Leg останавливает переход к
//     следующему; on_failure=continue → идём до конца.
//  5. Между Leg-ами — пауза inter_batch_interval (контролируемая выкатка).
//  6. После всех Leg-ов — Finalize voyage (succeeded / partial_failed / failed)
//     + Summary под ownership-guard-ом.

// ScenarioSpawner — спавн одного per-incarnation scenario-run-а. Изолирует
// voyageorch от scenario-runner / ServiceRegistry / incarnation-CRUD (parity
// tideorch.SurgeSpawner / errandrunorch.ErrandSpawner): production wire-up (S5)
// подставит адаптер, который резолвит ServiceRef (incarnation.SelectByName →
// ServiceRegistry.Resolve) и вызывает scenario.Runner.Start; unit-тесты — fake
// без зависимостей.
//
// Контракт:
//   - возвращает applyID запущенного прогона (ULID, для back-link на apply_runs
//     и последующего [IncarnationAwaiter.Await]);
//   - вызов async: scenario.Runner.Start возвращается сразу (прогон живёт в своей
//     goroutine), терминал доезжает через apply_runs; ожидание — отдельной фазой
//     через Awaiter;
//   - err != nil — прогон не удалось даже запустить (incarnation не найдена /
//     error_locked / ServiceRef не резолвится / Runner.Start отверг). Caller
//     зачитывает target как failed без applyID.
//
// cadenceID — back-link на Cadence-расписание (voyages.cadence_id, ADR-046 §2):
// nil ⇒ ручной Voyage; populated ⇒ дочерний Voyage расписания. Production-spawner
// кладёт его в RunSpec.CadenceID, чтобы терминальное событие прогона
// (incarnation.run_completed) несло cadence_id для постоянных Tiding-правил с
// cadence-селектором (T4b).
type ScenarioSpawner interface {
	SpawnScenarioRun(ctx context.Context, voyageID, incarnationName, scenarioName string, input []byte, startedByAID string, cadenceID *string) (applyID string, err error)
}

// OrphanLockReleaser снимает осиротевший applying-lock инкарнации, оставшийся от
// scenario-run мёртвого Keeper-владельца ЭТОГО Voyage прошлого attempt (recovery-
// шов, ADR-027(k)). Изолирует voyageorch от incarnation/applyrun-CRUD (parity
// ScenarioSpawner): production wire-up (daemon) даёт адаптер поверх
// incarnation.ReleaseApplyingOrphan; unit-тесты — fake.
//
// orphanApplyID — back-link apply_id осиротевшего прогона из voyage_targets ЭТОГО
// Voyage (от прошлого attempt). Реализация обязана быть FENCED single-winner:
// снимает lock ТОЛЬКО когда инкарнация в applying И orphanApplyID ей принадлежит
// (incarnation.ReleaseApplyingOrphan: FOR UPDATE + apply_id-match + CAS).
//
// Контракт возврата:
//   - released=true  — lock снят (applying → ready), re-run может стартовать;
//   - released=false, err=nil — снимать нечего (не applying / orphan apply_id не
//     наш / honest-финал прошлого владельца уже выиграл строку): caller продолжает
//     re-run БЕЗ снятия (lockRun сам отбракует, если состояние всё ещё не runnable);
//   - err != nil — CRUD-сбой PG: caller логирует и НЕ спавнит (fail-closed по
//     инкарнации, не abort всего Voyage).
//
// nil-поле → детект осиротевшего lock выключен (unit-тест без recovery-шва /
// dev-сборка): runOneIncarnation идёт сразу к спавну как до фикса.
type OrphanLockReleaser interface {
	ReleaseOrphanLock(ctx context.Context, voyageID, incarnationName string, attempt int, kid string, orphanApplyID string) (released bool, err error)
}

// IncarnationAwaiter — ждёт terminal-статус одного per-incarnation scenario-run-а
// по applyID. Production wire-up (S5) даёт реализацию поверх
// applyrun.SelectStatusesByApplyID (poll до терминала всех хостов инкарнации,
// parity tideorch.PgApplyTerminalAwaiter); unit-тесты — fake.
//
// Блокируется до terminal либо ctx.Done. Возвращает [TargetOutcome] (succeeded /
// failed / cancelled / no_match). ctx.Err при отмене (graceful-shutdown /
// on_failure abort) — caller трактует target как cancelled.
type IncarnationAwaiter interface {
	Await(ctx context.Context, applyID string) (TargetOutcome, error)
}

// TargetOutcome — терминальный исход одного per-incarnation scenario-run-а,
// проецируется в [voyage.TargetStatus]. Closed-set значений совпадает с
// CHECK voyage_targets_status_valid (terminal-подмножество).
type TargetOutcome string

const (
	OutcomeSucceeded TargetOutcome = "succeeded"
	OutcomeFailed    TargetOutcome = "failed"
	OutcomeCancelled TargetOutcome = "cancelled"
	OutcomeNoMatch   TargetOutcome = "no_match"
)

// toTargetStatus переводит TargetOutcome в voyage.TargetStatus для записи в
// voyage_targets. Неизвестное значение → failed (fail-closed: не молча success).
func (o TargetOutcome) toTargetStatus() voyage.TargetStatus {
	switch o {
	case OutcomeSucceeded:
		return voyage.TargetStatusSucceeded
	case OutcomeCancelled:
		return voyage.TargetStatusCancelled
	case OutcomeNoMatch:
		return voyage.TargetStatusNoMatch
	default:
		return voyage.TargetStatusFailed
	}
}

// isFailure сообщает, считается ли исход провалом для decision-gate on_failure и
// подсчёта Summary.Failed. cancelled/no_match — НЕ провал (cancelled — следствие
// abort/shutdown, no_match — benign «инкарнация вне scope», parity
// tideorch.classifyApplyOutcome).
func (o TargetOutcome) isFailure() bool { return o == OutcomeFailed }

// parseIncarnationTargets разбирает target_resolved (kind=scenario): JSONB-массив
// имён инкарнаций (snapshot набора от старта прогона, ADR-043). Пустой массив /
// невалидный JSON — ошибка (Voyage без целей — программная ошибка S5-handler-а,
// Insert требует непустой target_resolved). Дубликаты и пустые строки
// отвергаются (UNIQUE PK voyage_targets и невалидный target_id).
func parseIncarnationTargets(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("voyageorch: empty target_resolved")
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("voyageorch: target_resolved (kind=scenario) не JSON-массив имён: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("voyageorch: target_resolved пуст (нет инкарнаций)")
	}
	seen := make(map[string]struct{}, len(names))
	for i, n := range names {
		if n == "" {
			return nil, fmt.Errorf("voyageorch: target_resolved[%d] пустое имя инкарнации", i)
		}
		if _, dup := seen[n]; dup {
			return nil, fmt.Errorf("voyageorch: target_resolved содержит дубликат инкарнации %q", n)
		}
		seen[n] = struct{}{}
	}
	return names, nil
}

// chunkIncarnations режет плоский список инкарнаций на последовательные Leg-и
// размером не более batchSize. batchSize <= 0 → один Leg на всё (NULL/0
// batch_size = «весь прогон одним Leg», ADR-043). batch_index 0-based (CHECK
// voyage_targets_batch_index_non_negative; первый Leg = 0, parity Insert-у
// targets). Пустой вход → пустой результат (caller финализирует succeeded).
func chunkIncarnations(names []string, batchSize int) [][]string {
	if len(names) == 0 {
		return nil
	}
	if batchSize <= 0 {
		return [][]string{append([]string(nil), names...)}
	}
	legs := make([][]string, 0, (len(names)+batchSize-1)/batchSize)
	for off := 0; off < len(names); off += batchSize {
		end := off + batchSize
		if end > len(names) {
			end = len(names)
		}
		legs = append(legs, append([]string(nil), names[off:end]...))
	}
	return legs
}

// targetResult — runtime-исход одной инкарнации в Leg-е, собирается в per-target
// goroutine и складывается под mutex. ApplyID пуст, если spawn упал до старта
// прогона (тогда Status=failed без back-link-а).
type targetResult struct {
	IncarnationName string
	ApplyID         string
	Outcome         TargetOutcome
}

// executeScenarioVoyage — S2-исполнитель kind=scenario: батчевый прогон scenario
// поверх N инкарнаций (B1). Вызывается из [VoyageWorker.executeVoyage] после
// claim-а; renewal-goroutine уже держит lease (leaseLost закрывается при потере).
//
// Возвращает финальный [voyage.Status] + [*voyage.Summary] + error_code для
// Finalize/finalize-audit. error_code непуст только для fail-closed-путей до
// старта инкарнаций (spawner_not_configured / empty_scenario_name /
// target_resolve_failed); для happy-path и «все инкарнации failed» — пуст. На
// потерю lease / ctx.Done посреди прогона возвращает ("", nil, "") — caller НЕ
// финализирует (Reaper-reclaim вернёт Voyage в pending, другой Keeper подберёт).
func (w *VoyageWorker) executeScenarioVoyage(ctx context.Context, run *voyage.Voyage, leaseLost <-chan struct{}) (voyage.Status, *voyage.Summary, string) {
	if w.ScenarioSpawner == nil || w.ScenarioAwaiter == nil {
		// Production wire-up (S5) обязан передать оба; их отсутствие при
		// claim-нутом scenario-Voyage — программная ошибка setup-а. Fail-closed:
		// финализируем failed, не молча succeeded.
		w.Logger.Error("voyageorch: scenario execution requested but Spawner/Awaiter not configured",
			slog.String("voyage_id", run.VoyageID),
		)
		return voyage.StatusFailed, &voyage.Summary{Total: run.TotalBatches}, "spawner_not_configured"
	}
	if run.ScenarioName == nil || *run.ScenarioName == "" {
		w.Logger.Error("voyageorch: kind=scenario без scenario_name", slog.String("voyage_id", run.VoyageID))
		return voyage.StatusFailed, &voyage.Summary{}, "empty_scenario_name"
	}

	names, err := parseIncarnationTargets(run.TargetResolved)
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
	// абсолютного числа провалов → стоп. abort ≡ 1 (backcompat: первый провал →
	// стоп); continue/nil без fail_threshold ≡ 0 (без порога); явный N → N.
	// 0 = «без порога».
	failThreshold := voyage.ResolveFailThreshold(run.FailThreshold, run.OnFailure)

	// batch_mode=window → скользящее окно по ИНКАРНАЦИЯМ (ADR-043 amendment §1,
	// S-W2): общий пул concurrency воркеров из единой очереди инкарнаций, без
	// Leg-ов/барьеров МЕЖДУ инкарнациями. §7-инвариант: окно режет только между
	// инкарнациями — ВНУТРИ инкарнации scenario-runner сохраняет cross-host barrier
	// + per-incarnation state-commit (единица окна = целый scenario-run одной
	// инкарнации). barrier (NULL) → существующий chunk+runLeg путь ниже.
	if voyage.ResolveBatchMode(run.BatchMode) == voyage.BatchModeWindow {
		return w.runScenarioSlidingWindow(ctx, run, names, concurrency, failThreshold, leaseLost)
	}

	batchSize := 0
	if run.BatchSize != nil {
		batchSize = *run.BatchSize
	}
	legs := chunkIncarnations(names, batchSize)

	var (
		summary    = &voyage.Summary{Total: len(names)}
		anyFailure bool
	)

	for legIdx, leg := range legs {
		// Раннее обнаружение потери lease / отмены — до спавна нового Leg-а
		// (иначе MarkTarget*/Finalize упрутся в ownership-guard).
		select {
		case <-leaseLost:
			w.Logger.Warn("voyageorch: lease lost между Leg-ами — другой Keeper подберёт Voyage",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
			w.emitLeaseLost(run, "leg")
			return "", nil, ""
		case <-ctx.Done():
			w.Logger.Info("voyageorch: scenario-loop прерван ctx.Done",
				slog.String("voyage_id", run.VoyageID), slog.Any("reason", ctx.Err()))
			return "", nil, ""
		default:
		}

		// Пауза перед каждым Leg-ом, кроме первого (inter_batch_interval —
		// контролируемая выкатка, ADR-043). Прерывается ctx.Done / leaseLost.
		if legIdx > 0 && run.InterBatchInterval != nil && *run.InterBatchInterval > 0 {
			if !w.interBatchPause(ctx, *run.InterBatchInterval, leaseLost) {
				w.Logger.Warn("voyageorch: прерван на inter_batch_interval — не финализируем",
					slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
				return "", nil, ""
			}
		}

		w.emitLegStarted(run, legIdx, len(leg))

		results := w.runLeg(ctx, run, legIdx, leg, concurrency, leaseLost)

		// Потеря lease ПОСРЕДИ Leg-а: runLeg остановил spawn-loop и прервал
		// in-flight Await-ы через отменённый fanCtx, оставшиеся инкарнации помечены
		// cancelled. НЕ финализируем (Reaper-reclaim вернёт Voyage в pending,
		// другой Keeper подберёт), как и при потере lease между Leg-ами.
		select {
		case <-leaseLost:
			w.Logger.Warn("voyageorch: lease lost посреди Leg-а — другой Keeper подберёт Voyage",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
			w.emitLeaseLost(run, "leg")
			return "", nil, ""
		default:
		}

		var legAgg legOutcome
		for _, res := range results {
			legAgg.total++
			switch res.Outcome {
			case OutcomeSucceeded, OutcomeNoMatch:
				summary.Succeeded++
				legAgg.succeeded++
			case OutcomeCancelled:
				summary.Cancelled++
				legAgg.cancelled++
			default:
				summary.Failed++
				legAgg.failed++
			}
			if res.Outcome == OutcomeNoMatch {
				summary.NoMatch++
			}
			if res.Outcome.isFailure() {
				anyFailure = true
			}
		}

		w.emitLegCompleted(run, legIdx, legAgg)

		// Прогресс батчей для UI: Leg legIdx ЗАВЕРШЁН → current_batch_index =
		// legIdx+1 (best-effort, ownership-guarded). Ошибка/0-rows не валит прогон
		// (правда о ходе — в voyage_targets), только warn.
		w.advanceBatchProgress(ctx, run, legIdx+1)

		// Обобщённый abort-gate (ADR-043 amendment §3): достигнут порог числа
		// провалов → прекращаем переход к следующему Leg. threshold=0 → без порога
		// (continue). summary.Failed — кумулятив провалов по всем Leg-ам.
		if failThreshold > 0 && summary.Failed >= failThreshold {
			w.Logger.Info("voyageorch: достигнут fail_threshold — оставшиеся Leg-и пропущены",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx),
				slog.Int("failed", summary.Failed), slog.Int("fail_threshold", failThreshold))
			break
		}
	}

	return scenarioFinalStatus(summary, anyFailure), summary, ""
}

// runLeg исполняет один Leg: параллельный fan-out по инкарнациям под
// semaphore-cap (concurrency). Per-incarnation: spawn → MarkTargetRunning →
// Await → MarkTargetTerminal. Возвращает срез исходов (для агрегации Summary +
// decision-gate).
//
// leaseLost (parity errandrunorch.runFanOut): renewal-goroutine может потерять
// lease ПОСРЕДИ длинного серийного Leg-а (batch_size=NULL → один Leg=все N
// инкарнаций, concurrency=1). Тогда продолжать spawn нельзя — Voyage уже мог
// быть переподобран другим Keeper-ом (runaway-спавн + дубль-спавны). Поэтому
// производный fanCtx отменяется goroutine-наблюдателем при закрытии leaseLost:
//   - spawn-loop останавливается (acquire-select ловит fanCtx.Done);
//   - оставшиеся инкарнации помечаются cancelled (отчётность «не успели»);
//   - in-flight Await-ы прерываются через отменённый fanCtx.
//
// Финализацию при потере lease НЕ делаем — это решает caller
// (executeScenarioVoyage проверяет leaseLost после runLeg, Reaper-reclaim).
func (w *VoyageWorker) runLeg(ctx context.Context, run *voyage.Voyage, batchIndex int, leg []string, concurrency int, leaseLost <-chan struct{}) []targetResult {
	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()

	// goroutine-наблюдатель за leaseLost: на сигнал отменяет fanCtx, чтобы
	// остановить spawn-loop до старта оставшихся инкарнаций. На happy-path
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
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([]targetResult, 0, len(leg))
	)

	// markRemainingCancelled — оставшаяся инкарнация не стартует (fanCtx отменён:
	// lease lost / ctx.Done). Помечаем cancelled (отчётность «не успели»);
	// MarkTargetTerminal под parent ctx, чтобы запись прошла даже при
	// graceful-shutdown отмене fanCtx.
	markRemainingCancelled := func(name string) {
		mu.Lock()
		results = append(results, targetResult{IncarnationName: name, Outcome: OutcomeCancelled})
		mu.Unlock()
		_ = w.markTargetCancelled(ctx, run.VoyageID, name)
	}

	for _, name := range leg {
		// Приоритетный pre-check отмены: при освобождении semaphore завершившейся
		// инкарнацией и одновременно отменённом fanCtx `select` ниже выбрал бы
		// ветку acquire случайно (Go-семантика равноготовых case) и заспавнил бы
		// лишнюю инкарнацию после потери lease. Явная проверка делает остановку
		// детерминированной.
		select {
		case <-fanCtx.Done():
			markRemainingCancelled(name)
			continue
		default:
		}

		select {
		case sem <- struct{}{}:
		case <-fanCtx.Done():
			markRemainingCancelled(name)
			continue
		}

		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			defer func() { <-sem }()

			res := w.runOneIncarnation(fanCtx, run, name)
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(name)
	}

	wg.Wait()
	cancelFan() // happy-path: разбудить watcher через fanCtx.Done.
	<-watcherDone
	_ = batchIndex // batch_index уже зафиксирован в voyage_targets при Insert (S5); здесь только трекаем статус.
	return results
}

// runScenarioSlidingWindow — S-W2 исполнитель batch_mode=window для kind=scenario
// (ADR-043 amendment §1). Полное скользящее окно по ИНКАРНАЦИЯМ: пул
// `concurrency` воркеров тянет имена инкарнаций из ОДНОЙ общей очереди прогона;
// вернулся воркер → берёт следующую. Постоянно держится ≤ concurrency активных
// scenario-run-ов. Нет Leg-ов / барьеров МЕЖДУ инкарнациями — прогон плоский
// (batch_index=0 у всех целей зафиксирован при Insert-е, ADR-043 amendment §7).
//
// §7-инвариант (КРИТИЧНО): окно режет только МЕЖДУ инкарнациями. ВНУТРИ
// инкарнации scenario-runner сохраняет cross-host barrier + per-incarnation
// state-commit — единица окна = целый scenario-run одной инкарнации, который
// SpawnScenarioRun запускает, а Await ждёт до терминала. scenario-runner НЕ
// трогается: voyageorch лишь оркестрирует N независимых scenario-run-ов.
//
// Lease-fencing на каждую инкарнацию (parity command runOneCommand, S-med-2):
// перед спавном КАЖДОЙ инкарнации [runOneIncarnationFenced] делает
// VerifyOwnership — мы всё ещё владелец Voyage с моим claim-epoch (run.Attempt).
// При потере lease (Reaper-reclaim) НЕ спавним scenario-run (иначе дубль), хост
// помечается cancelled, поднимается fenceLost и через cancelFan останавливается
// окно. barrier-путь scenario (runLeg/runOneIncarnation) НЕ трогается.
//
// failThreshold / inter_unit_interval / отмена окна / дренаж очереди — parity
// command.runSlidingWindow. require_alive для scenario НЕ применяется (единица =
// инкарнация, presence-фильтр осмыслен для хостов; поле хранится, не применяется).
//
// Возврат симметричен [executeScenarioVoyage]: ("", nil, "") при потере lease /
// ctx.Done посреди окна (caller не финализирует — Reaper-reclaim); иначе
// финальный статус + summary.
func (w *VoyageWorker) runScenarioSlidingWindow(
	ctx context.Context, run *voyage.Voyage, names []string, concurrency int,
	failThreshold int, leaseLost <-chan struct{},
) (voyage.Status, *voyage.Summary, string) {
	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()

	// watcher: потеря lease (renew-тик) отменяет fanCtx → воркеры перестают тянуть
	// очередь и in-flight Await-ы прерываются. На happy-path выходит через fanCtx.Done.
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-leaseLost:
			cancelFan()
		case <-fanCtx.Done():
		}
	}()

	// queue — единая очередь инкарнаций окна; буфер на всё, чтобы заполнить без
	// блокировки, далее воркеры тянут из неё (вернулся → следующая).
	queue := make(chan string, len(names))
	for _, name := range names {
		queue <- name
	}
	close(queue)

	var (
		mu        sync.Mutex
		results   = make([]targetResult, 0, len(names))
		wg        sync.WaitGroup
		fenceLost atomic.Bool
		failCount atomic.Int64 // кумулятив провалов окна (обобщённый abort-gate §3).
	)

	appendResult := func(res targetResult) {
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
				// бы лишний scenario-run после потери lease / abort. Явная проверка
				// делает остановку детерминированной (parity runLeg).
				select {
				case <-fanCtx.Done():
					return
				default:
				}

				var (
					name string
					ok   bool
				)
				select {
				case <-fanCtx.Done():
					return
				case name, ok = <-queue:
					if !ok {
						return // очередь выработана
					}
				}

				// inter_unit_interval (§4): per-unit throttle перед спавном единицы.
				// Прерывается fanCtx (lease lost / abort / ctx.Done) — тогда инкарнация
				// уже снята с очереди, помечаем cancelled (не успела стартовать).
				if interUnit > 0 {
					t := time.NewTimer(interUnit)
					select {
					case <-t.C:
					case <-fanCtx.Done():
						t.Stop()
						appendResult(w.cancelIncarnation(ctx, run.VoyageID, name))
						return
					}
				}

				res := w.runOneIncarnationFenced(fanCtx, run, name, cancelFan, &fenceLost)
				appendResult(res)

				// Обобщённый abort-gate (§3): достигнут порог провалов → прекращаем
				// СПАВН новых из очереди (cancelFan останавливает выборку у всех
				// воркеров); текущие активные доработают свой scenario-run.
				// threshold=0 → без порога. fenceLost-путь уже дёрнул cancelFan
				// внутри runOneIncarnationFenced — здесь дублируется безопасно.
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
	// поймал реклейм раньше renew-тика). Раньше пометки cancelled остатка очереди:
	// при reclaim владелец сменился, мы не пишем в его voyage_targets.
	select {
	case <-leaseLost:
		w.Logger.Warn("voyageorch: lease lost посреди scenario-window — другой Keeper подберёт Voyage",
			slog.String("voyage_id", run.VoyageID))
		return "", nil, ""
	default:
	}
	if fenceLost.Load() {
		w.Logger.Warn("voyageorch: ownership-fence потерян посреди scenario-window — другой Keeper подберёт Voyage",
			slog.String("voyage_id", run.VoyageID))
		return "", nil, ""
	}

	// abort мог оборвать окно до выработки очереди (cancelFan остановил выборку):
	// инкарнации, оставшиеся в буфере queue, никто не тянул. Помечаем cancelled —
	// как barrier-путь (markRemainingCancelled), чтобы Total = succeeded+failed+
	// cancelled совпадал в обоих режимах. Воркеры уже завершены (wg.Wait), дренаж
	// closed-буфера безопасен; на happy-path очередь пуста — цикл no-op.
	for name := range queue {
		results = append(results, w.cancelIncarnation(ctx, run.VoyageID, name))
	}

	summary, anyFailure := summarizeIncarnations(results, len(names))
	return scenarioFinalStatus(summary, anyFailure), summary, ""
}

// summarizeIncarnations агрегирует исходы инкарнаций в [voyage.Summary] (Total =
// полный scope прогона, передаётся отдельно — results может быть короче при
// abort до дренажа). Возвращает также anyFailure для выбора финального статуса.
// Используется только window-каркасом; barrier-путь агрегирует инлайн (legAgg).
func summarizeIncarnations(results []targetResult, total int) (*voyage.Summary, bool) {
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

// cancelIncarnation помечает не-стартовавшую инкарнацию cancelled (abort оборвал
// выборку из очереди window-окна / inter_unit прерван отменой): пишет терминал в
// voyage_targets (best-effort, под parent ctx) и возвращает [targetResult].
// Parity command.cancelRemaining; обёртка над markTargetCancelled для возврата
// targetResult одним вызовом.
func (w *VoyageWorker) cancelIncarnation(ctx context.Context, voyageID, name string) targetResult {
	_ = w.markTargetCancelled(ctx, voyageID, name)
	return targetResult{IncarnationName: name, Outcome: OutcomeCancelled}
}

// runOneIncarnationFenced — window-вариант [runOneIncarnation] с lease-fencing
// перед спавном (parity command.runOneCommand, S-med-2). barrier-путь использует
// runOneIncarnation БЕЗ fencing (там потерю lease ловит acquire-select runLeg по
// fanCtx); window-воркер тянет очередь напрямую, поэтому fencing нужен per-unit.
//
// VerifyOwnership: воркер всё ещё владелец Voyage с моим claim-epoch
// (run.Attempt). Если lease потеряна (Reaper-reclaim, attempt++) — scenario-run
// НЕ спавним (иначе дубль-исполнение), инкарнация cancelled, fenceLost=true, и
// cancelFan останавливает окно. Транзиентная PG-ошибка (не ErrLeaseLost) —
// не подтверждённый реклейм: fail-closed по одной инкарнации (не спавним, failed),
// без abort всего окна.
func (w *VoyageWorker) runOneIncarnationFenced(ctx context.Context, run *voyage.Voyage, name string, cancelFan context.CancelFunc, fenceLost *atomic.Bool) targetResult {
	if err := voyage.VerifyOwnership(ctx, w.Pool, run.VoyageID, w.KID, run.Attempt); err != nil {
		if errors.Is(err, voyage.ErrLeaseLost) {
			w.Logger.Warn("voyageorch: ownership-fence lost перед spawn scenario-run — не отправлен",
				slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name),
				slog.String("kid", w.KID), slog.Int("attempt", run.Attempt))
			fenceLost.Store(true)
			cancelFan()
			w.trackTerminal(ctx, run.VoyageID, name, OutcomeCancelled)
			return targetResult{IncarnationName: name, Outcome: OutcomeCancelled}
		}
		w.Logger.Warn("voyageorch: ownership-fence check failed (transient) — scenario-run не отправлен",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name), slog.Any("error", err))
		w.trackTerminal(ctx, run.VoyageID, name, OutcomeFailed)
		return targetResult{IncarnationName: name, Outcome: OutcomeFailed}
	}
	return w.runOneIncarnation(ctx, run, name)
}

// runOneIncarnation запускает per-incarnation scenario-run и ждёт его терминал,
// обновляя voyage_targets. Ошибки трекинга (MarkTarget*) логируются, но не валят
// прогон инкарнации — авторитет исхода = Await (фактический статус apply_runs).
func (w *VoyageWorker) runOneIncarnation(ctx context.Context, run *voyage.Voyage, name string) targetResult {
	// Recovery-шов (ADR-027(k)): если на инкарнации висит МОЙ осиротевший
	// applying-lock от прошлого attempt ЭТОГО Voyage (scenario-run мёртвого
	// прежнего владельца не доехал до state-commit-а), снимаем его FENCED перед
	// re-run — иначе lockRun отвергнет спавн («incarnation уже applying») и Voyage
	// зависнет навсегда. CRUD-сбой реконсиляции → fail-closed по инкарнации (не
	// спавним, failed), без abort всего Voyage.
	if rerr := w.reconcileOrphanLock(ctx, run, name); rerr != nil {
		w.Logger.Warn("voyageorch: реконсиляция осиротевшего applying-lock провалена — scenario-run не отправлен",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name), slog.Any("error", rerr))
		w.trackTerminal(ctx, run.VoyageID, name, OutcomeFailed)
		return targetResult{IncarnationName: name, Outcome: OutcomeFailed}
	}

	applyID, err := w.ScenarioSpawner.SpawnScenarioRun(ctx, run.VoyageID, name, *run.ScenarioName, run.Input, run.StartedByAID, run.CadenceID)
	if err != nil {
		w.Logger.Warn("voyageorch: spawn scenario-run failed",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name), slog.Any("error", err))
		w.trackTerminal(ctx, run.VoyageID, name, OutcomeFailed)
		return targetResult{IncarnationName: name, Outcome: OutcomeFailed}
	}

	if merr := voyage.MarkTargetRunning(ctx, w.Pool, run.VoyageID, voyage.TargetKindIncarnation, name, applyID, run.Attempt); merr != nil {
		w.Logger.Warn("voyageorch: MarkTargetRunning failed (best-effort)",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name), slog.Any("error", merr))
	}

	outcome, awerr := w.ScenarioAwaiter.Await(ctx, applyID)
	if awerr != nil {
		// ctx.Done / await-ошибка: scenario-run сам доедет до терминала через
		// apply_runs, но мы его не дождались. cancelled на ctx-отмене (abort/
		// shutdown), иначе failed (не молча success).
		if errors.Is(awerr, context.Canceled) || errors.Is(awerr, context.DeadlineExceeded) {
			outcome = OutcomeCancelled
		} else {
			w.Logger.Warn("voyageorch: await incarnation terminal failed",
				slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name),
				slog.String("apply_id", applyID), slog.Any("error", awerr))
			outcome = OutcomeFailed
		}
	}

	w.trackTerminal(ctx, run.VoyageID, name, outcome)
	return targetResult{IncarnationName: name, ApplyID: applyID, Outcome: outcome}
}

// reconcileOrphanLock — recovery-шов ADR-027(k): детект + FENCED снятие
// осиротевшего applying-lock инкарнации перед повторным спавном scenario-run
// реклеймнутого Voyage. Возвращает error ТОЛЬКО на CRUD-сбое PG (caller →
// fail-closed по инкарнации); «нечего снимать» / «снято» — оба nil.
//
// ТРИ FENCING-условия (все обязательны перед снятием, иначе двойной apply на
// живом):
//
//  1. ДЕТЕКТ + apply_id-match: в voyage_targets ЭТОГО voyage_id есть строка
//     инкарнации с записанным back-link apply_id и НЕ-терминальным статусом
//     (running/awaiting). Этот apply_id — orphan прошлого attempt ПО ПОСТРОЕНИЮ:
//     MarkTargetRunning пишет back-link ПОСЛЕ спавна под CAS `v.attempt=$attempt`,
//     а мы СЕЙЧАС ДО спавна нашего attempt — значит записанный apply_id не наш,
//     он от прошлого прохода (attempt < run.Attempt). Финальную привязку apply_id
//     ↔ инкарнация проверяет incarnation.ReleaseApplyingOrphan (apply_runs-EXISTS).
//  2. reclaimed-attempt + self-ownership: VerifyOwnership(voyage_id, KID,
//     run.Attempt) — мы текущий владелец Voyage с claim-epoch run.Attempt. Если
//     нас самих реклеймнули (ErrLeaseLost) — НЕ трогаем lock (его снимет реальный
//     новый владелец); транзиентная PG-ошибка — fail-closed. Сам факт, что мы в
//     этой точке как ВЛАДЕЛЕЦ run.Attempt, а back-link уже стоял от чужого
//     apply_id — подтверждает re-claim (прошлый владелец потерял Voyage, его
//     RunResult fenced на уровне apply_runs.attempt, ADR-027(g)).
//  3. single-winner CAS: снятие atomic через incarnation.ReleaseApplyingOrphan
//     (FOR UPDATE + guard status='applying'). Если честный RunResult прошлого
//     владельца уже финализировал инкарнацию — снятие no-op (released=false).
func (w *VoyageWorker) reconcileOrphanLock(ctx context.Context, run *voyage.Voyage, name string) error {
	if w.OrphanReleaser == nil {
		return nil // детект выключен (unit / dev без recovery-шва)
	}

	// ДЕТЕКТ: back-link orphan apply_id из voyage_targets ЭТОГО Voyage. scope мал
	// (N инкарнаций одного Voyage) — полный SelectTargets + фильтр дешевле узкого
	// селектора. Нет строки / нет apply_id / target уже терминал → нечего снимать.
	targets, err := voyage.SelectTargets(ctx, w.Pool, run.VoyageID)
	if err != nil {
		return fmt.Errorf("voyageorch: select targets для orphan-детекта: %w", err)
	}
	orphanApplyID := ""
	for i := range targets {
		t := &targets[i]
		if t.TargetKind != voyage.TargetKindIncarnation || t.TargetID != name {
			continue
		}
		// Только running/awaiting с записанным back-link — orphan-кандидат.
		// Терминальный target (succeeded/failed/cancelled/no_match) уже доехал —
		// инкарнация не висит. ApplyID==nil — спавн прошлого attempt не дошёл до
		// MarkTargetRunning, lock не выставлен этим Voyage → не наш orphan.
		if t.ApplyID != nil && *t.ApplyID != "" &&
			(t.Status == voyage.TargetStatusRunning || t.Status == voyage.TargetStatusAwaiting) {
			orphanApplyID = *t.ApplyID
		}
		break
	}
	if orphanApplyID == "" {
		return nil // нет back-link прошлого attempt — нечего снимать
	}

	// FENCING-2/3 + single-winner снятие. ReleaseOrphanLock сначала
	// VerifyOwnership (мы владелец run.Attempt; ErrLeaseLost → released=false,
	// err=nil — нас реклеймнули, lock не трогаем), затем atomic CAS снятия.
	released, rerr := w.OrphanReleaser.ReleaseOrphanLock(ctx, run.VoyageID, name, run.Attempt, w.KID, orphanApplyID)
	if rerr != nil {
		return rerr
	}
	if released {
		w.Logger.Info("voyageorch: осиротевший applying-lock снят перед re-run (recovery-шов ADR-027(k))",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name),
			slog.String("orphan_apply_id", orphanApplyID),
			slog.String("kid", w.KID), slog.Int("attempt", run.Attempt))
	} else {
		w.Logger.Info("voyageorch: осиротевший applying-lock НЕ снят (fenced no-op: не applying / orphan apply_id не наш / нас реклеймнули)",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name),
			slog.String("orphan_apply_id", orphanApplyID),
			slog.String("kid", w.KID), slog.Int("attempt", run.Attempt))
	}
	return nil
}

// trackTerminal best-effort фиксирует терминал target-а в voyage_targets. Ошибка
// PG логируется warn — исход уже зафиксирован в targetResult (авторитет finalize).
func (w *VoyageWorker) trackTerminal(ctx context.Context, voyageID, name string, outcome TargetOutcome) {
	if err := voyage.MarkTargetTerminal(ctx, w.Pool, voyageID, voyage.TargetKindIncarnation, name, outcome.toTargetStatus()); err != nil {
		w.Logger.Warn("voyageorch: MarkTargetTerminal failed (best-effort)",
			slog.String("voyage_id", voyageID), slog.String("incarnation", name),
			slog.String("outcome", string(outcome)), slog.Any("error", err))
	}
}

// markTargetCancelled best-effort помечает не-стартовавший target cancelled
// (ctx.Done во время раздачи Leg-а). Ошибка PG логируется warn.
func (w *VoyageWorker) markTargetCancelled(ctx context.Context, voyageID, name string) error {
	if err := voyage.MarkTargetTerminal(ctx, w.Pool, voyageID, voyage.TargetKindIncarnation, name, voyage.TargetStatusCancelled); err != nil {
		w.Logger.Warn("voyageorch: MarkTargetTerminal(cancelled) failed (best-effort)",
			slog.String("voyage_id", voyageID), slog.String("incarnation", name), slog.Any("error", err))
		return err
	}
	return nil
}

// interBatchPause ждёт duration между Leg-ами. Возвращает false при прерывании
// (ctx.Done / leaseLost) — caller тогда не финализирует.
func (w *VoyageWorker) interBatchPause(ctx context.Context, d time.Duration, leaseLost <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-leaseLost:
		return false
	}
}

// scenarioFinalStatus выбирает терминальный статус Voyage по Summary:
//   - все success (или no_match)         → succeeded;
//   - был хоть один fail, но и успех есть → partial_failed;
//   - все failed (никто не success)      → failed.
//
// cancelled-only (abort до единого успеха) трактуется как failed —
// был провал, который и вызвал abort.
func scenarioFinalStatus(s *voyage.Summary, anyFailure bool) voyage.Status {
	if !anyFailure && s.Cancelled == 0 {
		return voyage.StatusSucceeded
	}
	if s.Succeeded == 0 {
		return voyage.StatusFailed
	}
	return voyage.StatusPartialFailed
}
