//go:build e2e

package harness

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The declaration is what turns a red L3a from a list of identical `--- FAIL:`
// lines into a statement about which layer died, and it is also the only thing
// here capable of saying the wrong one. Both guards below are docker-free: they
// read the harness sources, so they hold even in an environment where the tier
// itself cannot run.

// TestShouldDeclareTruthTable — all eight inputs, stated rather than sampled.
//
// The rows with teeth are the two `failedBefore=true, infraUp=false` ones: the
// test was ALREADY red when NewStack was called, so its redness is not evidence
// about bring-up. t.Failed() cannot tell those apart on its own, because
// (*common).Fail propagates to the parent the moment a subtest fails.
func TestShouldDeclareTruthTable(t *testing.T) {
	cases := []struct {
		infraUp, failedBefore, failedNow bool
		want                             bool
		why                              string
	}{
		{false, false, true, true, "the region turned a clean test red — the only case that is a stand-setup failure"},
		{false, false, false, false, "nothing failed; the keeper-binary t.Skipf leaves the region early and asserts nothing"},
		{false, true, true, false, "already red on entry: declaring would stamp an infra label over a finding"},
		{false, true, false, false, "cannot un-fail; unreachable in practice, and still not ours to declare"},
		{true, false, true, false, "infrastructure was up, so whatever failed after it is the code"},
		{true, false, false, false, "the happy path"},
		{true, true, true, false, "already red, and past the infrastructure besides"},
		{true, true, false, false, "already red, and nothing failed here"},
	}
	for _, c := range cases {
		got := shouldDeclare(c.infraUp, c.failedBefore, c.failedNow)
		if got != c.want {
			t.Errorf("shouldDeclare(infraUp=%v, failedBefore=%v, failedNow=%v) = %v, want %v — %s",
				c.infraUp, c.failedBefore, c.failedNow, got, c.want, c.why)
		}
	}
}

// productEntryPoints — the calls inside the harness that run code THIS REPO
// builds, as opposed to docker, Vault or the filesystem.
//
// Each must sit outside the declared bring-up region, because a failure in one
// of them is a finding. On this tier the stakes are the whole suite rather than
// one test: every L3a test starts with NewStack, so a region that swallowed
// `keeper init` would print STAND-SETUP on all forty of them at once, and a
// suite red in nothing but STAND-SETUP reads exactly like a bad day for docker.
var productEntryPoints = map[string]string{
	"runKeeperInit":       "`keeper init` — ADR-013 bootstrap, schema migrations, the JWT signing key",
	"startKeeperRun":      "`keeper run` and the /readyz wait",
	"spawnKeeperProc":     "`keeper run` for one KID of the multi-keeper cluster",
	"assertOwnKeeper":     "an authenticated call proving the process on our port is ours (NIM-469)",
	"RegisterSoulPreAuth": "raw INSERTs into souls/soul_seeds — where a dropped column dies",
	"RegisterService":     "POST /v1/services over examples/ (NIM-211: examples are the subject, not scenery)",
}

// TestDeclaredRegionsEndBeforeTheProductRuns — the bring-up declaration covers
// infrastructure only.
//
// This is not a hypothetical risk being pre-empted: it is the defect an
// independent validator found in NIM-406's first cut, where `infraUp` was set on
// the last line of the entry point and the declared region therefore swallowed
// every product call above it. The prose said "everything from here up is
// bring-up". Prose not holding is the thing this ticket is about.
func TestDeclaredRegionsEndBeforeTheProductRuns(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse harness sources: %v", err)
	}

	declaring := 0
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			base := filepath.Base(path)
			if base == "setupdecl.go" {
				continue // its own declaration is the definition, not a use
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				start := deferPos(fn)
				if start == token.NoPos {
					continue
				}
				declaring++
				checkRegion(t, fset, base, fn, start)
			}
		}
	}

	// Closed in the other direction: if the entry points stop deferring it at
	// all, everything above passes by finding nothing to check.
	if declaring < 2 {
		t.Fatalf("only %d function(s) defer declareStandSetupFailure. NewStack and "+
			"NewMultiKeeperStack both must, or their bring-up failures read as assertions "+
			"and a red suite is back to being forty indistinguishable FAIL lines.", declaring)
	}
}

// deferPos — where the region OPENS, or NoPos if this function does not declare
// one.
func deferPos(fn *ast.FuncDecl) token.Pos {
	pos := token.NoPos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		if id, ok := d.Call.Fun.(*ast.Ident); ok && id.Name == "declareStandSetupFailure" {
			if pos == token.NoPos || d.Pos() < pos {
				pos = d.Pos()
			}
		}
		return true
	})
	return pos
}

// checkRegion: find where the region closes, then insist no product call falls
// between the defer and that point.
func checkRegion(t *testing.T, fset *token.FileSet, file string, fn *ast.FuncDecl, start token.Pos) {
	t.Helper()

	end := token.NoPos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != "infraUp" {
			return true
		}
		if rhs, ok := as.Rhs[0].(*ast.Ident); ok && rhs.Name == "true" {
			if end == token.NoPos || as.Pos() < end {
				end = as.Pos()
			}
		}
		return true
	})
	if end == token.NoPos {
		t.Errorf("%s: %s defers declareStandSetupFailure but never sets `infraUp = true`, so every "+
			"exit from it is declared a stand-setup failure — including a successful one in a test "+
			"that is red for its own reasons.", file, fn.Name.Name)
		return
	}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch f := call.Fun.(type) {
		case *ast.Ident:
			name = f.Name
		case *ast.SelectorExpr:
			name = f.Sel.Name
		}
		what, isProduct := productEntryPoints[name]
		if !isProduct || call.Pos() < start || call.Pos() > end {
			return true
		}
		t.Errorf("%s:%d: %s calls %s (%s) INSIDE the declared bring-up region — the region runs from "+
			"line %d to line %d. A failure there would print STAND-SETUP under the words \"nothing "+
			"above is a finding about the code\", so a real regression would read as a bad day for "+
			"docker. Move `infraUp = true` above this call, or the call above the defer.",
			file, fset.Position(call.Pos()).Line, fn.Name.Name, name, what,
			fset.Position(start).Line, fset.Position(end).Line)
		return true
	})
}
