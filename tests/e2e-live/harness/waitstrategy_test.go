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
// It exists because the postgres half of NIM-406 was not a subtle bug: the
// container's entire wait was testcontainers.WithWaitStrategy(ForLog×2), with no
// host-port check anywhere under it, and nothing went red. The suite kept passing
// on an idle machine for as long as anyone looked at it, and the cost came due
// months later as four unreproducible gate failures that read like regressions.
//
// So the properties are pinned where breaking them is loud. Docker-free on
// purpose — this has to be checkable by the gate everyone runs, not by the
// 20-minute one that would be reporting the lie.

// stands enumerates the dependency containers Stack raises for itself, so a
// fourth cannot be added with a log-only wait and go unnoticed: the table is the
// checklist. Keep it in step with Stack.start* in stack.go.
//
// It is deliberately not "every container the harness starts". The soul
// container (container.go) is out of scope by construction rather than by
// oversight: it declares no ExposedPorts and nothing ever dials it from the
// host — the harness talks to it with docker exec, and soul→keeper is outbound —
// so wait.ForExec on an in-container signal is the property that fits it. These
// three are the ones reached through a mapped port, which is the hop that fails.
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
					"Its readiness is then a signal from INSIDE the container, and the "+
					"harness dials the port from the host — the gap NIM-406 was four "+
					"gate failures of. Add wait.ForListeningPort(%q) to the set.",
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
// The redis half of NIM-406 was a 10-second budget that nobody in this repo
// chose: the module default had to cover docker-daemon round-trips under load
// and could not, and because it was inherited rather than written down, its
// existence was not obvious from any file here. A check with no timeout of its
// own silently takes testcontainers' 60 s, which is the same failure with a
// different number.
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
// number in waitstrategy.go — and standBringUpTimeout, which is derived from
// them — assumes 2. Dropping .WithDeadline() leaves both guards above green
// because the leaves keep their timeouts, which is exactly why this one exists
// separately.
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
// This is NIM-406's own shape, and the first cut of NIM-406's fix walked into
// it: per-container budgets raised to 2 min summed to 6, inside a 5 min ctx in
// NewStack that nobody had compared them against. The smaller bound wins in
// silence, and the container last in line pays for the ones before it.
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

// TestStandBringUpBoundIsTheOneApplied — the derived budget is the one NewStack
// hands the containers.
//
// TestStandBringUpBoundCoversItsParts checks the arithmetic of a constant. It
// says nothing about whether anyone uses it, and that gap is not theoretical:
// NIM-406's original defect WAS a literal at the application site — a flat
// `5 * time.Minute` ctx that no file mentioning the per-container budgets ever
// referred to. Restoring that literal is a one-line edit that leaves every other
// guard in this file green, which makes those guards decorative for the one
// mistake they were written about.
//
// The declaring file is excluded so that the constant's own definition cannot
// satisfy this, the same way TestStandStrategiesAreWiredIn excludes the home of
// each constructor.
func TestStandBringUpBoundIsTheOneApplied(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse harness sources: %v", err)
	}

	applied := false
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			if filepath.Base(path) == "waitstrategy.go" {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "WithTimeout" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "context" {
					return true
				}
				if arg, ok := call.Args[1].(*ast.Ident); ok && arg.Name == "standBringUpTimeout" {
					applied = true
				}
				return true
			})
		}
	}
	if !applied {
		t.Fatalf("no context.WithTimeout(_, standBringUpTimeout) outside waitstrategy.go. " +
			"The stands' shared ctx is then some other number, and every budget derived " +
			"from standReadyTimeout is bounded by a figure nothing here compares them " +
			"against — the outer-bound trap NIM-406 is a case of, one level up.")
	}
}

// TestNoBareWaitStrategyOption — the stands' deadlines are not silently replaced
// by the library's 60 s.
//
// testcontainers.WithWaitStrategy(s...) is WithWaitStrategyAndDeadline(60s, s...)
// verbatim (options.go), and WithAdditionalWaitStrategy is the same. Either one
// re-wraps the strategy in a fresh wait.ForAll with a 60-second deadline, which
// overrides the .WithDeadline(standReadyTimeout) the constructors set. So the
// edit that undoes half of NIM-406 is deleting one word: postgres and redis go
// back to an effective 60 s, TestStandBudgetIsSharedNotSummed keeps passing
// because it inspects the CONSTRUCTOR rather than what the container received,
// and nothing else notices either.
//
// stack.go warns about this in prose. NIM-406 is about prose not holding.
func TestNoBareWaitStrategyOption(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse harness sources: %v", err)
	}

	banned := map[string]bool{"WithWaitStrategy": true, "WithAdditionalWaitStrategy": true}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			base := filepath.Base(path)
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
		}
	}
}

// TestStandStrategiesAreWiredIn — the strategies guarded above are the ones the
// containers actually get.
//
// Everything else in this file tests the constructors. None of it notices if
// startVault stops calling vaultWaitStrategy() and inlines wait.ForLog("Root
// Token:") again — the constructor would still be correct, still be guarded, and
// no longer be used. A guard that can be stepped around by deleting one call is
// decorative.
//
// The names come from the table via runtime rather than being written out, so a
// rename cannot leave this checking for a function nobody has; and the call site
// must be in a file other than the one declaring it, which is the difference
// between "wired into a container" and "wrapped by another helper here".
func TestStandStrategiesAreWiredIn(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse harness sources: %v", err)
	}

	// Build-tagged files parse fine — go/parser does not evaluate constraints —
	// so stack.go is in here even though this test binary is built without
	// e2e_live.
	declaredIn := map[string]string{}
	calledIn := map[string][]string{}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			name := filepath.Base(path)
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.FuncDecl:
					if node.Recv == nil {
						declaredIn[node.Name.Name] = name
					}
				case *ast.CallExpr:
					if id, ok := node.Fun.(*ast.Ident); ok {
						calledIn[id.Name] = append(calledIn[id.Name], name)
					}
				}
				return true
			})
		}
	}

	for _, stand := range stands {
		full := runtime.FuncForPC(reflect.ValueOf(stand.strategy).Pointer()).Name()
		fn := full[strings.LastIndex(full, ".")+1:]
		t.Run(stand.name, func(t *testing.T) {
			home, ok := declaredIn[fn]
			if !ok {
				t.Fatalf("%s: no declaration of %s found in the package sources", stand.name, fn)
			}
			for _, site := range calledIn[fn] {
				if site != home {
					return
				}
			}
			t.Fatalf("%s: nothing outside %s calls %s. The stand is then waiting on "+
				"whatever its start function spells out inline, and every check in this "+
				"file is guarding a function no container receives.", stand.name, home, fn)
		})
	}
}
