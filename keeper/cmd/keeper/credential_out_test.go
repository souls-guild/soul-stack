package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstrap"
)

// NIM-420 has two halves. The bootstrap package owns the writing; this
// file owns the CLI half, which nothing covered before: the `-` → stdout
// mapping, and the move of every human-facing line to stderr so that
// `keeper init --credential-out=- > archon.jwt` yields a file holding
// the JWT and nothing else.

func TestCredentialSink_DashHandsBootstrapStdoutAndOpensNoPath(t *testing.T) {
	t.Parallel()
	w, label := credentialSink(credentialStdoutArg)
	if w != io.Writer(os.Stdout) {
		t.Errorf("writer = %v, want os.Stdout — the token would go somewhere else entirely", w)
	}
	if label != "stdout" {
		t.Errorf("label = %q, want %q", label, "stdout")
	}
}

// The label must not stay `-`: with CredentialWriter set bootstrap never
// opens it, so it is only ever printed, and "Token written to -" reads
// like a file called `-`.
func TestCredentialSink_DashDoesNotLeakIntoTheLabel(t *testing.T) {
	t.Parallel()
	if _, label := credentialSink(credentialStdoutArg); label == credentialStdoutArg {
		t.Errorf("label = %q — the sentinel reached the message operators read", label)
	}
}

// Everything that is not the sentinel stays a path, untouched, and gets
// no writer — otherwise bootstrap would skip the filesystem for a path
// the operator meant literally. `./-` is the documented escape for a
// file genuinely named `-`, and it must survive as itself.
func TestCredentialSink_PathsAreLeftToBootstrap(t *testing.T) {
	t.Parallel()
	for _, credOut := range []string{"/etc/keeper/archon-alice.jwt", "./-", "-x", "--", ""} {
		w, label := credentialSink(credOut)
		if w != nil {
			t.Errorf("credentialSink(%q) returned a writer; bootstrap would never open the path", credOut)
		}
		if label != credOut {
			t.Errorf("credentialSink(%q) label = %q, want it unchanged", credOut, label)
		}
	}
}

func TestReportInit_StreamDestinationCarriesTheCompromiseWarning(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	reportInit(&buf, &bootstrap.Result{CredentialPath: "stdout", CredentialIsStream: true})

	got := buf.String()
	if !strings.Contains(got, "WARNING") || !strings.Contains(got, "rotate") {
		t.Errorf("stream report = %q, want the compromise warning", got)
	}
	if !strings.Contains(got, "Bootstrap complete. Token written to stdout") {
		t.Errorf("stream report = %q, want the completion line too", got)
	}
}

// The mode 0400 file is the case where the warning would be a lie.
func TestReportInit_FileDestinationHasNoStreamWarning(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	reportInit(&buf, &bootstrap.Result{CredentialPath: "/etc/keeper/archon-alice.jwt"})

	got := buf.String()
	if strings.Contains(got, "WARNING") {
		t.Errorf("file report = %q, want no stream warning", got)
	}
	if !strings.Contains(got, "Bootstrap complete. Token written to /etc/keeper/archon-alice.jwt") {
		t.Errorf("file report = %q, want the completion line", got)
	}
}

// The two reportInit tests above pass it a buffer, so they only prove
// reportInit writes where it is told. What they cannot see is the choice
// of writer at the call site, or a line that bypasses the writer
// entirely — `fmt.Println` and friends go to stdout with no argument
// naming it. Both are one careless line away, and on this path stdout is
// reserved: with `--credential-out=-` the token is the only thing in it.
func TestInitPath_WritesNothingToStdout(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// credentialSink is deliberately absent: handing os.Stdout to
	// bootstrap is the whole feature.
	implicitStdout := map[string]bool{"Print": true, "Printf": true, "Println": true}
	for _, name := range []string{"runInit", "reportInit"} {
		fn := findFuncDecl(file, name)
		if fn == nil {
			t.Fatalf("func %s not found in main.go — this guard now guards nothing", name)
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case pkg.Name == "os" && sel.Sel.Name == "Stdout":
				t.Errorf("%s references os.Stdout at %s — `--credential-out=- > f` would put this in the credential file",
					name, fset.Position(sel.Pos()))
			case pkg.Name == "fmt" && implicitStdout[sel.Sel.Name]:
				t.Errorf("%s calls fmt.%s at %s — that is stdout without saying so; use the Fprint form on stderr",
					name, sel.Sel.Name, fset.Position(sel.Pos()))
			}
			return true
		})
	}
}

// A write to a broken fd 1 or 2 raises SIGPIPE, and its default
// disposition kills the process. With the reader gone before the write —
// an interrupted `kubectl exec`, a consumer that died during the seconds
// keeper spends on Vault and migrations — `keeper init
// --credential-out=-` died at exit 141 with an empty stderr, after the
// operator row was committed and the audit written, and before the
// ErrTokenFileWriteFailed branch could print the token that is by then
// the only copy. signal.Notify is what makes the write return EPIPE
// instead.
//
// `| head -1` is not the example, though it reads like one: the token is
// a single write that fits the pipe buffer, so it lands before head has
// exited and the run ends 0 with or without the arming (measured on both
// builds). Citing it here would leave the next reader unable to
// reproduce the thing this guard exists for.
//
// It is guarded by shape because the alternative is a subprocess test
// that has to reach a committed bootstrap first, and because the line is
// pure setup: nothing reads the channel, so nothing else fails when it
// goes missing.
func TestInitPath_ArmsSIGPIPESoABrokenPipeIsRecoverable(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	fn := findFuncDecl(file, "runInit")
	if fn == nil {
		t.Fatal("func runInit not found in main.go — this guard now guards nothing")
	}

	var armed bool
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Notify" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "signal" {
			return true
		}
		for _, arg := range call.Args {
			if sig, ok := arg.(*ast.SelectorExpr); ok && sig.Sel.Name == "SIGPIPE" {
				armed = true
			}
		}
		return true
	})
	if !armed {
		t.Error("runInit does not signal.Notify SIGPIPE — a stdout reader that leaves before the write kills init at 141, past the commit, with the token lost")
	}
}

func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}
