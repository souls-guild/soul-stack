package jwt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// keeperModuleRoot is the keeper module, seen from this package's directory.
const keeperModuleRoot = "../.."

// TestNoRevocation_NeverInProductionCode — [NoRevocation] turns per-token
// revocation off for the verifier it is given (NIM-1045). It exists for tests and
// offline tools; a production caller passing it would let a revoked token
// through the MCP doors and the cookie exchange, which have no other check. So no
// non-test file in the keeper module may name it, except its declaration.
func TestNoRevocation_NeverInProductionCode(t *testing.T) {
	var offenders []string
	walkProductionGo(t, func(path string, f *ast.File) {
		if filepath.ToSlash(path) == filepath.ToSlash(filepath.Join(keeperModuleRoot, "internal/jwt/verifier.go")) {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "NoRevocation" {
				offenders = append(offenders, path)
			}
			return true
		})
	})
	if len(offenders) > 0 {
		t.Errorf("jwt.NoRevocation used in production code: %v — a verifier built with it does not refuse revoked tokens", offenders)
	}
}

// TestDaemonVerifier_TakesTheRBACHolder — the one production verifier gets the
// RBAC holder as its revocation source. Read off the call, because the daemon
// cannot be started in a unit test and a verifier that quietly lost its source
// fails no request until a token is revoked.
func TestDaemonVerifier_TakesTheRBACHolder(t *testing.T) {
	var calls []string
	walkProductionGo(t, func(path string, f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewVerifier" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "keeperjwt" {
				return true
			}
			src := "<missing>"
			if len(call.Args) == 3 {
				if arg, ok := call.Args[2].(*ast.SelectorExpr); ok {
					if recv, ok := arg.X.(*ast.Ident); ok {
						src = recv.Name + "." + arg.Sel.Name
					}
				}
			}
			calls = append(calls, path+": "+src)
			return true
		})
	})
	if len(calls) == 0 {
		t.Fatal("found no keeperjwt.NewVerifier call in the keeper module — the daemon's wiring moved and this guard reads nothing; point it at the new call")
	}
	for _, c := range calls {
		if !strings.HasSuffix(c, ": d.rbacHolder") {
			t.Errorf("%s — the production verifier must take the RBAC holder as its revocation source", c)
		}
	}
}

func walkProductionGo(t *testing.T, visit func(path string, f *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(keeperModuleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == keeperModuleRoot {
				return nil
			}
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		visit(path, f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", keeperModuleRoot, err)
	}
}

// TestIssue_OnlyThroughTheAccountedPaths — a token is minted in exactly two
// places: operator.MintJWT, which writes its operator_jwts row first, and the
// cookie exchange, which re-signs under the session's own jti. A third
// production call would compile, pass every other test and mint a token no row
// can revoke. Issue is the only method of that name in the keeper module, so a
// five-argument `.Issue(` call is a call to it.
func TestIssue_OnlyThroughTheAccountedPaths(t *testing.T) {
	allowed := map[string]bool{
		filepath.Join(keeperModuleRoot, "internal/operator/jwts.go"):       true,
		filepath.Join(keeperModuleRoot, "internal/api/huma_auth_token.go"): true,
	}
	// One call in each: a second mint added to either file would pass a per-file
	// allowlist and still have no row.
	calls := map[string]int{}
	walkProductionGo(t, func(path string, f *ast.File) {
		imported := importedNames(f)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 5 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Issue" {
				return true
			}
			// `pkg.Issue(...)` is a package function (certissue.Issue), not the method.
			if id, ok := sel.X.(*ast.Ident); ok && imported[id.Name] {
				return true
			}
			if allowed[filepath.Clean(path)] {
				calls[filepath.Clean(path)]++
				return true
			}
			t.Errorf("%s: a JWT minted outside operator.MintJWT and the cookie exchange — it has no operator_jwts row and cannot be revoked on its own", path)
			return true
		})
	})
	for path := range allowed {
		if calls[path] != 1 {
			t.Errorf("%s: %d calls to Issue, want exactly 1 — a second mint here is a token with no row, or the accounted path moved", path, calls[path])
		}
	}
}

// importedNames is the set of names a file refers to its imports by.
func importedNames(f *ast.File) map[string]bool {
	out := make(map[string]bool, len(f.Imports))
	for _, imp := range f.Imports {
		if imp.Name != nil {
			out[imp.Name.Name] = true
			continue
		}
		p := strings.Trim(imp.Path.Value, `"`)
		out[p[strings.LastIndex(p, "/")+1:]] = true
	}
	return out
}
