package servicevars

import (
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/overloads"
	"github.com/google/cel-go/parser"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// The static reading of a definition's text that soul-lint's var graph is built
// from (ADR-0082 amendment 2026-10-15, point 12a). It lives here rather than in
// soul-lint because it needs the cel-go AST, which soul-lint does not depend on.

// Segment is a piece of an interpolated string: literal text, or the expression of
// one `${ … }` block without its marker.
type Segment struct {
	Text string
	Expr bool
}

// VarRead is a name an expression evaluates off `vars`, as `vars.<name>`,
// `vars['<name>']`, `has(vars.<name>)` or `default(vars.<name>, …)`.
type VarRead struct {
	Name string
	// Guarded: the expression also tests `has(vars.<name>)` — what
	// `default(vars.<name>, …)` expands to — or `'<name>' in vars` on this root
	// name, anywhere in it. `has(vars.x.y)` guards y, not x.
	Guarded bool
	// KeyTested: every read of the name sits where a `'<name>' in vars` test on it
	// holds, so none reaches the result unless the name is defined where it looks: in
	// a branch of `?:` whose condition implies the key on that branch's side, or in a
	// term of `&&` (`||`) where another term implies the key when it is true (false),
	// since that term decides the result otherwise and CEL drops the error. A
	// condition implies the key through `!`, `&&`, `||` and `== true`-style
	// comparisons of the test, and a bool literal implies every key on the side it
	// never takes ([impliedKeys]). A key test reads keys only; has() evaluates and
	// keys nothing.
	KeyTested bool
}

// defaultMacro mirrors the engine's default(x, y) (shared/cel/default.go): has(x) ?
// x : y over a select, x itself over an identifier, and a parse error over anything
// else. The error is load-bearing: it is what decides where a `${ … }` block ends.
var defaultMacro = parser.NewGlobalMacro("default", 2, func(mef parser.ExprHelper, _ ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
	x, fallback := args[0], args[1]
	switch x.Kind() {
	case ast.SelectKind:
		s := x.AsSelect()
		presence := mef.NewPresenceTest(mef.Copy(s.Operand()), s.FieldName())
		return mef.NewCall(operators.Conditional, presence, mef.Copy(x), fallback), nil
	case ast.IdentKind:
		return mef.Copy(x), nil
	default:
		return nil, mef.NewError(x.ID(), "default(x, y): first argument must be a field or an identifier")
	}
})

var macroParser = sync.OnceValues(func() (*parser.Parser, error) {
	return parser.NewParser(parser.Macros(append(slices.Clone(parser.AllMacros), defaultMacro)...))
})

// SplitInterpolation splits raw the way the engine scans an interpolated string: a
// block closes at the first `}` at which its content parses as CEL, and `\${` is a
// literal `${`. ok is false when a block never closes; the engine refuses that
// string at compile.
func SplitInterpolation(raw string) (segs []Segment, ok bool) {
	p, err := macroParser()
	if err != nil {
		return nil, false
	}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			segs = append(segs, Segment{Text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(raw); {
		if raw[i] == '\\' && i+2 < len(raw) && raw[i+1] == '$' && raw[i+2] == '{' {
			lit.WriteString("${")
			i += 3
			continue
		}
		if raw[i] == '$' && i+1 < len(raw) && raw[i+1] == '{' {
			flush()
			expr, next, closed := closeBlock(p, raw, i+2)
			if !closed {
				return nil, false
			}
			segs = append(segs, Segment{Text: expr, Expr: true})
			i = next
			continue
		}
		lit.WriteByte(raw[i])
		i++
	}
	flush()
	return segs, true
}

func closeBlock(p *parser.Parser, raw string, start int) (string, int, bool) {
	for end := start; end < len(raw); end++ {
		if raw[end] != '}' {
			continue
		}
		inner := strings.TrimSpace(raw[start:end])
		if _, iss := p.Parse(common.NewTextSource(inner)); iss == nil || len(iss.GetErrors()) == 0 {
			return inner, end + 1, true
		}
	}
	return "", 0, false
}

// ExprReads reports, sorted by name, the names one CEL expression evaluates off
// `vars`, and whether it reads the namespace whole: hands `vars` itself to a value,
// a comparison or a function, which materializes every name (point 13). Key-only
// operations — `size(vars)`, `'k' in vars`, iterating over `vars` — are neither. A
// read whose key is not a literal (`vars[k]`) names no field and is not reported.
// Nor is a read whose result a bool literal decides — in the branch of `?:` a
// literal condition never takes, in a term of `&&` beside one a literal makes false,
// of `||` beside one it makes true: CEL never evaluates it, or drops its error, so
// it is neither a need nor an edge. A whole comparison is whole even there: the Keeper
// materializes the view before it evaluates an expression that may compare the root
// ([cel.ComparesVars]), whatever decides the result. An expression that does not parse
// reads nothing here; the engine refuses it at compile.
//
// Parsed with the engine's macros expanded, so `has(vars.x)` is the presence test it
// compiles to, `default(x, y)` is `has(x) ? x : y`, and a comprehension that binds
// the name `vars` hides the root in its body. A `soulprint.hosts.where("…")`
// predicate is read as the engine inlines it before compiling.
func ExprReads(expr string) (reads []VarRead, whole bool) {
	p, err := macroParser()
	if err != nil {
		return nil, false
	}
	parsed, iss := p.Parse(common.NewTextSource(expr))
	if iss != nil && len(iss.GetErrors()) > 0 {
		return nil, false
	}
	w := readWalk{
		reads: map[string]bool{}, hasTests: map[string]bool{}, keyTests: map[string]bool{},
		open: map[string]bool{}, keyed: map[string]int{},
	}
	w.inlineWhere = whereInlinable(expr, parsed.Expr())
	w.expr(parsed.Expr(), false)
	for _, name := range slices.Sorted(maps.Keys(w.reads)) {
		reads = append(reads, VarRead{
			Name:      name,
			Guarded:   w.hasTests[name] || w.keyTests[name],
			KeyTested: !w.open[name],
		})
	}
	return reads, w.whole || w.compares || cel.ComparesVars(parsed.Expr())
}

type readWalk struct {
	reads, hasTests, keyTests map[string]bool
	// open holds the names some read reaches outside every key test on them that
	// holds; keyed counts, per name, the key tests that hold where the walk is, and
	// vacuous the places whose result a bool literal decides, where every key holds
	// and nothing is read. compares: an inlined `.where("…")` predicate may compare the
	// root.
	open        map[string]bool
	keyed       map[string]int
	vacuous     int
	whole       bool
	compares    bool
	inlineWhere bool
}

const whereFunction = "where"

func (w *readWalk) read(name string) {
	if w.vacuous > 0 {
		return
	}
	w.reads[name] = true
	if w.keyed[name] == 0 {
		w.open[name] = true
	}
}

// held walks e where a key test on each name of keys holds.
func (w *readWalk) held(keys keySet, e ast.Expr, shadowed bool) {
	if keys.every {
		w.vacuous++
	}
	for _, name := range keys.names {
		w.keyed[name]++
	}
	w.expr(e, shadowed)
	if keys.every {
		w.vacuous--
	}
	for _, name := range keys.names {
		w.keyed[name]--
	}
}

// keySet is the names a condition proves are keys of `vars` on one side of it.
// every: no evaluation takes that side — a bool literal's other one — so it proves
// every key, vacuously, and a read held there is no read.
type keySet struct {
	every bool
	names []string
}

func (k keySet) union(o keySet) keySet {
	if k.every || o.every {
		return keySet{every: true}
	}
	return keySet{names: append(slices.Clone(k.names), o.names...)}
}

func (k keySet) intersect(o keySet) keySet {
	switch {
	case k.every:
		return o
	case o.every:
		return k
	}
	var out []string
	for _, name := range k.names {
		if slices.Contains(o.names, name) {
			out = append(out, name)
		}
	}
	return keySet{names: out}
}

func (w *readWalk) expr(e ast.Expr, shadowed bool) {
	switch e.Kind() {
	case ast.IdentKind:
		if isVarsRoot(e, shadowed) && w.vacuous == 0 {
			w.whole = true
		}
	case ast.SelectKind:
		s := e.AsSelect()
		if isVarsRoot(s.Operand(), shadowed) {
			// has() evaluates its operand on a lazy map, so a presence test reads.
			w.read(s.FieldName())
			if s.IsTestOnly() {
				w.hasTests[s.FieldName()] = true
			}
			return
		}
		w.expr(s.Operand(), shadowed)
	case ast.CallKind:
		w.call(e.AsCall(), shadowed)
	case ast.ComprehensionKind:
		cp := e.AsComprehension()
		if !isVarsRoot(cp.IterRange(), shadowed) {
			w.expr(cp.IterRange(), shadowed)
		}
		w.expr(cp.AccuInit(), shadowed)
		inner := shadowed || isVarsName(cp.IterVar()) || isVarsName(cp.IterVar2()) || isVarsName(cp.AccuVar())
		w.expr(cp.LoopCondition(), inner)
		w.expr(cp.LoopStep(), inner)
		w.expr(cp.Result(), inner)
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			w.expr(el, shadowed)
		}
	case ast.MapKind, ast.StructKind:
		// The engine's rewrite does not descend into a literal (rewriteNode,
		// shared/cel/hosts.go), so a `.where("…")` inside one is not inlined.
		inline := w.inlineWhere
		w.inlineWhere = false
		if e.Kind() == ast.MapKind {
			for _, en := range e.AsMap().Entries() {
				me := en.AsMapEntry()
				w.expr(me.Key(), shadowed)
				w.expr(me.Value(), shadowed)
			}
		} else {
			for _, f := range e.AsStruct().Fields() {
				w.expr(f.AsStructField().Value(), shadowed)
			}
		}
		w.inlineWhere = inline
	}
}

func (w *readWalk) call(c ast.CallExpr, shadowed bool) {
	args := c.Args()
	member := c.IsMemberFunction()
	fn := c.FunctionName()
	switch {
	case member && fn == whereFunction && w.inlineWhere:
		w.expr(c.Target(), shadowed)
		if pred, ok := wherePredicate(c); ok {
			w.expr(pred, shadowed)
			w.compares = w.compares || !shadowed && cel.ComparesVars(pred)
		}
		return
	case !member && fn == operators.Conditional && len(args) == 3:
		whenTrue, whenFalse := impliedKeys(args[0], shadowed)
		w.expr(args[0], shadowed)
		w.held(whenTrue, args[1], shadowed)
		w.held(whenFalse, args[2], shadowed)
		return
	case !member && (fn == operators.LogicalAnd || fn == operators.LogicalOr):
		// A term's error reaches the result only where no other term decides it: where
		// every other term of `&&` is true, of `||` false.
		terms := logicalTerms(fn, args, nil)
		undecided := make([]keySet, len(terms))
		for i, t := range terms {
			whenTrue, whenFalse := impliedKeys(t, shadowed)
			undecided[i] = whenFalse
			if fn == operators.LogicalAnd {
				undecided[i] = whenTrue
			}
		}
		for i, t := range terms {
			var keys keySet
			for j, k := range undecided {
				if j != i {
					keys = keys.union(k)
				}
			}
			w.held(keys, t, shadowed)
		}
		return
	case !member && fn == operators.Index && len(args) == 2 && isVarsRoot(args[0], shadowed):
		if key, ok := stringLiteral(args[1]); ok {
			w.read(key)
		} else {
			w.expr(args[1], shadowed)
		}
		return
	case !member && c.FunctionName() == operators.In && len(args) == 2 && isVarsRoot(args[1], shadowed):
		if key, ok := stringLiteral(args[0]); ok {
			w.keyTests[key] = true
		} else {
			w.expr(args[0], shadowed)
		}
		return
	case c.FunctionName() == overloads.Size && !member && len(args) == 1 && isVarsRoot(args[0], shadowed),
		c.FunctionName() == overloads.Size && member && len(args) == 0 && isVarsRoot(c.Target(), shadowed):
		return
	}
	if member {
		w.expr(c.Target(), shadowed)
	}
	for _, a := range args {
		w.expr(a, shadowed)
	}
}

// impliedKeys reports the names condition e proves are keys of `vars` where it
// evaluates to true, and where it evaluates to false: `'<name>' in vars`, a bool
// literal, which proves every key on the side it never takes, and what `!`, `&&`,
// `||` and an equality with a bool literal build of them. Anything else proves
// nothing. An error is neither true nor false, so it needs no case.
func impliedKeys(e ast.Expr, shadowed bool) (whenTrue, whenFalse keySet) {
	if b, ok := boolLiteral(e); ok {
		if b {
			return keySet{}, keySet{every: true}
		}
		return keySet{every: true}, keySet{}
	}
	if e.Kind() != ast.CallKind || e.AsCall().IsMemberFunction() {
		return keySet{}, keySet{}
	}
	c := e.AsCall()
	args := c.Args()
	switch fn := c.FunctionName(); {
	case fn == operators.In && len(args) == 2 && isVarsRoot(args[1], shadowed):
		if name, ok := stringLiteral(args[0]); ok {
			return keySet{names: []string{name}}, keySet{}
		}
	case fn == operators.LogicalNot && len(args) == 1:
		whenTrue, whenFalse = impliedKeys(args[0], shadowed)
		return whenFalse, whenTrue
	case (fn == operators.LogicalAnd || fn == operators.LogicalOr) && len(args) > 0:
		whenTrue, whenFalse = impliedKeys(args[0], shadowed)
		for _, a := range args[1:] {
			t, f := impliedKeys(a, shadowed)
			if fn == operators.LogicalAnd {
				whenTrue, whenFalse = whenTrue.union(t), whenFalse.intersect(f)
			} else {
				whenTrue, whenFalse = whenTrue.intersect(t), whenFalse.union(f)
			}
		}
		return whenTrue, whenFalse
	case (fn == operators.Equals || fn == operators.NotEquals) && len(args) == 2:
		for i, a := range args {
			if b, ok := boolLiteral(a); ok {
				whenTrue, whenFalse = impliedKeys(args[1-i], shadowed)
				if b != (fn == operators.Equals) {
					return whenFalse, whenTrue
				}
				return whenTrue, whenFalse
			}
		}
	}
	return keySet{}, keySet{}
}

func boolLiteral(e ast.Expr) (bool, bool) {
	if e.Kind() != ast.LiteralKind {
		return false, false
	}
	b, ok := e.AsLiteral().Value().(bool)
	return b, ok
}

// logicalTerms flattens a chain of one logical operator, which CEL evaluates as one:
// a term that decides the result absorbs an error in any other.
func logicalTerms(fn string, args []ast.Expr, out []ast.Expr) []ast.Expr {
	for _, a := range args {
		if a.Kind() == ast.CallKind && !a.AsCall().IsMemberFunction() && a.AsCall().FunctionName() == fn {
			out = logicalTerms(fn, a.AsCall().Args(), out)
			continue
		}
		out = append(out, a)
	}
	return out
}

func stringLiteral(e ast.Expr) (string, bool) {
	if e.Kind() != ast.LiteralKind {
		return "", false
	}
	s, ok := e.AsLiteral().Value().(string)
	return s, ok
}

// whereInlinable reports whether the engine inlines the `.where("…")` predicates of
// expr before compiling it, mirroring rewriteHostsWhere (shared/cel/hosts.go), which
// is all or nothing: the text names soulprint or `.where(`, and every where-call its
// rewrite reaches has one string-literal predicate that parses and holds no
// where-call itself, on soulprint.hosts, on soulprint, or on another such where-call.
// Otherwise the engine refuses the expression and its InterpolationReads reads the
// raw text, so nothing is inlined here either.
func whereInlinable(expr string, root ast.Expr) bool {
	if !strings.Contains(expr, "soulprint") && !strings.Contains(expr, ".where(") {
		return false
	}
	for _, c := range rewrittenWhereCalls(root, nil) {
		pred, parsed := wherePredicate(c)
		if !parsed || containsWhereCall(pred) || !hostsReceiver(c.Target()) {
			return false
		}
	}
	return true
}

// rewrittenWhereCalls lists the where-calls the engine's rewrite reaches (rewriteNode):
// through a call's target and arguments, a select's operand and a list's elements,
// never into a map or struct literal. A comprehension is a macro the rewrite sees as
// a call, so its parts are reached too.
func rewrittenWhereCalls(e ast.Expr, out []ast.CallExpr) []ast.CallExpr {
	switch e.Kind() {
	case ast.CallKind:
		c := e.AsCall()
		if isWhereCall(e) {
			return rewrittenWhereCalls(c.Target(), append(out, c))
		}
		if c.IsMemberFunction() {
			out = rewrittenWhereCalls(c.Target(), out)
		}
		for _, a := range c.Args() {
			out = rewrittenWhereCalls(a, out)
		}
	case ast.SelectKind:
		out = rewrittenWhereCalls(e.AsSelect().Operand(), out)
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			out = rewrittenWhereCalls(el, out)
		}
	case ast.ComprehensionKind:
		cp := e.AsComprehension()
		for _, part := range []ast.Expr{cp.IterRange(), cp.AccuInit(), cp.LoopCondition(), cp.LoopStep(), cp.Result()} {
			out = rewrittenWhereCalls(part, out)
		}
	}
	return out
}

func isWhereCall(e ast.Expr) bool {
	return e.Kind() == ast.CallKind && e.AsCall().IsMemberFunction() && e.AsCall().FunctionName() == whereFunction
}

func containsWhereCall(e ast.Expr) bool {
	found := false
	ast.PostOrderVisit(e, ast.NewExprVisitor(func(n ast.Expr) {
		found = found || isWhereCall(n)
	}))
	return found
}

// wherePredicate parses the one string-literal argument of a where-call.
func wherePredicate(c ast.CallExpr) (ast.Expr, bool) {
	if len(c.Args()) != 1 {
		return nil, false
	}
	text, ok := stringLiteral(c.Args()[0])
	if !ok {
		return nil, false
	}
	p, err := macroParser()
	if err != nil {
		return nil, false
	}
	parsed, iss := p.Parse(common.NewTextSource(text))
	if iss != nil && len(iss.GetErrors()) > 0 {
		return nil, false
	}
	return parsed.Expr(), true
}

func hostsReceiver(e ast.Expr) bool {
	switch e.Kind() {
	case ast.IdentKind:
		return e.AsIdent() == "soulprint"
	case ast.SelectKind:
		s := e.AsSelect()
		return !s.IsTestOnly() && s.FieldName() == "hosts" && s.Operand().Kind() == ast.IdentKind && s.Operand().AsIdent() == "soulprint"
	case ast.CallKind:
		return isWhereCall(e)
	}
	return false
}

// isVarsRoot reports whether e is the `vars` root itself, a leading dot allowed.
func isVarsRoot(e ast.Expr, shadowed bool) bool {
	return !shadowed && e.Kind() == ast.IdentKind && isVarsName(e.AsIdent())
}

func isVarsName(name string) bool { return name == "vars" || name == ".vars" }
