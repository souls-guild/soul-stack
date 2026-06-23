package config

// InputSchema / InputSchemaMap — типизированное представление блока `input:`
// (и симметричного `output:`) по нормативной спеке [`docs/input.md`]. Один и
// тот же DSL используется в destiny.yml / scenario/<name>/main.yml / манифесте
// модуля; реализация общая.
//
// `required` в DSL имеет два смысла, разделённых по контексту:
//   - на уровне параметра (любой type) — bool «обязателен ли параметр»;
//   - внутри type=object — []string список обязательных под-ключей `properties`.
//
// Чтобы downstream-код не парсил `any`, разделяем эти смыслы в Go: поля
// `Required` и `RequiredProps`. Кастомный `UnmarshalYAML` решает по типу
// YAML-ноды (`!!bool` → Required, `!!seq` → RequiredProps); semantic-validate
// проверяет согласованность с `type`.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// InputSchemaMap — отображение «имя параметра» → схема.
type InputSchemaMap map[string]*InputSchema

// InputSchema — схема одного параметра по [`docs/input.md`].
//
// Все поля валидны на разных type-ах одновременно по форме (YAML принимает
// «лишний ключ для типа»), но semantic-validate отвергает их как
// `input_key_invalid_for_type`. Pointer-ы (*int/*float64) различают
// «отсутствует» и «zero-value»; bool-флаги хранятся как bool (отсутствие = false).
//
// Поля `Required` и `RequiredProps` помечены `yaml:"-"` — заполняются через
// `UnmarshalYAML` ниже, чтобы reflect-walker не возмущался отсутствием
// `required` среди известных ключей.
type InputSchema struct {
	Type        string `yaml:"type"`
	Default     any    `yaml:"default,omitempty"`
	Enum        []any  `yaml:"enum,omitempty"`
	Secret      bool   `yaml:"secret,omitempty"`
	Description string `yaml:"description,omitempty"`

	Required      bool     `yaml:"-"`
	RequiredProps []string `yaml:"-"`
	requiredKind  requiredKind

	// RequiredWhen — CEL-предикат над `input.*`: параметр обязателен, КОГДА
	// предикат истинен (docs/input.md → «Условная обязательность»). Применим к
	// любому type. Условная пара безусловного Required: тот всегда обязателен,
	// этот — при истинном предикате. Контекст предиката — только input.* (это
	// input-валидация, не render): прочие имена → compile-ошибка узкого env
	// (см. requireInputValues / input_required_when.go). Пустая строка =
	// отсутствие ключа.
	RequiredWhen string `yaml:"required_when,omitempty"`
	// rawRequired сохраняет исходный AST-узел `required:` до классификации
	// в `requiredKind`. Используется в `validateInputSchemaNode`, чтобы
	// поднять `input_required_value_invalid`, когда значение не bool и не
	// sequence строк (см. UnmarshalYAML, ветка default).
	rawRequired ast.Node

	Pattern    string `yaml:"pattern,omitempty"`
	Format     string `yaml:"format,omitempty"`
	MinLength  *int   `yaml:"min_length,omitempty"`
	MaxLength  *int   `yaml:"max_length,omitempty"`
	AllowEmpty bool   `yaml:"allow_empty,omitempty"`

	// VaultScope — единственный prefix-glob, ограничивающий, какие пути Vault
	// KV оператор вправе зарезолвить через `vault:`-ref в значении ЭТОГО поля
	// (docs/input.md → «vault_scope»). Применим только к `type: string` +
	// `secret: true`. Без объявленного scope `vault:`-ref в значении поля —
	// ошибка резолва (default-deny). Авторских `vault:`-refs в task params это
	// НЕ касается — там отдельный доверенный канал (ADR-010).
	VaultScope string `yaml:"vault_scope,omitempty"`

	Min          *float64 `yaml:"min,omitempty"`
	Max          *float64 `yaml:"max,omitempty"`
	ExclusiveMin *float64 `yaml:"exclusive_min,omitempty"`
	ExclusiveMax *float64 `yaml:"exclusive_max,omitempty"`

	Items    *InputSchema `yaml:"items,omitempty"`
	MinItems *int         `yaml:"min_items,omitempty"`
	MaxItems *int         `yaml:"max_items,omitempty"`
	Unique   bool         `yaml:"unique,omitempty"`

	// Source — каталог-источник допустимых значений поля (ADR-044 S-T1).
	// Объект-дискриминатор: ровно один под-ключ задаёт множество, из которого
	// backend строит форму выбора оператору (напр. список SID инкарнации).
	// Применим к type=string (single-выбор) и type=array с items.type=string
	// (multi-выбор; лимиты — через min_items/max_items). Schema-валидация
	// проверяет только СТРУКТУРУ source (известный под-ключ + тип значения);
	// само множество резолвит backend при подготовке формы — не здесь.
	Source *InputSource `yaml:"source,omitempty"`

	Properties           InputSchemaMap `yaml:"properties,omitempty"`
	AdditionalProperties any            `yaml:"additional_properties,omitempty"`
}

