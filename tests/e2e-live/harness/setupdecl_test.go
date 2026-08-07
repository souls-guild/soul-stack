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

// The declaration is the hinge of NIM-406: it is the only thing that tells a
// reader whether a red gate is about the machine or about the code, and it is
// the only thing that can tell them the wrong one. setupdecl.go is deliberately
// outside the `e2e_live` tag so it can be tested without docker — and until this
// file existed, nothing took that offer up.

// TestShouldDeclareTruthTable — all eight inputs, stated rather than sampled.
//
// The rows with teeth are the two `failedBefore=true, infraUp=false` ones: the
// test was ALREADY red when the entry point was called, so its redness is not
// evidence about this region. t.Failed() cannot tell those apart on its own,
// because (*common).Fail propagates to the parent the moment a subtest fails.
func TestShouldDeclareTruthTable(t *testing.T) {
	cases := []struct {
		infraUp, failedBefore, failedNow bool
		want                             bool
		why                              string
	}{
		{false, false, true, true, "the region turned a clean test red — the only case that is a stand-setup failure"},
		{false, false, false, false, "nothing failed; a t.Skipf leaves the region early and asserts nothing"},
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
// of them is a finding. The Soul onboarding pair is the sharpest: tests/e2e has
// no real soul binary anywhere, so `soul init` (CSR Bootstrap) and `soul run`
// are exercised only here — a regression in them, labelled STAND-SETUP on all
// nine gate tests, would read as nothing but a bad day for docker.
//
// This is a list, and a new product call added inside a region is the gap it
// leaves. NIM-515 walked straight through it: a bare `os.ReadFile` of a path
// built from repoRoot, naming no entry point at all, put a finding about a
// DELETED FILE IN THIS REPOSITORY under the STAND-SETUP label on all nine gate
// tests. repoReadingFuncs below closes that half by a property rather than a
// name; the rule both encode is stated in setupdecl.go.
var productEntryPoints = map[string]string{
	"runKeeperInit":             "`keeper init` — ADR-013 bootstrap, migrations, the JWT signing key",
	"startKeeperRun":            "`keeper run`",
	"registerExampleService":    "POST /v1/services over examples/ (NIM-211: examples are the subject, not scenery)",
	"IssueBootstrapToken":       "raw INSERTs into souls/bootstrap_tokens — where a dropped column dies",
	"SpawnSoulContainer":        "`soul init` (CSR Bootstrap RPC), `soul run`, waiting for souls.status='connected'",
	"buildCommunityRedisBinary": "`go build` of this repo's community-redis plugin",
}

// TestDeclaredRegionsEndBeforeTheProductRuns — the bring-up declaration covers
// infrastructure only.
//
// This is the guard for the defect an independent validator found in NIM-406's
// first cut, and it is NIM-406's own shape one level up: the first cut set the
// flag on the last line of NewStack, so the declared region swallowed every
// product call above. The prose said "everything from here to `return s` is
// bring-up"; prose is what this ticket is about not holding.
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
		readsRepo := repoReadingFuncs(pkg)
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
				checkRegion(t, fset, base, fn, start, readsRepo)
			}
		}
	}

	// Closed in the other direction: if the entry points stop deferring it at
	// all, everything above passes by finding nothing to check.
	if declaring < 2 {
		t.Fatalf("only %d function(s) defer declareStandSetupFailure. NewStack and "+
			"BuildCommunityRedisPlugin both must, or their bring-up failures read as "+
			"assertions and the gate is back to being illegible.", declaring)
	}
}

// deferPos — where the region OPENS, or NoPos if this function does not declare
// one. The region has two ends, and only tracking both keeps the guard from
// punishing the fix: BuildCommunityRedisPlugin calls the plugin build first and
// declares afterwards, precisely so the build stays out.
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

// repoReadingFuncs — the package's functions that reach [repoRoot], directly or
// through each other.
//
// Reading this repository's own tree is the property, and it is derived rather
// than listed on purpose. A list can only name what someone thought to add: the
// call that shipped NIM-515 was `os.ReadFile` on a path under repoRoot, which no
// plausible entry-point list contains, and it labelled a deleted file in this
// repo as a fact about the machine on every gate test.
//
// The closure is one the fix itself needs: BuildCommunityRedisPlugin no longer
// reads the document inline, it calls readCommunityRedisDocument, and moving the
// read one frame down must not move it out of sight.
func repoReadingFuncs(pkg *ast.Package) map[string]bool {
	bodies := map[string]*ast.FuncDecl{}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			// Methods share the map with plain functions under their bare name.
			// A collision would only ever widen the set, and this package has no
			// method that reads the repo.
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				bodies[fn.Name.Name] = fn
			}
		}
	}

	reads := map[string]bool{"repoRoot": true}
	for grew := true; grew; {
		grew = false
		for name, fn := range bodies {
			if reads[name] {
				continue
			}
			if callsAny(fn.Body, reads) {
				reads[name], grew = true, true
			}
		}
	}
	delete(reads, "repoRoot") // the definition is not a use of itself
	return reads
}

// callsAny — does this body CALL one of these names? Calls, not identifiers:
// locateKeeperBinary holds a local `repoRoot` string of its own, and matching
// bare identifiers made the guard accuse it of reading the tree. A local never
// appears in call position, and this is the difference between a property and a
// coincidence of spelling.
func callsAny(body *ast.BlockStmt, names map[string]bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok && names[calleeName(call)] {
			found = true
			return false
		}
		return true
	})
	return found
}

// calleeName — the bare name being called, or "" for anything else (a call
// through a value, a conversion, an index expression).
func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// checkRegion: find where the region closes, then insist no product call and no
// read of this repository falls between the defer and that point.
func checkRegion(t *testing.T, fset *token.FileSet, file string, fn *ast.FuncDecl, start token.Pos, readsRepo map[string]bool) {
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
			"%d to %d. A failure there would print STAND-SETUP under the words \"nothing above is a "+
			"finding about the code\", so a real regression would read as a bad day for docker. Move "+
			"`infraUp = true` above this call, or the call above the defer.",
			file, fset.Position(call.Pos()).Line, fn.Name.Name, name, what,
			fset.Position(start).Line, fset.Position(end).Line)
		return true
	})

	// The same rule by property: a region that reads this repository's tree is
	// making a claim about the repository, and the label says "machine".
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || call.Pos() < start || call.Pos() > end {
			return true
		}
		name := calleeName(call)
		via := ""
		switch {
		case name == "repoRoot":
			via = "reads the repo tree directly"
		case readsRepo[name] && name != fn.Name.Name:
			via = "reaches repoRoot"
		default:
			return true
		}
		t.Errorf("%s:%d: %s calls %s (%s) INSIDE the declared bring-up region — the region runs from "+
			"%d to %d. Whatever it reads is a fact about THIS REPOSITORY: a file that moved or went "+
			"away is a finding, and here it would print STAND-SETUP. That is how NIM-377's deleted "+
			"manifest.yaml read as \"the stand didn't come up\" on all nine gate tests (NIM-515). "+
			"Move the read above the defer, beside `go build`.",
			file, fset.Position(call.Pos()).Line, fn.Name.Name, name, via,
			fset.Position(start).Line, fset.Position(end).Line)
		return true
	})
}
