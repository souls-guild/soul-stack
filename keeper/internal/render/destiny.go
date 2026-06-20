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
	for i := range resolved.Tasks {
		task := resolved.Tasks[i]

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
// base.Vars НЕ заполняется в процессе резолва → `vars.<other>` внутри значения
// vars.yml даёт no-such-key (запрет перекрёстных и само-ссылок, vars.md). Раздельный
// per-key резолв в один base гарантирует порядко-независимость (file-vars не видят
// друг друга — зеркало resolveTaskVars).
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
		base := hostVars(destinyIn, host, len(targeted))
		resolved := make(map[string]any, len(raw))
		for name, val := range raw {
			s, ok := val.(string)
			if !ok {
				// Non-string vars-значение — литерал (CEL трогает только строки,
				// симметрично resolveTaskVars/renderValue).
				resolved[name] = val
				continue
			}
			r, err := p.cel.EvalInterpolation(s, base)
			if err != nil {
				return nil, fmt.Errorf("render: destiny %q vars.%s (vars.yml, host %s): %w", destinyIn.Scenario.Name, name, host.SID, err)
			}
			resolved[name] = r
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
// (block:/parallel:/вложенный apply:) и scenario-only ключи на destiny-задаче
// (serial:/run_once: — недопустимы в destiny, docs/destiny/tasks.md §3;
// serial: scenario-уровня наследуется destiny через параметр renderApplyDestiny,
// не через поле destiny-задачи). Пилот поддерживает плоский destiny: module-
// задачи с on:/where: + loop: (слайс E снят — fan-out наследует изоляцию destiny
// через loopInvariantVars: AllowHosts=false, Register пуст). Остальные вложенные
// конструкции — слайсы C/…. include: внутри
// destiny раскрывается ДО render (within-destiny, в DestinyLoader.parseTasks /
// fixture-резолвере); дошедший до render include — ErrUnexpandedInclude (баг
// раскрытия), не «вне pilot».
func guardDestinyTask(task config.Task, idx int, destiny string) error {
	switch {
	case task.Apply != nil:
		return fmt.Errorf("%w: вложенный apply: в destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.Include != nil:
		return fmt.Errorf("%w: в destiny %q (task[%d] %q)", ErrUnexpandedInclude, destiny, idx, task.Name)
	case task.Block != nil:
		return fmt.Errorf("%w: block: в destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.Parallel:
		return fmt.Errorf("%w: parallel: в destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.RunOnce:
		return fmt.Errorf("%w: run_once: в destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.Serial != nil:
		return fmt.Errorf("%w: serial: в destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
	case task.Module == nil:
		return fmt.Errorf("%w: task[%d] %q в destiny %q не является module-задачей", ErrUnsupportedDSL, idx, task.Name, destiny)
	}
	return nil
}
