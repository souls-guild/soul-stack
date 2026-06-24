package render

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// tracer для in-process span-а render-пайплайна (ADR-024 §4). Берёт глобальный
// TracerProvider, поднятый [obs.SetupOTel] в cmd/keeper; при OTel disabled
// провайдер no-op — span бесплатен, код не ветвится.
var tracer = otel.Tracer("keeper/render")

// KVReader — узкое подмножество keeper/internal/vault.Client, нужное
// vault-resolve-фазе pipeline (`vault:`-refs в params). *vault.Client
// удовлетворяет интерфейсу как есть; сужение позволяет герметичный прогон
// раннера Trial ([ADR-023]) с fixture-backed reader-ом без поднятия Vault.
// Симметрично keeper/internal/coremod/vault.VaultReader.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// Pipeline оркестрирует Keeper-side фазы рендера scenario ([ADR-010]).
// Потокобезопасен: cel.Engine и KVReader держат собственные внутренние
// блокировки/пулы, Pipeline собственного изменяемого состояния не имеет.
type Pipeline struct {
	vault   KVReader
	cel     *cel.Engine
	logger  *slog.Logger
	metrics *RenderMetrics
}

// NewPipeline конструирует Pipeline. engine обязателен. vc допускает nil
// (scenario без vault-refs — vault-resolve no-op; ref при nil-reader →
// ошибка в фазе vault-resolve). logger допускает nil (диагностика подавляется).
// metrics допускает nil (keeper_render_*-метрики выключены — nil-safe методы
// [RenderMetrics] no-op; так поднимаются unit-тесты, dev-сборка, Trial).
//
// Резолвер destiny (apply:destiny) передаётся per-Render через
// [RenderInput.Destiny] (не поле Pipeline) — Pipeline неизменяем и шарится между
// конкурентными прогонами, а резолвер per-run (несёт destiny[]-refs конкретного
// service-снапшота). RenderInput.Destiny=nil → apply:destiny → [ErrUnsupportedDSL].
func NewPipeline(vc KVReader, engine *cel.Engine, logger *slog.Logger, metrics *RenderMetrics) *Pipeline {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Pipeline{vault: vc, cel: engine, logger: logger, metrics: metrics}
}

// Render прогоняет scenario через фазы vault-resolve → CEL-render → резолв
// `on:`/`where:` и возвращает плоский список отрендеренных задач + план
// диспатча (task → хосты).
//
// Pilot-объём DSL: поддержаны module-задачи (включая `core.file.rendered`) с
// sequential-исполнением и per-host fan-out-ом, а также `apply: destiny` —
// изолированный render-проход destiny (V2, ADR-009): destiny рендерится со СВОИМ
// input-scope, её задачи вклеиваются в общий план. Ключи serial:/run_once:/
// block:/include:/loop:/parallel: и on: keeper → [ErrUnsupportedDSL] (слайсы B/D/E).
//
// Index/TaskIndex — сквозной индекс по итоговому плану: scenario-задачи и
// вклеенные destiny-задачи нумеруются единым монотонным счётчиком (связь
// RenderedTask↔DispatchPlan↔TaskEvent.task_idx). Без apply:destiny индекс
// совпадает с позицией в scenario.tasks[].
//
// CEL-рендер params — per-host (soulprint.self хоста). В pilot params обязаны
// быть host-инвариантны: если задача даёт разные params на разных targeted-
// хостах, это host-зависимый рендер, который контракт «один RenderedTask на
// task» не выражает (per-host ApplyRequest — слой orchestrator-а .g) → ошибка.
func (p *Pipeline) Render(ctx context.Context, in RenderInput) (_ []*RenderedTask, _ []DispatchPlan, err error) {
	if in.Scenario == nil {
		return nil, nil, fmt.Errorf("render: scenario manifest is nil")
	}

	// keeper_render_*-метрики (ADR-024): длительность всего прохода + counter
	// ошибок. Наблюдение в defer по named-return err — симметрично span-у ниже,
	// одно измерение на проход. nil metrics → no-op. nil-scenario выше отсекается
	// ДО старта замера: это структурный отказ caller-а, не «рендер выполнялся».
	start := time.Now()
	defer func() { p.metrics.ObserveRender(time.Since(start), err) }()

	// In-process span на render-пайплайн (vault-resolve → CEL → on/where) —
	// child от scenario.run (ADR-024 §4): самая тяжёлая Keeper-side фаза прогона,
	// внутри scenario.run-span-а ранее не различалась. incarnation/scenario name —
	// доменные идентификаторы для фильтрации трейса (в metric-labels запрещены
	// §2.2); секретов (params / vault-значения) в атрибуты НЕ кладём. При OTel
	// disabled tracer no-op — Start/End бесплатны.
	ctx, span := tracer.Start(ctx, "render.pipeline",
		trace.WithAttributes(
			attribute.String("incarnation", in.Incarnation.Name),
			attribute.String("scenario", in.Scenario.Name),
			attribute.Int("tasks", len(in.Scenario.Tasks)),
			attribute.Int("hosts", len(in.Hosts)),
		),
	)
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, "render_failed")
		}
		span.End()
	}()

	in.Ctx = ctx // прокинуть в CEL vault() (отмена/таймаут ReadKV)

	// compute: резолвится ОДИН раз на прогон (рун-уровневый контекст без soulprint,
	// барьер host-инвариантности) ДО рендера задач — результат `compute.<name>`
	// виден в apply.input/where/params всех хостов через hostVars (ADR-009).
	computed, cerr := p.resolveCompute(in)
	if cerr != nil {
		return nil, nil, cerr
	}
	in.Compute = computed

	tasks := make([]*RenderedTask, 0, len(in.Scenario.Tasks))
	plans := make([]DispatchPlan, 0, len(in.Scenario.Tasks))
	idx := 0

	// passageStart фиксирует, с какого RenderedTask начинается текущая top-level
	// задача — чтобы заклеймить весь её выход (включая apply:destiny/loop-потомков)
	// passage-индексом originating-задачи (staged-render, ADR-056). Стампинг делаем
	// в конце каждой итерации (stampPassage), а не точечно: один проход, любые
	// expansion-ветки покрыты автоматически.
	for i := range in.Scenario.Tasks {
		task := in.Scenario.Tasks[i]

		passage := taskPassageAt(in.TaskPassage, i)
		passageStart := len(tasks)

		// assert-задача (ADR-009 amendment 2026-06-23) — keeper-side render-time
		// precondition. Обрабатывается ДО emitStaticWhenSkip/guardPilotDSL: assert НЕ
		// emit RenderedTask (это проверка, не задача), поэтому НЕЛЬЗЯ дать
		// emitStaticWhenSkip эмитить под неё placeholder. evalAssertTask сам соблюдает
		// `when:`-гейт (static-when-false → assert не вычисляется), а на провале
		// предиката возвращает ErrAssertFailed (render обрывается, idx не растёт).
		// idx/tasks/plans НЕ меняются — задачи после assert сдвигаются на её позицию.
		//
		// RUN-LEVEL «один раз»: в staged-render Render зовётся per-Passage с растущим
		// ActivePassage; assert вычисляем ТОЛЬКО когда его Passage активен (иначе
		// повтор на каждом Passage). Не-staged (TaskPassage==nil: Trial/Acolyte/
		// CheckDrift) → passage всегда 0 == ActivePassage 0 → один проход, БИТ-В-БИТ.
		if IsAssertTask(task) {
			if in.TaskPassage == nil || passage == in.ActivePassage {
				if err := p.evalAssertTask(in, task); err != nil {
					return nil, nil, err
				}
			}
			continue
		}

		// Static-when ПРЕДШЕСТВУЕТ guardPilotDSL (ADR-012(d), расширение static-when-
		// инварианта): статически-false `when:` → задача gated off → скипается ДО
		// ЛЮБОЙ eager-обработки, включая DSL-guard. Неактивная ветка с unsupported-DSL
		// (`parallel:`/`block:`) не блокирует активную — её DSL отвергается ТОЛЬКО при
		// активации (per-action валидация). Это не маскировка: задача физически не
		// исполняется (parallel: никогда не достигается). isStaticWhen/staticWhenSkips
		// register-/soulprint-независимы и строят flow_context из input/vars/essence/
		// incarnation/self — НЕ из DSL-полей, поэтому вызов ДО guard безопасен.
		if skipped, serr := p.emitStaticWhenSkip(ctx, in, task, &tasks, &plans, &idx); serr != nil {
			return nil, nil, serr
		} else if skipped {
			stampPassage(tasks, passageStart, passage)
			continue
		}

		if err := guardPilotDSL(task, i); err != nil {
			return nil, nil, err
		}

		// Будущий Passage (staged-render, ADR-056 §в.1): register ещё не собран —
		// НЕ резолвим register-зависимые where:/params: (упали бы на пустом
		// register — это и есть исходный drift). Эмитим placeholder ради сквозной
		// index-нумерации; orchestrator его в активном Passage не диспатчит. Когда
		// его Passage станет активным, повторный Render резолвит полноценно.
		// Гейт строго staged (TaskPassage задан): не-staged caller сюда не входит.
		if in.TaskPassage != nil && passage > in.ActivePassage {
			rt := &RenderedTask{Index: idx, Name: task.Name, Register: task.Register, ID: task.ID, Passage: passage}
			if task.Module != nil {
				rt.Module = task.Module.Module
			}
			tasks = append(tasks, rt)
			plans = append(plans, DispatchPlan{TaskIndex: idx})
			idx++
			continue
		}

		// keeper-side задача (`on: keeper`, docs/keeper/modules.md): хостов нет —
		// рендерим params в keeper-контексте (без per-host soulprint) и выдаём
		// единичный keeper-target. Исполняется локально на keeper-инстансе
		// (scenario-runner), не диспатчится Soul-у. apply:/loop: на keeper-задаче
		// в пилоте не поддержаны (guardPilotDSL пропускает apply, поэтому проверяем
		// здесь явно).
		if IsKeeperTask(task) {
			rt, derr := p.renderKeeperTask(ctx, in, task, idx)
			if derr != nil {
				return nil, nil, derr
			}
			tasks = append(tasks, rt)
			plans = append(plans, DispatchPlan{
				TaskIndex:  idx,
				TargetSIDs: []string{KeeperTargetSID},
				Keeper:     true,
			})
			idx++
			stampPassage(tasks, passageStart, passage)
			continue
		}

		targeted, err := resolveTargets(p.cel, in, task)
		if err != nil {
			return nil, nil, err
		}

		// run_once: режет таргет до одного хоста (первого по SID) ДО рендера
		// params и формирования плана — orchestration.md §2.2.2.
		targeted = applyRunOnce(targeted, task.RunOnce)

		// apply: destiny — изолированный render-проход destiny (V2). Её задачи
		// вклеиваются в общий план со сквозными индексами; одна apply-задача
		// разворачивается в N destiny-задач. run_once родителя уже применён к
		// targeted; serial: на apply-задаче распространяется на её destiny-задачи.
		if task.Apply != nil {
			width := serialWidth(task.Serial, len(targeted))
			dt, dp, derr := p.renderApplyDestiny(ctx, in, task.Apply, idx, targeted, width)
			if derr != nil {
				return nil, nil, derr
			}
			tasks = append(tasks, dt...)
			plans = append(plans, dp...)
			idx += len(dt)
			stampPassage(tasks, passageStart, passage)
			continue
		}

		// loop: на module-задаче (slice E1) — render-time fan-out: одна задача
		// раскрывается в N RenderedTask по элементам items, со сквозными
		// индексами (симметрично apply:destiny). loop размножается ПОСЛЕ резолва
		// таргета (on→where→run_once), внутри каждого targeted-хоста; serial:
		// наследуется всеми итерациями (оси ортогональны, orchestration.md §2.2).
		if task.Loop != nil {
			lt, lp, lerr := p.renderLoopTask(ctx, in, task, idx, targeted)
			if lerr != nil {
				return nil, nil, lerr
			}
			tasks = append(tasks, lt...)
			plans = append(plans, lp...)
			idx += len(lt)
			stampPassage(tasks, passageStart, passage)
			continue
		}

		// block: (pilot C1) — render-time fan-out в плоский слой RenderedTask, как
		// loop/apply:destiny. targeted уже резолвлен по block.on/block.where +
		// run_once (выше) — потомки наследуют on/where/run_once бесплатно. width из
		// block.serial раздаётся всем потомкам. stampPassage клеймит весь fan-out
		// одним Passage (block атомарен по Passage, ADR-056). Static-when-false
		// block НЕ гасится emitStaticWhenSkip (она пропускает block-задачу) — он
		// заходит сюда, walkBlockChildren вольёт block.when в каждого потомка через
		// AND и каждый child эмитит СВОЙ skip-placeholder с register/requisites
		// (flat-register-scope цел при skip — иначе register потомков терялся бы).
		if task.Block != nil {
			bt, bp, berr := p.renderBlockTask(ctx, in, task, idx, targeted)
			if berr != nil {
				return nil, nil, berr
			}
			tasks = append(tasks, bt...)
			plans = append(plans, bp...)
			idx += len(bt)
			stampPassage(tasks, passageStart, passage)
			continue
		}

		rt, err := p.renderTask(ctx, in, task, idx, targeted)
		if err != nil {
			return nil, nil, err
		}

		tasks = append(tasks, rt)
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(targeted),
			SerialWidth: serialWidth(task.Serial, len(targeted)),
		})
		idx++
		stampPassage(tasks, passageStart, passage)
	}

	// Резолв `onchanges:`/`onfail:` register-имён в task-индексы (Variant A) —
	// финальным проходом, когда весь план собран: при apply:destiny/loop Index
	// сквозные и раньше резолва ещё не известны (renderTaskIter рендерит до того,
	// как появятся последующие задачи-источники).
	if err := resolveOnChanges(tasks); err != nil {
		return nil, nil, err
	}
	if err := resolveOnFail(tasks); err != nil {
		return nil, nil, err
	}

	return tasks, plans, nil
}

