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
// [ErrVarCycle] with a trace. Non-string values pass through as literals (CEL only
// touches strings, symmetric with renderValue/resolveCompute); they contribute no
// edges.
//
// ISOLATION (CRITICAL): var→var is allowed ONLY within its own layer. base
// carries the resolve context (input/soulprint.self/incarnation for file-vars;
// see the callers), and base.Vars MUST be empty at the start of the layer —
// otherwise a `vars.<X>` reference into a foreign layer (a file-var from the
// task layer or vice versa) would resolve instead of failing with an isolation
// error. CEL activation only gets the `vars` key (this layer's accumulator); the
// restricted env (register/soulprint.hosts) is NOT relaxed — it's determined by
// base itself, which the caller builds isolated.
func resolveVarLayer(engine *cel.Engine, raw map[string]any, base cel.Vars) (map[string]any, error) {
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
		for _, ref := range refs {
			if _, exists := raw[ref]; !exists {
				return nil, fmt.Errorf("%w: vars.%s references vars.%s, which is not in the layer", ErrVarUnknownRef, name, ref)
			}
		}
		deps[name] = refs
	}

	order, cycle := topoSort(raw, deps)
	if cycle != nil {
		return nil, fmt.Errorf("%w: %s", ErrVarCycle, strings.Join(cycle, " → "))
	}

	acc := make(map[string]any, len(raw))
	base.Vars = acc // this layer's accumulator; a var in topo order sees earlier-computed ones
	for _, name := range order {
		val := raw[name]
		s, ok := val.(string)
		if !ok {
			acc[name] = val // literal — passes through
			continue
		}
		r, err := engine.EvalInterpolation(s, base)
		if err != nil {
			return nil, fmt.Errorf("render: vars.%s: %w", name, err)
		}
		acc[name] = r
	}
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
