package config

import (
	"fmt"
	"regexp"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ScenarioManifest — типизированное представление `scenario/<name>/main.yml` по
// нормативной спеке [`docs/scenario/orchestration.md`].
//
// Манифест содержит имя/описание сценария, входной контракт `input:` (общий
// стандарт docs/input.md), декларацию `state_changes` (что сценарий пишет в
// `incarnation.state` после успешного apply, §2) и список задач `tasks[]`.
//
// DSL-ядро задач унаследовано от destiny (ADR-009): scenario поддерживает
// все 22 ключа из docs/destiny/tasks.md плюс scenario-дельту (on/where/serial/
// run_once). Полиморфный декод задачи (module / apply / include / block) —
// в scenario_task.go.
type ScenarioManifest struct {
	Name         string         `yaml:"name"`
	Description  string         `yaml:"description,omitempty"`
	Input        InputSchemaMap `yaml:"input,omitempty"`
	StateChanges *StateChanges  `yaml:"state_changes,omitempty"`
	Vars         map[string]any `yaml:"vars,omitempty"`
	Tasks        []Task         `yaml:"tasks"`
}

// StateChanges — декларация мутаций `incarnation.state`, которые сценарий
// зафиксирует после успешного cross-host барьера (orchestration.md §7).
//
// `Sets` — map `<поле> → <CEL-выражение>`: и какое поле `incarnation.state`
// обновить, и откуда взять значение (рендерится Keeper-side в scenario-CEL-
// окружении после барьера, orchestration.md §7.1). Значение — строка-выражение
// (`${ … }`-маркер ADR-010) либо литерал; cross-host свёртка — last-wins по SID.
//
// `Appends`/`Modifies` (per-host коллекции) — future-расширение полной
// грамматики: пока остаются списками field-path-ов (`list of string`), движком
// не применяются. Пустой блок (`state_changes: {}`) валиден — state не меняется.
type StateChanges struct {
	Sets     map[string]string `yaml:"sets,omitempty"`
	Appends  []string          `yaml:"appends,omitempty"`
	Modifies []string          `yaml:"modifies,omitempty"`
}

// UnmarshalYAML — кастомный декод StateChanges. Стандартный NodeToValue падает
// на `sets: "scalar"` с обобщённой ошибкой type-mismatch (decode-fault), которая
// дублируется с информативной диагностикой validateStateChanges. Тут декодируем
// по AST по полю, узел неподходящей формы — просто пропускаем
// (validateStateChanges поднимет осмысленный type_mismatch с yaml_path):
//
//   - `sets` — mapping `<поле>: <выражение>` → map[string]string;
//   - `appends`/`modifies` — sequence строк → []string (future, см. StateChanges).
func (s *StateChanges) UnmarshalYAML(node ast.Node) error {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		// state_changes: <scalar/seq> — диагностику поднимет общий walker;
		// здесь оставляем zero-value.
		return nil
	}
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "sets":
			s.Sets = setsFromNode(kv.Value)
		case "appends":
			s.Appends = stringSeqFromNode(kv.Value)
		case "modifies":
			s.Modifies = stringSeqFromNode(kv.Value)
		}
	}
	return nil
}

// setsFromNode декодирует mapping `<поле>: <выражение>` в map[string]string.
// Не-mapping узел (старая seq-форма, скаляр) → nil (validateStateChanges
// поднимет type_mismatch). Значения не-строкового типа пропускаются —
// валидатор поднимет диагностику по yaml_path.
func setsFromNode(node ast.Node) map[string]string {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(mm.Values))
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		if sn, ok := kv.Value.(*ast.StringNode); ok {
			out[tok.Value] = sn.Value
		}
	}
	return out
}

// stringSeqFromNode декодирует sequence строк в []string. Не-sequence → nil.
func stringSeqFromNode(node ast.Node) []string {
	seq, ok := node.(*ast.SequenceNode)
	if !ok {
		return nil
	}
	vals := make([]string, 0, len(seq.Values))
	for _, item := range seq.Values {
		if sn, ok := item.(*ast.StringNode); ok {
			vals = append(vals, sn.Value)
		}
	}
	return vals
}

// reScenarioName — имя сценария: snake_case или kebab-case (имена операций над
// кластером: `create`, `add_user`, `update_acl`, `add_replica`, `restart`).
// В отличие от имён destiny/service (строго kebab) сценарий — verb-имя
// операции; snake_case в спеке и примерах ([scenario/concept.md],
// [architecture.md → раскладка service-репо]) — канон. Дефис тоже допустим
// (например, `add-user`).
var reScenarioName = regexp.MustCompile(`^[a-z][a-z0-9]*([_-][a-z0-9]+)*$`)