// renderTask рендерит params одной module-задачи (после vault-resolve + CEL) и
// собирает RenderedTask. Тонкая обёртка над renderTaskIter без loop-переменных.
func (p *Pipeline) renderTask(ctx context.Context, in RenderInput, task config.Task, idx int, targeted []*topology.HostFacts) (*RenderedTask, error) {
	return p.renderTaskIter(ctx, in, task, idx, targeted, nil)
}

// renderTaskIter рендерит params одной module-задачи (или одной `loop:`-итерации)
// per-host (после vault-resolve + CEL) и собирает RenderedTask. params рендерятся
// per-host и сверяются на host-инвариантность (pilot-ограничение, см. Render).
//
// loopVars — переменные текущей итерации (`<as>`/`<index_as>`); nil для задачи
// без loop:. host-инвариантность проверяется ПО-ИТЕРАЦИОННО: для фиксированных
// loopVars params обязаны совпадать на всех targeted-хостах. По оси ИТЕРАЦИЙ
// loop легитимно порождает разные params (caller renderLoopTask вызывает
// renderTaskIter с разными loopVars) — это не нарушение инварианта.
//
// Пустой targeted (where: отфильтровал всех) — задача всё равно появляется в
// списке (с пустым DispatchPlan); params рендерятся в контексте без soulprint,
// чтобы RenderedTask был полноценным, а orchestrator сам пропустил диспатч.
func (p *Pipeline) renderTaskIter(ctx context.Context, in RenderInput, task config.Task, idx int, targeted []*topology.HostFacts, loopVars map[string]any) (*RenderedTask, error) {
	// Guard fail-closed: host-вариативный flow-control-предикат (soulprint.self) на
	// multi-host таргете молча решился бы по фактам первого хоста для всех (dispatch
	// раздаёт один RenderedTask с flow_context первого хоста). Режем ДО построения
	// flow_context — симметрично reLoopWhenSoulprint (loop.go).
	if err := guardFlowControlHostInvariant(task, targeted); err != nil {
		return nil, err
	}

	rt := &RenderedTask{
		Index:    idx,
		Name:     task.Name,
		Module:   task.Module.Module,
		Register: task.Register,
		ID:       task.ID,
		NoLog:    task.NoLog,
		Timeout:  task.Timeout,
		// flow-control CEL-строки (ADR-012(d)) протягиваются КАК ЕСТЬ — Keeper их
		// НЕ вычисляет (зависят от register.* предыдущих задач, известных только
		// Soul-у). Host-инвариантны (текст предиката один на задачу); Soul
		// вычисляет per-host.
		When:           task.When,
		ChangedWhen:    task.ChangedWhen,
		FailedWhen:     task.FailedWhen,
		onChangesNames: task.OnChanges,
		onFailNames:    task.OnFail,
	}

	// retry: (destiny/tasks.md §9) — энфорс Soul-side; Keeper протягивает поля как
	// есть. nil Retry → одна попытка (zero-value RetryCount=0, until/delay пусты).
	if task.Retry != nil {
		rt.RetryCount = task.Retry.Count
		rt.RetryDelay = task.Retry.Delay
		rt.Until = task.Retry.Until
	}

	// Хосты для CEL-рендера: targeted, либо — если where: отфильтровал всех —
	// один синтетический пустой контекст (params без soulprint-зависимости).
	renderHosts := targeted
	if len(renderHosts) == 0 {
		renderHosts = []*topology.HostFacts{{}}
	}

	// Static-when placeholder-skip (ADR-012(d), Вариант b): при register-/soulprint-
	// независимом when:, вычислившемся в false на Keeper-е, params НЕ рендерятся —
	// задача всё равно станет SKIPPED на Soul-е (он вычислит тот же when по тому же
	// flow_context). Это лечит multi-action destiny: задачи неактивной ветки
	// (`when: input.action == 'apply'` при другом action) читают optional-input,
	// которого нет → no-such-key → render_failed эжер-рендера. Skip собирает ТОЛЬКО
	// flow_context (Soul читает его для evalWhen — сборка из input/vars/essence/
	// incarnation/self, НЕ из падающих params, безопасна) и оставляет полноценный
	// RenderedTask (Index/Passage/Register/When/requisites сохранены, params пусты).
	// Решение детерминировано (static-when host-инвариантен) — берётся на первом
	// хосте, fc остальных собирается ради валидности snapshot-а.
	if skip, serr := p.staticWhenSkips(in, task, renderHosts, len(targeted), loopVars); serr != nil {
		return nil, serr
	} else if skip != nil {
		rt.FlowContext = skip
		return rt, nil
	}

	resolved, err := resolveVaultRefs(ctx, p.vault, task.Module.Params)
	if err != nil {
		return nil, fmt.Errorf("render: task %q: %w", task.Name, err)
	}

	// seal / sealed-paths ([ADR-010] §7.4): пометить пути ячеек params, чьё СЫРОЕ
	// `${ … }`-значение читает secret-источник (secret-input/vault()). Обход СЫРЫХ
	// params (task.Module.Params, ДО resolveVaultRefs+CEL) — единственный, где
	// видны исходные выражения. Per-task (host-инвариантно), nil-Sealed → no-op.
	collectSealed(p.cel, in.Sealed, task.Module.Params, scenarioSealSources(in), "")

	isRendered := task.Module.Module == moduleFileRendered
	var firstSID string
	for hi, h := range renderHosts {
		vars := hostLoopVars(in, h, len(targeted), loopVars)
		vars, err = resolveTaskVars(p.cel, fileVarsForHost(in, h), task.Vars, vars)
		if err != nil {
			return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
		}
		st, err := renderParams(p.cel, resolved, vars)
		if err != nil {
			return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
		}
		// core.file.rendered: собираем per-host render_context {vars,self,role,
		// essence} (templating.md §3.2) из CEL-rendered params.vars и кладём в
		// params рядом с template_content. Плоский ключ vars удаляем — Soul читает
		// корень только из render_context (template_content + render_context, см.
		// §3.2/§6). render_context host-вариативен (self per-host) — он исключён из
		// host-инвариантной сверки ниже. Для golden-path (один хост) rt.Params
		// несёт render_context именно этого хоста.
		if isRendered {
			paramsVars := extractParamsVars(st)
			delete(st.Fields, paramVars)
			if err := setRenderContext(st, buildRenderContext(in, h, paramsVars)); err != nil {
				return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
			}
		}
		// flow_context (ADR-012(d)): per-host снапшот {input,vars,essence,
		// incarnation,self} для Soul-side flow-control-предикатов. Собирается из той
		// же vars, что и params (минус soulprint.hosts/loop, см. buildFlowContext).
		// Host-вариативен (self per-host) — как render_context, исключён из
		// host-инвариантной сверки; rt несёт значение первого хоста (golden-path).
		//
		// На hi>0 fc пересобирается ТОЛЬКО ради ошибки построения (валидность
		// snapshot-а этого хоста); wire-значение rt.FlowContext берётся от первого
		// хоста (hi==0). Это НЕ забытый per-host dispatch — host-вариативность
		// flow-control на multi-host уже отсечена guardFlowControlHostInvariant.
		fc, err := buildFlowContext(in, h, vars, len(targeted))
		if err != nil {
			return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
		}

		if hi == 0 {
			rt.Params = st
			rt.FlowContext = fc
			firstSID = h.SID
			continue
		}
		if !paramsHostInvariant(rt.Params, st) {
			return nil, fmt.Errorf(
				"render: task %q даёт host-зависимые params (%s vs %s) — host-вариативность params вне pilot-объёма (per-host ApplyRequest — слой orchestrator-а)",
				task.Name, firstSID, h.SID)
		}
		// Второй контур fail-closed: host-вариативный flow_context (vars,
		// производный от soulprint.self, протёкший в flow_context.vars). Текст
		// предиката тогда не содержит soulprint (например, `when: vars.is_debian`),
		// и regex-guard (guardFlowControlHostInvariant) его НЕ ловит — обход через
		// task-level vars. Здесь сверяем собранный flow_context-МИНУС-self между
		// хостами (self host-вариантен по природе и закрыт текстовым guard).
		//
		// ГЕЙТ: сверка активна ТОЛЬКО при наличии хоть одного непустого
		// flow-control-предиката. Без предиката Soul flow_context не читает — его
		// вариативность безразлична; легитимная задача с host-вариативным vars-в-
		// params (без when) должна падать на СВОЁМ paramsHostInvariant выше, не на
		// этой сверке.
		//
		// Оба контура (regex по тексту + сверка снапшота) — ВРЕМЕННЫЙ fail-closed
		// до per-host dispatch (open Q №25); при его реализации снимаются
		// согласованно.
		if hasFlowControl(task) && !flowContextHostInvariant(rt.FlowContext, fc) {
			return nil, fmt.Errorf(
				"render: task %q: host-вариативный flow_context (vars, производный от soulprint.self) на multi-host таргете (%s vs %s) — fail-closed; per-host dispatch отложен (отдельный ADR)",
				task.Name, firstSID, h.SID)
		}
	}

	// core.file.rendered: после CEL-фазы заменяем params.template (путь) на
	// literal template_content (Keeper читает .tmpl, A1/ADR-012(d)). text/template
	// не исполняется — рендер на Soul. Путь шаблона host-инвариантен, читаем один
	// раз после сборки rt.Params.
	if err := injectTemplateContent(rt, in.Templates); err != nil {
		return nil, err
	}

	return rt, nil
}

