package cel

import (
	"sort"
	"strings"

	"github.com/google/cel-go/common/ast"
)

// RootReads is how one CEL root (`input`, `vars`, `incarnation`) is reached by an
// expression: the field names selected off it, plus Whole for a read no field
// name can be recovered from — the bare identifier (`size(input)`, `input == {}`)
// or the index form (`input['k']`, `input[k]`). On the `vars` root alone a literal
// index, `vars['k']`, is the field `k` (ADR-0082 amendment 2026-10-15, point 15):
// the lazy view reads exactly that name for it.
//
// Whole is what keeps a caller that narrows a map down to Fields from silently
// handing an expression less than it asked for: a shape this walk cannot name is
// reported as "all of it", never as "nothing".
type RootReads struct {
	Fields map[string]bool
	Whole  bool
}

// PredicateReads reports what a BARE CEL predicate reads off each root in roots.
// Bare means the whole string is the expression (`when:` / `changed_when:` /
// `failed_when:` / `retry.until`, [templating.md] §2.1) — an interpolated
// `${ … }` string is [Engine.InterpolationReads]'s job, not this one. Every root
// gets an entry; a root the predicate never names gets an empty one.
//
// Parsed WITHOUT macros (parseNoMacro), like [Engine.DetectSealed]. That is
// load-bearing rather than incidental: under the evaluation env
// `has(input.password)` is a test-only Select, which no field name survives, while
// here it stays a plain call over `input.password` and the field is seen. A caller that dropped that field would get `has(…) == false` — a
// different answer, with no error anywhere to say so.
//
// A root reached in a shape the AST holds no field name for sets Whole instead of
// contributing nothing. Same for a predicate that does not parse: it fails at
// eval on Keeper or on Soul either way, and reporting it as "reads nothing" would
// convert that failure into a quietly different result. Detection is by count —
// every `input.f` puts one `input` ident and one Select over that ident in the
// tree, so an ident the Selects do not account for is a whole-root read.
//
// BOUNDARY: the walk does not descend into a `.where("…")` string argument, the
// one place in this language where a string literal is itself a predicate
// ([Engine.selectsIncarnationField] does descend, deliberately). It does not have
// to here: `.where` is only legal on soulprint.hosts, and the flow-control env
// this serves refuses both — `soulprint.hosts` is ErrUnsupported there and any
// other receiver is a compile error. A `where`-bearing predicate therefore fails
// with or without narrowing. Enabling `.where` in flow-control would make this a
// hole, and that is the change that must revisit it.
func (e *Engine) PredicateReads(expr string, roots []string) map[string]RootReads {
	fields := make(map[string]map[string]bool, len(roots))
	idents := make(map[string]int, len(roots))
	selects := make(map[string]int, len(roots))
	for _, root := range roots {
		fields[root] = map[string]bool{}
	}

	whole := expr != ""
	if expr != "" {
		if parsed, perr := e.parseNoMacro(expr); perr == nil {
			whole = false
			ast.PostOrderVisit(parsed.Expr(), ast.NewExprVisitor(func(n ast.Expr) {
				switch n.Kind() {
				case ast.IdentKind:
					if _, ok := fields[n.AsIdent()]; ok {
						idents[n.AsIdent()]++
					}
				case ast.SelectKind:
					base, field, ok := selectBaseField(n)
					if !ok {
						return
					}
					if _, tracked := fields[base]; tracked {
						selects[base]++
						fields[base][field] = true
					}
				case ast.CallKind:
					if field, ok := varsLiteralIndex(n); ok {
						if _, tracked := fields[varsRoot]; tracked {
							selects[varsRoot]++
							fields[varsRoot][field] = true
						}
					}
				}
			}))
		}
	}

	out := make(map[string]RootReads, len(roots))
	for _, root := range roots {
		out[root] = RootReads{
			Fields: fields[root],
			Whole:  whole || idents[root] > selects[root],
		}
	}
	return out
}

// HostPredicateReads is [Engine.PredicateReads] for a bare predicate that may call
// `soulprint.hosts.where("…")` — a `where:` — whose string argument is itself a
// predicate: it is inlined first, as [Engine.InterpolationReads] does for a block, so
// what it reads is seen.
func (e *Engine) HostPredicateReads(expr string, roots []string) map[string]RootReads {
	if rewritten, err := e.rewriteHostsWhere(expr, true); err == nil {
		expr = rewritten
	}
	return e.PredicateReads(expr, roots)
}

