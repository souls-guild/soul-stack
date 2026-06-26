package config

// Переиспользуемые именованные типы input-схемы (`service/<name>/types.yml`).
//
// Сервис может объявить каталог типов — секцию `types:` (имя → схема в том же
// InputSchema-DSL). Сценарии ссылаются на тип ключом-дискриминатором `$type`:
// самостоятельным полем (`<param>: { $type: T }`) или элементом массива
// (`items: { $type: T }`). При загрузке сервиса ссылки резолвятся —
// [ResolveTypeRefs] подставляет схему типа на место узла-ссылки.
//
// Резолв чистый (без I/O): caller (artifact/soul-lint) читает types.yml и
// input-схемы, парсит их `shared/config`-ом, затем зовёт [ParseTypeCatalog] +
// [ResolveTypeRefs]. Это параллель к резолву vars/templates-ресурсов на загрузке
// сервиса (см. artifact/service.go): схема каталога валидируется один раз,
// результат — самодостаточная input-схема без `$type`.
//
// MVP-границы (решение пользователя): object + array-of-type + вложенность
// тип→тип с cycle-detection. БЕЗ scalar-alias, кросс-сервисных каталогов и
// local-per-scenario типов.

import (
	"fmt"
	"regexp"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// typesCatalogFile — имя каталога типов в корне Service-репо, рядом с
// `service.yml`/`scenario/`/`migrations/`. Экспортируемая константа — caller
// (artifact/soul-lint) читает файл сам, пакет config остаётся без I/O.
const TypesCatalogFile = "types.yml"

// typesSectionKey — единственный top-level ключ в `types.yml`.
const typesSectionKey = "types"

// reInputTypeName — имя именованного типа (ключ в `types:` и значение `$type:`).
// Отдельное пространство имён от input-параметров (snake_case): тип — это
// логический «класс» формы, поэтому строго PascalCase `^[A-Z][A-Za-z0-9]*$`
// (naming-rules.md §«Named input types»). Стартует с заглавной буквы; внутри —
// только буквы/цифры (без `_`/дефисов/точек/пробелов): имя пишется как есть в
// `$type:` и должно быть однозначным токеном-классом.
var reInputTypeName = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)

// typeRefResolveLimit — страховочный предел числа ПЕРЕХОДОВ ПО type-ref-ссылкам
// (хопов A→$type B→$type C…) на случай, если cycle-detection пропустит патологию
// (он не должен). Считается ТОЛЬКО type-ref-хоп — структурный спуск в items/
// properties/additional_properties глубину НЕ инкрементит (иначе глубоко
// вложенный обычный object даёт ложный input_type_cycle). Глубина графа типов в
// реальных каталогах — единицы; 64 заведомо достаточно и ловит runaway по
// ref-графу без переполнения стека.
const typeRefResolveLimit = 64

// validateTypeRefNode — структурная валидность одного узла-ссылки `{$type: T}`.
// Узел уже опознан как ссылка (есть ключ `$type`), per-type-валидация для него
// пропущена. Проверяем ИНВАРИАНТЫ ССЫЛКИ:
//   - `$type` — непустая строка валидной формы (reInputTypeName);
//   - конфликта с собственной формой нет: `type:`/`properties:`/`items:`
//     ВМЕСТЕ с `$type` → input_type_ref_conflict (непонятно, что побеждает).
//     `items: { $type: T }` — это items-ССЫЛКА уровня РОДИТЕЛЯ, она НЕ попадает
//     сюда (родитель = array с items, а не узел с `$type`); здесь любой `items`
//     рядом с `$type` — именно конфликт.
//
// Само существование типа T в каталоге проверяет [ResolveTypeRefs] (нужен
// каталог) — здесь только локальная форма узла (симметрия validateSource:
// форма локально, множество — резолвером).
func validateTypeRefNode(s *InputSchema, refKV *ast.MappingValueNode, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic

	// $type — должен быть непустой строкой валидной формы.
	if _, ok := refKV.Value.(*ast.StringNode); !ok {
		tok := refKV.Value.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "$type must be a string (named type reference)",
			Hint:     "$type: <TypeName> — references an entry in service/<name>/types.yml",
			YAMLPath: path + ".$type",
		}))
	} else if ref := s.typeRef(); !reInputTypeName.MatchString(ref) {
		tok := refKV.Value.GetToken()
		msg := fmt.Sprintf("$type %q does not match %s", ref, reInputTypeName)
		if ref == "" {
			msg = "$type must be a non-empty type name"
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_type_ref_name_invalid",
			Message:  msg,
			Hint:     "type name: PascalCase — starts with an uppercase letter; only [A-Za-z0-9]; no underscores/dots/dashes/spaces",
			YAMLPath: path + ".$type",
		}))
	}

	// Конфликт: $type ВМЕСТЕ с собственной формой узла. Любой из этих ключей
	// рядом с $type делает узел двусмысленным.
	for _, key := range []string{"type", "properties", "items"} {
		kv, ok := present[key]
		if !ok {
			continue
		}
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_type_ref_conflict",
			Message:  fmt.Sprintf("$type cannot be combined with %q on the same node", key),
			Hint:     "a $type node is a reference — drop type:/properties:/items: (the named type provides the shape)",
			YAMLPath: path + "." + key,
		}))
	}

	return out
}

