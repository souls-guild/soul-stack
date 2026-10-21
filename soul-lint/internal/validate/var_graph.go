package validate

// The offline var checks of ADR-0082 amendment 2026-10-15, point 12a (NIM-1014).
// The Keeper evaluates a var only when something reads it, so a broken var nobody
// reads no longer fails a run; these checks are where it fails instead, once per
// edit. Every view an artifact forms — the rungs of point 2, lowest first — is walked
// in stacks the Keeper builds for some incarnation ([varView.eachSelection]), and
// inside one stack a read resolves as the lazy view of shared/cel (varview.go)
// resolves it: to the name's top winner under late binding (point 5), a
// self-reference to the definitions below the layer that wrote it (point 9). A node
// is a (name, layer bound) pair, so a cycle is reported in the text the Keeper raises
// for it (point 10).

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/servicevars"
)

const (
	codeVarCycle      = "var_cycle"
	codeVarUnknownRef = "var_unknown_ref"
	// codeVarInvalid is pending the user's confirmation of its name.
	codeVarInvalid = "var_invalid"
)

// varLayer is one layer of a view: a file of the service rung, the scenario's
// `vars:`, a destiny's `vars.yml`, or one task's block + task `vars:`.
type varLayer struct {
	rung cel.VarRung
	// origin names the layer as the Keeper names it in an error.
	origin   string
	defs     map[string]any
	strategy map[string]string
	// conditional: some incarnation's assembly leaves the layer out; oneOf: the layer
	// is one of the files a one-of step names, so no incarnation stacks it beside
	// another of them ([servicevars.StaticLayer]).
	conditional, oneOf bool
	// owned: this command reports the layer's own findings. A scenario's views carry
	// the service rung, whose findings belong to validate-service.
	owned bool
	// site locates a name's definition in its file.
	site func(name string) varSite
	// engine and scope are the environment the layer's definitions compile in.
	engine *cel.Engine
	scope  cel.Vars
}

// varSite is where a definition is written: its file, its YAML path there, and the
// resolver of a path to a line.
type varSite struct {
	file     string
	yamlPath string
	position func(yamlPath string) (line, column int, ok bool)
}

// varView is the layers one context reads, lowest first. lowerUnknown: a rung below
// them exists but was not read — a scenario linted without its service, a service
// rung that does not assemble, a vars.yml that does not parse — so finding nothing
// below a self-reference proves nothing. perLayer: the view is walked in a selection
// per conditional layer too, which only validate-service's own view is.
type varView struct {
	layers       []*varLayer
	lowerUnknown bool
	perLayer     bool
}

// varNode is a name as the layers up to bound define it: the key of the Keeper's
// memo and of its evaluation path.
type varNode struct {
	name  string
	bound int
}

// varLeaf is one string of a definition and the layer that contributed it; sub is
// its YAML path below the name.
type varLeaf struct {
	layer int
	sub   string
	raw   string
}

// varChecker runs the checks over the views of one artifact. A layer is checked
// for unknown references and for compiling once, in the first view that carries it;
// diagnostics repeated across views are reported once.
type varChecker struct {
	// unknownRefs is off when the rungs below a definition are not known, so an
	// absent name proves nothing.
	unknownRefs bool
	reads       map[string]stringReads
	checked     map[*varLayer]bool
	seen        map[string]bool
	// flagged holds the definitions, by [flagKey], an ERROR was reported for, and
	// rings the cycles, by [ringKey]: the stub evaluation does not report them again.
	flagged map[string]bool
	rings   map[string]bool
	// viewRings are the rings the view being walked has reported, whichever of its
	// selections found them.
	viewRings map[string]bool
	work      varWork
	out       []diag.Diagnostic
}

// varWork is what the cycle search did: the measure its guards hold it to.
type varWork struct {
	selections, nodes, edges, cells int
	// rings are the rings walked in each group, in the order the groups were searched.
	rings []int
}

