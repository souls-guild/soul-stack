package config

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Task — полиморфная задача scenario.
//
// Дискриминатор — присутствие ровно одного из ключей `module:` / `apply:` /
// `include:` / `block:`. Поле-discriminator non-nil после Unmarshal указывает
// вид задачи; остальные три должны быть nil. Mutual exclusion проверяется
// в validateTaskNode (через AST, не через struct-fields — это даёт line/col
// для диагностики).
//
// Все «общие» поля DSL-ядра (destiny/tasks.md §3) тут — opaque (any/map[string]any),
// потому что на уровне shared/config мы валидируем только структуру/типы/regex,
// CEL-разбор и cross-ref-проверки откладываются на M1.3/M1.5. В рантайме их
// заполнит/типизирует апплаер scenario-DSL.
type Task struct {
	// Common (DSL-ядро задач).
	Name        string         `yaml:"name,omitempty"`
	Vars        map[string]any `yaml:"vars,omitempty"`
	When        string         `yaml:"when,omitempty"`
	Parallel    bool           `yaml:"parallel,omitempty"`
	Loop        *LoopSpec      `yaml:"loop,omitempty"`
	Register    string         `yaml:"register,omitempty"`
	ID          string         `yaml:"id,omitempty"`
	Output      map[string]any `yaml:"output,omitempty"`
	NoLog       bool           `yaml:"no_log,omitempty"`
	OnChanges   []string       `yaml:"onchanges,omitempty"`
	OnFail      []string       `yaml:"onfail,omitempty"`
	Require     any            `yaml:"require,omitempty"` // []string OR "all"
	ChangedWhen string         `yaml:"changed_when,omitempty"`
	FailedWhen  string         `yaml:"failed_when,omitempty"`
	Retry       *RetrySpec     `yaml:"retry,omitempty"`
	Timeout     string         `yaml:"timeout,omitempty"`

	// Scenario-дельта (orchestration.md §2).
	On      any    `yaml:"on,omitempty"`     // "keeper" | []string
	Where   string `yaml:"where,omitempty"`  // CEL string
	Serial  any    `yaml:"serial,omitempty"` // int >= 1 | "<N>%"
	RunOnce bool   `yaml:"run_once,omitempty"`

	// Discriminator (ровно один non-nil).
	Module  *ModuleTask  `yaml:"module,omitempty"`
	Apply   *ApplyTask   `yaml:"apply,omitempty"`
	Include *IncludeTask `yaml:"include,omitempty"`
	Block   *BlockTask   `yaml:"block,omitempty"`
	Assert  *AssertSpec  `yaml:"assert,omitempty"`

	// Carry-through conditional-include (ADR-009 amendment, conditional-include
	// group-drop). НЕ-YAML поля (`yaml:"-"` → не парсятся из манифеста, не попадают
	// в taskKnownKeys/yamlFieldIndex — forward-compat как RenderInput.destinyIsolated).
	// Заполняются ТОЛЬКО [ExpandIncludes] при раскрытии include с `when:`: include-
	// when и id группы протаскиваются в КАЖДУЮ вклеенную задачу. Keeper-side render
	// дропает всю группу одним вычислением include-when (по IncludeGroupID), ДО
	// emitStaticWhenSkip. IncludeGroupID==0 — задача вне условного include (обычный
	// путь); IncludeWhen непустой ⇔ IncludeGroupID!=0.
	IncludeWhen    string `yaml:"-"`
	IncludeGroupID int    `yaml:"-"`
}

// AssertSpec — keeper-side render-time precondition прогона (ADR-009 amendment
// 2026-06-23). `that` — список CEL-bool-предикатов (вся строка = CEL без обёртки,
// как `where:`), все обязаны быть true на render-фазе Keeper (полный scenario-
// контекст, soulprint.hosts доступен — AllowHosts=true). Первый false обрывает
// render понятной ошибкой; assert НЕ emit RenderedTask (проверка, не задача).
// Дискриминатор задачи — взаимоисключим с module/apply/include/block.
type AssertSpec struct {
	That    []string `yaml:"that"`
	Message string   `yaml:"message,omitempty"`
}

// ModuleTask — задача-вызов state-модуля. `params:` живёт здесь же
// (на уровне DSL-ядра он привязан к module-задаче — destiny/tasks.md §4).
type ModuleTask struct {
	// Module — строковый идентификатор модуля «<ns>.<module>.<state>».
	Module string         `yaml:"-"`
	Params map[string]any `yaml:"params,omitempty"`
}

// ApplyTask — applier-задача, делегирующая работу в destiny.
type ApplyTask struct {
	Destiny string         `yaml:"destiny"`
	Input   map[string]any `yaml:"input,omitempty"`
}

// IncludeTask — подключение соседнего scenario-файла (или service-level
// fallback по двухуровневому резолву, orchestration.md §6).
type IncludeTask struct {
	// Include — относительное имя файла (например, `install.yml`).
	Include string `yaml:"-"`
}

// BlockTask — inline-группа задач. Содержимое — top-level список Task,
// то же DSL-ядро рекурсивно.
type BlockTask struct {
	Block []Task `yaml:"-"`
}

// LoopSpec — DSL-ядро §7. На уровне shared/config валидируем только структуру;
// type/значение `items` — CEL/template, разбор отложен.
type LoopSpec struct {
	Items   any    `yaml:"items"`
	As      string `yaml:"as,omitempty"`
	IndexAs string `yaml:"index_as,omitempty"`
	When    string `yaml:"when,omitempty"`
}

// RetrySpec — DSL-ядро §9.
type RetrySpec struct {
	Count int    `yaml:"count"`
	Delay string `yaml:"delay,omitempty"`
	Until string `yaml:"until,omitempty"`
}

// taskCommonStringFields — common string-поля задачи (DSL-ядро §3 + scenario-
// дельта `where:`). Используется в validateTaskNode для проверки, что в YAML-
// узле под ключом стоит строка, а не int/bool/seq/map. goccy при NodeToValue
// в string-поле молча коэрсит integer/bool scalar в строку, поэтому без
// AST-проверки `name: 42` проходит как валидный.
//
// `changed_when:`/`failed_when:` сюда НЕ входят: они принимают и bool-литерал
// (force-shortcut), и CEL-строку — отдельная проверка taskBoolOrCELFields.
var taskCommonStringFields = map[string]bool{
	"name":     true,
	"when":     true,
	"register": true,
	"id":       true,
	"timeout":  true,
	"where":    true,
}

