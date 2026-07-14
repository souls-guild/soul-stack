package cel

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/parser"
)

// soulprint.hosts + .where(<predicate>) (slice E3a, [ADR-010]).
//
// Mechanism: a compile-time AST rewrite of a static literal predicate into a
// native CEL filter-comprehension (NOT runtime execution of the string):
//
//	soulprint.hosts.where("<predicate>")  →  soulprint.hosts.filter(__host, <predicate with __host.* fields>)
//	soulprint.where("<predicate>")        ≡  soulprint.hosts.where("<predicate>")  (synonym)
//
// Key round-trip invariant: the rewrite phase parses WITHOUT macros
// (Engine.noMacroParser), so exists/all/map/filter/exists_one/.where stay plain
// CallKind and serialize correctly via parser.Unparse. With env.Parse (macros on)
// the macros would expand into ComprehensionKind, which this cel-go version's
// Unparse does NOT serialize (opaque "unsupported expression"). The final
// env.Compile (macros on) re-expands .filter/.exists back into a comprehension
// natively — which makes covens.exists(c, c=='db') in a predicate fully work.
//
// rewriteHostsWhere phases:
//   - parseNoMacro of the whole expression into an AST (macros NOT expanded: .where
//     and exists/all/filter are plain member-call CallKind);
//   - PostOrder walk: find `where`/1-arg call nodes whose receiver reduces to
//     soulprint.hosts / a soulprint.where(...) projection;
//   - validate the receiver (only soulprint.hosts/soulprint.where) and the argument
//     (only a string literal) — else a clear compile error;
//   - build the filter member-call: receiver.filter(__host, <pred>), where the
//     predicate is parsed separately (also macro-free) and its "bare" element
//     fields (role/covens/os.family/…) are qualified to __host.<field>; the outer
//     context (incarnation.*/input.*/…) and iter-vars of nested
//     exists/all/map/filter in the predicate are untouched;
//   - parser.Unparse of the whole rewritten tree back into a string → env.Compile
//     once (Unparse escapes literals correctly — injection is excluded by
//     construction, safer than manual RenumberIDs).
//
// [ADR-010]: docs/adr/0010-templating.md

// hostIterPrefix is the iter-var prefix of the filter-comprehension. Reserved in
// the config validator (loopReservedPrefixes) so a loop variable `as:`/`index_as:`
// does not collide with the built-in iter-var.
const hostIterPrefix = "__host"

// usesHostsAccessor is a coarse test for expressions that MUST go through the
// rewriteHostsWhere AST pass: those containing `soulprint` (hosts/where) or any
// `.where(` (so a generic .where on a non-soulprint receiver gets a clear
// validation error, not a vague "undeclared reference to where"). Precise
// classification happens on the AST; this test only decides "should the parser
// touch the expression" (hot path: most expressions skip it).
func usesHostsAccessor(expr string) bool {
	return strings.Contains(expr, "soulprint") || strings.Contains(expr, ".where(")
}

// rewriteHostsWhere rewrites soulprint.hosts.where(...)/soulprint.where(...) into
// a native filter-comprehension and returns the resulting CEL string. If the
// expression does not use the hosts accessor, it returns expr unchanged (hot
// path). allowHosts=false → touching soulprint.hosts/soulprint.where is a
// validation error (destiny-pass isolation, orchestration.md §4.1).
func (e *Engine) rewriteHostsWhere(expr string, allowHosts bool) (string, error) {
	if !usesHostsAccessor(expr) {
		return expr, nil
	}

	parsed, err := e.parseNoMacro(expr)
	if err != nil {
		return "", &ErrCompile{Expr: expr, Err: err}
	}
	root := parsed.Expr()

	if !containsWhereCall(root) {
		// soulprint.hosts without .where (e.g. soulprint.hosts.size()).
		if containsHostsAccessor(root) && !allowHosts {
			return "", &ErrUnsupported{Expr: expr, Feature: "soulprint.hosts (scenario-only; недоступен в destiny-проходе)"}
		}
		return expr, nil
	}

	// There is a .where(...). Isolation: with allowHosts=false, touching
	// soulprint.hosts/soulprint.where is an error (destiny pass). A generic .where
	// on a non-soulprint receiver is rejected below with a clear rewriteWhereCall error.
	if !allowHosts && containsHostsAccessor(root) {
		return "", &ErrUnsupported{Expr: expr, Feature: "soulprint.hosts (scenario-only; недоступен в destiny-проходе)"}
	}

	fac := ast.NewExprFactory()
	iter := freeIterVar(e, root)

	rewritten, err := e.rewriteNode(fac, root, iter)
	if err != nil {
		return "", &ErrCompile{Expr: expr, Err: err}
	}

	out, err := parser.Unparse(rewritten, parsed.SourceInfo())
	if err != nil {
		return "", &ErrCompile{Expr: expr, Err: fmt.Errorf("unparse переписанного дерева: %w", err)}
	}
	return out, nil
}

