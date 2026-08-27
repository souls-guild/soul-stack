//go:build e2e

package harness

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
//
// NIM-547: the table/constant agreement that used to live here has moved to
// TestStandTableIsTheContainersActuallyRaised. It was checking `len(stands) ==
// standCount` — a table against a constant, with nothing outside the test file
// in the comparison — and a mutation that dropped a stand row and edited the
// constant to match passed it. Two consistent edits to two adjacent lines is a
// rename, not an attack.
func TestStandBringUpBoundCoversItsParts(t *testing.T) {
	if min := standCount * standBringUpAttempts * standReadyTimeout; standBringUpTimeout < min {
		t.Fatalf("standBringUpTimeout is %v, under the %v its own contents can ask for "+
			"(%d stands × %d attempts × %v). The stands come up in sequence, so the last one "+
			"silently gets the remainder instead of its own budget, and fails as a "+
			"parent-context deadline without naming itself.",
			standBringUpTimeout, min, standCount, standBringUpAttempts, standReadyTimeout)
	}
}

// TestStandBudgetsHaveAnAbsoluteFloor — the numbers are anchored to something
// outside themselves.
//
// Everything else about the budgets is relative: the per-check timeouts are
// compared against standReadyTimeout, and standBringUpTimeout is derived from
// it. Set standReadyTimeout to ten seconds and the whole construction stays
// perfectly self-consistent — every assertion still passes, and L3a is back to
// the inherited-default regime this file was written to end. A mutation did
// exactly that and survived. Relative arithmetic needs one absolute anchor, or
// it is arithmetic about nothing.
//
// The floor is not a preference. NIM-349 measured a vault container missing a
// 45-second budget at loadavg ~12, and L3a raises forty stands back to back on a
// box that routinely has another session's suite on it. 90 s is above every
// bring-up anyone here has measured and below the 2 min actually set, so it
// fails on a regression toward the old regime and not on a deliberate revision.
//
// The ceiling is read out of the Makefile rather than written down, so it tracks
// the consumer instead of a copy of it: the per-test cap has to leave room for
// the suite it lives in, and `go test -timeout` is what actually kills the run.
func TestStandBudgetsHaveAnAbsoluteFloor(t *testing.T) {
	const floor = 90 * time.Second
	if standReadyTimeout < floor {
		t.Fatalf("standReadyTimeout is %v, under the %v floor. Every other budget check in "+
			"this file compares against this constant or derives from it, so lowering it "+
			"leaves them all green while restoring the regime NIM-469 was about: a container "+
			"budget too small for a loaded docker daemon, failing as an unattributable red.",
			standReadyTimeout, floor)
	}

	suite := suiteTimeoutFromMakefile(t)
	if standBringUpTimeout >= suite {
		t.Fatalf("standBringUpTimeout is %v but `make e2e` gives the WHOLE suite %v. One "+
			"stand's bring-up would then outlast the run that contains it, and the suite "+
			"dies as a panic-on-timeout rather than as a named stand failure — the tests "+
			"after it report nothing at all.", standBringUpTimeout, suite)
	}
}

