package config

import (
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// lookupPath resolves a yaml path like `$.foo.bar[2].baz` to the (line, column)
// position of the key in the source AST. Used to tie semantic diagnostics (on
// the parsed Go struct) to a location in the file.
//
// Returns the position of the **key** for a mapping frame (`baz` in
// `$.foo.bar[2].baz`) and the value node's position for the final terminal
// array step (`[2]`). `ok == false` — path not found / syntactically invalid.
func lookupPath(root *ast.MappingNode, path string) (line, column int, ok bool) {
	if root == nil {
		return 0, 0, false
	}
	return lookupPathIn(root, path)
}

// PositionIn resolves `yamlPath` to the (line, column) of that key in `data`, a
// standalone YAML document this package did not load — an `include:` body, a
// `vars/_stack.yaml`. Same path subset and same answer as the position on a
// [Diagnostic] raised in-package; `ok == false` for a path the document does not
// set, a path outside that subset, or bytes that do not parse.
//
// It exists because a linter rule that walks a file OTHER than the one it was
// given still has to cite a place in that file. Without it the choice is an
// address in the wrong file or no address at all, and both are worse than the
// two lines this costs.
//
// The root may be a sequence — an included task body is a bare list — so the
// path there starts `$[0]…` rather than `$.tasks[0]…`.
func PositionIn(data []byte, yamlPath string) (line, column int, ok bool) {
	file, err := parser.ParseBytes(stripBOM(data), parser.ParseComments)
	if err != nil || len(file.Docs) == 0 || file.Docs[0].Body == nil {
		return 0, 0, false
	}
	return lookupPathIn(file.Docs[0].Body, yamlPath)
}

// lookupPathIn is [lookupPath] over any root node, mapping or sequence.
func lookupPathIn(root ast.Node, path string) (line, column int, ok bool) {
	if root == nil || path == "" {
		return 0, 0, false
	}
	segs, err := splitYAMLPath(path)
	if err != nil || len(segs) == 0 {
		return 0, 0, false
	}
	current := root
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

// splitYAMLPath parses a string of the form `$.foo.bar[2].baz` into segments.
// Supports only the subset the config validators need (no quotes, escapes,
// wildcards — this is not the full yaml.PathString).
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
