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
//
// ShowWhen — опц. CEL-предикат над `input.*`: секция видима в UI, КОГДА он истинен
// (нет ключа → видима всегда, бит-в-бит forward-compat). КАВЕТ: это ПРЕЗЕНТАЦИЯ,
// НЕ валидационный гейт. Вычисляется client-side в UI (вариант A): backend отдаёт
// строку как есть, сам предикат не вычисляет. Скрытие секции не отменяет валидацию
// и резолв её полей backend-ом — если значение прислано, оно проверяется как
// обычно. Пара с required_when: show_when прячет, required_when требует.
type FormSection struct {
	Key         string      `yaml:"key"`
	Title       string      `yaml:"title,omitempty"`
	Description string      `yaml:"description,omitempty"`
	Collapsed   bool        `yaml:"collapsed,omitempty"`
	ShowWhen    string      `yaml:"show_when,omitempty"`
	Fields      []FormField `yaml:"fields,omitempty"`
}

// FormField — ссылка на одно поле `input:` с опц. человекочитаемой подписью.
//
// Name — имя ключа из `input:` (обязан существовать там; cross-инвариант
// form_field_unknown). Label — подпись в UI; опционален: пустой → UI берёт
// fallback (description поля input или само имя).
//
// ShowWhen — опц. CEL-предикат над `input.*` для условной видимости поля
// (семантика и кавет — как у FormSection.ShowWhen: презентация, client-side eval,
// НЕ гейт валидации). Placeholder / Hint — чистая презентация UI-виджета (текст в
// пустом поле / подсказка под полем); НЕ дублируют `input:`-контракт (description
// поля остаётся в `input:`, типы/обязательность тоже). Все три опциональны;
// отсутствие любого → бит-в-бит как до фичи.
type FormField struct {
	Name        string `yaml:"name"`
	Label       string `yaml:"label,omitempty"`
	ShowWhen    string `yaml:"show_when,omitempty"`
	Placeholder string `yaml:"placeholder,omitempty"`
	Hint        string `yaml:"hint,omitempty"`
}

// reFormSectionKey — стабильный машинный id секции: kebab/snake-идентификатор
// (буква/цифра/дефис/подчёркивание, старт с буквы). Совпадает по форме с именами
// covens/секций — пригоден как persistent UI-key без экранирования.
var reFormSectionKey = regexp.MustCompile(`^[a-z][a-z0-9]*([_-][a-z0-9]+)*$`)

// validateFormLayout — структурная + cross-инвариантная проверка `form:`-блока в
// SEMANTIC-фазе (non-extends сценарий): inputKeys берутся из уже декодированного
// `m.Input`. Тонкая обёртка над [validateFormAgainstInputKeys] — единственная
// разница с пост-merge-путём — источник множества input-ключей.
//
// Caller вызывает по topKeys["form"] И только когда extends НЕ задан: у covenant-
// сценария эффективный input существует лишь ПОСЛЕ merge фрагмента (keeper-side,
// нужен ФС), поэтому form там проверяется пост-merge тем же ядром на смерженном
// `m.Input` (см. ResolveScenarioCovenant). Гейт — scenario.go schemaValidateScenario.
func validateFormLayout(root *ast.MappingNode, m *ScenarioManifest, pathPrefix string) []diag.Diagnostic {
	inputKeys := make(map[string]bool, len(m.Input))
	for name := range m.Input {
		inputKeys[name] = true
	}
	return validateFormAgainstInputKeys(root, inputKeys, pathPrefix)
}