// InputSource — объект-дискриминатор каталога-источника значений поля (ADR-044
// S-T1). Ровно один под-ключ задаёт множество:
//   - IncarnationHosts (`incarnation_hosts: true`) — все SID текущей инкарнации;
//   - Choir (`choir: <name>`) — SID-ы конкретной Choir-партии инкарнации.
//
// Schema-валидация проверяет только структурную валидность (известные под-ключи,
// типы значений); резолв самого множества и проверку «значение ∈ множество» —
// backend при подготовке формы (см. input_value.go).
type InputSource struct {
	IncarnationHosts bool   `yaml:"incarnation_hosts,omitempty" json:"incarnation_hosts,omitempty"`
	Choir            string `yaml:"choir,omitempty" json:"choir,omitempty"`
}

type requiredKind int

const (
	requiredAbsent requiredKind = iota
	requiredBool                // верхнеуровневый `required: true/false`
	requiredList                // `required: [name1, name2]` внутри object
)

// Допустимые значения `type` и `format` — фиксированы [`docs/input.md`].
var (
	inputTypeEnum   = []string{"string", "integer", "number", "boolean", "array", "object"}
	inputFormatEnum = []string{
		"hostname", "fqdn", "ipv4", "ipv6", "cidr",
		"email", "uri", "uuid", "semver", "duration",
		"sid", // FQDN-форма SID (ADR-044 S-T1), валидатор reSID в input_value.go
	}

	// reInputParamName — имена input-параметров (ключи map). Параметр
	// доступен в шаблонах как `input.<name>` (CEL / text/template); точки,
	// пробелы, leading digit ломают template-resolution, поэтому форма
	// строго snake_case.
	reInputParamName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	// Закрытый набор YAML-ключей внутри одной InputSchema-ноды.
	// Используется для unknown_key-проверки (вне reflect-walker, потому
	// что у InputSchema особый смысл `required` и рекурсивные поля).
	inputSchemaKnownKeys = map[string]bool{
		"type": true, "required": true, "required_when": true, "default": true, "enum": true,
		"secret": true, "description": true,

		"pattern": true, "format": true, "min_length": true,
		"max_length": true, "allow_empty": true, "vault_scope": true,

		"min": true, "max": true, "exclusive_min": true, "exclusive_max": true,

		"items": true, "min_items": true, "max_items": true, "unique": true,

		"properties": true, "additional_properties": true,

		"source": true,
	}

	// inputSourceKnownKeys — закрытый набор под-ключей объекта-дискриминатора
	// source: (ADR-044 S-T1). Любой иной под-ключ — unknown_key.
	inputSourceKnownKeys = map[string]bool{
		"incarnation_hosts": true,
		"choir":             true,
	}
)

// UnmarshalYAML — кастомный декод для разрешения двух смыслов `required`.
//
// goccy зовёт этот метод вместо стандартного reflect-decode-а, передавая
// AST-узел. Мы:
//  1. снимаем подмаппинг по известным ключам, разрешая `required` отдельно;
//  2. для остальных ключей зовём общий goccy-decoder через временный
//     alias-тип (чтобы не словить рекурсию в UnmarshalYAML).
//
// Если value `required` — bool, кладём в `Required` (kind=requiredBool).
// Если sequence строк — в `RequiredProps` (kind=requiredList).
// Если что-то третье — кладём kind=requiredAbsent и поднимаем
// `input_required_value_invalid` в semantic-validate (через осмотр AST).
func (s *InputSchema) UnmarshalYAML(node ast.Node) error {
	m, ok := node.(*ast.MappingNode)
	if !ok {
		return fmt.Errorf("input schema must be a mapping, got %T", node)
	}
	// Snapshot `required`-узел и `additional_properties`-узел, выкусываем их
	// из mapping-а перед общим decode-ом: оба — поля с особой семантикой
	// (см. doc-комменты), goccy не знает, как их типизировать корректно.
	var reqNode, apNode ast.Node
	filtered := &ast.MappingNode{
		BaseNode:    m.BaseNode,
		Start:       m.Start,
		End:         m.End,
		IsFlowStyle: m.IsFlowStyle,
		Values:      make([]*ast.MappingValueNode, 0, len(m.Values)),
	}
	for _, kv := range m.Values {
		tok := kv.Key.GetToken()
		if tok != nil {
			switch tok.Value {
			case "required":
				reqNode = kv.Value
				continue
			case "additional_properties":
				apNode = kv.Value
				continue
			}
		}
		filtered.Values = append(filtered.Values, kv)
	}

	// Декодируем «всё кроме required/additional_properties» через alias-тип,
	// чтобы избежать рекурсии в UnmarshalYAML. Используем yaml.NodeToValue
	// по той же механике, что и общий parser.
	type rawSchema InputSchema
	var raw rawSchema
	if err := yaml.NodeToValue(filtered, &raw); err != nil {
		return err
	}
	*s = InputSchema(raw)

	// additional_properties: bool | schema. Под schema требуется
	// рекурсивный декод через *InputSchema, иначе recurseItemsProperties
	// не знает типа дочерней схемы (см. Bug 2 — pattern на integer внутри
	// AP-схемы не ловился).
	switch n := apNode.(type) {
	case nil:
		// not present
	case *ast.BoolNode:
		s.AdditionalProperties = n.Value
	case *ast.MappingNode:
		sub := &InputSchema{}
		if err := sub.UnmarshalYAML(n); err != nil {
			return err
		}
		s.AdditionalProperties = sub
	default:
		// Любой иной тип — `validateObjectSchema` поднимет type_mismatch
		// по AST; здесь оставляем nil.
	}

	// Разбор `required` по типу узла.
	s.rawRequired = reqNode
	switch n := reqNode.(type) {
	case nil:
		s.requiredKind = requiredAbsent
	case *ast.BoolNode:
		s.Required = n.Value
		s.requiredKind = requiredBool
	case *ast.SequenceNode:
		s.RequiredProps = make([]string, 0, len(n.Values))
		for _, item := range n.Values {
			if sn, ok := item.(*ast.StringNode); ok {
				s.RequiredProps = append(s.RequiredProps, sn.Value)
				continue
			}
			tok := item.GetToken()
			if tok != nil {
				s.RequiredProps = append(s.RequiredProps, tok.Value)
			}
		}
		s.requiredKind = requiredList
	default:
		// Любой другой тип (число, mapping, scalar-строка, null) — оставляем
		// requiredAbsent; `validateInputSchemaNode` поднимет диагностику
		// `input_required_value_invalid` по сохранённой rawRequired-ноде.
		s.requiredKind = requiredAbsent
	}

	return nil
}

