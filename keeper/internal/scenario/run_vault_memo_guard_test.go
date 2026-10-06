package scenario

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// ★ TestRun_BindsTheRunVaultMemoBeforeTheFirstRender is the unit-lane tripwire for
// NIM-934. The run-wide vault() memo exists only because Runner.run binds it on the
// run's context before anything reads Vault: Pipeline.Render binds a memo of its own
// whenever the context carries none, so deleting the run's binding — or moving it
// below the first Render — compiles, passes every render-tier test, and quietly
// returns the run to one memo per pass, where a secret gone mid-run fails the run.
// The behavioural proof needs Postgres and lives in the integration lane
// (TestIntegration_RunVaultMemo_*); this keeps `make check` honest without it.
//
// What it checks: run() binds `ctx = render.WithVaultFence(ctx, …)` as a statement of
// its own body — not inside a branch that may not execute — before its first
// `r.deps.Render.Render` and before keeper-side dispatch; and nothing in run() after
// it re-roots `ctx` on context.Background/TODO, which would drop the memo for
// everything that follows. It does NOT see a binding made in a helper run() calls
// (move it there and this test asks you to update it), a re-root spelled `var ctx =`
// or written in a closure declared before the binding, nor a module context built
// from a context not derived from the run's in keeper_dispatch.go — the integration
// test TestIntegration_RunVaultMemo_KeeperStepWriteIsSeenByTheNextPass catches that.
func TestRun_BindsTheRunVaultMemoBeforeTheFirstRender(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatalf("parse run.go: %v", err)
	}
	var run *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "run" && fn.Recv != nil {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("run.go declares no Runner.run — the walk is broken, and a broken walk gates nothing")
	}

	var bind, firstRender, firstKeeperDispatch token.Pos
	for _, stmt := range run.Body.List {
		s, ok := stmt.(*ast.AssignStmt)
		if !ok || len(s.Lhs) != 1 || len(s.Rhs) != 1 || !isIdent(s.Lhs[0], "ctx") || !isPkgCall(s.Rhs[0], "render", "WithVaultFence") {
			continue
		}
		if call := s.Rhs[0].(*ast.CallExpr); len(call.Args) > 0 && isIdent(call.Args[0], "ctx") {
			bind = s.Pos()
			break
		}
	}
	var reroots []token.Pos
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				if isIdent(lhs, "ctx") && i < len(s.Rhs) && rootsOnFreshContext(s.Rhs[i]) {
					reroots = append(reroots, s.Pos())
				}
			}
		case *ast.CallExpr:
			sel, ok := s.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case sel.Sel.Name == "Render" && isSelector(sel.X, "deps", "Render") && !firstRender.IsValid():
				firstRender = s.Pos()
			case sel.Sel.Name == "dispatchKeeperTasks" && !firstKeeperDispatch.IsValid():
				firstKeeperDispatch = s.Pos()
			}
		}
		return true
	})

	if !firstRender.IsValid() {
		t.Fatal("run() calls no r.deps.Render.Render — the walk is broken")
	}
	if !bind.IsValid() {
		t.Fatalf("run() no longer binds `ctx = render.WithVaultFence(ctx, …)` in its own body: every Render of the run "+
			"now takes a memo of its own, and a secret that disappears mid-run fails the run over a task "+
			"that already finished (NIM-934). First Render at %s", fset.Position(firstRender))
	}
	for _, pos := range reroots {
		if pos > bind {
			t.Errorf("%s: run() re-roots ctx on a fresh context after binding the run vault memo at %s: "+
				"every Render and keeper-side step after it loses the memo", fset.Position(pos), fset.Position(bind))
		}
	}
	if bind > firstRender {
		t.Errorf("run() binds the run vault memo at %s, AFTER its first Render at %s: that pass reads "+
			"with a memo the later passes never see", fset.Position(bind), fset.Position(firstRender))
	}
	if firstKeeperDispatch.IsValid() && bind > firstKeeperDispatch {
		t.Errorf("run() binds the run vault memo at %s, after keeper-side dispatch at %s: a step's "+
			"Vault write would not reach the memo the later passes read", fset.Position(bind), fset.Position(firstKeeperDispatch))
	}
}

// rootsOnFreshContext matches context.Background()/TODO() and any call whose first
// argument is one — context.WithTimeout(context.Background(), …) drops values too.
func rootsOnFreshContext(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	if isPkgCall(call, "context", "Background") || isPkgCall(call, "context", "TODO") {
		return true
	}
	return len(call.Args) > 0 && rootsOnFreshContext(call.Args[0])
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func isPkgCall(e ast.Expr, pkg, fn string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == fn && isIdent(sel.X, pkg)
}

// isSelector matches `<anything>.<x>.<y>` ending in x.y — r.deps.Render here.
func isSelector(e ast.Expr, x, y string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != y {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == x
}
