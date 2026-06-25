package render

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ResolvedDestiny — материализованная destiny для apply-задачи: распарсенные
// задачи и `input:`-контракт. Возвращается [DestinyResolver]. Изолированный
// render-проход (V2, ADR-009) рендерит Tasks в собственном CEL-env, видя только
// apply.input против Input-контракта — без scenario-scope (vars/register/
// soulprint).
type ResolvedDestiny struct {
	// Name — имя destiny (для диагностики).
	Name string
	// Tasks — плоский список задач `tasks/main.yml` destiny.
	Tasks []config.Task
	// Input — input:-схема `destiny.yml` (для defense-in-depth-проверки
	// apply.input против контракта).
	Input config.InputSchemaMap
	// Vars — RAW destiny-локалы из `vars.yml` (docs/destiny/vars.md), без
	// схемо-валидации (vars не типизированы). CEL-выражения `${ … }` в значениях
	// резолвятся внутри destiny-прохода (renderApplyDestiny) над
	// input+soulprint.self+incarnation — изолированно от scenario-scope; результат
	// — базовый слой `vars.*`, поверх которого task-level `vars:` переопределяют
	// одноимённые ключи (Вариант A, vars.md «Слияние file-vars ↔ task-vars»). nil
	// → destiny без локалов.
	Vars map[string]any
	// Templates — ридер `.tmpl` снапшота ЭТОЙ destiny (её шаблоны живут в её
	// собственном снапшоте, не в снапшоте сервиса; одноуровневый резолв —
	// scenario-local-слоя у destiny нет). nil → core.file.rendered внутри destiny
	// → ошибка handoff (TemplateReader не сконфигурирован). DestinyResolver
	// обязан его заполнить (прод — snapshot-backed, Trial — fixture-backed).
	Templates TemplateReader
}

// DestinyResolver резолвит destiny по имени из apply-задачи в распарсенный
// артефакт. В проде реализуется loader-backed адаптером (git-снапшот destiny,
// scenario-runner); в герметичном Trial L0 — fixture-резолвером (destiny рядом
// с case.yml). nil-resolver → apply:destiny отвергается с ErrUnsupportedDSL.
type DestinyResolver interface {
	Resolve(ctx context.Context, name string) (*ResolvedDestiny, error)
}