// taskBoolOrCELFields — поля-override результата задачи (`changed_when:` /
// `failed_when:`, destiny/tasks.md §«changed_when»/§«failed_when»). Допустимы
// две формы: bool-литерал (`false`/`true` — константный force-shortcut,
// «никогда не изменяет state» / «никогда не падает») ИЛИ CEL-строка
// (выражение-предикат). int/float/seq/map — ошибка типа.
var taskBoolOrCELFields = map[string]bool{
	"changed_when": true,
	"failed_when":  true,
}

// taskKnownKeys — union из всех легальных task-level ключей. Включает:
//   - common DSL-ядра (yaml-теги Task struct, кроме discriminator-полей
//     с тегом `yaml:"-"`);
//   - discriminator-ключи (`module`, `apply`, `include`, `block`) — у них
//     yaml:"-", потому что их декодит сам UnmarshalYAML;
//   - `params:` — сосед `module:`, технически живёт в ModuleTask;
//   - scenario-delta (`on`, `where`, `serial`, `run_once`);
//   - deprecated (`wait`, `filter`) — для них диагностика поднимается отдельно
//     с hint-ом в шаге 1 validateTaskNode, но в whitelist они нужны, чтобы
//     не подняться дубль-`unknown_key`.
var taskKnownKeys = func() map[string]bool {
	out := map[string]bool{
		// discriminator-ключи (yaml:"-" в struct → не попадают в yamlFieldIndex).
		"module":  true,
		"apply":   true,
		"include": true,
		"block":   true,
		// `params:` — сосед `module:`, валидируется через ModuleTask.
		"params": true,
	}
	for k := range yamlFieldIndex(reflect.TypeOf(Task{})) {
		out[k] = true
	}
	for k := range deprecatedTaskKeys {
		out[k] = true
	}
	return out
}()

// Регэксы валидации.
var (
	// reModuleAddress — 3-level kebab-case `<ns>.<module>.<state>` для
	// scenario-module-задачи. Симметрично reRequiredModule (destiny.go),
	// но добавляет третий сегмент `<state>` — destiny/tasks.md §4.
	reModuleAddress = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*\.[a-z][a-z0-9]*(-[a-z0-9]+)*\.[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

	// reIncludeFile — имя include-файла. Только `.yml`-расширение, без `/`
	// и без `..` (двухуровневый резолв делает движок, autor никогда не пишет
	// `../`, orchestration.md §6).
	reIncludeFile = regexp.MustCompile(`^[a-z][a-z0-9_-]*\.yml$`)

	// reRegisterID — identifier для `register:` и для `id:` (стабильный адрес
	// задачи для алертов «таска X изменила», ADR-009-amend). Один формат —
	// потому что register и id живут в ОДНОМ адресном пространстве подписки на
	// per-task-changed-события (ADR-052 §h). Совпадает с reInputParamName, но
	// переопределён отдельно — это разные пространства имён (register/id vs
	// input-параметры), хоть форма и одинаковая.
	reRegisterID = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	// reLoopVar — identifier для `loop.as:`/`loop.index_as:`: snake_case,
	// потому что имя становится голой CEL-переменной (`${ <as>.* }` /
	// `<as>.*` в expression-keys, destiny/tasks.md §7). Форма та же, что у
	// register-id.
	reLoopVar = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// SplitModuleAddr разбирает module-адрес `<namespace>.<module>.<state>` на
// (`<namespace>.<module>`, `state`). Последняя точка отделяет state-суффикс;
// при его отсутствии возвращает (addr, "", true) — модуль без конкретного
// state (legacy-плагины). Бракованные строки (пустая, `.state`, `core.`)
// возвращают ok=false.
//
// Единый источник правды разбора module-адреса для всех бинарей: Soul-side
// runtime (plantask/applyrunner) и Keeper-side scenario-dispatch вызывают эту
// функцию вместо локальных копий. Симметрично reModuleAddress, который
// валидирует ту же трёхсегментную форму статически.
func SplitModuleAddr(addr string) (name, state string, ok bool) {
	if addr == "" {
		return "", "", false
	}
	idx := strings.LastIndexByte(addr, '.')
	if idx < 0 {
		// Точки нет — модуль без state (`core`).
		return addr, "", true
	}
	if idx == 0 || idx == len(addr)-1 {
		// `.state` / `core.` — бракованные.
		return "", "", false
	}
	return addr[:idx], addr[idx+1:], true
}

// loopReservedNames — имена контекста CEL, которые `loop.as:`/`loop.index_as:`
// перекрывать нельзя: голая loop-переменная объявляется на верхнем уровне
// активации (shared/cel) и затёрла бы фиксированный контекст. Симметрично
// contextVars в shared/cel/engine.go.
var loopReservedNames = map[string]bool{
	"input":       true,
	"register":    true,
	"incarnation": true,
	"soulprint":   true,
	"essence":     true,
	"vars":        true,
}

// loopReservedPrefixes — префиксы имён, зарезервированные за служебными iter-
// переменными движка. `__host` — iter-переменная filter-comprehension, в которую
// раскрывается `soulprint.hosts.where(...)` (shared/cel/hosts.go, hostIterPrefix).
// loop-переменная с таким префиксом коллизила бы с встроенной iter-переменной,
// если бы попала в одно выражение → запрещаем на уровне config-валидатора.
var loopReservedPrefixes = []string{"__host"}

// UnmarshalYAML — кастомный декод Task. Стандартный reflect-decode goccy не
// может справиться с дискриминатором (`module:` — скаляр в YAML, *ModuleTask
// в Go), поэтому переопределяем: общие поля декодируем через alias-тип,
// специальные (`module:`/`include:`/`block:`/`params:`) — вручную.
func (t *Task) UnmarshalYAML(node ast.Node) error {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		// Сценарий-задача — scalar/sequence: декод не делает ничего, оставляя
		// зеro-value Task. Диагностику `type_mismatch` с координатами поднимет
		// validateTaskNode. Если возвращать error — поверх него встаёт ещё
		// `decode_fault` от parseAndValidate, и на ту же координату приходит
		// двойная диагностика.
		return nil
	}

	// Снимаем «специальные» ключи в отдельные ноды и формируем filtered-map
	// без них для прохода через alias-тип.
	var (
		moduleNode  ast.Node
		paramsNode  ast.Node
		includeNode ast.Node
		blockNode   ast.Node
	)
	filtered := &ast.MappingNode{
		BaseNode:    mm.BaseNode,
		Start:       mm.Start,
		End:         mm.End,
		IsFlowStyle: mm.IsFlowStyle,
		Values:      make([]*ast.MappingValueNode, 0, len(mm.Values)),
	}
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok != nil {
			switch tok.Value {
			case "module":
				moduleNode = kv.Value
				continue
			case "params":
				paramsNode = kv.Value
				continue
			case "include":
				includeNode = kv.Value
				continue
			case "block":
				blockNode = kv.Value
				continue
			}
		}
		filtered.Values = append(filtered.Values, kv)
	}

	// Декод «всего остального» через alias-тип (избегает рекурсии).
	type rawTask Task
	var raw rawTask
	if err := yaml.NodeToValue(filtered, &raw); err != nil {
		return err
	}
	*t = Task(raw)

	// module: <string> → ModuleTask с возможным params:.
	if moduleNode != nil {
		mt := &ModuleTask{}
		if sn, ok := moduleNode.(*ast.StringNode); ok {
			mt.Module = sn.Value
		}
		if paramsNode != nil {
			if pm, ok := paramsNode.(*ast.MappingNode); ok {
				params := map[string]any{}
				if err := yaml.NodeToValue(pm, &params); err == nil {
					mt.Params = params
				}
			}
		}
		t.Module = mt
	} else if paramsNode != nil {
		// `params:` без `module:` — нелегально (params привязан к module-задаче),
		// но валидацию делает validateTaskNode по AST. Здесь ничего не делаем,
		// чтобы не потерять диагностику.
		_ = paramsNode
	}

	// include: <string> → IncludeTask.
	if includeNode != nil {
		it := &IncludeTask{}
		if sn, ok := includeNode.(*ast.StringNode); ok {
			it.Include = sn.Value
		}
		t.Include = it
	}

	// block: [tasks...] → BlockTask.
	if blockNode != nil {
		bt := &BlockTask{}
		if seq, ok := blockNode.(*ast.SequenceNode); ok {
			bt.Block = make([]Task, len(seq.Values))
			for i, item := range seq.Values {
				if err := bt.Block[i].UnmarshalYAML(item); err != nil {
					return fmt.Errorf("block[%d]: %w", i, err)
				}
			}
		}
		t.Block = bt
	}

	return nil
}

