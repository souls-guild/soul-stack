//go:build e2e

package harness

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/wait"
)

// The guard for the readiness properties in waitstrategy.go.
//
// It exists because the postgres half of this was not a subtle bug: the
// container's entire wait was testcontainers.WithWaitStrategy(ForLog×2), with no
// host-port check anywhere under it, and nothing went red for as long as anyone
// looked at it on an idle machine. The cost came due as NIM-469 — two or three
// unreproducible failures per full run, each of which read like a regression and
// none of which was one.
//
// Docker-free on purpose, and wired into `make check-e2e-set` so it runs in the
// gate everyone runs rather than only inside the 9-minute suite it is about.
//
// These are the sibling of tests/e2e-live/harness/waitstrategy_test.go (NIM-406),
// kept deliberately parallel: the same five properties, in the same order, with
// the same names. Two tiers that guard the same mechanism differently is how the
// tiers drift, and drift here is invisible until a gate run goes red under load.
// The one difference is the build tag — every file in THIS harness carries
// `e2e`, and un-tagging one file to match the sibling would buy nothing, since
// tests/e2e is a separate module that `go test ./...` at the repo root never
// reaches.

// stands enumerates the dependency containers Stack raises for itself, so a
// fourth cannot be added with a log-only wait and go unnoticed: the table is the
// checklist. Keep it in step with Stack.start* in stack.go.
//
// These three are the ones reached through a mapped port, which is the hop that
// fails. The keeper process is not among them — it is a subprocess on the host,
// not a container, and its readiness is assertOwnKeeper's job.
var stands = []struct {
	name     string
	port     string
	strategy func() wait.Strategy
}{
	{"postgres", postgresContainerPort, postgresWaitStrategy},
	{"redis", redisContainerPort, redisWaitStrategy},
	{"vault", vaultContainerPort, vaultWaitStrategy},
}

// flatten returns every strategy in the tree, walking into wait.ForAll sets.
func flatten(s wait.Strategy) []wait.Strategy {
	multi, ok := s.(*wait.MultiStrategy)
	if !ok {
		return []wait.Strategy{s}
	}
	var out []wait.Strategy
	for _, inner := range multi.Strategies {
		out = append(out, flatten(inner)...)
	}
	return out
}

// TestStandWaitsForItsMappedPort — every stand waits for docker to actually
// serve its port on the host.
//
// The check is deliberately not "a *HostPortStrategy is present". That type also
// comes out of wait.ForMappedPort, which waits only for the mapping to EXIST and
// never dials — and a strategy that waits for a port to be allocated while
// nothing accepts on it is precisely the failure this guards. The same object
// can also have the dial switched off after the fact with SkipExternalCheck().
// HostPortStrategy.String() reports which checks it actually does, so the
// assertion is on the check rather than on the type or on which constructor
// happened to make it.
func TestStandWaitsForItsMappedPort(t *testing.T) {
	for _, stand := range stands {
		t.Run(stand.name, func(t *testing.T) {
			var found *wait.HostPortStrategy
			for _, s := range flatten(stand.strategy()) {
				hp, ok := s.(*wait.HostPortStrategy)
				if ok && hp.Port == stand.port {
					found = hp
					break
				}
			}
			if found == nil {
				t.Fatalf("%s waits for no host-port check on %s.\n"+
					"Its readiness is then a signal from INSIDE the container, while every "+
					"L3a test dials the port from the host — the gap NIM-469's run 1 died "+
					"in (`keeper init: pg ping: … connection refused`). "+
					"Add wait.ForListeningPort(%q) to the set.",
					stand.name, stand.port, stand.port)
			}
			// " to be listening" (both checks) and " to be accessible externally"
			// (external only) are the two descriptions that include the host dial;
			// " to be listening internally" and " to be mapped" are the two that
			// do not.
			if desc := found.String(); !strings.HasSuffix(desc, " to be listening") &&
				!strings.HasSuffix(desc, " to be accessible externally") {
				t.Fatalf("%s has a host-port strategy that never dials the port from the "+
					"host: %q.\nThat waits for the mapping to exist, which happens before "+
					"anything accepts on it — the same gap as having no port check at all.",
					stand.name, desc)
			}
		})
	}
}

// TestStandReadinessBudgetIsExplicit — no stand check inherits a library
// default.
//
// This is the redis half. Its budget was 10 s that nobody in this repo chose:
// the module default had to cover docker-daemon round-trips under load and could
// not, and because it was inherited rather than written down, its existence was
// not visible from any file here. A check with no timeout of its own silently
// takes testcontainers' 60 s, which is the same failure with a different number.
func TestStandReadinessBudgetIsExplicit(t *testing.T) {
	for _, stand := range stands {
		t.Run(stand.name, func(t *testing.T) {
			for _, s := range flatten(stand.strategy()) {
				timeouter, ok := s.(wait.StrategyTimeout)
				if !ok {
					t.Fatalf("%s: strategy %T cannot carry a timeout", stand.name, s)
				}
				got := timeouter.Timeout()
				if got == nil {
					t.Fatalf("%s: strategy %q has no timeout of its own and inherits "+
						"testcontainers' default. Give it standReadyTimeout so the budget "+
						"is one number this repo chose.", stand.name, s)
				}
				if *got != standReadyTimeout {
					t.Errorf("%s: strategy %q has timeout %v, want standReadyTimeout (%v). "+
						"Per-stand budgets are what let the shortest one expire first.",
						stand.name, s, *got, standReadyTimeout)
				}
			}
		})
	}
}

