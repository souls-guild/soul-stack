package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// run — тело run-goroutine: полный прогон одного scenario от резолва
// incarnation до commit-а incarnation.state. Все ошибки логируются и (где это
// меняет state) переводят incarnation в error_locked; наружу run ничего не
// возвращает (caller — goroutine).
func (r *Runner) run(ctx context.Context, spec RunSpec) {
	log := r.logger.With(
		slog.String("apply_id", spec.ApplyID),
		slog.String("incarnation", spec.IncarnationName),
		slog.String("scenario", spec.ScenarioName),
	)

	// In-process span на весь прогон scenario. incarnation/scenario name —
	// атрибуты для фильтрации трейса (в metric-labels их нельзя — cardinality,
	// ADR-024 §2.2); секретов не несут. apply_id — корреляция с RunResult /
	// audit. При OTel disabled tracer no-op — Start/End бесплатны.
	ctx, span := tracer.Start(ctx, "scenario.run",
		trace.WithAttributes(
			attribute.String("incarnation", spec.IncarnationName),
			attribute.String("scenario", spec.ScenarioName),
			attribute.String("apply_id", spec.ApplyID),
		),
	)

	// Метрика прогона фиксируется один раз на любом выходе. result/duration
	// заполняются по ходу: locked-выход не наполняет histogram (duration=0,
	// прогон не стартовал); ok/failed — наполняют от старта до терминала.
	started := time.Now()
	result := runResultFailed
	defer func() {
		var dur float64
		if result != runResultLocked {
			dur = time.Since(started).Seconds()
		}
		r.deps.Metrics.ObserveRun(result, dur)
		if result != runResultOK {
			span.SetStatus(codes.Error, result)
		}
		span.End()
	}()

	// 1. Резолв incarnation + проверка статуса, перевод в applying. После
	//    этого любой ранний выход обязан зафиксировать терминальный статус
	//    (lockRun уже выставил applying — incarnation нельзя оставить
	//    «висящей»).
	inc, err := r.lockRun(ctx, spec)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			result = runResultLocked
			log.Warn("scenario: incarnation уже в статусе applying — прогон отклонён")
			return
		}
		if errors.Is(err, ErrLocked) {
			result = runResultLocked
			log.Warn("scenario: incarnation в статусе error_locked — прогон отклонён до unlock")
			return
		}
		if errors.Is(err, ErrNotRunnable) {
			result = runResultLocked
			log.Warn("scenario: статус incarnation не допускает прогон — отклонён",
				slog.Any("error", err))
			return
		}
		log.Error("scenario: подготовка прогона провалена", slog.Any("error", err))
		return
	}
	stateBefore := inc.State

	// Терминальный статус провала зависит от режима финала (S-D2b):
	//   - TerminalCommitState → error_locked (обычный прогон, state не меняем);
	//   - TerminalDestroy      → destroy_failed (teardown упал; НЕ error_locked,
	//     из destroy_failed оператор повторяет destroy / анлочит в ready).
	// state не меняется в обоих случаях (last known-good).
	failStatus := failureStatus(spec.TerminalMode)

	// tasks/plans объявлены ДО abort, чтобы провальное incarnation.run_completed
	// (ADR-052 §k) видело ЧАСТИЧНОЕ состояние при ПОЗДНЕМ abort (dispatch_failed/
	// register_load_failed/…: render уже наполнил их, changed_tasks несёт что
	// успело CHANGED). На РАННЕМ abort (до render) остаются nil → buildChangedTasks
	// вернёт пусто. Заполняются присваиванием в render-точке (шаг 5), не `:=`.
	var (
		tasks []*render.RenderedTask
		plans []render.DispatchPlan
	)

	// С этого момента провал прогона = failStatus (state не меняем).
	// result остаётся runResultFailed на любом abort-выходе ниже; ok
	// выставляется только при успешном финале.
	abort := func(reason string, cause error) {
		// cause может нести vault:secret/-ref из render/dispatch-ошибки. Тот же
		// маскинг, что и для status_details в lockIncarnation, применяем к обоим
		// наблюдаемым каналам: OTel-трейс (span exception) и slog (лог-файл) —
		// нельзя оставлять асимметрию с маскированным DB-status_details.
		maskedCause := errors.New(maskErrText(cause))
		span.RecordError(maskedCause)
		log.Error("scenario: прогон провален — incarnation заблокирована",
			slog.String("reason", reason),
			slog.String("terminal_status", string(failStatus)),
			slog.String("error", maskErrText(cause)))
		finalized := r.lockIncarnation(ctx, spec, stateBefore, failStatus, reason, cause, log)
		// BAG-1: гарантируем терминальную строку apply_runs прогона. Ранний abort
		// (no_hosts и пр.) случается ДО dispatch-фазы — НИ ОДНОЙ строки apply_runs
		// ещё нет, а Voyage-awaiter поллит их до терминала ВСЕХ строк и при пустом
		// наборе вечно ждёт. error_locked — статус incarnation, не apply_runs;
		// awaiter его не видит. ensureTerminalApplyRun закрывает прогон в
		// терминальном статусе провала (терминалит недотерминальные при позднем
		// abort либо вставляет sentinel при пустом наборе) — barrier/awaiter
		// получают терминал. Статус зависит от причины: оператор-инициированный
		// Cancel → cancelled, всё прочее (включая timeout/Shutdown) → failed.
		// keepRunning: при операторском Cancel (errCancelRequested) running-apply
		// на ЖИВОМ хосте оставляем нетронутым — дойдёт честный RunResult. При
		// timeout/dead-host/ctx.Err RunResult НЕ придёт (хост завис), а reaper
		// running не подбирает (сужен до claimed, ADR-027) — поэтому running
		// форс-фейлим, иначе apply_run висит вечно (BAG-1 recovery).
		keepRunning := errors.Is(cause, errCancelRequested)
		r.ensureTerminalApplyRun(ctx, spec, reason, failureTerminalStatus(cause), keepRunning, log)

		// Терминальное событие провала прогона (T4-фундамент, ADR-052 §k):
		// incarnation.run_completed (status=failed) с частичным/пустым changed_tasks,
		// симметрично success-ветке run(). Гейт:
		//   - TerminalDestroy НЕ эмитит (у destroy свой терминал — destroy_completed/
		//     destroy_failed через writeDestroyFailedAudit, см. lockIncarnation);
		//   - SINGLE-WINNER: эмитим ТОЛЬКО когда наш lockIncarnation реально записал
		//     терминал (finalized). При ErrAlreadyFinalized (recovery-проигравший)
		//     событие отдаёт инстанс-победитель — не задвоить событие на прогон.
		// На раннем abort (до render) tasks/plans=nil → changed_tasks пуст; на позднем
		// — частичный (что успело CHANGED). status="failed" единым значением
		// (error_locked → failed, под-статусы не плодим). Best-effort: не валит
		// обработку провала (detached-ctx внутри emitRunCompleted).
		if spec.TerminalMode != TerminalDestroy && finalized {
			r.emitRunCompleted(ctx, spec, runCompletedStatusFailed, tasks, plans, log)
		}
	}

	// 2. Загрузка service-артефакта (один git-снапшот на прогон) + парсинг
	//    scenario/<name>/main.yml.
	art, err := r.deps.Loader.Load(ctx, spec.ServiceRef)
	if err != nil {
		abort("scenario_load_failed", fmt.Errorf("scenario: load service: %w", err))
		return
	}
	scn, err := r.parseScenario(art, spec.ScenarioName)
	if err != nil {
		abort("scenario_load_failed", err)
		return
	}

	// Раскрытие include в плоский список задач — ДО render (orchestration.md §6,
	// двухуровневый резолв scenario-локально → service-level, циклы детектируются
	// по resolved-пути). Render плоский список уже умеет; include-узлов в его
	// входе после этого не остаётся.
	expanded, idiags := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(r.deps.Loader, art, spec.ScenarioName))
	if diag.HasErrors(idiags) {
		abort("scenario_load_failed", fmt.Errorf("scenario: раскрытие include в %s/%s: %s", spec.ScenarioName, scenarioMainFile, firstError(idiags)))
		return
	}
	scn.Tasks = expanded

	// 3. Резолв хостов прогона (roster по Coven-метке incarnation).
	hosts, err := r.deps.Topology.LoadIncarnationHosts(ctx, spec.IncarnationName)
	if err != nil {
		abort("topology_failed", err)
		return
	}
	if len(hosts) == 0 {
		abort("no_hosts", fmt.Errorf("incarnation %q не имеет connected-хостов", spec.IncarnationName))
		return
	}

	// 4. Essence (effective-слой). render.Pipeline пробрасывает её в CEL как
	//    `essence.<path>` (slice E2). Берём OS-family первого хоста как
	//    представителя (per-host essence — расширение).
	essenceMap, err := r.deps.Essence.Resolve(essenceInput(art.LocalDir, inc, hosts[0]))
	if err != nil {
		abort("essence_failed", err)
		return
	}

	// 4.5. Эффективный input: применяем scenario `input:`-схему к переданным
	//      оператором значениям. Порядок: merge дефолтов + required →
	//      scoped-резолв `vault:`-ref input → value-валидация (pattern/enum
	//      проверяются на УЖЕ резолвнутом значении, docs/input.md §«vault_scope»).
	//      Резолв vault-ref — ОДИН раз здесь (render читает input N раз уже
	//      резолвнутым). Без merge непереданные параметры с default: остаются
	//      отсутствующими, и CEL `${ input.<def> }` падает «no such key».
	//      Vault=nil (unit/L0) → input-vault-refs не резолвятся.
	resolver := r.newInputVaultResolver(ctx, inputVaultAuditCtx{
		aid:         spec.StartedByAID,
		incarnation: spec.IncarnationName,
		scenario:    spec.ScenarioName,
	}, r.deps.InputDenyPaths)
	effectiveInput, err := config.ResolveInputValuesVault(scn.Input, spec.Input, resolver)
	if err != nil {
		abort("input_invalid", fmt.Errorf("scenario: input %s/%s: %w", spec.IncarnationName, spec.ScenarioName, err))
		return
	}

	// 5. Render: vault-resolve → CEL → on/where → []RenderedTask + []DispatchPlan.
	//    Destiny-резолвер — per-run: знает destiny[]-refs ЭТОГО service-снапшота
	//    (art.Manifest.Destiny[]) + default_destiny_source. nil-Destiny в Deps →
	//    apply:destiny отвергается render-фазой (ErrUnsupportedDSL).
	renderIn := render.RenderInput{
		Scenario: scn,
		Essence:  essenceMap,
		Input:    effectiveInput,
		Incarnation: render.IncarnationMeta{
			Name:           inc.Name,
			Service:        inc.Service,
			ServiceVersion: inc.ServiceVersion,
		},
		Hosts: hosts,
		Ctx:   ctx, // vault() (RenderStateChanges не идёт через Render → ctx нужен явно)
		// Templates: ридер .tmpl снапшота сервиса для core.file.rendered.
		// Двухуровневый резолв scenario-local→service-level (ADR-009): чтение
		// через artifact.ReadSnapshotFile поверх art.LocalDir (securejoin-защита).
		Templates: render.NewSnapshotTemplateReader(
			func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(art.LocalDir, rel) },
			scenarioTemplatePrefix(spec.ScenarioName),
		),
	}
	if r.deps.Destiny != nil {
		renderIn.Destiny = r.deps.Destiny.resolverFor(art.Manifest)
	}
	tasks, plans, err = r.deps.Render.Render(ctx, renderIn)
	if err != nil {
		abort("render_failed", err)
		return
	}

	// 5.5. Keeper-side задачи (`on: keeper`, docs/keeper/modules.md) исполняются
	//      ЛОКАЛЬНО на этом инстансе через keeper-side core-Registry — до host-
	//      fan-out-а (keeper-шаги в реальных сценариях идут первыми:
	//      provision/coven-bind → apply на хостах). Они пишут свою apply_runs-
	//      строку (sid=render.KeeperTargetSID) + register, которые финальный
	//      barrier и loadRegisterByHost видят наравне с host-строками. Первая
	//      упавшая keeper-задача → abort (host-dispatch не стартует). Прогон без
	//      keeper-задач → no-op.
	if err := r.dispatchKeeperTasks(ctx, spec, log, tasks, plans); err != nil {
		abort("keeper_dispatch_failed", err)
		return
	}

	// 6. Dispatch: ветвление пути (ADR-027, Phase 1.4.2).
	//
	//   - serial-guard: scenario с любой задачей `serial:` (после ExpandIncludes)
	//     → СТАРЫЙ путь (inline render+SendApply+per-wave barrier), даже при
	//     AcolyteEnabled. Распределённый serial — Phase 3.
	//   - иначе AcolyteEnabled → НОВЫЙ путь (dispatchPlanned): planned-задания на
	//     все roster-хосты + Summons; render/SendApply делает Acolyte при claim.
	//   - иначе → старый путь (прямой Insert(running)+SendApply).
	//
	// Render выше (шаги 4.5/5) выполняется в ОБОИХ путях: run-goroutine держит
	// tasks/renderIn для post-barrier register-load + state_changes-commit
	// (KEY-инвариант: barrier+commit остаются в run-goroutine в Phase 1). state
	// коммитится строго ПОСЛЕ барьера — единый barrier, не по-волново (§7).
	if r.acolyteEnabled && !hasSerialTask(scn) {
		if err := r.dispatchPlanned(ctx, spec, log, hosts, tasks); err != nil {
			abort("dispatch_failed", err)
			return
		}
	} else {
		if err := r.dispatch(ctx, spec, log, tasks, plans); err != nil {
			abort("dispatch_failed", err)
			return
		}
	}

	// 6.5. Teardown-финал (TerminalMode=TerminalDestroy, S-D2b): barrier прошёл
	//      на ВСЕХ хостах incarnation → teardown scenario `destroy` успешен.
	//      НЕ коммитим ready и НЕ трогаем incarnation.state (destroy не правит
	//      state-граф; teardown работает с хостами, не с jsonb): incarnation
	//      остаётся в `destroying`. Шаги 7-8 (register-load + state_changes-
	//      commit) — путь обычного прогона, для destroy не выполняются.
	//      result=runResultOK фиксирует успешный teardown для метрик/трейса.
	if spec.TerminalMode == TerminalDestroy {
		// In-process span на teardown-финал (снос строки incarnation после
		// успешного teardown на хостах). Дочерний к scenario.run — даёт отдельную
		// длительность archive+DELETE-tx в трейсе (отличить «teardown на хостах»
		// от «снос строки в БД»). Атрибуты БЕЗ секретов: incarnation/scenario name
		// уже несёт родитель; здесь — только число хостов/задач (cardinality-safe
		// числа, не label-ы). При OTel disabled tracer no-op — Start/End бесплатны.
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer dcancel()
		dctx, dspan := tracer.Start(dctx, "scenario.destroy_teardown",
			trace.WithAttributes(
				attribute.Int("hosts", len(hosts)),
				attribute.Int("tasks", len(tasks)),
			),
		)
		defer dspan.End()
		// S-D3: teardown прошёл на всех хостах → физический снос строки.
		// Archive (incarnation_archive / state_history_archive, миграция 039) +
		// single-winner DELETE WHERE status='destroying' + audit
		// destroy_completed — одна tx внутри DeleteAfterTeardown (каскад V3
		// снесёт live state_history / apply_runs / register; архив записан ДО
		// DELETE). result=runResultOK фиксирует успех для метрик даже если
		// DELETE дал no-op (Deleted=false — кто-то уже снёс строку): teardown
		// сам по себе успешен. Detached-ctx: исходный ctx мог быть отменён
		// (Shutdown/timeout), но снести строку после успешного teardown надо.
		res, derr := incarnation.DeleteAfterTeardown(dctx, r.deps.DB, r.deps.Audit, spec.IncarnationName, destroyForce(inc), log)
		if derr != nil {
			// DELETE/archive провалился уже после успешного teardown на хостах:
			// хосты снесены, но строка не удалена. error-выход НЕ переводит в
			// destroy_failed (failureStatus сработал бы только через abort до
			// этой точки) — оставляем destroying для триажа, оператор повторит
			// destroy. result остаётся runResultFailed (defer зафиксирует).
			dspan.RecordError(derr)
			dspan.SetStatus(codes.Error, "teardown_delete_failed")
			span.RecordError(derr)
			log.Error("scenario: teardown успешен, но снос строки incarnation провалился — остаётся в destroying",
				slog.Any("error", derr))
			return
		}
		dspan.SetAttributes(attribute.Bool("deleted", res.Deleted))
		result = runResultOK
		log.Info("scenario: destroy завершён — teardown успешен, строка incarnation снесена",
			slog.Bool("deleted", res.Deleted), slog.Int("tasks", len(tasks)), slog.Int("hosts", len(hosts)))
		return
	}

	// 7. После барьера — загрузка register-данных задач прогона (накопленных
	//    из TaskEvent-ов в apply_task_register) и резолв в per-host register-map
	//    (sid → register-name → payload) по mapping task_idx→register-name из
	//    tasks. Это даёт `sets: ${ register.<task>.<поле> }` доступ к register
	//    (слайс 2 полной грамматики, orchestration.md §7.1).
	registerByHost, err := r.loadRegisterByHost(ctx, spec.ApplyID, tasks)
	if err != nil {
		abort("register_load_failed", err)
		return
	}
	renderIn.RegisterByHost = registerByHost

	// 8. Все задачи success на всех хостах → рендер state_changes.sets
	//    (Keeper-side CEL, last-wins cross-host) и commit в incarnation.state.
	//    Рендер sets — строго ПОСЛЕ барьера (orchestration.md §7.1): значения
	//    фиксируются по факту успешного apply, не до него.
	renderedSets, err := r.deps.Render.RenderStateChanges(renderIn)
	if err != nil {
		abort("state_changes_render_failed", err)
		return
	}
	stateAfter := mergeStateChanges(stateBefore, renderedSets)
	if err := r.commitSuccess(ctx, spec, stateBefore, stateAfter); err != nil {
		// Single-winner (ADR-027(j) W1): incarnation уже выведена из applying
		// другим коммиттером (recovery-перехват / параллельный финал) — НЕ
		// провал. Не abort-им (не затираем чужой терминал error_locked-ом),
		// прогон сам по себе успешен на хостах. result=runResultOK.
		if errors.Is(err, incarnation.ErrAlreadyFinalized) {
			result = runResultOK
			log.Info("scenario: state-commit пропущен — incarnation уже финализирована другим коммиттером",
				slog.Int("tasks", len(tasks)), slog.Int("hosts", len(hosts)))
			// Проигравший коммит НЕ эмитит incarnation.run_completed (return до
			// emitRunCompleted ниже): событие отдаёт инстанс-победитель (чей
			// commitSuccess прошёл) — сознательная защита от дублей события на прогон.
			return
		}
		// Commit провалился уже после успешного apply на хостах — хосты в
		// нужном состоянии, но БД не синхронизирована. error_locked для
		// триажа (state не трогаем — UpdateStateFromRun в commitSuccess
		// атомарна, при фейле транзакция откатилась).
		abort("state_commit_failed", err)
		return
	}
	result = runResultOK
	log.Info("scenario: прогон завершён успешно", slog.Int("tasks", len(tasks)), slog.Int("hosts", len(hosts)))

	// Терминальное событие per-incarnation итога прогона (T3, ADR-052 §k):
	// incarnation.run_completed (status=success) с per-task changed_tasks.
	// Эмитится на успешном финале обычного прогона (TerminalDestroy финализируется
	// выше своим путём — destroy_completed/destroy_failed; provals — через abort).
	// Best-effort: не валит уже-успешный прогон (result=runResultOK зафиксирован).
	r.emitRunCompleted(ctx, spec, runCompletedStatusSuccess, tasks, plans, log)
}

