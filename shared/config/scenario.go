package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
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
	Validate     []ValidateRule `yaml:"validate,omitempty"`
	StateChanges *StateChanges  `yaml:"state_changes,omitempty"`
	Compute      ComputeBlock   `yaml:"compute,omitempty"`
	Vars         map[string]any `yaml:"vars,omitempty"`
	Tasks        []Task         `yaml:"tasks"`

	// Form — опциональный презентационный слой `input:`-формы (form_layout.go):
	// как UI группирует/подписывает поля input в секции. nil = ключа нет (UI
	// рисует input плоско, forward-compat). Не влияет на контракт ввода/валидацию.
	Form *FormLayout `yaml:"form,omitempty"`
}

// ValidateRule — одно правило top-level scenario-секции `validate:` (ADR-009
// amendment 2026-06-23, DSL wave 2). Декларативная input-валидация «должно быть
// X, не Y» вместо россыпи assert-задач: список правил `[{that, message}]`, каждое
// `that` — CEL-bool-предикат (вся строка = CEL без обёртки, как `where:`/`assert.
// that`), `message` — человекочитаемая причина отказа при `that == false`.
//
// КОНТЕКСТ ПРАВИЛА — INPUT-ONLY: env несёт ЕДИНСТВЕННУЮ переменную `input`
// (тот же узкий cel-go-sandbox, что `required_when` — input_required_when.go).
// validate: покрывает ИНВАРИАНТЫ ВВОДА (кросс-полевые предусловия, не выразимые
// одиночным schema-ключом — например, «`port` обязателен, если `tls` выключен»).
// Ссылка на essence/soulprint/register/vault в `that` → compile-ошибка undeclared
// reference (структурный барьер необъявленностью, не текстовый guard). Топология/
// roster-проверки остаются за `assert:` (у него полный scenario-CEL-контекст с
// soulprint.hosts) — validate: ДОПОЛНЯЕТ, не заменяет ни assert, ни required_when.
//
// КОГДА: pre-flight на CreateTyped/RunTyped (request-путь) — первый провалившийся
// rule даёт HTTP 422 validation_failed ДО коммита incarnation и ДО applying (как
// required_when и pre-flight assert, БЕЗ error_locked). Вычисление детерминировано
// от input (config.EvalValidateRules) — двухточечная render-fail-safe не нужна
// (input не меняется между request-путём и стартом goroutine, в отличие от roster
// у assert).
type ValidateRule struct {
	That    string `yaml:"that"`
	Message string `yaml:"message"`
}

// ComputeBlock — scenario-level вычисляемые переменные (`compute:`, ADR-009
// amendment 2026-06-23). Каждая запись — `<имя>: <CEL-выражение>`: Keeper резолвит
// её ОДИН раз на прогон в РУН-УРОВНЕВОМ scenario-контексте (input/essence/
// incarnation/register), затем результат доступен как `compute.<имя>` в
// `apply.input` И в `state_changes` (cel_render.resolveCompute).
//
// Назначение — снять дублирование общего выражения, которое иначе пишут дважды
// (apply.input и state_changes не видят task-level `vars:`): объявить большой
// merge() один раз и сослаться `${ compute.<имя> }`.
//
// Барьер изоляции (architect aebb2d39 §5):
//   - compute НЕ протекает в изолированный destiny-проход (destiny видит только
//     результат через apply.input — RenderInput.Compute там не пробрасывается,
//     ADR-009 V2);
//   - резолв-контекст compute РУН-УРОВНЕВЫЙ (БЕЗ soulprint.self/soulprint.hosts):
//     compute host-инвариантна по построению, поэтому одно и то же значение
//     корректно уходит и в apply.input (резолв на targeted[0]), и в state_changes
//     (per-run, не per-host). Ссылка на soulprint.* в compute → CEL no-such-key
//     (структурный барьер, не текстовый guard).
//
// Порядок объявления значим: compute[i] может ссылаться на ранее объявленный
// compute[j] (j < i) как `${ compute.<имя_j> }`. Поэтому хранится упорядоченным
// списком (не map: декод сохраняет порядок ключей YAML).
type ComputeBlock []ComputeVar

// ComputeVar — одна запись `compute:`-блока (имя + CEL-выражение). Value —
// строка-CEL (`${ … }`-интерполяция ИЛИ нативное выражение), резолвится
// cel_render.resolveCompute. Литерал-значение (число/bool/коллекция) тоже
// допустимо — non-string проходит насквозь как в `vars:`.
type ComputeVar struct {
	Name  string
	Value any
}

