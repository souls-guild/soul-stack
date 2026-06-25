package config

// Презентационный слой input-формы — опциональный top-level ключ `form:` в
// scenario-манифесте (`scenario/<name>/main.yml`). Чистая ПРЕЗЕНТАЦИЯ: как UI
// группирует и подписывает поля `input:` в day-2/create-форме. Контракт ввода
// (типы, валидация, обязательность) живёт ИСКЛЮЧИТЕЛЬНО в `input:` — `form:` его
// не дублирует и не меняет, ссылается на поля по имени.
//
// Граница ответственности:
//   - `input:` — что за поля, какого типа, обязательны ли (API/валидация);
//   - `form:` — в каких секциях и под какими подписями их рисует UI.
//
// FORWARD-COMPAT: ключ опционален. Нет `form:` → Form==nil, listing-проекция без
// поля, UI рисует input плоско (как до фичи). Новое поле в `input:`, не попавшее
// ни в одну секцию, НЕ ломает форму — UI дорисует его в дефолтную секцию в конце
// (поэтому "поле input без секции" — WARNING, не ERROR; см. validateFormLayout).

import (
	"fmt"
	"regexp"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// FormLayout — содержимое top-level `form:`: упорядоченный список секций. Порядок
// объявления = порядок отрисовки секций в UI.
type FormLayout struct {
	Sections []FormSection `yaml:"sections,omitempty"`
}

// FormSection — одна визуальная группа полей формы.
//
// Key — стабильный машинный id секции (для запоминания collapsed-state в UI между
// прогонами); обязан быть уникален в пределах формы. Title — заголовок группы.
// Description — опц. пояснение под заголовком. Collapsed — стартовое состояние
// «свёрнута» (default false). Fields — поля input, отрисовываемые в этой секции,
// в порядке объявления.
type FormSection struct {
	Key         string      `yaml:"key"`
	Title       string      `yaml:"title,omitempty"`
	Description string      `yaml:"description,omitempty"`
	Collapsed   bool        `yaml:"collapsed,omitempty"`
	Fields      []FormField `yaml:"fields,omitempty"`
}

// FormField — ссылка на одно поле `input:` с опц. человекочитаемой подписью.
//
// Name — имя ключа из `input:` (обязан существовать там; cross-инвариант
// form_field_unknown). Label — подпись в UI; опционален: пустой → UI берёт
// fallback (description поля input или само имя).
type FormField struct {
	Name  string `yaml:"name"`
	Label string `yaml:"label,omitempty"`
}

// reFormSectionKey — стабильный машинный id секции: kebab/snake-идентификатор
// (буква/цифра/дефис/подчёркивание, старт с буквы). Совпадает по форме с именами
// covens/секций — пригоден как persistent UI-key без экранирования.
var reFormSectionKey = regexp.MustCompile(`^[a-z][a-z0-9]*([_-][a-z0-9]+)*$`)

// validateFormLayout — структурная + cross-инвариантная проверка `form:`-блока.
// Активна ТОЛЬКО при наличии ключа (caller вызывает по topKeys["form"]). Все
// инварианты — ERROR, КРОМЕ form_field_uncovered и пустого label (WARNING):
//
//   - блок — mapping с единственным значимым ключом `sections:` (sequence);
//   - section.key — обязателен, формат reFormSectionKey, УНИКАЛЕН (ERROR при дубле);
//   - field.name — обязателен, существует ключом в `input:` (ERROR form_field_unknown);
//   - имя поля не встречается в >1 секции суммарно (ERROR form_field_duplicate);
//   - field.label — пустая строка → WARNING (fallback на description/имя);
//   - поле `input:`, не попавшее ни в одну секцию → WARNING form_field_uncovered.
//
// inputKeys — множество имён из `input:` (nil-безопасно: nil → form_field_unknown
// на каждом поле, uncovered не эмитится — нечего покрывать).
func validateFormLayout(root *ast.MappingNode, m *ScenarioManifest, pathPrefix string) []diag.Diagnostic {
	node := findValueNode(root, "form")
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		line, col := 0, 0
		if vt := node.GetToken(); vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "form must be a mapping with a sections: list",
			Hint:     "form: { sections: [ { key: ..., title: ..., fields: [...] } ] }",
			YAMLPath: pathPrefix,
		})}
	}

	sectionsNode, out := formSectionsNode(mm, pathPrefix)
	if sectionsNode == nil {
		return out
	}

	inputKeys := make(map[string]bool, len(m.Input))
	for name := range m.Input {
		inputKeys[name] = true
	}

	seenKeys := make(map[string]bool, len(sectionsNode.Values))
	// fieldToSection — имя поля → индекс первой секции, где оно объявлено
	// (для form_field_duplicate). Покрытые поля параллельно собираем в covered.
	covered := make(map[string]bool, len(inputKeys))
	fieldOwner := make(map[string]int, len(inputKeys))

	for si, item := range sectionsNode.Values {
		secPath := fmt.Sprintf("%s.sections[%d]", pathPrefix, si)
		out = append(out, validateFormSection(item, secPath, seenKeys, inputKeys, covered, fieldOwner, si)...)
	}

	// form_field_uncovered — поля input без секции (WARNING, не ERROR): UI дорисует
	// их в дефолтную секцию в конце; новое input-поле не должно ломать form.
	// Якорь — на ключ `form` (поле объявлено в input, не во form — точной позиции
	// внутри form нет). Эмитим только когда input известен (inputKeys непуст).
	if len(inputKeys) > 0 {
		formTok := findValueNode(root, "form").GetToken()
		line, col := 0, 0
		if formTok != nil {
			line, col = formTok.Position.Line, formTok.Position.Column
		}
		for name := range inputKeys {
			if covered[name] {
				continue
			}
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:     "form_field_uncovered",
				Message:  fmt.Sprintf("input.%s is not placed in any form section", name),
				Hint:     "add it to a form section, or leave it — the UI appends uncovered fields to a default section",
				YAMLPath: pathPrefix + ".sections",
			}))
		}
	}

	return out
}