// parseNoMacro parses the expression WITHOUT macros: exists/all/map/filter/
// exists_one/.where stay plain CallKind (round-trippable by Unparse). Used only in
// the rewrite phase; final compilation goes through env (macros on). An error is
// syntactic (clear, not opaque).
func (e *Engine) parseNoMacro(expr string) (*ast.AST, error) {
	parsed, iss := e.noMacroParser.Parse(common.NewTextSource(expr))
	if iss != nil && len(iss.GetErrors()) > 0 {
		return nil, fmt.Errorf("%s", iss.ToDisplayString())
	}
	return parsed, nil
}

// rewriteNode recursively walks the tree, rewriting where-calls over
// soulprint.hosts/soulprint.where into a filter-comprehension. Other nodes are
// copied as-is (nested where inside a predicate is rejected separately — see
// buildFilter).
func (e *Engine) rewriteNode(fac ast.ExprFactory, node ast.Expr, iter string) (ast.Expr, error) {
	switch node.Kind() {
	case ast.CallKind:
		c := node.AsCall()
		if c.IsMemberFunction() && c.FunctionName() == "where" {
			return e.rewriteWhereCall(fac, node, iter)
		}
		args := make([]ast.Expr, len(c.Args()))
		for i, a := range c.Args() {
			rw, err := e.rewriteNode(fac, a, iter)
			if err != nil {
				return nil, err
			}
			args[i] = rw
		}
		if c.IsMemberFunction() {
			tgt, err := e.rewriteNode(fac, c.Target(), iter)
			if err != nil {
				return nil, err
			}
			return fac.NewMemberCall(node.ID(), c.FunctionName(), tgt, args...), nil
		}
		return fac.NewCall(node.ID(), c.FunctionName(), args...), nil
	case ast.SelectKind:
		s := node.AsSelect()
		op, err := e.rewriteNode(fac, s.Operand(), iter)
		if err != nil {
			return nil, err
		}
		if s.IsTestOnly() {
			return fac.NewPresenceTest(node.ID(), op, s.FieldName()), nil
		}
		return fac.NewSelect(node.ID(), op, s.FieldName()), nil
	case ast.ListKind:
		l := node.AsList()
		elems := make([]ast.Expr, len(l.Elements()))
		for i, el := range l.Elements() {
			rw, err := e.rewriteNode(fac, el, iter)
			if err != nil {
				return nil, err
			}
			elems[i] = rw
		}
		return fac.NewList(node.ID(), elems, l.OptionalIndices()), nil
	default:
		return fac.CopyExpr(node), nil
	}
}

// rewriteWhereCall rewrites one `<receiver>.where(<arg>)` into
// `<hosts-receiver>.filter(iter, <pred>)`. Validates the receiver (only
// soulprint.hosts/soulprint.where) and the argument (only a string literal).
func (e *Engine) rewriteWhereCall(fac ast.ExprFactory, node ast.Expr, iter string) (ast.Expr, error) {
	c := node.AsCall()
	if len(c.Args()) != 1 {
		return nil, fmt.Errorf(".where(...) принимает ровно один аргумент-предикат (получено %d)", len(c.Args()))
	}

	hostsReceiver, err := e.hostsReceiver(fac, c.Target(), iter)
	if err != nil {
		return nil, err
	}

	arg := c.Args()[0]
	if arg.Kind() != ast.LiteralKind {
		return nil, fmt.Errorf(".where(...) predicate must be a static string literal")
	}
	predStr, ok := arg.AsLiteral().Value().(string)
	if !ok {
		return nil, fmt.Errorf(".where(...) predicate must be a static string literal")
	}

	predParsed, err := e.parseNoMacro(predStr)
	if err != nil {
		return nil, fmt.Errorf("predicate %q: %w", predStr, err)
	}
	pred := predParsed.Expr()
	if nestedWhere(pred) {
		return nil, fmt.Errorf("nested .where(...) внутри предиката не поддерживается")
	}

	boundPred := e.qualifyPredicate(fac, pred, iter, nil)
	return fac.NewMemberCall(node.ID(), "filter", hostsReceiver, fac.NewIdent(0, iter), boundPred), nil
}

