package statemigrate

import (
	"fmt"

	"github.com/goccy/go-yaml"
)

// rawMigration — промежуточная форма файла миграции до дискриминации операций.
// transform хранится как []map[string]any: дискриминатор операции (ровно один
// ключ из набора) проверяется вручную в toOps (паттерн config/destiny_tasks.go).
type rawMigration struct {
	FromVersion *int             `yaml:"from_version"`
	ToVersion   *int             `yaml:"to_version"`
	Description string           `yaml:"description"`
	Transform   []map[string]any `yaml:"transform"`
}

// Parse разбирает содержимое одного `NNN_to_MMM.yml` в *Migration. Возвращает
// ParseError при невалидном YAML / отсутствии версий / нарушении дискриминатора
// операции. Чистая функция: I/O (чтение файла) — на вызывающей стороне.
func Parse(data []byte) (*Migration, error) {
	if len(data) == 0 {
		return nil, &ParseError{Code: CodeEmptyDocument, Msg: "пустой файл миграции"}
	}

	var rm rawMigration
	if err := yaml.Unmarshal(data, &rm); err != nil {
		return nil, &ParseError{Code: CodeYAMLParse, Msg: err.Error()}
	}

	if rm.FromVersion == nil || rm.ToVersion == nil {
		return nil, &ParseError{Code: CodeVersionMissing, Msg: "обязательны from_version и to_version"}
	}
	if *rm.ToVersion != *rm.FromVersion+1 {
		return nil, &ParseError{Code: CodeVersionInvalid, Msg: fmt.Sprintf("to_version (%d) должен быть from_version+1 (%d)", *rm.ToVersion, *rm.FromVersion+1)}
	}

	ops, err := toOps(rm.Transform)
	if err != nil {
		return nil, err
	}

	return &Migration{
		FromVersion: *rm.FromVersion,
		ToVersion:   *rm.ToVersion,
		Description: rm.Description,
		Transform:   ops,
	}, nil
}

// opKeys — известные дискриминатор-ключи операции. foreach допускает соседние
// ключи as/do, поэтому в строгой «ровно один из набора» проверке участвуют
// только дискриминаторы; siblings foreach обрабатываются отдельно.
var opKeys = []string{"rename", "set", "delete", "move", "foreach"}

var foreachSiblings = map[string]bool{"as": true, "do": true, "in": true}

// toOps дискриминирует список raw-операций в типизированный []Op. Каждая
// операция — map с ровно одним ключом-дискриминатором (+ для foreach допустимы
// соседи as/do/in).
func toOps(raw []map[string]any) ([]Op, error) {
	ops := make([]Op, 0, len(raw))
	for i, item := range raw {
		op, err := toOp(item)
		if err != nil {
			return nil, fmt.Errorf("transform[%d]: %w", i, err)
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func toOp(item map[string]any) (Op, error) {
	disc := ""
	for _, k := range opKeys {
		if _, ok := item[k]; ok {
			if disc != "" {
				return Op{}, &ParseError{Code: CodeOpDiscriminator, Msg: fmt.Sprintf("операция содержит несколько ключей (%q и %q); ровно один из %v", disc, k, opKeys)}
			}
			disc = k
		}
	}
	if disc == "" {
		return Op{}, &ParseError{Code: CodeOpDiscriminator, Msg: fmt.Sprintf("операция без ключа-дискриминатора; ожидается ровно один из %v", opKeys)}
	}

	// Для не-foreach операций посторонние ключи (кроме самого дискриминатора)
	// запрещены; для foreach допустимы as/do/in.
	if disc != "foreach" {
		for k := range item {
			if k != disc {
				return Op{}, &ParseError{Code: CodeOpDiscriminator, Msg: fmt.Sprintf("операция %q содержит посторонний ключ %q", disc, k)}
			}
		}
	}

	switch disc {
	case "rename", "move":
		return toRename(item[disc])
	case "set":
		return toSet(item["set"])
	case "delete":
		return toDelete(item["delete"])
	case "foreach":
		return toForeach(item)
	default:
		return Op{}, &ParseError{Code: CodeOpDiscriminator, Msg: "неизвестный дискриминатор " + disc}
	}
}

// toRename разбирает { from: <path>, to: <path> } (общая форма rename/move).
func toRename(v any) (Op, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Op{}, &ParseError{Code: CodeOpFieldMissing, Msg: "rename/move: ожидается { from:, to: }"}
	}
	from, okf := stringField(m, "from")
	to, okt := stringField(m, "to")
	if !okf || !okt {
		return Op{}, &ParseError{Code: CodeOpFieldMissing, Msg: "rename/move: обязательны строковые from и to"}
	}
	return Op{Rename: &RenameOp{From: from, To: to}}, nil
}

// toSet разбирает { path: <path>, value: <yaml> }. value — произвольное
// значение (литерал/${ … }/вложенная структура), интерполяция — на apply.
func toSet(v any) (Op, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Op{}, &ParseError{Code: CodeOpFieldMissing, Msg: "set: ожидается { path:, value: }"}
	}
	path, okp := stringField(m, "path")
	if !okp {
		return Op{}, &ParseError{Code: CodeOpFieldMissing, Msg: "set: обязателен строковый path"}
	}
	val, okv := m["value"]
	if !okv {
		return Op{}, &ParseError{Code: CodeOpFieldMissing, Msg: "set: обязателен value"}
	}
	return Op{Set: &SetOp{Path: path, Value: val}}, nil
}

// toDelete разбирает { path: <path> }.
func toDelete(v any) (Op, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Op{}, &ParseError{Code: CodeOpFieldMissing, Msg: "delete: ожидается { path: }"}
	}
	path, okp := stringField(m, "path")
	if !okp {
		return Op{}, &ParseError{Code: CodeOpFieldMissing, Msg: "delete: обязателен строковый path"}
	}
	return Op{Delete: &DeleteOp{Path: path}}, nil
}