// typeRef — безопасный геттер TypeRef (nil-safe).
func (s *InputSchema) typeRef() string {
	if s == nil {
		return ""
	}
	return s.TypeRef
}

// TypeCatalog — резолвнутый каталог именованных типов сервиса: имя → схема.
// Значения — самодостаточные input-схемы (внутри них `$type`-ссылки уже
// разрешены при построении каталога, с cycle-detection).
type TypeCatalog map[string]*InputSchema

// ParseTypeCatalog парсит `types.yml` (top-level секция `types:`: имя → схема в
// InputSchema-DSL) и резолвит `$type`-ссылки МЕЖДУ типами с cycle-detection.
// data — сырое содержимое types.yml; filename — для File-поля диагностик.
//
// Возвращает каталог (имена → резолвнутые схемы) и diagnostics. При наличии
// error-диагностик каталог может быть частичным/nil — caller проверяет
// diag.HasErrors и не использует битый каталог.
//
// Ошибки:
//   - input_type_duplicate — два типа с одним именем (ключ-коллизия в `types:`);
//   - input_type_unknown   — `$type` ссылается на отсутствующий в каталоге тип;
//   - input_type_cycle     — циклическая ссылка (A→$type B→$type A);
//   - плюс обычные schema/semantic-ошибки самих схем типов (через
//     validateInputSchemaMap — type-required, формат имён и т.п.).
//
// Пустой/отсутствующий `types:` → пустой каталог без ошибок (сервис без типов
// валиден).
func ParseTypeCatalog(filename string, data []byte) (TypeCatalog, []diag.Diagnostic) {
	data = stripBOM(data)
	// AllowDuplicateMapKey: дубликат имени типа goccy иначе отверг бы parse-
	// ошибкой; разрешаем парс, чтобы поднять доменный input_type_duplicate по AST.
	file, err := parser.ParseBytes(data, parser.ParseComments, parser.AllowDuplicateMapKey())
	if err != nil {
		return nil, []diag.Diagnostic{yamlParseDiag(filename, err)}
	}
	if len(file.Docs) == 0 || file.Docs[0].Body == nil {
		// Пустой файл — пустой каталог. types.yml опционален.
		return TypeCatalog{}, nil
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		// Comment-only / whitespace-only файл → не mapping, но это валидный
		// «нет типов» (types.yml опционален). Скаляр/sequence на корне — ошибка.
		switch file.Docs[0].Body.(type) {
		case *ast.CommentGroupNode, *ast.NullNode:
			return TypeCatalog{}, nil
		}
		tok := file.Docs[0].Body.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File: filename, Line: line, Column: col,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("types.yml root must be a mapping, got %T", file.Docs[0].Body),
			YAMLPath: "$",
		}}
	}

	typesNode := findInputMapping(root, typesSectionKey)
	// Декод секции в InputSchemaMap (тот же DSL, что input:). Декодируем весь
	// файл в обёртку, чтобы переиспользовать UnmarshalYAML InputSchema.
	type typesFileYAML struct {
		Types InputSchemaMap `yaml:"types"`
	}
	var decoded typesFileYAML
	// AllowDuplicateMapKey на decode-фазе: дубликат имени типа уже разрешён на
	// parse-фазе (см. выше); без него NodeToValue отверг бы decode `type_mismatch`,
	// маскируя доменный input_type_duplicate (его поднимает AST-скан ниже).
	if derr := yaml.NodeToValue(root, &decoded, yaml.AllowDuplicateMapKey()); derr != nil {
		return nil, []diag.Diagnostic{decodeErrorDiag(filename, derr)}
	}

	var diags []diag.Diagnostic

	// Unknown top-level ключи (только `types:` допустим).
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value == typesSectionKey {
			continue
		}
		diags = append(diags, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File: filename, Code: "unknown_key",
			Message:  `unknown field "` + tok.Value + `"`,
			Hint:     "types.yml has a single top-level section: types:",
			YAMLPath: "$." + tok.Value,
		}))
	}

	if typesNode == nil {
		// Нет секции types: (или она не mapping) → пустой каталог.
		return TypeCatalog{}, withFile(diags, filename)
	}

	// input_type_duplicate — два типа с одним именем. goccy декодирует map с
	// last-wins, теряя дубликат; ловим по AST до того, как он схлопнется.
	seen := map[string]bool{}
	for _, kv := range typesNode.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		name := tok.Value
		if seen[name] {
			diags = append(diags, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				File: filename, Code: "input_type_duplicate",
				Message:  fmt.Sprintf("type %q is declared more than once", name),
				Hint:     "each type name in types: must be unique",
				YAMLPath: "$.types." + name,
			}))
			continue
		}
		seen[name] = true
		if !reInputTypeName.MatchString(name) {
			diags = append(diags, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				File: filename, Code: "input_type_ref_name_invalid",
				Message:  fmt.Sprintf("type name %q does not match %s", name, reInputTypeName),
				Hint:     "type name: PascalCase — uppercase first letter, only [A-Za-z0-9]",
				YAMLPath: "$.types." + name,
			}))
		}
	}

	// Schema/semantic-валидация самих схем типов (type-required, формат полей,
	// $type-ref-conflict внутри типа и т.п.). НЕ через validateInputSchemaMap:
	// тот применяет к ключам snake_case-regex input-параметров, а имена типов —
	// PascalCase (своя проверка имени уже сделана в duplicate-loop выше). Здесь
	// валидируем только ТЕЛО каждого типа. $type-ссылка ВНУТРИ типа на другой тип
	// допустима (вложенность тип→тип); её существование проверит резолв ниже.
	for _, kv := range typesNode.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		bodyNode, isMap := kv.Value.(*ast.MappingNode)
		if !isMap {
			diags = append(diags, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				File: filename, Code: "type_mismatch",
				Message:  fmt.Sprintf("type %q must be a schema (mapping)", tok.Value),
				YAMLPath: "$.types." + tok.Value,
			}))
			continue
		}
		diags = append(diags, validateInputSchemaNode(decoded.Types[tok.Value], bodyNode, "$.types."+tok.Value)...)
	}

	// Резолв `$type`-ссылок между типами с cycle-detection. Работаем на КОПИИ
	// схем (resolveSchemaRefs мутирует поддерево), чтобы каталог отдавал
	// самодостаточные значения.
	catalog := make(TypeCatalog, len(decoded.Types))
	for name, schema := range decoded.Types {
		catalog[name] = schema
	}
	for name := range catalog {
		resolved, rdiags := resolveOneType(name, catalog, "$.types."+name)
		diags = append(diags, withFile(rdiags, filename)...)
		if resolved != nil {
			catalog[name] = resolved
		}
	}

	return catalog, withFile(diags, filename)
}