// formSectionsNode извлекает sequence-узел `sections:` из mapping-а `form:`,
// отбраковывая прочие ключи (unknown_key) и неверную форму. Возвращает (nil, diags)
// при отсутствии/неверной форме `sections:` (caller прекращает обход).
func formSectionsNode(mm *ast.MappingNode, pathPrefix string) (*ast.SequenceNode, []diag.Diagnostic) {
	var out []diag.Diagnostic
	var sections *ast.MappingValueNode
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		if tok.Value == "sections" {
			sections = kv
			continue
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "unknown_key",
			Message:  `unknown field "` + tok.Value + `" in form`,
			Hint:     "form accepts only sections:",
			YAMLPath: pathPrefix + "." + tok.Value,
		}))
	}
	if sections == nil {
		out = append(out, diagAt(lineOf(mm), colOf(mm), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "form requires sections: [ ... ]",
			YAMLPath: pathPrefix + ".sections",
		}))
		return nil, out
	}
	seq, ok := sections.Value.(*ast.SequenceNode)
	if !ok {
		out = append(out, diagAtKV(sections, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "form.sections must be a list of sections",
			YAMLPath: pathPrefix + ".sections",
		}))
		return nil, out
	}
	if len(seq.Values) == 0 {
		out = append(out, diagAtKV(sections, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "empty_value",
			Message:  "form.sections must contain at least one section (drop form: for no layout)",
			YAMLPath: pathPrefix + ".sections",
		}))
		return nil, out
	}
	return seq, out
}

