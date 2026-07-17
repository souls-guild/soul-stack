package cel

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/parser"
)

// CEL function default(x, y) ([templating.md §2.3], [ADR-010 Amendment
// 2026-06-23]). Value-or-default, parity with Ansible `| default()`: return x if
// present/available, else y. Shortens the canonical has()-guard ([docs/input.md]):
//
//	default(essence.tls_enable, false)   ≡  has(essence.tls_enable) ? essence.tls_enable : false
//	default(input.redis_settings, {})    ≡  has(input.redis_settings) ? input.redis_settings : {}
//	int(default(essence.tls_port, 7379)) ≡  int(has(essence.tls_port) ? essence.tls_port : 7379)
//
// Implemented as a custom macro (compile-time AST rewrite), like vault()
// ([vault.go]). CEL evaluates arguments EAGERLY: as a plain function,
// default(essence.tls_enable, false) would fail "no such key" on a missing key
// BEFORE the call. The macro sees the first argument's AST BEFORE eval and rewrites
// it to the guarded has(x) ? x : y — the same eagerness-bypass technique as vault().
// Compile-time rewrite, not runtime execution of a string.
//
// Constraint (Ansible-default semantics): x must be a select-chain or an
// identifier. has() in CEL applies ONLY to field access (select), so:
//   - Select x (essence.tls_enable, a.b.c) → has(x) ? x : y;
//   - a bare root identifier (input/essence/…) is always present in the
//     activation ([Vars.activation] binds roots as an empty map, not "absent")
//     → expands into x itself (fallback unreachable — correct degenerate
//     semantics; has(ident) does not compile in CEL at all);
//   - an expression argument (default(size(x), 0), default(a+b, 0), index access
//     default(a['k'], 0)) → a clear compile error (*common.Error). Correct
//     semantics: default over a variable/field, not over a computation (for the
//     latter the author has a ternary).
//
// Pure: no I/O/secrets/crypto/eval-time state (syntactic sugar over has()?:).
// Registered in the Keeper-full env ([New]) and the Soul flow-control env
// ([NewFlowControl]); NOT registered in migration-CEL ([NewMigration], [ADR-019])
// — hermetic sandbox with minimal surface area (symmetric with merge()/glob()).
//
// Masking invariant: default(x, y) does NOT rename the destination key, so a secret
// substituted via default(essence.x, vault('…#password')) or assigned to a
// sensitive-named key (password/secret/token/tls_key/…) is masked by the output
// layer (shared/audit.MaskSecrets) IDENTICALLY to a direct substitution; it neither
// widens nor narrows the masking boundary (symmetric with merge()).

// defaultMacroName is the function name in the CEL env; users write default(x, y).
const defaultMacroName = "default"

// defaultEnvOptions returns the EnvOptions registering default(): a global 2-arg
// macro. There is NO real cel.Function — the macro fully expands into a has()?:
// ternary at parse time, no binding needed. Symmetric with vault()'s macro
// mechanism but without a companion function (vault() leaves a __vault_read
// binding; default() is a pure rewrite).
func defaultEnvOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Macros(parser.NewGlobalMacro(defaultMacroName, 2, expandDefaultMacro)),
	}
}

// expandDefaultMacro expands default(x, y) into has(x) ? x : y (for a select) or
// into x itself (for a bare root identifier — always present in the activation,
// has(ident) does not compile in CEL). x must be a Select or Ident; otherwise a
// clear compile error (default over an expression has no value-or-default meaning:
// for computations the author writes an explicit ternary).
func expandDefaultMacro(mef parser.ExprHelper, _ ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
	x, fallback := args[0], args[1]
	switch x.Kind() {
	case ast.SelectKind:
		s := x.AsSelect()
		// Index access (a['k']) is not a Select but a CallKind (_[_]) — it does
		// not reach here. A TestOnly select (has(...) itself) also cannot reach
		// here from a user in argument position. Build has(operand.field): a
		// presence test over the same operand/field as the original select.
		presence := mef.NewPresenceTest(mef.Copy(s.Operand()), s.FieldName())
		return mef.NewCall(operators.Conditional, presence, mef.Copy(x), fallback), nil
	case ast.IdentKind:
		// A root identifier is always present in the activation ([Vars.activation]);
		// has(ident) does not compile in CEL. Fallback unreachable — expand into x.
		return mef.Copy(x), nil
	default:
		return nil, mef.NewError(x.ID(),
			"default(x, y): first argument must be a field (essence.tls_enable, a.b.c) "+
				"or an identifier, not an expression - for computations use the ternary has(...)?...:...")
	}
}
