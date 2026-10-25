package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Each login method's session lives its own session_ttl, not
// auth.jwt.ttl_default (ADR-058 amendment NIM-1050). Building the three
// login deps needs LDAP, an IdP and a pool, so the wiring is read out of the
// source: the deps literal's TTL must be the value resolved from that
// method's own key, bounded by ttl_max.
func TestLoginDeps_TakeTheirMethodsSessionTTL(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "daemon.go", nil, 0)
	if err != nil {
		t.Fatalf("parse daemon.go: %v", err)
	}
	for _, c := range []struct {
		method, deps, key, block string
	}{
		{"setupLocalAuth", "LocalAuthDeps", "auth.local.session_ttl", "Local"},
		{"setupLDAPAuth", "LDAPAuthDeps", "auth.ldap.session_ttl", "LDAP"},
		{"setupOIDCAuth", "OIDCAuthDeps", "auth.oidc.session_ttl", "OIDC"},
	} {
		fn := findDaemonMethod(file, c.method)
		if fn == nil {
			t.Fatalf("method (*daemon).%s not found in daemon.go — this guard now guards nothing", c.method)
		}
		var ttlValue ast.Expr
		resolved := map[string]bool{}
		ast.Inspect(fn, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if sel, ok := n.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == c.deps {
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "TTL" {
								ttlValue = kv.Value
							}
						}
					}
				}
			case *ast.AssignStmt:
				if len(n.Rhs) != 1 {
					return true
				}
				call, ok := n.Rhs[0].(*ast.CallExpr)
				if !ok || exprString(call.Fun) != "resolveSessionTTL" || len(call.Args) != 3 {
					return true
				}
				if lit, ok := call.Args[0].(*ast.BasicLit); !ok || lit.Value != `"`+c.key+`"` {
					return true
				}
				if exprString(call.Args[1]) != "d.cfg.Auth."+c.block+".ResolvedSessionTTL" || exprString(call.Args[2]) != "d.ttlMax" {
					return true
				}
				if id, ok := n.Lhs[0].(*ast.Ident); ok {
					resolved[id.Name] = true
				}
			}
			return true
		})
		if ttlValue == nil {
			t.Errorf("%s: no TTL in the api.%s literal — the session would last zero", c.method, c.deps)
			continue
		}
		id, ok := ttlValue.(*ast.Ident)
		if !ok || !resolved[id.Name] {
			t.Errorf("%s: api.%s.TTL is %s at %s, not the value of resolveSessionTTL(%q, d.cfg.Auth.%s.ResolvedSessionTTL, d.ttlMax)",
				c.method, c.deps, exprString(ttlValue), fset.Position(ttlValue.Pos()), c.key, c.block)
		}
	}
}

// Both HTTP listeners resolve the client behind the same trusted proxies:
// a listener built without them would record the ingress as every client.
func TestHTTPListeners_GetTheTrustedProxies(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "daemon.go", nil, 0)
	if err != nil {
		t.Fatalf("parse daemon.go: %v", err)
	}
	for _, c := range []struct{ method, deps string }{
		{"setupAPIServer", "Deps"},
		{"setupMCPServer", "ServerDeps"},
	} {
		fn := findDaemonMethod(file, c.method)
		if fn == nil {
			t.Fatalf("method (*daemon).%s not found in daemon.go — this guard now guards nothing", c.method)
		}
		var value string
		fromConfig := map[string]bool{}
		ast.Inspect(fn, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if sel, ok := n.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == c.deps {
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "TrustedProxies" {
								value = exprString(kv.Value)
							}
						}
					}
				}
			case *ast.AssignStmt:
				if call, ok := n.Rhs[0].(*ast.CallExpr); ok && strings.HasSuffix(exprString(call.Fun), "Listen.ResolvedTrustedProxies") {
					if id, ok := n.Lhs[0].(*ast.Ident); ok {
						fromConfig[id.Name] = true
					}
				}
			}
			return true
		})
		if !fromConfig[value] {
			t.Errorf("%s: %s.TrustedProxies = %q, not the resolved listen.trusted_proxies", c.method, c.deps, value)
		}
	}
}

func exprString(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	default:
		return ""
	}
}

func TestResolveSessionTTL(t *testing.T) {
	ok := func(d time.Duration) func(time.Duration) (time.Duration, error) {
		return func(time.Duration) (time.Duration, error) { return d, nil }
	}
	if got, err := resolveSessionTTL("auth.ldap.session_ttl", ok(7*24*time.Hour), 90*24*time.Hour); err != nil || got != 7*24*time.Hour {
		t.Fatalf("7d under 90d: %s, %v", got, err)
	}
	if _, err := resolveSessionTTL("auth.ldap.session_ttl", ok(91*24*time.Hour), 90*24*time.Hour); err == nil || !strings.Contains(err.Error(), "auth.ldap.session_ttl") {
		t.Errorf("91d over 90d: want a refusal naming the key, got %v", err)
	}
}

// The resolver is handed the ceiling the daemon runs on: an omitted key is cut
// to it, so a cluster whose ttl_max is below a method's default keeps starting.
func TestResolveSessionTTL_OmittedKeyFollowsTheCeiling(t *testing.T) {
	for _, ttlMax := range []time.Duration{48 * time.Hour, 30 * time.Second} {
		got, err := resolveSessionTTL("auth.local.session_ttl", (&config.KeeperAuthLocal{}).ResolvedSessionTTL, ttlMax)
		if err != nil || got != ttlMax {
			t.Errorf("ttl_max %s: %s, %v; want the ceiling", ttlMax, got, err)
		}
	}
}