// validateTaskNode — валидация одного элемента `tasks[]` (или элемента внутри
// `block:`). Принимает AST-узел (нужны line/col), а не уже декодированный
// Task — discriminator-проверка и unknown_key читаются прямо по AST.
//
// Возвращает накопленные диагностики. Никогда не возвращает ошибку: всё, что
// не получилось разобрать, фиксируется как diag.Diagnostic уровня error.
func validateTaskNode(item ast.Node, pathPrefix string) []diag.Diagnostic {
	mm, ok := item.(*ast.MappingNode)
	if !ok {
		tok := item.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "scenario task must be a mapping",
			YAMLPath: pathPrefix,
		})}
	}

	var out []diag.Diagnostic

	// Соберём присутствующие ключи и их value-ноды (для диагностики позиций).
	present := map[string]*ast.MappingValueNode{}
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		present[tok.Value] = kv
	}

	// 1) Deprecated task-level ключи (`wait:` / `filter:`) и unknown_key для
	// всего, что не в whitelist. Симметрично top-level destiny/service/scenario:
	// без этой проверки опечатки `wheree`/`reigster` проходили exit 0.
	for k, kv := range present {
		if hint, dep := deprecatedTaskKeys[k]; dep {
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + k + `"`,
				Hint:     hint,
				YAMLPath: pathPrefix + "." + k,
			}))
			continue
		}
		if !taskKnownKeys[k] {
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + k + `"`,
				Hint:     "see docs/destiny/tasks.md §3 and docs/scenario/orchestration.md §2 for the full list of task keys",
				YAMLPath: pathPrefix + "." + k,
			}))
		}
	}

	// 1a) Common string-поля: name/when/register/timeout/where должны быть
	// строками (changed_when/failed_when вынесены в 1b — bool-литерал ИЛИ CEL).
	// goccy при NodeToValue коэрсит
	// scalar int/bool/float в строковое поле молча, поэтому проверяем по AST.
	for k, kv := range present {
		if !taskCommonStringFields[k] {
			continue
		}
		if _, isStr := kv.Value.(*ast.StringNode); isStr {
			continue
		}
		// null допустим — это «поле не задано», такой же эффект как отсутствие.
		if _, isNull := kv.Value.(*ast.NullNode); isNull {
			continue
		}
		vt := kv.Value.GetToken()
		line, col := 0, 0
		if vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("%s: must be a string", k),
			YAMLPath: pathPrefix + "." + k,
		}))
	}

	// 1b) changed_when/failed_when: bool-литерал (force-shortcut) ИЛИ CEL-строка.
	// `changed_when: false`/`failed_when: false` — идиоматичный шорткат (read-only
	// шаг / ignore-errors, destiny/tasks.md). int/float/seq/map — type_mismatch.
	for k, kv := range present {
		if !taskBoolOrCELFields[k] {
			continue
		}
		switch kv.Value.(type) {
		case *ast.StringNode, *ast.BoolNode, *ast.NullNode:
			// string = CEL-выражение; bool = константный force; null = «не задано».
			continue
		}
		vt := kv.Value.GetToken()
		line, col := 0, 0
		if vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("%s: must be a bool literal (force) or a CEL string (expression)", k),
			Hint:     "changed_when: false (force not-changed) | changed_when: \"<cel>\"",
			YAMLPath: pathPrefix + "." + k,
		}))
	}

	// 2) Discriminator: ровно один из {module, apply, include, block, assert}.
	// `assert:` — render-time precondition (ADR-009 amendment): проверка, а НЕ
	// исполняемая задача, поэтому делит дискриминатор-слот с остальными видами
	// (взаимоисключим с module/apply/include/block).
	discrKeys := []string{"module", "apply", "include", "block", "assert"}
	var found []string
	for _, k := range discrKeys {
		if _, ok := present[k]; ok {
			found = append(found, k)
		}
	}
	switch {
	case len(found) == 0:
		tok := mm.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "task_discriminator_missing",
			Message:  "task must declare exactly one of: module / apply / include / block / assert",
			Hint:     "see docs/destiny/tasks.md §2 and docs/scenario/orchestration.md §2",
			YAMLPath: pathPrefix,
		}))
	case len(found) > 1:
		// Диагностика на ключе второго+ дискриминатора; первый считаем «основным».
		for i := 1; i < len(found); i++ {
			kv := present[found[i]]
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "task_discriminator_multiple",
				Message:  fmt.Sprintf("task declares multiple discriminators: %v — exactly one of {module, apply, include, block, assert} is allowed", found),
				Hint:     "see docs/destiny/tasks.md §2",
				YAMLPath: pathPrefix + "." + found[i],
			}))
		}
	}

	// 3) Discriminator-specific валидация.
	if kv, ok := present["module"]; ok {
		out = append(out, validateModuleField(kv, pathPrefix)...)
		// `params:` — обязателен у module-задачи (хоть и `{}`). Без него
		// апплаер не отличит «забыли передать» от «передали `{}`». См.
		// docs/destiny/tasks.md §4. Тип `params:` валидируется через
		// ModuleTask.Params (map[string]any).
		if _, hasParams := present["params"]; !hasParams {
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "missing_required_field",
				Message:  "params: is required on a module task (use params: {} if there are no inputs)",
				Hint:     "module: <ns>.<module>.<state>\\n    params: { ... }  # or {}",
				YAMLPath: pathPrefix + ".params",
			}))
		}
		// Фаза проверки params против manifest-схемы модуля (core-модули).
		out = append(out, validateModuleParams(kv, present["params"], pathPrefix)...)
	}
	if kv, ok := present["apply"]; ok {
		out = append(out, validateApplyField(kv, pathPrefix)...)
	}
	if kv, ok := present["assert"]; ok {
		out = append(out, validateAssertField(kv, pathPrefix)...)
	}
	if kv, ok := present["include"]; ok {
		out = append(out, validateIncludeField(kv, pathPrefix)...)
	}
	if kv, ok := present["block"]; ok {
		out = append(out, validateBlockField(kv, pathPrefix)...)
		// `register:` на block-задаче семантически бессмыслен (block не
		// вызывает модуль), destiny/tasks.md §6.5. Поднимаем отдельный код.
		if rkv, hasReg := present["register"]; hasReg {
			tok := rkv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "register_on_block_invalid",
				Message:  "register: is not allowed on a block task (block does not invoke a module)",
				Hint:     "place register: on the inner module-task that produces the value",
				YAMLPath: pathPrefix + ".register",
			}))
		}
		out = append(out, validateBlockForbiddenKeys(present, pathPrefix)...)
	}

	// 4) Scenario-дельта: on / serial / run_once. `where:` уже проверен в
	// блоке 1a (общая проверка строковых полей); CEL-разбор отложен на M1.3.
	if kv, ok := present["on"]; ok {
		out = append(out, validateOnField(kv, pathPrefix)...)
	}
	if kv, ok := present["serial"]; ok {
		out = append(out, validateSerialField(kv, pathPrefix)...)
	}
	// serial: и run_once: взаимоисключающи (orchestration.md §2.2.2 «`run_once`»).
	_, hasSerial := present["serial"]
	_, hasRunOnce := present["run_once"]
	if hasSerial && hasRunOnce {
		// Диагностика на run_once (позиция второго ключа), как для discriminator.
		kv := present["run_once"]
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "serial_run_once_conflict",
			Message:  "serial: and run_once: are mutually exclusive (different width strategies)",
			Hint:     "use serial: for rolling waves of N hosts; run_once: for a single deterministic host",
			YAMLPath: pathPrefix + ".run_once",
		}))
	}

	// 4a) Requisite-поля onchanges/onfail/require — тип. onchanges/onfail —
	// строго list-of-string; require — list-of-string ИЛИ скаляр "all". Без
	// этой проверки скаляр вместо списка (`onchanges: redis_conf`) проходил
	// молча: cross-ref в validateTaskRefs смотрит только sequence-форму и
	// skip-ает скаляр, поэтому опечатка-в-форме не ловилась нигде.
	for _, k := range []string{"onchanges", "onfail"} {
		if kv, ok := present[k]; ok {
			out = append(out, validateRequisiteListField(kv, k, pathPrefix)...)
		}
	}
	if kv, ok := present["require"]; ok {
		out = append(out, validateRequireField(kv, pathPrefix)...)
	}

	// 5) Универсальные поля: loop, register, retry, timeout.
	if kv, ok := present["loop"]; ok {
		out = append(out, validateLoopField(kv, present, pathPrefix)...)
	}
	if kv, ok := present["register"]; ok {
		out = append(out, validateRegisterField(kv, pathPrefix)...)
	}
	if kv, ok := present["id"]; ok {
		out = append(out, validateIDField(kv, present, pathPrefix)...)
	}
	if kv, ok := present["retry"]; ok {
		out = append(out, validateRetryField(kv, pathPrefix)...)
	}
	if kv, ok := present["timeout"]; ok {
		// Тип-проверка (string) уже сделана в блоке 1a; повторно type_mismatch
		// не эмитим, только формат.
		if _, isStr := kv.Value.(*ast.StringNode); isStr {
			out = append(out, validateDurationField(kv, pathPrefix+".timeout")...)
		}
	}

	return out
}