// validateFormSection — валидация одной секции + накопление cross-state (seenKeys
// для уникальности section.key; covered/fieldOwner для покрытия/дублей полей).
func validateFormSection(
	node ast.Node, path string,
	seenKeys map[string]bool, inputKeys, covered map[string]bool, fieldOwner map[string]int, secIdx int,
) []diag.Diagnostic {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return []diag.Diagnostic{diagAt(lineOf(node), colOf(node), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "form section must be a mapping { key: ..., fields: [...] }",
			YAMLPath: path,
		})}
	}

	var out []diag.Diagnostic
	var keyKV, fieldsKV *ast.MappingValueNode
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "key":
			keyKV = kv
		case "fields":
			fieldsKV = kv
		case "title", "description", "collapsed":
			// презентационные скаляры — форму проверяет reflect-walker по тегам;
			// здесь только cross-инварианты.
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in form section`,
				Hint:     "section accepts key / title / description / collapsed / fields",
				YAMLPath: path + "." + tok.Value,
			}))
		}
	}

	out = append(out, validateFormSectionKey(keyKV, path, seenKeys)...)
	out = append(out, validateFormFields(fieldsKV, path, inputKeys, covered, fieldOwner, secIdx)...)
	return out
}

// validateFormSectionKey — key обязателен, формата reFormSectionKey, уникален.
func validateFormSectionKey(keyKV *ast.MappingValueNode, path string, seenKeys map[string]bool) []diag.Diagnostic {
	if keyKV == nil {
		return []diag.Diagnostic{diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "form section requires key: <stable id for UI collapsed-state>",
			YAMLPath: path + ".key",
		}}
	}
	sn, isStr := keyKV.Value.(*ast.StringNode)
	if !isStr || sn.Value == "" {
		return []diag.Diagnostic{diagAtKV(keyKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "empty_value",
			Message:  "form section.key must be a non-empty string",
			YAMLPath: path + ".key",
		})}
	}
	var out []diag.Diagnostic
	if !reFormSectionKey.MatchString(sn.Value) {
		out = append(out, diagAtKV(keyKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "name_invalid_format",
			Message:  fmt.Sprintf("form section.key %q does not match %s", sn.Value, reFormSectionKey),
			Hint:     "kebab/snake id: lowercase letters, digits, dashes/underscores; start with a letter",
			YAMLPath: path + ".key",
		}))
	}
	if seenKeys[sn.Value] {
		out = append(out, diagAtKV(keyKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "duplicate_key",
			Message:  fmt.Sprintf("form section.key %q is declared more than once", sn.Value),
			YAMLPath: path + ".key",
		}))
	}
	seenKeys[sn.Value] = true
	return out
}

// validateFormFields — fields[] секции: каждое field.name ∈ input (иначе
// form_field_unknown), не в >1 секции (form_field_duplicate), label непуст (иначе
// WARNING). Накопление covered/fieldOwner — для cross-секционных инвариантов.
func validateFormFields(
	fieldsKV *ast.MappingValueNode, path string,
	inputKeys, covered map[string]bool, fieldOwner map[string]int, secIdx int,
) []diag.Diagnostic {
	if fieldsKV == nil {
		return []diag.Diagnostic{diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "form section requires fields: [ { name: <input-key> } ]",
			YAMLPath: path + ".fields",
		}}
	}
	seq, ok := fieldsKV.Value.(*ast.SequenceNode)
	if !ok {
		return []diag.Diagnostic{diagAtKV(fieldsKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "form section.fields must be a list of { name, label }",
			YAMLPath: path + ".fields",
		})}
	}

	var out []diag.Diagnostic
	for fi, item := range seq.Values {
		fieldPath := fmt.Sprintf("%s.fields[%d]", path, fi)
		out = append(out, validateFormField(item, fieldPath, inputKeys, covered, fieldOwner, secIdx)...)
	}
	return out
}

// validateFormField — одно поле fields[i].
func validateFormField(
	node ast.Node, path string,
	inputKeys, covered map[string]bool, fieldOwner map[string]int, secIdx int,
) []diag.Diagnostic {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return []diag.Diagnostic{diagAt(lineOf(node), colOf(node), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "form field must be a mapping { name: <input-key>, label?: <str> }",
			YAMLPath: path,
		})}
	}

	var out []diag.Diagnostic
	var nameKV, labelKV *ast.MappingValueNode
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "name":
			nameKV = kv
		case "label":
			labelKV = kv
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in form field`,
				Hint:     "form field accepts only name / label",
				YAMLPath: path + "." + tok.Value,
			}))
		}
	}

	name := ""
	if nameKV != nil {
		if sn, isStr := nameKV.Value.(*ast.StringNode); isStr {
			name = sn.Value
		}
	}
	if name == "" {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "form field requires name: <input-key>",
			YAMLPath: path + ".name", Line: lineOf(mm), Column: colOf(mm),
		})
	} else {
		out = append(out, checkFormFieldName(name, nameKV, path, inputKeys, covered, fieldOwner, secIdx)...)
	}

	// label опционален; пустая строка (label: "") — WARNING (fallback на
	// description/имя), не ошибка.
	if labelKV != nil {
		if sn, isStr := labelKV.Value.(*ast.StringNode); isStr && sn.Value == "" {
			out = append(out, diagAtKV(labelKV, diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:     "form_field_empty_label",
				Message:  fmt.Sprintf("form field %q has an empty label", name),
				Hint:     "drop label to fall back to the input description/name, or set a non-empty label",
				YAMLPath: path + ".label",
			}))
		}
	}
	return out
}

// checkFormFieldName — name ∈ input (form_field_unknown), не дубль между секциями
// (form_field_duplicate); помечает поле covered.
func checkFormFieldName(
	name string, nameKV *ast.MappingValueNode, path string,
	inputKeys, covered map[string]bool, fieldOwner map[string]int, secIdx int,
) []diag.Diagnostic {
	var out []diag.Diagnostic
	if len(inputKeys) > 0 && !inputKeys[name] {
		out = append(out, diagAtKV(nameKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "form_field_unknown",
			Message:  fmt.Sprintf("form field name %q is not a key in input:", name),
			Hint:     "form fields reference input keys by name; declare the field in input: or fix the name",
			YAMLPath: path + ".name",
		}))
	}
	if _, dup := fieldOwner[name]; dup {
		out = append(out, diagAtKV(nameKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "form_field_duplicate",
			Message:  fmt.Sprintf("form field %q appears in more than one section", name),
			Hint:     "each input field belongs to exactly one form section",
			YAMLPath: path + ".name",
		}))
	} else {
		fieldOwner[name] = secIdx
	}
	covered[name] = true
	return out
}
