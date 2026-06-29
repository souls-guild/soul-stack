package config

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// inputSchemaType / inputSchemaMapType — точки остановки reflect-walker-а.
// Типы InputSchema / InputSchemaMap имеют собственную (рекурсивную) валидацию
// в `input_schema.go`: ключи map произвольные, у InputSchema `required` имеет
// два смысла. Общий reflect-walker не подходит и должен в эти типы не
// заходить.
var (
	inputSchemaType    = reflect.TypeOf(InputSchema{})
	inputSchemaMapType = reflect.TypeOf(InputSchemaMap(nil))
)

// destinyManifestType / serviceManifestType / scenarioManifestType — для
// подавления generic-`unknown_key` для top-level deprecated-ключей. По ним
// поднимает диагностику schemaValidateDestiny / schemaValidateService /
// schemaValidateScenario с осмысленным hint-ом; reflect-walker иначе
// добавлял бы вторую `unknown_key` без hint — дубль в JSON-выводе на ту же
// line/col.
var (
	destinyManifestType  = reflect.TypeOf(DestinyManifest{})
	serviceManifestType  = reflect.TypeOf(ServiceManifest{})
	scenarioManifestType = reflect.TypeOf(ScenarioManifest{})
)

// covenantFragmentType — covenant.yml. Generic-walker по reflect-полям
// ScenarioFragment (только 4 секции) пометил бы любой чужой top-level ключ
// (name/tasks/create/form/extends) как `unknown_key`; covenant-валидатор
// validateCovenantFragment поднимает по ним точный `covenant_unexpected_key`
// с hint-ом. Чтобы не было дубля на ту же line/col — walker для covenant молчит
// про unknown на верхнем уровне (suppressAll), отдавая разбор валидатору.
var covenantFragmentType = reflect.TypeOf(ScenarioFragment{})

// taskType — точка остановки reflect-walker-а. У Task свой UnmarshalYAML с
// дискриминатором и собственной валидацией (validateTaskNode); generic-обход
// по reflect-полям ловил бы `module:`/`include:` как unknown_key (они
// помечены `yaml:"-"` в Task) либо лез бы в полиморфные ветки. Симметрично
// inputSchemaType — стандартный паттерн «свой Unmarshal → стоп».
var taskType = reflect.TypeOf(Task{})

// stateChangesType — точка остановки reflect-walker-а. У `state_changes:`
// есть собственный валидатор validateStateChanges (по AST с осмысленным
// hint-ом про допустимые ключи sets/appends/modifies). Без suppress walker
// эмитит вторую `unknown_key` без hint-а на ту же line/col — дубль в
// JSON-выводе.
var stateChangesType = reflect.TypeOf(StateChanges{})

// computeBlockType — точка остановки reflect-walker-а. `compute:` — YAML-mapping
// `<имя>: <выражение>` со своим UnmarshalYAML (ComputeBlock) и валидатором
// validateComputeBlock. Без suppress generic-walker увидел бы []ComputeVar как
// sequence-of-struct и не нашёл бы соответствия mapping-узлу (ложные unknown_key).
var computeBlockType = reflect.TypeOf(ComputeBlock(nil))

// validateRuleSliceType — точка остановки reflect-walker-а для `validate:`.
// У блока собственный AST-валидатор validateValidateBlock (требует that/message,
// компилирует that input-only). Generic-walker по slice-of-struct ловил бы только
// unknown_key, но дублировал бы его с validateValidateBlock на ту же line/col —
// suppress (симметрично stateChangesType/computeBlockType).
var validateRuleSliceType = reflect.TypeOf([]ValidateRule(nil))

// formLayoutType — точка остановки reflect-walker-а для `form:`. У блока
// собственный AST-валидатор validateFormLayout (cross-инварианты против input:
// + unknown_key с осмысленным hint-ом по секциям/полям). Generic-walker по
// FormLayout-структуре дублировал бы unknown_key на ту же line/col — suppress
// (симметрично validateRuleSliceType/computeBlockType).
var formLayoutType = reflect.TypeOf(FormLayout{})

// walkUnknownKeys обходит AST-mapping `root` и yaml-теги типа `cfg`,
// собирая `unknown_key` диагностики для ключей, которых нет в Go-структуре.
//
// Используется вместо `yaml.Strict()`, потому что Strict падает на первой
// ошибке — нам важно показать все unknown сразу.
//
// Рекурсивно опускается в подмаппинги/слайсы. По slice-у проходит каждый
// элемент с тем же типом element-а.
func walkUnknownKeys(_ string, m *ast.MappingNode, cfg any, pathPrefix string) []diag.Diagnostic {
	t := reflect.TypeOf(cfg)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return walkMappingAgainstStruct(m, t, pathPrefix)
}

