package wire_test

// The guard for NIM-776: a consumer of the Operator API must NAME the wire
// type, not re-describe it.
//
// WHY A TEST AND NOT A REVIEW RULE. Re-describing a body is invisible to every
// other check in this repository. encoding/json ignores a key it was not told
// about, so a copy that has drifted decodes into a zero value and the request
// or the assert built on it is quietly wrong; the build stays green, and so do
// the tests, because a stub that writes the old key and a decoder that reads
// the old key agree with each other about a contract neither of them is. That
// is how NIM-729's rename left `soulctl push-provider create` sending `name`
// into a schema that had become `id` + additionalProperties:false — dead, and
// nothing said so until a human read the diff. This test would have refused the
// copy on the day it was written, which is the only moment at which refusing it
// is cheap; see the limits below for what it does not do afterwards.
//
// WHAT IT CHECKS. Every struct declared under a consumer tree that carries
// json tags is compared, by its set of json keys, against the wire types. A
// consumer key set that is a SUBSET of some wire type's key set is a copy of
// that body — whole or partial — and fails. Naming the wire type (or aliasing
// it) declares no fields of its own, so it never matches.
//
// WHAT IT DOES NOT CATCH, stated so the green is not read as more than it is:
//
//   - A domain that has not moved to this package yet has nothing to be a
//     subset OF, so its hand-written readers pass. The known ones today are
//     the RFC 7807 error body (soulctl's client.APIError restates four fields
//     of keeper/internal/api/problem.Details), the service-registration reply
//     read by the e2e git helpers, the augur create reply read by
//     tests/e2e/harness/oracle.go, the sigil listing read by
//     tests/e2e-live/harness/plugin.go, and the JWT claim set decoded by
//     soulctl's client.JWTClaims, which has no server-side struct at all. Each
//     starts failing here the day its declaration lands in this package —
//     the guard's reach grows with the package rather than needing an edit.
//   - A single-key reader whose key is one of the ubiquitous identifiers
//     (see ubiquitousKeys): `struct{ ID string }` says nothing about which
//     body it came from, and treating it as a copy would fire on every
//     unrelated decode.
//   - A REQUEST BODY BUILT AS A MAP. `map[string]any{"kind": ..., "target": ...}`
//     carries no json tags, so nothing here can see it, and the harnesses still
//     build several bodies that way. That is the same NIM-729 shape — a renamed
//     key against additionalProperties:false — so a map body is worth converting
//     when you touch one, not worth trusting because this test is green.
//   - DRIFT IN A COPY THAT ALREADY EXISTS. The subset rule catches a copy while
//     it still matches; once a field is renamed on the server the copy's old key
//     is no longer in the wire set, so the copy stops being a subset and this
//     test goes quiet on exactly the broken call. It prevents copies from being
//     written; it does not detect one that got in before it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// consumerTrees are the repo-relative roots that speak the Operator API from
// outside the keeper module. A new one (another CLI, another harness) belongs
// in this list on the day it is created.
var consumerTrees = []string{
	"soulctl",
	"tests/e2e",
	"tests/e2e-live",
	"tests/e2e-k8s",
	"tests/load",
}

// ubiquitousKeys are json keys that identify no particular body on their own.
// A one-field struct reading only one of these is not evidence of a copy.
var ubiquitousKeys = map[string]bool{
	"id": true, "name": true, "sid": true, "status": true, "error": true,
	// A lone `items` is a generic page wrapper, not a body: the four-key
	// envelope {items,offset,limit,total} still fires, which is the one that
	// means something.
	"items": true,
}

// allowedCopies lists the structs that ARE subsets of a wire type but are not
// copies of one. The key is "<repo-relative file>:<type name>:<sorted keys>" and
// not the key set alone, so an allowance written for one declaration cannot
// silently cover a real copy someone adds elsewhere with the same fields. Every
// entry carries the reason; an entry that stops matching anything fails the
// test, so the list cannot rot into permission.
var allowedCopies = map[string]string{
	"soulctl/internal/cmd/souls.go:bulkRes:error,sid,status": "soulctl's OWN summary of a " +
		"client-side fan-out over PUT /v1/souls/{sid}/ssh-target - it is printed, never decoded " +
		"from a response, and collides with RunTaskHostEntry only because both describe a " +
		"per-host outcome.",
}

