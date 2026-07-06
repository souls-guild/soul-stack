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
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
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

	// seal / sealed-paths ([ADR-010] §7.4): аккумулятор путей ячеек params, чьё
	// CEL-выражение читало secret-источник (secret-input/vault()/транзит). Render
	// (шаг 5) наполняет его per-task; abort/lockIncarnation используют sealed.Paths()
	// для seal-aware маскинга наблюдаемых каналов (audit.MaskSecretsSealed) поверх
	// vault+regex. Указатель шарится между passages staged-render-а (пути
	// накапливаются по всем Passage прогона). Объявлен ДО abort — abort может
	// сработать до Render (sealed пуст → деградация к vault+regex, БИТ-В-БИТ).
	sealed := render.NewSealedSet()

	// С этого момента провал прогона = failStatus (state не меняем).
	// result остаётся runResultFailed на любом abort-выходе ниже; ok
	// выставляется только при успешном финале.
	abort := func(reason string, cause error) {
		// cause может нести vault:secret/-ref из render/dispatch-ошибки. Тот же
		// маскинг, что и для status_details в lockIncarnation, применяем к обоим
		// наблюдаемым каналам: OTel-трейс (span exception) и slog (лог-файл) —
		// нельзя оставлять асимметрию с маскированным DB-status_details.
		maskedCause := errors.New(maskErrText(cause, sealed.Paths()))
		span.RecordError(maskedCause)
		log.Error("scenario: прогон провален — incarnation заблокирована",
			slog.String("reason", reason),
			slog.String("terminal_status", string(failStatus)),
			slog.String("error", maskErrText(cause, sealed.Paths())))
		finalized := r.lockIncarnation(ctx, spec, stateBefore, failStatus, reason, cause, sealed.Paths(), log)
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
	scn, err := r.parseScenario(art, spec.ScenarioName, spec.FromUpgrade)
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

	// Синтез install-шагов core.module.installed из service.yml::modules[]
	// (ADR-065): ПОСЛЕ ExpandIncludes (потребители в ветках видны), ДО Stratify
	// (синтез-шаг — roster-задача, стратифицируется как её потребитель).
	if synthed, names := config.SynthesizeModuleInstalls(scn.Tasks, art.Manifest.Modules); len(names) > 0 {
		scn.Tasks = synthed
		log.Info("scenario: синтезированы install-шаги модулей из manifest.modules[] (ADR-065)",
			slog.Any("modules", names))
	}

	// 2.5. Provision-aware effective run-timeout (ADR-0061). Deadline переехал сюда из
	//      Start (runner.go) — план распарсен, видно, provision-ли это. Обычный прогон
	//      держит defaultRunTimeout (5m) — защита от вечного barrier ЦЕЛА. Прогон с
	//      refresh-эмиттером (provision-from-zero: cloud-create + онбординг `await_online`
	//      до ceiling-а + деплой роли) легитимно длится дольше: base = ceiling барьера
	//      онбординга (ResolvedMaxAwaitTimeout) + deployBudget. БЕЗ этого defaultRunTimeout
	//      обрывал бы provision-прогон на 5-й минуте — раньше joinWait (6m) и await_timeout
	//      (до 30m). WithTimeout навешивается ПОВЕРХ runCtx (WithCancel из Start): под-
	//      контекст наследует отмену из active-map, поэтому Cancel/Shutdown по-прежнему
	//      рвут прогон. defer cancel — на любом return run() (включая ранний abort ниже).
	ctx, cancel := context.WithTimeout(ctx, r.effectiveRunTimeout(scn.Tasks))
	defer cancel()

	// 3. Резолв хостов прогона (roster по Coven-метке incarnation). Вынесен в
	//    resolveRoster — он вызывается ПОВТОРНО в stage-loop на refresh-границах
	//    (mid-run re-resolve, ADR-0061 §S3): после успешного `refresh_soulprint:
	//    true`-шага созданные+онбордившиеся хосты входят в roster следующего Passage.
	hosts, err := r.resolveRoster(ctx, spec.IncarnationName)
	if err != nil {
		abort("topology_failed", err)
		return
	}
	// no_hosts-гейт — fail-closed по умолчанию (S1-ограничение, ADR-0061): прогон
	// требует connected-хостов, пустой roster → error_locked. ДВА КЛАССА bypass для
	// provision-from-zero (ADR-0061 amendments):
	//
	//   (а) all-keeper (allKeeperTasks): ВСЕ задачи `on: keeper` — keeper-only
	//       сценарий (core.cloud.created создаёт VM С НУЛЯ), хостов на старте нет
	//       по определению.
	//   (б) mixed с refresh-эмиттером (HasRefreshEmitter): план несёт refresh-
	//       эмиттер (core.soul.registered с refresh_soulprint: true) → roster пере-
	//       резолвится mid-run (ADR-0061 §S2/§S3). Пустой стартовый roster законен:
	//       host-задачи деплоя стратифицируются в Passage ПОСЛЕ refresh-границы и
	//       видят уже пере-резолвленный live-снимок (онбордившиеся VM), а не пустой
	//       P0. Это и есть staged provision→роль одним прогоном.
	//
	// chicken-egg обоих классов: «run требует хостов, run их и создаёт». БЕЗ обоих
	// признаков — пустой roster есть no_hosts БИТ-В-БИТ (нерасширенное поведение).
	// Считаем по scn.Tasks ПОСЛЕ ExpandIncludes — плоский top-level список, тот же,
	// что видит Render и Stratify.
	provisionsRoster := config.HasRefreshEmitter(scn.Tasks)
	if len(hosts) == 0 && !allKeeperTasks(scn.Tasks) && !provisionsRoster {
		abort("no_hosts", fmt.Errorf("incarnation %q не имеет connected-хостов", spec.IncarnationName))
		return
	}

	// 4. Essence (effective-слой). render.Pipeline пробрасывает её в CEL как
	//    `essence.<path>` (slice E2). Берём OS-family первого хоста как
	//    представителя (per-host essence — расширение).
	//
	//    keeper-контекст (пустой roster, provision-from-zero): host-представителя
	//    нет, hosts[0] паникнул бы. Резолвим essence БЕЗ per-host overlay-я —
	//    default-слой + Coven-overlay инкарнации (корневая Coven-метка = inc.Name,
	//    ADR-008) + spec.essence-override. OS-family overlay пропускается (OSFamily
	//    пуст) — симметрично renderKeeperTask, который рендерит keeper-задачи без
	//    per-host soulprint. После онбординга созданных VM последующие Passage
	//    получают per-host essence обычным путём (mid-run re-resolve, ADR-0061 §S3).
	essenceIn := keeperEssenceInput(art.LocalDir, inc)
	if len(hosts) > 0 {
		essenceIn = essenceInput(art.LocalDir, inc, hosts[0])
	}
	essenceMap, err := r.deps.Essence.Resolve(essenceIn)
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
		// State — read-only снимок incarnation.state на момент row-lock прогона
		// (stateBefore захвачен под FOR UPDATE). Доступен в scenario-render CEL как
		// `incarnation.state.<path>` (ADR-009/010, Вариант A). ОДНА точка: renderIn
		// переиспользуется на всех passages staged-render-а, поэтому снимок
		// инвариантен (P0 ≡ P1+ = pre-run state, НЕ накапливается между passages).
		State: stateBefore,
		Ctx:   ctx, // vault() (RenderStateChanges не идёт через Render → ctx нужен явно)
		// Templates: ридер .tmpl снапшота сервиса для core.file.rendered.
		// Двухуровневый резолв scenario-local→service-level (ADR-009): чтение
		// через artifact.ReadSnapshotFile поверх art.LocalDir (securejoin-защита).
		Templates: render.NewSnapshotTemplateReader(
			func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(art.LocalDir, rel) },
			scenarioTemplatePrefix(spec.ScenarioName),
		),
		// seal (ADR-010 §7.4): Render наполняет sealed путями ячеек params, чьё
		// выражение читало secret-источник. Используется abort/lockIncarnation для
		// seal-aware маскинга. Указатель шарится между passages (накопление).
		Sealed: sealed,
	}
	if r.deps.Destiny != nil {
		renderIn.Destiny = r.deps.Destiny.resolverFor(art.Manifest)
	}

	// 4.9. Стратификация прогона по register-зависимости (staged-render, ADR-056
	//      §б) — ДО первого Render: задача, читающая register.X в where:/apply:
	//      input:/params:/vars:, попадает в Passage строго ПОСЛЕ probe-шага,
	//      эмитящего register: X. Stratify работает над тем же плоским top-level
	//      списком scn.Tasks (после ExpandIncludes), что и Render. N=1 (нет register-
	//      зависимостей) → Passage{Count:1, все passage 0} → поведение БИТ-В-БИТ.
	//      Цикл / висячая register-ссылка → errStratify → render_failed (явный
	//      отказ, не silent-wrong-target). Делаем ДО Render, потому что для staged
	//      первый Render обязан знать TaskPassage/ActivePassage=0 — иначе он
	//      эагерно отрендерил бы register-зависимый where будущего Passage по
	//      пустому register и упал (это исходный drift).
	passage, perr := render.Stratify(scn.Tasks)
	if perr != nil {
		abort("render_failed", fmt.Errorf("scenario: стратификация passage %s/%s: %w", spec.IncarnationName, spec.ScenarioName, perr))
		return
	}
	staged := passage.Count > 1

	// 4.91. Roster-refresh границы (ADR-0061 §S2/§S3). refreshBoundaries[P] — нужен
	//       ли re-resolve roster ПЕРЕД render-ом Passage P (в Passage P-1 завершился
	//       успешный `refresh_soulprint: true`-шаг → онбордившиеся хосты вошли в
	//       souls+coven → live-снимок roster изменился). out[0]=false (up-front roster).
	//       Без refresh-эмиттера все false → re-resolve не выполняется (БИТ-В-БИТ).
	//       RefreshBoundaries — чистая функция над тем же scn.Tasks и passage:
	//       границы стоят перед каждым Passage, следующим за Passage с refresh-эмиттером.
	refreshBoundaries := config.RefreshBoundaries(scn.Tasks, passage)

	// 4.92. Within-block register-зависимость — KEEPER-SIDE FAIL-CLOSED страховка
	//       (ADR-056, §«Риски — silent-wrong-target»). Потомок block:, читающий
	//       register СОСЕДНЕГО потомка ТОГО ЖЕ блока, не ловится Stratify (внутри-
	//       блочное ребро не пересекает границу top-level задач — block атомарен по
	//       Passage). peer-register становится доступен Soul-side только ПОСЛЕ probe,
	//       но where/when/params потребителя резолвятся Keeper-side ДО dispatch → where
	//       отберёт хосты по устаревшему/внешнему register МОЛЧА. soul-lint обязан
	//       поймать это офлайн; здесь — рантайм-страховка (отказ, не silent-wrong-target).
	if info, bad := config.WithinBlockRegisterDependency(scn.Tasks); bad {
		abort(config.CodeWithinBlockRegisterDependency, fmt.Errorf(
			"scenario %s/%s: задача %q внутри block: читает register %q, эмитнутый соседней %q ТОГО ЖЕ блока — невозможно на render (block атомарен, peer-register доступен только Soul-side ПОСЛЕ probe, а where/when/params резолвятся Keeper-side ДО dispatch); вынесите probe на top-level (разные Passage)",
			spec.IncarnationName, spec.ScenarioName, info.ReaderName, info.RegisterName, info.EmitterName))
		return
	}

	// 4.925. Cross-passage when-gating — KEEPER-SIDE FAIL-CLOSED страховка (ADR-056:85
	//        amend, FC-5). Задача гейтит `when:`/`changed_when:`/`failed_when:` по
	//        register, эмитнутому в БОЛЕЕ РАННЕМ Passage. flow-control = Soul-side
	//        per-task gating (ADR-012(d)), видит только register СВОЕГО Passage;
	//        cross-passage register ему недоступен (другой ApplyRequest) → `no such
	//        key` молча → задача FAILED. После narrow-fix flow-control сам Passage НЕ
	//        расщепляет, но probe мог уехать в ранний Passage по ДРУГОЙ причине (иная
	//        задача с `where: register.X`). where: это умеет (Keeper пере-рендерит с
	//        накопленным register), when: — нет. soul-lint ловит офлайн; здесь —
	//        рантайм-страховка (отказ, не молчаливый no-such-key-фейл). Гейт строго
	//        staged (N=1 → один Passage, cross-passage невозможен).
	if info, bad := config.CrossPassageWhenGating(scn.Tasks, passage); bad {
		abort(config.CodeCrossPassageWhenGating, fmt.Errorf(
			"scenario %s/%s: задача %q гейтит %s: по register %q из другого Passage (consumer passage %d, источник passage %d) — Soul-side gating видит только свой Passage, cross-passage register недоступен → no such key; используйте where: для cross-task register-таргетинга или register.self для same-task gating (ADR-056:85)",
			spec.IncarnationName, spec.ScenarioName, info.ConsumerName, info.Kind, info.RegisterName, info.ConsumerPassage, info.SourcePassage))
		return
	}

	// 4.95. serial + staged (N>1) — 2D serial×passage РЕАЛИЗОВАН (ADR-056 §S4 amend,
	//       S-2D1). Рестрикт `serial_staged_unsupported` СНЯТ. Оси serial (волны
	//       ХОСТОВ) и Passage (стратификация ЗАДАЧ) ортогональны и теперь крутятся
	//       совместно: Passage-цикл ниже исполняет каждый Passage по порядку, а
	//       dispatchPassage внутри бьёт хосты на serial-волны из задач ИМЕННО ЭТОГО
	//       Passage (effectiveSerialWidth на tasksForPassage-срезе → per-Passage
	//       width, НЕ per-RUN). Probe-Passage без serial едет одной волной, даже когда
	//       последующий Passage несёт serial:1 (никакого silent-wrong-width). serial+
	//       staged идёт тем же inline-путём, что serial БЕЗ staged, поэтому наследует
	//       crash-recovery-лимит staged-inline (ADR-056 §S4: Acolyte-reclaim не
	//       покрывает staged-inline) — это не новая регрессия.

	// 4.955. Cross-passage requisite — KEEPER-SIDE GATING (ADR-056 R3). onchanges/
	//        onfail-источник в БОЛЕЕ РАННЕМ Passage, чем потребитель, едет отдельным
	//        ApplyRequest → Soul gating одного Passage не видит результат источника
	//        другого Passage (R1-remap чинит ТОЛЬКО same-passage). Поэтому связь
	//        резолвит Keeper per-host по накопленным CHANGED/FAILED-фактам предыдущих
	//        Passage (crosspassage.go): cross-passage onchanges OR по CHANGED, onfail
	//        зеркально по FAILED∪TIMED_OUT. R2-reject СНЯТ — cross-passage поддержан.
	//
	//        Источник CHANGED/FAILED-фактов — журнал аудита (AuditReader). Без него
	//        keeper не может определить, спас ли cross-passage источник → fail-closed
	//        reject (симметрия nil passageCap §S5): угадывать «не changed» = молчаливо
	//        не выполнить реально-нужный consumer / не запустить rescue. Гейт строго
	//        staged (N=1 → один Passage, cross-passage невозможен). Детектор
	//        CrossPassageRequisite переиспользуется для проверки наличия cross-passage
	//        связи (есть → нужен reader).
	if staged && r.deps.AuditReader == nil {
		if info, bad := config.CrossPassageRequisite(scn.Tasks, passage); bad {
			abort("cross_passage_requisite_unsupported", fmt.Errorf(
				"scenario %s/%s: задача %q ссылается через %s: на register %q, чей источник в другом Passage (consumer passage %d, источник passage %d) — cross-passage gating требует журнала аудита (AuditReader), но он недоступен → отказ fail-closed (ADR-056 R3)",
				spec.IncarnationName, spec.ScenarioName, info.ConsumerName, info.Kind, info.RequisiteName, info.ConsumerPassage, info.SourcePassage))
			return
		}
	}

	// 4.96. Forward-compat staged-гейт (ADR-056 §S5). Staged-прогон шлёт N
	//       ApplyRequest на хост (по Passage); barrier каждого Passage ждёт его
	//       терминал — RunResult с echo passage. Soul, не умеющий эхать passage
	//       (старый бинарь без passage-capability), под N>1 вернёт RunResult с
	//       passage=0 на все Passage → barrier Passage 1+ ждал бы терминал, которого
	//       нет → ЗАВИСАНИЕ в applying. Поэтому ДО dispatch проверяем, что КАЖДЫЙ
	//       online-хост прогона анонсировал passage-capability. Любой неподдержи-
	//       вающий → fail-closed abort `soul_passage_unsupported` (не hang, не
	//       молчаливое одно-проходное исполнение, которое вернуло бы исходный drift).
	//       N=1-прогон (staged==false) гейт НЕ проходит — он шлёт один passage=0,
	//       совместим со старым Soul БИТ-В-БИТ. Чекер nil (нет Redis / unit) →
	//       отвергаем staged целиком: без presence-источника нельзя подтвердить
	//       поддержку, а слать N>1 вслепую — тот же риск зависания.
	if staged {
		sids := make([]string, len(hosts))
		for i, h := range hosts {
			sids[i] = h.SID
		}
		if r.passageCap == nil {
			abort("soul_passage_unsupported", fmt.Errorf(
				"scenario %s/%s: staged-прогон (%d Passage) требует подтверждения passage-capability хостов, но presence-чекер недоступен (нет Redis) — отказ fail-closed (ADR-056 §S5)",
				spec.IncarnationName, spec.ScenarioName, passage.Count))
			return
		}
		lacking, lerr := r.passageCap.SoulsLackingPassage(ctx, sids)
		if lerr != nil {
			abort("soul_passage_unsupported", fmt.Errorf(
				"scenario %s/%s: проверка passage-capability хостов staged-прогона провалилась — отказ fail-closed (ADR-056 §S5): %w",
				spec.IncarnationName, spec.ScenarioName, lerr))
			return
		}
		if len(lacking) > 0 {
			abort("soul_passage_unsupported", fmt.Errorf(
				"scenario %s/%s: staged-прогон (%d Passage по register-зависимости) требует Passage-aware Soul, но хосты %v не поддерживают поле passage — обнови soul-бинарь либо убери register-зависимость (ADR-056 §S5)",
				spec.IncarnationName, spec.ScenarioName, passage.Count, lacking))
			return
		}
	}

	if staged {
		// Первый Render staged-прогона рендерит Passage 0 полноценно, будущие
		// Passage — placeholder-ами (ActivePassage=0, register ещё пуст).
		renderIn.TaskPassage = passage.TaskPassage
		renderIn.ActivePassage = 0
	}

	tasks, plans, err = r.deps.Render.Render(ctx, renderIn)
	if err != nil {
		abort("render_failed", err)
		return
	}

	// NIM-37: персист host-инвариантного плана задач прогона (name/module/no_log/
	// passage per plan_index) для read-эндпоинта /tasks. `tasks` тут — полный план
	// прогона (staged: Passage 0 + placeholder-ы будущих Passage; метаданные плана
	// host-инвариантны и стабильны между Passage). Best-effort — не валит прогон.
	r.persistRunPlan(ctx, spec, tasks, log)

	// 6. Dispatch: ветвление пути (ADR-027, Phase 1.4.2) × staged-render (ADR-056).
	//
	// Keeper-side задачи (`on: keeper`, docs/keeper/modules.md) исполняются ЛОКАЛЬНО
	// на этом инстансе через keeper-side core-Registry — СТРОГО ДО host-dispatch-а
	// СВОЕГО Passage (keeper-шаги идут первыми: provision/coven-bind → apply на
	// хостах). Они пишут свою apply_runs-строку (sid=render.KeeperTargetSID, passage)
	// + register, которые barrier и loadRegisterByHost видят наравне с host-строками.
	// Первая упавшая keeper-задача → abort (host-dispatch этого Passage не стартует).
	// dispatchKeeperTasks теперь зовётся PER-Passage (Слайс 2): для staged-пути —
	// внутри stage-loop на ПЕРЕ-рендеренных при ActivePassage=p tasks (keeper-задача
	// Passage>0 на render шага 5 — placeholder без Params, диспатчить её можно только
	// после её Passage стал активным); для Acolyte-пути — единственный Passage 0
	// (Acolyte исключает staged).
	//
	//   - staged (Passage.Count>1): СТАРЫЙ путь (inline) — stage-loop ниже. Acolyte
	//     рендерит per-host при claim ОДИН раз (не per-Passage) — staged на Acolyte
	//     отложен в S4 (ADR-056 §S4). Поэтому при Count>1 идём inline даже при
	//     AcolyteEnabled.
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
	// коммитится строго ПОСЛЕ барьера ПОСЛЕДНЕГО Passage — единый commit, не
	// по-волново и не по-Passage (§7 / ADR-056 §г).
	if r.acolyteEnabled && !hasSerialTask(scn) && !staged {
		// Acolyte-путь — non-staged (Acolyte исключает staged): keeper-задачи все в
		// Passage 0, render шага 5 (ActivePassage=0) уже отрендерил их полноценно.
		// Исполняем их ДО planned-fan-out-а host-задач — keeper-fail → abort, planned
		// не пишется. КeeperRegister здесь пуст (P0, цепочки нет): host-fallback цел.
		if err := r.dispatchKeeperTasks(ctx, spec, log, 0, tasks, plans); err != nil {
			abort("keeper_dispatch_failed", err)
			return
		}
		if err := r.dispatchPlanned(ctx, spec, log, hosts, tasks); err != nil {
			abort("dispatch_failed", err)
			return
		}
	} else {
		// Passage-loop (ADR-056 §в): для каждого Passage P по порядку —
		//   render(задачи P, RegisterByHost = накоплено из Passage < P) →
		//   dispatch(задачи P) → barrier(P) → сбор register(P).
		// На P=0 переиспользуем render шага 5 (для staged он уже с ActivePassage=0;
		// для Count==1 — обычный render, БИТ-В-БИТ). P>0 (только staged): повторный
		// Render с per-host register Passage < P. state-commit — ОДИН раз после
		// последнего Passage (шаги 7-8), НЕ по-Passage (ADR-056 §г).
		passageTasks, passagePlans := tasks, plans
		for p := 0; p < passage.Count; p++ {
			if p > 0 {
				// Mid-run re-resolve roster (ADR-0061 §S3) на refresh-границе: в
				// Passage P-1 завершился успешный `refresh_soulprint: true`-шаг → его
				// барьер сошёлся (созданные+онбордившиеся хосты записаны в souls+coven)
				// → пере-резолвим roster ПЕРЕД render-ом Passage P. Семантика re-resolve —
				// СВЕЖИЙ LIVE-СНИМОК roster incarnation на refresh-границе (resolveRoster →
				// LoadIncarnationHosts → filterAlive): отражает ТЕКУЩИЙ online-набор. Он
				// растёт по мере онбординга провиженных хостов (созданные VM поднялись →
				// видны), но это НЕ монотонная операция: хост, ушедший offline к границе
				// (упал lease / status≠connected), из live-снимка ИСКЛЮЧАЕТСЯ — таргетинг
				// идёт на реально-online набор (на offline-хост роль катить не надо).
				// Обновлённый renderIn.Hosts прокидывается в повторный Render → soulprint.hosts
				// и on:[incarnation.name]-таргетинг Passage P видят актуальный набор
				// (resolveTargets/soulprint.hosts строятся из in.Hosts). Roster СТАБИЛЕН в
				// пределах Passage: re-resolve только на границах, per-Passage детерминизм
				// (волны/run_once/assert неизменны внутри Passage). Re-resolve fail → abort
				// (не молча на старом roster).
				if refreshBoundaries[p] {
					grown, rerr := r.resolveRoster(ctx, spec.IncarnationName)
					if rerr != nil {
						abort("topology_failed", fmt.Errorf("scenario: re-resolve roster перед Passage %d: %w", p, rerr))
						return
					}
					prevSize := len(renderIn.Hosts)
					renderIn.Hosts = grown
					log.Info("scenario: roster пере-резолвлен на refresh-границе — live-снимок (ADR-0061 §S3)",
						slog.Int("passage", p), slog.Int("roster_size", len(grown)), slog.Int("prev_roster_size", prevSize))
				}

				// Staged-прогон, P>0: повторный Render с накопленным per-host register
				// Passage < P. Задачи будущих Passage (> P) эмитятся placeholder-ами
				// (register не готов, ADR-056 §в.1); активный Passage P и предыдущие —
				// резолвятся полноценно (where: register.* теперь видит реальный факт).
				// loadRegisterByHostUpToPassage резолвит task_idx→register-name по УЖЕ
				// имеющимся tasks (Index стабилен между Passage — тот же план).
				reg, lerr := r.loadRegisterByHostUpToPassage(ctx, spec.ApplyID, p, tasks)
				if lerr != nil {
					abort("register_load_failed", lerr)
					return
				}
				renderIn.RegisterByHost = reg
				// keeper→keeper register-chaining (staged-render): keeper-задачи копят
				// register под синтетическим хостом KeeperTargetSID, а keeperVars
				// (render/dispatch.go) читает его из ИЗОЛИРОВАННОГО канала
				// renderIn.KeeperRegister (per-host карта keeper-контексту недоступна — у
				// keeper-задачи нет хоста). Переливаем keeper-bucket предыдущих Passage из
				// RegisterByHost в KeeperRegister, чтобы keeper-задача активного Passage
				// видела `register.<prev>.*` keeper-задач прошлых Passage (например
				// core.bootstrap.delivered читает register от core.cloud.created). Канал
				// ОТДЕЛЁН от плоской renderIn.Register намеренно (host-fallback guard,
				// hostRegister остаётся на Register): host-задача смешанного Passage с пустым
				// per-host bucket НЕ прочитает keeper-register. Потребляется per-passage
				// keeper-dispatch (этот же Passage, ниже). nil-bucket → KeeperRegister
				// сбрасываем (host-only Passage: keeper-контекст пуст, БИТ-В-БИТ).
				renderIn.KeeperRegister = keeperRegisterBucket(reg)
				renderIn.TaskPassage = passage.TaskPassage
				renderIn.ActivePassage = p
				pt, pp, rerr := r.deps.Render.Render(ctx, renderIn)
				if rerr != nil {
					abort("render_failed", rerr)
					return
				}
				passageTasks, passagePlans = pt, pp
				// Держим резолвнутые tasks/plans для register-резолва следующего Passage
				// и финальной свёртки changed_tasks (Index стабилен между Passage).
				tasks, plans = pt, pp
			}

			// Keeper-side задачи ЭТОГО Passage (Слайс 2): исполняются на ПЕРЕ-
			// рендеренных при ActivePassage=p tasks (passageTasks), СТРОГО ДО host-
			// dispatch-а этого Passage. На render шага 5 / p>0-re-render keeper-задача
			// Passage p несёт полноценные Params (placeholder-gate pipeline.go её на
			// СВОЁМ активном Passage НЕ глушит) — keeperTasksOf отфильтрует ровно
			// keeper-задачи passage p. keeper→keeper register-chaining: keeper-задача
			// Passage p видит register keeper-задач Passage<p через renderIn.KeeperRegister
			// (перелив выше). keeper-FAIL → abort (return) ДО dispatchPassage p: host-
			// dispatch этого Passage не стартует. Ordering ↔ refresh-границы: keeper-
			// dispatch p отрабатывает ДО начала итерации p+1, где re-resolve читает его
			// эффект (core.soul.registered{refresh_soulprint} пишет souls+coven). Passage
			// без keeper-задач → no-op (host-only Passage). N=1 → один вызов passage 0,
			// поведение как pre-loop вызов до Слайса 2 (БИТ-В-БИТ).
			if err := r.dispatchKeeperTasks(ctx, spec, log, p, passageTasks, passagePlans); err != nil {
				abort("keeper_dispatch_failed", err)
				return
			}

			pTasks, pPlans := tasksForPassage(passageTasks, passagePlans, p)
			// Cross-passage requisite-gate Passage p (ADR-056 R3): для p>0 загружаем
			// CHANGED/FAILED-факты Passage < p (из журнала аудита) и резолвим per-host
			// onchanges/onfail-связи, чей источник в более раннем Passage. p==0 →
			// gate=nil (нет более раннего Passage). Полный план (tasks) — для
			// passageByIndex источников, которых нет в Passage-p-срезе.
			var gate *crossPassageGate
			if p > 0 && r.deps.AuditReader != nil {
				changed, ferr := r.deps.AuditReader.SelectChangedTaskKeys(ctx, spec.ApplyID)
				if ferr != nil {
					abort("register_load_failed", fmt.Errorf("scenario: cross-passage changed-факты: %w", ferr))
					return
				}
				failed, ferr := r.deps.AuditReader.SelectFailedTaskKeys(ctx, spec.ApplyID)
				if ferr != nil {
					abort("register_load_failed", fmt.Errorf("scenario: cross-passage failed-факты: %w", ferr))
					return
				}
				gate = newCrossPassageGate(tasks, changed, failed)
			}
			if err := r.dispatchPassage(ctx, spec, log, p, pTasks, pPlans, gate); err != nil {
				abort("dispatch_failed", err)
				return
			}
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

	// 8. Все задачи success на всех хостах → рендер state_changes
	//    (Keeper-side CEL, last-wins cross-host) и commit в incarnation.state.
	//    Рендер — строго ПОСЛЕ барьера (orchestration.md §7): значения
	//    фиксируются по факту успешного apply, не до него. RenderStateOps —
	//    упорядоченный список set+add-операций (новая list-форма); merge применяет
	//    их к stateBefore по порядку (set-overwrite / идемпотентный add).
	renderedOps, err := r.deps.Render.RenderStateOps(renderIn)
	if err != nil {
		abort("state_changes_render_failed", err)
		return
	}
	stateAfter, err := mergeStateChanges(stateBefore, renderedOps, art.Manifest.StateSchema, r.deps.Render.EvalStateMatch, r.deps.Render.EvalStateOpExpr)
	if err != nil {
		// Фейл применения операции (on_conflict: error / неконсистентная коллекция /
		// match-предикат упал) — error_locked, state НЕ коммитнут (stateAfter не
		// дошёл до commitSuccess). orchestration.md §7: фейл свёртки = блокировка.
		abort("state_changes_apply_failed", err)
		return
	}
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

// resolveRoster резолвит roster хостов прогона по корневой Coven-метке
// incarnation (online-souls + soulprint + declared/Choir-роль). Вынесено из run()
// шага 3, потому что вызывается ПОВТОРНО в stage-loop на refresh-границах
// (mid-run re-resolve, ADR-0061 §S3): один и тот же путь резолва даёт up-front
// roster и пере-резолвленный roster между Passage. Источник истины presence —
// Redis SID-lease (фаза 2 Resolver), та же, что у барьера онбординга `await_online`
// — поэтому re-resolve видит ровно тех, кого барьер refresh-шага дождался online.
func (r *Runner) resolveRoster(ctx context.Context, incarnationName string) ([]*topology.HostFacts, error) {
	return r.deps.Topology.LoadIncarnationHosts(ctx, incarnationName)
}

// allKeeperTasks сообщает, состоит ли сценарий ЦЕЛИКОМ из keeper-side задач
// (каждая `on: keeper`, render.IsKeeperTask). Это ПЕРВЫЙ класс bypass-а no_hosts-
// гейта для provision-from-zero (ADR-0061 amendment): all-keeper create-сценарий
// (core.cloud.created создаёт VM С НУЛЯ) законно стартует на пустом roster.
//
// ALL, не ANY: смешанный keeper+host прогон ЭТОТ предикат НЕ проходит. Но bypass
// смешанного provision→роль обеспечивает ВТОРОЙ класс — config.HasRefreshEmitter
// (план с refresh-эмиттером, см. no_hosts-гейт выше): host-задача стратифицируется
// в Passage после refresh-границы и видит пере-резолвленный roster, а не пустой P0.
// Смешанный план БЕЗ refresh-эмиттера по-прежнему держит no_hosts (host-задача на
// пустом P0 корректна как no_hosts). Пустой сценарий (len==0) → false: «нет задач»
// не повод bypass-ить гейт. Считаем по tasks ПОСЛЕ ExpandIncludes (плоский top-
// level список, тот же, что видит Render).
func allKeeperTasks(tasks []config.Task) bool {
	if len(tasks) == 0 {
		return false
	}
	for _, t := range tasks {
		if !render.IsKeeperTask(t) {
			return false
		}
	}
	return true
}

// effectiveRunTimeout возвращает потолок длительности прогона по плану задач
// (ADR-0061, provision-aware). База — runTimeout (Deps.RunTimeout / defaultRunTimeout,
// 5m). Если план несёт refresh-эмиттер ([config.HasRefreshEmitter] — provision-from-
// zero: create VM + онбординг `await_online` + деплой роли), потолок поднимается до
// ceiling-а барьера онбординга (ResolvedMaxAwaitTimeout) + deployBudget, но только
// если он БОЛЬШЕ базы (max, не replace): оператор, поднявший RunTimeout выше eff для
// своих нужд, не урезается. Без refresh-эмиттера — ровно база (вечный barrier
// обрывается как раньше).
//
// ceiling берётся через hot-reload-aware maxAwaitTimeoutFn (тот же snapshot keeper.yml::
// max_await_timeout, что видит барьер онбординга в coremod) — eff согласован с реальным
// ceiling-ом `await_online`. nil-fn (unit/L0 без config.Store) → [config.DefaultMaxAwaitTimeout]
// (30m): provision-прогон всё равно получает расширенный потолок, просто без override-а.
func (r *Runner) effectiveRunTimeout(tasks []config.Task) time.Duration {
	base := r.runTimeout
	if !config.HasRefreshEmitter(tasks) {
		return base
	}
	ceiling := config.DefaultMaxAwaitTimeout
	if r.maxAwaitTimeoutFn != nil {
		ceiling = r.maxAwaitTimeoutFn()
	}
	if eff := ceiling + deployBudget; eff > base {
		return eff
	}
	return base
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
			// rerun-last: UnlockForRerun под FOR UPDATE уже перевёл
			// error_locked→applying минуя ready (race-free). Статус НЕ транзитим
			// повторно — обязаны УВИДЕТЬ applying, иначе старт отклоняется
			// (fail-closed): любой иной статус означает, что зарезервированная
			// строка ушла из-под нас (чужой перехват / неконсистентный вызов).
			if got.Status != incarnation.StatusApplying {
				return fmt.Errorf("%w: %s (rerun expected applying)", ErrNotRunnable, got.Status)
			}
			// Запись epoch applying-флага на уже-applying-строку (ADR-027 amend
			// (m-S1)): UnlockForRerun транзитит error_locked→applying БЕЗ epoch,
			// поэтому без этого дописывания rerun-last-applying оставался бы с
			// NULL-epoch и не попадал под reconcile_orphan_applying — краш владельца
			// mid-rerun-last давал orphan навсегда. lockApplyingWithEpoch WHERE
			// name=$1 (без status-guard) идемпотентно перезаписывает epoch, статус
			// остаётся applying. Остаточное микроокно UnlockForRerun-tx↔эта tx
			// деградирует в тот же NULL-epoch known-gap (не хуже текущего).
			if uerr := lockApplyingWithEpoch(ctx, tx, spec.IncarnationName, spec.ApplyID, r.kid, 0); uerr != nil {
				return uerr
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
		// Перевод в applying + запись epoch applying-флага ОДНИМ UPDATE/одной tx
		// (ADR-027 amend (m-S1)): apply_id прогона + attempt (echo начального
		// apply_runs.attempt=0, строки apply_runs ещё нет) + KID этого инстанса
		// (тот же источник, что lease-holder) + applying_since=NOW(). Reaper-правило
		// reconcile_orphan_applying по этому epoch различает живой прогон от
		// осиротевшего lock-а мёртвого владельца. Атомарность с status='applying'
		// исключает окно applying-без-epoch при крахе до commit.
		if uerr := lockApplyingWithEpoch(ctx, tx, spec.IncarnationName, spec.ApplyID, r.kid, 0); uerr != nil {
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
func (r *Runner) parseScenario(art *artifact.ServiceArtifact, scenarioName string, fromUpgrade bool) (*config.ScenarioManifest, error) {
	return parseScenarioFromArtifact(r.deps.Loader, art, scenarioName, fromUpgrade)
}

// scenarioRelPath строит rel-путь главного YAML сценария в снапшоте сервиса:
// upgrade/<name>/main.yml при fromUpgrade (ADR-0068), иначе scenario/<name>/main.yml.
func scenarioRelPath(scenarioName string, fromUpgrade bool) string {
	format := scenarioMainFile
	if fromUpgrade {
		format = upgradeMainFile
	}
	return fmt.Sprintf(format, scenarioName)
}

// parseScenarioFromArtifact — package-level форма [Runner.parseScenario]:
// читает и парсит scenario/<scenarioName>/main.yml из снапшота сервиса.
// Вынесена из метода, чтобы переиспользоваться Acolyte-путём ([RenderForHost])
// без подъёма Runner-а. Поведение идентично — чистый read+parse без side-эффектов.
func parseScenarioFromArtifact(loader *artifact.ServiceLoader, art *artifact.ServiceArtifact, scenarioName string, fromUpgrade bool) (*config.ScenarioManifest, error) {
	rel := scenarioRelPath(scenarioName, fromUpgrade)
	data, err := loader.ReadFile(art, rel)
	if err != nil {
		return nil, fmt.Errorf("scenario: read %s: %w", rel, err)
	}
	// Резолв $type на загрузке: render-pipeline и value-валидация ниже работают
	// с самодостаточной input-схемой (см. artifact.LoadScenarioManifestResolved).
	scn, _, diags, err := artifact.LoadScenarioManifestResolved(art, rel, data)
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
func (r *Runner) lockIncarnation(ctx context.Context, spec RunSpec, stateBefore map[string]any, failStatus incarnation.Status, reason string, cause error, sealedPaths map[string]bool, log *slog.Logger) (finalized bool) {
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
	// observability-only, на wire (ApplyRequest.Params) это не влияет. seal-aware
	// (ADR-010 §7.4): vault+regex слои + sealed-пути этого прогона + regex-аларм
	// (DefaultSealHooks). sealedPaths пуст (abort до Render) → деградация к
	// vault+regex БИТ-В-БИТ.
	details = audit.MaskSecretsSealed(details, audit.SealOpts{
		Sealed:        sealedPaths,
		RegexFallback: audit.DefaultSealHooks.RegexFallback,
		Logger:        audit.DefaultSealHooks.Logger,
	})
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
//     dispatched, ЛИБО keeper-dispatch Passage>0 упал ПОСЛЕ успешного host-dispatch
//     раннего Passage — Слайс 2: keeper-dispatch теперь ВНУТРИ stage-loop) —
//     терминалим КАЖДУЮ недотерминальную в `terminal` через [applyrun.UpdateStatus]
//     (он single-winner: уже-терминальные строки не трогаются, ADR-027(j)). running-
//     строки НЕ трогаем: apply уже ушёл на хост, честный терминал придёт с него
//     (RunResult). keeper-строка упавшего Passage уже в failed (dispatchKeeperTasks
//     записал её ДО возврата ошибки), host-строки раннего Passage остаются success.
//     Не вставляем sentinel — реальные строки есть;
//   - строк НЕТ (ранний abort: no_hosts / scenario_load_failed / topology_failed
//     / essence_failed / input_invalid / render_failed, а также keeper_dispatch_failed
//     на Passage 0 — keeper-задачи первого Passage идут ДО любого host-dispatch) —
//     вставляем ОДНУ sentinel-строку [render.RunSentinelSID] со status=`terminal` и
//     error_summary=reason.
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
		if uerr := applyrun.UpdateStatus(wctx, r.deps.DB, spec.ApplyID, st.SID, st.Passage, terminal, &summary); uerr != nil {
			log.Warn("scenario: терминализация строки apply_runs провалена",
				slog.String("sid", st.SID), slog.Int("passage", st.Passage), slog.String("reason", reason), slog.Any("error", uerr))
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

// persistRunPlan сохраняет host-инвариантный план задач прогона (apply_run_plan,
// NIM-37) для read-эндпоинта /tasks: строка на plan_index с name/module/no_log/
// passage. Вызывается один раз после render (шаг 5) — метаданные плана стабильны
// между Passage. name/module/no_log — НЕ секрет (адрес/тип задачи, не значения
// params), маскинг не нужен; params отложены в S1b.
//
// Best-effort: r.deps.DB=nil (unit без PG) или ошибка записи только логируются —
// потеря плана деградирует наблюдаемость /tasks, но не корректность прогона
// (симметрично accumulateRegister/emitRunCompleted).
func (r *Runner) persistRunPlan(ctx context.Context, spec RunSpec, tasks []*render.RenderedTask, log *slog.Logger) {
	if r.deps.DB == nil || len(tasks) == 0 {
		return
	}
	plan := make([]applyrun.RunPlanTask, 0, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		plan = append(plan, applyrun.RunPlanTask{
			ApplyID:   spec.ApplyID,
			PlanIndex: t.Index,
			Name:      t.Name,
			Module:    t.Module,
			NoLog:     t.NoLog,
			Passage:   t.Passage,
		})
	}
	if err := applyrun.InsertRunPlan(ctx, r.deps.DB, spec.ApplyID, plan); err != nil {
		log.Warn("scenario: персист плана задач прогона (apply_run_plan) провален — /tasks без плана",
			slog.String("apply_id", spec.ApplyID), slog.Any("error", err))
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

// maskErrText возвращает текст ошибки, прогнанный через seal-aware маскинг:
// render/dispatch-ошибка может нести vault:secret/-ref (наводку на секрет-
// локацию). Тот же фильтр, что для status_details, применяется и к slog-каналу
// (лог-файл — наблюдаемый канал). sealedPaths — пути sealed-ячеек прогона
// ([ADR-010] §7.4): здесь под единым ключом `error` они почти всегда no-op
// (свободный текст ошибки несёт путь/выражение, не значение по sealed-пути), но
// маскинг идёт тем же MaskSecretsSealed для симметрии со status_details и чтобы
// regex-аларм (DefaultSealHooks) фиксировал sensitive-by-name в тексте. nil →
// пустая строка.
func maskErrText(err error, sealedPaths map[string]bool) string {
	if err == nil {
		return ""
	}
	masked := audit.MaskSecretsSealed(map[string]any{"error": err.Error()}, audit.SealOpts{
		Sealed:        sealedPaths,
		RegexFallback: audit.DefaultSealHooks.RegexFallback,
		Logger:        audit.DefaultSealHooks.Logger,
	})
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