// TestStandBudgetIsSharedNotSummed — the whole set is bounded, not just its
// parts.
//
// wait.MultiStrategy runs its checks sequentially, so a set of two 2-minute
// checks with no deadline of its own bounds one container at 4 minutes. Every
// number in waitstrategy.go — and standBringUpTimeout, derived from them —
// assumes 2. Dropping .WithDeadline() leaves both guards above green because the
// leaves keep their timeouts, which is exactly why this one exists separately.
func TestStandBudgetIsSharedNotSummed(t *testing.T) {
	for _, stand := range stands {
		t.Run(stand.name, func(t *testing.T) {
			got := wholeSetDeadline(t, stand.strategy())
			if got != standReadyTimeout {
				t.Errorf("%s bounds its whole check set at %v, want standReadyTimeout (%v). "+
					"Its checks run one after another, so this is the number that decides "+
					"how long the container really gets.", stand.name, got, standReadyTimeout)
			}
		})
	}
}

// wholeSetDeadline reads the deadline a MultiStrategy applies to the whole set.
//
// It has to use reflection. The field is unexported, and MultiStrategy.Timeout()
// does NOT return it — that reports the separate `timeout` field, which these
// strategies never set, so it reads nil here and says nothing. There is no
// public way to ask.
//
// If a testcontainers upgrade renames or removes the field this fails loudly,
// and that is the right outcome rather than an annoyance: the arithmetic in
// waitstrategy.go is an assumption about how this library spends a budget, and
// an upgrade is precisely when the assumption needs re-checking.
func wholeSetDeadline(t *testing.T, s wait.Strategy) time.Duration {
	t.Helper()
	multi, ok := s.(*wait.MultiStrategy)
	if !ok {
		t.Fatalf("strategy is %T, not a *wait.MultiStrategy — nothing bounds the set as a whole", s)
	}
	f := reflect.ValueOf(multi).Elem().FieldByName("deadline")
	if !f.IsValid() {
		t.Fatalf("wait.MultiStrategy has no `deadline` field any more. The budgets in " +
			"waitstrategy.go assume the set is bounded as a whole; re-read wait/all.go " +
			"and re-derive them before deleting this check.")
	}
	if f.IsNil() {
		t.Fatalf("the check set carries no deadline, so its checks each get their own " +
			"budget in turn. Add .WithDeadline(standReadyTimeout) to the wait.ForAll.")
	}
	return time.Duration(f.Elem().Int())
}

// TestStandBringUpBoundCoversItsParts — the outer ctx is not smaller than the
// sum of the budgets inside it.
//
// NewStack handed the three containers a flat 5 min ctx while their budgets
// summed to under 2, so the cap was never load-bearing and nothing pointed at
// it. Raising the parts without moving the whole makes the outer bound the real
// one, in silence, and the container last in line pays for the ones before it.
func TestStandBringUpBoundCoversItsParts(t *testing.T) {
	if len(stands) != standCount {
		t.Fatalf("standCount is %d but the table lists %d stands. standBringUpTimeout is "+
			"computed from standCount, so a stand added here without moving it gets its "+
			"budget from whatever the others left.", standCount, len(stands))
	}
	if min := standCount * standReadyTimeout; standBringUpTimeout < min {
		t.Fatalf("standBringUpTimeout is %v, under the %v its own contents can ask for "+
			"(%d × %v). The stands come up in sequence, so the last one silently gets the "+
			"remainder instead of its own budget, and fails as a parent-context deadline "+
			"without naming itself.", standBringUpTimeout, min, standCount, standReadyTimeout)
	}
}