// renderApplyDestiny выполняет изолированный render-проход destiny (V2, ADR-009)
// для apply-задачи parent и возвращает её отрендеренные задачи + планы диспатча.
//
// Изоляция (КРИТИЧНО): destiny видит ТОЛЬКО свой input: (резолвнутый apply.input),
// НЕ scenario input/vars/register/soulprint. Это структурная граница — отдельный
// RenderInput с пустыми Register/Essence; SoulprintSelf хоста сохраняется
// (per-host факты — стабильный слой, доступный любому шагу), но scenario-scope
// (input родителя, register, vars) в destiny-env не попадает.
//
// startIndex — сквозной индекс первой destiny-задачи в итоговом плане родителя
// (RenderedTask.Index/DispatchPlan.TaskIndex монотонно растут по всему плану).
// loop: на destiny-задаче разворачивается в N RenderedTask (renderLoopTask), idx
// растёт на число итераций — индексы остаются сквозными (симметрично scenario).
//
// targeted — хосты apply-задачи (после резолва on:/where: и run_once: родителя).
// destiny наследует этот roster; per-task on:/where: внутри destiny в пилоте не
// поддержаны (guardDestinyTask отвергает их).
//
// serialWidth — ширина волны `serial:` apply-задачи родителя (orchestration.md
// §2.2.1): наследуется всеми destiny-задачами (вся destiny катится одной
// rolling-моделью по хостам). 0 = serial не задан.
func (p *Pipeline) renderApplyDestiny(
	ctx context.Context,
	parentIn RenderInput,
	apply *config.ApplyTask,
	startIndex int,
	targeted []*topology.HostFacts,
	serialWidth int,
) ([]*RenderedTask, []DispatchPlan, error) {
	if parentIn.Destiny == nil {
		return nil, nil, fmt.Errorf("%w: apply: destiny %q — DestinyResolver не сконфигурирован (RenderInput.Destiny=nil)", ErrUnsupportedDSL, apply.Destiny)
	}

	resolved, err := parentIn.Destiny.Resolve(ctx, apply.Destiny)
	if err != nil {
		return nil, nil, fmt.Errorf("render: apply destiny %q: %w", apply.Destiny, err)
	}

	// Резолв apply.input → значения входа destiny + defense-in-depth-проверка
	// против input:-контракта destiny (required-параметры присутствуют, defaults
	// применены). apply.input рендерится в scenario-env (родитель резолвит, что
	// передать); сам destiny видит только результат.
	destinyInput, err := p.resolveApplyInput(parentIn, apply, resolved, targeted)
	if err != nil {
		return nil, nil, err
	}

	// Изолированный RenderInput destiny: только input + roster + incarnation-meta.
	// Register/Essence/RegisterByHost — пустые: destiny не видит scenario-scope.
	// destinyIsolated=true: soulprint.hosts/soulprint.where в destiny — ошибка
	// изоляции (orchestration.md §4.1); проекция хостов в destiny НЕ пробрасывается.
	destinyIn := RenderInput{
		Scenario:        &config.ScenarioManifest{Name: resolved.Name, Tasks: resolved.Tasks},
		Input:           destinyInput,
		Incarnation:     parentIn.Incarnation,
		Hosts:           targeted,
		Templates:       resolved.Templates, // .tmpl из снапшота ИМЕННО этой destiny
		Ctx:             ctx,                // vault() в destiny-params: отмена/таймаут ReadKV
		destinyIsolated: true,
		// seal (ADR-010 §7.4): тот же аккумулятор прогона — destiny-params с
		// `${ vault(...) }` маркируются sealed наравне со scenario. destiny-input
		// secret-флаг детектится только если ResolvedDestiny несёт Input-схему (в
		// пилоте не пробрасывается — vault()-провенанс ловится без схемы; см.
		// observations: транзит destiny-secret-input — отдельный слайс).
		Sealed: parentIn.Sealed,
	}

	// destiny-локалы vars.yml (Вариант A, vars.md): резолв ОДИН раз на проход
	// per-host над destiny-env (input destiny + soulprint.self + incarnation),
	// изолированно от scenario-scope. resolveDestinyVars сам строит base-env с
	// пустыми Register/Essence/Vars и AllowHosts=false — `vars.<other>`/`register.*`/
	// `essence.*`/`soulprint.hosts` в значении vars.yml дают ошибку изоляции.
	destinyVars, verr := p.resolveDestinyVars(destinyIn, resolved.Vars, targeted)
	if verr != nil {
		return nil, nil, verr
	}
	destinyIn.DestinyVarsResolved = destinyVars

	tasks := make([]*RenderedTask, 0, len(resolved.Tasks))
	plans := make([]DispatchPlan, 0, len(resolved.Tasks))
	idx := startIndex

	// includeGroupKeep — кеш решения по условному include внутри destiny (group-drop,
	// ADR-009 amendment): id группы (Task.IncludeGroupID, проставлен ExpandIncludes) →
	// keep/drop. Раздельный от scenario-кеша (pipeline.go) — destiny-проход изолирован,
	// своё env. include-when вычисляется ОДИН раз на группу над destinyIn (изолированный
	// destiny-input), host-инвариантно. Безусловные задачи (IncludeGroupID==0) сюда не
	// попадают.
	includeGroupKeep := map[int]bool{}

	for i := range resolved.Tasks {
		task := resolved.Tasks[i]

		// Conditional-include group-drop (ADR-009 amendment) — ЗЕРКАЛО scenario-цикла
		// (pipeline.go), ДО emitStaticWhenSkip и block-обработки. Задача несёт
		// IncludeGroupID!=0 (config.ExpandIncludes раскрыл её include под статическим
		// `when:`). include-when вычисляется ОДИН раз на группу в ИЗОЛИРОВАННОМ destiny-env
		// (destinyIn: input = резолвнутый apply.input + schema-defaults, НЕ scenario-scope)
		// — НЕ parentIn. include-when false → РЕАЛЬНЫЙ дроп: continue БЕЗ эмита RenderedTask
		// и БЕЗ idx++ (индекс не резервируется, задача физически исчезает). Дискриминатор
		// IncludeGroupID ортогонален block: group-drop стоит ВЫШЕ block-ветки, поэтому при
		// keep=false дропается вся группа (вкл. block-задачу+потомков) ДО renderDestinyBlock.
		// Кеш includeGroupKeep раздельный от scenario; register-изоляция идентична scenario
		// (cross-file register дропнутой группы lint-запрещён офлайн → onchanges не падает).
		if task.IncludeGroupID != 0 {
			keep, ok := includeGroupKeep[task.IncludeGroupID]
			if !ok {
				k, derr := p.evalIncludeWhen(destinyIn, task.IncludeWhen)
				if derr != nil {
					return nil, nil, derr
				}
				keep = k
				includeGroupKeep[task.IncludeGroupID] = keep
			}
			if !keep {
				continue // group-drop: ни RenderedTask, ни idx — реальное исключение.
			}
		}

		// Static-when ПРЕДШЕСТВУЕТ guardDestinyTask (ADR-012(d), тот же инвариант,
		// что и в scenario-цикле pipeline.go): статически-false `when:` гейтит задачу
		// off ДО DSL-guard, поэтому unsupported-DSL (`parallel:`) в неактивной ветке
		// destiny не блокирует активную. Это лечит multi-action destiny redis:
		// diagnostic.yml несёт `parallel: true`+`when: input.action=='diagnose'`, а
		// при action=update_acls эти задачи неактивны — раньше guardDestinyTask
		// отвергал их ErrUnsupportedDSL ещё ДО static-when и ронял весь destiny-проход.
		if skipped, serr := p.emitStaticWhenSkip(ctx, destinyIn, task, &tasks, &plans, &idx); serr != nil {
			return nil, nil, serr
		} else if skipped {
			continue
		}

		if gerr := guardDestinyTask(task, i, resolved.Name); gerr != nil {
			return nil, nil, gerr
		}

		// block: внутри destiny-прохода (ADR-009 amendment 2026-06-24) — render-time
		// fan-out в плоский слой, как в scenario (renderBlockTask), но в destiny-
		// семантике: наследование env-agnostic (mergeBlockInheritance: when/vars/
		// requisites), roster наследуется ЦЕЛИКОМ (block НЕ сужает хосты — where/on
		// на destiny-block отвергнуты guardDestinyBlockChild), serialWidth родителя
		// destiny протягивается в каждый DispatchPlan потомка. Static-when-false блок
		// НЕ гасится emitStaticWhenSkip (она пропускает block-задачу) — заходит сюда,
		// walkBlockChildren вливает block.when в каждого потомка через AND и каждый
		// child эмитит СВОЙ skip-placeholder с register/requisites (flat-register-
		// scope цел при skip — register потомков виден снаружи через resolveOnChanges).
		if task.Block != nil {
			bt, bp, berr := p.renderDestinyBlock(ctx, destinyIn, task, idx, targeted, serialWidth)
			if berr != nil {
				return nil, nil, berr
			}
			tasks = append(tasks, bt...)
			plans = append(plans, bp...)
			idx += len(bt)
			continue
		}

		destinyTargeted, terr := resolveTargets(p.cel, destinyIn, task)
		if terr != nil {
			return nil, nil, terr
		}

		// loop: на destiny-задаче (слайс E снят) — render-time fan-out, как в
		// scenario-цикле (pipeline.go). renderLoopTask path-agnostic: items/when
		// резолвятся через loopInvariantVars над destinyIn → AllowHosts=false и
		// пустой Register наследуют изоляцию destiny (soulprint.hosts/register в
		// items — ошибка изоляции). idx растёт на число развёрнутых итераций.
		if task.Loop != nil {
			lt, lp, lerr := p.renderLoopTask(ctx, destinyIn, task, idx, destinyTargeted)
			if lerr != nil {
				return nil, nil, lerr
			}
			tasks = append(tasks, lt...)
			plans = append(plans, lp...)
			idx += len(lt)
			continue
		}

		rt, rerr := p.renderTask(ctx, destinyIn, task, idx, destinyTargeted)
		if rerr != nil {
			return nil, nil, rerr
		}

		tasks = append(tasks, rt)
		plans = append(plans, DispatchPlan{
			TaskIndex:   idx,
			TargetSIDs:  sidsOf(destinyTargeted),
			SerialWidth: serialWidth,
		})
		idx++
	}
	return tasks, plans, nil
}

