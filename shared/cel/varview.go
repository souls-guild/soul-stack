package cel

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/overloads"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// The lazy `vars.*` namespace ([ADR-0082] amendment 2026-10-15, NIM-1014). A
// [VarView] stacks the RAW definitions of every rung a context reads and evaluates a
// name only when something reads it, once per (name, layer bound). Every definition,
// whatever layer contributed it, reads `vars.<other>` through the same view — late
// binding — and keeps the environment of the layer it was written in.
//
// [ADR-0082]: docs/adr/0082-service-vars.md

// ErrVarCycle — the stable marker of a var→var cycle. [VarCycleError] matches it with
// errors.Is; its text starts with it.
var ErrVarCycle = errors.New("var_cycle")

// VarRung is the rung a [VarLayer] belongs to. The files of one rung deep-merge onto
// each other; a later rung replaces a name whole.
type VarRung uint8

const (
	RungService     VarRung = iota // a service's `vars/` files and `_stack.yaml` steps
	RungScenario                   // a scenario's top-level `vars:`
	RungDestinyFile                // a destiny's `vars.yml`
	RungTask                       // a block's and the task's own `vars:`
)

// VarLayer is one contribution to a view: a file, a step or a rung's map.
type VarLayer struct {
	Rung VarRung
	// Origin names the layer in an error: "vars/20-host-layout.yaml", "scenario
	// vars:", "vars.yml", "task vars:".
	Origin string
	// Defs are RAW definitions: a string at any depth may carry `${ … }`.
	Defs map[string]any
	// Strategy is per name, onto the LOWER layers of the same rung: "deep" (the
	// default) or "replace". Only the service rung has one.
	Strategy map[string]string
	// Engine compiles this layer's definitions: the rung decides which roots besides
	// `vars` they may name. Required when a definition carries `${ … }`.
	Engine *Engine
	// Base is the values those roots read. Its Vars and View are not read: `vars`
	// is the view.
	Base Vars
}

const strategyReplace = "replace"

// VarView is the lazy `vars.*` of one context, stacked on an optional parent view —
// the run's service view under a task's, say. Layers are indexed globally, the
// parent's first.
//
// Safe for concurrent use. Values it returns are shared with its memo and must not
// be mutated.
type VarView struct {
	parent *VarView
	layers []VarLayer
	// base is the index of the first own layer.
	base int
	// own is every name the own layers define: a parent value whose evaluation read
	// none of them is this view's value too.
	own map[string]struct{}

	namesOnce sync.Once
	names     []string

	mu   sync.Mutex
	memo map[varKey]*varEntry

	evaluations atomic.Int64
	onEvaluate  func(name string, bound int) // tests only
	onSettle    func(touched int)            // tests only
}

// varKey is a name as the layers up to bound define it. The bound is always the
// index of the name's highest contribution at or below the requested one, so one
// value has one key.
type varKey struct {
	name  string
	bound int
}

// varEntry is a memoized value and the `vars` names its evaluation read,
// transitively; whole is a read of the namespace itself (its size or its keys).
type varEntry struct {
	value any
	reads map[string]struct{}
	whole bool
}

// NewVarView stacks layers, lowest first, on parent (nil for none).
func NewVarView(parent *VarView, layers ...VarLayer) *VarView {
	v := &VarView{parent: parent, own: map[string]struct{}{}, memo: map[varKey]*varEntry{}}
	if parent != nil {
		v.base = len(parent.layers)
	}
	v.layers = make([]VarLayer, 0, v.base+len(layers))
	if parent != nil {
		v.layers = append(v.layers, parent.layers...)
	}
	v.layers = append(v.layers, layers...)
	for _, l := range layers {
		for name := range l.Defs {
			v.own[name] = struct{}{}
		}
	}
	return v
}

// Names returns, sorted, every name any layer defines. Nothing is evaluated.
func (v *VarView) Names() []string { return slices.Clone(v.sortedNames()) }