// validateInputSchemaMap — публичная точка входа для рекурсивной валидации
// блока input: (или output:). `pathPrefix` — yaml-path до самого блока
// (например, `$.input` или `$.output`). `node` — соответствующий
// MappingNode из AST (nil безопасен, проверки не выполняются).
func validateInputSchemaMap(m InputSchemaMap, node *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	if m == nil && node == nil {
		return nil
	}
	if node == nil {
		// schema-проверки без позиций — лучше ничего, чем падать.
		return nil
	}
	var out []diag.Diagnostic
	for _, kv := range node.Values {
		keyTok := kv.Key.GetToken()
		if keyTok == nil {
			continue
		}
		paramName := keyTok.Value
		paramPath := pathPrefix + "." + paramName
		if !reInputParamName.MatchString(paramName) {
			out = append(out, diagAt(keyTok.Position.Line, keyTok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "input_param_name_invalid",
				Message:  fmt.Sprintf("input parameter name %q does not match %s", paramName, reInputParamName),
				Hint:     "snake_case: starts with lowercase letter; only [a-z0-9_]; no dots/spaces — they break input.<name> in templates",
				YAMLPath: paramPath,
			}))
		}
		paramNode, ok := kv.Value.(*ast.MappingNode)
		if !ok {
			out = append(out, diagAt(keyTok.Position.Line, keyTok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  fmt.Sprintf("input parameter %q must be a mapping", paramName),
				YAMLPath: paramPath,
			}))
			continue
		}
		var schema *InputSchema
		if m != nil {
			schema = m[paramName]
		}
		out = append(out, validateInputSchemaNode(schema, paramNode, paramPath)...)
	}
	return out
}

// validateInputSchemaNode — валидация одной схемы (одного параметра).
// Делает unknown_key + per-key schema-проверки + рекурсию в items/properties.
func validateInputSchemaNode(s *InputSchema, node *ast.MappingNode, path string) []diag.Diagnostic {
	if node == nil {
		return nil
	}
	var out []diag.Diagnostic

	// Сбор присутствующих ключей и их AST-позиций.
	present := map[string]*ast.MappingValueNode{}
	for _, kv := range node.Values {
		keyTok := kv.Key.GetToken()
		if keyTok == nil {
			continue
		}
		name := keyTok.Value
		if !inputSchemaKnownKeys[name] {
			out = append(out, diagAt(keyTok.Position.Line, keyTok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + name + `"`,
				YAMLPath: path + "." + name,
			}))
			continue
		}
		present[name] = kv
	}

	// type — required.
	if _, ok := present["type"]; !ok {
		out = append(out, diagAt(node.GetToken().Position.Line, node.GetToken().Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "input parameter must declare type",
			Hint:     "type: one of string|integer|number|boolean|array|object",
			YAMLPath: path + ".type",
		}))
		// Без type дальнейшие per-type проверки невозможны.
		// Но items/properties всё равно валидируем, чтобы поднять и их ошибки.
		out = append(out, recurseItemsProperties(s, present, path)...)
		return out
	}

	if s == nil {
		// schema нет (decode-fatal раньше — diag уже выпущена).
		return out
	}

	// `required:` с не-bool / не-sequence значением (например `required: "foo"`,
	// `required: 1`, `required: null`). UnmarshalYAML классифицирует такое в
	// requiredAbsent и сохраняет исходную ноду в rawRequired; здесь поднимаем
	// диагностику. nil-ноду (ключа нет) и BoolNode / SequenceNode — пропускаем.
	if s.rawRequired != nil {
		switch s.rawRequired.(type) {
		case *ast.BoolNode, *ast.SequenceNode:
			// валидные формы — уже разобраны
		default:
			if kv, ok := present["required"]; ok {
				tok := kv.Value.GetToken()
				out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
					Code:     "input_required_value_invalid",
					Message:  "required must be bool (parameter-level) or sequence of strings (object property list)",
					Hint:     "use `required: true/false` for any type; `required: [a, b]` only inside type=object",
					YAMLPath: path + ".required",
				}))
			}
		}
	}

	// required_when — статически парсимый CEL над input.* (docs/input.md →
	// «Условная обязательность»). Применим к любому type, поэтому проверяется до
	// per-type-fork. Непарсимое/недопустимое выражение → input_required_when_invalid.
	if kv, ok := present["required_when"]; ok {
		out = append(out, validateRequiredWhen(s, kv, path)...)
	}

	// type — enum.
	if !contains(inputTypeEnum, s.Type) {
		tkv := present["type"]
		tt := tkv.Value.GetToken()
		out = append(out, diagAt(tt.Position.Line, tt.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_type_invalid",
			Message:  fmt.Sprintf("type %q is not in %v", s.Type, inputTypeEnum),
			YAMLPath: path + ".type",
		}))
		// при неизвестном type продолжаем валидировать рекурсию без per-type-fork.
		out = append(out, recurseItemsProperties(s, present, path)...)
		return out
	}

	// per-key проверки «ключ применим к данному type».
	out = append(out, checkKeyTypeApplicability(s, present, path)...)

	// per-type checks.
	switch s.Type {
	case "string":
		out = append(out, validateStringSchema(s, present, path)...)
	case "integer", "number":
		out = append(out, validateNumericSchema(s, present, path)...)
	case "boolean":
		// без специфики; ограничения — только общие.
	case "array":
		out = append(out, validateArraySchema(s, present, path)...)
	case "object":
		out = append(out, validateObjectSchema(s, present, path)...)
	}

	// source — структурная валидность каталога-источника (ADR-044 S-T1).
	// Применимость по type уже проверена checkKeyTypeApplicability; здесь —
	// форма дискриминатора + (для array) items.type=string.
	if _, has := present["source"]; has {
		out = append(out, validateSource(s, present, path)...)
	}

	// общие кросс-инварианты.
	out = append(out, validateCommonInvariants(s, present, path)...)

	// рекурсия в items/properties — после остальных проверок текущего уровня.
	out = append(out, recurseItemsProperties(s, present, path)...)

	return out
}

