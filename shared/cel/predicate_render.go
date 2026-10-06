package cel

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/parser"
)

// A post-module predicate (`changed_when:`, `failed_when:`, `retry.until:`) rendered on
// the Keeper (NIM-907 decision 4, [docs/scenario/predicate-rendering.md]). Every read of
// the host context outside the OPEN set — `register.self`, and inside a group the
// group's own registers — is substituted by its value on that host, and the Soul
// receives text that reads the open set alone:
//
//	failed_when: register.self.stdout != register.probe.stdout
//	→            register.self.stdout != dyn("bob")
//
// The contract is an equivalence (§1): for every value the open set can take, the
// rendered text gives what the original gives against the full context, an error
// included. The rewrite only SUBSTITUTES — nothing is simplified or dropped —
// so the tree the Soul checks and evaluates is the author's, node for node, and the
// argument for the equivalence is short:
//
//   - A read is a context root, a field of one, or an index into one by a key that
//     reads nothing open. Its value is the value the original reads there, and
//     `dyn(<value>)` is typed dyn, as the read was: every root is declared dyn, so the
//     checker sees what it saw. (`3.0 == 3` does not type-check where `dyn` == `dyn`
//     evaluates.)
//   - A read that errs (a missing key) becomes `dyn({}).keeper_eval_error`, which raises
//     only where the Soul reaches it, so `&&`, `||`, `?:`, exists() and all() absorb it
//     or not exactly as they absorbed the original's error.
//   - `has(x)` and `default(x, y)` take x as a FIELD, so a closed x is not replaced by
//     its value (`has("v")` does not parse): has(x) becomes the bool it evaluates to,
//     and default(x, y) its own expansion `has(x) ? x : y` with both decided.
//   - Iteration variables are open and resolved by SCOPE: in `l.exists(input, input == 1)`
//     `input` is the variable.
//   - cel-go's printer does not read back every tree its parser builds (see
//     [predicateRenderer.groupForParser]), and its checker types a cross-type comparison
//     by traversal order (see [predicateRenderer.inComprehension]).
//
// Simplifying the rendering — deciding what the host context alone decides, so less
// travels to the host — is deferred to NIM-970; this version does none of it.
//
// [docs/scenario/predicate-rendering.md]: docs/scenario/predicate-rendering.md

// Codes of a refused rendering. The host's task fails before dispatch with one of these
// (predicate-rendering.md §1).
const (
	// CodePredicateNotRenderable — a value the text cannot carry (invalid UTF-8, a
	// timestamp that does not read back, a type name an iteration variable captures), a
	// bare `register`, or a rendered text the Soul would not accept as sent (100 000
	// code points, 250 levels of nesting, a guard, or the whitespace normalization
	// misreading a string literal).
	CodePredicateNotRenderable = "predicate_not_renderable"
	// CodePredicateResidualReference — the rendered text still reads the host context
	// outside the open set. Guards the rewrite itself: a correct rewrite never trips it.
	CodePredicateResidualReference = "predicate_residual_reference"
)

// keeperErrorField is selected on an empty map to carry an error the Keeper met into
// the rendered text: `dyn({}).keeper_eval_error` raises "no such key" wherever the Soul
// evaluates it, and nowhere CEL's logic does not reach it.
const keeperErrorField = "keeper_eval_error"

// registerIdent — the `register` context root.
const registerIdent = "register"

// RegisterSelf is the open register every post-module predicate reads: the task's own
// result, which exists only after its module ran on the Soul.
const RegisterSelf = "self"

// ErrPredicateRender is a refused rendering. Expr is the author's predicate; the
// rendered text is never quoted, because it can hold a substituted secret.
type ErrPredicateRender struct {
	Code   string
	Expr   string
	Reason string
}

func (e *ErrPredicateRender) Error() string {
	return fmt.Sprintf("%s: predicate %q: %s", e.Code, e.Expr, e.Reason)
}

// RenderedPredicate is the text the Soul evaluates. Resolved reports that the original
// reads the host context outside the open set, so Text is the rendering; when it is
// false Text is the author's predicate (normalized) and needs no capability beyond the
// one a predicate needs today (predicate-rendering.md §5).
type RenderedPredicate struct {
	Text     string
	Resolved bool
}

