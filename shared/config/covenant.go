package config

// Covenant — переиспользуемый фрагмент общего контракта секций сценария
// (`covenant.yml` в корне service-репо). Сценарий подключает его через
// `extends: <covenant-name>` (ScenarioManifest.Extends) и наследует МИНИМУМ
// секций input/compute/state_changes/validate; собственные секции сценария
// ДОБАВЛЯЮТСЯ поверх (add-only merge, mergeSections).
//
// Граница слоёв (S1 — этот файл): config-уровень даёт типы (ScenarioFragment),
// merge-операцию (mergeSections) и валидацию ФОРМЫ фрагмента/extends. Сам резолв
// фрагмента по файловой системе снапшота (чтение covenant.yml по имени из
// extends) — keeper-side (S2, LoadScenarioManifestResolved): он декодирует
// фрагмент через [LoadCovenantFragmentFromBytes] и сольёт его в манифест через
// [MergeCovenant].

import (
	"fmt"
	"os"
	"regexp"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ScenarioFragment — типизированный covenant.yml. Несёт ТОЛЬКО общий контракт
// четырёх секций; идентичность/задачи/форма/наследование сценария фрагменту НЕ
// принадлежат (covenant — это контракт, а не самостоятельный сценарий):
//
//   - name/tasks/create — у фрагмента нет (он не исполняемый сценарий);
//   - form — презентационный слой остаётся сугубо локальным (mergeSections его
//     не трогает): один covenant обслуживает много сценариев с разной формой;
//   - extends — рекурсия covenant→covenant запрещена (covenant без extends),
//     иначе пришлось бы резолвить цепочку и закрывать циклы.
//
// Любой из перечисленных ключей в covenant.yml → covenant_unexpected_key
// (validateCovenantFragment). Декод — те же UnmarshalYAML, что у одноимённых
// секций ScenarioManifest (Input/Compute/StateChanges переиспользуют свои
// декодеры).
type ScenarioFragment struct {
	Input        InputSchemaMap `yaml:"input,omitempty"`
	Compute      ComputeBlock   `yaml:"compute,omitempty"`
	StateChanges *StateChanges  `yaml:"state_changes,omitempty"`
	Validate     []ValidateRule `yaml:"validate,omitempty"`
}

// reCovenantName — имя covenant-фрагмента в `extends:` (оно же базовое имя файла
// `covenant.yml`-семейства). Строго одноуровневое kebab-имя: начинается с буквы,
// далее буквы/цифры/дефис. По построению исключает `/`, `.`, `..` и абсолютный
// путь — traversal-кламп обеспечивается грамматикой имени, а не пост-проверкой
// filepath (имя НЕ может выразить выход за каталог).
var reCovenantName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidExtendsName сообщает, годно ли имя в `extends:` как ссылка на covenant
// (форма + traversal-кламп). Пустая строка → false (это «нет наследования», не
// валидное имя — вызывающая сторона различает пустоту до проверки). Единый
// источник правды о форме covenant-имени для config-валидатора (semantic-слой) и
// keeper-side резолвера (S2): резолвер строит путь к covenant.yml ТОЛЬКО для
// имени, прошедшего эту проверку.
func ValidExtendsName(name string) bool {
	return reCovenantName.MatchString(name)
}

// covenantFragmentKnownKeys — закрытый набор top-level ключей covenant.yml.
// Ключи, принадлежащие сценарию, но НЕ фрагменту (name/tasks/create/form/
// extends/description/vars), сюда не входят — их присутствие даёт
// covenant_unexpected_key (фрагмент несёт только контракт четырёх секций).
var covenantFragmentKnownKeys = map[string]bool{
	"input":         true,
	"compute":       true,
	"state_changes": true,
	"validate":      true,
}

// validateCovenantFragment — schema-time проверка ФОРМЫ covenant.yml поверх уже
// провалидированных секций (структура input/compute/state_changes/validate
// валидируется их собственными валидаторами в schemaValidateCovenant). Здесь —
// covenant-специфичный инвариант: лишний ключ, принадлежащий сценарию, а не
// фрагменту → covenant_unexpected_key (fail-closed). В частности `extends:`
// внутри covenant → covenant_unexpected_key: рекурсия covenant→covenant вне
// грамматики.
func validateCovenantFragment(root *ast.MappingNode) []diag.Diagnostic {
	if root == nil {
		return nil
	}
	var out []diag.Diagnostic
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		key := tok.Value
		if covenantFragmentKnownKeys[key] {
			continue
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "covenant_unexpected_key",
			Message:  fmt.Sprintf("covenant.yml carries only the shared section contract; unexpected field %q", key),
			Hint:     "covenant.yml allows only input / compute / state_changes / validate — name/tasks/create/form/extends belong to the scenario, not the fragment",
			YAMLPath: "$." + key,
		}))
	}
	return out
}