// walkMappingAgainstStruct — рекурсивный обход MappingNode + reflect.Type
// (struct). Возвращает список найденных unknown_key-ошибок.
//
// На каждом уровне:
//   - для каждой пары key→value мапы ищем поле struct по yaml-тегу;
//   - если поля нет — `unknown_key`;
//   - если есть, и под-значение — mapping/sequence — рекурсивно обходим
//     соответствующий элемент типа.
func walkMappingAgainstStruct(m *ast.MappingNode, t reflect.Type, path string) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	known := yamlFieldIndex(t)
	// Подавление дублей: для DestinyManifest deprecated top-level-ключи
	// генерят более информативную диагностику в schemaValidateDestiny (с hint).
	// Не выпускаем по ним вторую `unknown_key` отсюда — set-сравнение в
	// тестах прячет факт дубля, в JSON-выводе он виден строкой-двойником.
	var suppress map[string]bool
	var suppressAllUnknown bool
	switch t {
	case destinyManifestType:
		suppress = make(map[string]bool, len(deprecatedDestinyKeys))
		for k := range deprecatedDestinyKeys {
			suppress[k] = true
		}
	case serviceManifestType:
		suppress = make(map[string]bool, len(deprecatedServiceKeys))
		for k := range deprecatedServiceKeys {
			suppress[k] = true
		}
	case scenarioManifestType:
		suppress = make(map[string]bool, len(deprecatedScenarioKeys))
		for k := range deprecatedScenarioKeys {
			suppress[k] = true
		}
	case covenantFragmentType:
		suppressAllUnknown = true
	}
	var out []diag.Diagnostic
	for _, kv := range m.Values {
		key := kv.Key.GetToken()
		if key == nil {
			continue
		}
		keyName := key.Value
		fieldType, ok := known[keyName]
		if !ok {
			if suppressAllUnknown || suppress[keyName] {
				continue
			}
			out = append(out, diag.Diagnostic{
				Level:    diag.LevelError,
				Phase:    diag.PhaseSchemaValidate,
				Line:     key.Position.Line,
				Column:   key.Position.Column,
				Code:     "unknown_key",
				Message:  `unknown field "` + keyName + `"`,
				YAMLPath: path + "." + keyName,
			})
			continue
		}
		out = append(out, walkValueAgainstType(kv.Value, fieldType, path+"."+keyName)...)
	}
	return out
}

// walkValueAgainstType — рекурсия по AST-узлу + reflect.Type ожидаемого поля.
//
// Семантика по типу поля:
//   - struct → значение должно быть mapping, рекурсия в walkMappingAgainstStruct.
//   - pointer-to-struct → разворачиваем элемент, потом как struct.
//   - slice — пробегаем элементы (если element — struct, обходим каждый
//     против element-типа).
//   - map → не валидируем рекурсивно (используется только для reaper.rules,
//     где ключи произвольные). Если value-тип — struct, обходим каждое
//     значение.
//   - leaf-типы (string/int/bool) — не обходим.
func walkValueAgainstType(n ast.Node, t reflect.Type, path string) []diag.Diagnostic {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	// Останавливаем рекурсию у типов с особой валидацией (см. doc-comment
	// на inputSchemaType): unknown_keys и schema-проверки делает
	// validateInputSchemaMap/Node в input_schema.go.
	if t == inputSchemaType || t == inputSchemaMapType {
		return nil
	}
	// Task — собственный UnmarshalYAML + validateTaskNode по AST.
	if t == taskType {
		return nil
	}
	// StateChanges — собственный валидатор validateStateChanges с hint-ом.
	if t == stateChangesType {
		return nil
	}
	// ComputeBlock — собственный UnmarshalYAML (mapping имя→выражение) + валидатор
	// validateComputeBlock; generic-walker как по slice-of-struct сюда не заходит.
	if t == computeBlockType {
		return nil
	}
	// []ValidateRule — собственный AST-валидатор validateValidateBlock; generic-
	// walker по slice-of-struct сюда не заходит (дублировал бы unknown_key).
	if t == validateRuleSliceType {
		return nil
	}
	// FormLayout — собственный AST-валидатор validateFormLayout (cross-инварианты +
	// unknown_key по секциям/полям); generic-walker сюда не заходит.
	if t == formLayoutType {
		return nil
	}

	switch t.Kind() {
	case reflect.Struct:
		mm, ok := n.(*ast.MappingNode)
		if !ok {
			return nil
		}
		return walkMappingAgainstStruct(mm, t, path)

	case reflect.Slice, reflect.Array:
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() != reflect.Struct {
			return nil
		}
		// stop-types: у Task собственный UnmarshalYAML и собственный валидатор
		// validateTaskNode по AST — generic-walker заходить не должен.
		if elem == taskType {
			return nil
		}
		seq, ok := n.(*ast.SequenceNode)
		if !ok {
			return nil
		}
		var out []diag.Diagnostic
		for i, item := range seq.Values {
			itemPath := path + "[" + strconv.Itoa(i) + "]"
			if mm, ok := item.(*ast.MappingNode); ok {
				out = append(out, walkMappingAgainstStruct(mm, elem, itemPath)...)
			}
		}
		return out

	case reflect.Map:
		valT := t.Elem()
		for valT.Kind() == reflect.Pointer {
			valT = valT.Elem()
		}
		if valT.Kind() != reflect.Struct {
			return nil
		}
		mm, ok := n.(*ast.MappingNode)
		if !ok {
			return nil
		}
		var out []diag.Diagnostic
		for _, kv := range mm.Values {
			key := kv.Key.GetToken()
			if key == nil {
				continue
			}
			subPath := path + "." + key.Value
			if subMM, ok := kv.Value.(*ast.MappingNode); ok {
				out = append(out, walkMappingAgainstStruct(subMM, valT, subPath)...)
			}
		}
		return out
	}
	return nil
}

// yamlFieldIndex возвращает map yaml-имя → reflect.Type поля. Учитывает
// `omitempty`/прочие модификаторы (берётся только первая часть тега).
// Поля с тегом "-" пропускаются.
func yamlFieldIndex(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name := tag
		if i := strings.IndexByte(name, ','); i >= 0 {
			name = name[:i]
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}
