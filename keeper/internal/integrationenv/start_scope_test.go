package integrationenv

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// SCOPE, stated rather than implied.
//
// This asserts ONE thing: in the L1 suite, no container is brought up outside
// [Start]. It says nothing about whether the retry helps, whether the suites
// pass, or whether the containers are the right ones.
//
// It is the guard NIM-569 needs because the fix is a convention, and a
// convention held by 55 call sites decays at the 56th. The 56th will not look
// wrong -- it will be a copy of a TestMain written before this existed, which is
// exactly how the L1 harness accumulated 47 hand-written copies of the same
// 90-second timeout in the first place. When it decays it decays silently: the
// new package simply goes back to failing one sweep in fifty, and that reads as
// "L1 is flaky again" rather than as a missed wrap.
//
// It is deliberately NOT tagged `integration`, so `make check` runs it in
// milliseconds without docker. A guard that only fires inside the job it is
// guarding reports the breakage at the point where it has already cost a sweep.
//
// Resolution is by IMPORT PATH, not by the `tcpostgres`/`tcvault` aliases the
// tree happens to use today. An alias is a local naming choice; keying on one
// means a suite that imports the same package as `pg` is invisible to this
// guard while being exactly what it is about. It also means a testcontainers
// module nobody has used yet -- `modules/redis`, say -- is covered on the day
// it arrives rather than on the day someone remembers to extend a list.
//
// The guard is itself tested (start_scope_selftest_test.go). That is not
// ceremony: the first version of this file resolved an unaliased import to the
// last segment of its path and therefore covered ZERO of the seven generic
// bring-ups, while looking green and reading as if it covered everything -- the
// precise failure mode it exists to prevent, one level up.
const testcontainersPkg = "github.com/testcontainers/testcontainers-go"

// bringUp names the functions that create a container. The generic entry points
// live in the root package; every `modules/<name>` wrapper exposes `Run`, plus
// the `RunContainer` spelling it was renamed from and still keeps as a
// deprecated alias (modules/vault/vault.go).
func bringUp(importPath, fn string) bool {
	if importPath == testcontainersPkg {
		return fn == "GenericContainer" || fn == "Run" || fn == "RunContainer"
	}
	return strings.HasPrefix(importPath, testcontainersPkg+"/modules/") &&
		(fn == "Run" || fn == "RunContainer")
}

// packageIdent is the identifier an UNALIASED import binds. Go binds the
// package NAME, which is not always the last segment of the path: the
// testcontainers root package lives at `.../testcontainers-go` but is named
// `testcontainers`. Deriving the identifier from the path keyed this guard on
// `testcontainers-go`, which no file writes, so every generic bring-up resolved
// to the empty import path and passed straight through -- and the module
// wrappers were covered only by the accident that this tree aliases them. The
// divergence is stated here because it is the one thing the guard cannot
// derive.
func packageIdent(path string) string {
	if path == testcontainersPkg {
		return "testcontainers"
	}
	return path[strings.LastIndex(path, "/")+1:]
}

func TestContainersAreBroughtUpThroughStart(t *testing.T) {
	root := repoRoot(t)

	scanned, violations, blind, err := scanTree(root)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// An empty scan is the NIM-238 shape: nothing checked reads exactly like
	// nothing wrong. The tag or the walk root broke, not the tree.
	if scanned == 0 {
		t.Fatalf("no integration-tagged file found under %s -- this guard checked nothing", root)
	}

	if len(blind) > 0 {
		t.Errorf("testcontainers is dot-imported in %d file(s):\n\t%s\n\n"+
			"A dot-import writes the bring-up as a bare `GenericContainer(...)`, which this\n"+
			"guard cannot attribute to a package -- so the file would be scanned and found\n"+
			"clean whatever it contains. Import it normally.",
			len(blind), strings.Join(blind, "\n\t"))
	}

	if len(violations) > 0 {
		t.Errorf("container brought up outside integrationenv.Start in %d place(s):\n\t%s\n\n"+
			"Wrap the call so a bring-up that never becomes reachable is replaced rather than\n"+
			"reported as a failing package (NIM-569):\n\n"+
			"\tctr, err := integrationenv.Start(ctx, \"postgres\", func(ctx context.Context) (*tcpostgres.PostgresContainer, error) {\n"+
			"\t\treturn tcpostgres.Run(ctx, ...)\n"+
			"\t})\n\n"+
			"A bring-up that has moved into a helper is reported at the helper, not at the\n"+
			"caller: wrap it where the container is created, so the retry replaces the\n"+
			"container rather than re-running whatever else the helper does.",
			len(violations), strings.Join(violations, "\n\t"))
	}
}