func (v *VarView) sortedNames() []string {
	v.namesOnce.Do(func() {
		set := map[string]struct{}{}
		for _, l := range v.layers {
			for name := range l.Defs {
				set[name] = struct{}{}
			}
		}
		v.names = slices.Sorted(maps.Keys(set))
	})
	return v.names
}

// Has reports whether any layer defines name. Nothing is evaluated.
func (v *VarView) Has(name string) bool { return v.winner(name, v.top()) >= 0 }

// Lookup evaluates name, or returns its memoized value. found is false for a name no
// layer defines.
func (v *VarView) Lookup(name string) (value any, found bool, err error) {
	return v.root().lookup(name)
}

// Materialize evaluates every name, in sorted order, and returns the first error.
func (v *VarView) Materialize() (map[string]any, error) { return v.root().materialize() }

// Check evaluates every name as Materialize does and returns the first error: the
// strict render mode's check ([ADR-0082] amendment 2026-10-15, point 12b). It only
// observes. Nothing it evaluates reaches the coverage sink or the view's memo, and it
// adds nothing to Evaluations. So a trial's coverage, and the evaluations a later read
// makes, are what they would be without it. A value already in the memo is reused.
func (v *VarView) Check() error {
	l := &lazyVars{view: v, read: &varRead{failed: map[varKey]*varFailure{}, scratch: map[varKey]*varEntry{}}}
	_, err := l.materialize()
	return err
}

// varRead is what one read carries down every definition it evaluates. A read is a
// top-level Lookup, Materialize or Check, or one CEL evaluation over the lazy `vars`.
type varRead struct {
	// failed is each definition that failed in this read, with its first error, so it
	// is not evaluated again in the read: CEL evaluates every operand of `+`, and each
	// path to a failing definition would evaluate it once more, exponentially in a
	// lattice or a cyclic group. It dies with the read: a later read evaluates again.
	// Within the read a failure is kept only while the path it may depend on holds
	// ([varRead.settle]).
	failed map[varKey]*varFailure
	// pending holds each kept failure that may depend on a definition in progress, at
	// the path index of the deepest such definition, so that a return settles its own
	// failures without scanning the others.
	pending [][]*varFailure
	// scratch is a [VarView.Check]'s memo, so that each definition is evaluated once
	// per check and never stored where a later read would find it. nil in any other
	// read.
	scratch map[varKey]*varEntry
}

// failureMemoOff turns the failure memo of [varRead] off, so a read evaluates a failed
// definition again on every path to it. A test-only switch: it is the reference a
// guard compares the memo against, and no production code sets it.
var failureMemoOff bool

// varFailure is a definition's first error in a read. A cycle error depends on which
// definitions are in progress, and so does every failure that saw one, whether CEL
// propagated it or absorbed it (`||`, `&&`, a comprehension). dep is the deepest path
// index such a failure may depend on, -1 for none: a failure that depends on no
// definition in progress is the same on every path of the read. key is the failed
// definition once the read keeps it.
type varFailure struct {
	err error
	dep int
	key varKey
}

// keep records key's failure for the rest of the read.
func (r *varRead) keep(key varKey, f *varFailure) {
	f.key = key
	r.failed[key] = f
	if f.dep < 0 {
		return
	}
	for len(r.pending) <= f.dep {
		r.pending = append(r.pending, nil)
	}
	r.pending[f.dep] = append(r.pending[f.dep], f)
}

// settle runs when the evaluation at path index d returns, and updates each kept
// failure that may depend on it being in progress. If it returned a value, a later path
// reads that value where this one met a cycle, so the failure is dropped and is
// evaluated afresh if reached again. If it failed, a later path meets its kept failure
// instead, which depends on nothing deeper than d-1, and so does the failure now. It
// touches those failures alone and returns how many.
func (r *varRead) settle(d int, ok bool) int {
	if d >= len(r.pending) {
		return 0
	}
	kept := r.pending[d]
	for _, f := range kept {
		if ok {
			delete(r.failed, f.key)
		} else {
			f.dep = d - 1
		}
	}
	if !ok && d > 0 {
		r.pending[d-1] = append(r.pending[d-1], kept...)
	}
	r.pending[d] = kept[:0]
	return len(kept)
}

