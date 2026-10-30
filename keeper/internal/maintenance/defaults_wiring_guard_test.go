package maintenance_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// ★ Source-derived guard (NIM-1138): every reader of maintenance windows is
// wired with the cluster default, so none of them answers from the incarnation's
// own windows alone. The behaviour of each reader is pinned by its package's
// table (…DefaultWindowsHoldAsOwnWindows); this pins the wiring those tables
// cannot reach — the daemon's and the servers' constructors, and the call sites
// that pass the source on.
//
//   - nothing outside this package compiles a calculator itself
//     (maintenance.NewCalculator) or builds a Rules by hand: a calculator comes
//     from Rules a load produced, whose Effective is the one place the default
//     is applied;
//   - no load of the rules, no PlanRunStart and no maintenanceAt passes a nil
//     default, except CheckFreezeConsent, which reads freezes alone;
//   - every MaintenanceGate literal names its Defaults;
//   - every scenario.Deps / api.Deps / mcp.HandlerDeps literal of the daemon
//     names its MaintenanceDefaults;
//   - every handler or spawner built outside a test gets SetMaintenanceDefaults
//     in the function that builds it.
func TestDefaultWindows_EveryReaderIsWiredWithTheDefault(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	counts := map[string]int{}
	var violations []string
	report := func(pos token.Pos, format string) {
		violations = append(violations, fset.Position(pos).String()+": "+format)
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		inMaintenance := file.Name.Name == "maintenance"
		inDaemon := filepath.ToSlash(filepath.Dir(path)) == filepath.ToSlash(filepath.Join(root, "cmd", "keeper"))

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			built := map[string]token.Pos{}
			wired := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					name, recv := callee(n)
					switch {
					case recv == "maintenance" && name == "NewCalculator" && !inMaintenance:
						report(n.Pos(), "maintenance.NewCalculator outside the maintenance package skips Rules.Effective")
					case recv == "maintenance" && (name == "LoadRules" || name == "LoadRulesOf" || name == "Load"):
						counts["load"]++
						if len(n.Args) == 4 && isNil(n.Args[3]) && fn.Name.Name != "CheckFreezeConsent" {
							report(n.Pos(), "maintenance."+name+" with a nil default")
						}
					case name == "PlanRunStart":
						counts["plan"]++
						if len(n.Args) == 6 && isNil(n.Args[2]) {
							report(n.Pos(), "PlanRunStart with a nil default")
						}
					case name == "maintenanceAt":
						counts["render"]++
						if len(n.Args) == 5 && isNil(n.Args[2]) {
							report(n.Pos(), "maintenanceAt with a nil default")
						}
					case name == "SetMaintenanceDefaults" && recv != "":
						wired[recv] = true
					}
				case *ast.AssignStmt:
					for i, rhs := range n.Rhs {
						call, ok := rhs.(*ast.CallExpr)
						if !ok || i >= len(n.Lhs) {
							continue
						}
						switch name, _ := callee(call); name {
						case "NewIncarnationHandler", "NewVoyageHandler", "NewCadenceSpawner":
							if id, ok := n.Lhs[i].(*ast.Ident); ok {
								built[id.Name] = call.Pos()
							}
						}
					}
				case *ast.ReturnStmt:
					for _, res := range n.Results {
						if call, ok := res.(*ast.CallExpr); ok {
							switch name, _ := callee(call); name {
							case "NewIncarnationHandler", "NewVoyageHandler", "NewCadenceSpawner":
								report(call.Pos(), name+" returned unwired: it never gets SetMaintenanceDefaults")
							}
						}
					}
				case *ast.CompositeLit:
					typ := typeName(n.Type)
					switch {
					case typ == "maintenance.Rules":
						report(n.Pos(), "maintenance.Rules built by hand: rules come from LoadRules, LoadRulesOf or DefaultsSource.Unrecorded, which carry the default")
					case typ == "MaintenanceGate" || typ == "voyage.MaintenanceGate":
						counts["gate"]++
						if !hasKey(n, "Defaults") {
							report(n.Pos(), typ+" literal without Defaults: the gate holds no default")
						}
					case inDaemon && (typ == "scenario.Deps" || typ == "api.Deps" || typ == "mcp.HandlerDeps"):
						counts["deps"]++
						if !hasKey(n, "MaintenanceDefaults") {
							report(n.Pos(), typ+" literal without MaintenanceDefaults")
						}
					}
				}
				return true
			})
			for v, pos := range built {
				counts["built"]++
				if !wired[v] {
					report(pos, v+" is built here and never gets SetMaintenanceDefaults")
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Error(v)
	}

	// The guard sees what it guards: a renamed function or a moved constructor
	// would otherwise turn it into a pass over nothing.
	for what, least := range map[string]int{"load": 5, "plan": 3, "render": 3, "gate": 2, "deps": 4, "built": 4} {
		if counts[what] < least {
			t.Errorf("found %d %s sites, want at least %d: the guard no longer sees the readers", counts[what], what, least)
		}
	}
}

// callee names a call's function and, for pkg.F or x.M, the qualifier.
func callee(c *ast.CallExpr) (name, recv string) {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name, ""
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			return f.Sel.Name, id.Name
		}
		return f.Sel.Name, ""
	}
	return "", ""
}

func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name + "." + t.Sel.Name
		}
	}
	return ""
}

func hasKey(lit *ast.CompositeLit, key string) bool {
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				return true
			}
		}
	}
	return false
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}
