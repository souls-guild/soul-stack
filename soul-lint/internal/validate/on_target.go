package validate

// Offline `on:`-target check (ADR-008 amendment 2026-07-17/NIM-124):
// `incarnation.id` is NOT a Coven. An `on:` element that is exactly the
// interpolation `${ incarnation.id }` is a validation error — the
// whole-incarnation target is now an OMITTED `on:` (the roster is already
// membership-scoped), not `on: [id]`. Fail-closed: a stale scenario errors
// out instead of silently resolving to an empty host set.
//
// BOTH spellings are matched. `incarnation.name` is the retired root ([ADR-0085],
// NIM-730) and still evaluates for the length of the compatibility window — so
// for that whole window it can still be written here, and a rule that only knew
// the new spelling would hand a stale scenario a way past a fail-closed guard.
// The runtime resolver has no such gap: it compares the RESOLVED value against
// the incarnation's identifier and never sees a spelling at all.
//
// This mirrors the keeper render resolver (keeper/internal/render/dispatch.go,
// resolveCovenList), which rejects the same form at runtime after CEL eval.
// soul-lint does not evaluate CEL, but it sees the literal — a bare
// `${ incarnation.id }` interpolation whose sole expression is the identifier. A
// derived value (`env-${ incarnation.id }`, `${ incarnation.id + '-x' }`)
// is a REAL coven and is deliberately NOT flagged: it resolves to something
// other than the incarnation id, exactly as the runtime resolver treats it.
//
// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md

import (
	"fmt"
	"regexp"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// bareIncarnationIDInterp matches an `on:` element that is exactly the
// interpolation of the incarnation's identifier, in either spelling and in either
// syntax (any inner whitespace). It intentionally anchors the WHOLE string:
// `env-${ incarnation.id }` and `${ incarnation.id + '-x' }` do not match (they
// yield a different value, i.e. a real coven).
//
// The index form `incarnation['id']` is here for the same reason the sibling
// legacy-root rule handles it: it selects the same key by another syntax, and a
// guard that only knew the select form would have a way past it that reads as
// deliberate to anyone who found it.
var bareIncarnationIDInterp = regexp.MustCompile(
	`^\s*\$\{\s*incarnation\s*(?:\.\s*(?:id|name)|\[\s*(?:'(?:id|name)'|"(?:id|name)")\s*\])\s*\}\s*$`)

// onIncarnationIDDiagnostics walks scenario tasks (recursing into block:
// children) and raises on_incarnation_id for every `on:` list element equal
// to the bare identifier interpolation. tasks is ScenarioManifest.Tasks; nil
// manifest → no tasks → nil (caller guards).
func onIncarnationIDDiagnostics(scenarioPath string, tasks []config.Task) []diag.Diagnostic {
	var out []diag.Diagnostic
	for i := range tasks {
		t := &tasks[i]
		out = append(out, onFieldDiagnostics(scenarioPath, t)...)
		if t.Block != nil {
			out = append(out, onIncarnationIDDiagnostics(scenarioPath, t.Block.Block)...)
		}
	}
	return out
}

// onFieldDiagnostics checks a single task's `on:` value. Only the list form
// carries coven labels (`on: keeper` / omitted are handled elsewhere); each
// string element is tested against the bare-identifier interpolation.
func onFieldDiagnostics(scenarioPath string, t *config.Task) []diag.Diagnostic {
	elems, ok := onListElements(t.On)
	if !ok {
		return nil
	}
	var out []diag.Diagnostic
	for _, s := range elems {
		if bareIncarnationIDInterp.MatchString(s) {
			out = append(out, diag.Diagnostic{
				Level:   diag.LevelError,
				Phase:   diag.PhaseSemanticValidate,
				File:    scenarioPath,
				Code:    "on_incarnation_id",
				Message: fmt.Sprintf("on: element %q targets the incarnation's own id, which is not a Coven (ADR-008 amendment/NIM-124); omit on: to target the whole incarnation", s),
				Hint:    "membership is a first-class relation now: an omitted on: already means all member hosts -- remove the on: key instead of targeting ${ incarnation.id }",
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
