package middleware

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// gettingStartedPath, verifierSourcePath and authSourcePath are filesystem
// paths relative to this package directory, not go imports: docs/ sits at the
// repo root, and the detail strings are unexported literals.
const (
	gettingStartedPath = "../../../../docs/getting-started.md"
	verifierSourcePath = "../../jwt/verifier.go"
	authSourcePath     = "auth.go"
)

// detailCountWords maps a set size onto the word the getting-started list uses
// to state its own size. Only the plausible sizes are here; an unmapped one
// fails the test, which is the correct answer for a list that grew past what
// anyone wrote down.
var detailCountWords = map[int]string{3: "three", 4: "four", 5: "five", 6: "six", 7: "seven"}

// docCountClaim matches the sentence in which the list claims to be complete.
var docCountClaim = regexp.MustCompile("`detail` is one of (\\w+) fixed strings")

// TestUnauthenticatedDetails_DocumentedExhaustively binds the operator-facing
// 401 list in getting-started.md to the middleware that produces it.
//
// The list is read by someone holding a 401 they believe is wrong, and it tells
// them which causes a fresh token repairs and which it hides. That only works
// while it covers every `detail` [RequireJWT] can write, and the entry this
// replaces covered none of them by name; the first attempt at a list named the
// four the verifier classifies and left out the one written a branch above,
// before verification happens at all, which is the likeliest 401 on a request
// whose token is perfectly valid (NIM-664).
//
// So both halves are pinned. Every string has to appear, and the count the list
// states about itself has to match how many there are: a list that says "four"
// while showing five is the same defect stated the other way round, and prose
// cannot be made to fail a build any other way.
//
// The bound: the set is every string literal that reaches the wire as a
// `detail` from [RequireJWT] or from the classifier it delegates to, read out
// of those two functions. A detail assembled at run time, or written from a
// third file, is outside it — the login and exchange routes answer 401 outside
// this middleware. So is a fourth way of writing one from here: the scrape
// knows [problem.New] with [problem.TypeUnauthenticated] and this package's
// [WriteUnauthenticated], and a shape it does not know it cannot read. Both
// scrapes fail loudly when they stop understanding what they are reading,
// because a guard that quietly finds nothing is worse than none.
func TestUnauthenticatedDetails_DocumentedExhaustively(t *testing.T) {
	details := unauthenticatedDetails(t)

	raw, err := os.ReadFile(gettingStartedPath)
	if err != nil {
		t.Fatalf("read %s: %v", gettingStartedPath, err)
	}
	doc := string(raw)

	for _, d := range details {
		// The list writes each detail backtick-wrapped; matching that form
		// rather than the bare string keeps a mention in surrounding prose from
		// passing for an entry.
		if !strings.Contains(doc, "`"+d+"`") {
			t.Errorf("401 detail %q is not in the list in %s. An operator reads that list to "+
				"decide whether to reissue; a cause missing from it is one they will diagnose "+
				"by guessing", d, gettingStartedPath)
		}
	}

	m := docCountClaim.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("%s no longer states how many `detail` strings the list covers. That claim is "+
			"what makes the list usable as an exhaustive one, and it is what this test holds to "+
			"the code", gettingStartedPath)
	}
	want, ok := detailCountWords[len(details)]
	if !ok {
		t.Fatalf("%d unauthenticated details and no word for that count in detailCountWords", len(details))
	}
	if m[1] != want {
		t.Errorf("%s says the list is one of %s fixed strings, but RequireJWT writes %d of them (%q). "+
			"A list that undercounts itself reads as complete while it is not",
			gettingStartedPath, m[1], len(details), details)
	}
}

// unauthenticatedDetails returns every `detail` string a 401 from [RequireJWT]
// can carry, sorted and deduplicated.
func unauthenticatedDetails(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, d := range append(classifierDetails(t), middlewareDetails(t)...) {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)

	// The scrapes read source; this replays one real request, so a literal
	// renamed in auth.go cannot agree with a copy here while the wire says
	// something else.
	onWire := missingHeaderDetail(t)
	if !seen[onWire] {
		t.Fatalf("a request with no Authorization header answered detail %q, which is not among the "+
			"literals read out of %s (%q). The scrape has stopped describing the wire",
			onWire, authSourcePath, out)
	}
	return out
}