// StateChanges — декларация мутаций `incarnation.state`, которые сценарий
// зафиксирует после успешного cross-host барьера (orchestration.md §7).
//
// Две формы декода (UnmarshalYAML различает по виду YAML-узла):
//
//   - НОВАЯ list-форма (sequence): `state_changes:` — упорядоченный список
//     операций-глаголов (`- set:` / `- add:` / `- modify:` / `- remove:` /
//     `- foreach:`), применяемых по порядку объявления к промежуточному state.
//     Декодируется в `Ops` (см. StateChange); `IsList` = true. Целевая грамматика
//     ADR-057 (все глаголы реализованы).
//   - СТАРАЯ map-форма (mapping, DEPRECATED): `state_changes: { sets: {...},
//     appends: [...], modifies: [...] }`. Сохраняется ради backward-compat
//     существующих сценариев — декодируется в `Sets`/`Appends`/`Modifies`,
//     `IsList` = false. Семантика прежняя (orchestration.md §7.1): `Sets` — map
//     `<поле> → <CEL-выражение>`; cross-host свёртка — last-wins по SID.
//     `Appends`/`Modifies` движком не применяются (исторический плейсхолдер).
//
// Пустой блок валиден в обеих формах (state не меняется): `state_changes: {}`
// (старая) или `state_changes: []` (новая).
type StateChanges struct {
	// IsList — дискриминатор формы (true для новой list-формы). Render/merge
	// ветвятся по нему: list → ordered Ops; map → legacy Sets-overwrite.
	IsList bool `yaml:"-"`

	// Ops — упорядоченный список операций новой list-формы. nil/пусто в map-форме.
	Ops []StateChange `yaml:"-"`

	// Sets/Appends/Modifies — старая map-форма (DEPRECATED). nil в list-форме.
	Sets     map[string]string `yaml:"sets,omitempty"`
	Appends  []string          `yaml:"appends,omitempty"`
	Modifies []string          `yaml:"modifies,omitempty"`
}

// UnmarshalYAML декодирует `compute:` как mapping `<имя>: <выражение>` в
// УПОРЯДОЧЕННЫЙ список ComputeVar (порядок ключей YAML сохраняется — compute[i]
// может ссылаться на ранее объявленный compute[j], j<i). Значение — строка-CEL
// либо литерал (non-string проходит насквозь как в `vars:`). Не-mapping узел
// (scalar/sequence) → пустой блок: validateComputeBlock поднимет type_mismatch по
// yaml_path. Ключ без значения / пустой ключ пропускается (валидатор поднимет
// диагностику).
func (c *ComputeBlock) UnmarshalYAML(node ast.Node) error {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return nil
	}
	out := make(ComputeBlock, 0, len(mm.Values))
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value == "" {
			continue
		}
		out = append(out, ComputeVar{Name: tok.Value, Value: nodeToAny(kv.Value)})
	}
	*c = out
	return nil
}

// StateVerb — глагол одной операции state_changes (новая list-форма).
type StateVerb string

const (
	// VerbSet — перезапись поля целиком (семантика прежнего `sets`).
	VerbSet StateVerb = "set"
	// VerbAdd — добавить элемент в коллекцию (map/list) идемпотентно.
	VerbAdd StateVerb = "add"
	// VerbModify — патч ВСЕХ элементов коллекции, подходящих под Match (all-by-
	// default; orchestration.md §7.1).
	VerbModify StateVerb = "modify"
	// VerbRemove — удалить ВСЕ элементы коллекции, подходящие под Match.
	VerbRemove StateVerb = "remove"
	// VerbForeach — bulk fan-out N операций из CEL-списка/map (render-time, форма
	// из migration-DSL ADR-019). Раскрывается в N RenderedOp до merge.
	VerbForeach StateVerb = "foreach"
)

// Expect — опц. runtime-ассерт кратности match в modify/remove (ADR-057 §c).
// DEFAULT (пустое значение) = ExpectAny (любое число зацепленных, в т.ч. ноль).
type Expect string

const (
	// ExpectAny — любое число зацепленных элементов (DEFAULT). Пустая строка в
	// op.Expect трактуется как ExpectAny.
	ExpectAny Expect = "any"
	// ExpectOne — ровно один зацепленный элемент (иначе error_locked до коммита).
	ExpectOne Expect = "one"
	// ExpectAtMostOne — ноль или один зацепленный элемент.
	ExpectAtMostOne Expect = "at_most_one"
)

// OnConflict — политика идемпотентности `add` при совпадении идентичности.
type OnConflict string

const (
	// OnConflictSkip — элемент с такой идентичностью уже есть → no-op (DEFAULT).
	OnConflictSkip OnConflict = "skip"
	// OnConflictReplace — перезаписать существующий элемент новым значением.
	OnConflictReplace OnConflict = "replace"
	// OnConflictError — провалить прогон (error_locked, state не коммитнут).
	OnConflictError OnConflict = "error"
)