// RenderPredicate renders expr against one host's full context. vars is that context
// WITHOUT the open set; open names the registers that stay open — [RegisterSelf] for a
// post-module predicate, plus the group's own registers inside a group.
//
// e must be a flow-control Engine ([NewFlowControl]): the original is judged by the
// environment the Soul evaluates it in, and the result is checked against it.
//
// Errors: what compiling the original returns ([ErrCompile], [ErrUnsupported]), or
// [ErrPredicateRender].
func (e *Engine) RenderPredicate(expr string, vars Vars, open []string) (RenderedPredicate, error) {
	if !e.flowControl {
		return RenderedPredicate{}, errors.New("RenderPredicate needs a flow-control Engine (NewFlowControl): the predicate is judged in the Soul's environment")
	}
	norm := normalize(expr)
	if norm == "" {
		return RenderedPredicate{}, nil
	}
	loopNames := vars.loopNames()
	env, err := e.loopEnv(loopNames)
	if err != nil {
		return RenderedPredicate{}, err
	}
	if _, err := e.compile(env, loopNames, norm, false, false, vars.ComputeScope, vars.hostStance()); err != nil {
		return RenderedPredicate{}, err
	}
	parsed, err := e.parseNoMacro(norm)
	if err != nil {
		return RenderedPredicate{}, &ErrCompile{Expr: expr, Err: err}
	}

	r := newPredicateRenderer(e, expr, vars, loopNames, open, parsed.SourceInfo())
	if findNakedRegister(parsed.Expr()) {
		return RenderedPredicate{}, r.refusalErr(CodePredicateNotRenderable,
			"`register` is read without a field: that map is register.self and the registers the Keeper decides at once, "+
				"and no text reads one without the other; name the register (register.self.<field>, register.<name>.<field>)")
	}
	text := norm
	resolved := len(r.contextReads(parsed.Expr(), nil)) > 0
	var out ast.Expr
	if resolved {
		out = r.subst(parsed.Expr(), nil)
		if r.refusal != nil {
			return RenderedPredicate{}, r.refusal
		}
		if text, err = r.unparse(out); err != nil {
			return RenderedPredicate{}, r.refusalErr(CodePredicateNotRenderable, "printing the rendered tree: "+err.Error())
		}
	}
	if err := r.checkSoulAccepts(text); err != nil {
		// A substitution can take a comprehension with it — from an index key, or from
		// the field of a has()/default() ([predicateRenderer.inComprehension]); inside
		// one the text checks as the original did.
		if out == nil {
			return RenderedPredicate{}, err
		}
		wrapped, werr := r.unparse(r.inComprehension(out))
		if werr != nil || r.checkSoulAccepts(wrapped) != nil {
			return RenderedPredicate{}, err
		}
		text = wrapped
	}
	if err := r.checkResidual(text); err != nil {
		return RenderedPredicate{}, err
	}
	return RenderedPredicate{Text: text, Resolved: resolved}, nil
}

// scope is the set of iteration variables bound at a node.
type scope []string

func (s scope) has(name string) bool {
	for _, n := range s {
		if n == name {
			return true
		}
	}
	return false
}

func (s scope) with(name string) scope {
	return append(s[:len(s):len(s)], name)
}

type predicateRenderer struct {
	e         *Engine
	expr      string
	vars      Vars
	loopNames []string
	open      map[string]bool
	// context — the unbound identifiers that read the host context.
	context map[string]bool
	fac     ast.ExprFactory
	info    *ast.SourceInfo
	nextID  int64
	refusal *ErrPredicateRender
}

func newPredicateRenderer(e *Engine, expr string, vars Vars, loopNames, open []string, info *ast.SourceInfo) *predicateRenderer {
	r := &predicateRenderer{
		e:         e,
		expr:      expr,
		vars:      vars,
		loopNames: loopNames,
		open:      make(map[string]bool, len(open)),
		context:   make(map[string]bool, len(flowControlVars)+len(loopNames)),
		fac:       ast.NewExprFactory(),
		info:      info,
		// Above every id the parser handed out: Unparse looks macro calls up by id.
		nextID: math.MaxInt32,
	}
	for _, n := range open {
		r.open[n] = true
	}
	for _, n := range flowControlVars {
		r.context[n] = true
	}
	for _, n := range loopNames {
		r.context[n] = true
	}
	return r
}

// unparse prints on one line: the Soul normalizes whitespace, and the text must reach
// it unchanged (checkSoulAccepts).
func (r *predicateRenderer) unparse(e ast.Expr) (string, error) {
	return parser.Unparse(r.groupForParser(e), r.info, parser.WrapOnColumn(math.MaxInt32))
}