// lockRun резолвит incarnation, проверяет статус под FOR UPDATE и переводит её
// в рабочий статус прогона. Gate — explicit allow-list (fail-closed): набор
// допустимых стартовых статусов зависит от режима финала (S-D2b). Новый статус,
// добавленный в enum позже, по умолчанию ОТКЛОНЯЕТ прогон, а не молча разрешает.
//
// TerminalCommitState (обычный прогон): стартует ТОЛЬКО из ready → переводит в
// applying. Отказы (специфичные sentinel-ы — для понятного лога/result наверху):
//   - applying       → [ErrAlreadyRunning] (другой прогон в работе — на pilot
//     отказ, не очередь);
//   - error_locked   → [ErrLocked] (прогон отклоняется до явного unlock, ADR-009;
//     retry из error_locked НЕ разрешён);
//   - всё остальное (destroying / migration_failed / любой будущий статус) →
//     [ErrNotRunnable].
//
// TerminalDestroy (teardown scenario `destroy`): стартует ТОЛЬКО из destroying
// (S-D1 уже перевёл туда в Destroy-транзакции) и статус НЕ меняет — incarnation
// остаётся в destroying на всё время teardown-а (concurrent run/upgrade видят
// destroying и отклоняются их собственными gate-ами; провал teardown → лишь
// destroy_failed). Любой иной статус → [ErrNotRunnable]: teardown запускается
// только по уже инициированному destroy, не из произвольного состояния.
//
// Все проверки под одним FOR UPDATE — авторитет gate-а в транзакции, не только
// в HTTP-handler-е (TOCTOU-safe).
func (r *Runner) lockRun(ctx context.Context, spec RunSpec) (*incarnation.Incarnation, error) {
	var inc *incarnation.Incarnation
	err := pgx.BeginFunc(ctx, r.deps.DB, func(tx pgx.Tx) error {
		got, serr := selectForUpdate(ctx, tx, spec.IncarnationName)
		if serr != nil {
			return serr
		}
		if spec.TerminalMode == TerminalDestroy {
			// Teardown стартует строго из destroying; статус НЕ трогаем
			// (остаётся destroying на всё время прогона).
			if got.Status != incarnation.StatusDestroying {
				return fmt.Errorf("%w: %s", ErrNotRunnable, got.Status)
			}
			inc = got
			return nil
		}
		if spec.FromLocked {
			// rerun-create: UnlockForRerun под FOR UPDATE уже перевёл
			// error_locked→applying минуя ready (race-free). Статус НЕ транзитим
			// повторно — обязаны УВИДЕТЬ applying, иначе старт отклоняется
			// (fail-closed): любой иной статус означает, что зарезервированная
			// строка ушла из-под нас (чужой перехват / неконсистентный вызов).
			if got.Status != incarnation.StatusApplying {
				return fmt.Errorf("%w: %s (rerun expected applying)", ErrNotRunnable, got.Status)
			}
			inc = got
			return nil
		}
		switch got.Status {
		case incarnation.StatusReady, incarnation.StatusDrift:
			// ready — нормальный старт; drift — информационный статус Scry
			// (ADR-031): remediation drift-а = обычный apply, который при
			// успехе вернёт incarnation в ready через commitSuccess. Тот же
			// applying-перевод и тот же gate, как из ready — drift НЕ блокирует.
		case incarnation.StatusApplying:
			return ErrAlreadyRunning
		case incarnation.StatusErrorLocked:
			return ErrLocked
		default:
			// destroying / migration_failed / любой будущий статус.
			return fmt.Errorf("%w: %s", ErrNotRunnable, got.Status)
		}
		if uerr := updateStatus(ctx, tx, spec.IncarnationName, incarnation.StatusApplying); uerr != nil {
			return uerr
		}
		inc = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inc, nil
}

// parseScenario читает scenario/<scenarioName>/main.yml из уже
// материализованного снапшота сервиса и парсит его нормативным
// config-парсером (diagnostics уровня error → ошибка загрузки).
func (r *Runner) parseScenario(art *artifact.ServiceArtifact, scenarioName string) (*config.ScenarioManifest, error) {
	return parseScenarioFromArtifact(r.deps.Loader, art, scenarioName)
}

// parseScenarioFromArtifact — package-level форма [Runner.parseScenario]:
// читает и парсит scenario/<scenarioName>/main.yml из снапшота сервиса.
// Вынесена из метода, чтобы переиспользоваться Acolyte-путём ([RenderForHost])
// без подъёма Runner-а. Поведение идентично — чистый read+parse без side-эффектов.
func parseScenarioFromArtifact(loader *artifact.ServiceLoader, art *artifact.ServiceArtifact, scenarioName string) (*config.ScenarioManifest, error) {
	rel := fmt.Sprintf(scenarioMainFile, scenarioName)
	data, err := loader.ReadFile(art, rel)
	if err != nil {
		return nil, fmt.Errorf("scenario: read %s: %w", rel, err)
	}
	scn, _, diags, err := config.LoadScenarioManifestFromBytes(rel, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("scenario: parse %s: %w", rel, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("scenario: %s невалиден: %s", rel, firstError(diags))
	}
	return scn, nil
}

// failureStatus возвращает терминальный статус провала прогона по режиму
// финала (S-D2b): обычный прогон → error_locked; teardown (TerminalDestroy) →
// destroy_failed. destroy_failed — НЕ error_locked: семантика снятия разная
// (из destroy_failed оператор повторяет destroy / анлочит в ready, S-D2a).
func failureStatus(mode TerminalMode) incarnation.Status {
	if mode == TerminalDestroy {
		return incarnation.StatusDestroyFailed
	}
	return incarnation.StatusErrorLocked
}

// lockIncarnation переводит incarnation в failStatus (error_locked для обычного
// прогона / destroy_failed для teardown) со status_details (state не меняется —
// храним last known-good). Ошибки самого write-а только логируются: incarnation
// останется в applying, что триаж заметит.
//
// Возвращает finalized — true ТОЛЬКО когда ЭТОТ инстанс реально записал терминал
// (UpdateStateFromRun прошёл). false при single-winner-проигрыше
// (ErrAlreadyFinalized: строку уже вывел из applying другой коммиттер — recovery-
// перехват / параллельный финал) ЛИБО при ошибке записи. Сигнал нужен abort()-у,
// чтобы провальное incarnation.run_completed эмитил ровно один инстанс-победитель
// (не задвоить событие при recovery-перехвате, симметрично success-ветке, где
// проигравший commit возвращается до emitRunCompleted).
func (r *Runner) lockIncarnation(ctx context.Context, spec RunSpec, stateBefore map[string]any, failStatus incarnation.Status, reason string, cause error, log *slog.Logger) (finalized bool) {
	// commit пишем под detached-ctx: исходный ctx мог быть отменён
	// (Cancel/Shutdown/timeout), но зафиксировать error_locked надо в любом
	// случае. WithoutCancel: сохраняем trace-baggage, не наследуем cancel
	// teardown-пути. 5s-cap — защита от зависшего PG на отмене.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	details := map[string]any{
		"reason":   reason,
		"apply_id": spec.ApplyID,
	}
	if cause != nil {
		details["error"] = cause.Error()
	}
	// status_details читается наружу через GET incarnation без маскинга, а
	// cause.Error() может транзитом нести зарезолвленный секрет / vault-ref из
	// Params (например в render/dispatch-ошибке). Маскируем перед записью —
	// observability-only, на wire (ApplyRequest.Params) это не влияет.
	details = audit.MaskSecrets(details)
	historyID := audit.NewULID()
	err := pgx.BeginFunc(wctx, r.deps.DB, func(tx pgx.Tx) error {
		return incarnation.UpdateStateFromRun(
			wctx, tx,
			spec.IncarnationName, spec.ScenarioName, spec.ApplyID,
			stateBefore, stateBefore, // state не меняем при фейле
			failStatus, details,
			startedByPtr(spec.StartedByAID),
			historyID,
		)
	})
	if err != nil {
		// Single-winner (ADR-027(j) W1): строку уже вывел из applying/destroying
		// другой коммиттер (recovery-перехват) — терминал зафиксирован им, не
		// нами. Это НЕ «осталась в applying» и НЕ ошибка записи: логируем как
		// no-op и не пишем destroy_failed audit (терминал уже не наш).
		if errors.Is(err, incarnation.ErrAlreadyFinalized) {
			log.Info("scenario: терминальный статус провала пропущен — incarnation уже финализирована другим коммиттером",
				slog.String("terminal_status", string(failStatus)))
			return false
		}
		log.Error("scenario: запись терминального статуса провала провалена — incarnation осталась в applying",
			slog.String("terminal_status", string(failStatus)),
			slog.Any("error", err))
		return false
	}

	// Терминал destroy-провала фиксируется явным audit-событием (S-D3):
	// destroy_failed теперь имеет своё имя в каталоге, не только status_details +
	// slog. Пишется ТОЛЬКО для teardown-режима (обычный прогон → error_locked,
	// своё событие incarnation.locked — отдельная подсистема). reason уже
	// замаскирован выше (details прошли MaskSecrets). Фейл audit не валит — статус
	// destroy_failed уже закоммичен, теряем только trail.
	if failStatus == incarnation.StatusDestroyFailed {
		r.writeDestroyFailedAudit(wctx, spec, reason, details, log)
	}
	return true
}

// ensureTerminalApplyRun гарантирует, что у прогона `spec.ApplyID` есть хотя бы
// одна ТЕРМИНАЛЬНАЯ строка apply_runs после abort (BAG-1). Два случая:
//
//   - строки УЖЕ есть (поздний abort: dispatch успел вставить planned/claimed/
//     dispatched) — терминалим КАЖДУЮ недотерминальную в `terminal` через
//     [applyrun.UpdateStatus] (он single-winner: уже-терминальные строки не
//     трогаются, ADR-027(j)). running-строки НЕ трогаем: apply уже ушёл на хост,
//     честный терминал придёт с него (RunResult). Не вставляем sentinel —
//     реальные хосты есть;
//   - строк НЕТ (ранний abort: no_hosts / scenario_load_failed / topology_failed
//     / essence_failed / input_invalid / render_failed / keeper_dispatch_failed —
//     всё ДО dispatch) — вставляем ОДНУ sentinel-строку [render.RunSentinelSID]
//     со status=`terminal` и error_summary=reason.
//
// `terminal` — терминальный статус провала (см. [failureTerminalStatus]):
// cancelled при операторском Cancel, иначе failed.
//
// Источник истины reason — abort-reason (no_hosts и пр.). Ошибки записи только
// логируются (warn): incarnation уже переведена в error_locked, потеря barrier-
// строки — деградация наблюдаемости, не повод валить goroutine. Detached-ctx —
// исходный ctx мог быть отменён (Shutdown/timeout), как в [lockIncarnation].

// failureTerminalStatus переводит причину abort-а в терминальный статус
// apply_runs прогона. Различие СТРОГО по [errCancelRequested] — sentinel-у
// оператор-инициированного cluster-wide Cancel (G1): только он даёт cancelled.
// context.Canceled/DeadlineExceeded (RunTimeout, Shutdown-abort) идут через
// ctx.Err() и НЕ матчатся — это честный provider-провал, остаётся failed.
func failureTerminalStatus(cause error) applyrun.Status {
	if errors.Is(cause, errCancelRequested) {
		return applyrun.StatusCancelled
	}
	return applyrun.StatusFailed
}

func (r *Runner) ensureTerminalApplyRun(ctx context.Context, spec RunSpec, reason string, terminal applyrun.Status, keepRunning bool, log *slog.Logger) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	statuses, err := applyrun.SelectStatusesByApplyID(wctx, r.deps.DB, spec.ApplyID)
	if err != nil {
		log.Warn("scenario: чтение apply_runs для терминализации прогона провалено",
			slog.String("reason", reason), slog.Any("error", err))
		return
	}

	summary := reason
	if len(statuses) == 0 {
		// Ранний abort: ни одного хоста, ни одной строки. Sentinel закрывает
		// прогон терминалом, чтобы Voyage-awaiter не ждал вечно. reason — текст
		// причины abort-а (не несёт секретов; render/dispatch-cause сюда не идёт).
		run := applyrun.ApplyRun{
			ApplyID:         spec.ApplyID,
			SID:             render.RunSentinelSID,
			IncarnationName: spec.IncarnationName,
			Scenario:        spec.ScenarioName,
			Status:          terminal,
			ErrorSummary:    &summary,
			StartedByAID:    startedByPtr(spec.StartedByAID),
		}
		if ierr := applyrun.Insert(wctx, r.deps.DB, &run); ierr != nil {
			log.Warn("scenario: вставка sentinel-строки apply_runs провалена — прогон без терминальной строки",
				slog.String("reason", reason), slog.Any("error", ierr))
		}
		return
	}

	// Поздний abort: терминалим недотерминальные строки реальных хостов в
	// terminal (cancelled при операторском Cancel, иначе failed).
	// running-строки различаем ПО ПРИЧИНЕ (keepRunning):
	//   - operator-Cancel (keepRunning): apply ушёл на ЖИВОЙ хост — НЕ трогаем,
	//     дойдёт честный RunResult (honest reporting).
	//   - timeout/dead-host/ctx.Err (!keepRunning): RunResult НЕ придёт, reaper
	//     running не подбирает — форс-фейлим (BAG-1 recovery).
	for _, st := range statuses {
		switch st.Status {
		case applyrun.StatusPlanned, applyrun.StatusClaimed, applyrun.StatusDispatched:
			// недотерминальные, apply ещё не на хосте — закрываем в terminal
		case applyrun.StatusRunning:
			if keepRunning {
				continue // operator-Cancel: дойдёт честный RunResult
			}
			// timeout/dead-host: RunResult не придёт — форс-фейлим
		default:
			continue // уже терминальна
		}
		if uerr := applyrun.UpdateStatus(wctx, r.deps.DB, spec.ApplyID, st.SID, terminal, &summary); uerr != nil {
			log.Warn("scenario: терминализация строки apply_runs провалена",
				slog.String("sid", st.SID), slog.String("reason", reason), slog.Any("error", uerr))
		}
	}
}

// writeDestroyFailedAudit пишет audit-event incarnation.destroy_failed после
// перевода incarnation в destroy_failed (провал teardown-а, S-D3). source=
// keeper_internal (write-path — scenario-runner, archon_aid колонка NULL),
// correlation_id = apply_id. reason берётся из уже маскированного status_details
// (cause мог транзитом нести vault-ref). w == nil → trail не пишется.
func (r *Runner) writeDestroyFailedAudit(ctx context.Context, spec RunSpec, reason string, details map[string]any, log *slog.Logger) {
	if r.deps.Audit == nil {
		return
	}
	payload := map[string]any{
		"name":     spec.IncarnationName,
		"apply_id": spec.ApplyID,
		"reason":   reason,
	}
	// Маскированный текст причины (если есть) — из status_details, не из
	// сырого cause: details уже прошли MaskSecrets выше.
	if errText, ok := details["error"].(string); ok && errText != "" {
		payload["error"] = errText
	}
	ev := &audit.Event{
		EventType:     audit.EventIncarnationDestroyFailed,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: spec.ApplyID,
		Payload:       payload,
	}
	if err := r.deps.Audit.Write(ctx, ev); err != nil && log != nil {
		log.Warn("scenario: запись audit incarnation.destroy_failed провалена",
			slog.String("incarnation", spec.IncarnationName), slog.Any("error", err))
	}
}

// destroyForce извлекает намерение force из status_details incarnation (S-D1
// положил туда `force` при переводе в destroying). Отсутствие ключа / не-bool →
// false (консервативный дефолт: трактуем как teardown-destroy). Значение идёт
// только в audit-payload destroy_completed — фактическое поведение teardown-а
// уже отработано выше.
func destroyForce(inc *incarnation.Incarnation) bool {
	if inc == nil || inc.StatusDetails == nil {
		return false
	}
	f, _ := inc.StatusDetails["force"].(bool)
	return f
}

// commitSuccess фиксирует успешный прогон: state_changes коммитятся в
// incarnation.state, статус → ready, snapshot в state_history. Одна
// PG-транзакция (FOR UPDATE внутри UpdateStateFromRun).
func (r *Runner) commitSuccess(ctx context.Context, spec RunSpec, stateBefore, stateAfter map[string]any) error {
	historyID := audit.NewULID()
	return pgx.BeginFunc(ctx, r.deps.DB, func(tx pgx.Tx) error {
		return incarnation.UpdateStateFromRun(
			ctx, tx,
			spec.IncarnationName, spec.ScenarioName, spec.ApplyID,
			stateBefore, stateAfter,
			incarnation.StatusReady, nil,
			startedByPtr(spec.StartedByAID),
			historyID,
		)
	})
}

// run_completed-статусы payload-а incarnation.run_completed (ADR-052 §k). Единое
// событие на любой терминал обычного прогона; исход выносится в payload.status
// (паттерн task.executed/run.completed — фильтрация по полю, не по разбегу
// event_type). error_locked сворачивается в "failed" (под-статусы не плодим).
const (
	runCompletedStatusSuccess = "success"
	runCompletedStatusFailed  = "failed"
)

// emitRunCompleted пишет audit-событие incarnation.run_completed на терминале
// обычного прогона (T3/T4-фундамент, ADR-052 §k): per-incarnation итог
// scenario-run с массивом changed_tasks и status ∈ {success, failed}.
// source=keeper_internal (write-path — scenario-runner, archon_aid колонка NULL),
// correlation_id = apply_id. Одно событие на инкарнацию-прогон, НЕ per-host.
//
// Вызывается из ДВУХ точек: success-ветка run() (после commitSuccess) с
// status=success И abort() (после терминализации провала, только когда наш
// lockIncarnation реально записал терминал) с status=failed. TerminalDestroy в
// обе точки не приходит — у destroy свой терминал (destroy_completed/_failed).
//
// changed_tasks собирается из АГРЕГАТА журнала аудита (task.executed+CHANGED,
// AuditReader) — read-only по адресным полям (sid, task_idx); метаданные задач —
// из in-memory tasks (секрет-гигиена). Свёртка по адресу register∪id (loop-
// итерации одного адреса — одна запись, union уникальных sid). На провале
// tasks/plans могут быть nil (ранний abort до render) → buildChangedTasks(nil,…)
// возвращает nil (тест TestBuildChangedTasks_EmptyInputs), либо частичными
// (поздний abort) → changed_tasks несёт то, что успело CHANGED до падения.
// Audit/AuditReader nil → деградация: при nil AuditReader событие пишется без
// changed_tasks (только факт терминала), при nil Audit — не пишется вовсе.
//
// cadence_id кладётся в payload ТОЛЬКО при spec.CadenceID != nil (дочерний Voyage
// расписания, T4b) — ручной прогон ключ не несёт (консервативно, как drift-
// payload), чтобы постоянное Tiding-правило с cadence-селектором ловило именно
// результаты прогонов расписания.
//
// voyage_id кладётся в payload ТОЛЬКО при spec.VoyageID != nil (прогон через
// Voyage, ADR-052 amend §k) — прямые пути (create/rerun/destroy) минуют Voyage
// и ключ не несут (симметрия с cadence_id). Нужен Voyage detail для visibility-
// фетча per-incarnation run-событий вояжа: событие per-incarnation несёт
// correlation_id=apply_id, а страница вояжа фильтрует по voyage_id в payload.
//
// Detached-ctx: исходный ctx мог быть на грани таймаута/отменён (timeout/Cancel/
// Shutdown на провале), но зафиксировать терминал прогона надо. Все ошибки только
// логируются (warn) — прогон уже терминал-ил, потеря события = деградация
// наблюдаемости.
func (r *Runner) emitRunCompleted(ctx context.Context, spec RunSpec, status string, tasks []*render.RenderedTask, plans []render.DispatchPlan, log *slog.Logger) {
	if r.deps.Audit == nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	var changed []ChangedTask
	if r.deps.AuditReader != nil {
		keys, err := r.deps.AuditReader.SelectChangedTaskKeys(wctx, spec.ApplyID)
		if err != nil {
			// Чтение агрегата провалено — пишем событие без changed_tasks, не теряя
			// сам факт терминала. Свёртка best-effort.
			log.Warn("scenario: чтение changed-агрегата для incarnation.run_completed провалено — событие без changed_tasks",
				slog.Any("error", err))
		} else {
			changed = buildChangedTasks(tasks, plans, keys)
		}
	}

	payload := map[string]any{
		"incarnation":   spec.IncarnationName,
		"scenario":      spec.ScenarioName,
		"apply_id":      spec.ApplyID,
		"status":        status,
		"changed_tasks": changedTasksPayload(changed),
	}
	if spec.CadenceID != nil {
		payload["cadence_id"] = *spec.CadenceID
	}
	if spec.VoyageID != nil {
		payload["voyage_id"] = *spec.VoyageID
	}

	ev := &audit.Event{
		EventType:     audit.EventIncarnationRunCompleted,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: spec.ApplyID,
		Payload:       payload,
	}
	if err := r.deps.Audit.Write(wctx, ev); err != nil {
		log.Warn("scenario: запись audit incarnation.run_completed провалена",
			slog.String("incarnation", spec.IncarnationName), slog.Any("error", err))
	}
}

// changedTasksPayload конвертирует []ChangedTask в JSON-payload-форму события
// (snake_case-ключи). Отдельная функция — чтобы wire-форма payload не
// размазалась по коду эмиссии. Несёт ТОЛЬКО метаданные + counts (секрет-гигиена
// T3): register/params-значений нет. Пустой/nil вход → пустой slice (не nil),
// чтобы JSONB-payload нёс `"changed_tasks": []`, а не отсутствие ключа.
func changedTasksPayload(changed []ChangedTask) []map[string]any {
	out := make([]map[string]any, 0, len(changed))
	for _, c := range changed {
		out = append(out, map[string]any{
			"idx":           c.Idx,
			"name":          c.Name,
			"register":      c.Register,
			"id":            c.ID,
			"module":        c.Module,
			"changed_hosts": c.ChangedHosts,
			"total_hosts":   c.TotalHosts,
		})
	}
	return out
}

// startedByPtr превращает StartedByAID в *string (пустая строка → nil, чтобы
// FK started_by_aid писался NULL для прогонов без identity Архонта).
func startedByPtr(aid string) *string {
	if aid == "" {
		return nil
	}
	return &aid
}

// maskErrText возвращает текст ошибки, прогнанный через audit.MaskSecrets:
// render/dispatch-ошибка может нести vault:secret/-ref (наводку на секрет-
// локацию). Тот же фильтр, что для status_details, применяется и к slog-каналу
// (лог-файл — наблюдаемый канал). nil → пустая строка.
func maskErrText(err error) string {
	if err == nil {
		return ""
	}
	masked := audit.MaskSecrets(map[string]any{"error": err.Error()})
	if s, ok := masked["error"].(string); ok {
		return s
	}
	return err.Error()
}

// firstError возвращает сообщение первой error-диагностики (для краткого
// отчёта об ошибке парсинга scenario).
func firstError(diags []diag.Diagnostic) string {
	for i := range diags {
		if diags[i].Level == diag.LevelError {
			return diags[i].Message
		}
	}
	return "unknown validation error"
}