// StateChange — одна операция упорядоченного списка `state_changes` (новая
// list-форма). Глагол определяет, какие поля значимы:
//
//   - set:     Field + Value (перезапись поля целиком);
//   - add:     Field + Value (+ Key для map-коллекции / Match|Key для list-дедупа,
//   - OnConflict skip|replace|error, default skip);
//   - modify:  Field + Match + Patch (+ опц. Expect) — патч всех подходящих;
//   - remove:  Field + Match (+ опц. Expect) — удалить всех подходящих;
//   - foreach: In (CEL-список/map) + As (имя биндинга) + Do (вложенные глаголы) —
//     render-time fan-out N операций (форма из migration-DSL ADR-019). Foreach
//     несёт целевое поле в Field=="" (глагол `foreach:` указывает не коллекцию, а
//     CEL-выражение коллекции для итерации, оно лежит в In).
//
// Value/Patch — произвольное YAML-значение: строка-CEL (`${ … }`), литерал,
// либо вложенный объект/список с CEL-строками в ячейках (рендерится рекурсивно
// Keeper-side). Key/Match — строки-CEL (идентичность/предикат элемента).
type StateChange struct {
	Verb  StateVerb
	Field string

	Value      any
	Key        string
	Match      string
	OnConflict OnConflict

	// Patch — map путь-в-элементе → CEL/литерал (только modify). Merge-time:
	// каждое значение вычисляется поверх per-host scenario-контекста + биндингов
	// текущего элемента (elem/key/value). Точечный путь (`config.maxmemory`) —
	// вложенный merge, не перезапись записи целиком (ADR-057 §a).
	Patch any
	// Expect — опц. ассерт кратности match (modify/remove). "" → ExpectAny.
	Expect Expect

	// foreach: In — CEL-выражение коллекции для итерации (`${ … }`); As — имя
	// биндинга текущего элемента; Do — вложенные операции, применяемые на каждой
	// итерации с активным биндингом As.
	In string
	As string
	Do []StateChange
}

// stateOpVerbs — известные глаголы операции (для дискриминатора в декоде/валидации).
// `expect` НЕ глагол — это параметр modify/remove (ADR-057 §c).
var stateOpVerbs = map[string]StateVerb{
	"set":     VerbSet,
	"add":     VerbAdd,
	"modify":  VerbModify,
	"remove":  VerbRemove,
	"foreach": VerbForeach,
}

// UnmarshalYAML — DUAL-PARSE StateChanges по виду YAML-узла:
//
//   - SequenceNode → новая list-форма: декодируем каждый элемент-mapping в
//     StateChange (по глаголу-ключу), ставим IsList=true. Структурную валидацию
//     (обязательные/неприменимые ключи по каждому глаголу) поднимает
//     validateStateChanges по AST — здесь только декод значений.
//   - MappingNode → старая map-форма (DEPRECATED): прежний путь декода
//     sets/appends/modifies (переиспользуем setsFromNode/stringSeqFromNode).
//   - прочее (scalar/null) → zero-value (диагностику поднимет walker/валидатор).
//
// Узел неподходящей формы внутри элемента пропускается без паники —
// validateStateChanges поднимет осмысленную диагностику по yaml_path.
func (s *StateChanges) UnmarshalYAML(node ast.Node) error {
	switch n := node.(type) {
	case *ast.SequenceNode:
		s.IsList = true
		s.Ops = make([]StateChange, 0, len(n.Values))
		for _, item := range n.Values {
			if op, ok := stateOpFromNode(item); ok {
				s.Ops = append(s.Ops, op)
			}
		}
		return nil
	case *ast.MappingNode:
		for _, kv := range n.Values {
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
	default:
		// state_changes: <scalar/null> — zero-value (валидатор поднимет type_mismatch).
		return nil
	}
}

// stateOpFromNode декодирует один элемент list-формы (mapping с глаголом-ключом)
// в StateChange. Глагол-ключ (`set`/`add`/…) несёт целевой Field; прочие ключи
// (`value`/`key`/`match`/`on_conflict`/`patch`/`in`/`as`/`do`) — параметры op.
// Не-mapping элемент / отсутствие глагола → (zero, false): валидатор поднимет
// диагностику по yaml_path.
func stateOpFromNode(node ast.Node) (StateChange, bool) {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return StateChange{}, false
	}
	var op StateChange
	var hasVerb bool
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		key := tok.Value
		if verb, isVerb := stateOpVerbs[key]; isVerb {
			op.Verb = verb
			// `foreach:` несёт CEL-выражение коллекции (→ In), прочие глаголы —
			// имя целевого поля (→ Field). orchestration.md §7.1.
			if verb == VerbForeach {
				op.In = stringFromNode(kv.Value)
			} else {
				op.Field = stringFromNode(kv.Value)
			}
			hasVerb = true
			continue
		}
		switch key {
		case "value":
			op.Value = nodeToAny(kv.Value)
		case "key":
			op.Key = stringFromNode(kv.Value)
		case "match":
			op.Match = stringFromNode(kv.Value)
		case "on_conflict":
			op.OnConflict = OnConflict(stringFromNode(kv.Value))
		case "patch":
			op.Patch = nodeToAny(kv.Value)
		case "expect":
			op.Expect = Expect(stringFromNode(kv.Value))
		case "as":
			op.As = stringFromNode(kv.Value)
		case "do":
			if seq, isSeq := kv.Value.(*ast.SequenceNode); isSeq {
				for _, sub := range seq.Values {
					if subOp, okSub := stateOpFromNode(sub); okSub {
						op.Do = append(op.Do, subOp)
					}
				}
			}
		}
	}
	return op, hasVerb
}

// stringFromNode извлекает строковое значение узла (для глагол-Field, key, match,
// on_conflict). Не-строка → "" (валидатор поднимет type_mismatch).
func stringFromNode(node ast.Node) string {
	if sn, ok := node.(*ast.StringNode); ok {
		return sn.Value
	}
	return ""
}

