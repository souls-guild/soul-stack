package validate

// Offline `on:`-target check (ADR-008 amendment 2026-07-17/NIM-124):
// `incarnation.name` is NOT a Coven. An `on:` element that is exactly the
// interpolation `${ incarnation.name }` is a validation error — the
// whole-incarnation target is now an OMITTED `on:` (the roster is already
// membership-scoped), not `on: [name]`. Fail-closed: a stale scenario errors
// out instead of silently resolving to an empty host set.
//
// This mirrors the keeper render resolver (keeper/internal/render/dispatch.go,
// resolveCovenList), which rejects the same form at runtime after CEL eval.
// soul-lint does not evaluate CEL, but it sees the literal — a bare
// `${ incarnation.name }` interpolation whose sole expression is the name. A
// derived value (`env-${ incarnation.name }`, `${ incarnation.name + '-x' }`)
// is a REAL coven and is deliberately NOT flagged: it resolves to something
// other than the incarnation name, exactly as the runtime resolver treats it.

import (
	"fmt"
	"regexp"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// bareIncarnationNameInterp matches an `on:` element that is exactly the
// interpolation of `incarnation.name` (any inner whitespace). It intentionally
// anchors the WHOLE string: `env-${ incarnation.name }` and
// `${ incarnation.name + '-x' }` do not match (they yield a different value,
// i.e. a real coven).
var bareIncarnationNameInterp = regexp.MustCompile(`^\s*\$\{\s*incarnation\.name\s*\}\s*$`)

// onIncarnationNameDiagnostics walks scenario tasks (recursing into block:
// children) and raises on_incarnation_name for every `on:` list element equal
// to the bare `${ incarnation.name }` interpolation. tasks is
// ScenarioManifest.Tasks; nil manifest → no tasks → nil (caller guards).
func onIncarnationNameDiagnostics(scenarioPath string, tasks []config.Task) []diag.Diagnostic {
	var out []diag.Diagnostic
	for i := range tasks {
		t := &tasks[i]
		out = append(out, onFieldDiagnostics(scenarioPath, t)...)
		if t.Block != nil {
			out = append(out, onIncarnationNameDiagnostics(scenarioPath, t.Block.Block)...)
		}
	}
	return out
}

// onFieldDiagnostics checks a single task's `on:` value. Only the list form
// carries coven labels (`on: keeper` / omitted are handled elsewhere); each
// string element is tested against the bare-name interpolation.
func onFieldDiagnostics(scenarioPath string, t *config.Task) []diag.Diagnostic {
	elems, ok := onListElements(t.On)
	if !ok {
		return nil
	}
	var out []diag.Diagnostic
	for _, s := range elems {
		if bareIncarnationNameInterp.MatchString(s) {
			out = append(out, diag.Diagnostic{
				Level:   diag.LevelError,
				Phase:   diag.PhaseSemanticValidate,
				File:    scenarioPath,
				Code:    "on_incarnation_name",
				Message: fmt.Sprintf("on: element %q targets the incarnation's own name, which is not a Coven (ADR-008 amendment/NIM-124); omit on: to target the whole incarnation", s),
				Hint:    "membership is a first-class relation now: an omitted on: already means all member hosts -- remove the on: key instead of targeting ${ incarnation.name }",
			})
		}
	}
	return out
}

// onListElements returns the string elements of a list-form `on:`. goccy decodes
// a YAML sequence into []any (or, if homogeneous, possibly []string); both are
// handled. A scalar `on:` (e.g. "keeper") or a non-list value returns ok=false
// (nothing to check on this path).
func onListElements(on any) ([]string, bool) {
	switch v := on.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	case []string:
		return v, true
	default:
		return nil, false
	}
}
