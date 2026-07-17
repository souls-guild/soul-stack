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
