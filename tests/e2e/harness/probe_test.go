//go:build e2e

package harness

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestIdentityFailureCauseSeparatesAClockFromAStranger — assertOwnKeeper's
// verdict must come from the keeper's detail string, not from the 401.
//
// Until NIM-621 it could not: a wall clock that stepped backwards between
// `keeper init` minting the token and this request produced the same
// `invalid token` a foreign keeper produces, and this check answered "this
// process is not the keeper this stack started" with an address to go and
// investigate. That is not a hypothetical on this box — the step was caught
// twice in one session (NIM-611) — and the reader is sent after a port
// collision that never happened, on a stack that is in fact its own.
//
// The detail strings are READ out of the keeper rather than written down here,
// for the reason suiteTimeoutFromMakefile reads its number: a copy stops
// matching the moment someone renames the original, and the test keeps passing
// against its copy while the harness silently falls back to the default reading.
func TestIdentityFailureCauseSeparatesAClockFromAStranger(t *testing.T) {
	skew := keeperPublicDetail(t, "publicDetailClockSkew")
	issuer := keeperPublicDetail(t, "publicDetailInvalidIssuer")
	generic := keeperPublicDetail(t, "publicDetailInvalidToken")

	defaultHint, defaultAdvice := identityFailureCause("")

	for _, tc := range []struct {
		name   string
		body   string
		expect string
	}{
		{"clock skew", `{"detail":"` + skew + `"}`, "clock"},
		{"foreign issuer", `{"detail":"` + issuer + `"}`, "issuer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hint, advice := identityFailureCause(tc.body)
			if hint == defaultHint || advice == defaultAdvice {
				t.Fatalf("body %q lands on the default reading (%q / %q). The keeper names this "+
					"cause exactly, and answering it with the address is how a %s problem gets "+
					"read as a port collision — the reader goes looking at ports on a stack that "+
					"is talking to its own keeper.", tc.body, hint, advice, tc.expect)
			}
			if !strings.Contains(hint+" "+advice, tc.expect) {
				t.Errorf("body %q is recognised but says nothing about the %s: %q / %q",
					tc.body, tc.expect, hint, advice)
			}
		})
	}

	// The other direction: the generic detail MUST stay on the default. If a
	// matcher above is widened until it swallows this one, every 401 starts
	// reporting a cause it does not have, which is worse than the default —
	// the default at least admits it does not know.
	if hint, advice := identityFailureCause(`{"detail":"` + generic + `"}`); hint != defaultHint || advice != defaultAdvice {
		t.Errorf("the keeper's catch-all detail %q is being read as a specific cause (%q / %q). "+
			"It is what a malformed token, a missing claim and a foreign signing key all produce; "+
			"naming any one of them here is a guess presented as a diagnosis.", generic, hint, advice)
	}
}

// keeperPublicDetail reads one publicDetail* literal out of the keeper's
// verifier. tests/e2e cannot import it — `keeper/internal/...` is internal to
// another module — so the coupling is textual and has to be checked as such.
func keeperPublicDetail(t *testing.T, name string) string {
	t.Helper()
	const verifier = "../../../keeper/internal/jwt/verifier.go"
	src, err := os.ReadFile(verifier)
	if err != nil {
		t.Fatalf("read %s: %v", verifier, err)
	}
	m := regexp.MustCompile(name + `\s*=\s*"([^"]+)"`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s no longer defines %s. assertOwnKeeper matches that string to tell a "+
			"clock problem from a wrong address; renamed or removed, the match silently stops "+
			"firing and every 401 goes back to reporting a port collision.", verifier, name)
	}
	return string(m[1])
}

// TestEveryStackConstructorProvesItsKeeperIsItsOwn — a stack handed to a test
// must have proved that the process answering on KeeperHTTPURL is the keeper it
// started.
//
// This guard exists because a mutation run on the NIM-469 guards found the hole:
// deleting the assertOwnKeeper call out of NewStack left every check in this
// package green. setupdecl_test.go looks at where the product calls sit relative
// to the declared bring-up region, which is a statement about POSITION and says
// nothing when a call is not there at all — and Go does not complain about an
// unused method, so the deletion compiles. That is precisely the shape of defect
// this ticket is about: the harness keeps running and keeps reporting, and what
// it reports is no longer true.
//
// The property is stated over the constructors as a class rather than over the
// two that exist today, so an entry point added later is covered on the day it
// is written. It is deliberately about presence only. Whether the call sits in
// the right place is the other test's question, and both must hold.
func TestEveryStackConstructorProvesItsKeeperIsItsOwn(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse harness sources: %v", err)
	}

	constructors := 0
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || !returnsStack(fn) {
					continue
				}
				constructors++
				if callsIdent(fn.Body, "assertOwnKeeper") {
					continue
				}
				t.Errorf("%s: %s returns a *Stack without calling assertOwnKeeper. Its "+
					"KeeperHTTPURL was reserved by opening :0 and closing it again, and /readyz "+
					"answers 2xx for whoever holds the port — so without this check a stack can "+
					"come up green pointed at another test's keeper and stay that way for the "+
					"whole run. What that looks like downstream is a 401 or a state assert "+
					"against an untouched database, hundreds of lines later.", path, fn.Name.Name)
			}
		}
	}

	// Closed in the other direction: if the constructors are renamed or their
	// signatures change, the loop above passes by matching nothing.
	if constructors < 2 {
		t.Fatalf("found %d function(s) returning a *Stack; NewStack and NewMultiKeeperStack "+
			"are both expected. Either one was removed, or this guard stopped recognising "+
			"the shape of a constructor and is now checking nothing.", constructors)
	}
}

// returnsStack reports whether fn's result list contains a *Stack. Results only
// — several helpers here TAKE a *Stack and return something else, and those are
// not constructors.
func returnsStack(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, res := range fn.Type.Results.List {
		star, ok := res.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if id, ok := star.X.(*ast.Ident); ok && id.Name == "Stack" {
			return true
		}
	}
	return false
}

// callsIdent reports whether body calls a function or method of this name,
// however it is qualified.
func callsIdent(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			found = found || f.Name == name
		case *ast.SelectorExpr:
			found = found || f.Sel.Name == name
		}
		return !found
	})
	return found
}