// nodeToAny декодирует произвольный YAML-узел (value/patch) в Go-значение через
// goccy NodeToValue: строка-CEL, литерал, либо вложенный объект/список (CEL-
// строки в ячейках рендерятся рекурсивно Keeper-side). Сбой декода → nil
// (валидатор поднимет диагностику по yaml_path).
func nodeToAny(node ast.Node) any {
	var v any
	if err := yaml.NodeToValue(node, &v); err != nil {
		return nil
	}
	return v
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

// stateChangesKnownKeys — закрытый набор ключей старой map-формы `state_changes:`.
var stateChangesKnownKeys = map[string]bool{
	"sets":     true,
	"appends":  true,
	"modifies": true,
}

// stateOpKnownKeys — закрытый набор ключей одной операции list-формы (глагол +
// параметры). Глаголы — из stateOpVerbs; остальные — общие параметры op. Ключ
// вне набора → unknown_key. `expect` — параметр (modify/remove), не глагол; `as`/
// `do` — параметры foreach; `in` ключом не является (foreach: несёт выражение).
var stateOpKnownKeys = map[string]bool{
	"set": true, "add": true, "modify": true, "remove": true,
	"foreach": true,
	"value":   true, "key": true, "match": true, "on_conflict": true,
	"patch": true, "expect": true, "as": true, "do": true,
}

// stateOpConflictValues — допустимые значения `on_conflict`.
var stateOpConflictValues = map[string]bool{
	"skip": true, "replace": true, "error": true,
}

// stateOpExpectValues — допустимые значения `expect` (modify/remove).
var stateOpExpectValues = map[string]bool{
	"one": true, "at_most_one": true, "any": true,
}

// foreachReservedBindings — имена, которые `foreach.as:` перекрывать нельзя:
// голый as-биндинг объявляется в merge-time CEL-контексте (render.renderForeach)
// и затёр бы фиксированный scenario-контекст ИЛИ локальные биндинги элемента
// коллекции. Сверх loopReservedNames (input/register/incarnation/soulprint/
// essence/vars) добавлены elem/key/value — локальные биндинги текущего элемента
// в add-match/modify-patch (ADR-057 §b): `as: elem` затенило бы elem-биндинг
// вложенной add-операции (reserved_binding_name).
var foreachReservedBindings = map[string]bool{
	"input": true, "register": true, "incarnation": true,
	"soulprint": true, "essence": true, "vars": true,
	"elem": true, "key": true, "value": true,
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

	// 4a) `compute:` — структурная валидация (только если ключ есть).
	if topKeys["compute"] {
		out = append(out, validateComputeBlock(root, "$.compute")...)
	}

	// 5) `input:` — общий валидатор схемы.
	if topKeys["input"] {
		out = append(out, validateInputSchemaMap(m.Input, findInputMapping(root, "input"), "$.input")...)
	}

	// 5a) `validate:` — top-level список input-инвариантов (только если ключ есть).
	if topKeys["validate"] {
		out = append(out, validateValidateBlock(root, "$.validate")...)
	}

	// 5b) `form:` — презентационный слой формы + cross-инварианты против input:
	// (form_field_unknown/duplicate/uncovered, section.key уникальность). Активен
	// только при наличии ключа.
	if topKeys["form"] {
		out = append(out, validateFormLayout(root, m, "$.form")...)
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

// reComputeName — имя compute-переменной: должно быть CEL-полем-доступным
// (`compute.<name>`), то есть идентификатор snake/camel, начинающийся с буквы или
// `_`. Цифры/буквы/подчёркивание внутри. Дефис/точка/пробел запрещены (сломали бы
// `compute.<name>`-доступ в CEL).
var reComputeName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// computeReservedNames — имена, которые compute-переменная перекрывать нельзя:
// корневые контекст-имена CEL (input/register/incarnation/soulprint/essence/vars)
// + сам корень `compute`. Compute-переменные кладутся под `compute.<name>`, но имя
// `compute` как переменная затёрло бы весь блок — запрещаем для самодокументируемости.
var computeReservedNames = map[string]bool{
	"input": true, "register": true, "incarnation": true,
	"soulprint": true, "essence": true, "vars": true, "compute": true,
}

// validateComputeBlock — проверка структуры `compute:`-блока (ADR-009 amendment
// 2026-06-23): mapping `<имя>: <CEL-выражение|литерал>`. Имя — CEL-field-доступный
// идентификатор (reComputeName), не из computeReservedNames; дубликат имени —
// ошибка (затёр бы ранний compute). Значение-строка — непустое; non-string литерал
// валиден (проходит насквозь как vars). Не-mapping блок → type_mismatch.
func validateComputeBlock(root *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	node := findValueNode(root, "compute")
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		line, col := 0, 0
		if vt := node.GetToken(); vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "compute must be a mapping of <name> → CEL-expression",
			Hint:     "compute: { <name>: \"${ ... }\" } — scenario-level computed vars (ADR-009)",
			YAMLPath: pathPrefix,
		})}
	}

	var out []diag.Diagnostic
	seen := make(map[string]bool, len(mm.Values))
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		name := tok.Value
		switch {
		case computeReservedNames[name]:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "reserved_binding_name",
				Message:  fmt.Sprintf("compute.%s shadows a reserved CEL context name", name),
				Hint:     "reserved: input, register, incarnation, soulprint, essence, vars, compute",
				YAMLPath: pathPrefix + "." + name,
			}))
		case !reComputeName.MatchString(name):
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "name_invalid_format",
				Message:  fmt.Sprintf("compute name %q is not a valid CEL identifier", name),
				Hint:     "use letters/digits/underscore, start with a letter or _ (accessed as compute.<name>)",
				YAMLPath: pathPrefix + "." + name,
			}))
		case seen[name]:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "duplicate_key",
				Message:  fmt.Sprintf("compute.%s is declared more than once", name),
				YAMLPath: pathPrefix + "." + name,
			}))
		}
		seen[name] = true

		// Значение: строка-CEL должна быть непустой; non-string литерал валиден.
		if sn, isStr := kv.Value.(*ast.StringNode); isStr && sn.Value == "" {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "empty_value",
				Message:  fmt.Sprintf("compute.%s must be a non-empty expression", name),
				YAMLPath: pathPrefix + "." + name,
			}))
		}
	}
	return out
}