func TestConsumersDoNotRedeclareWireTypes(t *testing.T) {
	root := repoRoot(t)

	wireSets := collectJSONStructs(t, filepath.Join(root, "shared", "api", "wire"))
	// A parse that silently found little would make this test pass by having no
	// yardstick to compare against. The floor tracks the package: ~90 today, so
	// 70 catches a broken walk or a botched deletion without tripping on the
	// ordinary removal of a domain.
	if len(wireSets) < 70 {
		t.Fatalf("only %d wire types found under shared/api/wire — the guard has no yardstick", len(wireSets))
	}

	usedAllowances := map[string]bool{}
	for _, tree := range consumerTrees {
		for _, got := range collectJSONStructs(t, filepath.Join(root, tree)) {
			if len(got.keys) == 0 {
				continue
			}
			if len(got.keys) == 1 && ubiquitousKeys[got.keys[0]] {
				continue
			}
			canon := strings.Join(got.keys, ",")
			allowKey := got.file + ":" + got.name + ":" + canon
			for _, want := range wireSets {
				if !isCopyOf(got.keys, want.keys) {
					continue
				}
				if _, ok := allowedCopies[allowKey]; ok {
					usedAllowances[allowKey] = true
					break
				}
				t.Errorf("%s:%d: %s re-describes the wire body %s (keys %s).\n"+
					"\tName wire.%s instead of restating its fields: a copy decodes an "+
					"unknown key into a zero value, so it diverges without failing anything (NIM-776).",
					got.file, got.line, got.name, want.name, canon, want.name)
				break
			}
		}
	}

	for key, reason := range allowedCopies {
		if !usedAllowances[key] {
			t.Errorf("allowedCopies has a stale entry %q (%s): nothing matches it any more — delete it", key, reason)
		}
	}
}

type jsonStruct struct {
	name string
	file string
	line int
	keys []string // sorted, deduplicated, "-" excluded
}

// collectJSONStructs parses every .go file under dir and returns one entry per
// struct type carrying at least one json tag. Anonymous structs count: an
// inline `struct{ ApplyID string \`json:"apply_id"\` }` in a decode is the same
// second description as a named one.
func collectJSONStructs(t *testing.T, dir string) []jsonStruct {
	t.Helper()
	var out []jsonStruct
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "vendor" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(repoRootOf(dir), path)
		// The enclosing type name, when there is one, makes the failure
		// message point at something the reader can grep for.
		enclosing := ""
		ast.Inspect(f, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				enclosing = ts.Name.Name
			}
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			keys := jsonKeys(st)
			if len(keys) == 0 {
				return true
			}
			name := enclosing
			if name == "" {
				name = "anonymous struct"
			}
			out = append(out, jsonStruct{
				name: name,
				file: rel,
				line: fset.Position(st.Pos()).Line,
				keys: keys,
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

// jsonKeys extracts the sorted, deduplicated json key names of a struct's own
// fields. `json:"-"` and untagged fields contribute nothing: they are not part
// of any wire description.
func jsonKeys(st *ast.StructType) []string {
	seen := map[string]bool{}
	for _, fld := range st.Fields.List {
		if fld.Tag == nil {
			continue
		}
		raw, err := unquoteTag(fld.Tag.Value)
		if err != nil {
			continue
		}
		v, ok := reflect.StructTag(raw).Lookup("json")
		if !ok {
			continue
		}
		key, _, _ := strings.Cut(v, ",")
		if key == "" || key == "-" {
			continue
		}
		seen[key] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// isCopyOf reports whether got describes want. A subset is the plain case — a
// whole or partial restatement of the body. One key that is NOT in want is
// tolerated as long as at least two ARE: a harness that copies a body and hangs
// one bookkeeping field off it is the same second description, and requiring a
// strict subset would let it through.
func isCopyOf(got, want []string) bool {
	shared, extra := 0, 0
	for _, k := range got {
		if slices.Contains(want, k) {
			shared++
		} else {
			extra++
		}
	}
	switch {
	case extra == 0:
		return shared > 0
	case extra == 1:
		return shared >= 2
	default:
		return false
	}
}

// unquoteTag strips the Go quoting off a struct tag literal. Both forms are
// legal — a raw backquoted string and an interpreted double-quoted one — and
// strconv.Unquote handles each, so a tag written the second way is inspected
// rather than silently skipped for having no readable json key.
func unquoteTag(lit string) (string, error) {
	return strconv.Unquote(lit)
}

// repoRoot walks up from the test's directory to the tree that holds go.work.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.work above %s — cannot locate the repo root", wd)
		}
		dir = parent
	}
}

// repoRootOf is repoRoot without a *testing.T, for rendering paths.
func repoRootOf(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
