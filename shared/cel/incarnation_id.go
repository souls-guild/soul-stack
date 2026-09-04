package cel

// The `incarnation.name` → `incarnation.id` root rename ([ADR-0085], NIM-730) and
// the static half of its compatibility window.
//
// The RUNTIME half is [Vars.incarnationRoot]: both keys are in the activation, so
// a scenario on either spelling evaluates. That is deliberately silent — an
// engine that refused the old root would break every service repository at once,
// which is what the window exists to avoid. The consequence is that nothing about
// a run says the file is on a retired spelling, and the drop at the end of the
// window turns each remaining one into a `no such key` at EVALUATION: `incarnation`
// is `cel.DynType`, so no compile step and no type check sees it coming, and in
// the flow-control environment the failure lands mid-run on the host, after
// earlier tasks have applied ([ADR-0085] §"The stale CEL root is a runtime
// failure, not a compile error").
//
// So the detector below is the whole static catcher the platform has for this
// rename, and soul-lint's `incarnation_name_legacy_root` is built on it. It is
// AST-based for the same reason the `compute` rules are: `incarnation.name` in
// prose, and inside a CEL string CONSTANT, are not references, and a regex would
// disagree with the engine in exactly those places. The one string literal that
// IS descended into is a `.where(…)` predicate, because the engine parses that
// one too — see [Engine.selectsIncarnationField].
//
// [ADR-0085]: docs/adr/0085-entity-id-and-label.md

import celast "github.com/google/cel-go/common/ast"

// hostsWhereFunc is the host-accessor's member function, whose single string
// argument is a PREDICATE the engine parses rather than a constant it passes
// through (hosts.go). Spelled out here for the same reason [indexOperator] is.
const hostsWhereFunc = "where"

const (
	// incarnationRootName is the CEL root carrying the incarnation's own facts.
	incarnationRootName = "incarnation"
	// incarnationIDKey is the identifier field on that root.
	incarnationIDKey = "id"
	// legacyIncarnationIDKey is its pre-[ADR-0085] spelling.
	legacyIncarnationIDKey = "name"
)

// ExpressionReadsLegacyIncarnationID reports whether a WHOLE-STRING CEL
// expression (an expression key: `when:`, `where:`, `loop.when:`) selects
// `incarnation.name` — the retired spelling of `incarnation.id`.
//
// Only the SELECT of that one field counts. `incarnation.service` is not a hit,
// and neither is a bare `incarnation` handed to `has()`/`size()`: this reports
// what the author wrote, and the rename it asks for is of a field, not the root.
// The index form `incarnation['name']` IS a hit — it selects the same key by a
// different syntax, and staying quiet on it would leave a way past the only
// static catcher there is.
//
// Unparseable text → false: a syntax error belongs to the compiler, which reports
// it with a position (mirrors [Engine.VarRefs] / [Engine.ExpressionReferencesCompute]).
func (e *Engine) ExpressionReadsLegacyIncarnationID(expr string) bool {
	if !containsIdentText(expr, incarnationRootName) {
		return false
	}
	parsed, err := e.parseNoMacro(expr)
	if err != nil {
		return false
	}
	return e.selectsIncarnationField(parsed.Expr(), legacyIncarnationIDKey)
}

// InterpolationReadsLegacyIncarnationID is
// [Engine.ExpressionReadsLegacyIncarnationID] over an interpolated string
// (`params:` values, `vars:` values, `on:` elements, `loop.items:`): true when ANY
// `${ … }` block selects the retired field. Text around the blocks is literal and
// is not searched — "the incarnation.name is written here" in a `description:` is
// prose, not a reference.
func (e *Engine) InterpolationReadsLegacyIncarnationID(raw string) bool {
	if !containsIdentText(raw, incarnationRootName) {
		return false
	}
	segs, err := e.scanInterpolation(raw)
	if err != nil {
		return false
	}
	for _, s := range segs {
		if !s.expr {
			continue
		}
		if e.ExpressionReadsLegacyIncarnationID(s.text) {
			return true
		}
	}
	return false
}

// selectsIncarnationField walks the parsed expression for a read of
// `incarnation.<field>` in either readable form: the Select (`incarnation.name`)
// and the index operator over a string literal (`incarnation['name']`). Macros
// are off in [Engine.parseNoMacro], so `has(incarnation.name)` is a call over a
// plain Select and funnels through the same node.
//
// It also descends into a `.where("…")` PREDICATE, which is the one string
// literal in this language that is not a constant: the host-accessor rewrite
// parses it and leaves `incarnation` resolving from the activation rather than
// qualifying it to the iteration variable ([predicateContextRoots]). So
// `soulprint.hosts.where("incarnation.name == 'x'")` really does read the retired
// root, and treating the argument as ordinary text would make the platform's only
// static catcher silent on it. Every OTHER string literal is a constant and is
// correctly not descended into.
func (e *Engine) selectsIncarnationField(root celast.Expr, field string) bool {
	found := false
	celast.PostOrderVisit(root, celast.NewExprVisitor(func(n celast.Expr) {
		if found {
			return
		}
		switch n.Kind() {
		case celast.SelectKind:
			s := n.AsSelect()
			if isIncarnationIdent(s.Operand()) && s.FieldName() == field {
				found = true
			}
		case celast.CallKind:
			c := n.AsCall()
			if c.IsMemberFunction() {
				if e.predicateSelectsIncarnationField(c, field) {
					found = true
				}
				return
			}
			if c.FunctionName() != indexOperator {
				return
			}
			args := c.Args()
			if len(args) != 2 || !isIncarnationIdent(args[0]) || args[1].Kind() != celast.LiteralKind {
				return
			}
			if key, ok := args[1].AsLiteral().Value().(string); ok && key == field {
				found = true
			}
		}
	}))
	return found
}

// predicateSelectsIncarnationField re-parses a `.where("…")` argument and asks
// the same question of it. One level of re-parse per call node; the nesting a
// predicate may itself contain is handled by the recursion in
// [Engine.selectsIncarnationField], which this calls back into.
//
// A predicate that does not parse is not a finding — the compiler reports a
// syntax error there with a position, exactly as for a top-level expression.
func (e *Engine) predicateSelectsIncarnationField(c celast.CallExpr, field string) bool {
	if c.FunctionName() != hostsWhereFunc {
		return false
	}
	args := c.Args()
	if len(args) != 1 || args[0].Kind() != celast.LiteralKind {
		return false
	}
	src, ok := args[0].AsLiteral().Value().(string)
	if !ok {
		return false
	}
	parsed, err := e.parseNoMacro(src)
	if err != nil {
		return false
	}
	return e.selectsIncarnationField(parsed.Expr(), field)
}

func isIncarnationIdent(e celast.Expr) bool {
	return e != nil && e.Kind() == celast.IdentKind && e.AsIdent() == incarnationRootName
}