// validateStateChanges — проверка структуры `state_changes:`-блока. DUAL-FORM:
//
//   - sequence на месте блока → новая list-форма: каждый элемент — операция-
//     глагол (validateStateOp); все глаголы (set/add/modify/remove/foreach)
//     валидируются по полной грамматике ADR-057.
//   - mapping → старая map-форма (DEPRECATED): прежний путь (sets/appends/
//     modifies). Пустой `state_changes: {}` валиден.
//   - прочее (scalar/null) — decode уже поднял type_mismatch; здесь молча.
func validateStateChanges(root *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	node := findValueNode(root, "state_changes")
	switch n := node.(type) {
	case *ast.SequenceNode:
		var out []diag.Diagnostic
		for i, item := range n.Values {
			out = append(out, validateStateOp(item, fmt.Sprintf("%s[%d]", pathPrefix, i))...)
		}
		return out
	case *ast.MappingNode:
		return validateStateChangesMap(n, pathPrefix)
	default:
		return nil
	}
}

// validateStateChangesMap — старая map-форма (sets/appends/modifies, DEPRECATED).
//
// Предохранитель (b) ADR-057 transit: валидная map-форма НЕ ошибка (dual-parse
// один релиз), но обязана дать DEPRECATION-WARN — иначе сценарий молча едет на
// форме, которую следующий релиз удалит. Для appends/modifies — отдельный, более
// строгий warn: они были no-op-плейсхолдерами (state НЕ растёт), их надо
// переписать на add/modify, иначе латентный баг (ADR-057 §контекст).
func validateStateChangesMap(node *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	var out []diag.Diagnostic

	// Один deprecation-warn на весь блок (на сам ключ state_changes — позиция
	// первого known-ключа), чтобы не дублировать на каждом sets/appends/modifies.
	if pos := firstKnownStateChangeKeyPos(node); pos != nil {
		out = append(out, diagAt(pos.Line, pos.Column, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "deprecated_form",
			Message:  "state_changes map-form (sets/appends/modifies) is deprecated and will be removed next release",
			Hint:     "rewrite as the ordered list-of-verbs form (- set: / - add: / - modify: / - remove:) — ADR-057",
			YAMLPath: pathPrefix,
		}))
	}

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
				Hint:     "state_changes (map-form, deprecated) allows only sets / appends / modifies; prefer the ordered list-form (- set: / - add:)",
				YAMLPath: pathPrefix + "." + keyName,
			}))
			continue
		}
		if keyName == "sets" {
			out = append(out, validateSetsMap(tok.Position.Line, tok.Position.Column, kv.Value, pathPrefix)...)
			continue
		}
		// appends/modifies — no-op-плейсхолдеры: state НЕ растёт. Отдельный warn,
		// чтобы автор не думал, что декларация работает (ADR-057 transit).
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "noop_placeholder",
			Message:  fmt.Sprintf("state_changes.%s is a no-op placeholder — it never applied, incarnation.state does not grow", keyName),
			Hint:     "rewrite on the list-form: appends → - add: / modifies → - modify: (otherwise state will not change) — ADR-057",
			YAMLPath: pathPrefix + "." + keyName,
		}))
		out = append(out, validateStringSeq(tok.Position.Line, tok.Position.Column, kv.Value, keyName, pathPrefix)...)
	}
	return out
}

// firstKnownStateChangeKeyPos возвращает позицию первого известного ключа
// map-формы (sets/appends/modifies) для якоря deprecation-warn-а на блоке.
// nil — пустой `state_changes: {}` (deprecated-warn не нужен: пустой блок никого
// не вводит в заблуждение, валиден в обеих формах).
func firstKnownStateChangeKeyPos(node *ast.MappingNode) *struct{ Line, Column int } {
	for _, kv := range node.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		if stateChangesKnownKeys[tok.Value] {
			return &struct{ Line, Column int }{tok.Position.Line, tok.Position.Column}
		}
	}
	return nil
}