// validateModuleField — `module: <ns>.<module>.<state>` строка + опциональный
// `params:`-сосед (params валидируется по схеме модуля на M1.5, сейчас только
// тип map проверяем неявно через UnmarshalYAML).
func validateModuleField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	sn, ok := kv.Value.(*ast.StringNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "module: must be a string in form <namespace>.<module>.<state>",
			YAMLPath: pathPrefix + ".module",
		})}
	}
	if !reModuleAddress.MatchString(sn.Value) {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "module_format_invalid",
			Message:  fmt.Sprintf("module %q does not match <namespace>.<module>.<state>", sn.Value),
			Hint:     "three-level kebab-case address; e.g. core.pkg.installed, core.file.rendered",
			YAMLPath: pathPrefix + ".module",
		})}
	}
	return nil
}

// validateApplyField — apply:-задача: `destiny:` + `input:`-сосед.
func validateApplyField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	mm, ok := kv.Value.(*ast.MappingNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "apply: must be a mapping with destiny: + input:",
			YAMLPath: pathPrefix + ".apply",
		})}
	}
	var out []diag.Diagnostic
	var hasDestiny bool
	for _, sub := range mm.Values {
		tok := sub.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "destiny":
			hasDestiny = true
			sn, isStr := sub.Value.(*ast.StringNode)
			if !isStr {
				vt := sub.Value.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  "apply.destiny must be a string",
					YAMLPath: pathPrefix + ".apply.destiny",
				}))
				continue
			}
			if !reDestinyName.MatchString(sn.Value) {
				vt := sn.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "name_invalid_format",
					Message:  fmt.Sprintf("apply.destiny %q does not match %s", sn.Value, reDestinyName),
					Hint:     "kebab-case destiny name from service.yml → destiny[]",
					YAMLPath: pathPrefix + ".apply.destiny",
				}))
			}
		case "input":
			if _, isMap := sub.Value.(*ast.MappingNode); !isMap {
				// `input: null` — допустимо (нет входа). `input: <scalar>` —
				// type_mismatch. Для null goccy создаст nil-узел; пропускаем.
				if _, isNull := sub.Value.(*ast.NullNode); !isNull {
					vt := sub.Value.GetToken()
					out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "type_mismatch",
						Message:  "apply.input must be a mapping",
						YAMLPath: pathPrefix + ".apply.input",
					}))
				}
			}
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in apply:`,
				Hint:     "apply: accepts only destiny: + input:",
				YAMLPath: pathPrefix + ".apply." + tok.Value,
			}))
		}
	}
	if !hasDestiny {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "apply.destiny is required",
			Hint:     "apply: { destiny: <name>, input: { ... } }",
			YAMLPath: pathPrefix + ".apply.destiny",
		}))
	}
	return out
}

// validateAssertField — assert-задача (render-time precondition, ADR-009
// amendment 2026-06-23): `assert: { that: [<CEL-bool>…], message?: <str> }`.
//
// `that` — обязателен, непустой список строк (каждая строка целиком = CEL-bool,
// разбор CEL отложен на render-фазу — как `where:`). `message` — опционален,
// строка (дефолт-сообщение, если опущен). Прочие ключи внутри assert: —
// unknown_key (fail-closed).
func validateAssertField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	mm, ok := kv.Value.(*ast.MappingNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "assert: must be a mapping with that: + optional message:",
			YAMLPath: pathPrefix + ".assert",
		})}
	}
	var out []diag.Diagnostic
	var hasThat bool
	for _, sub := range mm.Values {
		tok := sub.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "that":
			hasThat = true
			seq, isSeq := sub.Value.(*ast.SequenceNode)
			if !isSeq {
				vt := sub.Value.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  "assert.that must be a non-empty list of CEL-bool predicate strings",
					YAMLPath: pathPrefix + ".assert.that",
				}))
				continue
			}
			if len(seq.Values) == 0 {
				vt := sub.Value.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "missing_required_field",
					Message:  "assert.that must not be empty (at least one CEL-bool predicate)",
					Hint:     "assert: { that: [ \"<cel>\" ], message: \"...\" }",
					YAMLPath: pathPrefix + ".assert.that",
				}))
				continue
			}
			for j, item := range seq.Values {
				if _, isStr := item.(*ast.StringNode); isStr {
					continue
				}
				it := item.GetToken()
				line, col := 0, 0
				if it != nil {
					line, col = it.Position.Line, it.Position.Column
				}
				out = append(out, diagAt(line, col, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  fmt.Sprintf("assert.that[%d]: must be a string (CEL-bool predicate)", j),
					YAMLPath: fmt.Sprintf("%s.assert.that[%d]", pathPrefix, j),
				}))
			}
		case "message":
			if _, isStr := sub.Value.(*ast.StringNode); !isStr {
				if _, isNull := sub.Value.(*ast.NullNode); !isNull {
					vt := sub.Value.GetToken()
					out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "type_mismatch",
						Message:  "assert.message must be a string",
						YAMLPath: pathPrefix + ".assert.message",
					}))
				}
			}
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in assert:`,
				Hint:     "assert: accepts only that: + message:",
				YAMLPath: pathPrefix + ".assert." + tok.Value,
			}))
		}
	}
	if !hasThat {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "assert.that is required",
			Hint:     "assert: { that: [ \"<cel>\" ], message: \"...\" }",
			YAMLPath: pathPrefix + ".assert.that",
		}))
	}
	return out
}