// toForeach разбирает обе формы:
//   - краткая: `foreach: <expr>` + соседние `as:` / `do:`;
//   - структурная: `foreach: { in: <expr>, as:, do: }`.
//
// in берётся из значения foreach-скаляра ИЛИ из вложенного in:.
func toForeach(item map[string]any) (Op, error) {
	// Посторонние ключи на уровне item: только foreach + as/do/in.
	for k := range item {
		if k != "foreach" && !foreachSiblings[k] {
			return Op{}, &ParseError{Code: CodeOpDiscriminator, Msg: fmt.Sprintf("foreach: посторонний ключ %q", k)}
		}
	}

	var in, as string
	var doRaw any

	switch fv := item["foreach"].(type) {
	case string:
		in = fv
		as, _ = stringField(item, "as")
		doRaw = item["do"]
	case map[string]any:
		in, _ = stringField(fv, "in")
		if a, ok := stringField(fv, "as"); ok {
			as = a
		} else {
			as, _ = stringField(item, "as")
		}
		if d, ok := fv["do"]; ok {
			doRaw = d
		} else {
			doRaw = item["do"]
		}
	default:
		return Op{}, &ParseError{Code: CodeOpFieldMissing, Msg: "foreach: ожидается выражение-строка или { in:, as:, do: }"}
	}

	// Структурная форма может класть in через сосед-ключ in: (если foreach-скаляр пуст).
	if in == "" {
		in, _ = stringField(item, "in")
	}
	if in == "" {
		return Op{}, &ParseError{Code: CodeOpFieldMissing, Msg: "foreach: обязателен in (выражение коллекции)"}
	}
	if as == "" {
		return Op{}, &ParseError{Code: CodeForeachMissingAs, Msg: "foreach: обязателен as (имя переменной итерации)"}
	}

	doItems, err := toDoList(doRaw)
	if err != nil {
		return Op{}, err
	}
	return Op{Foreach: &ForeachOp{In: in, As: as, Do: doItems}}, nil
}

// toDoList приводит do: к []map[string]any и рекурсивно дискриминирует во
// вложенный []Op (вложенность foreach допустима).
func toDoList(v any) ([]Op, error) {
	if v == nil {
		return nil, &ParseError{Code: CodeOpFieldMissing, Msg: "foreach: обязателен do (список операций)"}
	}
	list, ok := v.([]any)
	if !ok {
		return nil, &ParseError{Code: CodeOpFieldMissing, Msg: "foreach.do: ожидается список операций"}
	}
	raw := make([]map[string]any, 0, len(list))
	for i, el := range list {
		m, ok := el.(map[string]any)
		if !ok {
			return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("foreach.do[%d]: ожидается операция-map", i)}
		}
		raw = append(raw, m)
	}
	return toOps(raw)
}

// stringField извлекает строковое поле map. (значение, найдено-и-строка).
func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