// MergeCovenant сливает covenant-фрагмент в манифест сценария ADD-ONLY: фрагмент
// — БАЗА (минимум), сценарий ДОБАВЛЯЕТ дельту. Shallow по верхнему ключу секции;
// дубль ключа в обоих (одно имя input-поля, одно имя compute, один `set <поле>`)
// → ошибка (fail-closed, НЕ last-wins/override — это защита от незаметного
// переопределения общего контракта). Порядок append — covenant-ПЕРВЫМ
// (compute/state_changes/validate): общий контракт логически предшествует
// дельте сценария.
//
// `form` НЕ трогается (остаётся локальным — fragment его не несёт). Вызов делает
// keeper-side резолвер (S2) ПОСЛЕ того, как обе стороны раздельно прошли
// schema-валидацию: ошибки здесь — только конфликты ключей, не структурные.
//
// local обязан быть не-nil (резолвер вызывает на уже декодированном манифесте).
// Возвращает первую найденную ошибку конфликта (детерминированно по порядку
// секций input → compute → state_changes → validate, внутри секции — по порядку
// ключей фрагмента), чтобы оператор чинил конфликты по одному.
func MergeCovenant(fragment ScenarioFragment, local *ScenarioManifest) error {
	if err := mergeInputSections(fragment.Input, local); err != nil {
		return err
	}
	if err := mergeComputeSections(fragment.Compute, local); err != nil {
		return err
	}
	if err := mergeStateChangeSections(fragment.StateChanges, local); err != nil {
		return err
	}
	mergeValidateSections(fragment.Validate, local)
	return nil
}

// mergeInputSections — shallow union input по имени поля. Дубль имени в обоих →
// section_key_conflict (НЕ last-wins: covenant задаёт общее поле, сценарий не
// вправе молча переопределить его схему). Поля фрагмента, которых нет у
// сценария, добавляются как есть (covenant — минимум).
func mergeInputSections(fragment InputSchemaMap, local *ScenarioManifest) error {
	if len(fragment) == 0 {
		return nil
	}
	if local.Input == nil {
		local.Input = make(InputSchemaMap, len(fragment))
	}
	for name, schema := range fragment {
		if _, dup := local.Input[name]; dup {
			return &SectionKeyConflict{Section: "input", Key: name}
		}
		local.Input[name] = schema
	}
	return nil
}

// mergeComputeSections — append covenant-ПЕРВЫМ: результат = fragment ++ local,
// порядок внутри каждой стороны сохранён (compute[i] ссылается на ранее
// объявленный compute[j], j<i — covenant-объявления становятся доступны
// дельте сценария). Дубль имени compute → section_key_conflict.
func mergeComputeSections(fragment ComputeBlock, local *ScenarioManifest) error {
	if len(fragment) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(fragment)+len(local.Compute))
	for _, cv := range fragment {
		seen[cv.Name] = true
	}
	for _, cv := range local.Compute {
		if seen[cv.Name] {
			return &SectionKeyConflict{Section: "compute", Key: cv.Name}
		}
	}
	merged := make(ComputeBlock, 0, len(fragment)+len(local.Compute))
	merged = append(merged, fragment...)
	merged = append(merged, local.Compute...)
	local.Compute = merged
	return nil
}

// mergeStateChangeSections — append covenant-ПЕРВЫМ для list-формы (Ops):
// результат = fragment.Ops ++ local.Ops. Конфликт — только по `set <поле>`
// (перезапись одного поля дважды: covenant и сценарий борются за финальное
// значение — section_key_conflict). Прочие глаголы (add/modify/remove/foreach)
// у одного поля множественны легитимно (несколько add в коллекцию, патчи по
// разным match) — НЕ конфликт.
//
// Map-форма (DEPRECATED, Sets): union по ключу `Sets`, дубль → section_key_
// conflict. Смешение форм (covenant list + scenario map или наоборот) не
// поддержано — это разные грамматики state_changes; резолвер (S2) обязан был
// отвергнуть такой манифест раньше. Здесь: при несовпадении IsList берём более
// строгий путь — конфликт не детектируется кросс-форменно, append идёт по
// форме local (covenant другой формы оставляем валидатору S2).
func mergeStateChangeSections(fragment *StateChanges, local *ScenarioManifest) error {
	if fragment == nil {
		return nil
	}
	if local.StateChanges == nil {
		local.StateChanges = &StateChanges{IsList: fragment.IsList}
	}
	sc := local.StateChanges

	// list-форма: append Ops covenant-первым, конфликт по set <поле>.
	if fragment.IsList || sc.IsList {
		localSets := make(map[string]bool)
		for _, op := range sc.Ops {
			if op.Verb == VerbSet {
				localSets[op.Field] = true
			}
		}
		for _, op := range fragment.Ops {
			if op.Verb == VerbSet && localSets[op.Field] {
				return &SectionKeyConflict{Section: "state_changes", Key: "set " + op.Field}
			}
		}
		sc.IsList = true
		sc.Ops = append(append(make([]StateChange, 0, len(fragment.Ops)+len(sc.Ops)), fragment.Ops...), sc.Ops...)
		return nil
	}

	// map-форма (DEPRECATED): union Sets, дубль ключа → конфликт.
	for field := range fragment.Sets {
		if _, dup := sc.Sets[field]; dup {
			return &SectionKeyConflict{Section: "state_changes", Key: "set " + field}
		}
	}
	if len(fragment.Sets) > 0 && sc.Sets == nil {
		sc.Sets = make(map[string]string, len(fragment.Sets))
	}
	for field, expr := range fragment.Sets {
		sc.Sets[field] = expr
	}
	return nil
}

