package config

import (
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"
)

// lookupPath резолвит yaml-путь вида `$.foo.bar[2].baz` к позиции
// (line, column) ключа в исходном AST. Используется, чтобы привязать
// semantic-диагностики (на распарсенной Go-структуре) к месту в файле.
//
// Возвращает позицию **ключа** для mapping-кадра (`baz` в `$.foo.bar[2].baz`)
// и позицию value-узла для последнего терминального шага в массиве (`[2]`).
// `ok == false` — путь не найден / синтаксически невалиден.
func lookupPath(root *ast.MappingNode, path string) (line, column int, ok bool) {
	if root == nil || path == "" {
		return 0, 0, false
	}
	segs, err := splitYAMLPath(path)
	if err != nil || len(segs) == 0 {
		return 0, 0, false
	}
	var current ast.Node = root
	var keyTok *struct{ Line, Column int }
	for _, seg := range segs {
		if seg.isIndex {
			seq, isSeq := current.(*ast.SequenceNode)
			if !isSeq || seg.index < 0 || seg.index >= len(seq.Values) {
				return 0, 0, false
			}
			current = seq.Values[seg.index]
			t := current.GetToken()
			if t != nil {
				keyTok = &struct{ Line, Column int }{t.Position.Line, t.Position.Column}
			}
			continue
		}
		m, isMap := current.(*ast.MappingNode)
		if !isMap {
			return 0, 0, false
		}
		found := false
		for _, kv := range m.Values {
			kt := kv.Key.GetToken()
			if kt == nil {
				continue
			}
			if kt.Value == seg.name {
				keyTok = &struct{ Line, Column int }{kt.Position.Line, kt.Position.Column}
				current = kv.Value
				found = true
				break
			}
		}
		if !found {
			return 0, 0, false
		}
	}
	if keyTok == nil {
		return 0, 0, false
	}
	return keyTok.Line, keyTok.Column, true
}

type pathSeg struct {
	name    string
	isIndex bool
	index   int
}

// splitYAMLPath разбирает строку формата `$.foo.bar[2].baz` в сегменты.
// Поддерживает только подмножество, нужное конфиг-валидаторам (никаких
// квот, escape-ов, wildcards — это не yaml.PathString во всей полноте).
func splitYAMLPath(p string) ([]pathSeg, error) {
	if !strings.HasPrefix(p, "$") {
		return nil, errBadPath
	}
	rest := p[1:]
	var out []pathSeg
	for len(rest) > 0 {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			var name string
			if end == -1 {
				name = rest
				rest = ""
			} else {
				name = rest[:end]
				rest = rest[end:]
			}
			if name == "" {
				return nil, errBadPath
			}
			out = append(out, pathSeg{name: name})
		case '[':
			closeIdx := strings.IndexByte(rest, ']')
			if closeIdx < 0 {
				return nil, errBadPath
			}
			n, err := strconv.Atoi(rest[1:closeIdx])
			if err != nil {
				return nil, errBadPath
			}
			out = append(out, pathSeg{isIndex: true, index: n})
			rest = rest[closeIdx+1:]
		default:
			return nil, errBadPath
		}
	}
	return out, nil
}

var errBadPath = badPathErr{}

type badPathErr struct{}

func (badPathErr) Error() string { return "bad yaml path" }
