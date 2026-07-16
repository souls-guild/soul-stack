package statemigrate

import "strings"

// interpolateValue recursively traverses the value-tree of set.value and resolves
// `${ … }`-CEL interpolation in each string leaf ([docs/migrations.md
// §"Operations transform:" set]). Narrow copy of render-pipeline logic
// (keeper/internal/render.renderValue) — statemigrate core doesn't depend on
// render package to stay clean.
//
// Interpolation semantics inherit shared/cel: a string leaf with exactly one
// `${ }` block yields native result type (number/map/list); otherwise
// concatenation via stringification. Strings without `${ }` pass as literals (Eval
// returns them unchanged). Non-string leaves (numbers/bool/nil) pass through.
func interpolateValue(v any, ev Evaluator, scope Scope) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := interpolateValue(val, ev, scope)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			rv, err := interpolateValue(val, ev, scope)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	case string:
		// No marker — literal (don't run CEL idly; set usually writes static ACL strings).
		if !strings.Contains(t, "${") {
			return t, nil
		}
		res, err := ev.Interpolate(t, scope)
		if err != nil {
			return nil, &EvalError{Class: ClassCELInterp, Msg: "set.value: " + t, Err: err}
		}
		return res, nil
	default:
		return v, nil
	}
}