// checkKeyTypeApplicability — ловит ключи, применённые к неподходящему type.
// Действует только когда type валиден (иначе ложные срабатывания).
func checkKeyTypeApplicability(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	type rule struct {
		key       string
		onlyTypes []string
	}
	rules := []rule{
		{"pattern", []string{"string"}},
		{"format", []string{"string"}},
		{"min_length", []string{"string"}},
		{"max_length", []string{"string"}},
		{"allow_empty", []string{"string"}},
		{"vault_scope", []string{"string"}},

		{"min", []string{"integer", "number"}},
		{"max", []string{"integer", "number"}},
		{"exclusive_min", []string{"integer", "number"}},
		{"exclusive_max", []string{"integer", "number"}},

		{"items", []string{"array"}},
		{"min_items", []string{"array"}},
		{"max_items", []string{"array"}},
		{"unique", []string{"array"}},

		{"properties", []string{"object"}},
		{"additional_properties", []string{"object"}},

		// source применим к single-выбору (string) и multi-выбору (array);
		// для array дополнительно требуется items.type=string —
		// проверяется в validateSource (структурная валидность источника).
		{"source", []string{"string", "array"}},
	}
	var out []diag.Diagnostic
	for _, r := range rules {
		kv, ok := present[r.key]
		if !ok {
			continue
		}
		if !contains(r.onlyTypes, s.Type) {
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "input_key_invalid_for_type",
				Message:  fmt.Sprintf("key %q is not applicable to type %q", r.key, s.Type),
				Hint:     fmt.Sprintf("allowed only for type %v", r.onlyTypes),
				YAMLPath: path + "." + r.key,
			}))
		}
	}
	// `required` как []string — только при type=object.
	if s.requiredKind == requiredList && s.Type != "object" {
		if kv, ok := present["required"]; ok {
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "input_key_invalid_for_type",
				Message:  fmt.Sprintf(`"required" as list is only allowed for type "object", got type %q`, s.Type),
				Hint:     "use `required: true/false` (bool) for parameter-level requirement; list form is for object properties",
				YAMLPath: path + ".required",
			}))
		}
	}
	return out
}

func validateStringSchema(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	if s.Pattern != "" {
		if _, err := regexp.Compile(s.Pattern); err != nil {
			kv := present["pattern"]
			tok := kv.Value.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "input_pattern_invalid",
				Message:  fmt.Sprintf("pattern does not compile as RE2: %v", err),
				YAMLPath: path + ".pattern",
			}))
		}
	}
	if s.Format != "" && !contains(inputFormatEnum, s.Format) {
		kv := present["format"]
		tok := kv.Value.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_format_invalid",
			Message:  fmt.Sprintf("format %q is not in %v", s.Format, inputFormatEnum),
			YAMLPath: path + ".format",
		}))
	}
	if s.Pattern != "" && s.Format != "" {
		kv := present["pattern"]
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "input_pattern_format_conflict",
			Message:  "both `pattern` and `format` declared; format takes precedence",
			Hint:     "use one — `format` for known kinds, `pattern` for custom",
			YAMLPath: path + ".pattern",
		}))
	}
	if s.MinLength != nil && *s.MinLength < 0 {
		out = append(out, diagAtKV(present["min_length"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "value_out_of_range", Message: "min_length must be >= 0",
			YAMLPath: path + ".min_length",
		}))
	}
	if s.MaxLength != nil && *s.MaxLength < 0 {
		out = append(out, diagAtKV(present["max_length"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "value_out_of_range", Message: "max_length must be >= 0",
			YAMLPath: path + ".max_length",
		}))
	}
	if s.MinLength != nil && s.MaxLength != nil && *s.MinLength > *s.MaxLength {
		out = append(out, diagAtKV(present["max_length"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "value_out_of_range",
			Message:  fmt.Sprintf("max_length (%d) must be >= min_length (%d)", *s.MaxLength, *s.MinLength),
			YAMLPath: path + ".max_length",
		}))
	}
	if s.AllowEmpty && s.MinLength != nil && *s.MinLength >= 1 {
		out = append(out, diagAtKV(present["allow_empty"], diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "input_allow_empty_min_length_conflict",
			Message:  fmt.Sprintf("allow_empty: true conflicts with min_length: %d", *s.MinLength),
			Hint:     "drop allow_empty if min_length >= 1; empty string would never pass",
			YAMLPath: path + ".allow_empty",
		}))
	}
	if _, has := present["vault_scope"]; has {
		out = append(out, validateVaultScope(s, present, path)...)
	}
	return out
}