// validateStateOp — валидация одной операции list-формы (элемент `state_changes[i]`).
//
// Элемент обязан быть mapping с РОВНО одним глаголом-ключом (`set`/`add`/…), чьё
// значение — непустое имя целевого поля. Параметры (`value`/`key`/`match`/
// `on_conflict`/`patch`/`expect`/`as`/`do`) проверяются по применимости к глаголу:
//
//   - set:    нужен value; match/key/on_conflict/patch/expect — неприменимы;
//   - add:    нужен value; on_conflict ∈ {skip,replace,error}; key (map) / match
//     (list-дедуп) опц.; patch/expect — неприменимы;
//   - modify: нужен match + patch; опц. expect; value/key/on_conflict/as/do неприм.;
//   - remove: нужен match; опц. expect; value/key/on_conflict/patch/as/do неприм.;
//   - foreach: нужен as + do (непустой); вложенный foreach в do отвергается.
func validateStateOp(node ast.Node, path string) []diag.Diagnostic {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		vt := node.GetToken()
		line, col := 0, 0
		if vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "state_changes operation must be a mapping with a verb key (- set: / - add: / …)",
			YAMLPath: path,
		})}
	}

	var out []diag.Diagnostic
	var verbTok = struct {
		name string
		line int
		col  int
		set  bool
	}{}
	seen := make(map[string]*ast.MappingValueNode, len(mm.Values))

	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		key := tok.Value
		seen[key] = kv
		if !stateOpKnownKeys[key] {
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + key + `"`,
				Hint:     "operation keys: <verb> (set/add/modify/remove/foreach) + value/key/match/on_conflict/patch/expect/as/do",
				YAMLPath: path + "." + key,
			}))
			continue
		}
		if _, isVerb := stateOpVerbs[key]; isVerb {
			if verbTok.set {
				out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "invalid_value",
					Message:  fmt.Sprintf("state_changes operation has multiple verbs (%q and %q) — exactly one expected", verbTok.name, key),
					YAMLPath: path,
				}))
				continue
			}
			verbTok.name, verbTok.line, verbTok.col, verbTok.set = key, tok.Position.Line, tok.Position.Column, true
			// `foreach:` несёт CEL-выражение коллекции; прочие глаголы — имя
			// целевого поля. В обоих случаях значение обязано быть непустой
			// строкой (foreach без выражения / verb без поля — ошибка).
			if stringFromNode(kv.Value) == "" {
				msg := fmt.Sprintf("%s: target field must be a non-empty string", key)
				if key == "foreach" {
					msg = "foreach: requires a non-empty CEL collection expression (foreach: \"${ ... }\")"
				}
				out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "empty_value",
					Message:  msg,
					YAMLPath: path + "." + key,
				}))
			}
		}
	}

	if !verbTok.set {
		return append(out, diagAt(lineOf(mm), colOf(mm), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "state_changes operation has no verb (expected one of set/add/modify/remove/foreach)",
			YAMLPath: path,
		}))
	}

	switch verbTok.name {
	case "set":
		out = append(out, validateSetOp(seen, path, verbTok.line, verbTok.col)...)
	case "add":
		out = append(out, validateAddOp(seen, path, verbTok.line, verbTok.col)...)
	case "modify":
		out = append(out, validateModifyOp(seen, path, verbTok.line, verbTok.col)...)
	case "remove":
		out = append(out, validateRemoveOp(seen, path, verbTok.line, verbTok.col)...)
	case "foreach":
		out = append(out, validateForeachOp(seen, path, verbTok.line, verbTok.col)...)
	}
	return out
}

// validateModifyOp — `modify` требует match + patch (map путь→CEL); опц. expect;
// value/key/on_conflict/in/as/do неприменимы. patch обязан быть mapping.
// Предохранитель широкого match: константно-истинный (`match: true`) или
// отсутствующий match — WARN «широкий предикат патчит всю коллекцию» (§7.1 (d)).
func validateModifyOp(seen map[string]*ast.MappingValueNode, path string, vline, vcol int) []diag.Diagnostic {
	var out []diag.Diagnostic
	out = append(out, warnWideMatch(seen, path, vline, vcol, "modify")...)
	if seen["patch"] == nil {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "modify: requires patch: { <path-in-element>: \"${ ... }\" } (orchestration.md §7.1)",
			YAMLPath: path + ".patch",
		}))
	} else {
		out = append(out, validatePatchMap(seen["patch"], path)...)
	}
	out = append(out, validateExpectValue(seen, path)...)
	out = append(out, rejectKeys(seen, path, []string{"value", "key", "on_conflict", "as", "do"}, "modify:")...)
	return out
}

// validateRemoveOp — `remove` требует match; опц. expect; value/key/on_conflict/
// patch/in/as/do неприменимы. Тот же предохранитель широкого match.
func validateRemoveOp(seen map[string]*ast.MappingValueNode, path string, vline, vcol int) []diag.Diagnostic {
	var out []diag.Diagnostic
	out = append(out, warnWideMatch(seen, path, vline, vcol, "remove")...)
	out = append(out, validateExpectValue(seen, path)...)
	out = append(out, rejectKeys(seen, path, []string{"value", "key", "on_conflict", "patch", "as", "do"}, "remove:")...)
	return out
}