// hostsReceiver reduces a where-call receiver to a host-list expression:
//   - soulprint.hosts → as-is;
//   - soulprint (bare ident) → soulprint.where(...) ≡ soulprint.hosts.where(...):
//     the receiver reduces to soulprint.hosts;
//   - soulprint.where("<p>") as an outer .where receiver → expands into filter.
//
// Other receivers are an error (a generic .where on an arbitrary list is forbidden).
func (e *Engine) hostsReceiver(fac ast.ExprFactory, recv ast.Expr, iter string) (ast.Expr, error) {
	if isSoulprintHosts(recv) {
		return fac.CopyExpr(recv), nil
	}
	if isSoulprintIdent(recv) {
		// soulprint.where(...) is a synonym for soulprint.hosts.where(...): the
		// inner where's receiver = soulprint, so we substitute soulprint.hosts.
		return fac.NewSelect(0, fac.NewIdent(0, "soulprint"), "hosts"), nil
	}
	if recv.Kind() == ast.CallKind {
		c := recv.AsCall()
		if c.IsMemberFunction() && c.FunctionName() == "where" {
			// A nested where as the outer .where receiver — expand recursively
			// (it must itself reduce to soulprint.hosts / soulprint).
			return e.rewriteWhereCall(fac, recv, iter)
		}
	}
	return nil, fmt.Errorf(".where(...) разрешён только на soulprint.hosts / soulprint.where(...); generic .where на произвольном списке запрещён")
}

// qualifyPredicate qualifies "bare" element fields in the predicate (role,
// covens, os.family, …) to iter.<field>; identifiers of the known outer context
// (incarnation/input/register/soulprint/essence/vars + the iter-var itself) are
// untouched. So the predicate inside filter references the current element's
// fields __host.*, while the outer context resolves from the activation.
//
// bound holds the iter-vars of nested comprehension macros
// (covens.exists(c, …)/role.all(…)/…), declared by the macro's first argument.
// Inside such a macro's body they are NOT qualified (they are the macro's local
// variables, not element fields). Since the rewrite phase parses macro-free, a
// macro here is an ordinary member-call: the receiver is qualified as an element
// field (covens → __host.covens), and the body with the extra bound variable.
func (e *Engine) qualifyPredicate(fac ast.ExprFactory, node ast.Expr, iter string, bound map[string]bool) ast.Expr {
	switch node.Kind() {
	case ast.IdentKind:
		name := node.AsIdent()
		if name == iter || predicateContextRoots[name] || bound[name] {
			return fac.CopyExpr(node)
		}
		return fac.NewSelect(node.ID(), fac.NewIdent(0, iter), name)
	case ast.SelectKind:
		s := node.AsSelect()
		op := e.qualifyPredicate(fac, s.Operand(), iter, bound)
		if s.IsTestOnly() {
			return fac.NewPresenceTest(node.ID(), op, s.FieldName())
		}
		return fac.NewSelect(node.ID(), op, s.FieldName())
	case ast.CallKind:
		c := node.AsCall()
		if macroVar, body, ok := comprehensionMacro(c); ok {
			// The macro body sees its iter-var as local: bind it for the duration
			// of walking the body arguments.
			inner := withBound(bound, macroVar)
			args := make([]ast.Expr, len(c.Args()))
			args[0] = fac.CopyExpr(c.Args()[0]) // iter-var ident — leave as-is
			for _, i := range body {
				args[i] = e.qualifyPredicate(fac, c.Args()[i], iter, inner)
			}
			return fac.NewMemberCall(node.ID(), c.FunctionName(), e.qualifyPredicate(fac, c.Target(), iter, bound), args...)
		}
		args := make([]ast.Expr, len(c.Args()))
		for i, a := range c.Args() {
			args[i] = e.qualifyPredicate(fac, a, iter, bound)
		}
		if c.IsMemberFunction() {
			return fac.NewMemberCall(node.ID(), c.FunctionName(), e.qualifyPredicate(fac, c.Target(), iter, bound), args...)
		}
		return fac.NewCall(node.ID(), c.FunctionName(), args...)
	case ast.ListKind:
		l := node.AsList()
		elems := make([]ast.Expr, len(l.Elements()))
		for i, el := range l.Elements() {
			elems[i] = e.qualifyPredicate(fac, el, iter, bound)
		}
		return fac.NewList(node.ID(), elems, l.OptionalIndices())
	default:
		return fac.CopyExpr(node)
	}
}

// comprehensionMacro recognizes a receiver-style comprehension-macro call
// (exists/all/exists_one/existsOne/map/filter) whose first argument is an ident
// declaring the macro's iter-var. Returns that variable's name and the indices of
// the body arguments (everything but the first: predicate/transform). Under the
// macro-free rewrite parse these macros are ordinary member-calls; the final
// env.Compile expands them natively.
func comprehensionMacro(c ast.CallExpr) (string, []int, bool) {
	if !c.IsMemberFunction() {
		return "", nil, false
	}
	switch c.FunctionName() {
	case "exists", "all", "exists_one", "existsOne", "filter":
		if len(c.Args()) != 2 || c.Args()[0].Kind() != ast.IdentKind {
			return "", nil, false
		}
		return c.Args()[0].AsIdent(), []int{1}, true
	case "map":
		// map(v, transform) or map(v, filter, transform).
		if (len(c.Args()) == 2 || len(c.Args()) == 3) && c.Args()[0].Kind() == ast.IdentKind {
			body := make([]int, 0, len(c.Args())-1)
			for i := 1; i < len(c.Args()); i++ {
				body = append(body, i)
			}
			return c.Args()[0].AsIdent(), body, true
		}
	}
	return "", nil, false
}

