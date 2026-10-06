package cel

import (
	"strings"

	"github.com/google/cel-go/common/ast"
)

// internalIdentFeature is the [ErrUnsupported] feature of [Engine.guardInternalIdents].
const internalIdentFeature = "identifier with prefix '__' (reserved for internal CEL mechanisms)"

// guardInternalIdents refuses an author expression that names an identifier, a
// function, a field or a message type with the `__` prefix. The prefix is reserved
// for this package's own mechanisms: the vault() macro expands to
// `__vault_read(path, __vault_resolver)`, and a direct call reaches the same Vault
// reader while skipping every check keyed on the `vault(` token — the load-time
// own-namespace scan ([ADR-0083] §7) and the Passage vault axis. The guard applies
// with or without a KVReader. It runs on the author text, before macro expansion,
// so a legal `vault('secret/x')` names nothing internal.
//
// It reads the parsed tree, not the text: where a literal, a comment or a backtick
// name ends is then the parser's answer and not a second lexer's. A `__` inside a
// literal is data (`vault('secret/db__primary/x')` passes).
//
// A `.where("<predicate>")` argument is a literal here and code once
// [Engine.rewriteHostsWhere] inlines it, so the calls in it are checked too. Not its
// bare names: the rewrite makes those fields of the host element (a host may carry a
// field `__host`, see hosts_test.go), so none of them is ever an internal name.
//
// Text this parser refuses is refused here if it contains `__` at all. The compiler
// refuses it anyway — this parser accepts everything the compile parser does, both
// being cel-go's defaults, and macros only refuse more — and the fallback keeps the
// guard closed should that ever stop being true.
func (e *Engine) guardInternalIdents(expr string) error {
	parsed, err := e.parseNoMacro(expr)
	if err != nil {
		if strings.Contains(expr, "__") {
			return &ErrUnsupported{Expr: expr, Feature: internalIdentFeature}
		}
		return nil
	}
	if e.namesInternal(parsed.Expr(), true) {
		return &ErrUnsupported{Expr: expr, Feature: internalIdentFeature}
	}
	return nil
}

// namesInternal reports whether the tree names anything internal. bareNames=false
// checks function names only, for a `.where` predicate (see [Engine.guardInternalIdents]).
func (e *Engine) namesInternal(root ast.Expr, bareNames bool) bool {
	found := false
	ast.PostOrderVisit(root, ast.NewExprVisitor(func(n ast.Expr) {
		if found {
			return
		}
		switch n.Kind() {
		case ast.IdentKind:
			found = bareNames && isInternalName(n.AsIdent())
		case ast.SelectKind:
			found = bareNames && isInternalName(n.AsSelect().FieldName())
		case ast.StructKind:
			s := n.AsStruct()
			found = bareNames && isInternalName(s.TypeName())
			for _, f := range s.Fields() {
				found = found || (bareNames && isInternalName(f.AsStructField().Name()))
			}
		case ast.CallKind:
			c := n.AsCall()
			found = isInternalName(c.FunctionName()) || e.wherePredicateNamesInternal(c)
		}
	}))
	return found
}

// wherePredicateNamesInternal checks the calls in the literal predicate of a
// `.where("<predicate>")` call; any other call answers false.
func (e *Engine) wherePredicateNamesInternal(c ast.CallExpr) bool {
	if !c.IsMemberFunction() || c.FunctionName() != "where" || len(c.Args()) != 1 ||
		c.Args()[0].Kind() != ast.LiteralKind {
		return false
	}
	pred, ok := c.Args()[0].AsLiteral().Value().(string)
	if !ok {
		return false
	}
	parsed, err := e.parseNoMacro(pred)
	if err != nil {
		return strings.Contains(pred, "__")
	}
	return e.namesInternal(parsed.Expr(), false)
}

// isInternalName reports a `__`-prefixed name in any segment: the parser keeps a
// leading dot (`.__vault_read`) and qualifies a message type (`.a.__T`).
func isInternalName(name string) bool {
	for _, seg := range strings.Split(name, ".") {
		if len(seg) > 2 && strings.HasPrefix(seg, "__") {
			return true
		}
	}
	return false
}