// staticWhenSkips решает, пропускать ли рендер params задачи по статическому when:
// (ADR-012(d), Вариант b placeholder-skip). Возвращает:
//   - (fc, nil) — задачу СКИПАЕМ: when статический (register-/soulprint-независимый)
//     и вычислился в false. fc — flow_context первого хоста (его Soul читает для
//     собственного evalWhen → подтвердит when:false → SKIPPED, как сейчас);
//   - (nil, nil) — НЕ скипаем: when не статический (register/soulprint/пустой) ИЛИ
//     статический-но-true. Обычный путь с рендером params.
//   - (nil, err) — ошибка сборки flow_context либо eval статического предиката
//     (битый when — Keeper падает так же, как упал бы Soul; см. evalStaticWhen).
//
// Решение детерминировано: static-when host-инвариантен по построению (не зависит
// от soulprint.self/register — единственных host-вариативных слоёв), поэтому
// вычисляется на ПЕРВОМ хосте. flow_context остальных хостов всё равно собирается
// (валидность snapshot-а каждого хоста — как и в основном цикле renderTaskIter), но
// в исход static-when не входит. Так skip консистентен по хостам и по Passage:
// один input/state-снимок прогона даёт один и тот же false на всех хостах и при
// повторном рендере следующего Passage.
func (p *Pipeline) staticWhenSkips(
	in RenderInput,
	task config.Task,
	renderHosts []*topology.HostFacts,
	targetCount int,
	loopVars map[string]any,
) (*structpb.Struct, error) {
	if !isStaticWhen(task.When) {
		return nil, nil
	}

	var firstFC *structpb.Struct
	for hi, h := range renderHosts {
		vars := hostLoopVars(in, h, targetCount, loopVars)
		vars, err := resolveTaskVars(p.cel, fileVarsForHost(in, h), task.Vars, vars)
		if err != nil {
			return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
		}
		fc, err := buildFlowContext(in, h, vars, targetCount)
		if err != nil {
			return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
		}
		if hi == 0 {
			firstFC = fc
		}
	}

	pass, err := evalStaticWhen(task.When, firstFC)
	if err != nil {
		return nil, fmt.Errorf("render: task %q: static-when %q: %w", task.Name, task.When, err)
	}
	if pass {
		return nil, nil // when:true — задача активна, рендерим params обычным путём.
	}
	return firstFC, nil
}

// emitStaticWhenSkip — ранний static-when placeholder-skip, выполняемый в НАЧАЛЕ
// итерации обхода задач, ДО guardPilotDSL/guardDestinyTask (ADR-012(d), расширение
// static-when-инварианта). Если `when:` статический (register-/soulprint-независим)
// и вычислился в false — задача gated off: эмитим skip-placeholder(ы), мутируя
// tasks/plans/idx через указатели, и возвращаем skipped=true. Caller делает
// `continue` БЕЗ guard и БЕЗ рендера — поэтому unsupported-DSL (`parallel:`/`block:`)
// в неактивной ветке не отвергается (он недостижим — задача не исполняется).
//
// Возвращает:
//   - (false, nil) — НЕ static-skip: when не статический ИЛИ статический-но-true.
//     Caller идёт обычным путём (guard → resolveTargets → render);
//   - (true, nil) — задача скипнута, placeholder(ы) уже добавлены;
//   - (false, err) — ошибка сборки flow_context/eval статического предиката.
//
// flow_context строится из in.Hosts (синтетический пустой хост при пустом roster):
// static-when host-инвариантен (не зависит от soulprint.self), исход одинаков на
// всех хостах; `self` в placeholder-flow_context Soul читает лишь как данные для
// собственного evalWhen, который тот же предикат тоже признает false → SKIPPED.
//
// loop-задача (task.Loop != nil): N/1 skip-placeholder через loopStaticSkip —
// паритет Index с активной веткой (резолвимый items → N, нерезолвимый → 1). Это
// ловит loop ДО renderLoopTask — единственный путь static-skip для loop (внутри
// renderLoopTask static-when-гейта больше нет). Не-loop задача → один placeholder.
func (p *Pipeline) emitStaticWhenSkip(
	ctx context.Context,
	in RenderInput,
	task config.Task,
	tasks *[]*RenderedTask,
	plans *[]DispatchPlan,
	idx *int,
) (bool, error) {
	if !isStaticWhen(task.When) {
		return false, nil
	}

	// block-задача со static-false when: НЕ гасится здесь одним placeholder-ом
	// (иначе ветка renderBlockTask/renderDestinyBlock не отработает, потомки не
	// материализуются, и их register теряется — resolveOnChanges снаружи падает
	// ErrOnChangesUnknownRegister). Отдаём её ветке block: mergeBlockInheritance
	// вольёт block.when в КАЖДОГО потомка через AND, static-when каждого потомка
	// станет false, и каждый child сам пройдёт через emitStaticWhenSkip внутри
	// walkBlockChildren, эмитнув placeholder со СВОИМ Register/requisites/ID
	// (паттерн loopStaticSkip — block раскрывается per-потомок, не одним
	// placeholder-ом). flat-register-scope цел и при skip. Сам block register не
	// несёт (запрещён валидатором) — на block-узле терять нечего.
	if task.Block != nil {
		return false, nil
	}

	renderHosts := in.Hosts
	if len(renderHosts) == 0 {
		renderHosts = []*topology.HostFacts{{}}
	}
	skip, err := p.staticWhenSkips(in, task, renderHosts, len(in.Hosts), nil)
	if err != nil {
		return false, err
	}
	if skip == nil {
		return false, nil // static-true → активна, обычный путь.
	}

	if task.Loop != nil {
		asName := task.Loop.As
		if asName == "" {
			asName = defaultLoopVar
		}
		lt, lp, lerr := p.loopStaticSkip(in, task, *idx, in.Hosts, asName, skip)
		if lerr != nil {
			return false, lerr
		}
		*tasks = append(*tasks, lt...)
		*plans = append(*plans, lp...)
		*idx += len(lt)
		return true, nil
	}

	*tasks = append(*tasks, p.staticSkipPlaceholder(task, *idx, skip))
	*plans = append(*plans, DispatchPlan{TaskIndex: *idx})
	*idx++
	return true, nil
}