// validateIncludeField — `include: <file>.yml`.
func validateIncludeField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	sn, ok := kv.Value.(*ast.StringNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "include: must be a string (file name)",
			YAMLPath: pathPrefix + ".include",
		})}
	}
	if !reIncludeFile.MatchString(sn.Value) {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "name_invalid_format",
			Message:  fmt.Sprintf("include %q must be a sibling file name ending in .yml (no slashes, no ..)", sn.Value),
			Hint:     "two-level resolve is done by the engine; authors never write ../ — see orchestration.md §6",
			YAMLPath: pathPrefix + ".include",
		})}
	}
	return nil
}

// validateBlockField — `block:` — массив вложенных задач, рекурсивная валидация.
func validateBlockField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	seq, ok := kv.Value.(*ast.SequenceNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "block: must be a sequence of tasks",
			YAMLPath: pathPrefix + ".block",
		})}
	}
	var out []diag.Diagnostic
	for i, item := range seq.Values {
		out = append(out, validateTaskNode(item, fmt.Sprintf("%s.block[%d]", pathPrefix, i))...)
	}
	return out
}

// blockForbiddenKeys — module-специфичные ключи, недопустимые на BLOCK-уровне
// (fail-closed, destiny/tasks.md §6.5 их на block не упоминает). block не
// вызывает модуль, поэтому override результата модуля (`changed_when`/
// `failed_when`), retry/timeout/output/no_log одного вызова и `params:` (аргументы
// модуля) на нём бессмысленны. Каждый ключ режется кодом `<key>_on_block_invalid`
// (симметрично register_on_block_invalid). `register:` уже режется отдельно выше.
//
// `parallel:` тоже вне pilot block-а (parallel на block — слайс позже) — режется
// тем же механизмом, кодом parallel_on_block_invalid.
//
// Унаследованные block-ом ключи (`when`/`where`/`vars`/`onchanges`/`onfail`/
// `require`/`on`/`serial`/`run_once`/`name`/`loop`) в список НЕ входят: §6.5
// явно допускает их на block.
var blockForbiddenKeys = []string{
	"changed_when",
	"failed_when",
	"retry",
	"timeout",
	"output",
	"no_log",
	"params",
	"parallel",
}