// validateVaultScope — семантика ключа `vault_scope` (docs/input.md →
// «vault_scope»). Применим только к secret-полю: vault_scope открывает
// оператору ограниченное чтение Vault через input-ref, что осмысленно лишь
// для секретов. Сам glob должен быть непустым и нести prefix (`<mount>/...`),
// иначе сужения нет. type=string уже гарантирован checkKeyTypeApplicability.
func validateVaultScope(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	if !s.Secret {
		out = append(out, diagAtKV(present["vault_scope"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_vault_scope_requires_secret",
			Message:  "vault_scope is only allowed on secret: true fields",
			Hint:     "add `secret: true` — vault_scope grants scoped Vault read for the field value",
			YAMLPath: path + ".vault_scope",
		}))
	}
	if !validVaultScopeGlob(s.VaultScope) {
		out = append(out, diagAtKV(present["vault_scope"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_vault_scope_invalid",
			Message:  fmt.Sprintf("vault_scope %q is not a valid prefix-glob", s.VaultScope),
			Hint:     "form: `<mount>/<path-prefix>/*` (one trailing `*`), e.g. `secret/services/redis/*`",
			YAMLPath: path + ".vault_scope",
		}))
	}
	return out
}

// validateSource — структурная валидность каталога-источника `source:`
// (ADR-044 S-T1). Проверяет ТОЛЬКО форму: source — mapping; под-ключи из
// inputSourceKnownKeys; тип значения каждого под-ключа корректен
// (`incarnation_hosts` — bool, `choir` — string). Само множество (резолв SID-ов
// инкарнации / Choir-партии) и проверку «значение ∈ множество» делает backend
// при подготовке формы — здесь только синтаксис.
//
// Дополнительно: для type=array source осмыслен лишь когда items.type=string
// (multi-выбор SID-ов). Применимость source к самому type уже проверена
// checkKeyTypeApplicability (string|array); здесь добавляется array→items.
func validateSource(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	kv := present["source"]
	srcNode, ok := kv.Value.(*ast.MappingNode)
	if !ok {
		out = append(out, diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_source_invalid",
			Message:  "source must be a mapping (object-discriminator)",
			Hint:     "source: { incarnation_hosts: true } or source: { choir: <name> }",
			YAMLPath: path + ".source",
		}))
		return out
	}

	for _, sub := range srcNode.Values {
		keyTok := sub.Key.GetToken()
		if keyTok == nil {
			continue
		}
		name := keyTok.Value
		if !inputSourceKnownKeys[name] {
			out = append(out, diagAt(keyTok.Position.Line, keyTok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + name + `" in source`,
				Hint:     "known source keys: incarnation_hosts (bool), choir (string)",
				YAMLPath: path + ".source." + name,
			}))
			continue
		}
		out = append(out, validateSourceSubKey(name, sub, path)...)
	}

	// Инвариант дискриминатора: РОВНО ОДИН активный источник (см. doc-коммент
	// InputSource). Активным считается incarnation_hosts только при true и choir
	// только при непустой строке: `incarnation_hosts: false` / `choir: ""` /
	// пустой `source: {}` дают 0 активных, два заданных — 2. Любое != 1 — ошибка.
	// (Пустую choir отдельно ловит validateSourceSubKey осмысленным message; здесь
	// мы её просто не считаем активной, чтобы не дублировать диагностику.)
	active := 0
	if s.Source != nil {
		if s.Source.IncarnationHosts {
			active++
		}
		if s.Source.Choir != "" {
			active++
		}
	}
	if active != 1 {
		out = append(out, diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_source_invalid",
			Message:  fmt.Sprintf("source must declare exactly one active catalog, got %d", active),
			Hint:     "set exactly one: incarnation_hosts: true OR choir: <name>",
			YAMLPath: path + ".source",
		}))
	}

	// Для array source требует items.type=string (multi-выбор SID-ов).
	if s.Type == "array" && (s.Items == nil || s.Items.Type != "string") {
		out = append(out, diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_source_invalid_for_type",
			Message:  "source on type=array requires items.type=string",
			Hint:     "multi-выбор из каталога — массив строк (SID); set items: { type: string }",
			YAMLPath: path + ".source",
		}))
	}

	return out
}