// validateFormAgainstInputKeys — ЯДРО form-проверки с ПАРАМЕТРИЗОВАННЫМ источником
// inputKeys: множество имён эффективного `input:` передаётся снаружи (из AST/типи-
// зированного `m.Input` non-extends-сценария ИЛИ из СМЕРЖЕННОГО `m.Input` covenant-
// сценария пост-merge). Все инварианты — ERROR, КРОМЕ form_field_uncovered и пустого
// label/placeholder/hint (WARNING):
//
//   - блок — mapping с единственным значимым ключом `sections:` (sequence);
//   - section.key — обязателен, формат reFormSectionKey, УНИКАЛЕН (ERROR при дубле);
//   - field.name — обязателен, существует ключом в input (ERROR form_field_unknown);
//   - имя поля не встречается в >1 секции суммарно (ERROR form_field_duplicate);
//   - field.label/placeholder/hint — пустая строка → WARNING (fallback / drop ключ);
//   - section/field.show_when — если есть, компилируемый CEL над input.* (иначе
//     ERROR form_show_when_invalid; input-only sandbox, как required_when);
//   - поле input, не попавшее ни в одну секцию → WARNING form_field_uncovered.
//
// inputKeys — множество имён эффективного input (nil-безопасно: nil → form_field_
// unknown на каждом поле, uncovered не эмитится — нечего покрывать). root — AST
// корня манифеста/документа (узел `form:` находится по нему; позиции/якоря — из
// него же, чтобы диагностики указывали на реальные строки исходника).
func validateFormAgainstInputKeys(root *ast.MappingNode, inputKeys map[string]bool, pathPrefix string) []diag.Diagnostic {
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
	var keyKV, fieldsKV, showWhenKV *ast.MappingValueNode
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
		case "show_when":
			showWhenKV = kv
		case "title", "description", "collapsed":
			// презентационные скаляры — форму проверяет reflect-walker по тегам;
			// здесь только cross-инварианты.
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in form section`,
				Hint:     "section accepts key / title / description / collapsed / show_when / fields",
				YAMLPath: path + "." + tok.Value,
			}))
		}
	}

	out = append(out, validateFormSectionKey(keyKV, path, seenKeys)...)
	out = append(out, validateFormShowWhen(showWhenKV, path)...)
	out = append(out, validateFormFields(fieldsKV, path, inputKeys, covered, fieldOwner, secIdx)...)
	return out
}

// validateFormShowWhen — schema-time проверка опц. ключа `show_when` (секции или
// поля): если присутствует — это непустая, компилируемая через [compileRequiredWhen]
// CEL-строка над `input.*` (тот же input-only sandbox, что у required_when).
// Обращение к essence/soulprint/register/vault/now → undeclared-reference compile-
// ошибка → form_show_when_invalid (зеркало input_required_when_invalid).
//
// КАВЕТ (вариант A, client-side eval): show_when — ПРЕЗЕНТАЦИЯ, не валидационный
// гейт. Линтер проверяет лишь компилируемость; сам предикат вычисляет UI. Скрытое
// поле всё равно валидируется/резолвится backend-ом, если значение прислано.
//
// kv == nil (ключа нет) → видимо всегда, ноль диагностик (forward-compat).
func validateFormShowWhen(kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	if kv == nil {
		return nil
	}
	sn, isStr := kv.Value.(*ast.StringNode)
	if !isStr || sn.Value == "" {
		// Пустая строка / не-строка — бессмысленный предикат: тихо «никогда не
		// видимо» — footgun автора. Отвергаем явно (симметрия с required_when).
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "form_show_when_invalid",
			Message:  "show_when must be a non-empty CEL predicate over input.*",
			Hint:     `e.g. show_when: "input.tls_enabled"`,
			YAMLPath: path + ".show_when",
		})}
	}
	if _, err := compileRequiredWhen(sn.Value); err != nil {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "form_show_when_invalid",
			Message:  fmt.Sprintf("show_when does not compile as CEL over input.*: %v", err),
			Hint:     "predicate may reference only input.* (no essence/soulprint/register/vault/now)",
			YAMLPath: path + ".show_when",
		})}
	}
	return nil
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
	var nameKV, labelKV, showWhenKV, placeholderKV, hintKV *ast.MappingValueNode
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
		case "show_when":
			showWhenKV = kv
		case "placeholder":
			placeholderKV = kv
		case "hint":
			hintKV = kv
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in form field`,
				Hint:     "form field accepts name / label / show_when / placeholder / hint",
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

	out = append(out, validateFormShowWhen(showWhenKV, path)...)
	// placeholder / hint — презентационные подсказки UI; пустая строка
	// бессмысленна (drop ключ) → WARNING, как у пустого label.
	out = append(out, warnEmptyPresentation(placeholderKV, name, "placeholder", path)...)
	out = append(out, warnEmptyPresentation(hintKV, name, "hint", path)...)
	return out
}

// warnEmptyPresentation — пустая строка у опц. презентационного ключа (placeholder
// / hint) бессмысленна: drop ключ. Эмитит form_field_empty_label WARNING (тот же
// класс, что у label — «пустая презентационная подпись»). kv == nil → ничего.
func warnEmptyPresentation(kv *ast.MappingValueNode, name, key, path string) []diag.Diagnostic {
	if kv == nil {
		return nil
	}
	if sn, isStr := kv.Value.(*ast.StringNode); isStr && sn.Value == "" {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "form_field_empty_label",
			Message:  fmt.Sprintf("form field %q has an empty %s", name, key),
			Hint:     "drop the key, or set a non-empty value",
			YAMLPath: path + "." + key,
		})}
	}
	return nil
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
