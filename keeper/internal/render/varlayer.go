package render

import (
	"errors"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// ErrVarUnknownRef — a var value references, via `${ vars.<X> }`, a name <X> that
// doesn't exist in the SAME layer (file-level or task-level). EAGER marker: the
// error is raised while building the layer's dependency graph, even if the
// referencing var is never used afterward. Failing early beats a silent
// no-such-key in an unused branch: a broken cross-reference is an author typo, not
// a valid "deferred" var. The message carries var_unknown_ref as a stable marker
// (expect_render_error trials / log grep).
var ErrVarUnknownRef = errors.New("render: var_unknown_ref")

// ErrVarCycle — a cyclic var→var dependency within one layer (a → b → … → a). The
// error message carries the cycle TRACE (`a → b → c → a`) for the author to
// debug. A cycle is unresolvable by topological sort (Kahn): after removing all
// zero-in-degree nodes, only cycle nodes remain in acc.
var ErrVarCycle = errors.New("render: var_cycle")

// resolveVarLayer resolves ONE layer of `vars.*` variables (either file-level
// vars.yml OR task-level `vars:`) with support for var→var references WITHIN the
// layer (eager-topological). Mirrors resolveCompute (compute.go): resolves in
// dependency topological order, accumulating the result into base.Vars, so a var
// declared later can see an earlier-computed var of the same layer.
//
// The dependency graph is built via engine.VarRefs on each string value (AST
// walk, not regex): `${ vars.X }` → edge current-var → X. A reference to a name
// absent from raw → [ErrVarUnknownRef] (eager, even for an unused var). A cycle →
// [ErrVarCycle] with a trace. Non-string values pass through as literals and
// contribute no edges: a `${ … }` nested INSIDE a map or list var is not resolved,
// it survives as its own text ([destiny/vars.md] §"Valid value types"). This is
// NOT what renderValue does to `params:` — that one descends and evaluates every
// string it finds — so an author who wants an interpolated collection writes the
// whole var as one CEL expression rather than a YAML literal with cells in it.
//
// [destiny/vars.md]: docs/destiny/vars.md
//
// ISOLATION (CRITICAL): var→var reaches its own layer and the layers BELOW it,
// never sideways or upward. `lower` carries the already-resolved layers this one
// sits on top of — for a task's `vars:` that is the SERVICE's own vars
// (ADR-0082), which are the bottom of the flat `vars.*` ladder; nil for a
// destiny's `vars.yml`, which sits on nothing.
//
// Downward references are what makes the merged namespace usable: before
// ADR-0082 a task var reached the service layer by spelling it `${ vars.X }`,
// a different root that was always in scope. With one root the same expression
// is `${ vars.X }`, and refusing it would turn a working scenario into
// var_unknown_ref for no reason an author could act on.
//
// SIDEWAYS is still refused, unchanged: a task-var cannot see a file-var and
// vice versa ([destiny/vars.md]), because `lower` for the task layer is the
// service layer alone — the file layer is merged in by the CALLER, after this
// returns. base carries the rest of the resolve context
// (input/soulprint.self/incarnation); the restricted env
// (register/soulprint.hosts) is NOT relaxed — it is determined by base itself,
// which the caller builds isolated.
//
// [destiny/vars.md]: docs/destiny/vars.md
func resolveVarLayer(engine *cel.Engine, raw map[string]any, lower map[string]any, base cel.Vars) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	// deps[name] — names in the same layer that name's value references (edges
	// name → dep). A reference to a nonexistent name → eager ErrVarUnknownRef.
	deps := make(map[string][]string, len(raw))
	for name, val := range raw {
		s, ok := val.(string)
		if !ok {
			continue // literal — contributes no edges
		}
		refs, err := engine.VarRefs(s)
		if err != nil {
			return nil, fmt.Errorf("render: vars.%s: %w", name, err)
		}
		var sameLayer []string
		for _, ref := range refs {
			_, below := lower[ref]

			// SHADOW-AND-DERIVE. A var that references ITS OWN name is reading the
			// layer below — it is redefining that name in terms of the value it is
			// shadowing, which is the single most natural way to use a merged
			// namespace (`conf_dir: "${ vars.conf_dir }/conf.d"`). Under the two-root
			// world it was spelled `${ essence.conf_dir }` and needed no rule at all.
			// Treating it as a same-layer edge cannot ever resolve, and reports a
			// var_cycle naming one node — an error about a cycle the author cannot
			// find, on the exact shape the merge exists to enable.
			if ref == name && below {
				continue
			}

			if _, exists := raw[ref]; exists {
				sameLayer = append(sameLayer, ref) // an edge: resolve order matters
				continue
			}
			if below {
				continue // already resolved, no edge to order against
			}
			return nil, fmt.Errorf("%w: vars.%s references vars.%s, which is in neither this layer nor the one below it", ErrVarUnknownRef, name, ref)
		}
		deps[name] = sameLayer
	}

	order, cycle := topoSort(raw, deps)
	if cycle != nil {
		return nil, fmt.Errorf("%w: %s", ErrVarCycle, strings.Join(cycle, " → "))
	}

	// visible = the layers below + this layer's accumulator. A var in topo order
	// sees both the lower layers and the earlier-computed vars of its own.
	visible := make(map[string]any, len(lower)+len(raw))
	for k, v := range lower {
		visible[k] = v
	}
	acc := make(map[string]any, len(raw))
	base.Vars = visible
	for _, name := range order {
		val := raw[name]
		s, ok := val.(string)
		if !ok {
			acc[name] = val // literal — passes through
			visible[name] = val
			continue
		}
		r, err := engine.EvalInterpolation(s, base)
		if err != nil {
			return nil, fmt.Errorf("render: vars.%s: %w", name, err)
		}
		acc[name] = r
		visible[name] = r
	}
	// Only this layer's own names: the caller decides how they stack.
	return acc, nil
}

