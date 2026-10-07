package cel

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/cel-go/common/ast"
)

const directVaultRead = "__vault_read('secret/redis/admin', __vault_resolver).password"

// bypassSpellings read Vault on souls/main 1a408146 or on the first version of this
// fix: each one makes a text scan misjudge where a literal, a comment or a backtick
// name ends, and the direct call drops out of what the scan sees.
var bypassSpellings = []string{
	"true // it's\n && " + directVaultRead + " == 's3cr3t' // '",
	`'\'' + ` + directVaultRead + ` + '\'' == "'s3cr3t'"`,
	`"\"" + ` + directVaultRead + ` + "\"" == "s3cr3t"`,
	"true // '''\n && " + directVaultRead + " == 's3cr3t' // '''",
	`r"a\" + ` + directVaultRead + ` + "b" == "s3cr3t"`,
	`'''it's''' + ` + directVaultRead + ` == "s3cr3t"`,
	"{'a//b': 1}.`a//b` == 1 && " + directVaultRead + " == 's3cr3t'",
	"{'a//b': 1}.`a//b` == 1 || " + directVaultRead + " == 's3cr3t'",
}

// A direct `__vault_read` call is refused however the text around it pairs quotes,
// and Vault is never read.
func TestGuard_InternalCallNotHiddenByMisreadLiteral(t *testing.T) {
	for _, expr := range bypassSpellings {
		kv := &stubKV{secrets: map[string]map[string]any{"secret/redis/admin": {"password": "s3cr3t"}}}
		_, err := newVaultEngine(t, kv).EvalExpression(expr, Vars{})
		var ue *ErrUnsupported
		if !errors.As(err, &ue) || ue.Feature != internalIdentFeature {
			t.Errorf("%q: got %T %v, want *ErrUnsupported for the `__` prefix", expr, err, err)
		}
		if len(kv.calls) != 0 {
			t.Errorf("%q: Vault was read past the guard: %v", expr, kv.calls)
		}
	}
}

// Each kind of name is refused on its own, so dropping any one arm of the walk
// shows here rather than being masked by the other names in a full direct call.
func TestGuard_EachInternalNameKindRefused(t *testing.T) {
	for _, tc := range []struct {
		kind, expr string
		vars       Vars
	}{
		{"identifier", `__vault_resolver != null`, Vars{}},
		{"call", `__vault_read('secret/redis/admin', vars.r) == 'y'`, Vars{}},
		{"select field", `vars.__x == 1`, Vars{}},
		{"presence test", `has(vars.__x)`, Vars{}},
		{"leading-dot identifier", `.__vault_resolver != null`, Vars{}},
		{"leading-dot call", `.__vault_read('secret/redis/admin', .__vault_resolver).password == 's3cr3t'`, Vars{}},
		{"message type", `.a.__T{} == 1`, Vars{}},
		{"message field", `a.T{__f: 1} == 1`, Vars{}},
		{"call in a .where predicate", `soulprint.hosts.where("__vault_read('secret/redis/admin', role) == 'y'").size() > 0`, scenarioVars()},
	} {
		kv := &stubKV{secrets: map[string]map[string]any{"secret/redis/admin": {"password": "s3cr3t"}}}
		_, err := newVaultEngine(t, kv).EvalExpression(tc.expr, tc.vars)
		var ue *ErrUnsupported
		if !errors.As(err, &ue) || ue.Feature != internalIdentFeature {
			t.Errorf("%s %q: got %T %v, want *ErrUnsupported for the `__` prefix", tc.kind, tc.expr, err, err)
		}
		if len(kv.calls) != 0 {
			t.Errorf("%s %q: Vault was read past the guard: %v", tc.kind, tc.expr, kv.calls)
		}
	}
}