// classifierDetails returns the 401 `detail` strings [jwt.ClassifyVerifyErr]
// can return, read out of the verifier's source. It follows the returns rather
// than the constant declarations: a case returning a bare literal is exactly
// the way a new cause reaches the wire unannounced.
func classifierDetails(t *testing.T) []string {
	t.Helper()
	file := parseSource(t, verifierSourcePath)

	consts := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if s, ok := stringLit(vs.Values[i]); ok {
						consts[name.Name] = s
					}
				}
			}
		}
	}

	fn := funcDecl(file, "ClassifyVerifyErr")
	if fn == nil {
		t.Fatalf("%s no longer declares ClassifyVerifyErr. It is what turns a verify error into the "+
			"string on the wire; without it this test silently checks nothing", verifierSourcePath)
	}

	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return true
		}
		switch e := ret.Results[0].(type) {
		case *ast.BasicLit:
			s, ok := stringLit(e)
			if !ok {
				t.Errorf("ClassifyVerifyErr returns a non-string literal; this test can no longer "+
					"read the set of details out of %s", verifierSourcePath)
				return false
			}
			if s != "" { // the documented nil-error answer, not a detail
				out = append(out, s)
			}
		case *ast.Ident:
			s, ok := consts[e.Name]
			if !ok {
				t.Errorf("ClassifyVerifyErr returns %s, which is not a string constant declared in "+
					"%s. The set of details on the wire is no longer readable from this file",
					e.Name, verifierSourcePath)
				return false
			}
			out = append(out, s)
		default:
			t.Errorf("ClassifyVerifyErr returns an expression this test cannot read (%T). A detail "+
				"built at run time is outside what it can hold the doc to", e)
			return false
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("ClassifyVerifyErr in %s returns no readable detail string", verifierSourcePath)
	}
	return out
}

// middlewareDetails returns the `detail` literals [RequireJWT] writes itself,
// before or beside the classifier — the branch the first version of the list
// left out.
func middlewareDetails(t *testing.T) []string {
	t.Helper()
	file := parseSource(t, authSourcePath)

	fn := funcDecl(file, "RequireJWT")
	if fn == nil {
		t.Fatalf("%s no longer declares RequireJWT", authSourcePath)
	}

	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		// Two shapes write an unauthenticated detail from here: the problem
		// constructor, and this package's own wrapper around it. Anything else
		// is a shape this scrape does not know, which is what the bound says.
		var detail ast.Expr
		switch {
		case isSelector(call.Fun, "problem", "New") && isSelector(call.Args[0], "problem", "TypeUnauthenticated"):
			detail = call.Args[2]
		case isIdent(call.Fun, "WriteUnauthenticated"):
			detail = call.Args[2]
		default:
			return true
		}

		if s, ok := stringLit(detail); ok {
			out = append(out, s)
			return true
		}
		// The one non-literal detail is the classifier's own answer, read
		// separately. Anything else is a detail this test cannot see.
		if c, ok := detail.(*ast.CallExpr); ok && isSelector(c.Fun, "jwt", "ClassifyVerifyErr") {
			return true
		}
		t.Errorf("RequireJWT writes a 401 detail that is neither a literal nor "+
			"jwt.ClassifyVerifyErr; %s can no longer be held to the causes it lists", gettingStartedPath)
		return false
	})
	if len(out) == 0 {
		t.Fatalf("RequireJWT in %s writes no literal detail of its own. The header branch is the one "+
			"the list left out; if it is gone, this test guards less than it claims", authSourcePath)
	}
	return out
}

func parseSource(t *testing.T, path string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return file
}

func funcDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

func isIdent(e ast.Expr, name string) bool {
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == name
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// missingHeaderDetail returns the `detail` [RequireJWT] writes when no token
// reaches the verifier at all, taken from a real response.
func missingHeaderDetail(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	RequireJWT(newVerifier(t))(nextShouldNotRun(t)).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))

	var p problem.Details
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decode the problem for a request with no Authorization header: %v", err)
	}
	if p.Type != problem.TypeUnauthenticated || p.Detail == "" {
		t.Fatalf("a request with no Authorization header answered type=%q detail=%q, want %q with a "+
			"detail naming the cause", p.Type, p.Detail, problem.TypeUnauthenticated)
	}
	return p.Detail
}