// scanTree parses every integration-tagged file under root and returns how many
// it read, the bring-ups outside [Start], and the files it cannot read honestly.
func scanTree(root string) (scanned int, violations, blind []string, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			// examples/ is its own module and is not in the L1 package set
			// (Makefile MODULES); its tagged files are L2 material run by hand.
			// tests/e2e/ is a separate module under the `e2e` tag, so the tag
			// filter below already excludes it -- this is about examples/ only.
			if path == filepath.Join(root, "examples") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !hasIntegrationTag(string(src)) {
			return nil
		}
		scanned++

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		fileViolations, fileBlind := inspect(fset, file)
		for _, line := range fileViolations {
			violations = append(violations, rel+":"+strconv.Itoa(line))
		}
		for _, line := range fileBlind {
			blind = append(blind, rel+":"+strconv.Itoa(line))
		}
		return nil
	})
	return scanned, violations, blind, err
}

// inspect returns the lines of file that bring a container up outside [Start],
// and the lines of any dot-import that would make that answer meaningless.
func inspect(fset *token.FileSet, file *ast.File) (violations, blind []int) {
	aliases := make(map[string]string, len(file.Imports))
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := packageIdent(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." && (path == testcontainersPkg || strings.HasPrefix(path, testcontainersPkg+"/")) {
			blind = append(blind, fset.Position(imp.Pos()).Line)
			continue
		}
		aliases[name] = path
	}

	wrapped := startClosures(file)
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if !bringUp(aliases[pkg.Name], sel.Sel.Name) {
			return true
		}
		for _, lit := range wrapped {
			if lit.Pos() <= call.Pos() && call.End() <= lit.End() {
				return true
			}
		}
		violations = append(violations, fset.Position(call.Pos()).Line)
		return true
	})
	return violations, blind
}

// startClosures collects the function literals passed to integrationenv.Start.
//
// The unqualified spelling is accepted only inside this package, where the call
// really is unqualified. Accepting a bare `Start(...)` everywhere would let any
// same-named function in any package launder every bring-up nested inside it.
func startClosures(file *ast.File) []*ast.FuncLit {
	own := file.Name.Name == "integrationenv"
	var lits []*ast.FuncLit
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		named := false
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			x, ok := fn.X.(*ast.Ident)
			named = ok && x.Name == "integrationenv" && fn.Sel.Name == "Start"
		case *ast.Ident:
			named = own && fn.Name == "Start"
		}
		if !named {
			return true
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.FuncLit); ok {
				lits = append(lits, lit)
			}
		}
		return true
	})
	return lits
}

// hasIntegrationTag reports whether a file builds ONLY under the tag. The
// constraint is evaluated rather than compared, so `//go:build linux &&
// integration` -- a shape this tree already uses in soul/internal/beacon -- is
// covered, while `linux || integration` is not: a file that also builds without
// the tag is not part of the L1 suite this guard describes.
func hasIntegrationTag(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//go:build ") {
			expr, err := constraint.Parse(line)
			if err != nil {
				return false
			}
			required := expr.Eval(func(string) bool { return true }) &&
				!expr.Eval(func(tag string) bool { return tag != "integration" })
			return required
		}
		if strings.HasPrefix(line, "package ") {
			return false
		}
	}
	return false
}

// repoRoot walks up from the test's directory to the go.work that marks the
// workspace root, so the guard covers every module in the L1 set rather than
// only the one it happens to live in.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.work above %s", dir)
		}
		dir = parent
	}
}