// staticSkipPlaceholder строит один skip-placeholder для задачи с статически-false
// when: (Params=nil — рендер пропущен; flow_context первого хоста; протянутые
// When/ID/Register/requisites). Module берётся при наличии module-задачи (block/
// parallel-без-module → пустой Module — placeholder валиден, исполнения не будет).
func (p *Pipeline) staticSkipPlaceholder(task config.Task, idx int, skip *structpb.Struct) *RenderedTask {
	rt := &RenderedTask{
		Index:          idx,
		Name:           task.Name,
		Register:       task.Register,
		ID:             task.ID,
		NoLog:          task.NoLog,
		Timeout:        task.Timeout,
		When:           task.When,
		ChangedWhen:    task.ChangedWhen,
		FailedWhen:     task.FailedWhen,
		onChangesNames: task.OnChanges,
		onFailNames:    task.OnFail,
		FlowContext:    skip,
	}
	if task.Module != nil {
		rt.Module = task.Module.Module
	}
	return rt
}

// EvalAsserts вычисляет ТОЛЬКО assert-задачи scenario (ADR-009 amendment
// 2026-06-23, двухточечная eval) — без эмита RenderedTask и без vault-resolve/
// dispatch/on-where. Переиспользуется pre-flight-гейтом create-прогона
// (request-путь, ДО коммита incarnation — keeper/internal/scenario.PreflightAssert):
// тот же source-of-truth вычисления предиката, что и render-ветка ([Render] →
// [evalAssertTask]), без диалекта.
//
// Контракт совпадает с assert-веткой Render бит-в-бит: проходит scenario.Tasks по
// порядку, для каждой [IsAssertTask]-задачи зовёт общий [evalAssertTask] (тот же
// when:-гейт, тот же run-level CEL-контекст с soulprint.hosts). Первый false →
// [ErrAssertFailed] (обрыв, текст = message + индекс/текст предиката). Не-assert
// задачи пропускаются (pre-flight не рендерит их — это делает Render на старте
// прогона). Все assert true / scenario без assert → nil (большинство сценариев —
// no-op, как и требует пилот).
//
// NOT staged: pre-flight всегда не-staged (один проход, TaskPassage=nil), assert —
// run-level «один раз на прогон» по построению; passage-фильтр Render-а здесь не
// нужен (он защищает от повтора assert на каждом Passage staged-прохода, чего в
// pre-flight нет). nil-Scenario → ошибка (структурный отказ caller-а, как в Render).
func (p *Pipeline) EvalAsserts(ctx context.Context, in RenderInput) error {
	if in.Scenario == nil {
		return fmt.Errorf("render: scenario manifest is nil")
	}
	in.Ctx = ctx // assert.that[] может звать vault() — прокинуть отмену/таймаут
	// compute: доступен в assert.that[] так же, как в params/where (один резолв,
	// рун-уровневый контекст без soulprint). Идемпотентно с Render/RenderStateOps.
	computed, cerr := p.resolveCompute(in)
	if cerr != nil {
		return cerr
	}
	in.Compute = computed
	for i := range in.Scenario.Tasks {
		task := in.Scenario.Tasks[i]
		if !IsAssertTask(task) {
			continue
		}
		if err := p.evalAssertTask(in, task); err != nil {
			return err
		}
	}
	return nil
}

// evalAssertTask вычисляет assert-задачу (ADR-009 amendment 2026-06-23) —
// keeper-side render-time precondition прогона. RUN-LEVEL (один раз, не per-host):
// проверяет инвариант топологии прогона, а не per-host-предикат.
//
// Гейт `when:` соблюдён: если when статический (register-/soulprint-независимый,
// isStaticWhen) и вычислился в false — assert НЕ вычисляется (placeholder-skip
// неактивной ветки, как у обычной задачи: cluster-assert на standalone-прогоне
// молчит). when пустой или статически-true → assert вычисляется. Non-static when
// (register/soulprint-зависимый) на assert вне pilot-объёма — assert run-level и
// register-карта неполна; такой when трактуется как «активен» (предикаты всё равно
// вычисляются), вырожденный кейс не валим, но в пилоте он не используется.
//
// Предикаты `that[]` вычисляются в ПОЛНОМ scenario-CEL-контексте, включая
// soulprint.hosts (AllowHosts=!destinyIsolated — как evalWhere/resolveTargets):
// run-level контекст строится hostVars-ом по ПЕРВОМУ хосту roster-а (self здесь
// не используется предикатами топологии; size(soulprint.hosts) host-инвариантен).
// Первый false → ErrAssertFailed (render обрывается ДО dispatch): текст = message
// (или дефолт) + индекс/текст непрошедшего предиката. Все true → nil (assert
// «исчезает» из плана, RenderedTask не эмитится — это делает caller через continue).
func (p *Pipeline) evalAssertTask(in RenderInput, task config.Task) error {
	// Гейт when: статически-false → assert не вычисляется (неактивный режим).
	if isStaticWhen(task.When) {
		renderHosts := in.Hosts
		if len(renderHosts) == 0 {
			renderHosts = []*topology.HostFacts{{}}
		}
		fc, err := buildFlowContext(in, renderHosts[0], hostVars(in, renderHosts[0], len(in.Hosts)), len(in.Hosts))
		if err != nil {
			return fmt.Errorf("render: assert %q: when flow_context: %w", task.Name, err)
		}
		pass, err := evalStaticWhen(task.When, fc)
		if err != nil {
			return fmt.Errorf("render: assert %q: static-when %q: %w", task.Name, task.When, err)
		}
		if !pass {
			return nil // when:false — assert неактивен (placeholder-skip-семантика).
		}
	}

	// Run-level контекст: первый хост roster-а (или синтетический пустой при пустом
	// roster). soulprint.hosts проецируется из in.Hosts (AllowHosts=true в scenario-
	// проходе), size(soulprint.hosts) host-инвариантен — выбор первого хоста для self
	// на результат топологических предикатов не влияет.
	host := &topology.HostFacts{}
	if len(in.Hosts) > 0 {
		host = in.Hosts[0]
	}
	vars := hostVars(in, host, len(in.Hosts))
	vars, err := resolveTaskVars(p.cel, fileVarsForHost(in, host), task.Vars, vars)
	if err != nil {
		return fmt.Errorf("render: assert %q: %w", task.Name, err)
	}

	for i, pred := range task.Assert.That {
		ok, err := evalBoolExpr(p.cel, "assert.that", pred, vars)
		if err != nil {
			return fmt.Errorf("render: assert %q: %w", task.Name, err)
		}
		if !ok {
			return fmt.Errorf("%w: %s (предикат that[%d] %q вычислился в false)", ErrAssertFailed, assertMessage(task), i, pred)
		}
	}
	return nil
}

// assertMessage — человекочитаемое сообщение провала assert: авторский message
// или дефолт по имени задачи (если message опущен).
func assertMessage(task config.Task) string {
	if task.Assert != nil && task.Assert.Message != "" {
		return task.Assert.Message
	}
	if task.Name != "" {
		return fmt.Sprintf("assert %q не прошёл", task.Name)
	}
	return "assert-предикат не прошёл"
}

