package render

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// renderBlockTask разворачивает block-задачу (pilot C1, destiny/tasks.md §6.5,
// orchestration.md §2.2.1) в плоский слой RenderedTask на render-фазе — по образцу
// renderApplyDestiny/renderLoopTask. block — НЕ wire-сущность: контракт
// (proto/DispatchPlan/Soul) не меняется. «Весь блок одной волной» эмёрджентно:
// все потомки несут одинаковые SerialWidth+TargetSIDs (унаследованы от block) и
// один Passage (stampPassage клеймит весь fan-out — block атомарен по Passage,
// ADR-056; Stratify учитывает это).
//
// Наследование (mergeBlockInheritance) — каждому потомку:
//   - when: AND-merge `(<block.when>) && (<child.when>)` (один из пустых → берётся
//     другой). Предикаты протягиваются КАК CEL-строки (Soul вычисляет, ADR-012(d)).
//   - where: AND-merge с child.where (тот же закон; резолвится Keeper-side на потомке).
//   - vars: block.vars база, child.vars поверх (child перебивает одноимённые).
//   - onchanges/onfail: union block+child имён → потомок.
//
// width вычисляется ОДИН раз из block.Serial против числа targeted-хостов и
// наследуется всеми потомками (SerialWidth в каждый DispatchPlan). targeted —
// хосты block-задачи (после on:/where:/run_once: родителя, резолв в Render ДО
// ветки). idx сквозной: потомки получают монотонные Index без дыр.
//
// Виды потомков:
//   - вложенный block: → рекурсивный renderBlockTask (наследование каскадом);
//   - apply: → renderApplyDestiny (унаследованный width);
//   - module: → renderTask;
//   - loop: на потомке → ОТВЕРГАЕТСЯ (вне pilot C1, понятная ошибка);
//   - parallel:/include: на потомке → ОТВЕРГАЕТСЯ (guardPilotBlockChild).
//
// Static-when-false потомок: emitStaticWhenSkip в начале цикла эмитит
// placeholder(ы) ДО guard/render — симметрично top-level Render-циклу (неактивная
// ветка с unsupported-DSL не блокирует активную). Block-level static-when:false НЕ
// гасится emitStaticWhenSkip (она пропускает block-задачу): static-false block
// доходит сюда, mergeBlockInheritance вливает block.when в КАЖДОГО потомка через
// AND, static-when каждого потомка становится false, и каждый child эмитит СВОЙ
// skip-placeholder с register/requisites через walkBlockChildren. Так register
// потомков static-false block виден снаружи (flat-register-scope инвариантен
// относительно static-skip — паттерн loopStaticSkip).
func (p *Pipeline) renderBlockTask(
	ctx context.Context,
	in RenderInput,
	blockTask config.Task,
	startIndex int,
	targeted []*topology.HostFacts,
) ([]*RenderedTask, []DispatchPlan, error) {
	width := serialWidth(blockTask.Serial, len(targeted))

	// childTarget: потомок резолвит свой on:/where:/run_once: против хостов блока
	// (scenario-оркестрация внутри block разрешена — orchestration.md §2.2.2);
	// блок СУЖАЕТ roster потомка.
	childTarget := func(child config.Task) ([]*topology.HostFacts, error) {
		return p.blockChildTargets(in, child, targeted)
	}
	// childRecurse: вложенный block → рекурсия того же renderBlockTask.
	childRecurse := func(child config.Task, idx int, childTargeted []*topology.HostFacts) ([]*RenderedTask, []DispatchPlan, error) {
		return p.renderBlockTask(ctx, in, child, idx, childTargeted)
	}
	return p.walkBlockChildren(ctx, in, blockTask, startIndex, width, guardPilotBlockChild, childTarget, childRecurse)
}

// blockChildGuard — проверка вида потомка block-а перед рендером. scenario-слой
// (renderBlockTask) подставляет guardPilotBlockChild (разрешает apply/scenario-
// оркестрацию потомка); destiny-слой (renderDestinyBlock) — guardDestinyBlockChild
// (отвергает scenario-ключи). idx — позиция потомка для диагностики, blockName —
// имя контейнера.
type blockChildGuard func(child config.Task, idx int, blockName string) error

// blockChildTargeter резолвит roster потомка block-а. scenario-слой сужает roster
// по on:/where:/run_once: потомка; destiny-слой наследует roster блока ЦЕЛИКОМ
// (block в destiny НЕ несёт оркестрацию — отвергнута guard-ом).
type blockChildTargeter func(child config.Task) ([]*topology.HostFacts, error)

// blockChildRecurser рендерит ВЛОЖЕННЫЙ block-потомок (child.Block != nil). Каждый
// слой подставляет свою рекурсию (renderBlockTask / renderDestinyBlock), чтобы
// наследование каскадом не разъехалось между слоями.
type blockChildRecurser func(child config.Task, idx int, childTargeted []*topology.HostFacts) ([]*RenderedTask, []DispatchPlan, error)

