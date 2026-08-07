package bootstrap

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Two invariants of the credential writer that no behavioural test can
// hold down, because the mutations that break them are observable only
// under a race. They are cheap to reintroduce and expensive to notice,
// so they are pinned to the shape of the source instead.
//
// Both are phrased against what a stat call is applied to — a name or a
// descriptor — rather than against which package it came from. That
// distinction is the whole point and was got wrong once already: an
// earlier version of the fd guard accepted any package-qualified `Stat`
// except `os.`, so `syscall.Stat(path, &raw)` — the exact regression it
// exists to catch — passed it, while the legitimate
// `syscall.Fstat(int(f.Fd()), &raw)` would have failed it. Hence
// TestShapeGuards_CatchWhatTheyClaimTo below, which runs the guards
// against sources that are known-bad and known-good.

// pathStat and fdStat name the two ways of asking, keyed by the
// package-level spelling. A method call on the opened file is handled
// separately: the receiver is a variable, not a package.
var (
	pathStat = map[string]bool{"os.Stat": true, "os.Lstat": true, "syscall.Stat": true, "syscall.Lstat": true}
	fdStat   = map[string]bool{"syscall.Fstat": true}
)

// A name is not a destination. Lstat classifies the name, which is all
// it can do; Stat resolves the whole chain, and /dev/stdout is a symlink
// to /proc/self/fd/1, itself a symlink to whatever fd 1 is — so with
// stdout redirected to a file, Stat answers "regular file" and hands the
// path to the branch that begins by unlinking it.
//
// TestWriteTokenFile_SymlinkToRegularFileIsRefused does catch this one
// today. This guard is here because that test catches it for a reason
// that could be removed on purpose — someone deciding the symlink case
// is over-strict — while the /dev/stdout consequence is a separate fact
// that would then go unmentioned.
func TestWriteTokenFile_ClassifiesTheNameNotItsTarget(t *testing.T) {
	t.Parallel()
	fn := parseFuncDecl(t, "keeper_init.go", "writeTokenFile")
	for _, c := range nameStatComplaints(fn) {
		t.Error(c)
	}
}

// nameStatComplaints reports why fn does not classify its target by the
// name alone. Empty means it does.
func nameStatComplaints(fn *ast.FuncDecl) []string {
	var out []string
	var sawLstat bool
	ast.Inspect(fn, func(n ast.Node) bool {
		switch qualifiedName(n) {
		case "os.Lstat", "syscall.Lstat":
			sawLstat = true
		case "os.Stat", "syscall.Stat":
			out = append(out, fmt.Sprintf("%s resolves the symlink chain — /dev/stdout >file "+
				"would answer \"regular file\" and be unlinked; classify the name with Lstat",
				qualifiedName(n)))
		}
		return true
	})
	if !sawLstat {
		out = append(out, "the target is no longer classified with Lstat — this guard now guards nothing")
	}
	return out
}

// writeTokenInPlace re-checks its target after opening it, and the
// re-check has to derive from the descriptor. A second lookup of the
// path passes every test in this package: for a stable path the two
// agree, and the disagreement needs the path swapped between the open
// and the check. That is exactly the attack the re-check exists for —
// the fd pins the inode, a name does not — and it is not something a
// test can schedule.
func TestWriteTokenInPlace_ChecksTheOpenFdNotThePathAgain(t *testing.T) {
	t.Parallel()
	fn := parseFuncDecl(t, "keeper_init.go", "writeTokenInPlace")
	for _, c := range fdStatComplaints(fn) {
		t.Error(c)
	}
}

// fdStatComplaints reports why fn re-checks something other than the
// descriptor it opened. Empty means it checks the descriptor.
//
// The opened-file variable is discovered from the os.OpenFile call
// rather than assumed to be named `f`, so that renaming it neither
// breaks the guard nor — the direction that actually matters — silently
// satisfies it.
func fdStatComplaints(fn *ast.FuncDecl) []string {
	var out []string
	file := openedFileVar(fn)
	if file == "" {
		return []string{"no os.OpenFile result to re-check — this guard now guards nothing"}
	}

	var sawFdStat bool
	ast.Inspect(fn, func(n ast.Node) bool {
		name := qualifiedName(n)
		switch {
		case pathStat[name]:
			out = append(out, fmt.Sprintf("%s looks the path up a second time — it can be a "+
				"different file by then; stat the descriptor", name))
		case fdStat[name], name == file+".Stat":
			sawFdStat = true
		}
		return true
	})
	if !sawFdStat {
		out = append(out, "what was opened is no longer stat'ed through its descriptor — "+
			"a symlink to a regular file is written through again")
	}
	return out
}

// openedFileVar returns the name the os.OpenFile result is bound to, or
// "" if there is no such call.
func openedFileVar(fn *ast.FuncDecl) string {
	var name string
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 || len(assign.Rhs) != 1 {
			return true
		}
		if qualifiedName(assign.Rhs[0]) != "os.OpenFile" {
			return true
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			name = id.Name
		}
		return true
	})
	return name
}