func newVarChecker(unknownRefs bool) *varChecker {
	return &varChecker{
		unknownRefs: unknownRefs,
		reads:       map[string]stringReads{},
		checked:     map[*varLayer]bool{},
		seen:        map[string]bool{},
		flagged:     map[string]bool{},
		rings:       map[string]bool{},
		viewRings:   map[string]bool{},
	}
}

// flagKey is a definition as the Keeper names it in a [cel.VarError]: its name and
// its layer's origin.
func flagKey(name, origin string) string { return name + "\x00" + origin }

// stringReads is what one string of a definition reads off `vars`: every name it
// evaluates; the names some `${ … }` block reads without testing has() or `in` on
// them; the names some block reads outside a key test on them that holds; and
// whether a block reads the namespace whole.
type stringReads struct {
	all, strict []string
	// unkeyed: a read of the definition's own name with nothing below is a cycle
	// only through one of these. Where `'x' in vars` holds, something below defines x.
	unkeyed map[string]bool
	whole   bool
}

// readsOf parses a string once for every selection and view that holds it.
func (c *varChecker) readsOf(raw string) stringReads {
	if r, ok := c.reads[raw]; ok {
		return r
	}
	var r stringReads
	if strings.Contains(raw, "${") {
		if segs, ok := servicevars.SplitInterpolation(raw); ok {
			all, strict, unkeyed := map[string]bool{}, map[string]bool{}, map[string]bool{}
			for _, s := range segs {
				if !s.Expr {
					continue
				}
				reads, whole := servicevars.ExprReads(s.Text)
				r.whole = r.whole || whole
				for _, read := range reads {
					all[read.Name] = true
					if !read.Guarded {
						strict[read.Name] = true
					}
					if !read.KeyTested {
						unkeyed[read.Name] = true
					}
				}
			}
			r.all, r.strict, r.unkeyed = slices.Sorted(maps.Keys(all)), slices.Sorted(maps.Keys(strict)), unkeyed
		}
	}
	c.reads[raw] = r
	return r
}

func (c *varChecker) check(v *varView) {
	c.findCycles(v)
	for i, l := range v.layers {
		if !l.owned || c.checked[l] {
			continue
		}
		c.checked[l] = true
		if c.unknownRefs {
			c.findUnknownRefs(v, i)
		}
		c.compile(l)
	}
}

func (v *varView) top() int { return len(v.layers) - 1 }

// varSelection is one stack of a view's layers: by lists, per name, the layers of
// the stack that define it, ascending, and extra is one more layer, -1 for none.
type varSelection struct {
	v     *varView
	by    map[string][]int
	extra int
	// names are every name the stack defines, sorted.
	names []string
}

// eachSelection walks v's selections, the stacks the Keeper builds for some
// incarnation, in this order: S_all, every layer but the files of one-of steps;
// S_base, the always-stacked layers alone; then, in a perLayer view, for each
// conditional layer L in stack order, S_L, the always-stacked layers and L. A
// selection that is the same stack as an earlier one is not walked again. A layer is
// conditional when its step has a `when:` or a `foreach:` or `file:` that reads
// `incarnation.*`; an `optional:` step whose file exists and a constant `foreach:`
// with a constant `file:` are always stacked. A one-of step's composed `file:`
// without `foreach:` names at most one file per incarnation, so its files are
// alternatives: S_all stacks none of them, and each is walked in its own S_L.
//
// So a cycle that needs a particular set of two or more conditional layers, short
// of all of them, is not found — the stub evaluation of validate-service builds the
// stack its own incarnation selects. And mutually exclusive `when:` conditions are
// not decided, so S_all may be a stack no incarnation builds.
func (v *varView) eachSelection(visit func(*varSelection)) {
	every, always := map[string][]int{}, map[string][]int{}
	var conditional []int
	// stacked counts the conditional layers S_all stacks.
	stacked := 0
	for i, l := range v.layers {
		for name := range l.defs {
			if !l.oneOf {
				every[name] = append(every[name], i)
			}
			if !l.conditional {
				always[name] = append(always[name], i)
			}
		}
		if l.conditional {
			conditional = append(conditional, i)
			if !l.oneOf {
				stacked++
			}
		}
	}
	visit(&varSelection{v: v, by: every, extra: -1, names: slices.Sorted(maps.Keys(every))})
	alwaysNames := slices.Sorted(maps.Keys(always))
	if stacked > 0 {
		visit(&varSelection{v: v, by: always, extra: -1, names: alwaysNames})
	}
	if !v.perLayer {
		return
	}
	for _, l := range conditional {
		if stacked == 1 && !v.layers[l].oneOf {
			continue
		}
		names := slices.Concat(alwaysNames, slices.Collect(maps.Keys(v.layers[l].defs)))
		slices.Sort(names)
		visit(&varSelection{v: v, by: always, extra: l, names: slices.Compact(names)})
	}
}