// resolveDestinyVars резолвит destiny-локалы `vars.yml` (raw) per-host в
// destiny-env (Вариант A, vars.md). Возвращает sid → имя→резолвленное-значение.
//
// Изоляция (КРИТИЧНО): base-env строится hostVars-ом над destinyIn — изолированным
// RenderInput destiny (Register/Essence пусты, destinyIsolated=true → AllowHosts
// =false). Доступны input.* (destiny-input, не scenario), soulprint.self.* и
// incarnation.*; `register.*`/`essence.*`/`soulprint.hosts` дают ошибку изоляции.
// base.Vars пуст на СТАРТЕ слоя (resolveVarLayer накапливает его сам) — ссылка
// `vars.<other>` резолвится только на file-var ЭТОГО ЖЕ слоя (var→var разрешён,
// eager-topological), а на чужой слой/register/soulprint.hosts — ошибка изоляции.
//
// var→var (vars.md, ADR-009/ADR-010 amendment 2026-06-24): file-var может ссылаться
// на другой file-var через `${ vars.<other> }`; resolveVarLayer строит граф через
// VarRefs и резолвит в топопорядке. Порядок ключей в vars.yml безразличен. Цикл →
// ErrVarCycle с трассой, ссылка на несуществующий var → ErrVarUnknownRef (eager,
// даже если ссылающийся var не используется). Изоляция НЕ ослаблена: var→var живёт
// строго внутри file-слоя.
//
// Резолв per-host: значения могут ссылаться на soulprint.self (host-вариативный),
// поэтому каждый хост получает свою карту. nil raw → nil (destiny без локалов).
// Пустой targeted (where: отфильтровал всех) → один синтетический хост под ключ "".
func (p *Pipeline) resolveDestinyVars(destinyIn RenderInput, raw map[string]any, targeted []*topology.HostFacts) (map[string]map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	hosts := targeted
	if len(hosts) == 0 {
		hosts = []*topology.HostFacts{{}}
	}
	out := make(map[string]map[string]any, len(hosts))
	for _, host := range hosts {
		base := hostVars(destinyIn, host, len(targeted)) // base.Vars пуст — старт слоя
		resolved, err := resolveVarLayer(p.cel, raw, base)
		if err != nil {
			return nil, fmt.Errorf("render: destiny %q (vars.yml, host %s): %w", destinyIn.Scenario.Name, host.SID, err)
		}
		out[host.SID] = resolved
	}
	return out, nil
}