// topoSort orders layer names so each name comes AFTER the names it references
// (deps[name]) — a resolve order where a referencing var sees already-computed
// dependencies. The algorithm is Kahn's by in-degree; nodes with equal degree are
// taken in lexicographic order for determinism (YAML key order doesn't matter —
// case #7).
//
// Returns cycle != nil if the graph isn't acyclic: cycle is the trace of one
// cycle (`a → b → c → a`, closed by repeating the starting node) for ErrVarCycle.
// A self-reference (a→a) is a special case of a cycle, trace `a → a`.
func topoSort(raw map[string]any, deps map[string][]string) (order []string, cycle []string) {
	// remaining[name] — the count of its OUTGOING unresolved dependencies: a node
	// is ready to resolve once all its deps are already in order (Kahn by out-degree).
	remaining := make(map[string]int, len(raw))
	// dependents[dep] — who references dep (reverse edges), so that once dep is
	// ready we can decrement its dependents' counters.
	dependents := make(map[string][]string, len(raw))
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
		remaining[name] = len(deps[name])
		for _, d := range deps[name] {
			dependents[d] = append(dependents[d], name)
		}
	}

	// Queue of ready nodes (remaining==0), we pull the lexicographically smallest —
	// determinism when equally ready. Small layers (units to tens of vars) → a
	// linear minimum search is cheaper and simpler than a heap.
	resolved := make(map[string]bool, len(raw))
	for len(order) < len(raw) {
		next := ""
		for _, name := range names {
			if resolved[name] || remaining[name] != 0 {
				continue
			}
			if next == "" || name < next {
				next = name
			}
		}
		if next == "" {
			// No ready nodes, but not everything is resolved → the remainder forms a cycle.
			return nil, traceCycle(names, deps, resolved)
		}
		resolved[next] = true
		order = append(order, next)
		for _, dep := range dependents[next] {
			remaining[dep]--
		}
	}
	return order, nil
}

// traceCycle builds a human-readable trace of one cycle among the still-unresolved
// nodes (resolved[x]==false). Walks deps edges from the first unresolved node
// until hitting one already visited in this walk — the segment from its first
// appearance to the repeat is the cycle (closed by repeating the starting element).
func traceCycle(names []string, deps map[string][]string, resolved map[string]bool) []string {
	// Start from the lexicographically smallest unresolved node — trace determinism
	// (case #7: YAML order must not affect the error text).
	start := ""
	for _, n := range names {
		if !resolved[n] {
			if start == "" || n < start {
				start = n
			}
		}
	}
	pos := make(map[string]int)
	var path []string
	cur := start
	for {
		if i, seen := pos[cur]; seen {
			return append(path[i:], cur) // close by repeating
		}
		pos[cur] = len(path)
		path = append(path, cur)
		// The next cycle node is the first unresolved dependency (deterministically
		// the smallest among deps leading back into the cycle).
		nextHop := ""
		for _, d := range deps[cur] {
			if resolved[d] {
				continue
			}
			if nextHop == "" || d < nextHop {
				nextHop = d
			}
		}
		if nextHop == "" {
			return path // safety: ran out of deps (shouldn't happen for a cycle)
		}
		cur = nextHop
	}
}