// definers are the layers of the stack at or below bound that define name, ascending.
func (s *varSelection) definers(name string, bound int) []int {
	idx := s.by[name]
	out := idx[:sort.SearchInts(idx, bound+1)]
	if s.extra < 0 || s.extra > bound {
		return out
	}
	if _, ok := s.v.layers[s.extra].defs[name]; !ok {
		return out
	}
	at := sort.SearchInts(out, s.extra)
	return slices.Insert(slices.Clip(out), at, s.extra)
}

// winner is the top layer of the stack at or below bound that defines name, or -1:
// the Keeper's bound for a read of name there.
func (s *varSelection) winner(name string, bound int) int {
	w := -1
	if idx := s.by[name]; len(idx) > 0 {
		if i := sort.SearchInts(idx, bound+1) - 1; i >= 0 {
			w = idx[i]
		}
	}
	if s.extra > w && s.extra <= bound {
		if _, ok := s.v.layers[s.extra].defs[name]; ok {
			w = s.extra
		}
	}
	return w
}

// lintCell is a name's raw definition as the layers of a stack combine it, as the
// Keeper's varCell is: a map merged from two layers keeps a cell per key, anything
// else is raw, contributed whole by layer.
type lintCell struct {
	layer  int
	raw    any
	fields map[string]*lintCell
}

func mergeLintCells(lower, upper *lintCell) *lintCell {
	lf, lok := lower.mapFields()
	uf, uok := upper.mapFields()
	if !lok || !uok {
		return upper
	}
	out := make(map[string]*lintCell, len(lf)+len(uf))
	maps.Copy(out, lf)
	for k, c := range uf {
		if l, ok := out[k]; ok {
			c = mergeLintCells(l, c)
		}
		out[k] = c
	}
	return &lintCell{layer: upper.layer, fields: out}
}

func (c *lintCell) mapFields() (map[string]*lintCell, bool) {
	if c.fields != nil {
		return c.fields, true
	}
	m, ok := c.raw.(map[string]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]*lintCell, len(m))
	for k, x := range m {
		out[k] = &lintCell{layer: c.layer, raw: x}
	}
	return out, true
}

// strings visits every string the cell's evaluation reads, with the layer that
// contributed it.
func (c *lintCell) strings(visit func(layer int, raw string)) {
	if c.fields == nil {
		rawStrings(c.raw, c.layer, visit)
		return
	}
	for _, k := range slices.Sorted(maps.Keys(c.fields)) {
		c.fields[k].strings(visit)
	}
}

// leaves lists the strings of [lintCell.strings], in its order, with their paths
// below the name.
func (c *lintCell) leaves(sub string, out []varLeaf) []varLeaf {
	if c.fields == nil {
		return rawLeaves(c.raw, c.layer, sub, out)
	}
	for _, k := range slices.Sorted(maps.Keys(c.fields)) {
		out = c.fields[k].leaves(sub+"."+k, out)
	}
	return out
}