// groupForParser rewrites the tree so cel-go's printer and parser agree on it: Unparse
// leaves a unary operand unparenthesized in three places where the parser then reads
// something else, and wrapping that operand in dyn() — a call, so a primary — groups
// it without changing a value:
//
//   - under another unary: `!(!x)` prints `!!x`, and the parser drops an even run of
//     `!` or `-` (VisitLogicalNot, VisitNegate) — for a non-bool x a value where the
//     original errs;
//   - as a select operand, a member call's target or an index operand: `(-x).f` prints
//     `-x.f`, which parses as `-(x.f)`.
//
// A negative literal prints with its `-` and is grouped for the same reasons.
func (r *predicateRenderer) groupForParser(e ast.Expr) ast.Expr {
	group := func(x ast.Expr) ast.Expr {
		if isUnaryCall(x) || isNegativeLiteral(x) {
			return r.fac.NewCall(r.id(), "dyn", x)
		}
		return x
	}
	switch e.Kind() {
	case ast.SelectKind:
		s := e.AsSelect()
		return r.fac.NewSelect(r.id(), group(r.groupForParser(s.Operand())), s.FieldName())
	case ast.CallKind:
		c := e.AsCall()
		args := make([]ast.Expr, len(c.Args()))
		for i, a := range c.Args() {
			args[i] = r.groupForParser(a)
		}
		if isUnary(c) || (!c.IsMemberFunction() && c.FunctionName() == operators.Index) {
			args[0] = group(args[0])
		}
		if c.IsMemberFunction() {
			return r.fac.NewMemberCall(r.id(), c.FunctionName(), group(r.groupForParser(c.Target())), args...)
		}
		return r.fac.NewCall(r.id(), c.FunctionName(), args...)
	case ast.ListKind:
		l := e.AsList()
		els := make([]ast.Expr, len(l.Elements()))
		for i, el := range l.Elements() {
			els[i] = r.groupForParser(el)
		}
		return r.fac.NewList(r.id(), els, l.OptionalIndices())
	case ast.MapKind:
		entries := e.AsMap().Entries()
		out := make([]ast.EntryExpr, len(entries))
		for i, en := range entries {
			me := en.AsMapEntry()
			out[i] = r.fac.NewMapEntry(r.id(), r.groupForParser(me.Key()), r.groupForParser(me.Value()), me.IsOptional())
		}
		return r.fac.NewMap(r.id(), out)
	case ast.StructKind:
		st := e.AsStruct()
		out := make([]ast.EntryExpr, len(st.Fields()))
		for i, f := range st.Fields() {
			sf := f.AsStructField()
			out[i] = r.fac.NewStructField(r.id(), sf.Name(), r.groupForParser(sf.Value()), sf.IsOptional())
		}
		return r.fac.NewStruct(r.id(), st.TypeName(), out)
	}
	return e
}

func isUnary(c ast.CallExpr) bool {
	return !c.IsMemberFunction() && len(c.Args()) == 1 &&
		(c.FunctionName() == operators.LogicalNot || c.FunctionName() == operators.Negate)
}

func isUnaryCall(e ast.Expr) bool {
	return e.Kind() == ast.CallKind && isUnary(e.AsCall())
}

// isNegativeLiteral — a literal printed with a leading `-`.
func isNegativeLiteral(e ast.Expr) bool {
	if e.Kind() != ast.LiteralKind {
		return false
	}
	switch v := e.AsLiteral().(type) {
	case types.Int:
		return v < 0
	case types.Double:
		return math.Signbit(float64(v))
	}
	return false
}

func (r *predicateRenderer) id() int64 {
	r.nextID++
	return r.nextID
}

func (r *predicateRenderer) refusalErr(code, reason string) *ErrPredicateRender {
	return &ErrPredicateRender{Code: code, Expr: r.expr, Reason: reason}
}

func (r *predicateRenderer) refuse(code, reason string) {
	if r.refusal == nil {
		r.refusal = r.refusalErr(code, reason)
	}
}

// comprehensionMacros — the receiver macros of [cel.StandardMacros] with the iteration
// variable as their first argument. A macro-free parse leaves them as member calls.
var comprehensionMacros = map[string]bool{
	"all":        true,
	"exists":     true,
	"exists_one": true,
	"filter":     true,
	"map":        true,
}