// walkBlockChildren — ЕДИНЫЙ обход потомков block-а для обоих слоёв (scenario и
// destiny). Source-of-truth порядка обработки потомка, чтобы наследование block
// не разъехалось между renderBlockTask и renderDestinyBlock (drift-риск major):
//
//	mergeBlockInheritance(blockTask, child)   — влить when/where/vars/requisites
//	→ emitStaticWhenSkip(child)               — AND-when мог стать static-false
//	→ guard(child)                            — граница ключей слоя (callback)
//	→ render: вложенный block (recurse) / apply / module
//
// Слоевая разница вынесена в три callback-а:
//   - guard: какие виды/ключи потомка допустимы (scenario vs destiny-граница);
//   - target: как резолвится roster потомка (сужение on/where vs наследование);
//   - recurse: чем рендерится вложенный block (рекурсия того же слоя).
//
// width — ширина волны контейнера (serial: блока для scenario / serial: apply-
// задачи для destiny): протягивается в каждый DispatchPlan потомка (block НЕ несёт
// свой serial у потомков — наследует ширину контейнера). idx сквозной.
//
// apply-потомок: renderApplyDestiny ВСЕГДА разрешён только когда guard его пропустил
// (destiny-guard отвергает apply на потомке — вложенный apply в destiny запрещён,
// guardDestinyTask); поэтому ветка child.Apply достижима лишь в scenario-слое.
func (p *Pipeline) walkBlockChildren(
	ctx context.Context,
	in RenderInput,
	blockTask config.Task,
	startIndex int,
	width int,
	guard blockChildGuard,
	target blockChildTargeter,
	recurse blockChildRecurser,
) ([]*RenderedTask, []DispatchPlan, error) {
	tasks := make([]*RenderedTask, 0, len(blockTask.Block.Block))
	plans := make([]DispatchPlan, 0, len(blockTask.Block.Block))
	idx := startIndex
	for i := range blockTask.Block.Block {
		child := mergeBlockInheritance(blockTask, blockTask.Block.Block[i])

		// Static-when-false потомок: placeholder(ы) ДО guard/render (как top-level
		// Render-цикл). idx двигается на число эмитнутых placeholder-ов. Static-when
		// унаследованного when статичен ровно тогда, когда статичны оба слагаемых
		// (block.when + child.when) — isStaticWhen проверяет AND-строку целиком.
		if skipped, serr := p.emitStaticWhenSkip(ctx, in, child, &tasks, &plans, &idx); serr != nil {
			return nil, nil, serr
		} else if skipped {
			continue
		}

		if gerr := guard(child, i, blockTask.Name); gerr != nil {
			return nil, nil, gerr
		}

		// Вложенный block: рекурсия — наследование каскадом (внешний предикат уже
		// влит в child через mergeBlockInheritance, теперь child раздаёт его своим
		// потомкам). serial НЕ наследуется в child (mergeBlockInheritance его не
		// трогает): вложенный block без serial: едет своей шириной (0 = одной волной).
		if child.Block != nil {
			childTargeted, terr := target(child)
			if terr != nil {
				return nil, nil, terr
			}
			bt, bp, berr := recurse(child, idx, childTargeted)
			if berr != nil {
				return nil, nil, berr
			}
			tasks = append(tasks, bt...)
			plans = append(plans, bp...)
			idx += len(bt)
			continue
		}

		childTargeted, terr := target(child)
		if terr != nil {
			return nil, nil, terr
		}

		// apply-потомок: изолированный render-проход destiny с унаследованным width.
		// Достижимо только в scenario-слое (destiny-guard отвергает apply на потомке).
		// child.Register материализуется терминальной core.noop.run агрегата (Вариант B,
		// renderApplyDestiny) — applier-register block-потомка адресуем извне как
		// register.<child>.* (orchestration.md §2.1.1).
		if child.Apply != nil {
			dt, dp, derr := p.renderApplyDestiny(ctx, in, child.Apply, idx, childTargeted, width, child.Register)
			if derr != nil {
				return nil, nil, derr
			}
			tasks = append(tasks, dt...)
			plans = append(plans, dp...)
			idx += len(dt)
			continue
		}

		// module-потомок.
		rt, rerr := p.renderTask(ctx, in, child, idx, childTargeted)
		if rerr != nil {
			return nil, nil, rerr
		}
		tasks = append(tasks, rt)
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(childTargeted),
			SerialWidth: width,
		})
		idx++
	}
	return tasks, plans, nil
}