func rawStrings(x any, layer int, visit func(layer int, raw string)) {
	switch t := x.(type) {
	case string:
		visit(layer, t)
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			rawStrings(t[k], layer, visit)
		}
	case []any:
		for _, el := range t {
			rawStrings(el, layer, visit)
		}
	}
}

// rawLeaves lists every string of a raw definition, at any depth (point 1).
func rawLeaves(x any, layer int, sub string, out []varLeaf) []varLeaf {
	switch t := x.(type) {
	case string:
		return append(out, varLeaf{layer: layer, sub: sub, raw: t})
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			out = rawLeaves(t[k], layer, sub+"."+k, out)
		}
	case []any:
		for i, el := range t {
			out = rawLeaves(el, layer, fmt.Sprintf("%s[%d]", sub, i), out)
		}
	}
	return out
}

// varWalk is the reference graph of one selection: its nodes by id, discovered from
// each name's top winner, the edges between them, and each node's cell.
type varWalk struct {
	c     *varChecker
	v     *varView
	sel   *varSelection
	ids   map[varNode]int32
	nodes []varNode
	adj   [][]int32
	// done: the node's edges are computed; mark: the node whose edges last took it.
	done  []bool
	mark  []int32
	order []int32
	tops  []int32
	cells map[varNode]*lintCell
}

func newVarWalk(c *varChecker, v *varView, sel *varSelection) *varWalk {
	tops := make([]int32, len(sel.names))
	for i := range tops {
		tops[i] = -1
	}
	return &varWalk{c: c, v: v, sel: sel, ids: map[varNode]int32{}, tops: tops, cells: map[varNode]*lintCell{}}
}

func (w *varWalk) node(n varNode) int32 {
	if id, ok := w.ids[n]; ok {
		return id
	}
	id := int32(len(w.nodes))
	w.ids[n] = id
	w.nodes = append(w.nodes, n)
	w.adj = append(w.adj, nil)
	w.done = append(w.done, false)
	w.mark = append(w.mark, -1)
	return id
}

// top is the node a read of the i-th name of the stack resolves to.
func (w *varWalk) top(i int) int32 {
	if w.tops[i] < 0 {
		name := w.sel.names[i]
		w.tops[i] = w.node(varNode{name: name, bound: w.sel.winner(name, w.v.top())})
	}
	return w.tops[i]
}

// add computes the edges of node id and of every node they reach.
func (w *varWalk) add(id int32) {
	if w.done[id] {
		return
	}
	w.done[id] = true
	w.order = append(w.order, id)
	w.expand(id)
	for _, t := range w.adj[id] {
		w.add(t)
	}
}

// cell assembles n as the Keeper's assemble does: within one rung a map recurses and
// anything else replaces, a `replace` strategy or a new rung replaces the name
// whole. Each prefix of a name's definers is assembled once per selection.
func (w *varWalk) cell(n varNode) *lintCell {
	if c, ok := w.cells[n]; ok {
		return c
	}
	defs := w.sel.definers(n.name, n.bound)
	from := len(defs) - 1
	var acc *lintCell
	for ; from > 0; from-- {
		if c, ok := w.cells[varNode{name: n.name, bound: defs[from-1]}]; ok {
			acc = c
			break
		}
	}
	for _, j := range defs[from:] {
		l := w.v.layers[j]
		c := &lintCell{layer: j, raw: l.defs[n.name]}
		if acc != nil && l.rung == w.v.layers[acc.layer].rung && l.strategy[n.name] != config.StrategyReplace {
			c = mergeLintCells(acc, c)
		}
		acc = c
		w.cells[varNode{name: n.name, bound: j}] = acc
		w.c.work.cells++
	}
	return acc
}