// TestStandBringUpBoundIsTheOneApplied — EVERY place that raises the stands
// bounds them by the derived budget.
//
// The test above checks the arithmetic of a constant. It says nothing about
// whether anyone uses it, and that gap is not theoretical: the defect here WAS a
// literal at the application site — a flat `5 * time.Minute` that no file
// mentioning the per-container budgets ever referred to. Restoring that literal
// is a one-line edit which leaves every other guard in this file green, and that
// would make them decorative for the one mistake they were written about.
//
// Asked as a for-all over the raising sites, not as "does anybody use the
// constant". That distinction is not pedantry — the first cut of this guard was
// the existential one, it passed, and multikeeper.go was at that moment still
// handing the same three containers a flat 5 min. One site had been fixed and
// the guard reported the property as held, which is the failure mode a guard is
// supposed to be immune to. A budget that applies at one of two entry points is
// not a budget.
//
// A raising site is a function that calls startPostgres/startRedis/startVault;
// the bound has to be established in that same function, so the number is
// visible where the containers are. The declaring file is excluded so the
// constant's own definition cannot satisfy this, and a zero count is a failure:
// if the starters are ever renamed this must go red rather than pass vacuously.
func TestStandBringUpBoundIsTheOneApplied(t *testing.T) {
	starters := map[string]bool{"startPostgres": true, "startRedis": true, "startVault": true}
	sites := 0
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		if base == "waitstrategy.go" {
			return
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			raises, bound := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if starters[sel.Sel.Name] {
					raises = true
				}
				if sel.Sel.Name != "WithTimeout" || len(call.Args) != 2 {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "context" {
					return true
				}
				if arg, ok := call.Args[1].(*ast.Ident); ok && arg.Name == "standBringUpTimeout" {
					bound = true
				}
				return true
			})
			if !raises {
				continue
			}
			sites++
			if !bound {
				t.Errorf("%s: %s raises the stands but does not bound them with "+
					"context.WithTimeout(_, standBringUpTimeout). Its ctx is then some other "+
					"number, and every budget derived from standReadyTimeout is capped by a "+
					"figure nothing compares them against — the stands come up in sequence, so "+
					"the last one silently gets the remainder and fails as `context deadline "+
					"exceeded` from the parent, without naming itself.",
					fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
	})
	if sites == 0 {
		t.Fatalf("no function outside waitstrategy.go calls startPostgres/startRedis/startVault. "+
			"Either the stands are raised somewhere this cannot see or the starters were "+
			"renamed; either way %v no longer describes anything and this guard is checking "+
			"an empty set.", starters)
	}
}

// TestNoBareWaitStrategyOption — the stands' deadlines are not silently replaced
// by the library's 60 s.
//
// testcontainers.WithWaitStrategy(s...) is WithWaitStrategyAndDeadline(60s, s...)
// verbatim (options.go), and WithAdditionalWaitStrategy is the same. Either one
// re-wraps the strategy in a fresh wait.ForAll with a 60-second deadline, which
// overrides the .WithDeadline(standReadyTimeout) the constructors set.
//
// So the edit that undoes half of this ticket is deleting one word: postgres and
// redis go back to an effective 60 s, TestStandBudgetIsSharedNotSummed keeps
// passing because it inspects the CONSTRUCTOR rather than what the container
// received, and nothing else notices either. stack.go warns about this in prose;
// NIM-469 is about prose not holding.
func TestNoBareWaitStrategyOption(t *testing.T) {
	banned := map[string]bool{"WithWaitStrategy": true, "WithAdditionalWaitStrategy": true}
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !banned[sel.Sel.Name] {
				return true
			}
			t.Errorf("%s:%d: %s() takes the library's hard-coded 60 s deadline and wraps "+
				"the strategy in it, discarding the standReadyTimeout (%v) the constructor "+
				"set. Use %sAndDeadline(standReadyTimeout, …), or set ContainerRequest."+
				"WaitingFor directly.",
				base, fset.Position(call.Pos()).Line, sel.Sel.Name, standReadyTimeout, sel.Sel.Name)
			return true
		})
	})
}

// TestStandStrategiesAreWiredIn — the strategies guarded above are the ones the
// containers actually get.
//
// Everything else in this file tests the constructors. None of it notices if
// startVault stops calling vaultWaitStrategy() and inlines wait.ForLog("Root
// Token:") again — the constructor would still be correct, still be guarded, and
// entirely unused. Each constructor's own file is excluded so that its
// definition cannot count as its use.
func TestStandStrategiesAreWiredIn(t *testing.T) {
	used := map[string]bool{}
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		if base == "waitstrategy.go" {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok {
				used[id.Name] = true
			}
			return true
		})
	})
	for _, stand := range stands {
		name := funcNameOf(stand.strategy)
		if !used[name] {
			t.Errorf("%s() is never called outside waitstrategy.go, so whatever the %s "+
				"container actually waits for is not what this file guards. Every check "+
				"above would stay green with the container back on a log-only wait.",
				name, stand.name)
		}
	}
}

// funcNameOf recovers a constructor's identifier from the table, so the guard
// above names the function that is missing rather than a string someone kept in
// step by hand.
func funcNameOf(fn func() wait.Strategy) string {
	full := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
	if i := strings.LastIndex(full, "."); i >= 0 {
		full = full[i+1:]
	}
	return strings.TrimSuffix(full, "-fm")
}

// forEachHarnessFile parses every non-test source in this package and hands each
// to fn.
//
// Mode 0: build tags are not evaluated, which is what we want — the question is
// what the source says, and a file excluded by a tag can still be the one that
// starts a container in the configuration that matters.
func forEachHarnessFile(t *testing.T, fn func(base string, file *ast.File, fset *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse harness sources: %v", err)
	}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			fn(filepath.Base(path), file, fset)
		}
	}
}
