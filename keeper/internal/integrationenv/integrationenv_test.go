package integrationenv

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// NO build tag on this file, deliberately. These guards protect the rule that
// decides whether an integration suite may skip itself, so they must run in the
// docker-less gate (`make check`) — a guard that only runs under
// `-tags=integration` would be protected by the very thing it protects.

// TestRequireDocker_DefaultsToRequiring pins the inversion NIM-238 is about: an
// absent variable means REQUIRE, not skip. The old helper returned true only
// when SOUL_STACK_INTEGRATION_REQUIRE_DOCKER was set, so forgetting it printed a
// green result for a suite that ran nothing.
func TestRequireDocker_DefaultsToRequiring(t *testing.T) {
	t.Setenv(SkipEnv, "")
	t.Setenv(RequireEnv, "")
	if !RequireDocker() {
		t.Fatal("RequireDocker() = false with no variables set — an absent variable must never mean 'skip quietly'")
	}
}

// TestRequireDocker_SkipIsExplicit — the escape hatch works, and it is the ONLY
// one. A skip is a claim that nothing was verified, so it has to be said out
// loud; any non-empty value counts, because the failure mode we care about is
// silence, not a mistyped "false".
func TestRequireDocker_SkipIsExplicit(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "please"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(SkipEnv, v)
			if RequireDocker() {
				t.Errorf("RequireDocker() = true with %s=%q — the opt-out must work", SkipEnv, v)
			}
		})
	}
}

// TestRequireDocker_LegacyVarCannotTurnRequirementOff — the old opt-in variable
// stays honoured (make test-integration and CI both set it), but it no longer
// GRANTS the requirement, so setting it to something false-looking must not
// silently restore the old trap.
func TestRequireDocker_LegacyVarCannotTurnRequirementOff(t *testing.T) {
	t.Setenv(SkipEnv, "")
	for _, v := range []string{"", "0", "false", "no"} {
		t.Setenv(RequireEnv, v)
		if !RequireDocker() {
			t.Errorf("RequireDocker() = false with %s=%q — only %s may relax the requirement", RequireEnv, v, SkipEnv)
		}
	}
}

// dockerClientEnv — docker's OWN client configuration. These are not ours to
// define and they say nothing about whether a suite may skip: reading them is
// talking TO docker, not deciding whether to test. keeper/internal/trial probes
// DOCKER_HOST looking for a socket, and two registry helpers read
// DOCKER_AUTH_CONFIG for pull credentials.
//
// The exemption is deliberately a FOREIGN vocabulary — a fixed list published by
// the docker CLI, not a list we extend as we invent things. Every name we coin
// ourselves stays caught, however it is spelled.
var dockerClientEnv = map[string]bool{
	"DOCKER_API_VERSION": true,
	"DOCKER_AUTH_CONFIG": true,
	"DOCKER_CERT_PATH":   true,
	"DOCKER_CONFIG":      true,
	"DOCKER_CONTEXT":     true,
	"DOCKER_HOST":        true,
	"DOCKER_TLS_VERIFY":  true,
}

// isPolicyEnv reports whether reading name re-forks the skip-or-fail decision
// this package owns.
//
// Matched by PROPERTY, not by name — this is half of NIM-481. The guard used to
// compare against SkipEnv and RequireEnv literally, i.e. against the two
// constants NIM-238 itself had just introduced. That can only catch a FUTURE fork
// spelled in the new vocabulary, and is blind by construction to the ~35-copy
// population the guard exists to finish off, every one of which predates those
// names. One copy was still alive in keeper/internal/oracle on the pre-NIM-238
// name REQUIRE_DOCKER, exiting 0 with zero tests run, and this guard had been
// green over it the whole time.
//
// The property: the variable is about docker, or it sits in our own
// SOUL_STACK_INTEGRATION_* namespace. Both hold for names nobody has coined yet.
func isPolicyEnv(name string) bool {
	upper := strings.ToUpper(name)
	if dockerClientEnv[upper] {
		return false
	}
	return strings.Contains(upper, "DOCKER") || strings.HasPrefix(upper, "SOUL_STACK_INTEGRATION")
}

// exemptFiles — files that must ask the environment themselves because the helper
// is genuinely out of reach. Each entry is a hole in the rule, so each states why
// it had to be cut, and an entry that stops firing fails the guard: a stale
// exemption covers whatever moves into it next.
//
// Empty since NIM-761 removed examples/module/*, which held the only
// one. That is the strongest state for this guard, not a gap in it: every file
// under the checkout now goes through the helper.
var exemptFiles = map[string]string{}

// skipDirs — not our source: generated stubs, build output, fixtures.
var skipDirs = map[string]bool{
	".git":         true,
	"bin":          true,
	"gen":          true,
	"node_modules": true,
	"testdata":     true,
}