// validateSourceSubKey — проверка типа значения одного известного под-ключа
// source. incarnation_hosts — bool, choir — непустая строка.
func validateSourceSubKey(name string, sub *ast.MappingValueNode, path string) []diag.Diagnostic {
	subPath := path + ".source." + name
	switch name {
	case "incarnation_hosts":
		if _, ok := sub.Value.(*ast.BoolNode); !ok {
			tok := sub.Value.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  "source.incarnation_hosts must be bool",
				YAMLPath: subPath,
			})}
		}
	case "choir":
		sn, ok := sub.Value.(*ast.StringNode)
		if !ok {
			tok := sub.Value.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  "source.choir must be a string (Choir name)",
				YAMLPath: subPath,
			})}
		}
		if sn.Value == "" {
			tok := sub.Value.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:     "input_source_invalid",
				Message:  "source.choir must not be empty",
				YAMLPath: subPath,
			})}
		}
	}
	return nil
}

func validateNumericSchema(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	if s.Min != nil && s.ExclusiveMin != nil {
		out = append(out, diagAtKV(present["exclusive_min"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_min_conflict",
			Message:  "min and exclusive_min are mutually exclusive",
			YAMLPath: path + ".exclusive_min",
		}))
	}
	if s.Max != nil && s.ExclusiveMax != nil {
		out = append(out, diagAtKV(present["exclusive_max"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_max_conflict",
			Message:  "max and exclusive_max are mutually exclusive",
			YAMLPath: path + ".exclusive_max",
		}))
	}
	// min <= max если оба заданы (включительные границы).
	if s.Min != nil && s.Max != nil && *s.Min > *s.Max {
		out = append(out, diagAtKV(present["max"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "value_out_of_range",
			Message:  fmt.Sprintf("max (%v) must be >= min (%v)", *s.Max, *s.Min),
			YAMLPath: path + ".max",
		}))
	}
	return out
}

func validateArraySchema(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	// items — обязателен для array.
	if _, ok := present["items"]; !ok {
		out = append(out, diagAtKV(present["type"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "array parameter must declare items",
			Hint:     "items: <schema> — defines element shape, recursively",
			YAMLPath: path + ".items",
		}))
	}
	if s.MinItems != nil && *s.MinItems < 0 {
		out = append(out, diagAtKV(present["min_items"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "value_out_of_range", Message: "min_items must be >= 0",
			YAMLPath: path + ".min_items",
		}))
	}
	if s.MaxItems != nil && *s.MaxItems < 0 {
		out = append(out, diagAtKV(present["max_items"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "value_out_of_range", Message: "max_items must be >= 0",
			YAMLPath: path + ".max_items",
		}))
	}
	if s.MinItems != nil && s.MaxItems != nil && *s.MinItems > *s.MaxItems {
		out = append(out, diagAtKV(present["max_items"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "value_out_of_range",
			Message:  fmt.Sprintf("max_items (%d) must be >= min_items (%d)", *s.MaxItems, *s.MinItems),
			YAMLPath: path + ".max_items",
		}))
	}
	return out
}

func validateObjectSchema(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	if _, ok := present["properties"]; !ok {
		out = append(out, diagAtKV(present["type"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "object parameter must declare properties",
			Hint:     "properties: { <name>: <schema>, ... }",
			YAMLPath: path + ".properties",
		}))
	}
	// additional_properties — bool или mapping (схема). Никаких других типов.
	if kv, ok := present["additional_properties"]; ok {
		v := kv.Value
		_, isMap := v.(*ast.MappingNode)
		_, isBool := v.(*ast.BoolNode)
		if !isMap && !isBool {
			tok := v.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  "additional_properties must be bool or schema (mapping)",
				YAMLPath: path + ".additional_properties",
			}))
		}
	}
	// `required: [name1, name2]` — каждое имя должно быть среди properties.
	if s.requiredKind == requiredList && s.Properties != nil {
		for _, name := range s.RequiredProps {
			if _, ok := s.Properties[name]; !ok {
				out = append(out, diagAtKV(present["required"], diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "missing_required_field",
					Message:  fmt.Sprintf("required references unknown property %q", name),
					Hint:     "every entry of `required` must match a key in `properties`",
					YAMLPath: path + ".required",
				}))
			}
		}
	}
	return out
}