// qualifiedName renders a call's target as `receiver.Selector` when the
// receiver is a bare identifier, and "" for anything else. Both a
// package qualifier and a variable receiver come out the same way, which
// is what lets the guards talk about `f.Stat` and `syscall.Fstat` in one
// vocabulary.
func qualifiedName(n ast.Node) string {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return recv.Name + "." + sel.Sel.Name
}

// The guards above are the only backstop for two mutations that behave
// identically to correct code, so a guard that passes everything is
// worse than no guard: it reads as coverage. This runs each one against
// a source it must reject and a source it must accept.
//
// The known-bad cases are not hypothetical. `syscall.Stat` is the mutant
// that passed the previous version of the fd guard while reintroducing
// the TOCTOU window, and `syscall.Fstat` is the legitimate spelling that
// same version would have rejected.
func TestShapeGuards_CatchWhatTheyClaimTo(t *testing.T) {
	t.Parallel()

	inPlace := func(body string) string {
		return "package p\nfunc writeTokenInPlace() {\n" + body + "\n}\n"
	}
	byName := func(body string) string {
		return "package p\nfunc writeTokenFile() {\n" + body + "\n}\n"
	}

	cases := []struct {
		name    string
		src     string
		fn      string
		check   func(*ast.FuncDecl) []string
		wantBad bool
	}{{
		name:  "fd guard accepts a method call on the opened file",
		src:   inPlace("f, _ := os.OpenFile(path, 0, 0)\nst, _ := f.Stat()\n_ = st"),
		fn:    "writeTokenInPlace",
		check: fdStatComplaints,
	}, {
		name:  "fd guard accepts syscall.Fstat on the descriptor",
		src:   inPlace("f, _ := os.OpenFile(path, 0, 0)\nvar raw syscall.Stat_t\n_ = syscall.Fstat(int(f.Fd()), &raw)"),
		fn:    "writeTokenInPlace",
		check: fdStatComplaints,
	}, {
		name:  "fd guard accepts the file bound to any name",
		src:   inPlace("dst, _ := os.OpenFile(path, 0, 0)\nst, _ := dst.Stat()\n_ = st"),
		fn:    "writeTokenInPlace",
		check: fdStatComplaints,
	}, {
		name:    "fd guard rejects syscall.Stat on the path",
		src:     inPlace("f, _ := os.OpenFile(path, 0, 0)\nvar raw syscall.Stat_t\n_ = syscall.Stat(path, &raw)"),
		fn:      "writeTokenInPlace",
		check:   fdStatComplaints,
		wantBad: true,
	}, {
		name:    "fd guard rejects os.Stat on the path",
		src:     inPlace("f, _ := os.OpenFile(path, 0, 0)\nst, _ := os.Stat(path)\n_ = st"),
		fn:      "writeTokenInPlace",
		check:   fdStatComplaints,
		wantBad: true,
	}, {
		name:    "fd guard rejects a re-check that never happens",
		src:     inPlace("f, _ := os.OpenFile(path, 0, 0)\n_ = f"),
		fn:      "writeTokenInPlace",
		check:   fdStatComplaints,
		wantBad: true,
	}, {
		name:    "fd guard reports itself empty when the open is gone",
		src:     inPlace("st, _ := os.Stat(path)\n_ = st"),
		fn:      "writeTokenInPlace",
		check:   fdStatComplaints,
		wantBad: true,
	}, {
		name:  "name guard accepts os.Lstat",
		src:   byName("st, _ := os.Lstat(path)\n_ = st"),
		fn:    "writeTokenFile",
		check: nameStatComplaints,
	}, {
		name:    "name guard rejects os.Stat",
		src:     byName("st, _ := os.Stat(path)\n_ = st"),
		fn:      "writeTokenFile",
		check:   nameStatComplaints,
		wantBad: true,
	}, {
		name:    "name guard rejects syscall.Stat",
		src:     byName("var raw syscall.Stat_t\n_ = syscall.Stat(path, &raw)"),
		fn:      "writeTokenFile",
		check:   nameStatComplaints,
		wantBad: true,
	}, {
		name:    "name guard rejects classifying nothing at all",
		src:     byName("_ = path"),
		fn:      "writeTokenFile",
		check:   nameStatComplaints,
		wantBad: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.check(parseFuncSrc(t, tc.src, tc.fn))
			switch {
			case tc.wantBad && len(got) == 0:
				t.Errorf("guard passed a source it must reject:\n%s", tc.src)
			case !tc.wantBad && len(got) > 0:
				t.Errorf("guard rejected a source it must accept: %s\n%s",
					strings.Join(got, "; "), tc.src)
			}
		})
	}
}

func parseFuncDecl(t *testing.T, filename, name string) *ast.FuncDecl {
	t.Helper()
	return parseFunc(t, filename, nil, name)
}

func parseFuncSrc(t *testing.T, src, name string) *ast.FuncDecl {
	t.Helper()
	return parseFunc(t, "synthetic.go", src, name)
}

func parseFunc(t *testing.T, filename string, src any, name string) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("func %s not found in %s — this guard now guards nothing", name, filename)
	return nil
}
