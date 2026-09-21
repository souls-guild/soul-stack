package validate

// Offline compute checks (NIM-619). Two rules share one walk over the scenario,
// because both answer the same question — "will this `compute.<name>` resolve?" —
// and differ only in why the answer is no:
//
//   - compute_out_of_scope: the context has no `compute` namespace at all;
//   - compute_unknown_name: the context HAS it, but the scenario declares no entry
//     by that name (a misspelling), or declares it LATER in the block than the
//     entry reading it (entries resolve in declaration order).
//
// The namespace is resolved ONCE per run in a run-level, soulprint-free context
// (ADR-009 amendment 2026-06-23) and is therefore readable in everything Keeper
// RENDERS: a task's params:/vars:/where:/apply.input, assert.that[], and — since
// NIM-619 — an `on: keeper` task (render.keeperVars), a `core.state.<verb>`
// capture included. Four scenario contexts read without it, and each is visible in
// the YAML without evaluating anything:
//
//   - `loop.items:` / `loop.when:`, the host-invariant loop axis
//     (render.loopInvariantVars);
//   - the `on: [covens]` elements (render.resolveCovenList);
//   - a capture's `match:` predicate, evaluated per collection element at merge
//     time (render.Pipeline.StateOpEvaluators — elem/key/value only);
//   - `when:` / `changed_when:` / `failed_when:` / `until:`, which Keeper does not
//     render at all — they travel to Soul as text and are evaluated in the
//     flow-control sandbox (cel.NewFlowControl), whose env has no `compute`.
//
// Both halves ask shared/cel the same question through the same AST walk
// ([cel.Engine.InterpolationReferencesCompute] /
// [cel.Engine.ExpressionComputeNames] and their siblings): the word in prose or
// inside a CEL string literal is not a reference, and a rule built on a regex
// would disagree with the engine in exactly those places. A reference whose name
// is not in the source (`compute[input.k]`, `size(compute)`) leaves the cell to the
// run — the linter reports what the author wrote, never a name it inferred.
//
// NOT covered offline, deliberately:
//
//   - the isolated destiny pass — the runtime guard refuses it, but soul-lint's
//     destiny entry point lints `destiny.yml`, whose tasks live in a file this rule
//     never receives;
//   - tasks pulled in by `include:` — they are not in ScenarioManifest.Tasks;
//   - the name rule as a whole when a covenant fragment failed to resolve, since
//     the declared set would then be missing the fragment's own entries
//     (mergeComputeSections prepends them).

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// computeScopeEngine — the CEL engine used only to PARSE (no evaluation, no
// vault): built lazily once per process, like keeper's flowControlEngine. A
// construction failure disables the rules rather than failing the lint: these are
// additional checks over a scenario the other rules have already read, and the
// runtime guard still refuses the same expression.
var (
	computeScopeEngineOnce sync.Once
	computeScopeEngineInst *cel.Engine
)

func computeScopeEngine() *cel.Engine {
	computeScopeEngineOnce.Do(func() {
		eng, err := cel.New()
		if err != nil {
			return // leaves the instance nil — the rules no-op
		}
		computeScopeEngineInst = eng
	})
	return computeScopeEngineInst
}

// computeDiagnostics runs both rules over a parsed scenario.
//
// namesResolved is the caller's statement that scn.Compute is the FULL declared
// set — i.e. that covenant resolution either did not apply or did not fail. False
// disables the name rule (an incomplete set would report a covenant's own entries
// as typos) and leaves the scope rule, which does not depend on names.
func computeDiagnostics(scenarioPath string, scn *config.ScenarioManifest, namesResolved bool) []diag.Diagnostic {
	eng := computeScopeEngine()
	if eng == nil || scn == nil {
		return nil
	}
	c := &computeChecker{eng: eng, path: scenarioPath}
	if namesResolved {
		c.declared = declaredComputeNames(scn.Compute)
	}
	c.computeBlock(scn.Compute)
	c.tasks(scn.Tasks, "$.tasks")
	return c.out
}

// declaredComputeNames maps each declared name to its position in the block.
// Non-nil even for an empty block: "this scenario declares nothing" is a fact the
// name rule uses, not a reason to stay silent.
func declaredComputeNames(block config.ComputeBlock) map[string]int {
	out := make(map[string]int, len(block))
	for i, cv := range block {
		out[cv.Name] = i
	}
	return out
}

// computeChecker carries the walk's state. declared == nil disables the name rule.
type computeChecker struct {
	eng      *cel.Engine
	path     string
	declared map[string]int
	out      []diag.Diagnostic
}

// visible is the "everything declared" limit for a context outside the block: by
// the time any task renders, the whole block has resolved.
const visible = 1 << 30