// blockChildTargets резолвит таргет block-потомка: on:/where: child (уже несущего
// унаследованный where через mergeBlockInheritance) против хостов блока. on: на
// потомке внутри block в пилоте не используется типично, но резолвится тем же
// resolveTargets (как в scenario-цикле); затем run_once: потомка (нечастый кейс,
// но грамматика разрешает — orchestration.md §2.2.2).
//
// КРИТИЧНО (изоляция таргета): резолв идёт против хостов БЛОКА (targeted —
// результат on:/where:/run_once: block-задачи в Render), НЕ против всего roster-а:
// where: потомка сужает уже отобранное блоком множество, симметрично двухфазному
// резолву on:→where: (orchestration.md §4).
func (p *Pipeline) blockChildTargets(in RenderInput, child config.Task, targeted []*topology.HostFacts) ([]*topology.HostFacts, error) {
	scoped := in
	scoped.Hosts = targeted
	childTargeted, err := resolveTargets(p.cel, scoped, child)
	if err != nil {
		return nil, err
	}
	return applyRunOnce(childTargeted, child.RunOnce), nil
}

// mergeBlockInheritance строит потомка block-а с влитым наследованием от
// контейнера (destiny/tasks.md §6.5). НЕ мутирует исходные структуры — возвращает
// копию child с переписанными When/Where/Vars/OnChanges/OnFail. Остальные поля
// (Module/Apply/Block/Loop/Register/params/serial/run_once/…) — как в исходном
// потомке: serial НЕ наследуется потомком (ширина волны раздаётся через DispatchPlan
// в renderBlockTask, а не через поле задачи).
func mergeBlockInheritance(blockTask config.Task, child config.Task) config.Task {
	out := child
	out.When = andMergePredicate(blockTask.When, child.When)
	out.Where = andMergePredicate(blockTask.Where, child.Where)
	out.Vars = mergeVars(blockTask.Vars, child.Vars)
	out.OnChanges = unionNames(blockTask.OnChanges, child.OnChanges)
	out.OnFail = unionNames(blockTask.OnFail, child.OnFail)
	return out
}

// andMergePredicate соединяет два CEL-предиката (when:/where:) по AND с
// сохранением скобок-приоритета: `(<outer>) && (<inner>)`. Пустой один → берётся
// другой как есть (без обёртки); оба пусты → "". Это закон destiny/tasks.md §6.5
// «внутренний when комбинируется с внешним по AND».
func andMergePredicate(outer, inner string) string {
	switch {
	case outer == "" && inner == "":
		return ""
	case outer == "":
		return inner
	case inner == "":
		return outer
	default:
		return "(" + outer + ") && (" + inner + ")"
	}
}

// mergeVars сливает block.vars (база) и child.vars (поверх): child перебивает
// одноимённые ключи (destiny/tasks.md §9, более локальный scope побеждает). Оба
// пусты → nil. Не мутирует входные карты.
func mergeVars(base, override map[string]any) map[string]any {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// unionNames объединяет два списка requisite-имён (onchanges/onfail) без
// дубликатов, сохраняя порядок «block-имена, затем child-имена». Оба пусты → nil.
func unionNames(blockNames, childNames []string) []string {
	if len(blockNames) == 0 && len(childNames) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(blockNames)+len(childNames))
	out := make([]string, 0, len(blockNames)+len(childNames))
	for _, n := range append(append([]string{}, blockNames...), childNames...) {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// guardPilotBlockChild отвергает виды потомков block-а вне pilot C1 явной
// [ErrUnsupportedDSL]. Поддержаны: module:, apply:, вложенный block: (рекурсия в
// renderBlockTask проверяет их раньше — сюда block-потомок не доходит). Вне pilot:
//   - loop: на потомке (render-time fan-out внутри block отложен);
//   - parallel: на потомке (parallel в block — слайс позже);
//   - include: (должен раскрываться ДО render — ErrUnexpandedInclude);
//   - пустая задача (нет дискриминатора).
//
// block-потомок (child.Block != nil) сюда не попадает — renderBlockTask ветвит на
// рекурсию ДО guard. parallel/loop НА САМОМ block-е (не потомке) отвергает
// config-валидатор (parallel_on_block_invalid / loop валидация) и guardPilotDSL
// (parallel) на top-level.
func guardPilotBlockChild(child config.Task, idx int, blockName string) error {
	switch {
	case child.Loop != nil:
		return fmt.Errorf("%w: loop: на потомке block %q (task[%d] %q) — вне pilot-объёма block", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Parallel:
		return fmt.Errorf("%w: parallel: на потомке block %q (task[%d] %q) — вне pilot-объёма block", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Include != nil:
		return fmt.Errorf("%w: include: на потомке block %q (task[%d] %q)", ErrUnexpandedInclude, blockName, idx, child.Name)
	case child.Module == nil && child.Apply == nil && child.Block == nil:
		return fmt.Errorf("%w: task[%d] %q в block %q не является module/apply/block-задачей", ErrUnsupportedDSL, idx, child.Name, blockName)
	}
	return nil
}