// suiteTimeoutFromMakefile reads the L3a suite budget from the target that
// actually runs it.
//
// Read rather than copied, for the reason setupdecl.go's marker is read out of
// classify-l1-failure.py: a number duplicated into a test is a number that stops
// matching the moment someone edits the original, and the test keeps passing
// against its own copy. If the target is reshaped so this cannot find the flag,
// that is a failure and not something to skip past — the relationship being
// guarded is exactly "these two numbers are about the same run".
func suiteTimeoutFromMakefile(t *testing.T) time.Duration {
	t.Helper()
	const makefile = "../../../Makefile"
	src, err := os.ReadFile(makefile)
	if err != nil {
		t.Fatalf("read %s: %v", makefile, err)
	}
	// `e2e\s`, not `e2e`: the same Makefile runs the L3b tier as
	// `go test -tags=e2e_live … -timeout=30m`, and `-tags=e2e` is a prefix of it.
	// Without the boundary, deleting -timeout from the L3a line — the exact
	// regression this guard exists to catch — makes the pattern fall through to
	// the e2e_live line, read ITS 30m, and pass while L3a runs on go test's
	// silent 10 min default.
	m := regexp.MustCompile(`go test -tags=e2e\s[^\n]*-timeout=(\S+?)\s`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s no longer runs the e2e tier with an explicit -timeout. Either the tier "+
			"is now on go test's silent 10 min default — under which a loaded bring-up is "+
			"killed mid-stand with no verdict — or the target moved and this guard is "+
			"comparing standBringUpTimeout against nothing.", makefile)
	}
	d, err := time.ParseDuration(string(m[1]))
	if err != nil {
		t.Fatalf("%s: -timeout=%s is not a duration: %v", makefile, m[1], err)
	}
	return d
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
// A raising site is a function that brings the stands up; the bound has to be
// established in that same function, so the number is visible where the
// containers are. The declaring file is excluded so the constant's own
// definition cannot satisfy this, and a zero count is a failure: if the entry
// points are ever reshaped this must go red rather than pass vacuously.
//
// NIM-547 changed two things here, both because a mutation walked through the
// old version.
//
// The set is now DERIVED. It used to be the literal
// {startPostgres, startRedis, startVault}, and refactoring the call sites — this
// ticket's own change, which routes each starter through bringUpStand — emptied
// it. The `sites == 0` backstop is what caught that, which is the entire reason
// to write one; without it the guard would have gone green over nothing.
//
// And EVERY context.WithTimeout in a raising function must be the derived bound,
// not merely one of them. The old check set `bound` on the first match and
// stopped caring, so adding a second, narrower ctx below it — the shape a
// "let's not wait so long for this bit" edit takes — left the guard green while
// the narrower one governed.
func TestStandBringUpBoundIsTheOneApplied(t *testing.T) {
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
			var others []token.Position
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == bringUpStandName {
					raises = true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == bringUpStandName {
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
					return true
				}
				// Recorded, not reported: whether a second budget in this
				// function is a defect depends on whether the function raises
				// stands at all, and the walk does not know that yet. Most of
				// this package is assertion helpers with their own short ctx,
				// and they are none of this guard's business.
				others = append(others, fset.Position(call.Pos()))
				return true
			})
			if !raises {
				continue
			}
			sites++
			for _, pos := range others {
				t.Errorf("%s: %s raises the stands and also opens a context.WithTimeout that "+
					"is not standBringUpTimeout. Whichever of the two is smaller governs, and "+
					"the derived budget stops being the budget — silently, because every "+
					"arithmetic check in this file still passes.", pos, fn.Name.Name)
			}
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
		t.Fatalf("no function outside waitstrategy.go calls %s(). Either the stands are now "+
			"raised somewhere this cannot see, or the entry point was renamed; either way "+
			"this guard is checking an empty set.", bringUpStandName)
	}
}

// bringUpStandName — the single door the stands come up through (daemonhealth.go).
// Named once here so the guards below agree on it and a rename fails loudly in
// one place rather than emptying three sets quietly.
const bringUpStandName = "bringUpStand"

// containerRaiser — one method of this package that hands a container request to
// testcontainers, discovered in the sources rather than listed.
type containerRaiser struct {
	fn  string
	pos token.Position
}

// containerRaisers answers "what raises a container here?" by reading the code
// that does it.
//
// This is the fix for the shape that let six mutations through. Every guard in
// this file used to start from a literal — a table, a name map, a constant — and
// a literal describes today's names. Drop a stand and its table row together, or
// add a fourth container in a file the table does not mention, and the guards
// have nothing to say: they were never looking at the containers, only at the
// list.
//
// The subject here is "a call to a testcontainers constructor", which is what
// raising a container IS. Import aliases are resolved per file, so a new stand
// added under any alias, in any file of this package, is found; and the
// constructor names are the three the library exposes for it.
func containerRaisers(t *testing.T) []containerRaiser {
	t.Helper()
	ctors := map[string]bool{"Run": true, "RunContainer": true, "GenericContainer": true}
	var found []containerRaiser
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		tc := map[string]bool{}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !strings.Contains(path, "testcontainers-go") {
				continue
			}
			if imp.Name != nil {
				tc[imp.Name.Name] = true
				continue
			}
			// An unaliased import is referred to by its PACKAGE name, which the
			// AST does not carry — and Go does not require it to match the
			// directory. It does not here: .../testcontainers-go is package
			// `testcontainers`. Both spellings go in the set, since a wrong guess
			// makes this guard silently find fewer raisers, and a guard that
			// under-counts is the failure mode this whole rewrite is about. The
			// count checks in the two tests below are what catch it if it ever
			// guesses wrong again.
			dir := path[strings.LastIndex(path, "/")+1:]
			tc[dir] = true
			tc[strings.TrimSuffix(dir, "-go")] = true
		}
		if len(tc) == 0 {
			return
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !ctors[sel.Sel.Name] {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && tc[pkg.Name] {
					found = append(found, containerRaiser{fn: fn.Name.Name, pos: fset.Position(fn.Pos())})
					return false
				}
				return true
			})
		}
	})
	return found
}

