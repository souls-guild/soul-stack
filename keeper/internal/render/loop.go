package render

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// defaultLoopVar — имя loop-переменной по умолчанию (destiny/tasks.md §7,
// `as:` опущен).
const defaultLoopVar = "item"

// renderLoopTask раскрывает `loop:` на module-задаче в render-фазе (slice E1):
// одна задача → N RenderedTask по элементам items, со сквозными индексами
// (симметрично renderApplyDestiny). В отличие от config-splice include
// (раскрывается ДО render), loop раскрывается ИМЕННО здесь — items это
// CEL/template-выражение (`${ input.users }` / `${ vars.x }`), а CEL живёт
// только в render-фазе.
//
// Порядок (orchestration.md §2.2):
//  1. items резолвится один раз на прогон (host-инвариантный источник
//     input/vars; soulprint в контексте нет — items не должен зависеть от хоста).
//  2. array → as=элемент, index_as=0-based индекс; object → as=значение,
//     index_as=ключ, порядок итерации алфавитный по ключам (destiny/tasks.md §7).
//  3. when: — per-item truthy-фильтр в том же host-инвариантном контексте
//     (фильтр по содержимому элемента, без soulprint); отфильтрованная итерация
//     не порождает RenderedTask. Ссылка на soulprint в when: → ошибка
//     (host-вариативный when отложен вместе с per-host loop-фильтрацией).
//  4. каждая оставшаяся итерация рендерится renderTaskIter с loop-переменными;
//     host-инвариантность проверяется ПО-ИТЕРАЦИОННО (см. renderTaskIter).
//
// targeted — хосты задачи (после on:/where:/run_once: родителя); loop катится
// на каждом targeted-хосте (volna serial: ортогональна оси итераций — весь loop
// прокатывается на каждом хосте волны). SerialWidth наследуется всеми
// итерациями. Пустой результат (items пуст / when: отфильтровал всё) → 0 задач
// (валидный no-op).
func (p *Pipeline) renderLoopTask(
	ctx context.Context,
	in RenderInput,
	task config.Task,
	startIndex int,
	targeted []*topology.HostFacts,
) ([]*RenderedTask, []DispatchPlan, error) {
	asName := task.Loop.As
	if asName == "" {
		asName = defaultLoopVar
	}

	// Static-when-false loop-задача сюда НЕ доходит: оба call-site (scenario-цикл
	// pipeline.go, destiny.go) предваряются emitStaticWhenSkip, который для
	// статически-false loop эмитит N/1 skip-placeholder через loopStaticSkip и
	// делает continue ДО renderLoopTask. Поэтому здесь static-when уже гарантированно
	// активен (true) или отсутствует — повторный static-when-гейт был бы недостижим
	// и лишь дублировал per-host staticWhenSkips. renderLoopTask видит только
	// fan-out активной ветки.
	iters, err := resolveLoopItems(p.cel, in, task.Loop, asName)
	if err != nil {
		return nil, nil, fmt.Errorf("render: task %q loop: %w", task.Name, err)
	}

	width := serialWidth(task.Serial, len(targeted))
	tasks := make([]*RenderedTask, 0, len(iters))
	plans := make([]DispatchPlan, 0, len(iters))
	idx := startIndex
	for _, it := range iters {
		keep, werr := evalLoopWhen(p.cel, in, task.Loop.When, it)
		if werr != nil {
			return nil, nil, fmt.Errorf("render: task %q: %w", task.Name, werr)
		}
		if !keep {
			continue
		}

		rt, rerr := p.renderTaskIter(ctx, in, task, idx, targeted, it)
		if rerr != nil {
			return nil, nil, rerr
		}
		tasks = append(tasks, rt)
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(targeted),
			SerialWidth: width,
		})
		idx++
	}
	return tasks, plans, nil
}

// loopStaticSkip эмитит skip-placeholder(ы) для loop-задачи со статически-false
// when: (решение «резолвим→N / нерезолвим→1», architect):
//
//   - items РЕЗОЛВИТСЯ (host-инвариантный контекст) → N skip-placeholder по
//     итерациям со сквозными Index — ПАРИТЕТ с per-iter skip из renderTaskIter,
//     который раньше получался естественно (когда items резолвился, цикл крутил
//     renderTaskIter, каждый скипал params). Не схлопываем N→1, чтобы план/Index
//     совпадали с активной веткой (детерминизм по Passage).
//   - items НЕ резолвится (no-such-key/non-collection — типично absent optional-
//     input в неактивной ветке) → НЕ падаем (это и есть исходный баг), эмитим
//     ОДИН skip-placeholder за всю задачу. Soul вычислит тот же when:false по
//     flow_context → SKIPPED; loop при skipped-задаче не раскрывается.
//
// Все placeholder несут flow_context первого хоста (skip), Params=nil, When/ID/
// Register/Passage протянуты — как scenario static-when placeholder-skip.
func (p *Pipeline) loopStaticSkip(
	in RenderInput,
	task config.Task,
	startIndex int,
	targeted []*topology.HostFacts,
	asName string,
	skip *structpb.Struct,
) ([]*RenderedTask, []DispatchPlan, error) {
	iters, err := resolveLoopItems(p.cel, in, task.Loop, asName)
	if err != nil {
		// Нерезолвимый items в скипнутой задаче — НЕ ошибка: эмитим один placeholder.
		rt := p.loopSkipPlaceholder(task, startIndex, skip)
		return []*RenderedTask{rt}, []DispatchPlan{{TaskIndex: startIndex}}, nil
	}

	tasks := make([]*RenderedTask, 0, len(iters))
	plans := make([]DispatchPlan, 0, len(iters))
	idx := startIndex
	for range iters {
		tasks = append(tasks, p.loopSkipPlaceholder(task, idx, skip))
		plans = append(plans, DispatchPlan{TaskIndex: idx})
		idx++
	}
	return tasks, plans, nil
}