// validateForeachOp — `foreach` требует as: (имя биндинга) + do: (непустой список
// вложенных операций); value/key/match/on_conflict/patch/expect неприменимы.
// Каждая вложенная do-операция валидируется рекурсивно (validateStateOp).
//
// `as:` не должен затенять зарезервированное имя CEL-контекста или локальный
// биндинг элемента (foreachReservedBindings) → reserved_binding_name.
//
// Вложенный foreach в do — вне грамматики (ADR-057: do несёт CRUD-глаголы, не
// повторный цикл). validateStateOp его НЕ отвергает (foreach — валидный верхний
// глагол), поэтому каждый do-элемент явно проверяется здесь: do-foreach прошёл бы
// lint, а render.renderForeach раскрыл бы его через renderOneStateOp с Verb=foreach
// → merge упал бы в рантайме (`verb foreach не поддержан` → state_changes_apply_
// failed → error_locked ПОСЛЕ apply на хостах). Ловим на этапе валидации (BUG-2).
func validateForeachOp(seen map[string]*ast.MappingValueNode, path string, vline, vcol int) []diag.Diagnostic {
	var out []diag.Diagnostic
	if as := seen["as"]; as == nil || stringFromNode(as.Value) == "" {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "foreach: requires as: <name> (binding for the current iteration element)",
			YAMLPath: path + ".as",
		}))
	} else if name := stringFromNode(as.Value); foreachReservedBindings[name] {
		tok := as.Key.GetToken()
		line, col := vline, vcol
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "reserved_binding_name",
			Message:  fmt.Sprintf("foreach.as %q shadows a reserved name (CEL context or per-element binding)", name),
			Hint:     "reserved: input, register, incarnation, soulprint, essence, vars, elem, key, value",
			YAMLPath: path + ".as",
		}))
	}
	doKV := seen["do"]
	if doKV == nil {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "foreach: requires do: [<verb...>] (operations applied per iteration)",
			YAMLPath: path + ".do",
		}))
	} else if seq, ok := doKV.Value.(*ast.SequenceNode); ok {
		if len(seq.Values) == 0 {
			out = append(out, diagAt(vline, vcol, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "empty_value",
				Message:  "foreach.do must contain at least one operation",
				YAMLPath: path + ".do",
			}))
		}
		for i, item := range seq.Values {
			doPath := fmt.Sprintf("%s.do[%d]", path, i)
			out = append(out, validateStateOp(item, doPath)...)
			out = append(out, rejectNestedForeach(item, doPath)...)
		}
	} else {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "foreach.do must be a sequence of operations",
			YAMLPath: path + ".do",
		}))
	}
	out = append(out, rejectKeys(seen, path, []string{"value", "key", "match", "on_conflict", "patch", "expect"}, "foreach:")...)
	return out
}

// rejectNestedForeach отбраковывает foreach-глагол внутри do: вложенный цикл вне
// грамматики ADR-057 (do несёт только CRUD-глаголы). Проверка по AST — ищет ключ
// `foreach` среди ключей do-элемента.
func rejectNestedForeach(node ast.Node, path string) []diag.Diagnostic {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return nil
	}
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value != "foreach" {
			continue
		}
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "nested_foreach_unsupported",
			Message:  "nested foreach in do: is not supported (do: carries CRUD verbs set/add/modify/remove only) — ADR-057",
			Hint:     "flatten the iteration: a single foreach with the combined collection, or precompute the list in vars:",
			YAMLPath: path + ".foreach",
		})}
	}
	return nil
}

// validatePatchMap проверяет, что `patch:` — mapping (путь-в-элементе → значение).
// Пустой patch валиден грамматически (no-op merge), но бессмыслен — допускаем без
// ошибки (симметрично пустому state_changes).
func validatePatchMap(patchKV *ast.MappingValueNode, path string) []diag.Diagnostic {
	if _, ok := patchKV.Value.(*ast.MappingNode); ok {
		return nil
	}
	tok := patchKV.Key.GetToken()
	line, col := 0, 0
	if tok != nil {
		line, col = tok.Position.Line, tok.Position.Column
	}
	return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:     "type_mismatch",
		Message:  "modify.patch must be a mapping of <path-in-element> → CEL/literal",
		YAMLPath: path + ".patch",
	})}
}

// validateExpectValue проверяет значение `expect` ∈ {one, at_most_one, any}.
func validateExpectValue(seen map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	exp := seen["expect"]
	if exp == nil {
		return nil
	}
	val := stringFromNode(exp.Value)
	if stateOpExpectValues[val] {
		return nil
	}
	tok := exp.Key.GetToken()
	line, col := 0, 0
	if tok != nil {
		line, col = tok.Position.Line, tok.Position.Column
	}
	return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:     "invalid_value",
		Message:  fmt.Sprintf("expect %q is invalid (expected one / at_most_one / any)", val),
		YAMLPath: path + ".expect",
	})}
}