// TestStandTableIsTheContainersActuallyRaised — the checklist is the containers,
// not a list of them.
//
// `stands` drives every readiness assertion in this file, and it used to be
// checked only against standCount — a table against a constant, both in reach of
// one edit. A mutation deleted the vault row and changed 3 to 2, and the file
// went green while vault's readiness was guarded by nothing. The symmetric
// mutation, adding a fourth container in a file the table does not list, was
// invisible for the same reason.
//
// So the comparison is now against the sources: exactly one table row, and one
// unit of standCount, per method that calls a testcontainers constructor.
func TestStandTableIsTheContainersActuallyRaised(t *testing.T) {
	raisers := containerRaisers(t)
	if len(raisers) == 0 {
		t.Fatalf("no method in this package calls a testcontainers constructor. Either the " +
			"stands moved out of this package or the library's entry points were renamed; " +
			"either way every guard in this file is now comparing the table against nothing.")
	}

	byStand := map[string]containerRaiser{}
	for _, r := range raisers {
		byStand[standNameOf(r.fn)] = r
	}
	if len(byStand) != len(raisers) {
		t.Fatalf("two container raisers reduce to the same stand name: %v. The name is what a "+
			"failed bring-up is reported under, so two stands sharing one would make the "+
			"diagnosis ambiguous exactly when it matters.", raisers)
	}

	for _, stand := range stands {
		if _, ok := byStand[stand.name]; !ok {
			t.Errorf("the table lists a %q stand, but no method in this package raises a "+
				"container for it. Either it is gone and its readiness checks above are "+
				"testing a constructor nobody calls, or it is raised under a name this "+
				"cannot derive.", stand.name)
		}
		delete(byStand, stand.name)
	}
	for name, r := range byStand {
		t.Errorf("%s raises a container the table does not list (stand %q). Every readiness "+
			"property in this file is asserted per table row, so this container waits for "+
			"whatever its author wrote and no guard here has an opinion about it.",
			r.pos, name)
	}

	if standCount != len(raisers) {
		t.Errorf("standCount is %d but %d methods raise containers. standBringUpTimeout is "+
			"computed from standCount, so the stands share a budget sized for a different "+
			"number of them, and the last one in sequence gets the remainder.",
			standCount, len(raisers))
	}
}