// expand computes the nodes id's evaluation may read. A read of another name goes to
// its top winner. A read of the node's own name is a self-reference and goes to the
// top definition below the layer of the cell that wrote it; with none, it is a
// cycle unless a key test guards the read or the rungs below are unknown. A whole
// read of `vars` reads every name of the stack, its own below it included (point
// 13): the Keeper materializes the view to answer it.
func (w *varWalk) expand(id int32) {
	n := w.nodes[id]
	edge := func(t int32) {
		if w.mark[t] != id {
			w.mark[t] = id
			w.adj[id] = append(w.adj[id], t)
			w.c.work.edges++
		}
	}
	w.cell(n).strings(func(layer int, raw string) {
		r := w.c.readsOf(raw)
		self := func() {
			if below := w.sel.winner(n.name, layer-1); below >= 0 {
				edge(w.node(varNode{name: n.name, bound: below}))
			} else if !w.v.lowerUnknown && (r.whole || r.unkeyed[n.name]) {
				w.c.reportCycle(w.v, []varNode{{name: n.name, bound: layer - 1}}, true, func(int) varLeaf {
					return w.selfReader(n, layer)
				})
			}
		}
		if r.whole {
			for i, name := range w.sel.names {
				if name == n.name {
					self()
				} else {
					edge(w.top(i))
				}
			}
			return
		}
		for _, name := range r.all {
			if name == n.name {
				self()
			} else if t := w.sel.winner(name, w.v.top()); t >= 0 {
				edge(w.node(varNode{name: name, bound: t}))
			}
		}
	})
}

// readerOf is the first string of from's cell, in [lintCell.strings] order, whose
// reads take the edge to to: where a ring through that edge is cited and can be
// broken. In a map merged deep it can be a lower layer's than from's bound.
func (w *varWalk) readerOf(from, to varNode) varLeaf {
	for _, leaf := range w.cell(from).leaves("", nil) {
		r := w.c.readsOf(leaf.raw)
		if !r.whole && !slices.Contains(r.all, to.name) {
			continue
		}
		if to.name != from.name || w.sel.winner(to.name, leaf.layer-1) == to.bound {
			return leaf
		}
	}
	return varLeaf{layer: from.bound}
}

// selfReader is the first string of n's cell at layer that reads n's own name with
// nothing below: where that one-node cycle is cited.
func (w *varWalk) selfReader(n varNode, layer int) varLeaf {
	for _, leaf := range w.cell(n).leaves("", nil) {
		if r := w.c.readsOf(leaf.raw); leaf.layer == layer && (r.whole || r.unkeyed[n.name]) {
			return leaf
		}
	}
	return varLeaf{layer: layer}
}

// maxCyclesPerGroup bounds the rings the search walks in one strongly-connected
// group of one selection, reported or not: a group of k definitions that each read
// `vars` whole holds factorially many.
const maxCyclesPerGroup = 16

// findCycles walks every selection of v and reports every self-reference with
// nothing below it and every distinct ring among the first maxCyclesPerGroup the
// search walks in each group of nodes that reach one another. Inside one selection a
// read has one winner, so a ring passes a name more than once only through
// self-references, as the Keeper's evaluation path does. A ring is reported once per
// view, at the first selection that finds it. Which ring the Keeper raises depends
// on its evaluation order and its incarnation, which the text does not decide, and
// the bound can leave it unwalked in a large group.
func (c *varChecker) findCycles(v *varView) {
	c.viewRings = map[string]bool{}
	v.eachSelection(func(sel *varSelection) {
		c.work.selections++
		w := newVarWalk(c, v, sel)
		for i := range sel.names {
			w.add(w.top(i))
		}
		c.work.nodes += len(w.nodes)
		for _, group := range stronglyConnected(w.order, w.adj) {
			if len(group) < 2 && !slices.Contains(w.adj[group[0]], group[0]) {
				continue
			}
			walked := 0
			w.elementaryCycles(group, maxCyclesPerGroup, func(ring []varNode) {
				walked++
				c.reportCycle(v, ring, false, func(i int) varLeaf {
					return w.readerOf(ring[i], ring[(i+1)%len(ring)])
				})
			})
			c.work.rings = append(c.work.rings, walked)
		}
	})
}