// reCovenName — kebab-case coven-метка в `on: [coven, ...]`. Совпадает с
// именем сервиса/сценария по форме (одноуровневое kebab). Допускается также
// CEL-обёртка `${ ... }` (например, `${ incarnation.name }`) — для неё
// regex-валидация пропускается.
var reCovenName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// reSerialPercent — процентная форма `serial: "<N>%"` (§2.4).
// Целая часть — положительное число без leading zeros (1..99 включительно;
// 100% эквивалентно дефолту «вся ширина» и не имеет смысла как явная форма,
// но grammar-wise валидно).
var reSerialPercent = regexp.MustCompile(`^[1-9][0-9]*%$`)

// deprecatedScenarioKeys — устаревшие top-level ключи `main.yml` сценария.
// `wait:` и `filter:` явно изъяты orchestration.md §2/§4 — поднимаем
// `unknown_key` с hint-ом о замене. Симметрично `deprecatedDestinyKeys`.
var deprecatedScenarioKeys = map[string]string{
	"wait":   "wait: removed (orchestration.md §2); express the same with retry:+until: on a probe step",
	"filter": "filter: removed (orchestration.md §4); use where: with register.<probe>.* predicate or stable soulprint.self.* facts",
	// `version:` — git ref, не поле манифеста (ADR-007).
	"version": "version is a git ref under which the scenario is committed, not a manifest field; see ADR-007",
}

// deprecatedTaskKeys — устаревшие task-level ключи (внутри элемента `tasks[]`
// или внутри `block:`). Симметрично deprecatedScenarioKeys.
var deprecatedTaskKeys = map[string]string{
	"wait":   "wait: removed (orchestration.md §2); express with retry:+until: on a probe step",
	"filter": "filter: removed (orchestration.md §4); use where: predicate instead",
}

// stateChangesKnownKeys — закрытый набор ключей внутри `state_changes:`.
var stateChangesKnownKeys = map[string]bool{
	"sets":     true,
	"appends":  true,
	"modifies": true,
}

// schemaValidateScenario — пост-decode проверки ScenarioManifest.
func schemaValidateScenario(path string, root *ast.MappingNode, m *ScenarioManifest) []diag.Diagnostic {
	_ = path
	var out []diag.Diagnostic

	topKeys := topLevelKeys(root)

	// 1) Deprecated top-level keys → `unknown_key` с осмысленным hint-ом.
	// Подавление дубля из reflect-walker — через `scenarioManifestType` в walk.go.
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		hint, dep := deprecatedScenarioKeys[tok.Value]
		if !dep {
			continue
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "unknown_key",
			Message:  `unknown field "` + tok.Value + `"`,
			Hint:     hint,
			YAMLPath: "$." + tok.Value,
		}))
	}

	// 2) `name:` — required + format.
	if !topKeys["name"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "name is required at top-level",
			Hint:     "set name: <kebab-case>, matching scenario/<name>/ folder",
			YAMLPath: "$.name",
		})
	} else if !reScenarioName.MatchString(m.Name) {
		msg := fmt.Sprintf("name %q does not match %s", m.Name, reScenarioName)
		if m.Name == "" {
			msg = "name must be non-empty kebab-case string"
		}
		out = append(out, atPath(root, "$.name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: msg,
			Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter",
		}))
	}

	// 3) `tasks:` — required (ключ должен присутствовать). Пустой список
	// валиден (no-op сценарий — например, `restart` мог бы не иметь задач, хотя
	// на практике их хотя бы одна). Отсутствие ключа — ошибка.
	if !topKeys["tasks"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "tasks is required at top-level",
			Hint:     "declare tasks: [...] — list of scenario tasks; empty list is allowed for no-op scenarios",
			YAMLPath: "$.tasks",
		})
	}

	// 4) `state_changes:` — структурная валидация (только если ключ есть).
	if topKeys["state_changes"] {
		out = append(out, validateStateChanges(root, "$.state_changes")...)
	}

	// 5) `input:` — общий валидатор схемы.
	if topKeys["input"] {
		out = append(out, validateInputSchemaMap(m.Input, findInputMapping(root, "input"), "$.input")...)
	}

	// 6) `tasks[]` — полиморфная валидация каждой задачи.
	tasksNode := findSequenceValue(root, "tasks")
	if tasksNode != nil {
		for i, item := range tasksNode.Values {
			out = append(out, validateTaskNode(item, fmt.Sprintf("$.tasks[%d]", i))...)
		}
	}

	return out
}