// comprehensionVar reports the iteration variable of a comprehension macro call. The
// original compiled with macros on, so a call of this name and arity IS the macro.
func comprehensionVar(c ast.CallExpr) (string, bool) {
	if !c.IsMemberFunction() || !comprehensionMacros[c.FunctionName()] {
		return "", false
	}
	args := c.Args()
	if len(args) != 2 && !(len(args) == 3 && c.FunctionName() == "map") {
		return "", false
	}
	if args[0].Kind() != ast.IdentKind {
		return "", false
	}
	return args[0].AsIdent(), true
}

// isPresenceCall — `has(x)`, the global macro.
func isPresenceCall(c ast.CallExpr) bool {
	return !c.IsMemberFunction() && c.FunctionName() == "has" && len(c.Args()) == 1
}

// isDefaultCall — `default(x, y)` ([default.go]).
func isDefaultCall(c ast.CallExpr) bool {
	return !c.IsMemberFunction() && c.FunctionName() == defaultMacroName && len(c.Args()) == 2
}

// isRegisterRoot — the `register` context root, not an iteration variable of that name.
func isRegisterRoot(e ast.Expr, sc scope) bool {
	return e.Kind() == ast.IdentKind && e.AsIdent() == registerIdent && !sc.has(registerIdent)
}

// isOpenRegister — `register.<name>` with name in the open set.
func (r *predicateRenderer) isOpenRegister(e ast.Expr, sc scope) bool {
	if e.Kind() != ast.SelectKind {
		return false
	}
	s := e.AsSelect()
	return isRegisterRoot(s.Operand(), sc) && r.open[s.FieldName()]
}

// readsOpen reports whether e reads the open set or an iteration variable bound OUTSIDE
// e (sc); a variable e binds itself is local to it. An index key that does is not part
// of a closed read.
func (r *predicateRenderer) readsOpen(e ast.Expr, sc scope) bool {
	found := false
	var walk func(e ast.Expr, local scope)
	walk = func(e ast.Expr, local scope) {
		if found {
			return
		}
		switch e.Kind() {
		case ast.IdentKind:
			name := e.AsIdent()
			found = !local.has(name) && (sc.has(name) || name == registerIdent)
			return
		case ast.SelectKind:
			s := e.AsSelect()
			if op := s.Operand(); op.Kind() == ast.IdentKind && op.AsIdent() == registerIdent &&
				!local.has(registerIdent) && !sc.has(registerIdent) {
				found = r.open[s.FieldName()]
				return
			}
		}
		forEachChild(e, local, walk)
	}
	walk(e, nil)
	return found
}

// contextReads lists what e reads of the host context outside the open set, resolving
// every name by scope: an iteration variable named `input` is the variable. A subtree
// that reads nothing (`0`, `"bob"`, `int`) is kept verbatim — the Soul evaluates it the
// same.
func (r *predicateRenderer) contextReads(e ast.Expr, sc scope) []string {
	var reads []string
	var walk func(e ast.Expr, sc scope)
	walk = func(e ast.Expr, sc scope) {
		switch e.Kind() {
		case ast.IdentKind:
			if name := e.AsIdent(); !sc.has(name) && r.context[name] {
				reads = append(reads, name)
			}
			return
		case ast.SelectKind:
			if r.isOpenRegister(e, sc) {
				return
			}
			if s := e.AsSelect(); isRegisterRoot(s.Operand(), sc) {
				reads = append(reads, registerIdent+"."+s.FieldName())
				return
			}
		}
		forEachChild(e, sc, walk)
	}
	walk(e, sc)
	return reads
}

// findNakedRegister reports a `register` root read without a field after it.
func findNakedRegister(e ast.Expr) bool {
	found := false
	var walk func(e ast.Expr, sc scope)
	walk = func(e ast.Expr, sc scope) {
		switch e.Kind() {
		case ast.IdentKind:
			found = found || isRegisterRoot(e, sc)
			return
		case ast.SelectKind:
			if isRegisterRoot(e.AsSelect().Operand(), sc) {
				return
			}
		}
		forEachChild(e, sc, walk)
	}
	walk(e, nil)
	return found
}