// resolveApplyInput вычисляет вход destiny из apply.input.
//
// apply.input — литералы/CEL в scenario-env (родитель решает, что передать в
// destiny). Резолвим в контексте родителя (input/incarnation/soulprint.self
// первого targeted-хоста, либо пустой), затем сверяем результат с input:-схемой
// destiny (defense in depth, ADR-009): обязательные параметры присутствуют,
// отсутствующие с default — добираются.
//
// apply.input host-инвариантен в пилоте: значения вычисляются один раз (на
// первом targeted-хосте), как и params module-задач (host-вариативность вне
// пилота).
func (p *Pipeline) resolveApplyInput(
	parentIn RenderInput,
	apply *config.ApplyTask,
	resolved *ResolvedDestiny,
	targeted []*topology.HostFacts,
) (map[string]any, error) {
	var host *topology.HostFacts
	if len(targeted) > 0 {
		host = targeted[0]
	} else {
		host = &topology.HostFacts{}
	}
	vars := hostVars(parentIn, host, len(targeted))

	rendered := make(map[string]any, len(apply.Input))
	for name, raw := range apply.Input {
		val, err := renderValue(p.cel, raw, vars, "apply.input."+name)
		if err != nil {
			return nil, fmt.Errorf("render: apply destiny %q input %q: %w", apply.Destiny, name, err)
		}
		rendered[name] = val
	}

	if err := applyInputContract(rendered, resolved.Input, apply.Destiny); err != nil {
		return nil, err
	}
	return rendered, nil
}