// validateStateChanges — проверка структуры `state_changes:`-блока.
//
// Допустимы только три ключа. `sets` — mapping `<поле>: <выражение>` (ключи
// непустые, значения — непустые строки-выражения, orchestration.md §7.1).
// `appends`/`modifies` (future) — массивы строк (field-path-ов). Пустой block
// (`state_changes: {}`) валиден. Скаляр на месте блока даёт `type_mismatch` уже
// на decode-фазе; здесь дополнительно проверяем unknown keys и тип values.
func validateStateChanges(root *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	node := findInputMapping(root, "state_changes")
	if node == nil {
		// либо null, либо не mapping — decode уже поднял диагностику.
		return nil
	}
	var out []diag.Diagnostic
	for _, kv := range node.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		keyName := tok.Value
		if !stateChangesKnownKeys[keyName] {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + keyName + `"`,
				Hint:     "state_changes allows only sets / appends / modifies",
				YAMLPath: pathPrefix + "." + keyName,
			}))
			continue
		}
		if keyName == "sets" {
			out = append(out, validateSetsMap(tok.Position.Line, tok.Position.Column, kv.Value, pathPrefix)...)
			continue
		}
		out = append(out, validateStringSeq(tok.Position.Line, tok.Position.Column, kv.Value, keyName, pathPrefix)...)
	}
	return out
}

// validateSetsMap проверяет `sets` как mapping `<поле>: <выражение>`: значение
// блока — mapping, каждое значение — непустая строка-выражение (CEL/литерал).
// keyLine/keyCol — позиция ключа `sets` (fallback, если у value нет токена).
func validateSetsMap(keyLine, keyCol int, value ast.Node, pathPrefix string) []diag.Diagnostic {
	mm, ok := value.(*ast.MappingNode)
	if !ok {
		vt := value.GetToken()
		line, col := keyLine, keyCol
		if vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "state_changes.sets must be a mapping of field → CEL-expression",
			Hint:     "sets: { <field>: \"${ ... }\" } — orchestration.md §7.1",
			YAMLPath: pathPrefix + ".sets",
		})}
	}
	var out []diag.Diagnostic
	for _, kv := range mm.Values {
		ftok := kv.Key.GetToken()
		if ftok == nil {
			continue
		}
		sn, isStr := kv.Value.(*ast.StringNode)
		if !isStr {
			vt := kv.Value.GetToken()
			line, col := ftok.Position.Line, ftok.Position.Column
			if vt != nil {
				line, col = vt.Position.Line, vt.Position.Column
			}
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  fmt.Sprintf("state_changes.sets.%s must be a string expression", ftok.Value),
				YAMLPath: fmt.Sprintf("%s.sets.%s", pathPrefix, ftok.Value),
			}))
			continue
		}
		if sn.Value == "" {
			out = append(out, diagAt(ftok.Position.Line, ftok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "empty_value",
				Message:  fmt.Sprintf("state_changes.sets.%s must be a non-empty expression", ftok.Value),
				YAMLPath: fmt.Sprintf("%s.sets.%s", pathPrefix, ftok.Value),
			}))
		}
	}
	return out
}

// validateStringSeq проверяет `appends`/`modifies` как sequence строк (future).
// keyLine/keyCol — позиция ключа (fallback, если у элемента нет токена).
func validateStringSeq(keyLine, keyCol int, value ast.Node, keyName, pathPrefix string) []diag.Diagnostic {
	seq, ok := value.(*ast.SequenceNode)
	if !ok {
		vt := value.GetToken()
		line, col := keyLine, keyCol
		if vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("state_changes.%s must be a sequence of strings", keyName),
			YAMLPath: pathPrefix + "." + keyName,
		})}
	}
	var out []diag.Diagnostic
	for i, item := range seq.Values {
		if _, isStr := item.(*ast.StringNode); !isStr {
			vt := item.GetToken()
			line, col := keyLine, keyCol
			if vt != nil {
				line, col = vt.Position.Line, vt.Position.Column
			}
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  fmt.Sprintf("state_changes.%s[%d] must be a string", keyName, i),
				YAMLPath: fmt.Sprintf("%s.%s[%d]", pathPrefix, keyName, i),
			}))
		}
	}
	return out
}

// findSequenceValue — value-узел под ключом `name`, если value — SequenceNode.
// Симметрично findInputMapping для sequence-кейса.
func findSequenceValue(m *ast.MappingNode, name string) *ast.SequenceNode {
	if m == nil {
		return nil
	}
	for _, kv := range m.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value != name {
			continue
		}
		if s, ok := kv.Value.(*ast.SequenceNode); ok {
			return s
		}
		return nil
	}
	return nil
}

// semanticValidateScenario — cross-field/cross-task инварианты ScenarioManifest.
//
// Покрыто: duplicate_task_address (register ∪ id) + unknown_register_reference
// по списку `tasks[]` (включая вложенные block:), см. validateTaskRefs.
// CEL-syntax и cross-ref внутри CEL-предикатов (`when:`/`changed_when:`/
// `until:`) — отложены (M1.3/M1.5).
func semanticValidateScenario(_ *ScenarioManifest, root *ast.MappingNode) []diag.Diagnostic {
	return validateTaskRefs(findSequenceValue(root, "tasks"), "$.tasks")
}