// loopSkipPlaceholder строит один skip-placeholder loop-итерации: Params=nil
// (рендер пропущен), flow_context первого хоста, протянутые When/ID/Register +
// onchanges/onfail-имена — напрямую, не заходя в renderTaskIter (один источник
// Index/Passage; Passage стампится stampPassage в caller-е Render).
//
// onChangesNames/onFailNames протягиваются симметрично staticSkipPlaceholder
// (pipeline.go): без них requisite-имена static-false loop-задачи потерялись бы
// на skip-placeholder, и финальный resolveOnChanges/resolveOnFail не нашёл бы
// источники — латентная потеря requisites у loop-задачи с onchanges:/onfail:.
func (p *Pipeline) loopSkipPlaceholder(task config.Task, idx int, skip *structpb.Struct) *RenderedTask {
	return &RenderedTask{
		Index:          idx,
		Name:           task.Name,
		Module:         task.Module.Module,
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
}

// resolveLoopItems вычисляет items и раскладывает в упорядоченный список
// loop-контекстов (`map[<as>]=элемент` + опционально `map[<index_as>]=индекс/
// ключ`). items резолвится один раз на прогон в host-инвариантном контексте
// (input/register/incarnation; без soulprint — items не зависит от хоста).
//
// array → as=элемент, index_as=0-based; object → as=значение, index_as=ключ,
// порядок алфавитный по ключам (destiny/tasks.md §7). Скаляр/строка (например,
// items, не разрешившийся в коллекцию) → ошибка: loop требует array/object.
func resolveLoopItems(engine *cel.Engine, in RenderInput, loop *config.LoopSpec, asName string) ([]map[string]any, error) {
	resolved, err := renderValue(engine, loop.Items, loopInvariantVars(in, nil), "loop.items")
	if err != nil {
		return nil, err
	}

	indexAs := loop.IndexAs
	switch coll := resolved.(type) {
	case []any:
		out := make([]map[string]any, len(coll))
		for i, el := range coll {
			ctx := map[string]any{asName: el}
			if indexAs != "" {
				ctx[indexAs] = i
			}
			out[i] = ctx
		}
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(coll))
		for k := range coll {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]map[string]any, len(keys))
		for i, k := range keys {
			ctx := map[string]any{asName: coll[k]}
			if indexAs != "" {
				ctx[indexAs] = k
			}
			out[i] = ctx
		}
		return out, nil
	default:
		return nil, fmt.Errorf("loop.items вычислился в %T, ожидался array или object", resolved)
	}
}

// loopInvariantVars — host-инвариантный контекст оси loop: input/register/
// incarnation/essence + переменные текущей итерации (`<as>`/`<index_as>`), БЕЗ
// soulprint.self. items и when: резолвятся именно в нём — все host-инвариантны в
// пилоте (симметрично resolveCovenList: on: тоже резолвится не per-host).
// essence host-инвариантна, поэтому доступна в items/when (`items:
// ${ essence.users }`). loopVars=nil для самого items (loop-переменных ещё нет).
//
// soulprint.hosts (+ .where) ДОСТУПЕН: это host-инвариантный roster прогона (не
// per-host факты), легитимный источник items (`items: ${ soulprint.hosts.where(
// "role == 'replica'") }`). soulprint.self в loop по-прежнему недоступен —
// per-host loop отложен; в loop.when ссылка на любой soulprint режется отдельным
// guard-ом (reLoopWhenSoulprint) ДО построения этого контекста.
func loopInvariantVars(in RenderInput, loopVars map[string]any) cel.Vars {
	return cel.Vars{
		Input:          in.Input,
		Register:       in.Register,
		Incarnation:    incarnationVars(in, len(in.Hosts)),
		SoulprintHosts: soulprintHosts(in),
		Essence:        in.Essence,
		Loop:           loopVars,
		AllowHosts:     !in.destinyIsolated,
	}
}

// reLoopWhenSoulprint ловит ссылку на soulprint в loop.when. when: в пилоте
// host-инвариантен (вычисляется один раз на прогон, как items), поэтому
// host-вариативный предикат по soulprint конкретного хоста не поддержан —
// per-host loop-фильтрация отложена (soulprint.hosts / E3).
var reLoopWhenSoulprint = regexp.MustCompile(`\bsoulprint\b`)

// evalLoopWhen вычисляет per-item фильтр loop.when в host-инвариантном
// контексте — том же, что и items (input/register/incarnation + loop-
// переменные, без soulprint). Пустой when → true (нет фильтра).
//
// when: задуман как фильтр по СОДЕРЖИМОМУ элемента (`item.enabled`), не как
// per-host предикат. Ссылка на soulprint → понятная ошибка (host-вариативный
// when в loop не поддержан в пилоте), а не молчаливое решение по первому хосту:
// это симметрично guard-у host-инвариантности params (см. renderTaskIter).
// Не-bool результат → ошибка (when: обязан возвращать булево, как where:).
func evalLoopWhen(engine *cel.Engine, in RenderInput, when string, loopVars map[string]any) (bool, error) {
	if when == "" {
		return true, nil
	}
	if reLoopWhenSoulprint.MatchString(when) {
		return false, fmt.Errorf(
			"loop.when %q ссылается на soulprint — host-вариативный when в loop не поддержан в пилоте (loop host-инвариантен; per-host loop-фильтрация отложена)",
			when)
	}
	return evalBoolExpr(engine, "loop.when", when, loopInvariantVars(in, loopVars))
}