// Layers returns the raw stack, the parent's layers first, for the static analyses.
func (v *VarView) Layers() []VarLayer { return slices.Clone(v.layers) }

// Definitions returns every layer's definition of each name, lowest layer first —
// the conservative union an analysis of what a name may read walks.
func (v *VarView) Definitions() map[string][]any {
	out := map[string][]any{}
	for _, l := range v.layers {
		for name, def := range l.Defs {
			out[name] = append(out[name], def)
		}
	}
	return out
}

// Evaluations counts the definitions evaluated for this view. An evaluation counts
// once, on the outermost view whose memo keeps its value: a service var no override
// touches counts on the service view whichever task view asked first. A memo hit is
// not one, nor is a failure the same read still keeps; a failed evaluation counts on
// the view that ran it.
func (v *VarView) Evaluations() int64 { return v.evaluations.Load() }

func (v *VarView) top() int { return len(v.layers) - 1 }

func (v *VarView) root() *lazyVars {
	return &lazyVars{view: v, read: &varRead{failed: map[varKey]*varFailure{}}}
}

// winner is the index of name's highest contribution at or below bound, or -1.
func (v *VarView) winner(name string, bound int) int {
	for i := min(bound, v.top()); i >= 0; i-- {
		if _, ok := v.layers[i].Defs[name]; ok {
			return i
		}
	}
	return -1
}

// resolve returns name as the layers up to bound define it, evaluated on behalf of
// the definitions on path, or the failure that stopped it. self is a definition
// reading its own name below itself, for which nothing below is a cycle rather than an
// absent name. A failure read still keeps is returned again, unevaluated, ahead of the
// shared memo, so another read's value cannot change what this one reports. In a
// [VarView.Check] what is evaluated is kept in the read's scratch instead of the memo.
func (v *VarView) resolve(name string, bound int, path []varKey, self bool, read *varRead) (*varEntry, bool, *varFailure) {
	w := v.winner(name, bound)
	if w < 0 {
		if self {
			// Nothing on the path decides it: only the layers below the reader's own.
			return nil, false, &varFailure{err: newVarCycleError([]varKey{{name: name, bound: bound}}), dep: -1}
		}
		return nil, false, nil
	}
	key := varKey{name: name, bound: w}
	for i, k := range path {
		if k == key {
			return nil, false, &varFailure{err: newVarCycleError(path[i:]), dep: i}
		}
	}
	if f, ok := read.failed[key]; ok {
		return nil, false, f
	}
	if e, ok := v.cached(key); ok {
		return e, true, nil
	}
	if read.scratch != nil {
		if e, ok := read.scratch[key]; ok {
			return e, true, nil
		}
	}
	e, fail := v.evaluate(key, path, read)
	touched := read.settle(len(path), fail == nil)
	if v.onSettle != nil {
		v.onSettle(touched)
	}
	if fail != nil {
		if !failureMemoOff {
			read.keep(key, fail)
		}
		return nil, false, fail
	}
	if read.scratch != nil {
		read.scratch[key] = e
		return e, true, nil
	}
	return v.remember(key, e), true, nil
}

// cached looks key up in this view's memo, then in the parent's when the parent
// layers hold all of key and the parent's value is this view's too.
func (v *VarView) cached(key varKey) (*varEntry, bool) {
	v.mu.Lock()
	e, ok := v.memo[key]
	v.mu.Unlock()
	if ok {
		return e, true
	}
	if key.bound >= v.base {
		return nil, false
	}
	if e, ok := v.parent.cached(key); ok && v.reusable(e) {
		return e, true
	}
	return nil, false
}

// reusable: e's evaluation read nothing this view's own layers redefine. A read of
// the whole namespace changes with any own definition.
func (v *VarView) reusable(e *varEntry) bool {
	if e.whole && len(v.own) > 0 {
		return false
	}
	for name := range e.reads {
		if _, ok := v.own[name]; ok {
			return false
		}
	}
	return true
}

