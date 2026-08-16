package validate

// Offline compute-scope check (NIM-619): `compute.<name>` written where the
// `compute` namespace does not exist.
//
// The namespace is resolved ONCE per run in the Soul-side context (ADR-009
// amendment 2026-06-23) and is readable from a task's params:/where:/apply.input
// and from state_changes. Three scenario contexts render WITHOUT it, and each is
// visible in the YAML without evaluating anything:
//
//   - an `on: keeper` task's params: and its own vars: (render.keeperVars);
//   - `loop.items:` / `loop.when:`, the host-invariant loop axis
//     (render.loopInvariantVars);
//   - the `on: [covens]` elements (render.resolveCovenList).
//
// The runtime half of the rule (shared/cel.guardComputeScope) refuses all three at
// compile time with the same wording — this is the offline half, so the author
// hears it from soul-lint instead of from a failed apply. Both halves ask
// shared/cel the same question through the same AST walk
// ([cel.Engine.InterpolationReferencesCompute] /
// [cel.Engine.ExpressionReferencesCompute]): the word in prose or inside a CEL
// string literal is not a reference, and a rule built on a regex would disagree
// with the engine in exactly those places.
//
// NOT covered offline: the fourth context, the isolated destiny pass. soul-lint's
// destiny entry point lints `destiny.yml`, whose tasks live in a separate file
// this rule never receives; the runtime guard covers it (NIM-619 comment).

import (
	"fmt"
	"sort"
	"sync"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// computeScopeEngine — the CEL engine used only to PARSE (no evaluation, no
// vault): built lazily once per process, like keeper's flowControlEngine. A
// construction failure disables the rule rather than failing the lint: this is an
// additional check over a scenario the other rules have already read, and the
// runtime guard still refuses the same expression.
var (
	computeScopeEngineOnce sync.Once
	computeScopeEngineInst *cel.Engine
)

func computeScopeEngine() *cel.Engine {
	computeScopeEngineOnce.Do(func() {
		eng, err := cel.New()
		if err != nil {
			return // leaves the instance nil — the rule no-ops
		}
		computeScopeEngineInst = eng
	})
	return computeScopeEngineInst
}

// computeScopeDiagnostics raises compute_out_of_scope for every `compute.*`
// reference in a scenario context that has no such namespace. tasks is
// ScenarioManifest.Tasks; nil manifest → no tasks → nil (caller guards).
func computeScopeDiagnostics(scenarioPath string, tasks []config.Task) []diag.Diagnostic {
	eng := computeScopeEngine()
	if eng == nil {
		return nil
	}
	var out []diag.Diagnostic
	computeScopeWalk(eng, scenarioPath, tasks, "$.tasks", &out)
	return out
}

// computeScopeWalk recurses over the task list, including block: children.
//
// `on:` is read from EACH task on its own and is never inherited downwards:
// mergeBlockInheritance carries when/where/vars/requisites and never On, so a
// descendant is keeper-side only by saying `on: keeper` itself. A block's `on:`
// narrows the roster its descendants run on, and propagating it here would flag
// params: that render in the ordinary per-host context, where compute IS
// readable. `on: keeper` on the BLOCK is not the counter-example it looks like:
// that construction renders no children at all — the top-level dispatch tests
// IsKeeperTask before the block branch and hands it to renderKeeperTask, which
// is module-only (NIM-652).
func computeScopeWalk(eng *cel.Engine, path string, tasks []config.Task, prefix string, out *[]diag.Diagnostic) {
	for i := range tasks {
		t := &tasks[i]
		where := fmt.Sprintf("%s[%d]", prefix, i)

		// `on: [covens]` — the labels resolve once per run, before any host is
		// chosen. The scalar `on: keeper` is not a list and falls through here.
		if elems, ok := onListElements(t.On); ok {
			for j, s := range elems {
				*out = appendComputeScopeDiag(*out, eng, path,
					fmt.Sprintf("%s.on[%d]", where, j), s, cel.ComputeOutOfScopeCovenList)
			}
		}

		if t.Loop != nil {
			computeScopeValue(eng, path, t.Loop.Items, where+".loop.items",
				cel.ComputeOutOfScopeLoopAxis, out)
			// loop.when: is an expression key — the whole string is CEL, no `${ }`.
			if t.Loop.When != "" && eng.ExpressionReferencesCompute(t.Loop.When) {
				*out = append(*out, computeScopeDiag(path, where+".loop.when",
					t.Loop.When, cel.ComputeOutOfScopeLoopAxis))
			}
		}

		// A keeper-side task: params: and the task's own vars: both render in the
		// run-level keeper context, so `vars:` is no way around the params: rule
		// (resolveTaskVars layers onto the very same context).
		if isKeeperTask(t) {
			if t.Module != nil {
				computeScopeValue(eng, path, t.Module.Params, where+".params",
					cel.ComputeOutOfScopeKeeperTask, out)
			}
			computeScopeValue(eng, path, t.Vars, where+".vars",
				cel.ComputeOutOfScopeKeeperTask, out)
		}

		if t.Block != nil {
			computeScopeWalk(eng, path, t.Block.Block, where+".block", out)
		}
	}
}

// isKeeperTask mirrors render.IsKeeperTask (the scalar `on: keeper`, the only
// scalar form the validator accepts). Duplicated rather than imported: soul-lint is
// offline and does not depend on keeper/.
func isKeeperTask(t *config.Task) bool {
	s, ok := t.On.(string)
	return ok && s == config.KeeperTarget
}

// computeScopeValue walks a decoded YAML value (params:/vars:/loop.items:) and
// tests every string it contains. Map keys are visited in sorted order so a
// scenario always produces its diagnostics in the same order — a map's range order
// would otherwise shuffle the report between runs.
func computeScopeValue(eng *cel.Engine, path string, v any, where string, scope cel.ComputeScope, out *[]diag.Diagnostic) {
	switch val := v.(type) {
	case string:
		*out = appendComputeScopeDiag(*out, eng, path, where, val, scope)
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			computeScopeValue(eng, path, val[k], where+"."+k, scope, out)
		}
	case []any:
		for i, e := range val {
			computeScopeValue(eng, path, e, fmt.Sprintf("%s[%d]", where, i), scope, out)
		}
	case []string:
		for i, s := range val {
			*out = appendComputeScopeDiag(*out, eng, path, fmt.Sprintf("%s[%d]", where, i), s, scope)
		}
	}
}

// appendComputeScopeDiag tests one interpolated string and appends a diagnostic if
// it reads the namespace.
func appendComputeScopeDiag(out []diag.Diagnostic, eng *cel.Engine, path, where, raw string, scope cel.ComputeScope) []diag.Diagnostic {
	if !eng.InterpolationReferencesCompute(raw) {
		return out
	}
	return append(out, computeScopeDiag(path, where, raw, scope))
}

// computeScopeDiag builds the diagnostic. The message says what the runtime error
// now says — the NAMESPACE is absent here, not the name — because the message that
// sent this ticket's author hunting was one naming a key ("no such key:
// topology_node_count") that was spelled perfectly.
func computeScopeDiag(path, where, raw string, scope cel.ComputeScope) diag.Diagnostic {
	context, hint := scope.Describe()
	return diag.Diagnostic{
		Level: diag.LevelError,
		Phase: diag.PhaseSemanticValidate,
		File:  path,
		Code:  "compute_out_of_scope",
		Message: fmt.Sprintf(
			"%q reads compute.*, but the compute namespace does not exist in %s -- the whole namespace is absent there, not just this name",
			raw, context),
		Hint:     hint,
		YAMLPath: where,
	}
}