// `__` that is data is not refused: inside a literal of any form, inside a comment,
// mid-name, and as a host field inside a .where predicate.
func TestGuard_InternalNameAsDataPasses(t *testing.T) {
	e := newVaultEngine(t, &stubKV{})
	for _, expr := range []string{
		`"a\" __vault_read" == "x"`,
		`'it\'s __x' == "x"`,
		`"""a "__x" b""" == "x"`,
		`r'__x' == "x"`,
		`b"__x" == b"x"`,
		"\"x\" == \"y\" // __vault_read(\n",
		"{'a__b': 1}.`a__b` == 2",
		`{"__k": 1}["__k"] == 2`,
	} {
		out, err := e.EvalExpression(expr, Vars{})
		if err != nil {
			t.Errorf("%q: %v — the guard read data as a name", expr, err)
			continue
		}
		if out.Value() != false {
			t.Errorf("%q = %v, want false", expr, out.Value())
		}
	}
	// Evaluated against hosts that carry the field in hosts_test.go; here only the
	// guard's verdict matters.
	_, err := e.EvalExpression(`soulprint.hosts.where("__host == 'x'").size()`, scenarioVars())
	var ue *ErrUnsupported
	if errors.As(err, &ue) {
		t.Errorf(".where predicate reading a host field `__host` was refused: %v", err)
	}
}

// Text the parser refuses is refused as internal when it names `__` anywhere, and
// left to the compiler otherwise.
func TestGuard_UnparseableTextFailsClosed(t *testing.T) {
	e := newVaultEngine(t, &stubKV{})
	_, err := e.EvalExpression(directVaultRead+" ===", Vars{})
	var ue *ErrUnsupported
	if !errors.As(err, &ue) || ue.Feature != internalIdentFeature {
		t.Errorf("unparseable text naming `__`: got %T %v, want *ErrUnsupported", err, err)
	}
	_, err = e.EvalExpression(`1 ===`, Vars{})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Errorf("unparseable text without `__`: got %T %v, want *ErrCompile", err, err)
	}
}

// FuzzGuard_NoDirectVaultCallCompiles checks the guard against an oracle that does
// not share its code: the vault() macro is the only legal source of `__vault_read`,
// so in whatever compiles, the macro-expanded tree holds exactly as many
// `__vault_read` calls and `__vault_resolver` names as the author wrote vault() calls.
func FuzzGuard_NoDirectVaultCallCompiles(f *testing.F) {
	for _, s := range bypassSpellings {
		f.Add(s)
	}
	for _, s := range []string{
		`vault('secret/redis/admin').password == 's3cr3t'`,
		`vault('secret/redis/admin#password') == vault('secret/redis/admin').password`,
		"vault('secret/redis/admin').password == 's3cr3t' // __vault_read(",
		"{'a//b': 1}.`a//b` == 1",
	} {
		f.Add(s)
	}
	kv := &stubKV{secrets: map[string]map[string]any{"secret/redis/admin": {"password": "s3cr3t"}}}
	e, err := New(WithVault(kv))
	if err != nil {
		f.Fatalf("New: %v", err)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		_, err := e.EvalExpression(expr, Vars{})
		var ee *ErrEval
		if err != nil && !errors.As(err, &ee) {
			return
		}
		norm := normalize(expr)
		authored, err := e.parseNoMacro(norm)
		if err != nil {
			t.Fatalf("%q compiled but does not parse without macros: %v", expr, err)
		}
		expanded, iss := e.env.Parse(norm)
		if iss != nil && iss.Err() != nil {
			t.Fatalf("%q compiled but does not parse: %v", expr, iss.Err())
		}
		macro := countNamed(authored.Expr(), ast.CallKind, "vault")
		reads := countNamed(expanded.NativeRep().Expr(), ast.CallKind, vaultFuncName)
		resolvers := countNamed(expanded.NativeRep().Expr(), ast.IdentKind, vaultResolverVar)
		if reads != macro || resolvers != macro {
			t.Errorf("%q compiled with %d vault() calls written but %d %s calls and %d %s names after expansion",
				expr, macro, reads, vaultFuncName, resolvers, vaultResolverVar)
		}
	})
}

// countNamed counts the call or identifier nodes named name, a leading dot ignored.
func countNamed(root ast.Expr, kind ast.ExprKind, name string) int {
	n := 0
	ast.PostOrderVisit(root, ast.NewExprVisitor(func(x ast.Expr) {
		if x.Kind() != kind {
			return
		}
		var got string
		switch kind {
		case ast.CallKind:
			got = x.AsCall().FunctionName()
		case ast.IdentKind:
			got = x.AsIdent()
		}
		if strings.TrimPrefix(got, ".") == name {
			n++
		}
	}))
	return n
}