// TestEveryStandComesUpThroughBringUpStand — no container is raised on a path
// that cannot say which layer failed.
//
// bringUpStand is what turns testcontainers' `get state: … context deadline
// exceeded` into a statement about the daemon or about the container
// (daemonhealth.go, NIM-533). Calling a starter directly still compiles, still
// works on an idle machine, and gives back exactly the unattributable red this
// tier spent NIM-469 and this ticket getting rid of — so "went through the door"
// has to be a property, not a convention.
//
// Both halves are checked, and they fail differently. A raiser missing from the
// bringUpStand calls is a stand with no diagnosis; a raiser invoked directly by
// name anywhere is a second path around the door. And the stand name passed for
// the diagnosis must be the one derived from the raiser: a call that labels
// startRedis "postgres" produces a confident report about the wrong container,
// which is worse than the error it replaced.
func TestEveryStandComesUpThroughBringUpStand(t *testing.T) {
	raisers := containerRaisers(t)
	if len(raisers) == 0 {
		t.Fatalf("no method in this package calls a testcontainers constructor — this guard " +
			"has nothing to check. See TestStandTableIsTheContainersActuallyRaised.")
	}
	isRaiser := map[string]bool{}
	for _, r := range raisers {
		isRaiser[r.fn] = true
	}

	routed := map[string]bool{}
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)

			// A raiser invoked by name is a way around the door.
			if isSel && isRaiser[sel.Sel.Name] {
				t.Errorf("%s: %s is called directly. Every stand has to come up through %s(), "+
					"which is what reports whether the daemon or the container was the thing "+
					"that failed; a direct call brings the container up fine and leaves a "+
					"failure indistinguishable from a regression in the code under test.",
					fset.Position(call.Pos()), sel.Sel.Name, bringUpStandName)
				return true
			}

			door := (isSel && sel.Sel.Name == bringUpStandName)
			if id, isID := call.Fun.(*ast.Ident); !door && !(isID && id.Name == bringUpStandName) {
				return true
			} else if isID {
				door = true
			}
			if !door || len(call.Args) != 3 {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: %s() is called with a non-literal stand name. The name is what "+
					"the failure is reported under; computing it means no guard can check "+
					"that the report names the container it is about.",
					fset.Position(call.Pos()), bringUpStandName)
				return true
			}
			passed, ok := call.Args[2].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			label := strings.Trim(lit.Value, `"`)
			if want := standNameOf(passed.Sel.Name); label != want {
				t.Errorf("%s: %s() raises %s under the name %q, but that method's stand is %q. "+
					"A bring-up failure would then be reported against the wrong container, "+
					"with the daemon evidence of a different one attached.",
					fset.Position(call.Pos()), bringUpStandName, passed.Sel.Name, label, want)
			}
			routed[passed.Sel.Name] = true
			return true
		})
	})

	for _, r := range raisers {
		if !routed[r.fn] {
			t.Errorf("%s: %s raises a container but is never passed to %s(). That stand's "+
				"failures arrive as the library's error text, which names the container it "+
				"was waiting on rather than the layer that failed — the exact red NIM-533 "+
				"is about.", r.pos, r.fn, bringUpStandName)
		}
	}
}

// standNameOf derives the stand name a raising method is about, so the table,
// the diagnosis label and the method cannot drift apart without something
// saying so.
func standNameOf(method string) string {
	return strings.ToLower(strings.TrimPrefix(method, "start"))
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
//
// NIM-547 widened it from a name check to a value check. Banning the two names
// left the sanctioned spelling wide open: WithWaitStrategyAndDeadline(60 *
// time.Second, …) is the identical action under a permitted identifier, and a
// mutation that wrote it survived every guard in this file. The property was
// never "do not call that function" — it is "the deadline the container gets is
// the one this repo chose", so the deadline argument is what gets read.
func TestNoBareWaitStrategyOption(t *testing.T) {
	banned := map[string]bool{"WithWaitStrategy": true, "WithAdditionalWaitStrategy": true}
	checked := map[string]bool{
		"WithWaitStrategyAndDeadline":           true,
		"WithAdditionalWaitStrategyAndDeadline": true,
	}
	sites := 0
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case banned[sel.Sel.Name]:
				sites++
				t.Errorf("%s:%d: %s() takes the library's hard-coded 60 s deadline and wraps "+
					"the strategy in it, discarding the standReadyTimeout (%v) the constructor "+
					"set. Use %sAndDeadline(standReadyTimeout, …), or set ContainerRequest."+
					"WaitingFor directly.",
					base, fset.Position(call.Pos()).Line, sel.Sel.Name, standReadyTimeout, sel.Sel.Name)
			case checked[sel.Sel.Name]:
				sites++
				if len(call.Args) == 0 {
					return true
				}
				if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == "standReadyTimeout" {
					return true
				}
				t.Errorf("%s:%d: %s() is given a deadline that is not standReadyTimeout. The "+
					"function name is permitted, the effect is not: this is the same "+
					"re-wrapping the banned spellings do, and it discards the "+
					"standReadyTimeout (%v) the strategy constructor set.",
					base, fset.Position(call.Pos()).Line, sel.Sel.Name, standReadyTimeout)
			}
			return true
		})
	})
	if sites == 0 {
		t.Fatalf("no wait-strategy option is passed anywhere in this package. Either the "+
			"stands now set ContainerRequest.WaitingFor directly everywhere — in which case "+
			"delete this guard deliberately — or the library renamed these options and %v "+
			"describes nothing.", checked)
	}
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