// validateCommonInvariants — кросс-проверки на общих ключах (enum/default/required).
func validateCommonInvariants(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic

	// enum для array/object — запрещено в MVP. Литералы массивов и объектов
	// не comparable в Go runtime, а reflect.DeepEqual для редкого
	// «enum-литералов-композитов» — over-engineering до появления реального
	// запроса. См. ADR-комментарий в input.md (post-MVP). enum-проверка ниже
	// и `default in enum` ниже для этих типов пропускаются.
	enumOnComposite := len(s.Enum) > 0 && (s.Type == "array" || s.Type == "object")
	if enumOnComposite {
		out = append(out, diagAtKV(present["enum"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_enum_unsupported_for_type",
			Message:  fmt.Sprintf("enum is unsupported for type=%s in MVP", s.Type),
			Hint:     "enum literals of arrays/objects are post-MVP; use a different type or drop enum",
			YAMLPath: path + ".enum",
		}))
	}

	// enum — каждый элемент соответствует type. Для array/object пропускаем,
	// диагностика уже выпущена выше.
	if len(s.Enum) > 0 && !enumOnComposite {
		for i, v := range s.Enum {
			if !valueMatchesType(v, s.Type) {
				out = append(out, diagAtKV(present["enum"], diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "input_enum_type_mismatch",
					Message:  fmt.Sprintf("enum[%d] = %s does not match type %q", i, formatLiteral(v), s.Type),
					YAMLPath: path + ".enum",
				}))
			}
		}
	}

	// default — соответствует type. Особый случай: string-default может быть
	// CEL/template-выражением (`${ ... }` или `{{ ... }}`); такие пропускаем —
	// для type=string синтаксис как строка валиден всегда.
	if _, has := present["default"]; has {
		if !defaultMatchesType(s.Default, s.Type) {
			out = append(out, diagAtKV(present["default"], diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "input_default_type_mismatch",
				Message:  fmt.Sprintf("default = %s does not match type %q", formatLiteral(s.Default), s.Type),
				YAMLPath: path + ".default",
			}))
		} else {
			// Top-level type ok — спускаемся в содержимое для array/object,
			// чтобы не пропустить mismatching элементы/поля внутри литерала.
			out = append(out, validateDefaultContent(s, present["default"], path+".default")...)
		}

		// default должен быть в enum, если оба заданы. Для array/object enum
		// запрещён (см. выше) — пропускаем, иначе пришлось бы сравнивать
		// composite-литералы. Без этого проверка скрытого расхождения
		// «выбор только из enum, но default — мимо» остаётся для scalar-типов.
		if len(s.Enum) > 0 && !enumOnComposite {
			if !enumContains(s.Enum, s.Default) {
				out = append(out, diagAtKV(present["default"], diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "input_default_not_in_enum",
					Message:  fmt.Sprintf("default = %s is not in enum %s", formatLiteral(s.Default), formatEnum(s.Enum)),
					Hint:     "default must be one of enum values (or drop enum)",
					YAMLPath: path + ".default",
				}))
			}
		}
	}

	// required: true + default → конфликт (warning).
	if s.requiredKind == requiredBool && s.Required {
		if _, has := present["default"]; has {
			out = append(out, diagAtKV(present["default"], diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:     "input_required_default_conflict",
				Message:  "required: true together with default is contradictory",
				Hint:     "drop one — `default` already implies optional",
				YAMLPath: path + ".default",
			}))
		}
	}

	return out
}

// recurseItemsProperties — рекурсия в items (array), properties (object) и
// additional_properties (object, когда задано как schema, а не bool).
// Делается отдельно от per-type, чтобы спускаться и в schema без type
// (там мы уже выдали missing_required_field, но вложенные ошибки тоже хотим
// показать пользователю в одном проходе).
func recurseItemsProperties(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	if kv, ok := present["items"]; ok {
		if itemNode, isMap := kv.Value.(*ast.MappingNode); isMap {
			var sub *InputSchema
			if s != nil {
				sub = s.Items
			}
			out = append(out, validateInputSchemaNode(sub, itemNode, path+".items")...)
		}
	}
	if kv, ok := present["properties"]; ok {
		if propsNode, isMap := kv.Value.(*ast.MappingNode); isMap {
			var sub InputSchemaMap
			if s != nil {
				sub = s.Properties
			}
			out = append(out, validateInputSchemaMap(sub, propsNode, path+".properties")...)
		}
	}
	if kv, ok := present["additional_properties"]; ok {
		// additional_properties: <schema> — это форма «map произвольных
		// ключей с общей schema-значением» (см. examples/destiny/redis
		// → users). Голый bool не валидируем (там нет вложенной схемы).
		if apNode, isMap := kv.Value.(*ast.MappingNode); isMap {
			var sub *InputSchema
			if s != nil {
				if ap, ok := s.AdditionalProperties.(*InputSchema); ok {
					sub = ap
				}
			}
			out = append(out, validateInputSchemaNode(sub, apNode, path+".additional_properties")...)
		}
	}
	return out
}

// valueMatchesType — true, если literal Go-значение соответствует input-type.
// Используется для enum и default. `array` / `object` намеренно прозрачны:
// для enum-литералов и default-литералов «соответствие type» в форме
// «не противоречит» — нативный YAML-decode уже типизировал scalar.
func valueMatchesType(v any, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		switch x := v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			// goccy YAML отдаёт `42` как uint64; `42.0` — как float64.
			// Если literal — целое в float-обёртке, всё ещё ок.
			return x == float64(int64(x))
		}
		return false
	case "number":
		switch v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
			float32, float64:
			return true
		}
		return false
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}

// defaultMatchesType — обёртка над valueMatchesType с послаблением для
// type=string: literal с `${ ... }` или `{{ ... }}` — допустимый выражение-
// default, любая string-форма валидна.
func defaultMatchesType(v any, t string) bool {
	if t == "string" {
		_, ok := v.(string)
		return ok
	}
	return valueMatchesType(v, t)
}

func formatLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(x)
	default:
		return fmt.Sprintf("%v (%T)", x, x)
	}
}