func compareNodes(a, b varNode) int {
	if c := strings.Compare(a.name, b.name); c != 0 {
		return c
	}
	return a.bound - b.bound
}

// stronglyConnected is Tarjan's algorithm over the nodes in order, of a graph whose
// nodes are 0..len(adj)-1.
func stronglyConnected(order []int32, adj [][]int32) [][]int32 {
	return tarjan(order, adj, func(int32) bool { return false })
}

// tarjan is [stronglyConnected] over the nodes skip does not leave out.
func tarjan(order []int32, adj [][]int32, skip func(int32) bool) [][]int32 {
	index, low := make([]int32, len(adj)), make([]int32, len(adj))
	for i := range index {
		index[i] = -1
	}
	onStack := make([]bool, len(adj))
	var stack []int32
	var out [][]int32
	next := int32(0)
	var strong func(n int32)
	strong = func(n int32) {
		index[n], low[n] = next, next
		next++
		stack = append(stack, n)
		onStack[n] = true
		for _, t := range adj[n] {
			switch {
			case skip(t):
			case index[t] < 0:
				strong(t)
				low[n] = min(low[n], low[t])
			case onStack[t]:
				low[n] = min(low[n], index[t])
			}
		}
		if low[n] != index[n] {
			return
		}
		var group []int32
		for {
			t := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[t] = false
			group = append(group, t)
			if t == n {
				break
			}
		}
		out = append(out, group)
	}
	for _, n := range order {
		if index[n] < 0 && !skip(n) {
			strong(n)
		}
	}
	return out
}

// elementaryCycles is Johnson's algorithm inside one group: it hands report every
// ring once, in evaluation order and without its closing repeat, and stops after
// limit of them. The nodes are ranked by name, then bound. Each round starts at the
// least node of a strongly-connected part of the nodes not yet started from, so it
// walks at least one ring and there are at most limit rounds. A path closes to the
// start before it goes deeper.
func (w *varWalk) elementaryCycles(group []int32, limit int, report func([]varNode)) {
	sorted := slices.SortedFunc(slices.Values(group), func(a, b int32) int { return compareNodes(w.nodes[a], w.nodes[b]) })
	rank := make(map[int32]int32, len(sorted))
	for i, id := range sorted {
		rank[id] = int32(i)
	}
	size := len(sorted)
	adj := make([][]int32, size)
	order := make([]int32, size)
	for i, id := range sorted {
		order[i] = int32(i)
		for _, t := range w.adj[id] {
			if r, ok := rank[t]; ok {
				adj[i] = append(adj[i], r)
			}
		}
	}
	part := make([]bool, size)
	closes := make([]bool, size)
	blocked := make([]bool, size)
	blockedBy := make([][]int32, size)
	var path []int32
	counted := 0
	for lo := int32(0); int(lo) < size && counted < limit; lo++ {
		var start []int32
		for _, scc := range tarjan(order[lo:], adj, func(t int32) bool { return t < lo }) {
			if len(scc) > 1 && (start == nil || slices.Min(scc) < slices.Min(start)) {
				start = scc
			}
		}
		if start == nil {
			break
		}
		s := slices.Min(start)
		for _, n := range start {
			part[n], blocked[n], blockedBy[n] = true, false, nil
		}
		for _, n := range start {
			closes[n] = slices.Contains(adj[n], s)
		}
		var unblock func(u int32)
		unblock = func(u int32) {
			blocked[u] = false
			waiting := blockedBy[u]
			blockedBy[u] = nil
			for _, t := range waiting {
				if blocked[t] {
					unblock(t)
				}
			}
		}
		var circuit func(n int32) bool
		circuit = func(n int32) bool {
			found := false
			path = append(path, n)
			blocked[n] = true
			if closes[n] && counted < limit {
				ring := make([]varNode, len(path))
				for i, r := range path {
					ring[i] = w.nodes[sorted[r]]
				}
				report(ring)
				counted++
				found = true
			}
			for _, t := range adj[n] {
				if counted >= limit {
					break
				}
				if t != s && part[t] && !blocked[t] && circuit(t) {
					found = true
				}
			}
			if found {
				unblock(n)
			} else {
				for _, t := range adj[n] {
					if part[t] {
						blockedBy[t] = append(blockedBy[t], n)
					}
				}
			}
			path = path[:len(path)-1]
			return found
		}
		circuit(s)
		for _, n := range start {
			part[n], closes[n], blockedBy[n] = false, false, nil
		}
		lo = s
	}
}