type envRead struct {
	file string // repo-relative
	line int
	name string
}

// checkoutRoot walks up from this package to the directory holding go.work.
//
// The other half of NIM-481. The guard used to anchor on filepath.Join("..",
// ".."), which from here is the keeper module root — so shared/, soul/, tests/
// and examples/ were outside its world entirely, and a second copy of the trap
// was living in examples/module/aws the whole time. Anchoring on a
// marker instead of a hop count also means the guard fails loudly if the layout
// moves, rather than silently narrowing its coverage. (That second copy went with
// the CloudDriver examples in NIM-761; the anchoring is what still matters.)
func checkoutRoot(t *testing.T) string {
	t.Helper()
	start, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve own dir: %v", err)
	}
	for dir := start; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.work in any parent of %s — cannot locate the checkout root", start)
		}
		dir = parent
	}
}

// envReadsIn returns every environment variable this file asks for by name.
//
// Parsed, not grepped, for two reasons. Dozens of suites carry the run command in
// a doc comment (`SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=…`);
// flagging those would make the guard noise that gets silenced, so what is
// forbidden is code that asks the environment, not text that mentions it. And the
// old substring matcher only recognised one exact spelling — `os.Getenv("NAME"` —
// so a line break, a backtick, or a named constant walked straight past it.
//
// Known limit: an argument that resolves through another FILE of the same package
// is not followed. Same-file constants are, which is where these helpers live.
func envReadsIn(t *testing.T, root, path string, src []byte) []envRead {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// Same-file string constants, so `const dockerEnv = "…"; os.Getenv(dockerEnv)`
	// resolves rather than hiding the name from the matcher.
	consts := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if v, ok := stringLit(vs.Values[i]); ok {
					consts[name.Name] = v
				}
			}
		}
	}

	// This package's own exported names, for `os.Getenv(integrationenv.SkipEnv)`
	// from outside: borrowing the constant is still re-forking the decision.
	exported := map[string]string{"SkipEnv": SkipEnv, "RequireEnv": RequireEnv}

	rel, _ := filepath.Rel(root, path)
	var reads []envRead
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv") {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
			return true
		}

		var name string
		switch arg := call.Args[0].(type) {
		case *ast.BasicLit:
			name, _ = stringLit(arg)
		case *ast.Ident:
			name = consts[arg.Name]
		case *ast.SelectorExpr:
			name = exported[arg.Sel.Name]
		}
		if name != "" {
			reads = append(reads, envRead{file: rel, line: fset.Position(call.Pos()).Line, name: name})
		}
		return true
	})
	return reads
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// TestNoPackageReadsTheDockerEnvItself is the drift guard, and the reason this
// package exists at all rather than 35 copies of one `if`.
//
// Those copies were byte-identical in behaviour and all wrong in the same
// direction; nothing stopped a 36th from appearing, and nothing announced it
// when one did. The rule is now: exactly one place reads these variables. A
// package that starts reading them again is re-forking the decision, so the
// gate says so by name instead of waiting for a suite to go quietly green.
func TestNoPackageReadsTheDockerEnvItself(t *testing.T) {
	root := checkoutRoot(t)
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve own dir: %v", err)
	}

	var offenders []string
	firedExemptions := map[string]bool{}

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// This package is the one legitimate reader.
			if path == self || skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// Cheap gate before parsing: the tree is ~2200 files and fewer than forty
		// of them read the environment at all.
		if !bytes.Contains(src, []byte("os.Getenv")) && !bytes.Contains(src, []byte("os.LookupEnv")) {
			return nil
		}
		for _, read := range envReadsIn(t, root, path, src) {
			if !isPolicyEnv(read.name) {
				continue
			}
			if _, exempt := exemptFiles[read.file]; exempt {
				firedExemptions[read.file] = true
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s:%d reads %s", read.file, read.line, read.name))
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("these files decide the docker skip-or-fail question themselves instead of calling integrationenv.RequireDocker():\n\t%s\n"+
			"That is how the decision forked into ~35 copies that all defaulted to a silent skip (NIM-238), "+
			"and how one of them survived the cleanup unnoticed (NIM-481). "+
			"Call the shared helper, or change the policy here where it is one edit.", strings.Join(offenders, "\n\t"))
	}

	for file, reason := range exemptFiles {
		if firedExemptions[file] {
			continue
		}
		t.Errorf("exemption for %s no longer matches anything (%q).\n"+
			"Either the file stopped reading the environment — then delete the entry, because an exemption "+
			"nobody needs quietly covers whatever moves into that path next — or it moved, and the hole moved with it.", file, reason)
	}
}
