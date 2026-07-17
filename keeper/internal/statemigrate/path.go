package statemigrate

import (
	"fmt"
	"strings"
)

// pathSegment — a single segment of a state-path address: either a literal (letters/digits/
// `_`/`-`), or a `${ <CEL> }` interpolation, resolved to a string before navigation.
type pathSegment struct {
	literal string // non-empty if the segment is a literal
	expr    string // non-empty if the segment = ${ <expr> }
}

// parsePath splits an address like `state.foo.bar.${ name }` into segments AFTER
// the root `state.` ([docs/migrations.md §"Addressing — path:"]). The prefix
// `state.` is required. Segmentation happens on `.` at the top level; dots inside
// `${ … }` belong to the expression and don't separate segments.
//
// Returns a ParseError (a malformed path is a migration author's mistake,
// caught at parse/apply time, not per state object).
func parsePath(raw string) ([]pathSegment, error) {
	const prefix = "state"
	trimmed := strings.TrimSpace(raw)
	switch {
	case trimmed == prefix:
		// Bare `state` — the whole root; no segments. set/rename/delete
		// on the root are unsupported (no point) — an empty path is rejected
		// by the caller (apply.go).
		return nil, nil
	case strings.HasPrefix(trimmed, prefix+"."):
		// ok, cut the body after `state.`
	default:
		return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q must start with 'state.'", raw)}
	}

	body := trimmed[len(prefix)+1:]
	if body == "" {
		return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: empty address after 'state.'", raw)}
	}

	var segs []pathSegment
	i := 0
	var cur strings.Builder
	flushLiteral := func() error {
		s := cur.String()
		cur.Reset()
		if s == "" {
			return &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: empty segment", raw)}
		}
		segs = append(segs, pathSegment{literal: s})
		return nil
	}

	for i < len(body) {
		// Start of a `${ … }` block: the whole segment is an interpolation.
		if body[i] == '$' && i+1 < len(body) && body[i+1] == '{' {
			if cur.Len() != 0 {
				return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: ${…} must be a standalone segment (between dots)", raw)}
			}
			end := strings.IndexByte(body[i:], '}')
			if end < 0 {
				return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: ${ without a closing }", raw)}
			}
			end += i
			expr := strings.TrimSpace(body[i+2 : end])
			if expr == "" {
				return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: empty ${ }", raw)}
			}
			segs = append(segs, pathSegment{expr: expr})
			i = end + 1
			// After the block we expect either the end or a `.` separator.
			if i < len(body) {
				if body[i] != '.' {
					return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: expected '.' or end after ${…}", raw)}
				}
				i++ // consumed the separator; the next segment is required
				if i >= len(body) {
					return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: path ends with '.'", raw)}
				}
			}
			continue
		}
		if body[i] == '.' {
			if err := flushLiteral(); err != nil {
				return nil, err
			}
			i++
			if i >= len(body) {
				return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: path ends with '.'", raw)}
			}
			continue
		}
		cur.WriteByte(body[i])
		i++
	}
	if cur.Len() > 0 {
		if err := flushLiteral(); err != nil {
			return nil, err
		}
	}
	return segs, nil
}

// resolveSegments resolves ${ … } segments into string keys via the Evaluator,
// returning a flat list of final navigation keys. Literals pass through as
// is; expr segments are evaluated and stringified. scope carries state +
// active foreach variables.
func resolveSegments(segs []pathSegment, ev Evaluator, scope Scope) ([]string, error) {
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if s.expr == "" {
			out = append(out, s.literal)
			continue
		}
		val, err := ev.Eval(s.expr, scope)
		if err != nil {
			return nil, &EvalError{Class: ClassCELInterp, Msg: fmt.Sprintf("path segment ${ %s }", s.expr), Err: err}
		}
		key, ok := stringKey(val)
		if !ok {
			return nil, &EvalError{Class: ClassPathSegment, Msg: fmt.Sprintf("path segment ${ %s } produced %T, expected a string/number key", s.expr, val)}
		}
		out = append(out, key)
	}
	return out, nil
}

// stringKey coerces a CEL segment's result into a string map key. Strings pass
// through as is; ints/uints use decimal form (map keys in state are JSON-safe strings).
func stringKey(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case int:
		return fmt.Sprintf("%d", t), true
	case int64:
		return fmt.Sprintf("%d", t), true
	case uint64:
		return fmt.Sprintf("%d", t), true
	default:
		return "", false
	}
}