// resolveOneType резолвит схему одного типа `name` из каталога: рекурсивно
// подставляет все `$type`-ссылки внутри неё, отслеживая путь обхода для
// cycle-detection. `path` — yaml-path для диагностик.
func resolveOneType(name string, catalog TypeCatalog, path string) (*InputSchema, []diag.Diagnostic) {
	schema := catalog[name]
	if schema == nil {
		return nil, nil
	}
	// Стек активных типов на текущей ветке обхода — для обнаружения цикла.
	stack := map[string]bool{name: true}
	return resolveSchemaRefs(schema, catalog, stack, path, 0)
}

// resolveSchemaRefs возвращает копию `schema` со всеми `$type`-ссылками,
// разрешёнными подстановкой схем из каталога. `stack` — множество типов на
// текущей ветке обхода (cycle-detection: повторный заход в тип на стеке =
// input_type_cycle). Резолв НЕ мутирует исходную схему — строит новую,
// поэтому общий тип, использованный дважды в разных ветках, резолвится
// независимо без ложного цикла.
//
// `depth` — число пройденных type-ref-ХОПОВ на текущей ветке (цикл — свойство
// ref-графа): инкрементится ТОЛЬКО при заходе в целевой тип ссылки. Структурный
// спуск в items/properties/additional_properties глубину НЕ трогает, иначе
// глубоко вложенный обычный object ложно превысил бы лимит и дал input_type_cycle.
func resolveSchemaRefs(schema *InputSchema, catalog TypeCatalog, stack map[string]bool, path string, depth int) (*InputSchema, []diag.Diagnostic) {
	if schema == nil {
		return nil, nil
	}
	if depth > typeRefResolveLimit {
		return schema, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_type_cycle",
			Message:  fmt.Sprintf("type reference resolution exceeded %d ref-hops (possible cycle)", typeRefResolveLimit),
			YAMLPath: path,
		}}
	}

	// Узел-ссылка: подставляем схему целевого типа (рекурсивно резолвнутую).
	if schema.TypeRef != "" {
		ref := schema.TypeRef
		if stack[ref] {
			return nil, []diag.Diagnostic{{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:     "input_type_cycle",
				Message:  fmt.Sprintf("type reference cycle detected at %q", ref),
				Hint:     "a type must not reference itself transitively (A → $type B → $type A)",
				YAMLPath: path + ".$type",
			}}
		}
		target, ok := catalog[ref]
		if !ok {
			return nil, []diag.Diagnostic{{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:     "input_type_unknown",
				Message:  fmt.Sprintf("$type %q is not declared in types.yml", ref),
				Hint:     "declare the type under types: or fix the reference name",
				YAMLPath: path + ".$type",
			}}
		}
		stack[ref] = true
		resolved, diags := resolveSchemaRefs(target, catalog, stack, path, depth+1)
		delete(stack, ref)
		// Описание/обязательность узла-ссылки (description/required/required_when
		// на самом `$type`-узле) сейчас в MVP не переносим: узел-ссылка несёт
		// ТОЛЬКО `$type`. Любой иной ключ рядом — уже отвергнут conflict-проверкой
		// (type/properties/items) либо допустим как presentational (description) —
		// его сохраняем, накладывая поверх резолвнутой схемы.
		if resolved != nil {
			applyRefOverlay(schema, resolved)
		}
		return resolved, diags
	}

	// Обычный узел: копируем и рекурсивно резолвим items/properties/AP.
	// Структурный спуск НЕ инкрементит depth — лимит считает только type-ref-хопы
	// (см. doc-комментарий): глубокий обычный object не должен ложно сработать.
	out := *schema
	var diags []diag.Diagnostic

	if schema.Items != nil {
		ri, d := resolveSchemaRefs(schema.Items, catalog, stack, path+".items", depth)
		out.Items = ri
		diags = append(diags, d...)
	}
	if len(schema.Properties) > 0 {
		props := make(InputSchemaMap, len(schema.Properties))
		for pn, ps := range schema.Properties {
			rp, d := resolveSchemaRefs(ps, catalog, stack, path+".properties."+pn, depth)
			props[pn] = rp
			diags = append(diags, d...)
		}
		out.Properties = props
	}
	if ap, ok := schema.AdditionalProperties.(*InputSchema); ok {
		rap, d := resolveSchemaRefs(ap, catalog, stack, path+".additional_properties", depth)
		out.AdditionalProperties = rap
		diags = append(diags, d...)
	}

	return &out, diags
}