// renderKeeperTask рендерит keeper-side задачу (`on: keeper`, docs/keeper/
// modules.md): params вычисляются ОДИН раз в keeper-контексте (keeperVars — без
// per-host soulprint), потому что хостов нет — шаг исполняется на самом keeper-
// инстансе. host-инвариантной сверки нет (single keeper-target).
//
// Пилот: keeper-задача — module-only (apply:/loop:/block: на ней отвергнуты выше
// guardPilotDSL/здесь). core.file.rendered keeper-side не бывает (это Soul-side
// модуль), поэтому render_context/template_content НЕ собираются. flow_context не
// строится: keeper-задача исполняется scenario-runner-ом локально, который
// flow-control-предикаты (when/changed_when/failed_when) пока не вычисляет —
// поля протягиваются как CEL-строки для симметрии RenderedTask, но keeper-исполнитель
// MVP их игнорирует (как Soul до их интеграции). register: протягивается —
// keeper-исполнитель аккумулирует register этой задачи под KeeperTargetSID.
func (p *Pipeline) renderKeeperTask(ctx context.Context, in RenderInput, task config.Task, idx int) (*RenderedTask, error) {
	if task.Apply != nil {
		return nil, fmt.Errorf("%w: apply: на keeper-side задаче (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
	}
	if task.Loop != nil {
		return nil, fmt.Errorf("%w: loop: на keeper-side задаче (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
	}

	resolved, err := resolveVaultRefs(ctx, p.vault, task.Module.Params)
	if err != nil {
		return nil, fmt.Errorf("render: keeper task %q: %w", task.Name, err)
	}

	// seal / sealed-paths ([ADR-010] §7.4): keeper-side задача (core.vault.kv-read
	// и пр.) тоже может нести `${ vault(...) }`/`${ input.<secret> }` в params.
	collectSealed(p.cel, in.Sealed, task.Module.Params, scenarioSealSources(in), "")

	vars := keeperVars(in)
	// keeper-side задача — не destiny-проход (destiny-tasks все Soul-side в пилоте);
	// file-vars базы нет (DestinyVarsResolved nil вне renderApplyDestiny).
	vars, err = resolveTaskVars(p.cel, nil, task.Vars, vars)
	if err != nil {
		return nil, fmt.Errorf("render: keeper task %q: %w", task.Name, err)
	}
	st, err := renderParams(p.cel, resolved, vars)
	if err != nil {
		return nil, fmt.Errorf("render: keeper task %q: %w", task.Name, err)
	}

	rt := &RenderedTask{
		Index:          idx,
		Name:           task.Name,
		Module:         task.Module.Module,
		Params:         st,
		Register:       task.Register,
		ID:             task.ID,
		NoLog:          task.NoLog,
		Timeout:        task.Timeout,
		When:           task.When,
		ChangedWhen:    task.ChangedWhen,
		FailedWhen:     task.FailedWhen,
		onChangesNames: task.OnChanges,
		onFailNames:    task.OnFail,
	}
	if task.Retry != nil {
		rt.RetryCount = task.Retry.Count
		rt.RetryDelay = task.Retry.Delay
		rt.Until = task.Retry.Until
	}
	return rt, nil
}

// reFlowControlSoulprint ловит ссылку на soulprint в любом flow-control-предикате
// (when/changed_when/failed_when). Стиль и форма — как reLoopWhenSoulprint
// (loop.go): один regex по границе слова, fail-closed на multi-host.
var reFlowControlSoulprint = regexp.MustCompile(`\bsoulprint\b`)

// flowControlEngine — общий Soul-side flow-control-движок ([cel.NewFlowControl],
// ADR-012(d)) для Keeper-side static-when placeholder-skip. КРИТИЧНО: это та же
// sandbox-песочница, что Soul использует для evalWhen (applyrunner.go) — не
// full Keeper-env. Гарантия бит-в-бит эквивалентности static-when-false на Keeper
// и when-false на Soul (один и тот же env, один и тот же flow_context). Движок
// потокобезопасен (compile-cache под RWMutex) и переиспользуется всеми прогонами;
// собирается лениво один раз (паттерн rbac/soulprint, statepredicate) — конструктор
// от рантайма не зависит, но строить его в init() — платить на каждый импорт.
var (
	flowControlEngineOnce sync.Once
	flowControlEngineInst *cel.Engine
	flowControlEngineErr  error
)

func flowControlEngine() (*cel.Engine, error) {
	flowControlEngineOnce.Do(func() {
		flowControlEngineInst, flowControlEngineErr = cel.NewFlowControl()
	})
	return flowControlEngineInst, flowControlEngineErr
}

// isStaticWhen сообщает, можно ли вычислить предикат when: Keeper-side ДО рендера
// params (placeholder-skip, ADR-012(d), Вариант b). Статическим считается непустой
// when, который НЕ зависит от register.* (результатов предыдущих задач, известных
// только Soul-у) и НЕ зависит от soulprint (host-вариативного слоя). Такой предикат
// детерминирован на Keeper-е из flow_context (input/vars/essence/incarnation), и
// его false-исход одинаков на всех хостах прогона.
//
// Переиспользует канонические парсеры (без дубля regex):
//   - config.ExtractRegisterRefs (shared/config/task_refs.go) — register-ссылки;
//     любая register.<name> (кроме register.self, которой во when по семантике нет
//     для gating) делает when register-зависимым → НЕ статический;
//   - reFlowControlSoulprint (guardFlowControlHostInvariant) — soulprint host-
//     вариативен (soulprint.self), исключаем из «статического».
//
// Пустой when → false (нет предиката — нечего вычислять Keeper-side; задача
// безусловна, идёт обычным путём с рендером params). Смешанный when
// (register+input) → НЕ статический (есть register-ссылка) — остаётся Soul-side.
func isStaticWhen(when string) bool {
	// Допущение: register-зависимость детектится только по точечной форме
	// register.<name> (ExtractRegisterRefs). Bracket-форма register["x"] во when вне
	// поддержки — симметрично checkPredicateRefs в config-валидаторе. Для when это
	// латентно (probe-register всегда точечный).
	if when == "" {
		return false
	}
	if len(config.ExtractRegisterRefs(when)) != 0 {
		return false
	}
	if reFlowControlSoulprint.MatchString(when) {
		return false
	}
	return true
}

// evalStaticWhen вычисляет статический when: Keeper-side через тот же flow-control-
// движок и тот же flow_context, что поедут на Soul (evalWhen). Возвращает результат
// предиката. Вызывается ТОЛЬКО для when, прошедшего isStaticWhen (register-/
// soulprint-независимый) — register в активации пуст, и его пустота не влияет на
// исход. Бит-в-бит эквивалентно Soul-side evalWhen (один env, один flow_context).
//
// Ошибка eval (например, no-such-key на отсутствующем input) пробрасывается caller-у:
// Keeper падает с render_failed на битом статическом предикате так же, как Soul упал
// бы на нём в evalWhen — расхождения поведения нет, ошибка автора видна раньше.
func evalStaticWhen(when string, fc *structpb.Struct) (bool, error) {
	engine, err := flowControlEngine()
	if err != nil {
		return false, fmt.Errorf("static-when: сборка flow-control-движка: %w", err)
	}
	// Активация — flowControlVars Soul-side формы (flow_context + пустой register).
	// register-карта пуста: isStaticWhen уже гарантировал отсутствие register.* в
	// when, поэтому пустота register на исход не влияет (симметрия с Soul, где
	// register для register-независимого when тоже не читается).
	return engine.EvalPredicate(when, flowControlVarsFromStruct(fc, nil))
}

// flowControlVarsFromStruct распаковывает flow_context-снапшот в cel.Vars Soul-side
// формы — точное зеркало soul/internal/runtime.flowControlVars (applyrunner.go),
// чтобы static-when на Keeper биндился ТЕМИ ЖЕ именами, что evalWhen на Soul.
// register передаётся отдельно (nil для static-when — register-независимый предикат).
// nil/отсутствующие секции → пустые map (штатный CEL no-such-key, не паника).
func flowControlVarsFromStruct(flowCtx *structpb.Struct, register map[string]any) cel.Vars {
	fc := map[string]any{}
	if flowCtx != nil {
		fc = flowCtx.AsMap()
	}
	flowSection := func(key string) map[string]any {
		if sec, ok := fc[key].(map[string]any); ok {
			return sec
		}
		return map[string]any{}
	}
	return cel.Vars{
		Input:         flowSection("input"),
		Vars:          flowSection("vars"),
		Essence:       flowSection("essence"),
		Incarnation:   flowSection("incarnation"),
		SoulprintSelf: flowSection(flowContextSelfKey),
		Register:      register,
		// AllowHosts намеренно false: NewFlowControl форсит изоляцию soulprint.hosts.
	}
}

// guardFlowControlHostInvariant отвергает host-вариативный flow-control-предикат
// (when/changed_when/failed_when со ссылкой на soulprint.self) на multi-host
// таргете. dispatch-модель pilot раздаёт ОДИН RenderedTask (с flow_context первого
// хоста) на всю targeted-группу — поэтому такой предикат молча вычислился бы по
// фактам первого хоста для ВСЕХ хостов. Fail-closed: вместо тихого неверного
// результата — явная ошибка о горизонте pilot.
//
// Single-host (len==1): flow_context.self корректен для единственного хоста →
// soulprint.self в предикате допустим (golden-path redis single-host). Multi-host
// с host-ИНВАРИАНТНЫМ предикатом (register.*/input.*/essence.*/incarnation.*) →
// OK: один на всю группу — корректно.
//
// Обобщён сразу на три поля: changed_when/failed_when тиражируются следующим
// slice, ошибка паттерна не должна размножиться.
// hasFlowControl сообщает, задан ли у задачи хоть один непустой flow-control-
// предикат (when/changed_when/failed_when). Гейт второго контура fail-closed
// (flowContextHostInvariant): без предиката Soul flow_context не читает, его
// host-вариативность безразлична.
func hasFlowControl(task config.Task) bool {
	return task.When != "" || task.ChangedWhen != "" || task.FailedWhen != ""
}

func guardFlowControlHostInvariant(task config.Task, targeted []*topology.HostFacts) error {
	if len(targeted) <= 1 {
		return nil
	}
	for _, p := range []struct{ kind, expr string }{
		{"when", task.When},
		{"changed_when", task.ChangedWhen},
		{"failed_when", task.FailedWhen},
	} {
		if reFlowControlSoulprint.MatchString(p.expr) {
			return fmt.Errorf(
				"render: task %q: %s %q — host-вариативный flow-control-предикат (soulprint.self) на multi-host таргете не поддержан в pilot — per-host dispatch отложен (отдельный ADR)",
				task.Name, p.kind, p.expr)
		}
	}
	return nil
}

// paramsHostInvariant сверяет params двух хостов на host-инвариантность,
// ИСКЛЮЧАЯ per-host-ожидаемые ключи core.file.rendered: template_content (его
// инжектит injectTemplateContent один раз после цикла) и render_context (он
// per-host по построению — несёт self конкретного хоста, templating.md §3.2).
// Для прочих ключей — точная proto-сверка (pilot-ограничение «один RenderedTask
// на task», см. Render): self-зависимый ШАБЛОН легитимен (его контекст уезжает
// в per-host render_context), self-зависимые ПРОЧИЕ params — нет.
func paramsHostInvariant(a, b *structpb.Struct) bool {
	return proto.Equal(stripPerHostKeys(a), stripPerHostKeys(b))
}

// stripPerHostKeys возвращает поверхностную копию struct без per-host-ключей
// (template_content/render_context). Исходный struct не мутируется (Fields-map
// шарит значения read-only — для proto.Equal этого достаточно).
func stripPerHostKeys(s *structpb.Struct) *structpb.Struct {
	if s == nil || s.Fields == nil {
		return s
	}
	out := &structpb.Struct{Fields: make(map[string]*structpb.Value, len(s.Fields))}
	for k, v := range s.Fields {
		if k == paramTemplateContent || k == paramRenderContext {
			continue
		}
		out.Fields[k] = v
	}
	return out
}

// flowContextHostInvariant сверяет flow_context двух хостов на host-инвариантность
// ВТОРЫМ контуром fail-closed (первый — guardFlowControlHostInvariant по тексту
// предиката). proto-сверка снапшотов с вычетом ТОЛЬКО ключа `self`.
//
// flow_context = {input, vars, essence, incarnation, self} (buildFlowContext).
// input/essence/incarnation host-ИНВАРИАНТНЫ по построению (общий контекст
// прогона); self host-ВАРИАНТЕН ВСЕГДА (per-host факты) и закрыт отдельным
// regex-guard на текст предиката — поэтому вычитается из сверки. Остаётся vars:
// task-level `vars:` МОГУТ быть host-вариантны (если значение производно от
// soulprint.self), и тогда текст предиката `vars.<key>` НЕ содержит soulprint —
// regex-guard его не ловит. Этот контур ловит именно vars-laundering.
//
// Инвариант: register в flow_context НЕ кладётся (Soul строит его сам из
// результатов предыдущих задач, см. buildFlowContext); если в будущем добавят —
// его тоже вычитать из сверки (host-вариативен по природе, как self).
func flowContextHostInvariant(a, b *structpb.Struct) bool {
	return proto.Equal(stripSelfKey(a), stripSelfKey(b))
}

// stripSelfKey возвращает поверхностную копию struct без ключа `self` (по образцу
// stripPerHostKeys, но вырезает ровно один ключ). Исходный struct не мутируется.
func stripSelfKey(s *structpb.Struct) *structpb.Struct {
	if s == nil || s.Fields == nil {
		return s
	}
	out := &structpb.Struct{Fields: make(map[string]*structpb.Value, len(s.Fields))}
	for k, v := range s.Fields {
		if k == flowContextSelfKey {
			continue
		}
		out.Fields[k] = v
	}
	return out
}

// extractParamsVars достаёт CEL-rendered значение params.vars как map[string]any
// для render_context.vars (templating.md §3.2/§6). Отсутствует/не-объект → nil
// (buildRenderContext подставит пустой map). Источник — структура, уже
// прошедшая renderParams, поэтому это просто чтение поля.
func extractParamsVars(st *structpb.Struct) map[string]any {
	if st == nil || st.Fields == nil {
		return nil
	}
	v, ok := st.Fields[paramVars]
	if !ok {
		return nil
	}
	sv, ok := v.GetKind().(*structpb.Value_StructValue)
	if !ok {
		return nil
	}
	return sv.StructValue.AsMap()
}

// setRenderContext кладёт собранный render-context в params под ключ
// render_context (structpb-конвертация {vars,self,role,essence}).
func setRenderContext(st *structpb.Struct, rc map[string]any) error {
	rcStruct, err := structpb.NewStruct(rc)
	if err != nil {
		return fmt.Errorf("render_context → structpb: %w", err)
	}
	if st.Fields == nil {
		st.Fields = map[string]*structpb.Value{}
	}
	st.Fields[paramRenderContext] = structpb.NewStructValue(rcStruct)
	return nil
}

// RenderStateChanges рендерит `state_changes` сценария в map field→value для
// коммита set-операций (orchestration.md §7.1). Совместимость: возвращает ту же
// плоскую map, что и раньше — проекцию ТОЛЬКО set-операций (старой map-формы
// `sets:` и новой list-формы `- set:`). add-операции в эту проекцию НЕ входят:
// они требуют упорядоченного применения к промежуточному state (см.
// [Pipeline.RenderStateOps] / scenario.mergeStateChanges). Сохранена ради
// trial-ассерта `assert.state_changes` (поле→значение) и существующих
// state-merge unit-тестов.
//
// Реализована поверх RenderStateOps: рендерит весь упорядоченный список, затем
// проецирует set-операции в map (последняя по порядку перезаписывает раннюю).
func (p *Pipeline) RenderStateChanges(in RenderInput) (map[string]any, error) {
	ops, err := p.RenderStateOps(in)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(ops))
	for i := range ops {
		if ops[i].Verb == config.VerbSet {
			out[ops[i].Field] = ops[i].Value
		}
	}
	return out, nil
}

// RenderStateOps рендерит упорядоченный список операций `state_changes` сценария
// (orchestration.md §7, новая list-форма) в []RenderedOp с уже вычисленными
// Keeper-side значениями. Вызывается после барьера (run.go), отдельно от Render.
//
// Контекст CEL — input/incarnation/soulprint.self + register этого хоста (слайс
// 2: register probe-задач прогона, in.RegisterByHost[sid]); vars/essence/state/
// soulprint.hosts — future полной грамматики, в пилоте недоступны.
//
// Cross-host свёртка — last-wins по сортировке SID: Value (и для map-add — Key)
// каждой операции вычисляются на каждом хосте по порядку SID, поздний
// перезаписывает раннего (детерминизм, output.md). Match-предикат list-дедупа
// НЕ вычисляется здесь — он протягивается как строка и применяется merge-ом
// per-existing-элемент (зависит от каждого элемента state, не от хоста).
//
// Две формы StateChanges:
//   - list-форма (IsList): операции из sc.Ops в порядке объявления;
//   - map-форма (DEPRECATED): sc.Sets → set-операции (порядок недетерминирован,
//     но set-семантика field-overwrite от порядка не зависит — last-wins на поле).
//
// nil/пустой блок → nil. Пустой in.Hosts → nil (некому вычислять; caller run.go
// уже отверг прогон без хостов).
func (p *Pipeline) RenderStateOps(in RenderInput) ([]RenderedOp, error) {
	if in.Scenario == nil {
		return nil, fmt.Errorf("render: scenario manifest is nil")
	}
	sc := in.Scenario.StateChanges
	if sc == nil {
		return nil, nil
	}

	hosts := sortedHostsBySID(in.Hosts)
	if len(hosts) == 0 {
		return nil, nil
	}

	// compute: резолвится так же, как в Render (один раз, рун-уровневый контекст без
	// soulprint) — RenderStateOps зовётся отдельно после барьера (run.go) с тем же
	// RenderInput, но без предыдущего Render-прохода. Идемпотентно: если caller уже
	// заполнил in.Compute, resolveCompute вернёт его как есть. Так `compute.<name>`
	// в state_changes даёт ТО ЖЕ значение, что в apply.input (drift-guard снят compute).
	computed, cerr := p.resolveCompute(in)
	if cerr != nil {
		return nil, cerr
	}
	in.Compute = computed

	if sc.IsList {
		return p.renderStateOpsList(in, hosts, sc.Ops)
	}
	return p.renderStateOpsLegacy(in, hosts, sc.Sets)
}

// renderStateOpsLegacy рендерит старую map-форму `sets:` в set-операции. Каждое
// поле — CEL-выражение, last-wins cross-host. Имена полей детерминированно
// сортируются для стабильного порядка операций (set-семантика от порядка не
// зависит, но детерминизм важен для логов/сверки).
func (p *Pipeline) renderStateOpsLegacy(in RenderInput, hosts []*topology.HostFacts, sets map[string]string) ([]RenderedOp, error) {
	if len(sets) == 0 {
		return nil, nil
	}
	fields := make([]string, 0, len(sets))
	for f := range sets {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	out := make([]RenderedOp, 0, len(fields))
	for _, field := range fields {
		var val any
		for _, h := range hosts {
			v, err := p.cel.EvalInterpolation(sets[field], stateChangesVars(in, h))
			if err != nil {
				return nil, fmt.Errorf("render: state_changes.sets.%s (host %s): %w", field, h.SID, err)
			}
			val = v // last-wins по SID
		}
		out = append(out, RenderedOp{Verb: config.VerbSet, Field: field, Value: val})
	}
	return out, nil
}

// renderStateOpsList рендерит новую list-форму: каждую операцию по порядку.
//
//   - set/add: Value (произвольное YAML с CEL-ячейками) и map-add Key рендерятся
//     per-host last-wins; Match list-дедупа протягивается строкой (merge-time);
//   - modify/remove: Match/Patch протягиваются КАК ЕСТЬ (вычисляются merge-time
//     per-element — зависят от каждого элемента state). К RenderedOp прикладывается
//     per-RUN снимок scenario-контекста (Context, last-wins по SID) — он нужен
//     merge-time, т.к. match `key == input.username` / patch `${ input.acl }`
//     видят полный sets-контекст (ADR-057 §b);
//   - foreach: render-time fan-out — итерируем по элементам CEL-коллекции,
//     каждую вложенную do-операцию рендерим с активным биндингом `as`-имени.
//     Раскрывается в N RenderedOp (по числу элементов × числу do-операций).
func (p *Pipeline) renderStateOpsList(in RenderInput, hosts []*topology.HostFacts, ops []config.StateChange) ([]RenderedOp, error) {
	out := make([]RenderedOp, 0, len(ops))
	for i := range ops {
		op := ops[i]
		if op.Verb == config.VerbForeach {
			expanded, err := p.renderForeach(in, hosts, op, i, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, expanded...)
			continue
		}
		ro, err := p.renderOneStateOp(in, hosts, op, i, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, ro)
	}
	return out, nil
}

// renderOneStateOp рендерит ОДНУ не-foreach операцию (set/add/modify/remove).
// loopBind — биндинг текущей foreach-итерации (`as`-имя → элемент), nil вне
// foreach; он подмешивается в CEL-контекст всех ячеек value/key/patch/match.
func (p *Pipeline) renderOneStateOp(in RenderInput, hosts []*topology.HostFacts, op config.StateChange, idx int, loopBind map[string]any) (RenderedOp, error) {
	ro := RenderedOp{Verb: op.Verb, Field: op.Field, Match: op.Match, OnConflict: op.OnConflict, Expect: op.Expect}

	// modify/remove: Match/Patch вычисляются merge-time per-element. Здесь —
	// только снимок per-RUN scenario-контекста (last-wins по SID) + подстановка
	// foreach-биндинга в Patch-строки (он render-time, see renderForeach).
	if op.Verb == config.VerbModify || op.Verb == config.VerbRemove {
		ctx := p.stateContextSnapshot(in, hosts, loopBind)
		ro.Context = ctx
		if op.Verb == config.VerbModify {
			patch, err := patchMapFromAny(op.Patch)
			if err != nil {
				return RenderedOp{}, fmt.Errorf("render: state_changes[%d] modify %q: %w", idx, op.Field, err)
			}
			ro.Patch = patch
		}
		// foreach-биндинг в match/patch резолвится merge-time через Context
		// (loopBind влит в snapshot), match/patch остаются строками. Match с
		// `as`-именем (например `elem.sid == replica`) увидит replica из Context.
		return ro, nil
	}

	// set/add: Value (рекурсивный CEL) + map-add Key — per-host last-wins.
	var val any
	var key string
	for _, h := range hosts {
		vars := stateChangesVars(in, h)
		vars.Loop = mergeLoop(vars.Loop, loopBind)
		v, err := renderValue(p.cel, op.Value, vars, fmt.Sprintf("state_changes[%d].value", idx))
		if err != nil {
			return RenderedOp{}, fmt.Errorf("render: state_changes[%d] %s %q (host %s): %w", idx, op.Verb, op.Field, h.SID, err)
		}
		val = v // last-wins по SID
		if op.Key != "" {
			kv, kerr := p.cel.EvalInterpolation(op.Key, vars)
			if kerr != nil {
				return RenderedOp{}, fmt.Errorf("render: state_changes[%d].key %q (host %s): %w", idx, op.Key, h.SID, kerr)
			}
			key = fmt.Sprint(kv)
		}
	}
	ro.Value = val
	ro.Key = key
	// add внутри foreach: match-предикат list-дедупа может ссылаться на `as`-имя
	// (ADR-057 пример add_replicas: `match: "elem == sid"`). Чистый add-match
	// (EvalStateMatch) видит только elem/value; чтобы `sid` резолвился merge-time,
	// прикладываем foreach-биндинг как Context — merge берёт context-aware
	// вычислитель, если Context != nil (findListMatch). Вне foreach Context=nil →
	// прежний чистый add-match elem/value.
	if op.Verb == config.VerbAdd && len(loopBind) > 0 {
		ro.Context = loopBind
	}
	return ro, nil
}

// renderForeach раскрывает foreach в render-фазе: вычисляет CEL-коллекцию op.In,
// итерирует по элементам (list → as=элемент; map → as=объект-запись с .key/.value),
// и для каждого элемента рендерит все do-операции с активным биндингом as-имени.
// Раскрывается в N×M RenderedOp (N элементов × M do-операций).
//
// ★ Форма биндинга (ADR-057 §3, ЗАФИКСИРОВАНА):
//   - foreach по LIST → `as`=ЭЛЕМЕНТ как есть. Для list of scalars `${replica}`
//     даёт скаляр; для list of objects `${replica.sid}` — поле объекта.
//   - foreach по MAP → `as`=ОБЪЕКТ-ЗАПИСЬ {key, value}: `${change.key}` —
//     ключ записи, `${change.value.acl}` — поле значения. Это симметрично
//     register-карте (sid→payload) и migration-DSL foreach по map.
//
// Коллекция op.In вычисляется ОДИН раз (host-инвариантна в пилоте: foreach по
// input.*/vars.* — общий контекст прогона; foreach по soulprint.self.* был бы
// host-вариативен и здесь не поддержан — снимок последнего хоста по SID, как и
// прочая last-wins-свёртка). nestedBind — биндинг внешнего foreach (вложенность
// грамматикой запрещена, но параметр для симметрии).
func (p *Pipeline) renderForeach(in RenderInput, hosts []*topology.HostFacts, op config.StateChange, idx int, nestedBind map[string]any) ([]RenderedOp, error) {
	// Коллекция вычисляется в контексте последнего хоста по SID (last-wins).
	last := hosts[len(hosts)-1]
	vars := stateChangesVars(in, last)
	vars.Loop = mergeLoop(vars.Loop, nestedBind)
	collVal, err := p.cel.EvalInterpolation(op.In, vars)
	if err != nil {
		return nil, fmt.Errorf("render: state_changes[%d].foreach %q: %w", idx, op.In, err)
	}

	binds, err := foreachBindings(op.As, collVal)
	if err != nil {
		return nil, fmt.Errorf("render: state_changes[%d].foreach %q: %w", idx, op.In, err)
	}

	out := make([]RenderedOp, 0, len(binds)*len(op.Do))
	for _, bind := range binds {
		merged := mergeLoop(nestedBind, bind)
		for di := range op.Do {
			sub := op.Do[di]
			ro, derr := p.renderOneStateOp(in, hosts, sub, idx, merged)
			if derr != nil {
				return nil, fmt.Errorf("render: state_changes[%d].foreach.do[%d]: %w", idx, di, derr)
			}
			out = append(out, ro)
		}
	}
	return out, nil
}

// foreachBindings строит per-iteration биндинги `as`-имени из вычисленной
// коллекции. ★ list → as=элемент; map → as={key, value}-запись (ADR-057 §3).
// Порядок map-итерации детерминирован (сортировка ключей) — воспроизводимость
// state-коммита. Не list/не map (скаляр/nil) → ошибка: foreach требует коллекцию.
func foreachBindings(asName string, coll any) ([]map[string]any, error) {
	switch c := coll.(type) {
	case []any:
		out := make([]map[string]any, 0, len(c))
		for _, elem := range c {
			out = append(out, map[string]any{asName: elem})
		}
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]map[string]any, 0, len(c))
		for _, k := range keys {
			// as=объект-запись: .key (строка) + .value (значение записи).
			out = append(out, map[string]any{asName: map[string]any{"key": k, "value": c[k]}})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("foreach: выражение дало %T, ожидался list или map", coll)
	}
}

// patchMapFromAny приводит произвольное YAML-значение patch к map[string]any
// (путь-в-элементе → CEL/литерал). nil → пустой map (no-op merge). Не-map → ошибка
// (config-валидатор уже отверг, defense in depth).
func patchMapFromAny(v any) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("patch должен быть map путь→значение, получено %T", v)
	}
	return m, nil
}