// reportCycle renders ring — a cycle without its closing repeat, in evaluation
// order — as the Keeper does (shared/cel newVarCycleError): rotated to its smallest
// name, at that name's lowest bound when it occurs more than once, so a ring names a
// name once per layer it passes, as the Keeper does. self: ring is a one-node cycle
// whose node no layer defines. reader(i) is the string of ring[i] that reads the
// next node, or for self the one that reads its own name.
func (c *varChecker) reportCycle(v *varView, ring []varNode, self bool, reader func(i int) varLeaf) {
	start := 0
	for i, k := range ring {
		s := ring[start]
		if k.name < s.name || (k.name == s.name && k.bound < s.bound) {
			start = i
		}
	}
	rotated := append(slices.Clone(ring[start:]), ring[:start]...)
	names := make([]string, 0, len(rotated)+1)
	for _, k := range rotated {
		names = append(names, k.name)
	}
	key := ringKey(names)
	if c.viewRings[key] {
		return
	}
	text := (&cel.VarCycleError{Path: append(slices.Clone(names), rotated[0].name)}).Error()

	at := make([]varLeaf, len(rotated))
	for i := range at {
		at[i] = reader((start + i) % len(ring))
	}
	// Cited at the first definition of the ring this command owns: a cycle a task
	// closes through a service var is the scenario's to report.
	cite := -1
	for i, leaf := range at {
		if v.layers[leaf.layer].owned {
			cite = i
			break
		}
	}
	if cite < 0 {
		return
	}
	c.viewRings[key] = true
	c.rings[key] = true
	var where []string
	for i, k := range rotated {
		l := v.layers[at[i].layer]
		c.flagged[flagKey(k.name, l.origin)] = true
		where = append(where, fmt.Sprintf("vars.%s (%s)", k.name, l.origin))
	}
	hint := "break the chain at one of: " + strings.Join(slices.Compact(where), ", ")
	if self {
		hint = "a self-reference reads the definition below its own layer, and nothing below defines it"
	}
	c.emit(v.layers[at[cite].layer].site(rotated[cite].name), at[cite].sub, codeVarCycle, text, hint)
}

// ringKey is a ring by its names alone, without the closing repeat, at the rotation
// that compares smallest: one key for the ring whichever layer of a name it names
// more than once the rendering starts at, which depends on the order the layers are
// stacked in.
func ringKey(names []string) string {
	best := names
	for i := 1; i < len(names); i++ {
		if r := append(slices.Clone(names[i:]), names[:i]...); slices.Compare(r, best) < 0 {
			best = r
		}
	}
	return strings.Join(best, "\x00")
}

// findUnknownRefs reports, for the definitions of layer i, a read no layer of its
// own rung or a rung below defines. A read guarded by has() on the same root name
// in the same block is an optional-override hook, not a need.
func (c *varChecker) findUnknownRefs(v *varView, i int) {
	l := v.layers[i]
	for _, name := range slices.Sorted(maps.Keys(l.defs)) {
		for _, leaf := range rawLeaves(l.defs[name], i, "", nil) {
			for _, ref := range c.readsOf(leaf.raw).strict {
				if ref == name || definedAtOrBelow(v, ref, l.rung) {
					continue
				}
				c.flagged[flagKey(name, l.origin)] = true
				c.emit(l.site(name), leaf.sub, codeVarUnknownRef,
					fmt.Sprintf("vars.%s (%s) reads vars.%s, which neither its own rung nor a rung below it defines", name, l.origin, ref),
					fmt.Sprintf("define %s at or below this rung, or make the read optional: default(vars.%s, …) or has(vars.%s) ? … : …", ref, ref, ref))
			}
		}
	}
}