// InterpolationReads is [Engine.PredicateReads] over an INTERPOLATED string — a
// `params:` value, a `vars:` value, an `on:` element, an `apply: input:` value — as
// opposed to a bare predicate. Every `${ … }` block is scanned and the results are
// unioned per root: Fields accumulate, Whole is true if ANY block reads the root
// whole.
//
// A string with no blocks reads nothing (a literal cell), which is the one case
// where "nothing" is the honest answer rather than the fail-closed one.
//
// ★ Each block is scanned AFTER [Engine.rewriteHostsWhere] inlines it. A
// `soulprint.hosts.where("<predicate>")` argument is a string LITERAL, and no AST
// walk sees a root inside a literal — so `soulprint.hosts.where("sid ==
// soulprint.self.sid")` would report reading `soulprint.hosts` and nothing else,
// while at eval it reads this host's `self`. A block the rewrite refuses is scanned
// raw: it will not compile either way, and reporting "reads nothing" for text that
// is about to fail is how a caller narrowing on this answer gets it wrong in the
// permissive direction.
func (e *Engine) InterpolationReads(raw string, roots []string) map[string]RootReads {
	out := make(map[string]RootReads, len(roots))
	for _, root := range roots {
		out[root] = RootReads{Fields: map[string]bool{}}
	}
	segs, err := e.scanInterpolation(raw)
	if err != nil {
		// Unscannable text (an unbalanced marker) fails at render with a message that
		// has a position; claiming it reads nothing would let a caller act on it first.
		for _, root := range roots {
			out[root] = RootReads{Fields: map[string]bool{}, Whole: true}
		}
		return out
	}
	for _, s := range segs {
		if !s.expr {
			continue
		}
		text := s.text
		if rewritten, rerr := e.rewriteHostsWhere(text, true); rerr == nil {
			text = rewritten
		}
		for root, r := range e.PredicateReads(text, roots) {
			merged := out[root]
			merged.Whole = merged.Whole || r.Whole
			for field := range r.Fields {
				merged.Fields[field] = true
			}
			out[root] = merged
		}
	}
	return out
}

// isVarsIndex — a node of the form `vars[<expr>]` (CEL index operator `_[_]` over
// the bare identifier `vars`). cel-go represents `a[b]` as a global call with
// FunctionName == operators.Index and two arguments; the first argument is
// IdentKind `vars`. A member call (`x[y]()`) is excluded by the node's shape.
func isVarsIndex(n ast.Expr) bool {
	c := n.AsCall()
	if c.IsMemberFunction() || c.FunctionName() != "_[_]" {
		return false
	}
	args := c.Args()
	if len(args) == 0 {
		return false
	}
	op := args[0]
	return op.Kind() == ast.IdentKind && op.AsIdent() == "vars"
}

// varsLiteralIndex returns k for `vars['k']`: an index on the bare `vars` root by a
// string literal.
func varsLiteralIndex(n ast.Expr) (string, bool) {
	if !isVarsIndex(n) {
		return "", false
	}
	args := n.AsCall().Args()
	if len(args) != 2 || args[1].Kind() != ast.LiteralKind {
		return "", false
	}
	key, ok := args[1].AsLiteral().Value().(string)
	return key, ok
}

// RegisterNames returns the sorted names expr reads as `register.<name>` or
// `register.hosts.<name>`, `self` excluded, read from the parse tree — so a name that
// sits inside a literal, a value the Keeper substituted, is not one. ok is false when
// expr does not parse; the caller then falls back to the text reading
// (config.ExtractRegisterRefs), which misreads in both directions — but only text
// that fails to compile anyway reaches it.
func (e *Engine) RegisterNames(expr string) ([]string, bool) {
	parsed, err := e.parseNoMacro(expr)
	if err != nil {
		return nil, false
	}
	seen := map[string]bool{}
	ast.PostOrderVisit(parsed.Expr(), ast.NewExprVisitor(func(n ast.Expr) {
		if n.Kind() != ast.SelectKind {
			return
		}
		s := n.AsSelect()
		op := s.Operand()
		switch {
		case isRegisterIdentNode(op):
			if f := s.FieldName(); f != RegisterSelf && f != registerHostsKey {
				seen[f] = true
			}
		case op.Kind() == ast.SelectKind && op.AsSelect().FieldName() == registerHostsKey &&
			isRegisterIdentNode(op.AsSelect().Operand()):
			seen[s.FieldName()] = true
		}
	}))
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, true
}

// isRegisterIdentNode reports the `register` root, a leading dot allowed.
func isRegisterIdentNode(n ast.Expr) bool {
	return n.Kind() == ast.IdentKind && strings.TrimPrefix(n.AsIdent(), ".") == registerIdent
}