// subst returns e with every closed read of the host context substituted by its value
// on this host. Nothing else changes: no node is simplified, dropped or reordered.
func (r *predicateRenderer) subst(e ast.Expr, sc scope) ast.Expr {
	if r.refusal != nil {
		return e
	}
	if r.isClosedRead(e, sc) {
		return r.value(r.eval(e), sc)
	}
	switch e.Kind() {
	case ast.SelectKind:
		s := e.AsSelect()
		return r.fac.NewSelect(r.id(), r.subst(s.Operand(), sc), s.FieldName())
	case ast.CallKind:
		c := e.AsCall()
		if isPresenceCall(c) && r.isClosedRead(c.Args()[0], sc) {
			return r.presence(c.Args()[0])
		}
		if isDefaultCall(c) && r.isClosedRead(c.Args()[0], sc) {
			return r.defaultExpansion(c, sc)
		}
		if v, ok := comprehensionVar(c); ok {
			inner := sc.with(v)
			args := []ast.Expr{r.fac.CopyExpr(c.Args()[0])}
			for _, a := range c.Args()[1:] {
				args = append(args, r.subst(a, inner))
			}
			return r.fac.NewMemberCall(r.id(), c.FunctionName(), r.subst(c.Target(), sc), args...)
		}
		args := make([]ast.Expr, len(c.Args()))
		for i, a := range c.Args() {
			args[i] = r.subst(a, sc)
		}
		if c.IsMemberFunction() {
			return r.fac.NewMemberCall(r.id(), c.FunctionName(), r.subst(c.Target(), sc), args...)
		}
		return r.fac.NewCall(r.id(), c.FunctionName(), args...)
	case ast.ListKind:
		l := e.AsList()
		els := make([]ast.Expr, len(l.Elements()))
		for i, el := range l.Elements() {
			els[i] = r.subst(el, sc)
		}
		return r.fac.NewList(r.id(), els, l.OptionalIndices())
	case ast.MapKind:
		entries := e.AsMap().Entries()
		out := make([]ast.EntryExpr, len(entries))
		for i, en := range entries {
			me := en.AsMapEntry()
			out[i] = r.fac.NewMapEntry(r.id(), r.subst(me.Key(), sc), r.subst(me.Value(), sc), me.IsOptional())
		}
		return r.fac.NewMap(r.id(), out)
	case ast.StructKind:
		st := e.AsStruct()
		out := make([]ast.EntryExpr, len(st.Fields()))
		for i, f := range st.Fields() {
			sf := f.AsStructField()
			out[i] = r.fac.NewStructField(r.id(), sf.Name(), r.subst(sf.Value(), sc), sf.IsOptional())
		}
		return r.fac.NewStruct(r.id(), st.TypeName(), out)
	}
	return e
}

// isClosedRead — a read of the host context the Keeper resolves whole: a context root,
// a field of one, or an index into one by a key that reads nothing open.
// `register.<open>` is not one, and a bare `register` is refused before this is asked.
func (r *predicateRenderer) isClosedRead(e ast.Expr, sc scope) bool {
	switch e.Kind() {
	case ast.IdentKind:
		name := e.AsIdent()
		return name != registerIdent && !sc.has(name) && r.context[name]
	case ast.SelectKind:
		s := e.AsSelect()
		if isRegisterRoot(s.Operand(), sc) {
			return !r.open[s.FieldName()]
		}
		return r.isClosedRead(s.Operand(), sc)
	case ast.CallKind:
		c := e.AsCall()
		return !c.IsMemberFunction() && c.FunctionName() == operators.Index &&
			r.isClosedRead(c.Args()[0], sc) && !r.readsOpen(c.Args()[1], sc)
	}
	return false
}

// value prints what a closed read evaluated to: `dyn(<literal>)`, or the error term.
func (r *predicateRenderer) value(v ref.Val, sc scope) ast.Expr {
	if types.IsError(v) {
		return r.errorTerm()
	}
	return r.fac.NewCall(r.id(), "dyn", r.literal(v, sc))
}

// presence decides `has(x)` over a closed read x: the bool it evaluates to, or the error
// term where x's operand errs.
func (r *predicateRenderer) presence(x ast.Expr) ast.Expr {
	v := r.eval(r.fac.NewCall(r.id(), "has", r.fac.CopyExpr(x)))
	if b, ok := v.(types.Bool); ok {
		return r.fac.NewLiteral(r.id(), b)
	}
	return r.errorTerm()
}