// mergeValidateSections — append covenant-ПЕРВЫМ. Правила НАКАПЛИВАЮТСЯ (без
// дубль-детекта): два правила могут совпадать текстуально и это не ошибка —
// validate — это конъюнкция инвариантов, лишнее правило лишь повторно
// проверяет то же предусловие. covenant-инварианты вычисляются первыми.
func mergeValidateSections(fragment []ValidateRule, local *ScenarioManifest) {
	if len(fragment) == 0 {
		return
	}
	merged := make([]ValidateRule, 0, len(fragment)+len(local.Validate))
	merged = append(merged, fragment...)
	merged = append(merged, local.Validate...)
	local.Validate = merged
}

// schemaValidateCovenant — пост-decode проверки covenant.yml (ScenarioFragment).
// Сначала covenant-специфичный инвариант формы (validateCovenantFragment: только
// 4 секции, чужой ключ → covenant_unexpected_key), затем СТРУКТУРА каждой
// присутствующей секции теми же валидаторами, что у одноимённых секций сценария
// (covenant несёт тот же DSL — переиспользуем, не дублируем).
func schemaValidateCovenant(_ string, root *ast.MappingNode, m *ScenarioFragment) []diag.Diagnostic {
	out := validateCovenantFragment(root)

	topKeys := topLevelKeys(root)
	if topKeys["input"] {
		out = append(out, validateInputSchemaMap(m.Input, findInputMapping(root, "input"), "$.input")...)
	}
	if topKeys["compute"] {
		out = append(out, validateComputeBlock(root, "$.compute")...)
	}
	if topKeys["state_changes"] {
		out = append(out, validateStateChanges(root, "$.state_changes")...)
	}
	if topKeys["validate"] {
		out = append(out, validateValidateBlock(root, "$.validate")...)
	}
	return out
}

// LoadCovenantFragment — точка входа с I/O: читает `covenant.yml` по пути и
// декодирует+валидирует фрагмент. Контракт идентичен LoadScenarioManifest.
// keeper-side резолвер (S2) строит путь к covenant.yml по имени из extends
// (через ValidExtendsName) и зовёт это.
func LoadCovenantFragment(path string, opts ValidateOptions) (*ScenarioFragment, *Document, []diag.Diagnostic, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File: path, Code: "io_error", Message: err.Error(),
		}}, err
	}
	frag, doc, diags := LoadCovenantFragmentFromBytes(path, src, opts)
	return frag, doc, diags, nil
}

// LoadCovenantFragmentFromBytes — основная точка входа без I/O (для тестов и
// keeper-side резолва из снапшота в памяти). Декод + валидация формы covenant.
// semantic-фаза covenant пуста (форма проверяется в schema-фазе через
// schemaValidateCovenant); covenantNoSemantic держит сигнатуру parseAndValidate.
func LoadCovenantFragmentFromBytes(filename string, data []byte, opts ValidateOptions) (*ScenarioFragment, *Document, []diag.Diagnostic) {
	cfg := &ScenarioFragment{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, covenantNoSemantic)
	return cfg, doc, diags
}

// covenantNoSemantic — пустая semantic-фаза covenant (вся валидация covenant —
// schema-time). Параллель semanticValidateScenario, но фрагменту нечего
// проверять кросс-полево (нет tasks/register-графа; форма — в schema-фазе).
func covenantNoSemantic(_ *ScenarioFragment, _ *ast.MappingNode) []diag.Diagnostic {
	return nil
}

// SectionKeyConflict — конфликт add-only merge: ключ секции объявлен и в
// covenant, и в сценарии. Несёт секцию (input/compute/state_changes) и ключ
// (имя поля / имя compute / `set <поле>`) для адресной диагностики.
type SectionKeyConflict struct {
	Section string
	Key     string
}

// Code — машинный код диагностики (для маппинга в diag/HTTP-слой S2).
func (e *SectionKeyConflict) Code() string { return "section_key_conflict" }

func (e *SectionKeyConflict) Error() string {
	return fmt.Sprintf("section_key_conflict: %s.%s declared in both covenant and scenario (add-only merge forbids override)", e.Section, e.Key)
}
