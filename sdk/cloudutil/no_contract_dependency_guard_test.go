package cloudutil

// Guard for the reason this package exists apart from the contract it was carved out
// of (NIM-761).
//
// `sdk/clouddriver` held two unrelated halves: the CloudDriver gRPC contract, and the
// convergence plumbing every slow-remote-API plugin needs — retry, wait-until-ready,
// confirm-destroy, error classification. The contract went away with NIM-761; the
// plumbing moved here verbatim because it was never about clouds and its only live
// consumer, a cloud provider plugin, already links `sdk/module` for its actual contract.
//
// The invariant is that the two halves stay apart. A single `pluginv1` import here and
// the package is contract-coupled again — silently, because it would still compile and
// every test would still pass. `ReportDestroy` was exactly that shape: the one function
// in `destroy.go` that took a `grpc.ServerStreamingServer[pluginv1.DestroyEvent]`, and
// the one that could not move. It was deleted rather than carried over.
//
// Non-test files only: a test may legitimately want a proto type to build a fixture,
// and a fixture does not couple the shipped package to anything.
//
// Two checks, because the direct one is not enough. The AST pass reads THIS package's
// own import lines and is what names the offending file; the `go list -deps` pass reads
// the whole transitive closure, which is where the realistic regression lives — importing
// `sdk/module` here would pull `pluginv1` and `grpc` in behind it, and the AST pass would
// see nothing but a `sdk/module` import that looks innocent.

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenImports — prefixes no non-test file in this package may import. Prefixes
// rather than exact paths, so a future `proto/plugin/v2` is caught by the same rule.
var forbiddenImports = []string{
	"github.com/souls-guild/soul-stack/proto/",
	"google.golang.org/grpc",
}

func TestCloudutilDoesNotDependOnAPluginContract(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++

		f, err := parser.ParseFile(fset, filepath.Clean(name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, spec.Path.Value, err)
			}
			for _, bad := range forbiddenImports {
				if strings.HasPrefix(path, bad) {
					t.Errorf("%s imports %q — this package is the CONTRACT-FREE half of the former "+
						"sdk/clouddriver (NIM-761), and a plugin-contract dependency here re-couples it "+
						"to a wire format it has no business knowing. Put the contract-shaped code in "+
						"the plugin, or in a package that admits to serving a contract.", name, path)
				}
			}
		}
	}

	// A rename or a move would otherwise leave this test passing over nothing.
	if scanned == 0 {
		t.Fatal("no non-test .go files found in the package — the guard would pass vacuously")
	}
}

// TestCloudutilTransitiveClosureIsContractFree closes the hole the AST pass leaves: an
// import that is itself innocent but drags the contract in behind it. `go list -deps`
// answers for the whole closure of the non-test build, which is the thing that actually
// ships inside a plugin binary.
func TestCloudutilTransitiveClosureIsContractFree(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Skipf("go list unavailable (%v): %s", err, out)
	}
	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatal("go list -deps returned nothing — the guard would pass vacuously")
	}
	// Sanity: the closure must contain the package itself, or the command answered
	// about something else and every assertion below is about the wrong set.
	var sawSelf bool
	for _, d := range deps {
		if d == "github.com/souls-guild/soul-stack/sdk/cloudutil" {
			sawSelf = true
		}
	}
	if !sawSelf {
		t.Fatalf("go list -deps did not report sdk/cloudutil itself; the closure is not this package's: %v", deps)
	}
	for _, d := range deps {
		for _, bad := range forbiddenImports {
			if strings.HasPrefix(d, bad) {
				t.Errorf("the build closure of sdk/cloudutil reaches %q — something it imports pulls the "+
					"plugin wire format in transitively, which is the same coupling the direct check "+
					"forbids, only harder to see. Run `go list -deps .` and follow the chain.", d)
			}
		}
	}
}
