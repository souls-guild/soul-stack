package cel

import (
	"strings"

	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
)

// register.hosts.<name> — the per-host register accessor (NIM-711, amendment to
// [ADR-0084]).
//
// A value that exists only per host (a probed node id, a generated port, a cluster
// member address) has no other route into `incarnation.state`: the capture that
// writes state is a keeper task, and a keeper task binds no soulprint, targets no
// host and reads only the keeper register bucket ([ADR-056] slice 2). This root
// inverts the run's register buckets by name — `register.hosts.<name>` is the map
// {SID → payload} for register `<name>` across the hosts that produced it — so ONE
// capture writes the whole per-host set in one expression.
//
// Isolation is the point of the accessor, so it is cut off at COMPILE time
// ([Vars.AllowRegisterHosts], fail-closed zero value) rather than left to resolve
// as an empty map:
//
//   - a HOST task must not see other hosts' registers through it (`register.<name>`
//     there is deliberately this host's own value — [ADR-0083] §5);
//   - the destiny pass sees no run hosts at all (same reason as soulprint.hosts,
//     [orchestration.md §4.1]);
//   - flow-control (`when:`/`changed_when:`/`until:`) is evaluated by the Soul in
//     its own sandbox, which has no cross-host data to evaluate against;
//   - migration-CEL is a pure function of the old state ([ADR-019]).
//
// Symmetric to the soulprint.hosts cut-off in [hosts.go], with one difference: that
// one keys on [Vars.AllowHosts], which is TRUE for host tasks in the scenario pass
// — exactly the context register.hosts must stay closed in. Hence a separate flag.

// usesRegisterHostsAccessor is a coarse test deciding whether an expression is worth
// an AST parse for the register.hosts gate (hot path: nearly every expression skips
// it). Both tokens must appear literally, so `register . hosts` (CEL permits
// whitespace around `.`) cannot slip past — unlike a `strings.Contains(expr,
// "register.hosts")` test. False positives (e.g. `register.x + vars.hosts`) only
// cost one parse; the AST check below decides.
func usesRegisterHostsAccessor(expr string) bool {
	return strings.Contains(expr, "register") && strings.Contains(expr, registerHostsKey)
}

// guardRegisterHosts rejects `register.hosts` outside a keeper task. reportExpr is
// the AUTHOR's text (what the error shows), scanExpr the text actually analysed —
// they differ after [Engine.rewriteHostsWhere], whose Unparse inlines a
// `.where("<predicate>")` string literal into the tree, making a register.hosts
// reference inside that predicate visible to the AST walk for the first time.
//
// A parse failure here is NOT an error: this is a gate, not a validator — env.Compile
// reports the syntax problem right after, with position information.
func (e *Engine) guardRegisterHosts(reportExpr, scanExpr string, allow bool) error {
	if allow || !usesRegisterHostsAccessor(scanExpr) {
		return nil
	}
	parsed, err := e.parseNoMacro(scanExpr)
	if err != nil {
		return nil
	}
	if !containsRegisterHostsAccessor(parsed.Expr()) {
		return nil
	}
	return &ErrUnsupported{
		Expr:    reportExpr,
		Feature: "register.hosts (keeper-side only; a host task sees only its own register)",
	}
}

// containsRegisterHostsAccessor reports whether the tree reads register.hosts
// anywhere. The presence test `has(register.hosts)` counts: it answers "does this
// accessor exist here", which outside a keeper task it does not.
func containsRegisterHostsAccessor(root ast.Expr) bool {
	found := false
	ast.PostOrderVisit(root, ast.NewExprVisitor(func(e ast.Expr) {
		if isRegisterHosts(e) {
			found = true
		}
	}))
	return found
}

// isRegisterHosts — a node reading the `hosts` field of the bare `register` ident,
// in either of the two forms CEL writes it: the select `register.hosts` and the
// index `register["hosts"]`. A field named `hosts` deeper in a payload
// (`register.probe.hosts`) is a different shape and is untouched — its operand is a
// Select, not the `register` ident.
//
// The index form is caught for the DIAGNOSTIC, not for the isolation: a context
// without the flag never has the field in its activation at all
// ([Vars.registerRoot]), so the index would resolve to no-such-key anyway. Catching
// it makes both spellings fail the same way, at compile, naming the accessor.
func isRegisterHosts(e ast.Expr) bool {
	switch e.Kind() {
	case ast.SelectKind:
		s := e.AsSelect()
		return s.FieldName() == registerHostsKey && isRegisterIdent(s.Operand())
	case ast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() || c.FunctionName() != operators.Index {
			return false
		}
		args := c.Args()
		if len(args) != 2 || !isRegisterIdent(args[0]) {
			return false
		}
		key, ok := args[1].AsLiteral().(types.String)
		return ok && string(key) == registerHostsKey
	default:
		return false
	}
}

func isRegisterIdent(e ast.Expr) bool {
	return e.Kind() == ast.IdentKind && e.AsIdent() == "register"
}
