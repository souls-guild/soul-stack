package cel

import (
	"fmt"

	"github.com/google/cel-go/common/ast"
)

// ErrVarIndexForm — interpolation reaches the vars layer in index form
// (`vars['k']` / `vars[expr]`) instead of select form (`vars.k`). Building the
// var→var dependency graph (resolveVarLayer) needs the key statically known from
// the AST, which an arbitrary (or dynamic `vars[input.x]`) index does not
// guarantee. Rejected entirely and deterministically — a single boundary is
// simpler for the author (canonical vars.* is the only form, like
// soulprint.self.<path> in ADR-018). Dedicated sentinel so the caller can tell it
// apart from the syntactic ErrCompile via errors.Is.
var ErrVarIndexForm = fmt.Errorf("accessing vars via the index form vars[...] is not supported - use vars.<name>")

// VarRefs extracts the `vars.<X>` names referenced by an interpolation string raw
// (`${ … }` blocks). Mirror of DetectSealed (seal.go): scanInterpolation →
// per-block parseNoMacro → PostOrderVisit → collect selectBaseField where
// base=="vars". This is an AST walk, not regex: `vars.x` in literal text OUTSIDE
// `${ … }` is not a reference, and `vars` inside a CEL string literal (`"vars.x"`)
// is not either (that's a StringConstant, not a Select).
//
// Returns names in order of first appearance in PostOrderVisit (deduplicated);
// order is not significant for the caller (resolveVarLayer builds a graph). raw
// with no `${ … }` or no vars references → empty slice, nil error.
//
// Index form `vars['k']` / `vars[expr]` → [ErrVarIndexForm] (deterministic, see its
// doc): the key name is not extracted from the AST uniformly.
//
// A syntactically broken block never reaches per-block parseNoMacro:
// scanInterpolation (parseBlock) gates the block boundary via env.Parse and returns
// *ErrCompile earlier on an invalid expression. The `continue` on perr below is
// defensive (mirror of DetectSealed, in case parseBlock is relaxed or called
// directly): VarRefs doesn't duplicate validation, it just collects references from
// parseable expressions.
func (e *Engine) VarRefs(raw string) ([]string, error) {
	segs, err := e.scanInterpolation(raw)
	if err != nil {
		return nil, err
	}
	var refs []string
	seen := map[string]bool{}
	for _, s := range segs {
		if !s.expr {
			continue
		}
		parsed, perr := e.parseNoMacro(s.text)
		if perr != nil {
			continue // broken CEL — not our concern (mirror of DetectSealed)
		}
		var visitErr error
		ast.PostOrderVisit(parsed.Expr(), ast.NewExprVisitor(func(n ast.Expr) {
			if visitErr != nil {
				return
			}
			switch n.Kind() {
			case ast.SelectKind:
				if base, field, ok := selectBaseField(n); ok && base == "vars" {
					if !seen[field] {
						seen[field] = true
						refs = append(refs, field)
					}
				}
			case ast.CallKind:
				if isVarsIndex(n) {
					visitErr = fmt.Errorf("%w (expression %q)", ErrVarIndexForm, s.text)
				}
			}
		}))
		if visitErr != nil {
			return nil, visitErr
		}
	}
	return refs, nil
}

// RootReads is how one CEL root (`input`, `vars`, `incarnation`) is reached by an
// expression: the field names selected off it, plus Whole for a read no field
// name can be recovered from — the bare identifier (`size(input)`, `input == {}`)
// or the index form (`input['k']`, `input[k]`).
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
// `${ … }` string is [Engine.VarRefs]'s job, not this one. Every root gets an
// entry; a root the predicate never names gets an empty one.
//
// Parsed WITHOUT macros (parseNoMacro), like [Engine.DetectSealed] and
// [Engine.VarRefs]. That is load-bearing rather than incidental: under the
// evaluation env `has(input.password)` is a test-only Select, which no field name
// survives, while here it stays a plain call over `input.password` and the field
// is seen. A caller that dropped that field would get `has(…) == false` — a
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
