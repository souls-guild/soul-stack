package render

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
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

	tasks := make([]*RenderedTask, 0, len(in.Scenario.Tasks))
	plans := make([]DispatchPlan, 0, len(in.Scenario.Tasks))
	idx := 0

	for i := range in.Scenario.Tasks {
		task := in.Scenario.Tasks[i]

		if err := guardPilotDSL(task, i); err != nil {
			return nil, nil, err
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

	resolved, err := resolveVaultRefs(ctx, p.vault, task.Module.Params)
	if err != nil {
		return nil, fmt.Errorf("render: task %q: %w", task.Name, err)
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

	isRendered := task.Module.Module == moduleFileRendered
	var firstSID string
	for hi, h := range renderHosts {
		vars := hostLoopVars(in, h, len(targeted), loopVars)
		vars, err = resolveTaskVars(p.cel, task.Vars, vars)
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

	vars := keeperVars(in)
	vars, err = resolveTaskVars(p.cel, task.Vars, vars)
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

// RenderStateChanges рендерит `state_changes.sets` сценария (поле → CEL-выражение)
// в map отрендеренных значений для коммита в incarnation.state (orchestration.md
// §7.1). Вызывается после барьера (run.go), отдельно от Render. Контекст sets —
// input/incarnation/soulprint.self + register этого хоста (слайс 2: register
// probe-задач прогона, in.RegisterByHost[sid], резолвнутый по register-имени);
// vars/essence/state/soulprint.hosts — future-расширение полной грамматики, в
// пилоте недоступны.
//
// Cross-host свёртка — last-wins по сортировке SID: для каждого поля sets берётся
// значение, вычисленное на хосте с лексикографически последним SID. Хосты прогона
// (in.Hosts) сортируются по SID; sets вычисляются для каждого по порядку, поле
// перезаписывается — побеждает последний. Это детерминированно (как «последняя
// запись побеждает» в output.md).
//
// Пустой/nil sets → пустой map (state не меняется). Пустой in.Hosts → пустой map
// (некому вычислять; caller — run.go — уже отверг прогон без хостов).
//
// appends/modifies (future, per-host коллекции) здесь НЕ обрабатываются.
func (p *Pipeline) RenderStateChanges(in RenderInput) (map[string]any, error) {
	if in.Scenario == nil {
		return nil, fmt.Errorf("render: scenario manifest is nil")
	}
	sc := in.Scenario.StateChanges
	if sc == nil || len(sc.Sets) == 0 {
		return map[string]any{}, nil
	}

	hosts := sortedHostsBySID(in.Hosts)
	if len(hosts) == 0 {
		return map[string]any{}, nil
	}

	out := make(map[string]any, len(sc.Sets))
	for _, h := range hosts {
		vars := stateChangesVars(in, h)
		for field, expr := range sc.Sets {
			val, err := p.cel.EvalInterpolation(expr, vars)
			if err != nil {
				return nil, fmt.Errorf("render: state_changes.sets.%s (host %s): %w", field, h.SID, err)
			}
			out[field] = val
		}
	}
	return out, nil
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
// для apply+loop, дошедшего до render). guard оставлен для слайса C (block/
// parallel) и пустых задач.
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
	case task.Block != nil:
		return fmt.Errorf("%w: block: (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
	case task.Parallel:
		return fmt.Errorf("%w: parallel: (task[%d] %q)", ErrUnsupportedDSL, idx, task.Name)
	case task.Module == nil:
		return fmt.Errorf("%w: task[%d] %q не является module-задачей", ErrUnsupportedDSL, idx, task.Name)
	}
	return nil
}

// compile-time проверка, что *structpb.Struct — proto.Message (используется в
// proto.Equal). Если структура поменяется, поломка вылезет тут, а не в рантайме.
var _ proto.Message = (*structpb.Struct)(nil)