// applyRefOverlay переносит presentational-поля с узла-ссылки на резолвнутую
// схему (узел `{ $type: T, description: "..." }` сохраняет своё description).
// Поверх резолвнутого типа кладём ТОЛЬКО непустые presentational-ключи ссылки,
// не затирая саму форму типа. MVP: только description (единственный безопасный
// presentational-ключ, не меняющий контракт формы).
func applyRefOverlay(ref, resolved *InputSchema) {
	if ref.Description != "" {
		resolved.Description = ref.Description
	}
}

// ResolveTypeRefs резолвит `$type`-ссылки в input-схеме `in` (например, input:
// сценария) по каталогу типов сервиса. Возвращает НОВУЮ схему-map с
// подставленными типами и diagnostics (input_type_unknown / input_type_cycle).
//
// Исходный `in` не мутируется. Узлы без `$type` копируются как есть (back-compat:
// схемы без типов проходят насквозь). Каталог `catalog` уже резолвнут
// внутри себя ([ParseTypeCatalog]); здесь резолвится только сторона потребителя.
func ResolveTypeRefs(in InputSchemaMap, catalog TypeCatalog) (InputSchemaMap, []diag.Diagnostic) {
	if in == nil {
		return nil, nil
	}
	out := make(InputSchemaMap, len(in))
	var diags []diag.Diagnostic
	for name, schema := range in {
		// Свежий стек на каждый параметр: ссылка из параметра в тип не образует
		// цикла сама по себе; циклы — только МЕЖДУ типами (уже отловлены в
		// ParseTypeCatalog), но проверяем повторно для надёжности.
		stack := map[string]bool{}
		resolved, d := resolveSchemaRefs(schema, catalog, stack, "$.input."+name, 0)
		out[name] = resolved
		diags = append(diags, d...)
	}
	return out, diags
}

// withFile проставляет File тем диагностикам, где оно пустое (унификация с
// parseAndValidate). Возвращает тот же slice (мутирует in-place).
func withFile(ds []diag.Diagnostic, file string) []diag.Diagnostic {
	for i := range ds {
		if ds[i].File == "" {
			ds[i].File = file
		}
	}
	return ds
}
