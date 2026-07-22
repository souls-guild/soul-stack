package cel

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/cel-go/common/types/ref"
)

// ErrPredicateNotBool — typed marker for "a top-level predicate evaluated but
// returned non-bool". Wrapped inside [ErrEval] (see [Engine.EvalPredicate]) so the
// caller can tell this case from other eval runtime errors (no-such-key,
// div-by-zero) via errors.Is — without fragile message-text matching. Additive
// sentinel: ErrEval's text is preserved, consumers keyed on the old text don't
// break.
var ErrPredicateNotBool = errors.New("predicate did not return bool")

// ErrCompile — CEL compile error (syntax, unknown identifier, incompatible types).
// Per [templating.md §10] this is a validation-phase error before the run starts.
type ErrCompile struct {
	Expr string
	Err  error
}

func (e *ErrCompile) Error() string {
	return fmt.Sprintf("CEL compile %q: %v", e.Expr, e.Err)
}

func (e *ErrCompile) Unwrap() error { return e.Err }

// ErrEval — CEL eval error (div-by-zero, access to a null field, etc.). Per
// [templating.md §10], a step runtime error.
type ErrEval struct {
	Expr string
	Err  error
}

func (e *ErrEval) Error() string {
	return fmt.Sprintf("CEL eval %q: %v", e.Expr, e.Err)
}

func (e *ErrEval) Unwrap() error { return e.Err }

// ErrUnsupported — the expression uses a construct outside pilot scope (now(); or
// vault() when the Engine has no KVReader — see functions.go). Not a panic, not a
// CEL error: a distinct class so the caller can tell "not yet implemented /
// unavailable here" from "Destiny author error".
type ErrUnsupported struct {
	Expr    string
	Feature string
}

func (e *ErrUnsupported) Error() string {
	return fmt.Sprintf("CEL unsupported %q: construct %s not yet implemented (pilot)", e.Expr, e.Feature)
}

// EvalExpression evaluates an expression where the whole string is CEL — the form
// of top-level expression keys (where:/when:/changed_when:/failed_when:/until:,
// [templating.md §2.1]). Returns ref.Val as-is; type interpretation (bool for
// where:/when:, etc.) is the caller's.
//
// If vars.Loop is non-empty (`loop:` iteration, destiny/tasks.md §7), the expression
// is compiled against a child env with the declared loop names — the bare form
// `<as>.*` resolves alongside the base context.
//
// Errors: [ErrCompile] / [ErrUnsupported] (before eval), [ErrEval] (on eval).
func (e *Engine) EvalExpression(expr string, vars Vars) (ref.Val, error) {
	norm := normalize(expr)

	loopNames := vars.loopNames()
	env, err := e.loopEnv(loopNames)
	if err != nil {
		return nil, err
	}
	prg, err := e.compile(env, strings.Join(loopNames, "\x00"), norm, vars.AllowHosts)
	if err != nil {
		return nil, err
	}

	act := vars.activation(e.migration)
	if e.kv != nil {
		// Per-eval resolver for vault(): immutable Engine.kv + request-scoped ctx.
		// Placed in the activation (not on the Engine) → vault() is concurrency-safe
		// with a shared Engine. ctx defaults to Background (offline soul-lint/Trial).
		ctx := vars.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		act[vaultResolverVar] = &vaultResolver{ctx: ctx, kv: e.kv}
	}

	out, _, err := prg.Eval(act)
	if err != nil {
		return nil, &ErrEval{Expr: expr, Err: err}
	}

	// DSL-coverage hook ([ADR-023]): after a successful eval, not on compile (the
	// Program is cached — a compile hook would undercount on repeated evals).
	// EvalInterpolation calls EvalExpression for each block, so interpolation
	// `${ … }` is caught here too. nil sink → no-op. expr is passed normalized —
	// one cache/coverage key.
	if e.sink != nil {
		e.sink.Record(norm, out)
	}
	return out, nil
}

// EvalPredicate evaluates a top-level bool predicate (whole string = CEL,
// [templating.md §2.1]) and coerces the result to bool. A convenience wrapper over
// [Engine.EvalExpression] for flow-control keys (when:/changed_when:/failed_when:/
// where:): empty expr → (true, nil) (no predicate = unconditional); non-bool result
// → [ErrEval] (a predicate must return a boolean).
//
// Errors: [ErrCompile]/[ErrUnsupported] (before eval) and [ErrEval] (on eval or on a
// non-bool result) — the caller distinguishes them via errors.As per the error table
// ([templating.md §10]).
func (e *Engine) EvalPredicate(expr string, vars Vars) (bool, error) {
	if expr == "" {
		return true, nil
	}
	out, err := e.EvalExpression(expr, vars)
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, &ErrEval{
			Expr: expr,
			Err:  fmt.Errorf("predicate returned %s, expected bool: %w", out.Type().TypeName(), ErrPredicateNotBool),
		}
	}
	return b, nil
}