// defaultExpansion renders `default(x, y)` over a closed read x as the macro's own
// expansion ([default.go]) — `has(x) ? x : y` for a field, x itself for a root — with
// has(x) and x decided and y substituted. The ternary stays even where its condition is
// a literal: the arm alone can type narrower than the ternary the checker typed, so
// dropping the other one could refuse what the original compiled.
func (r *predicateRenderer) defaultExpansion(c ast.CallExpr, sc scope) ast.Expr {
	x, y := c.Args()[0], c.Args()[1]
	if x.Kind() == ast.IdentKind {
		return r.value(r.eval(x), sc)
	}
	return r.fac.NewCall(r.id(), operators.Conditional, r.presence(x), r.value(r.eval(x), sc), r.subst(y, sc))
}

// eval runs a closed read through this Engine with the full context, inside a
// one-element comprehension ([predicateRenderer.inComprehension]) so it type-checks
// wherever it type-checked in the original. The text comes from the parse of the
// original, so the compile cache stays bounded by the scenario's own text.
func (r *predicateRenderer) eval(e ast.Expr) ref.Val {
	text, err := r.unparse(r.inComprehension(e))
	if err != nil {
		r.refuse(CodePredicateNotRenderable, "printing a subexpression: "+err.Error())
		return types.NewErr("%v", err)
	}
	env, err := r.e.loopEnv(r.loopNames)
	if err != nil {
		r.refuse(CodePredicateNotRenderable, err.Error())
		return types.NewErr("%v", err)
	}
	prg, err := r.e.compile(env, r.loopNames, text, false, false, r.vars.ComputeScope, r.vars.hostStance())
	if err != nil {
		r.refuse(CodePredicateNotRenderable, fmt.Sprintf("subexpression %q does not compile alone: %v", text, err))
		return types.NewErr("%v", err)
	}
	out, _, err := prg.Eval(r.vars.activation(false))
	if err != nil {
		return types.WrapErr(err)
	}
	return out
}

// inComprehension returns `[0].map(v, e)[0]`, which evaluates to what e evaluates to and
// type-checks wherever e type-checked in the original.
//
// ★ cel-go's checker does not type a cross-type numeric comparison (`0 < 2.5`) the same
// everywhere: checker.Env.enterScope and exitScope drop the filter on those overloads,
// so from the first comprehension the check enters on, `0 < 2.5` compiles, and before
// it, it is refused (cel-go v0.29.2). A read compiled alone, or a rendering in which a
// substitution took a comprehension with it (from an index key, or from the field of a
// has()/default()), can therefore be refused where the original compiled; inside a
// comprehension it never is, and the runtime dispatches the comparison the same either
// way.
func (r *predicateRenderer) inComprehension(e ast.Expr) ast.Expr {
	zero := func() ast.Expr { return r.fac.NewLiteral(r.id(), types.IntZero) }
	elems := r.fac.NewList(r.id(), []ast.Expr{zero()}, nil)
	mapped := r.fac.NewMemberCall(r.id(), "map", elems, r.fac.NewIdent(r.id(), freshIdent(e)), e)
	return r.fac.NewCall(r.id(), operators.Index, mapped, zero())
}

// freshIdent names an identifier e does not mention.
func freshIdent(e ast.Expr) string {
	used := map[string]bool{}
	var walk func(e ast.Expr, sc scope)
	walk = func(e ast.Expr, sc scope) {
		if e.Kind() == ast.IdentKind {
			used[e.AsIdent()] = true
		}
		forEachChild(e, sc, walk)
	}
	walk(e, nil)
	for i := 0; ; i++ {
		if v := fmt.Sprintf("v%d", i); !used[v] {
			return v
		}
	}
}

// errorTerm is `dyn({}).keeper_eval_error`. The dyn() is load-bearing: the checker gives
// an empty map literal fresh type parameters, so a bare `{}.f` unifies with whatever
// stands beside it and can narrow an enclosing ternary the original typed dyn.
func (r *predicateRenderer) errorTerm() ast.Expr {
	empty := r.fac.NewCall(r.id(), "dyn", r.fac.NewMap(r.id(), nil))
	return r.fac.NewSelect(r.id(), empty, keeperErrorField)
}