// remember stores e, and in every ancestor whose value it is too, and counts the
// evaluation on the outermost of them. The first write wins, so concurrent duplicates
// converge on one entry.
func (v *VarView) remember(key varKey, e *varEntry) *varEntry {
	e = v.store(key, e)
	owner := v
	for ; key.bound < owner.base && owner.reusable(e); owner = owner.parent {
		owner.parent.store(key, e)
	}
	owner.evaluations.Add(1)
	return e
}

func (v *VarView) store(key varKey, e *varEntry) *varEntry {
	v.mu.Lock()
	defer v.mu.Unlock()
	if prev, ok := v.memo[key]; ok {
		return prev
	}
	v.memo[key] = e
	return e
}

func (v *VarView) evaluate(key varKey, path []varKey, read *varRead) (*varEntry, *varFailure) {
	if v.onEvaluate != nil {
		v.onEvaluate(key.name, key.bound)
	}
	f := &varFrame{view: v, path: append(path[:len(path):len(path)], key), reads: &varReads{}, read: read, dep: -1}
	value, err := f.cell(v.assemble(key))
	if err != nil {
		if read.scratch == nil {
			v.evaluations.Add(1)
		}
		return nil, &varFailure{err: err, dep: f.dep}
	}
	return &varEntry{value: value, reads: f.reads.names, whole: f.reads.whole}, nil
}

// varCell is a name's raw definition as the layers combine it. A map merged from two
// layers keeps a cell per key; anything else is raw, contributed whole by layer.
type varCell struct {
	layer  int
	raw    any
	fields map[string]*varCell
}

// assemble combines key.name's contributions up to key.bound: within one rung a map
// recurses and anything else replaces, a "replace" strategy or a new rung replaces
// the name whole. A whole `${ … }` is a string here, so an expression's result is
// never merged with a map.
func (v *VarView) assemble(key varKey) *varCell {
	var acc *varCell
	var accRung VarRung
	for i := 0; i <= key.bound; i++ {
		l := v.layers[i]
		def, ok := l.Defs[key.name]
		if !ok {
			continue
		}
		c := &varCell{layer: i, raw: def}
		if acc != nil && l.Rung == accRung && l.Strategy[key.name] != strategyReplace {
			c = mergeCells(acc, c)
		}
		acc, accRung = c, l.Rung
	}
	return acc
}

func mergeCells(lower, upper *varCell) *varCell {
	lf, lok := lower.mapFields()
	uf, uok := upper.mapFields()
	if !lok || !uok {
		return upper
	}
	out := make(map[string]*varCell, len(lf)+len(uf))
	maps.Copy(out, lf)
	for k, c := range uf {
		if l, ok := out[k]; ok {
			c = mergeCells(l, c)
		}
		out[k] = c
	}
	return &varCell{layer: upper.layer, fields: out}
}

func (c *varCell) mapFields() (map[string]*varCell, bool) {
	if c.fields != nil {
		return c.fields, true
	}
	m, ok := c.raw.(map[string]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]*varCell, len(m))
	for k, x := range m {
		out[k] = &varCell{layer: c.layer, raw: x}
	}
	return out, true
}

// varFrame is one definition being evaluated: path is the chain of definitions in
// progress, this one last; reads collects what its evaluation reads.
type varFrame struct {
	view  *VarView
	path  []varKey
	reads *varReads
	read  *varRead
	// dep is the deepest path index above this definition that a failure its evaluation
	// saw may depend on, -1 for none ([varFailure]).
	dep int
}

// saw records a failure this evaluation met, propagated or absorbed. A kept failure's
// dep is only its deepest dependency, so one that may depend on this definition may
// depend on any definition above it too.
func (f *varFrame) saw(dep int) {
	f.dep = max(f.dep, min(dep, len(f.path)-2))
}