// applyInputContract сверяет резолвнутый apply.input с input:-схемой destiny
// (defense in depth, ADR-009): добирает defaults для отсутствующих параметров,
// отвергает отсутствие обязательного параметра без default.
//
// Полная type/pattern/enum-валидация значений против схемы — отдельный валидатор
// (его нет ни для scenario-input, ни для destiny-input в проекте); здесь —
// минимальный required+default-контракт, гарантирующий корректность CEL-рендера
// destiny.
func applyInputContract(values map[string]any, schema config.InputSchemaMap, destiny string) error {
	for name, sc := range schema {
		if sc == nil {
			continue
		}
		if _, ok := values[name]; ok {
			continue
		}
		if sc.Default != nil {
			values[name] = sc.Default
			continue
		}
		if sc.Required {
			return fmt.Errorf("render: apply destiny %q: обязательный input %q не передан и не имеет default", destiny, name)
		}
	}
	return nil
}

// guardDestinyTask отвергает вложенные DSL-конструкции destiny вне пилот-объёма
// (parallel:/вложенный apply:) и scenario-only ключи на destiny-задаче
// (serial:/run_once: — недопустимы в destiny, docs/destiny/tasks.md §3;
// serial: scenario-уровня наследуется destiny через параметр renderApplyDestiny,
// не через поле destiny-задачи). Пилот поддерживает плоский destiny: module-
// задачи с on:/where: + loop: (слайс E снят — fan-out наследует изоляцию destiny
// через loopInvariantVars: AllowHosts=false, Register пуст) + block: (ADR-009
// amendment 2026-06-24 — render-time fan-out, renderDestinyBlock).
// include: внутри destiny раскрывается ДО render (within-destiny, в
// DestinyLoader.parseTasks / fixture-резолвере); дошедший до render include —
// ErrUnexpandedInclude (баг раскрытия), не «вне pilot».
//
// ★ block-задача (task.Block != nil) ПРОХОДИТ guardDestinyTask: в renderApplyDestiny
// guardDestinyTask вызывается РАНЬШЕ ветки block (guardDestinyTask :145 → ветка
// `if task.Block != nil` renderDestinyBlock :157). Поэтому `case task.Block != nil`
// ниже — LOAD-BEARING на живом пути: он намеренно ПРОПУСКАЕТ block (return nil, не
// считая её module-задачей), после чего renderApplyDestiny ветвит на renderDestinyBlock.
// Удалить его как «мёртвый» нельзя — без него block упал бы в `case task.Module == nil`
// (не-module-задача). Граница ключей ВНУТРИ destiny-block — guardDestinyBlockChild.
func guardDestinyTask(task config.Task, idx int, destiny string) error {
	switch {
	case task.Apply != nil:
		return fmt.Errorf("%w: вложенный apply: в destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.Include != nil:
		return fmt.Errorf("%w: в destiny %q (task[%d] %q)", ErrUnexpandedInclude, destiny, idx, task.Name)
	case task.Parallel:
		return fmt.Errorf("%w: parallel: в destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.RunOnce:
		return fmt.Errorf("%w: run_once: в destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.Serial != nil:
		return fmt.Errorf("%w: serial: в destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.Block != nil:
		// LOAD-BEARING (не мёртвый код): guardDestinyTask вызывается РАНЬШЕ ветки block
		// в renderApplyDestiny (:145 vs :157), поэтому block ПРОХОДИТ этот guard первым.
		// Намеренно пропускаем (return nil) — block валиден в destiny (ADR-009 amendment),
		// её обработает renderDestinyBlock сразу после. Без этого case block упала бы в
		// `case task.Module == nil` ниже (не module-задача → ошибка).
		return nil
	case task.Module == nil:
		return fmt.Errorf("%w: task[%d] %q в destiny %q не является module-задачей", ErrUnsupportedDSL, idx, task.Name, destiny)
	}
	return nil
}

// renderDestinyBlock разворачивает block-задачу ВНУТРИ destiny-прохода (ADR-009
// amendment 2026-06-24) в плоский слой RenderedTask — зеркало renderBlockTask
// (block.go) в destiny-семантике. Переиспользует тот же обход walkBlockChildren
// (единый source-of-truth наследования: mergeBlockInheritance → emitStaticWhenSkip
// → guard → render), отличаясь тремя слоевыми инвариантами:
//
//   - guard потомка — guardDestinyBlockChild: отвергает scenario-оркестрацию
//     (where/serial/run_once/on/parallel/loop/include/apply) на block или его
//     потомке. В destiny эти ключи бессмысленны (нет roster-резолва на потомке).
//   - roster наследуется ЦЕЛИКОМ (block НЕ сужает хосты): target-callback всегда
//     возвращает targeted блока, в отличие от scenario, где where: потомка сужает.
//   - serialWidth родителя destiny протягивается в каждый DispatchPlan потомка
//     (block НЕ несёт свой serial — он отвергнут guard-ом; width приходит из serial:
//     apply-задачи родителя через renderApplyDestiny).
//
// width=0 для apply-потомка не используется — apply на потомке отвергает
// guardDestinyBlockChild раньше, ветка child.Apply в walkBlockChildren недостижима.
//
// flat register-scope (кейс #10): register потомков block виден СНАРУЖИ block —
// потомки вклеиваются в общий плоский tasks[] destiny-прохода со сквозными idx,
// resolveOnChanges/resolveOnFail на выходе Render резолвят по плоскому списку
// (collectFlatAddresses в config-слое уже рекурсивен через block:).
func (p *Pipeline) renderDestinyBlock(
	ctx context.Context,
	destinyIn RenderInput,
	blockTask config.Task,
	startIndex int,
	targeted []*topology.HostFacts,
	width int,
) ([]*RenderedTask, []DispatchPlan, error) {
	// Граница ключей на САМОМ block-узле. Верхнеуровневый block ПРОХОДИТ
	// guardDestinyTask (тот пропускает его `case task.Block`, см. выше), но НЕ его
	// ключевые проверки module-специфичных полей — потому guardDestinyBlock здесь
	// проверяет их на самом блоке. Вложенный block ловится guardDestinyBlockChild
	// как block-потомок; здесь — единый текст ошибки для обоих путей.
	if gerr := guardDestinyBlock(blockTask); gerr != nil {
		return nil, nil, gerr
	}
	// roster наследуется блоком ЦЕЛИКОМ: block в destiny не несёт on/where (guard
	// отвергает) — потомок применяется к тем же хостам, что и блок.
	childTarget := func(_ config.Task) ([]*topology.HostFacts, error) {
		return targeted, nil
	}
	// вложенный block → рекурсия того же destiny-слоя (наследование каскадом).
	childRecurse := func(child config.Task, idx int, childTargeted []*topology.HostFacts) ([]*RenderedTask, []DispatchPlan, error) {
		return p.renderDestinyBlock(ctx, destinyIn, child, idx, childTargeted, width)
	}
	return p.walkBlockChildren(ctx, destinyIn, blockTask, startIndex, width, guardDestinyBlockChild, childTarget, childRecurse)
}

// guardDestinyBlockChild — граница ключей destiny-block (render-слой; config-слой
// общий на оба слоя и block-ключи там валидны). Отвергает scenario-оркестрацию на
// потомке destiny-block явной [ErrUnsupportedDSL]:
//
//	where / serial / run_once / on / parallel / loop / include / apply
//
// — все они бессмысленны в destiny (нет roster-резолва потомка, нет вложенного
// destiny). ВАЛИДНЫ (наследование env-agnostic + плоское ядро): when (AND-merge),
// name, vars, onchanges/onfail/require (union), вложенный block:; потомок — module:
// или вложенный block:.
//
// Симметрично guardPilotBlockChild (scenario-слой), но строже: scenario разрешает
// apply/serial/run_once/where/on на потомке, destiny — нет.
//
// Граница ключей на САМОМ destiny-block (не потомке) — guardDestinyBlock,
// вызывается в renderDestinyBlock (block проходит guardDestinyTask, чей `case
// task.Block` его пропускает, затем ветка renderDestinyBlock зовёт guardDestinyBlock).
func guardDestinyBlockChild(child config.Task, idx int, blockName string) error {
	switch {
	case child.Where != "":
		return fmt.Errorf("%w: where: на потомке destiny-block %q (task[%d] %q) — scenario-оркестрация в destiny запрещена", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Serial != nil:
		return fmt.Errorf("%w: serial: на потомке destiny-block %q (task[%d] %q) — scenario-оркестрация в destiny запрещена", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.RunOnce:
		return fmt.Errorf("%w: run_once: на потомке destiny-block %q (task[%d] %q) — scenario-оркестрация в destiny запрещена", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.On != nil:
		return fmt.Errorf("%w: on: на потомке destiny-block %q (task[%d] %q) — scenario-оркестрация в destiny запрещена", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Parallel:
		return fmt.Errorf("%w: parallel: на потомке destiny-block %q (task[%d] %q) — scenario-оркестрация в destiny запрещена", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Loop != nil:
		return fmt.Errorf("%w: loop: на потомке destiny-block %q (task[%d] %q) — вне destiny-объёма block", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Include != nil:
		return fmt.Errorf("%w: include: на потомке destiny-block %q (task[%d] %q)", ErrUnexpandedInclude, blockName, idx, child.Name)
	case child.Apply != nil:
		return fmt.Errorf("%w: apply: на потомке destiny-block %q (task[%d] %q) — вложенный apply в destiny запрещён", ErrUnsupportedDSL, blockName, idx, child.Name)
	case child.Module == nil && child.Block == nil:
		return fmt.Errorf("%w: task[%d] %q в destiny-block %q не является module/block-задачей", ErrUnsupportedDSL, idx, child.Name, blockName)
	}
	return nil
}

// guardDestinyBlock — граница ключей на САМОМ block-узле destiny (не потомке).
// Верхнеуровневый destiny-block ветвится в renderApplyDestiny ДО guardDestinyTask,
// поэтому serial:/on:/run_once:/parallel:/loop: на нём НЕ ловятся ни guardDestinyTask
// (block минует её), ни mergeBlockInheritance (она не наследует эти ключи потомку —
// where наследуется, остальные остаются на block-узле). Отвергаем их здесь.
//
// where: на block-узле наследуется потомкам через mergeBlockInheritance и будет
// поймано guardDestinyBlockChild на первом потомке — но если блок пуст (потомков
// нет), where остался бы непроверенным; ловим его и тут для полноты.
func guardDestinyBlock(blockTask config.Task) error {
	switch {
	case blockTask.Where != "":
		return fmt.Errorf("%w: where: на destiny-block %q — scenario-оркестрация в destiny запрещена", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.Serial != nil:
		return fmt.Errorf("%w: serial: на destiny-block %q — scenario-оркестрация в destiny запрещена", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.RunOnce:
		return fmt.Errorf("%w: run_once: на destiny-block %q — scenario-оркестрация в destiny запрещена", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.On != nil:
		return fmt.Errorf("%w: on: на destiny-block %q — scenario-оркестрация в destiny запрещена", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.Parallel:
		return fmt.Errorf("%w: parallel: на destiny-block %q — scenario-оркестрация в destiny запрещена", ErrUnsupportedDSL, blockTask.Name)
	case blockTask.Loop != nil:
		return fmt.Errorf("%w: loop: на destiny-block %q — вне destiny-объёма block", ErrUnsupportedDSL, blockTask.Name)
	}
	return nil
}