// tasks recurses over the task list, including block: children.
//
// A task's SIDE is its own and is never inherited downwards:
// mergeBlockInheritance carries when/where/vars/requisites and never On, and
// since NIM-749 the side comes from the task's own module address anyway. A
// keeper-side BLOCK renders no children at all — the top-level dispatch tests
// IsKeeperTask before the block branch and hands it to renderKeeperTask, which is
// module-only (NIM-652); a keeper-side task INSIDE a block is refused outright
// (`block_on_keeper_invalid`), so neither shape reaches this walk.
func (c *computeChecker) tasks(tasks []config.Task, prefix string) {
	for i := range tasks {
		t := &tasks[i]
		where := fmt.Sprintf("%s[%d]", prefix, i)

		// `on: [covens]` — the labels resolve once per run, before any host is
		// chosen. The scalar `on: keeper` is not a list and falls through here; it
		// reads compute like any other task (NIM-619).
		if elems, ok := onListElements(t.On); ok {
			for j, s := range elems {
				c.interpolation(fmt.Sprintf("%s.on[%d]", where, j), s, cel.ComputeOutOfScopeCovenList)
			}
		}

		if t.Loop != nil {
			c.value(t.Loop.Items, where+".loop.items", cel.ComputeOutOfScopeLoopAxis)
			// loop.when: is an expression key — the whole string is CEL, no `${ }`.
			c.expression(where+".loop.when", t.Loop.When, cel.ComputeOutOfScopeLoopAxis)
		}

		// The flow-control predicates are not rendered at all: Keeper copies them
		// into the RenderedTask verbatim and they are evaluated in the Soul-side
		// sandbox (cel.NewFlowControl), which declares input/register/incarnation/
		// soulprint/vars and NOT compute. Checking a name against the compute: block
		// here would announce "the namespace exists, this name does not" about a
		// namespace that is absent — this ticket's own mistake, re-committed offline.
		c.expression(where+".when", t.When, cel.ComputeOutOfScopeFlowControl)
		c.expression(where+".changed_when", t.ChangedWhen, cel.ComputeOutOfScopeFlowControl)
		c.expression(where+".failed_when", t.FailedWhen, cel.ComputeOutOfScopeFlowControl)
		if t.Retry != nil {
			c.expression(where+".retry.until", t.Retry.Until, cel.ComputeOutOfScopeFlowControl)
		}

		// Everything below renders WITH the namespace, so what is checked there is
		// the name. `where:` belongs to this half and not the one above: it is a
		// keeper-side per-host predicate, rendered from hostVars like params:.
		c.expression(where+".where", t.Where, cel.ComputeAvailable)
		if t.Assert != nil {
			for j, that := range t.Assert.That {
				c.expression(fmt.Sprintf("%s.assert.that[%d]", where, j), that, cel.ComputeAvailable)
			}
		}
		c.value(t.Vars, where+".vars", cel.ComputeAvailable)
		if t.Module != nil {
			c.value(t.Module.Params, where+".params", cel.ComputeAvailable)
			c.captureMatch(t, where)
		}
		if t.Apply != nil {
			c.value(t.Apply.Input, where+".apply.input", cel.ComputeAvailable)
		}

		if t.Block != nil {
			c.tasks(t.Block.Block, where+".block")
		}
	}
}

// computeBlock checks the block against ITSELF: entry i resolves with entries j<i
// in scope (render.resolveCompute accumulates), so a reference to a later name is
// a forward reference that fails at run time with the very "no such key" this
// ticket is about.
func (c *computeChecker) computeBlock(block config.ComputeBlock) {
	if c.declared == nil {
		return
	}
	for i, cv := range block {
		s, ok := cv.Value.(string)
		if !ok {
			continue // a literal passes through unrendered
		}
		c.check("$.compute."+cv.Name, s, false, cel.ComputeAvailable, i)
	}
}

// captureMatch is the one param of a `core.state.<verb>` capture that the walk
// above reads wrong on its own. `match:` is an ordinary module param, so its `${ }`
// cells are substituted by the render with the namespace in scope — that half the
// generic params walk already covers. What it cannot see is the residual text:
// merge evaluates the whole predicate again, once per collection element, against
// elem/key/value and nothing else (render.Pipeline.StateOpEvaluators), so a BARE
// `compute.x` written there reads a namespace that is absent at that point.
//
// The two checks do not overlap. A whole string holding `${ … }` does not parse as
// CEL, so the expression half stays silent on exactly the cells the interpolation
// half judged.
func (c *computeChecker) captureMatch(t *config.Task, where string) {
	if _, ok := config.StateCaptureVerb(t); !ok {
		return
	}
	match, _ := t.Module.Params["match"].(string)
	c.expression(where+".params.match", match, cel.ComputeOutOfScopeStateMatch)
}