// cell evaluates c. Map keys go in sorted order, so of two failing cells the same
// one is reported every time.
func (f *varFrame) cell(c *varCell) (any, error) {
	if c.fields == nil {
		return f.raw(c.raw, c.layer)
	}
	out := make(map[string]any, len(c.fields))
	for _, k := range slices.Sorted(maps.Keys(c.fields)) {
		val, err := f.cell(c.fields[k])
		if err != nil {
			return nil, err
		}
		out[k] = val
	}
	return out, nil
}

func (f *varFrame) raw(x any, layer int) (any, error) {
	switch t := x.(type) {
	case string:
		return f.interpolate(t, layer)
	case map[string]any:
		out := make(map[string]any, len(t))
		for _, k := range slices.Sorted(maps.Keys(t)) {
			val, err := f.raw(t[k], layer)
			if err != nil {
				return nil, err
			}
			out[k] = val
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, el := range t {
			val, err := f.raw(el, layer)
			if err != nil {
				return nil, err
			}
			out[i] = val
		}
		return out, nil
	default:
		return x, nil
	}
}

func (f *varFrame) interpolate(s string, layer int) (any, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	l := f.view.layers[layer]
	if l.Engine == nil {
		return nil, &VarError{Name: f.path[len(f.path)-1].name, Origin: l.Origin, Err: errors.New("the layer has no Engine to evaluate ${ … }")}
	}
	vars := l.Base
	vars.frame = &lazyVars{view: f.view, frame: f, below: layer - 1, read: f.read}
	out, err := l.Engine.EvalInterpolation(s, vars)
	if err != nil {
		return nil, definitionError(f.path[len(f.path)-1].name, l.Origin, err)
	}
	return out, nil
}

// definitionError wraps err with the definition it failed in, unless err already
// carries a var error from deeper down: only the innermost failing definition is
// named, so the text does not depend on which var was asked for.
func definitionError(name, origin string, err error) error {
	var cyc *VarCycleError
	if errors.As(err, &cyc) {
		return cyc
	}
	var ve *VarError
	if errors.As(err, &ve) {
		return ve
	}
	return &VarError{Name: name, Origin: origin, Err: err}
}

// varReads is the read-set of a definition, transitive through what it read.
type varReads struct {
	names map[string]struct{}
	whole bool
}

func (r *varReads) add(name string) {
	if r.names == nil {
		r.names = map[string]struct{}{}
	}
	r.names[name] = struct{}{}
}

func (r *varReads) absorb(e *varEntry) {
	for name := range e.reads {
		r.add(name)
	}
	r.whole = r.whole || e.whole
}

// VarError is a definition that failed to evaluate.
type VarError struct {
	Name   string
	Origin string
	Err    error
}

func (e *VarError) Error() string {
	if e.Origin == "" {
		return fmt.Sprintf("vars.%s: %v", e.Name, e.Err)
	}
	return fmt.Sprintf("vars.%s (%s): %v", e.Name, e.Origin, e.Err)
}

func (e *VarError) Unwrap() error { return e.Err }

// VarCycleError is a var→var cycle. Path is closed (its first name repeats at the
// end) and starts at its smallest name.
type VarCycleError struct {
	Path []string
}

func (e *VarCycleError) Error() string {
	return ErrVarCycle.Error() + ": " + strings.Join(e.Path, " → ")
}

func (e *VarCycleError) Is(target error) bool { return target == ErrVarCycle }

// newVarCycleError renders ring, a cycle without its closing repeat, rotated to its
// smallest name, at that name's lowest bound when it occurs twice.
func newVarCycleError(ring []varKey) *VarCycleError {
	start := 0
	for i, k := range ring {
		s := ring[start]
		if k.name < s.name || (k.name == s.name && k.bound < s.bound) {
			start = i
		}
	}
	path := make([]string, 0, len(ring)+1)
	for i := range ring {
		path = append(path, ring[(start+i)%len(ring)].name)
	}
	return &VarCycleError{Path: append(path, ring[start].name)}
}

// lazyVars is `vars` in a CEL activation: a map that evaluates a name when it is
// read. cel-go qualifies a traits.Mapper through Find for select, index and presence
// alike (interpreter/attributes.go, refQualify), so `vars.x`, `vars['x']`,
// `vars[expr]`, `has(vars.x)` and `default(vars.x, …)` all evaluate x; size, `in`
// and an iteration see keys only.
//
// At a view's top, frame is nil. Inside a definition it is that definition's
// evaluation, and a read of its own name goes to bound below. read is the read in
// progress; inside a [VarView.Check] it carries a scratch memo, and its evaluations
// record no coverage.
type lazyVars struct {
	view  *VarView
	frame *varFrame
	below int
	read  *varRead
}

var _ traits.Mapper = (*lazyVars)(nil)

// bound is where a read of name looks, and records the read: a self-read is not
// recorded, since it reaches only layers below the definition's own.
func (l *lazyVars) bound(name string) (bound int, self bool) {
	if l.frame == nil {
		return l.view.top(), false
	}
	if path := l.frame.path; path[len(path)-1].name == name {
		return l.below, true
	}
	l.frame.reads.add(name)
	return l.view.top(), false
}

func (l *lazyVars) lookup(name string) (any, bool, error) {
	bound, self := l.bound(name)
	var path []varKey
	if l.frame != nil {
		path = l.frame.path
	}
	e, found, fail := l.view.resolve(name, bound, path, self, l.read)
	if fail != nil {
		if l.frame != nil {
			l.frame.saw(fail.dep)
		}
		return nil, false, fail.err
	}
	if !found {
		return nil, false, nil
	}
	if l.frame != nil {
		l.frame.reads.absorb(e)
	}
	return e.value, true, nil
}

func (l *lazyVars) materialize() (map[string]any, error) {
	if l.frame != nil {
		l.frame.reads.whole = true
	}
	names := l.view.sortedNames()
	out := make(map[string]any, len(names))
	for _, name := range names {
		val, _, err := l.lookup(name)
		if err != nil {
			return nil, err
		}
		out[name] = val
	}
	return out, nil
}

func (l *lazyVars) Find(key ref.Val) (ref.Val, bool) {
	name, ok := key.(types.String)
	if !ok {
		return nil, false
	}
	val, found, err := l.lookup(string(name))
	if err != nil {
		return types.WrapErr(err), false
	}
	if !found {
		return nil, false
	}
	return orderedTypes.NativeToValue(val), true
}

func (l *lazyVars) Get(key ref.Val) ref.Val {
	val, found := l.Find(key)
	if !found {
		return types.ValOrErr(val, "no such key: %v", key)
	}
	return val
}

func (l *lazyVars) Contains(key ref.Val) ref.Val {
	name, ok := key.(types.String)
	if !ok {
		return types.False
	}
	bound, _ := l.bound(string(name))
	return types.Bool(l.view.winner(string(name), bound) >= 0)
}

func (l *lazyVars) Size() ref.Val {
	if l.frame != nil {
		l.frame.reads.whole = true
	}
	return types.Int(len(l.view.sortedNames()))
}

func (l *lazyVars) Iterator() traits.Iterator {
	if l.frame != nil {
		l.frame.reads.whole = true
	}
	return types.NewStringList(types.DefaultTypeAdapter, l.view.sortedNames()).Iterator()
}

func (l *lazyVars) ConvertToNative(typeDesc reflect.Type) (any, error) {
	m, err := l.materialize()
	if err != nil {
		return nil, err
	}
	return types.DefaultTypeAdapter.NativeToValue(m).ConvertToNative(typeDesc)
}

func (l *lazyVars) ConvertToType(typeVal ref.Type) ref.Val {
	switch typeVal {
	case types.MapType:
		return l
	case types.TypeType:
		return types.MapType
	}
	return types.NewErr("type conversion error from '%s' to '%s'", types.MapType, typeVal)
}

func (l *lazyVars) Equal(other ref.Val) ref.Val {
	m, err := l.materialize()
	if err != nil {
		return types.WrapErr(err)
	}
	return types.DefaultTypeAdapter.NativeToValue(m).Equal(other)
}

// Type is the CEL map type: the map overloads and their runtime type guards dispatch
// on it.
func (l *lazyVars) Type() ref.Type { return types.MapType }

// Value is the materialized map, or the error that stopped it, as cel-go's own error
// value answers. A result that holds a lazyVars is converted by [toNative], which
// returns that error instead.
func (l *lazyVars) Value() any {
	m, err := l.materialize()
	if err != nil {
		return err
	}
	return m
}

// comparesVars reports whether a checked expression may compare the `vars` root itself
// by `==`, `!=` or list membership, at any depth. Each is a whole read (ADR-0082
// amendment 2026-10-15, point 13), but cel-go v0.29.2 drops a lazy map's error in all
// of them except `vars == m`: `!=` is `Equal(l, r) != true`
// (interpreter/interpretable.go:492), a map or list on the left compares by element and
// stops only on false (common/types/map.go:276, list.go:229), membership keeps only a
// true (list.go:138), and `== null` never asks the map (util.go:44). So such a program
// has the view materialized before it runs, and the view's error is the evaluation's.
//
// The root reaches a comparison through every value that may hold it ([varsFlow]): a
// literal list or map, a ternary, an index or a select into such a value, a
// comprehension's variables and result, and the result of any call but those in
// [scalarResult] — dyn(), a list concatenation and merge(), which copies its arguments'
// values shallowly — so `[vars][0] != {}`, `[vars].exists(m, m != {})` and
// `merge({'x': vars}).x != {}` are seen. A comprehension that binds the name `vars`
// hides the root in its body.
func comparesVars(e ast.Expr) bool { return comparesIn(e, varsScope{varsRoot: flowRoot}) }

// ComparesVars is [comparesVars] over a parsed expression, so that soul-lint judges a
// whole read as the Keeper does (shared/servicevars/reads.go). The checker rewrites a
// leading-dot `.vars` to `vars`; the parser keeps it.
func ComparesVars(e ast.Expr) bool {
	return comparesIn(e, varsScope{varsRoot: flowRoot, "." + varsRoot: flowRoot})
}

func comparesIn(e ast.Expr, s varsScope) bool {
	in := func(e ast.Expr) bool { return comparesIn(e, s) }
	switch e.Kind() {
	case ast.CallKind:
		c := e.AsCall()
		args := c.Args()
		if len(args) == 2 {
			switch c.FunctionName() {
			case operators.Equals, operators.NotEquals:
				if s.flow(args[0])|s.flow(args[1]) != 0 {
					return true
				}
			case operators.In:
				// `'x' in vars` reads keys only; a list on the right compares by element.
				if s.flow(args[0])|s.flow(args[1]).element() != 0 {
					return true
				}
			}
		}
		if c.IsMemberFunction() && in(c.Target()) {
			return true
		}
		return slices.ContainsFunc(args, in)
	case ast.ComprehensionKind:
		cp := e.AsComprehension()
		if in(cp.IterRange()) || in(cp.AccuInit()) {
			return true
		}
		loop, result := s.comprehension(cp)
		return comparesIn(cp.LoopCondition(), loop) || comparesIn(cp.LoopStep(), loop) || comparesIn(cp.Result(), result)
	case ast.ListKind:
		return slices.ContainsFunc(e.AsList().Elements(), in)
	case ast.MapKind:
		for _, en := range e.AsMap().Entries() {
			me := en.AsMapEntry()
			if in(me.Key()) || in(me.Value()) {
				return true
			}
		}
	case ast.StructKind:
		for _, f := range e.AsStruct().Fields() {
			if in(f.AsStructField().Value()) {
				return true
			}
		}
	case ast.SelectKind:
		return in(e.AsSelect().Operand())
	}
	return false
}

// scalarResult reports whether fn's result cannot hold an argument's value: every
// overload cel-go v0.29.2 declares for these (common/stdlib/standard.go) returns a
// bool, an int, another scalar or a type, and shared/cel adds none. A call to any
// other function carries what its arguments carry ([varsScope.flow]).
func scalarResult(fn string) bool {
	switch fn {
	case operators.Equals, operators.NotEquals,
		operators.Less, operators.LessEquals, operators.Greater, operators.GreaterEquals,
		operators.In, operators.LogicalAnd, operators.LogicalOr, operators.LogicalNot, operators.NotStrictlyFalse,
		overloads.Size, overloads.TypeConvertType,
		overloads.TypeConvertBool, overloads.TypeConvertBytes, overloads.TypeConvertDouble,
		overloads.TypeConvertInt, overloads.TypeConvertString, overloads.TypeConvertUint,
		overloads.Contains, overloads.EndsWith, overloads.StartsWith, overloads.Matches:
		return true
	}
	return false
}

// varsFlow is where the `vars` root may sit in a value: bit k is the root k levels
// down, bit 0 the root itself, and the top bit that depth or deeper.
type varsFlow uint8

const (
	flowRoot varsFlow = 1
	flowDeep varsFlow = 1 << 7
)

// holder is a list or map whose elements are f.
func (f varsFlow) holder() varsFlow { return f<<1 | f&flowDeep }

// element is an element of f. An element of the root is a var's value, never the root.
func (f varsFlow) element() varsFlow { return f>>1 | f&flowDeep }

// varsScope is what each identifier may hold: the root under its name, and a
// comprehension's variables.
type varsScope map[string]varsFlow

func (s varsScope) with(name string, f varsFlow) varsScope {
	out := maps.Clone(s)
	out[name] = f
	return out
}

// flow is where the root may sit in e's value.
func (s varsScope) flow(e ast.Expr) varsFlow {
	var f varsFlow
	switch e.Kind() {
	case ast.IdentKind:
		return s[e.AsIdent()]
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			f |= s.flow(el)
		}
		return f.holder()
	case ast.MapKind:
		for _, en := range e.AsMap().Entries() {
			f |= s.flow(en.AsMapEntry().Value())
		}
		return f.holder()
	case ast.StructKind:
		for _, fl := range e.AsStruct().Fields() {
			f |= s.flow(fl.AsStructField().Value())
		}
		return f.holder()
	case ast.SelectKind:
		if sel := e.AsSelect(); !sel.IsTestOnly() {
			return s.flow(sel.Operand()).element()
		}
	case ast.CallKind:
		c := e.AsCall()
		args := c.Args()
		switch {
		case c.FunctionName() == operators.Conditional && len(args) == 3:
			return s.flow(args[1]) | s.flow(args[2])
		case c.FunctionName() == operators.Index && len(args) == 2:
			return s.flow(args[0]).element()
		case scalarResult(c.FunctionName()):
			return 0
		}
		// Any other call, dyn(), `+` and merge() among them, may return what its
		// arguments hold. Taking their depths is sound only while no function nests an
		// argument deeper than it sits: merge() lifts a list's maps one level, which
		// this over-counts, and nothing in the env wraps one.
		if c.IsMemberFunction() {
			f = s.flow(c.Target())
		}
		for _, a := range args {
			f |= s.flow(a)
		}
		return f
	case ast.ComprehensionKind:
		_, result := s.comprehension(e.AsComprehension())
		return result.flow(e.AsComprehension().Result())
	}
	return 0
}

// comprehension returns the scopes of cp's loop and of its result. The iteration
// variable holds an element of the range, which over a map is a key and is taken for an
// element all the same; the accumulator holds what its initial value or any step may.
func (s varsScope) comprehension(cp ast.ComprehensionExpr) (loop, result varsScope) {
	iter := s.with(cp.IterVar(), s.flow(cp.IterRange()).element())
	if v2 := cp.IterVar2(); v2 != "" {
		iter = s.with(cp.IterVar(), 0).with(v2, s.flow(cp.IterRange()).element())
	}
	accu := s.flow(cp.AccuInit())
	for {
		loop = iter.with(cp.AccuVar(), accu)
		next := accu | loop.flow(cp.LoopStep())
		if next == accu {
			return loop, s.with(cp.AccuVar(), accu)
		}
		accu = next
	}
}