// warnWideMatch — предохранитель (a) ADR-057 §d: modify/remove без match: ИЛИ с
// константно-истинным предикатом (`match: true`) перепатчат/снесут ВСЮ коллекцию.
// Не ошибка (автор мог хотеть именно «всех»), но WARN — намерение должно быть
// явным. soul-lint выводит warn, exit-code остаётся 0.
func warnWideMatch(seen map[string]*ast.MappingValueNode, path string, vline, vcol int, verb string) []diag.Diagnostic {
	m := seen["match"]
	if m == nil {
		return []diag.Diagnostic{diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "wide_match",
			Message:  fmt.Sprintf("%s without match: affects the WHOLE collection (all elements)", verb),
			Hint:     "add match: \"<CEL-predicate>\" to scope the operation, or confirm bulk intent is desired",
			YAMLPath: path,
		})}
	}
	if isConstTrueMatch(stringFromNode(m.Value)) {
		tok := m.Key.GetToken()
		line, col := vline, vcol
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "wide_match",
			Message:  fmt.Sprintf("%s with constant-true match affects the WHOLE collection (all elements)", verb),
			Hint:     "narrow the predicate (key == X / elem.id == Y), or confirm bulk intent is desired",
			YAMLPath: path + ".match",
		})}
	}
	return nil
}

// isConstTrueMatch распознаёт константно-истинный предикат (`true`, `1 == 1`),
// сносящий/патчащий всю коллекцию. Полноценный CEL-анализ не нужен — ловим
// очевидную литеральную форму `true` (с возможными пробелами/обёрткой `${ }`).
//
// TODO(wide-match): расширить до «предикат не ссылается на elem/key/value» (любой
// match, игнорирующий элемент, зацепляет всю коллекцию — подозрителен). Корректно
// это требует CEL-AST-обхода (shared/cel): regex по идентификаторам даёт ложные
// срабатывания на `register.value`/`input.key`/поле `x.elem`. Пока ловим только
// литерал `true`; полное покрытие — отдельным слайсом с разбором AST.
func isConstTrueMatch(expr string) bool {
	s := strings.TrimSpace(expr)
	s = strings.TrimPrefix(s, "${")
	s = strings.TrimSuffix(s, "}")
	return strings.TrimSpace(s) == "true"
}

// validateSetOp — `set` требует value; match/key/on_conflict/patch/expect
// неприменимы. `expect` — ассерт кратности ТОЛЬКО для modify/remove (ADR-057 §c);
// на set он молча игнорировался бы движком (ловушка для оператора, BUG-1).
func validateSetOp(seen map[string]*ast.MappingValueNode, path string, vline, vcol int) []diag.Diagnostic {
	var out []diag.Diagnostic
	if seen["value"] == nil {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "set: requires value: (CEL-expression or literal to overwrite the field)",
			YAMLPath: path + ".value",
		}))
	}
	out = append(out, rejectKeys(seen, path, []string{"match", "key", "on_conflict", "patch", "expect", "in", "as", "do"}, "set:")...)
	return out
}

// validateAddOp — `add` требует value; on_conflict ∈ {skip,replace,error};
// key (map) / match (list-дедуп) опц.; patch/expect/in/as/do неприменимы.
// `expect` — ассерт кратности ТОЛЬКО для modify/remove (ADR-057 §c); на add он
// молча игнорировался бы движком (ловушка: оператор ждёт страховку от дубля на
// add, её там нет — дедуп делает on_conflict, BUG-1).
func validateAddOp(seen map[string]*ast.MappingValueNode, path string, vline, vcol int) []diag.Diagnostic {
	var out []diag.Diagnostic
	if seen["value"] == nil {
		out = append(out, diagAt(vline, vcol, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "add: requires value: (element to add — object or scalar)",
			YAMLPath: path + ".value",
		}))
	}
	if oc := seen["on_conflict"]; oc != nil {
		val := stringFromNode(oc.Value)
		if !stateOpConflictValues[val] {
			tok := oc.Key.GetToken()
			line, col := vline, vcol
			if tok != nil {
				line, col = tok.Position.Line, tok.Position.Column
			}
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "invalid_value",
				Message:  fmt.Sprintf("on_conflict %q is invalid (expected skip / replace / error)", val),
				YAMLPath: path + ".on_conflict",
			}))
		}
	}
	out = append(out, rejectKeys(seen, path, []string{"patch", "expect", "in", "as", "do"}, "add:")...)
	return out
}

// rejectKeys поднимает unknown_key для каждого присутствующего, но неприменимого
// к глаголу ключа (например, patch: на add, match: на set).
func rejectKeys(seen map[string]*ast.MappingValueNode, path string, keys []string, verb string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, k := range keys {
		kv := seen[k]
		if kv == nil {
			continue
		}
		tok := kv.Key.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "unknown_key",
			Message:  fmt.Sprintf("%s does not accept %q", verb, k),
			YAMLPath: path + "." + k,
		}))
	}
	return out
}

// lineOf/colOf — позиция узла (fallback 0 при отсутствии токена).
func lineOf(node ast.Node) int {
	if tok := node.GetToken(); tok != nil {
		return tok.Position.Line
	}
	return 0
}

func colOf(node ast.Node) int {
	if tok := node.GetToken(); tok != nil {
		return tok.Position.Column
	}
	return 0
}

// findValueNode — raw value-узел под top-level ключом name (любой формы:
// mapping/sequence/scalar). Параллель findInputMapping/findSequenceValue, но без
// фильтра по виду — нужен для dual-form-диспетчеризации (validateStateChanges).
func findValueNode(root *ast.MappingNode, name string) ast.Node {
	if root == nil {
		return nil
	}
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value != name {
			continue
		}
		return kv.Value
	}
	return nil
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