// EvalInterpolation evaluates a string with embedded `${ … }` expressions
// ([templating.md §2.2]). Rules:
//
//   - Exactly one `${expr}` block with no surrounding text — the CEL result's
//     native Go value is returned ([templating.md §5(a)]).
//   - Otherwise (multiple blocks and/or surrounding text) — each block is evaluated
//     and stringified, the result concatenated into the string ([templating.md
//     §5(b)]). Stringifying a list/map when concatenating is an error
//     ([templating.md §5]).
//   - The literal `\${` escapes the marker: `${` remains in the output
//     ([templating.md §9.1]).
//
// Bracket balancing inside `${ … }` is done via the CEL parser (see
// scanInterpolation), not by text counting.
func (e *Engine) EvalInterpolation(raw string, vars Vars) (any, error) {
	segs, err := e.scanInterpolation(raw)
	if err != nil {
		return nil, err
	}

	// Cell is exactly one CEL block with no surrounding text: native type.
	if len(segs) == 1 && segs[0].expr {
		val, err := e.EvalExpression(segs[0].text, vars)
		if err != nil {
			return nil, err
		}
		return toNative(val.Value()), nil
	}

	var b strings.Builder
	for _, s := range segs {
		if !s.expr {
			b.WriteString(s.text)
			continue
		}
		val, err := e.EvalExpression(s.text, vars)
		if err != nil {
			return nil, err
		}
		str, err := stringify(s.text, val)
		if err != nil {
			return nil, err
		}
		b.WriteString(str)
	}
	return b.String(), nil
}

// toNative normalizes a native CEL value into plain Go data usable for further
// render/loop expansion (map[string]any / []any / scalars). CEL containers over the
// dyn layer (a filter-comprehension over soulprint.hosts) return .Value() as
// []ref.Val and maps with ref.Val/interface keys — these must be unwrapped
// recursively, else renderValue/resolveLoopItems won't recognize the types. Scalars
// (string/int/bool/…) pass through.
func toNative(v any) any {
	switch t := v.(type) {
	case ref.Val:
		return toNative(t.Value())
	case []ref.Val:
		out := make([]any, len(t))
		for i, el := range t {
			out[i] = toNative(el)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, el := range t {
			out[i] = toNative(el)
		}
		return out
	case map[ref.Val]ref.Val:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[toNativeKey(k.Value())] = toNative(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = toNative(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[toNativeKey(k)] = toNative(val)
		}
		return out
	default:
		return v
	}
}

// toNativeKey coerces a map key to a string (soulprint.hosts element keys are
// strings; cel may have wrapped them in ref.Val/interface).
func toNativeKey(k any) string {
	if rv, ok := k.(ref.Val); ok {
		k = rv.Value()
	}
	if s, ok := k.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", k)
}

// segment — a piece of an interpolated string: either literal text (expr=false) or
// a CEL expression without the `${ }` wrapper (expr=true).
type segment struct {
	text string
	expr bool
}

// scanInterpolation splits raw into literal segments and `${ … }` blocks. A block's
// closing `}` is determined by the CEL parser: the substring after `${` is extended
// to the first `}` at which the content parses as valid CEL — that is the `}` at the
// top level of the expression's bracket nesting ([templating.md §2.2]). If no `}`
// yields a valid parse before end of string — [ErrCompile] ("${ without closing }",
// [templating.md §10]).
//
// Escaping `\${` ([templating.md §9.1]): the sequence does not open a block; `${`
// goes into the literal.
func (e *Engine) scanInterpolation(raw string) ([]segment, error) {
	var segs []segment
	var lit strings.Builder
	i := 0
	for i < len(raw) {
		// Escaped marker: \${  →  literal ${.
		if raw[i] == '\\' && i+2 < len(raw) && raw[i+1] == '$' && raw[i+2] == '{' {
			lit.WriteString("${")
			i += 3
			continue
		}
		if raw[i] == '$' && i+1 < len(raw) && raw[i+1] == '{' {
			if lit.Len() > 0 {
				segs = append(segs, segment{text: lit.String()})
				lit.Reset()
			}
			inner, next, err := e.parseBlock(raw, i+2)
			if err != nil {
				return nil, err
			}
			segs = append(segs, segment{text: inner, expr: true})
			i = next
			continue
		}
		lit.WriteByte(raw[i])
		i++
	}
	if lit.Len() > 0 {
		segs = append(segs, segment{text: lit.String()})
	}
	return segs, nil
}

// parseBlock finds the end of a `${ … }` block opened at position start (the first
// byte after `${`). Returns the expression text (trimmed) and the index just past
// the closing `}`.
func (e *Engine) parseBlock(raw string, start int) (string, int, error) {
	for end := start; end < len(raw); end++ {
		if raw[end] != '}' {
			continue
		}
		inner := raw[start:end]
		if _, issues := e.env.Parse(strings.TrimSpace(inner)); issues == nil || issues.Err() == nil {
			return strings.TrimSpace(inner), end + 1, nil
		}
	}
	return "", 0, &ErrCompile{
		Expr: raw[start:],
		Err:  fmt.Errorf("${ without a closing } or an invalid expression"),
	}
}

// stringify coerces a CEL value to a string for concatenation ([templating.md §5]).
// Scalars (int/float/bool/string) and timestamp — canonical stringification via
// cel-go ConvertToType(StringType). A list/map cannot be concatenated with a string
// — [ErrEval] ([templating.md §5]).
func stringify(expr string, val ref.Val) (string, error) {
	switch v := val.Value().(type) {
	case string:
		return v, nil
	}

	str := val.ConvertToType(stringType)
	if s, ok := str.Value().(string); ok {
		return s, nil
	}

	return "", &ErrEval{
		Expr: expr,
		Err:  fmt.Errorf("result of type %s cannot be concatenated with a string (move it to a separate cell)", val.Type().TypeName()),
	}
}