// withBound returns a copy of bound with name added (non-mutating, so the binding
// does not leak into sibling branches of the walk).
func withBound(bound map[string]bool, name string) map[string]bool {
	out := make(map[string]bool, len(bound)+1)
	for k := range bound {
		out[k] = true
	}
	out[name] = true
	return out
}

// predicateContextRoots are the outer-context root identifiers that inside a
// .where(...) predicate are NOT qualified to iter.<…> (they resolve from the
// activation). Symmetric with contextVars. The iter-var is added separately.
var predicateContextRoots = map[string]bool{
	"input":       true,
	"register":    true,
	"incarnation": true,
	"soulprint":   true,
	"essence":     true,
	"vars":        true,
}

// containsHostsAccessor reports whether the tree uses soulprint.hosts or
// soulprint.where(...) anywhere (for the destiny-pass isolation gate).
func containsHostsAccessor(root ast.Expr) bool {
	found := false
	ast.PostOrderVisit(root, ast.NewExprVisitor(func(e ast.Expr) {
		if isSoulprintHosts(e) {
			found = true
			return
		}
		if e.Kind() == ast.CallKind {
			c := e.AsCall()
			if c.IsMemberFunction() && c.FunctionName() == "where" && isSoulprintIdent(c.Target()) {
				found = true
			}
		}
	}))
	return found
}

// containsWhereCall reports whether the tree has any `where` member-call.
func containsWhereCall(root ast.Expr) bool {
	found := false
	ast.PostOrderVisit(root, ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() == ast.CallKind {
			c := e.AsCall()
			if c.IsMemberFunction() && c.FunctionName() == "where" {
				found = true
			}
		}
	}))
	return found
}

// nestedWhere reports whether a nested where-call exists anywhere in the predicate tree.
func nestedWhere(pred ast.Expr) bool {
	found := false
	ast.PostOrderVisit(pred, ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() == ast.CallKind {
			c := e.AsCall()
			if c.IsMemberFunction() && c.FunctionName() == "where" {
				found = true
			}
		}
	}))
	return found
}

// isSoulprintHosts — a node of the form soulprint.hosts (Select hosts on ident soulprint).
func isSoulprintHosts(e ast.Expr) bool {
	if e.Kind() != ast.SelectKind {
		return false
	}
	s := e.AsSelect()
	return !s.IsTestOnly() && s.FieldName() == "hosts" && isSoulprintIdent(s.Operand())
}

// isSoulprintIdent — the `soulprint` identifier node.
func isSoulprintIdent(e ast.Expr) bool {
	return e.Kind() == ast.IdentKind && e.AsIdent() == "soulprint"
}

// freeIterVar returns a guaranteed-free iter-var name for the
// filter-comprehension: hostIterPrefix, or hostIterPrefix+counter if the base
// name occurs among the expression's identifiers/fields OR inside where-predicate
// string literals (a collision — e.g. the author used `__host` as an element
// field name or a context identifier). Predicate literals are parsed separately:
// their identifiers are invisible in the outer AST.
func freeIterVar(e *Engine, root ast.Expr) string {
	used := map[string]bool{}
	collect := func(node ast.Expr) {
		ast.PostOrderVisit(node, ast.NewExprVisitor(func(n ast.Expr) {
			switch n.Kind() {
			case ast.IdentKind:
				used[n.AsIdent()] = true
			case ast.SelectKind:
				used[n.AsSelect().FieldName()] = true
			case ast.CallKind:
				// Parse the where-predicate content (string literal) to account for
				// its identifiers when choosing a free iter-var.
				c := n.AsCall()
				if c.IsMemberFunction() && c.FunctionName() == "where" && len(c.Args()) == 1 && c.Args()[0].Kind() == ast.LiteralKind {
					if s, ok := c.Args()[0].AsLiteral().Value().(string); ok {
						if pp, err := e.parseNoMacro(s); err == nil {
							ast.PostOrderVisit(pp.Expr(), ast.NewExprVisitor(func(p ast.Expr) {
								switch p.Kind() {
								case ast.IdentKind:
									used[p.AsIdent()] = true
								case ast.SelectKind:
									used[p.AsSelect().FieldName()] = true
								}
							}))
						}
					}
				}
			}
		}))
	}
	collect(root)

	if !used[hostIterPrefix] {
		return hostIterPrefix
	}
	for i := 0; ; i++ {
		cand := fmt.Sprintf("%s%d", hostIterPrefix, i)
		if !used[cand] {
			return cand
		}
	}
}