// validateBlockForbiddenKeys поднимает ошибку `<key>_on_block_invalid` для каждого
// присутствующего module-специфичного ключа на block-задаче (fail-closed, §6.5).
// Вызывается только когда дискриминатор — block.
func validateBlockForbiddenKeys(present map[string]*ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, key := range blockForbiddenKeys {
		kv, ok := present[key]
		if !ok {
			continue
		}
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     key + "_on_block_invalid",
			Message:  fmt.Sprintf("%s: is not allowed on a block task (block does not invoke a module — see docs/destiny/tasks.md §6.5)", key),
			Hint:     "place module-specific keys on the inner module-task; block: only carries when/where/vars/requisites/serial/run_once",
			YAMLPath: pathPrefix + "." + key,
		}))
	}
	return out
}

// validateOnField — `on:` literal `keeper` или sequence строк (coven-id-ы).
func validateOnField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	switch v := kv.Value.(type) {
	case *ast.StringNode:
		// `on: keeper` — единственная допустимая строковая форма (orchestration.md §3).
		if v.Value != "keeper" {
			tok := v.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "enum_invalid",
				Message:  fmt.Sprintf("on: %q — only 'keeper' is allowed as scalar; use a sequence of coven-ids otherwise", v.Value),
				Hint:     "on: keeper | on: [coven-a, coven-b]",
				YAMLPath: pathPrefix + ".on",
			})}
		}
		return nil
	case *ast.SequenceNode:
		var out []diag.Diagnostic
		for i, item := range v.Values {
			itemPath := fmt.Sprintf("%s.on[%d]", pathPrefix, i)
			sn, ok := item.(*ast.StringNode)
			if !ok {
				tok := item.GetToken()
				out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  "on[]: must be strings (coven-ids)",
					YAMLPath: itemPath,
				}))
				continue
			}
			// CEL-обёртку `${ ... }` пропускаем — это валидный coven-резолвер
			// (например, `${ incarnation.name }`). Regex-форму применяем только
			// к голым именам.
			if !isCELWrapped(sn.Value) {
				if !reCovenName.MatchString(sn.Value) {
					tok := sn.GetToken()
					out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "name_invalid_format",
						Message:  fmt.Sprintf("on[%d] %q is not a valid coven-id (kebab-case)", i, sn.Value),
						Hint:     "kebab-case literal or CEL ${ ... } expression",
						YAMLPath: itemPath,
					}))
				} else if err := covenLabelValidator().Validate(sn.Value); err != nil {
					// Опциональный хук поверх формата: подменяемый
					// CovenLabelValidator (Q1b справочник, ADR-008-amend). В
					// пилоте — no-op; реальная подмена приходит через
					// SetCovenLabelValidator на старте clienta.
					tok := sn.GetToken()
					out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
						Code:     "coven_label_unknown",
						Message:  fmt.Sprintf("on[%d] %q is not a recognized coven label: %v", i, sn.Value, err),
						Hint:     "check the spelling against the coven registry; until the registry exists, only kebab-case format is enforced",
						YAMLPath: itemPath,
					}))
				}
			}
		}
		return out
	default:
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "on: must be the string 'keeper' or a sequence of coven-id strings",
			YAMLPath: pathPrefix + ".on",
		})}
	}
}

// validateSerialField — int >= 1 или percent-string `"<N>%"`.
func validateSerialField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	switch v := kv.Value.(type) {
	case *ast.IntegerNode:
		// goccy типизирует целые как int64/uint64 через GetValue. Используем
		// строковое представление токена для портативности.
		tok := v.GetToken()
		// Грубая, но достаточная проверка: токен не должен быть "0" или
		// отрицательным. Для отрицательных goccy парсит знак как часть токена.
		if tok.Value == "0" || (len(tok.Value) > 0 && tok.Value[0] == '-') {
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "value_out_of_range",
				Message:  fmt.Sprintf("serial: %s must be >= 1", tok.Value),
				YAMLPath: pathPrefix + ".serial",
			})}
		}
		return nil
	case *ast.StringNode:
		if _, ok := ParseSerialPercent(v.Value); !ok {
			tok := v.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "value_out_of_range",
				Message:  fmt.Sprintf("serial: %q must match int >= 1 OR percent-form \"<N>%%\" (1..99)", v.Value),
				Hint:     "examples: serial: 1, serial: 3, serial: \"25%\"",
				YAMLPath: pathPrefix + ".serial",
			})}
		}
		return nil
	default:
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "serial: must be int >= 1 or percent-form \"<N>%\"",
			YAMLPath: pathPrefix + ".serial",
		})}
	}
}

// validateRegisterField — identifier. Тип-проверка (string) сделана в общем
// блоке 1a validateTaskNode; здесь — только формат.
func validateRegisterField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	sn, ok := kv.Value.(*ast.StringNode)
	if !ok {
		return nil
	}
	if !reRegisterID.MatchString(sn.Value) {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "register_identifier_invalid",
			Message:  fmt.Sprintf("register %q does not match %s", sn.Value, reRegisterID),
			Hint:     "snake_case identifier: starts with a-z; only [a-z0-9_]",
			YAMLPath: pathPrefix + ".register",
		})}
	}
	return nil
}

