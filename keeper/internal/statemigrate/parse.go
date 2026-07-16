package statemigrate

import (
	"fmt"

	"github.com/goccy/go-yaml"
)

// rawMigration — an intermediate form of the migration file before operation discrimination.
// transform is stored as []map[string]any: the operation discriminator (exactly one
// key from the set) is checked manually in toOps (the config/destiny_tasks.go pattern).
type rawMigration struct {
	FromVersion *int             `yaml:"from_version"`
	ToVersion   *int             `yaml:"to_version"`
	Description string           `yaml:"description"`
	Transform   []map[string]any `yaml:"transform"`
}

// Parse parses the contents of a single `NNN_to_MMM.yml` into a *Migration. Returns
// a ParseError on invalid YAML / missing versions / a violated operation
// discriminator. A pure function: I/O (reading the file) is the caller's responsibility.
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

// opKeys — the known operation discriminator keys. foreach allows sibling
// keys as/do, so only discriminators participate in the strict "exactly one
// of the set" check; foreach siblings are handled separately.
var opKeys = []string{"rename", "set", "delete", "move", "foreach"}

var foreachSiblings = map[string]bool{"as": true, "do": true, "in": true}

// toOps discriminates a list of raw operations into a typed []Op. Each
// operation is a map with exactly one discriminator key (+ for foreach, sibling
// as/do/in keys are allowed).
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

	// For non-foreach operations, extraneous keys (other than the discriminator itself)
	// are forbidden; for foreach, as/do/in are allowed.
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

// toRename parses { from: <path>, to: <path> } (the shared rename/move form).
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

// toSet parses { path: <path>, value: <yaml> }. value is an arbitrary
// value (literal/${ … }/nested structure), interpolation happens at apply time.
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

// toDelete parses { path: <path> }.
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

// toForeach parses both forms:
//   - short: `foreach: <expr>` + sibling `as:` / `do:`;
//   - structural: `foreach: { in: <expr>, as:, do: }`.
//
// in is taken from the foreach scalar value OR from the nested in:.
func toForeach(item map[string]any) (Op, error) {
	// Extraneous keys at the item level: only foreach + as/do/in.
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

	// The structural form can place in via the sibling in: key (if the foreach scalar is empty).
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

// toDoList coerces do: to []map[string]any and recursively discriminates it into
// a nested []Op (nested foreach is allowed).
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

// stringField extracts a string field from a map. (value, found-and-is-string).
func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