func definedAtOrBelow(v *varView, name string, rung cel.VarRung) bool {
	for _, l := range v.layers {
		if _, ok := l.defs[name]; ok && l.rung <= rung {
			return true
		}
	}
	return false
}

// compile reports a definition that does not compile in its rung's environment. Each
// block is evaluated over an empty activation and only an evaluation error is let
// through: every refusal the engine makes before it runs a program is a compile one.
func (c *varChecker) compile(l *varLayer) {
	if l.engine == nil {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(l.defs)) {
		for _, leaf := range rawLeaves(l.defs[name], 0, "", nil) {
			if err := compileErr(l.engine, leaf.raw, l.scope); err != nil {
				c.flagged[flagKey(name, l.origin)] = true
				c.emit(l.site(name), leaf.sub, codeVarInvalid,
					fmt.Sprintf("vars.%s (%s) does not compile in the %s environment: %v", name, l.origin, rungEnvironment(l.rung), err),
					rungEnvironmentHint(l.rung))
			}
		}
	}
}

func compileErr(engine *cel.Engine, raw string, scope cel.Vars) error {
	if !strings.Contains(raw, "${") {
		return nil
	}
	segs, ok := servicevars.SplitInterpolation(raw)
	if !ok {
		_, err := engine.EvalInterpolation(raw, scope)
		return notEvalErr(err)
	}
	for _, s := range segs {
		if !s.Expr {
			continue
		}
		_, err := engine.EvalExpression(s.Text, scope)
		if err = notEvalErr(err); err != nil {
			return err
		}
	}
	return nil
}

func notEvalErr(err error) error {
	var ev *cel.ErrEval
	if err == nil || errors.As(err, &ev) {
		return nil
	}
	return err
}

func rungEnvironment(r cel.VarRung) string {
	switch r {
	case cel.RungService:
		return "service vars/"
	case cel.RungScenario:
		return "scenario vars:"
	case cel.RungDestinyFile:
		return "destiny vars.yml"
	default:
		return "task"
	}
}

func rungEnvironmentHint(r cel.VarRung) string {
	switch r {
	case cel.RungService:
		return "a service definition may name incarnation.* and vars.* only (ADR-0082 point 6)"
	case cel.RungScenario:
		return "a scenario vars: definition sees run data only — input.*, incarnation.*, compute.*, vars.*; " +
			"a value that depends on the host, a probe or Vault belongs in the task's own vars: (ADR-0082 point 6)"
	case cel.RungDestinyFile:
		return "a vars.yml definition may name input.*, soulprint.self.*, incarnation.* and vars.* (ADR-0082 point 6)"
	default:
		return "a task definition compiles in the environment of the task that reads it (ADR-0082 point 6)"
	}
}

// emit appends one ERROR, once: the same finding in two views is one finding.
func (c *varChecker) emit(s varSite, sub string, code, msg, hint string) {
	yamlPath := s.yamlPath + sub
	key := strings.Join([]string{code, s.file, yamlPath, msg}, "\x00")
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	d := diag.Diagnostic{
		Level:    diag.LevelError,
		Phase:    diag.PhaseSemanticValidate,
		File:     s.file,
		Code:     code,
		Message:  msg,
		Hint:     hint,
		YAMLPath: yamlPath,
	}
	if line, col, ok := nearestAddress(s.position, yamlPath); ok {
		d.Line, d.Column = line, col
	}
	c.out = append(c.out, d)
}