// validateIDField — `id:` — стабильный адрес задачи для подписки на алерты
// «таска X изменила» (ADR-009-amend, ADR-052 §h). Опционален. Тип-проверка
// (string) сделана в общем блоке 1a validateTaskNode; здесь — формат +
// взаимоисключения.
//
// Формат — register-формат (тот же reRegisterID): id и register живут в ОДНОМ
// адресном пространстве подписки.
//
// Взаимоисключения:
//   - `id` ⊕ `register`: задача с register уже адресуема по нему → id избыточен
//     и создаёт двусмысленность адреса. Ошибка id_register_conflict.
//   - `id` только на module-задаче (pilot): block/include своего changed-сигнала
//     не имеют. На них id пока запрещён (ошибка id_unsupported_target); снятие
//     ограничения — отдельный заход при запросе.
func validateIDField(kv *ast.MappingValueNode, present map[string]*ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	sn, ok := kv.Value.(*ast.StringNode)
	if !ok {
		return nil
	}

	var out []diag.Diagnostic

	if !reRegisterID.MatchString(sn.Value) {
		tok := sn.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "id_identifier_invalid",
			Message:  fmt.Sprintf("id %q does not match %s", sn.Value, reRegisterID),
			Hint:     "snake_case identifier: starts with a-z; only [a-z0-9_]",
			YAMLPath: pathPrefix + ".id",
		}))
	}

	// id ⊕ register: у задачи с register адрес уже есть.
	if _, hasReg := present["register"]; hasReg {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "id_register_conflict",
			Message:  "id: and register: cannot both be set on one task — a task with register is already addressable; use id only on tasks without register",
			Hint:     "keep only register: (subscribe to alerts by its name), or drop register: and use id:",
			YAMLPath: pathPrefix + ".id",
		}))
	}

	// pilot: id только на module-задаче.
	if _, isModule := present["module"]; !isModule {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "id_unsupported_target",
			Message:  "id: on a block/include task is not supported yet — in the pilot id is allowed only on a module task (it has its own changed signal)",
			Hint:     "put id: on a specific module task; extending to block/include is a separate change",
			YAMLPath: pathPrefix + ".id",
		}))
	}

	return out
}

// validateLoopField — `loop: { items: <required>, as?, index_as?, when? }`
// (destiny/tasks.md §7).
//
// Слайс E1: `loop:` поддержан только на module-задаче (render-time fan-out,
// см. orchestration.md §2.2). На include:/apply:/block: — отвергается с
// кодом loop_unsupported_target (раскрытие loop для этих видов отложено).
//
// items: обязателен; тип/значение (CEL/template-expr) не проверяем — разбор
// в render-фазе. as/index_as — валидные snake_case-идентификаторы (если
// заданы) и не из зарезервированного контекста. when: — строка (CEL-предикат).
func validateLoopField(kv *ast.MappingValueNode, present map[string]*ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	// Слайс E1: loop легитимен только на module-задаче.
	if _, isModule := present["module"]; !isModule {
		tok := kv.Key.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "loop_unsupported_target",
			Message:  "loop: is supported only on a module task (loop on include/apply/block is not yet implemented)",
			Hint:     "wrap the iterated work in a module task, or split into separate tasks",
			YAMLPath: pathPrefix + ".loop",
		})}
	}

	mm, ok := kv.Value.(*ast.MappingNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "loop: must be a mapping with items: + optional as:/index_as:/when:",
			YAMLPath: pathPrefix + ".loop",
		})}
	}

	var out []diag.Diagnostic
	knownLoopKeys := map[string]bool{"items": true, "as": true, "index_as": true, "when": true}
	var hasItems bool
	var asNode, indexAsNode *ast.MappingValueNode
	for _, sub := range mm.Values {
		tok := sub.Key.GetToken()
		if tok == nil {
			continue
		}
		k := tok.Value
		if !knownLoopKeys[k] {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + k + `" in loop:`,
				Hint:     "loop: accepts only items, as, index_as, when",
				YAMLPath: pathPrefix + ".loop." + k,
			}))
			continue
		}
		switch k {
		case "items":
			hasItems = true
			// Тип не проверяем: items — CEL/template-expr (строка `${ … }`)
			// либо inline-литерал (seq/map), разбор в render-фазе.
		case "as":
			asNode = sub
			out = append(out, validateLoopVar(sub, k, pathPrefix)...)
		case "index_as":
			indexAsNode = sub
			out = append(out, validateLoopVar(sub, k, pathPrefix)...)
		case "when":
			if _, isStr := sub.Value.(*ast.StringNode); !isStr {
				if _, isNull := sub.Value.(*ast.NullNode); !isNull {
					vt := sub.Value.GetToken()
					out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "type_mismatch",
						Message:  "loop.when must be a string (CEL predicate)",
						YAMLPath: pathPrefix + ".loop.when",
					}))
				}
			}
		}
	}
	if !hasItems {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "loop.items is required",
			Hint:     "loop: { items: ${ input.<x> }, as: <name> }",
			YAMLPath: pathPrefix + ".loop.items",
		}))
	}
	// as: и index_as: должны различаться: в render-контексте они кладутся в
	// общий map loop-переменных, и одинаковое имя молча затёрло бы элемент
	// индексом (`as=x, index_as=x` → на хост уходит индекс вместо элемента).
	if asNode != nil && indexAsNode != nil {
		asStr, asOK := asNode.Value.(*ast.StringNode)
		ixStr, ixOK := indexAsNode.Value.(*ast.StringNode)
		if asOK && ixOK && asStr.Value == ixStr.Value {
			tok := ixStr.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "loop_var_conflict",
				Message:  fmt.Sprintf("loop.as and loop.index_as are both %q; they must differ", asStr.Value),
				Hint:     "use distinct names, e.g. as: item, index_as: i",
				YAMLPath: pathPrefix + ".loop.index_as",
			}))
		}
	}
	return out
}

// validateLoopVar — `loop.as:`/`loop.index_as:` — snake_case-идентификатор,
// не из зарезервированного CEL-контекста (он становится голой переменной).
func validateLoopVar(sub *ast.MappingValueNode, key, pathPrefix string) []diag.Diagnostic {
	sn, isStr := sub.Value.(*ast.StringNode)
	if !isStr {
		vt := sub.Value.GetToken()
		return []diag.Diagnostic{diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("loop.%s must be a string identifier", key),
			YAMLPath: pathPrefix + ".loop." + key,
		})}
	}
	if !reLoopVar.MatchString(sn.Value) {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "loop_var_invalid",
			Message:  fmt.Sprintf("loop.%s %q does not match %s", key, sn.Value, reLoopVar),
			Hint:     "snake_case identifier: starts with a-z; only [a-z0-9_]",
			YAMLPath: pathPrefix + ".loop." + key,
		})}
	}
	if loopReservedNames[sn.Value] {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "loop_var_reserved",
			Message:  fmt.Sprintf("loop.%s %q shadows a reserved CEL context name", key, sn.Value),
			Hint:     "reserved: input, register, incarnation, soulprint, essence, vars",
			YAMLPath: pathPrefix + ".loop." + key,
		})}
	}
	for _, prefix := range loopReservedPrefixes {
		if strings.HasPrefix(sn.Value, prefix) {
			tok := sn.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "loop_var_reserved",
				Message:  fmt.Sprintf("loop.%s %q uses reserved prefix %q (engine filter iter-variable)", key, sn.Value, prefix),
				Hint:     "the __host* prefix is reserved for the soulprint.hosts.where(...) filter; choose another name",
				YAMLPath: pathPrefix + ".loop." + key,
			})}
		}
	}
	return nil
}

