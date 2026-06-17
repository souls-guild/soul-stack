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

	tasks := make([]*RenderedTask, 0, len(resolved.Tasks))
	plans := make([]DispatchPlan, 0, len(resolved.Tasks))
	idx := startIndex
	for i := range resolved.Tasks {
		task := resolved.Tasks[i]
		if gerr := guardDestinyTask(task, i, resolved.Name); gerr != nil {
			return nil, nil, gerr
		}

		destinyTargeted, terr := resolveTargets(p.cel, destinyIn, task)
		if terr != nil {
			return nil, nil, terr
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
// (block:/loop:/parallel:/вложенный apply:) и scenario-only ключи на destiny-
// задаче (serial:/run_once: — недопустимы в destiny, docs/destiny/tasks.md §3;
// serial: scenario-уровня наследуется destiny через параметр renderApplyDestiny,
// не через поле destiny-задачи). Пилот поддерживает плоский destiny: module-
// задачи с on:/where:. Вложенные конструкции — слайсы C/E. include: внутри
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
	case task.Loop != nil:
		return fmt.Errorf("%w: loop: в destiny %q (task[%d] %q)", ErrUnsupportedDSL, destiny, idx, task.Name)
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