// value walks a decoded YAML value (params:/vars:/apply.input:/loop.items:) and
// tests every string it contains. Map keys are visited in sorted order so a
// scenario always produces its diagnostics in the same order — a map's range order
// would otherwise shuffle the report between runs.
func (c *computeChecker) value(v any, where string, scope cel.ComputeScope) {
	switch val := v.(type) {
	case string:
		c.interpolation(where, val, scope)
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c.value(val[k], where+"."+k, scope)
		}
	case []any:
		for i, e := range val {
			c.value(e, fmt.Sprintf("%s[%d]", where, i), scope)
		}
	case []string:
		for i, s := range val {
			c.interpolation(fmt.Sprintf("%s[%d]", where, i), s, scope)
		}
	}
}

func (c *computeChecker) interpolation(where, raw string, scope cel.ComputeScope) {
	c.check(where, raw, false, scope, visible)
}

func (c *computeChecker) expression(where, expr string, scope cel.ComputeScope) {
	c.check(where, expr, true, scope, visible)
}

// check is the single decision point. Out of scope → one diagnostic for the whole
// string, whatever names it holds: the namespace is what is missing, and listing
// names there would repeat the mistake the ticket was filed about. In scope → one
// diagnostic per undeclared name, deduplicated so `${ compute.x }-${ compute.x }`
// is reported once.
//
// limit is the number of block entries visible at this point: [visible] outside the
// block, the entry's own index inside it.
func (c *computeChecker) check(where, raw string, whole bool, scope cel.ComputeScope, limit int) {
	if raw == "" {
		return
	}
	if scope != cel.ComputeAvailable {
		refs := c.eng.InterpolationReferencesCompute(raw)
		if whole {
			refs = c.eng.ExpressionReferencesCompute(raw)
		}
		if refs {
			c.out = append(c.out, computeScopeDiag(c.path, where, raw, scope))
		}
		return
	}
	if c.declared == nil {
		return
	}
	names, dynamic := c.eng.InterpolationComputeNames(raw)
	if whole {
		names, dynamic = c.eng.ExpressionComputeNames(raw)
	}
	// A reference whose name is not in the source (`compute[input.k]`, `size(compute)`)
	// makes the whole cell unjudgeable, not just itself: the extractor parses with
	// macros OFF, so a comprehension variable named `compute` reads as the namespace
	// and its field as a name. `input.hosts.map(compute, compute.role)` is legal CEL
	// and would otherwise be reported as the undeclared name `role`. The unpaired
	// identifier is what both cases have in common, so it silences the name rule for
	// the cell and leaves the reading to the run.
	if dynamic {
		return
	}
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		idx, ok := c.declared[n]
		switch {
		case !ok:
			c.out = append(c.out, c.unknownNameDiag(where, raw, n))
		case idx >= limit:
			c.out = append(c.out, c.forwardRefDiag(where, raw, n))
		}
	}
}

// computeScopeDiag builds the out-of-scope diagnostic. The message says what the
// runtime error now says — the NAMESPACE is absent here, not the name — because the
// message that sent this ticket's author hunting was one naming a key ("no such
// key: topology_node_count") that was spelled perfectly.
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

// unknownNameDiag is the other half of the pair, and the one the ticket's author
// was looking for when the scope error misled them: here the namespace IS present,
// so a name that is not in it really is a misspelling.
func (c *computeChecker) unknownNameDiag(where, raw, name string) diag.Diagnostic {
	return diag.Diagnostic{
		Level: diag.LevelError,
		Phase: diag.PhaseSemanticValidate,
		File:  c.path,
		Code:  "compute_unknown_name",
		Message: fmt.Sprintf(
			"%q reads compute.%s, which the scenario never declares -- the namespace exists here, this name does not",
			raw, name),
		Hint:     c.declaredHint(),
		YAMLPath: where,
	}
}

// forwardRefDiag — the name IS declared, further down. Entries resolve in
// declaration order, so at this point it does not exist yet; the run-time symptom
// is the same "no such key" as a typo, which is why it is worth separating here.
func (c *computeChecker) forwardRefDiag(where, raw, name string) diag.Diagnostic {
	return diag.Diagnostic{
		Level: diag.LevelError,
		Phase: diag.PhaseSemanticValidate,
		File:  c.path,
		Code:  "compute_unknown_name",
		Message: fmt.Sprintf(
			"%q reads compute.%s, which is declared LATER in the compute: block -- entries resolve in declaration order, so it has no value yet",
			raw, name),
		Hint:     fmt.Sprintf("move %s above this entry, or read it from a task instead (the whole block has resolved by then)", name),
		YAMLPath: where,
	}
}

// declaredHint lists what the scenario does declare, capped: a block with fifty
// entries would otherwise push the actual message off the terminal.
func (c *computeChecker) declaredHint() string {
	if len(c.declared) == 0 {
		return "the scenario has no compute: block -- add one, or drop the reference"
	}
	names := make([]string, 0, len(c.declared))
	for n := range c.declared {
		names = append(names, n)
	}
	sort.Strings(names)
	const maxNames = 8
	suffix := ""
	if len(names) > maxNames {
		names, suffix = names[:maxNames], ", ..."
	}
	return "declared compute: entries are " + strings.Join(names, ", ") + suffix
}