// validateRequisiteListField — `onchanges:`/`onfail:` строго list-of-string
// (destiny/tasks.md §«onchanges»/§«onfail»). Скаляр/map/int → type_mismatch;
// non-string элемент списка → type_mismatch на элементе. null = «не задано».
// Имена register-ов проверяет cross-ref-фаза (validateTaskRefs) — здесь только
// форма.
func validateRequisiteListField(kv *ast.MappingValueNode, key, pathPrefix string) []diag.Diagnostic {
	if _, isNull := kv.Value.(*ast.NullNode); isNull {
		return nil
	}
	seq, ok := kv.Value.(*ast.SequenceNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("%s: must be a list of register names", key),
			Hint:     fmt.Sprintf("%s: [register_a, register_b]", key),
			YAMLPath: pathPrefix + "." + key,
		})}
	}
	return requisiteListElems(seq, key, pathPrefix)
}

// validateRequireField — `require:` допускает две формы: список register-имён
// ИЛИ скаляр "all" (orchestration/destiny tasks). Скаляр != "all" → ошибка;
// non-string элемент списка → type_mismatch. null = «не задано».
func validateRequireField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	switch v := kv.Value.(type) {
	case *ast.NullNode:
		return nil
	case *ast.SequenceNode:
		return requisiteListElems(v, "require", pathPrefix)
	case *ast.StringNode:
		if v.Value == "all" {
			return nil
		}
		tok := v.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("require: scalar %q is invalid — use a list of register names or the literal \"all\"", v.Value),
			Hint:     "require: [register_a] | require: all",
			YAMLPath: pathPrefix + ".require",
		})}
	default:
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "require: must be a list of register names or the literal \"all\"",
			Hint:     "require: [register_a] | require: all",
			YAMLPath: pathPrefix + ".require",
		})}
	}
}

// requisiteListElems — каждый элемент requisite-списка обязан быть строкой
// (register-имя либо CEL-обёртка). int/bool/seq/map в элементе → type_mismatch.
func requisiteListElems(seq *ast.SequenceNode, key, pathPrefix string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for j, item := range seq.Values {
		if _, isStr := item.(*ast.StringNode); isStr {
			continue
		}
		tok := item.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("%s[%d]: must be a string (register name)", key, j),
			YAMLPath: fmt.Sprintf("%s.%s[%d]", pathPrefix, key, j),
		}))
	}
	return out
}

// validateRetryField — `retry: { count: >=1, delay: duration, until: string }`.
func validateRetryField(kv *ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	mm, ok := kv.Value.(*ast.MappingNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "retry: must be a mapping",
			YAMLPath: pathPrefix + ".retry",
		})}
	}
	var out []diag.Diagnostic
	knownRetryKeys := map[string]bool{"count": true, "delay": true, "until": true}
	var hasCount bool
	for _, sub := range mm.Values {
		tok := sub.Key.GetToken()
		if tok == nil {
			continue
		}
		k := tok.Value
		if !knownRetryKeys[k] {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + k + `" in retry:`,
				Hint:     "retry: accepts only count, delay, until",
				YAMLPath: pathPrefix + ".retry." + k,
			}))
			continue
		}
		switch k {
		case "count":
			in, isInt := sub.Value.(*ast.IntegerNode)
			if !isInt {
				vt := sub.Value.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  "retry.count must be an integer",
					YAMLPath: pathPrefix + ".retry.count",
				}))
				continue
			}
			hasCount = true
			it := in.GetToken()
			if it.Value == "0" || (len(it.Value) > 0 && it.Value[0] == '-') {
				out = append(out, diagAt(it.Position.Line, it.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "value_out_of_range",
					Message:  fmt.Sprintf("retry.count must be >= 1, got %s", it.Value),
					YAMLPath: pathPrefix + ".retry.count",
				}))
			}
		case "delay":
			out = append(out, validateDurationField(sub, pathPrefix+".retry.delay")...)
		case "until":
			if _, isStr := sub.Value.(*ast.StringNode); !isStr {
				vt := sub.Value.GetToken()
				out = append(out, diagAt(vt.Position.Line, vt.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "type_mismatch",
					Message:  "retry.until must be a string (CEL predicate)",
					YAMLPath: pathPrefix + ".retry.until",
				}))
			}
		}
	}
	if !hasCount {
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "retry.count is required",
			Hint:     "retry: { count: <int>, delay: <duration>, until: <expr> }",
			YAMLPath: pathPrefix + ".retry.count",
		}))
	}
	return out
}

// validateDurationField валидирует строку по convention `duration` Soul Stack
// (config.ParseDuration): Go-time.ParseDuration (`30s`/`5m`/`1h30m`) плюс
// суффикс `<N>d` для дней (`30d`). Единая convention с keeper.yml-валидацией,
// Reaper и core.url — см. docs/keeper/config.md → «Конвенции типов».
// Применяется ко всем destiny duration-полям (task.timeout, retry.delay и т.п.).
func validateDurationField(kv *ast.MappingValueNode, yamlPath string) []diag.Diagnostic {
	sn, ok := kv.Value.(*ast.StringNode)
	if !ok {
		tok := kv.Value.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "duration must be a string (Go duration syntax or <N>d for days)",
			YAMLPath: yamlPath,
		})}
	}
	if _, err := ParseDuration(sn.Value); err != nil {
		tok := sn.GetToken()
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "duration_invalid",
			Message:  fmt.Sprintf("invalid duration %q: %v", sn.Value, err),
			Hint:     "examples: 30s, 5m, 1h30m, 30d",
			YAMLPath: yamlPath,
		})}
	}
	return nil
}

// isCELWrapped — true, если строка целиком — одна обёртка `${ … }`. На M1.2.c
// не разбираем CEL, но грубое распознавание нужно: `${ incarnation.name }` в
// `on:`-литералах не должен ловиться на kebab-case regex.
func isCELWrapped(s string) bool {
	if len(s) < 4 {
		return false
	}
	if s[0] != '$' || s[1] != '{' {
		return false
	}
	return s[len(s)-1] == '}'
}