// literal prints a decided value as CEL that evaluates to an equal value of the same
// type. Map keys are sorted, so the same values give the same text.
func (r *predicateRenderer) literal(v ref.Val, sc scope) ast.Expr {
	switch val := v.(type) {
	case types.Null, types.Bool, types.Int, types.Uint, types.Bytes:
		return r.fac.NewLiteral(r.id(), val)
	case types.Double:
		// NaN and the infinities have no literal; double() converts "NaN", "+Inf" and
		// "-Inf" back (strconv.ParseFloat), as timestamp() converts RFC 3339.
		if f := float64(val); math.IsNaN(f) || math.IsInf(f, 0) {
			return r.fac.NewCall(r.id(), "double", r.fac.NewLiteral(r.id(), types.String(strconv.FormatFloat(f, 'g', -1, 64))))
		}
		return r.fac.NewLiteral(r.id(), val)
	case types.String:
		if !utf8.ValidString(string(val)) {
			r.refuse(CodePredicateNotRenderable, "a substituted string is not valid UTF-8")
		}
		return r.fac.NewLiteral(r.id(), val)
	case types.Timestamp:
		// RFC 3339 keeps the offset, which string() of the timestamp prints, but not every
		// timestamp has an RFC 3339 text timestamp() takes back: the offset is cut to whole
		// minutes, and the parser and cel-go bound the offset, the local year and the
		// instant. So the text is read back the way the Soul will read it.
		text := val.Time.Format(time.RFC3339Nano)
		if !timestampReadsBack(val, text) {
			r.refuse(CodePredicateNotRenderable, "a substituted timestamp's RFC 3339 text does not read back through timestamp() to the same instant (an offset with seconds, an offset or local year the parser rejects, or an instant outside the years 0001-9999 UTC)")
		}
		return r.fac.NewCall(r.id(), "timestamp", r.fac.NewLiteral(r.id(), types.String(text)))
	case types.Duration:
		return r.fac.NewCall(r.id(), "duration", r.fac.NewLiteral(r.id(), types.String(val.Duration.String())))
	case *types.Type:
		return r.typeName(val, sc)
	case traits.Lister:
		return r.list(val, sc)
	case traits.Mapper:
		return r.mapLiteral(val, sc)
	}
	r.refuse(CodePredicateNotRenderable, fmt.Sprintf("a substituted value of type %s has no CEL literal", v.Type().TypeName()))
	return r.fac.NewLiteral(r.id(), types.NullValue)
}

// timestampReadsBack — the instant is enough: the text carries the local time and the
// offset, so an offset cut to whole minutes moves the instant it reads back as.
func timestampReadsBack(t types.Timestamp, text string) bool {
	back, ok := types.String(text).ConvertToType(types.TimestampType).(types.Timestamp)
	return ok && back.Time.Equal(t.Time)
}

// typeName prints a type value by its name — `type("int")` would be a string. An
// iteration variable of that name in scope would capture the identifier.
func (r *predicateRenderer) typeName(t *types.Type, sc scope) ast.Expr {
	name := t.TypeName()
	root, _, _ := strings.Cut(name, ".")
	if sc.has(root) {
		r.refuse(CodePredicateNotRenderable, fmt.Sprintf("the type %s would be printed where the iteration variable %q captures its name", name, root))
		return r.fac.NewIdent(r.id(), name)
	}
	if got, err := r.e.EvalExpression(name, Vars{}); err != nil || got.Equal(t) != types.True {
		r.refuse(CodePredicateNotRenderable, fmt.Sprintf("the type %s does not read back from its name", name))
	}
	return r.fac.NewIdent(r.id(), name)
}

func (r *predicateRenderer) list(l traits.Lister, sc scope) ast.Expr {
	n, ok := l.Size().(types.Int)
	if !ok {
		r.refuse(CodePredicateNotRenderable, "a substituted list has no size")
		return r.fac.NewList(r.id(), nil, nil)
	}
	els := make([]ast.Expr, 0, int(n))
	for i := types.Int(0); i < n; i++ {
		els = append(els, r.literal(l.Get(i), sc))
	}
	return r.fac.NewList(r.id(), els, nil)
}

func (r *predicateRenderer) mapLiteral(m traits.Mapper, sc scope) ast.Expr {
	var keys []ref.Val
	for it := m.Iterator(); it.HasNext() == types.True; {
		keys = append(keys, it.Next())
	}
	sort.Slice(keys, func(i, j int) bool { return keyLess(keys[i], keys[j]) })
	entries := make([]ast.EntryExpr, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, r.fac.NewMapEntry(r.id(), r.literal(k, sc), r.literal(m.Get(k), sc), false))
	}
	return r.fac.NewMap(r.id(), entries)
}