// formatEnum — рендер enum-литерала для диагностики (стабильный порядок,
// тот же что в YAML — не сортируем).
func formatEnum(es []any) string {
	out := make([]string, 0, len(es))
	for _, v := range es {
		out = append(out, formatLiteral(v))
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// enumContains — сравнение по семантике YAML-scalar-ов: для чисел учитываем
// смешанные int/float ровно как в valueMatchesType (`42` ≡ `42.0`).
func enumContains(enum []any, v any) bool {
	for _, e := range enum {
		if equalScalar(e, v) {
			return true
		}
	}
	return false
}

// equalScalar — равенство по значению с послаблением int↔float, чтобы
// `default: 1` и `enum: [1, 2]` (где goccy типизирует элементы как uint64)
// совпадали без ложного срабатывания.
//
// Defensive: slice/map не comparable в runtime — `a == b` для них паникует
// (видели на enum-литералах массивов до того, как ввели
// `input_enum_unsupported_for_type`). Эти типы не должны сюда долетать,
// validateCommonInvariants отрезает их раньше; здесь — страховка на случай
// будущего расширения enum для composites.
func equalScalar(a, b any) bool {
	if !isComparableScalar(a) || !isComparableScalar(b) {
		return false
	}
	if a == b {
		return true
	}
	af, aok := toFloat64(a)
	bf, bok := toFloat64(b)
	if aok && bok {
		return af == bf
	}
	return false
}

// isComparableScalar — true для значений, которые безопасно сравнивать через
// оператор `==` (scalar Go-типы). Slice/map не comparable в runtime → false.
func isComparableScalar(v any) bool {
	switch v.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	}
	return false
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// validateDefaultContent — рекурсивная проверка содержимого default-литерала
// против вложенной схемы. Применяется для array (каждый элемент против
// items.type) и object (каждое поле против properties.<name>.type) на
// произвольную глубину вложенности (qa.1 явно требовала рекурсии: 1-уровневая
// проверка пропускала mismatches в array[object[array]]).
//
// `kv` — `default:` MappingValueNode (для line/col диагностик; внутрь не
// спускаемся по AST — default — литерал, AST-позиций под-элементов нет).
//
// CEL/template-обёртки `${...}` / `{{...}}` оставляем нетронутыми внутри
// (defaultMatchesType уже пропустил их на верхнем уровне; в элементах
// массива/полях объекта это сейчас не встречается на практике, чтобы городить
// разбор обёрток).
func validateDefaultContent(s *InputSchema, kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	if s == nil || s.Default == nil {
		return nil
	}
	return validateDefaultValue(s, s.Default, kv, path)
}

// validateDefaultValue — проверяет одно значение `v` против схемы `sub` и
// рекурсивно спускается в array/object элементы. Отдельная функция (а не
// inline в validateDefaultContent), чтобы рекурсия не тащила за собой
// `s.Default` верхнего уровня — каждый зов получает своё под-значение.
//
// На корневом вызове top-level type-match уже проверен `defaultMatchesType`
// в validateCommonInvariants — поэтому здесь, на корневом уровне, type-mismatch
// не дублируется. Сравнение типа делается только при спуске в дочерние
// элементы (см. ниже в array/object-ветках).
func validateDefaultValue(sub *InputSchema, v any, kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	if sub == nil || sub.Type == "" {
		return nil
	}
	var out []diag.Diagnostic
	switch sub.Type {
	case "array":
		arr, ok := v.([]any)
		if !ok || sub.Items == nil || sub.Items.Type == "" {
			return nil
		}
		for i, el := range arr {
			elPath := fmt.Sprintf("%s[%d]", path, i)
			if !valueMatchesType(el, sub.Items.Type) {
				out = append(out, diagAtKV(kv, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "input_default_type_mismatch",
					Message:  fmt.Sprintf("default%s = %s does not match items.type %q", strings.TrimPrefix(elPath, path), formatLiteral(el), sub.Items.Type),
					YAMLPath: elPath,
				}))
				continue
			}
			out = append(out, validateDefaultValue(sub.Items, el, kv, elPath)...)
		}
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		for k, fv := range obj {
			prop, ok := sub.Properties[k]
			if !ok || prop == nil || prop.Type == "" {
				continue
			}
			fieldPath := path + "." + k
			if !valueMatchesType(fv, prop.Type) {
				out = append(out, diagAtKV(kv, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "input_default_type_mismatch",
					Message:  fmt.Sprintf("default.%s = %s does not match properties.%s.type %q", k, formatLiteral(fv), k, prop.Type),
					YAMLPath: fieldPath,
				}))
				continue
			}
			out = append(out, validateDefaultValue(prop, fv, kv, fieldPath)...)
		}
	}
	return out
}

// diagAt — обёртка для удобства: проставляет line/col, остальное оставляет.
func diagAt(line, col int, d diag.Diagnostic) diag.Diagnostic {
	d.Line = line
	d.Column = col
	return d
}

func diagAtKV(kv *ast.MappingValueNode, d diag.Diagnostic) diag.Diagnostic {
	if kv == nil {
		return d
	}
	tok := kv.Key.GetToken()
	if tok == nil {
		return d
	}
	d.Line = tok.Position.Line
	d.Column = tok.Position.Column
	return d
}

// findInputMapping ищет MappingNode под top-level ключом блока (`input` /
// `output` / etc.) и возвращает его. Если ключа нет — nil.
func findInputMapping(root *ast.MappingNode, topKey string) *ast.MappingNode {
	if root == nil {
		return nil
	}
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value != topKey {
			continue
		}
		if m, ok := kv.Value.(*ast.MappingNode); ok {
			return m
		}
		return nil
	}
	return nil
}