// stateContextSnapshot строит per-RUN снимок scenario-контекста (last-wins по
// SID) как плоскую map для merge-time вычисления modify/remove match/patch.
// Содержит input/register/incarnation/self/essence/vars + влитый foreach-биндинг
// (loopBind). Берётся контекст ПОСЛЕДНЕГО хоста по SID — register/self
// host-вариативны, last-wins (output.md, симметрично set/add).
func (p *Pipeline) stateContextSnapshot(in RenderInput, hosts []*topology.HostFacts, loopBind map[string]any) map[string]any {
	last := hosts[len(hosts)-1]
	vars := stateChangesVars(in, last)
	ctx := map[string]any{}
	putIfSet := func(name string, m map[string]any) {
		if m != nil {
			ctx[name] = m
		}
	}
	putIfSet("input", vars.Input)
	putIfSet("register", vars.Register)
	putIfSet("incarnation", vars.Incarnation)
	putIfSet("essence", vars.Essence)
	putIfSet("vars", vars.Vars)
	putIfSet("compute", vars.Compute)
	if vars.SoulprintSelf != nil {
		ctx["soulprint"] = map[string]any{"self": vars.SoulprintSelf}
	}
	for k, v := range loopBind {
		ctx[k] = v
	}
	return ctx
}

// mergeLoop сливает два loop-биндинга (внешний + текущий) в новую map. Текущий
// (b) перекрывает внешний (a) при коллизии имён. nil-аргументы безопасны.
func mergeLoop(a, b map[string]any) map[string]any {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// EvalStateMatch вычисляет match-предикат идентичности list-элемента `add`-
// операции (orchestration.md §7, new list-форма). Биндинги: `elem` —
// существующий элемент коллекции, `value` — добавляемый (уже отрендеренный).
// Оба кладутся как top-level CEL-имена (через Vars.Loop — тот же механизм
// loop-переменных). Прочий scenario-контекст (input/register/…) в match НЕ
// доступен: идентичность — чистая функция от elem+value (как migration-CEL —
// чистая функция от state, ADR-019). Пустой predicate сюда не приходит (merge
// при пустом match сравнивает элементы deep-equal-ом без CEL).
//
// Результат обязан быть bool (evalBoolExpr). Вызывается merge-ом per-existing-
// элемент — этот метод stateless относительно Pipeline (cel.Engine потокобезопасен).
func (p *Pipeline) EvalStateMatch(predicate string, elem, value any) (bool, error) {
	vars := cel.Vars{Loop: map[string]any{"elem": elem, "value": value}}
	return evalBoolExpr(p.cel, "state_changes.add.match", predicate, vars)
}

// EvalStateOpExpr — merge-time вычислитель CEL для modify/remove (см.
// [StateOpEvalFunc]). В отличие от EvalStateMatch (изолированный elem/value для
// add-дедупа), здесь expr видит ПОЛНЫЙ scenario-контекст прогона (ctx — снимок
// input/register/incarnation/soulprint.self/essence/vars, собранный
// stateContextSnapshot) ПЛЮС биндинги текущего элемента (binds — elem/key/value).
// boolOut=true → match-предикат (EvalExpression→bool); boolOut=false →
// patch-значение (EvalInterpolation→native). Так modify-match `key ==
// input.username` видит и key (элемент), и input.* (контекст).
//
// Вызывается merge-ом (scenario/trial) per-matched-элемент; stateless
// относительно Pipeline (cel.Engine потокобезопасен).
func (p *Pipeline) EvalStateOpExpr(expr string, ctx, binds map[string]any, boolOut bool) (any, error) {
	vars := stateOpVars(ctx)
	vars.Loop = mergeLoop(vars.Loop, binds)
	if boolOut {
		return evalBoolExpr(p.cel, "state_changes.match", expr, vars)
	}
	return p.cel.EvalInterpolation(expr, vars)
}

// stateOpVars распаковывает плоский ctx-снимок (stateContextSnapshot) обратно в
// cel.Vars для merge-time-вычисления modify/remove. Симметрично stateChangesVars,
// но источник — уже собранный per-RUN snapshot (хост-резолв сделан на render-
// стороне), а не топология. soulprint.self достаётся из вложенного soulprint-map.
func stateOpVars(ctx map[string]any) cel.Vars {
	asMap := func(k string) map[string]any {
		m, _ := ctx[k].(map[string]any)
		return m
	}
	v := cel.Vars{
		Input:       asMap("input"),
		Register:    asMap("register"),
		Incarnation: asMap("incarnation"),
		Essence:     asMap("essence"),
		Vars:        asMap("vars"),
		Compute:     asMap("compute"),
	}
	if sp, ok := ctx["soulprint"].(map[string]any); ok {
		if self, ok := sp["self"].(map[string]any); ok {
			v.SoulprintSelf = self
		}
	}
	// foreach-биндинг (as-имя) лежит в ctx как top-level ключ — переносим в Loop.
	loop := map[string]any{}
	for k, val := range ctx {
		switch k {
		case "input", "register", "incarnation", "essence", "vars", "compute", "soulprint":
			continue
		}
		loop[k] = val
	}
	if len(loop) > 0 {
		v.Loop = loop
	}
	return v
}

// guardPilotDSL отвергает task-ключи вне pilot-объёма явной [ErrUnsupportedDSL],
// а не silent-skip-ом. config-валидатор уже гарантирует структурную
// корректность; здесь — граница реализации pilot-а.
//
// apply: destiny здесь НЕ отвергается — он раскрывается изолированным
// render-проходом (renderApplyDestiny, V2). include: тоже НЕ отвергается как
// «вне pilot» — он раскрывается ДО render (config.ExpandIncludes); если всё же
// дошёл до render нераскрытым — это ErrUnexpandedInclude (баг раскрытия).
// serial:/run_once: тоже НЕ отвергаются (slice D): run_once режет таргет в
// resolveTargets, serial вычисляет ширину волны в DispatchPlan.
//
// loop: на MODULE-задаче (slice E1) НЕ отвергается — он раскрывается в
// render-фазе (renderLoopTask: одна задача → N RenderedTask по элементам
// items). loop: на include/apply/block по-прежнему вне pilot-объёма (config-
// валидатор отвергает loop на не-module-задаче раньше; здесь — defense in depth
// для apply+loop, дошедшего до render).
//
// block: (pilot C1) здесь НЕ отвергается — он раскрывается render-time fan-out-ом
// (renderBlockTask, как loop/apply:destiny). parallel: на block по-прежнему вне
// pilot (case task.Parallel выше ловит блок с parallel:true до block-accept).
// guard оставлен для parallel:, нераскрытого include: и пустых задач.
func guardPilotDSL(task config.Task, idx int) error {
	switch {
	case task.Apply != nil:
		// module == nil допустим для apply-задачи (discriminator — apply).
		// loop: на apply отложен (slice E.later) — отвергаем явно.
		if task.Loop != nil {
			return fmt.Errorf("%w: loop: на apply-задаче (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
		}
		return nil
	case task.Include != nil:
		return fmt.Errorf("%w: (task[%d] %q)", ErrUnexpandedInclude, idx, task.Name)
	case task.Parallel:
		return fmt.Errorf("%w: parallel: (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
	case task.Block != nil:
		// block-задача валидна (pilot C1); module == nil допустим (discriminator —
		// block). loop: на block по-прежнему вне pilot — config-валидатор отвергает
		// loop на не-module-задаче раньше (defense in depth здесь не требуется,
		// renderBlockTask на block.Loop не смотрит).
		return nil
	case task.Module == nil:
		return fmt.Errorf("%w: task[%d] %q не является module-задачей", ErrUnsupportedDSL, idx, task.Name)
	}
	return nil
}

// taskPassageAt возвращает passage-индекс top-level задачи i из плана
// стратификации (RenderInput.TaskPassage). nil-план либо i вне диапазона → 0
// (N=1 / не-staged caller: Trial / Acolyte RenderForHost / CheckDrift) —
// поведение БИТ-В-БИТ как до staged-render. Index-out-of-range трактуем как 0
// fail-safe: лишний Passage-0 безопаснее паники на рассинхроне длины.
func taskPassageAt(plan []int, i int) int {
	if i < 0 || i >= len(plan) {
		return 0
	}
	return plan[i]
}

// stampPassage проставляет passage всем RenderedTask, добавленным текущей
// top-level задачей (tasks[from:]). Один вызов в конце каждой итерации Render
// клеймит и apply:destiny/loop-потомков (block — атомарная единица Passage,
// ADR-056), не размазывая стампинг по веткам.
func stampPassage(tasks []*RenderedTask, from, passage int) {
	if passage == 0 {
		return // zero-value уже стоит — не трогаем (fast-path N=1).
	}
	for i := from; i < len(tasks); i++ {
		tasks[i].Passage = passage
	}
}

// compile-time проверка, что *structpb.Struct — proto.Message (используется в
// proto.Equal). Если структура поменяется, поломка вылезет тут, а не в рантайме.
var _ proto.Message = (*structpb.Struct)(nil)