// keyLess orders map keys by type, then value: bool < int < uint < string.
func keyLess(a, b ref.Val) bool {
	ra, rb := keyRank(a), keyRank(b)
	if ra != rb {
		return ra < rb
	}
	switch av := a.(type) {
	case types.Bool:
		return !bool(av) && bool(b.(types.Bool))
	case types.Int:
		return av < b.(types.Int)
	case types.Uint:
		return av < b.(types.Uint)
	case types.String:
		return av < b.(types.String)
	}
	return fmt.Sprint(a.Value()) < fmt.Sprint(b.Value())
}

func keyRank(k ref.Val) int {
	switch k.(type) {
	case types.Bool:
		return 0
	case types.Int:
		return 1
	case types.Uint:
		return 2
	case types.String:
		return 3
	}
	return 4
}

// checkResidual walks the rendered text — re-parsed, as the Soul reads it — and refuses
// any read of the host context outside the open set. Compiling cannot find one: every
// root is declared in the flow-control environment. The walk resolves names by scope,
// so an iteration variable named `input` is the variable, not the root.
func (r *predicateRenderer) checkResidual(text string) error {
	parsed, err := r.e.parseNoMacro(text)
	if err != nil {
		return r.refusalErr(CodePredicateNotRenderable, "the rendered text does not parse")
	}
	if leaked := r.contextReads(parsed.Expr(), nil); len(leaked) > 0 {
		sort.Strings(leaked)
		return r.refusalErr(CodePredicateResidualReference,
			fmt.Sprintf("the rendered text still reads %s outside the open set", strings.Join(leaked, ", ")))
	}
	return nil
}

// forEachChild calls f on each direct child of e with the scope it is evaluated in.
func forEachChild(e ast.Expr, sc scope, f func(ast.Expr, scope)) {
	switch e.Kind() {
	case ast.SelectKind:
		f(e.AsSelect().Operand(), sc)
	case ast.CallKind:
		c := e.AsCall()
		if v, ok := comprehensionVar(c); ok {
			f(c.Target(), sc)
			inner := sc.with(v)
			for _, a := range c.Args()[1:] {
				f(a, inner)
			}
			return
		}
		if c.IsMemberFunction() {
			f(c.Target(), sc)
		}
		for _, a := range c.Args() {
			f(a, sc)
		}
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			f(el, sc)
		}
	case ast.MapKind:
		for _, en := range e.AsMap().Entries() {
			me := en.AsMapEntry()
			f(me.Key(), sc)
			f(me.Value(), sc)
		}
	case ast.StructKind:
		for _, fl := range e.AsStruct().Fields() {
			f(fl.AsStructField().Value(), sc)
		}
	}
}

// expressionCodePointLimit — cel-go's parser default, which the Soul's environment keeps.
const expressionCodePointLimit = 100_000

// checkSoulAccepts runs the rendered text through what the Soul does before it
// evaluates a predicate — normalize, the guards, the parser's limits, the type check —
// uncached, since the text is one host's. The Keeper never sends what the Soul would
// reject or read differently.
func (r *predicateRenderer) checkSoulAccepts(text string) error {
	if n := utf8.RuneCountInString(text); n > expressionCodePointLimit {
		return r.refusalErr(CodePredicateNotRenderable,
			fmt.Sprintf("the rendered text has %d code points; the Soul's parser stops at %d", n, expressionCodePointLimit))
	}
	if normalize(text) != text {
		return r.refusalErr(CodePredicateNotRenderable,
			"the Soul's whitespace normalization would change a string literal in the rendered text: it does not read an escaped quote")
	}
	var soul Vars
	if _, err := r.e.compileUncached(r.e.env, nil, text, false, false, soul.ComputeScope, soul.hostStance()); err != nil {
		return r.refusalErr(CodePredicateNotRenderable, "the Soul would not compile the rendered text: "+soulCompileReason(err))
	}
	return nil
}

// soulCompileReason names why the Soul would refuse the rendered text without quoting
// it: the text, and a parser message's excerpt of it, can hold a substituted secret. Only
// messages that name types or limits pass through.
func soulCompileReason(err error) string {
	var ue *ErrUnsupported
	if errors.As(err, &ue) {
		return "unsupported construct " + ue.Feature
	}
	var ce *ErrCompile
	if errors.As(err, &ce) {
		msg := firstLine(ce.Err.Error())
		for _, safe := range []string{"recursion limit exceeded", "no matching overload", "does not support field selection", "expected type"} {
			if strings.Contains(msg, safe) {
				return msg
			}
		}
	}
	return "rejected"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
