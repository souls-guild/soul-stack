//go:build e2e

package harness

import (
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// The declaration is what turns a red L3a from a list of identical `--- FAIL:`
// lines into a statement about which layer died, and it is also the only thing
// here capable of saying the wrong one. Both guards below are docker-free: they
// read the harness sources, so they hold even in an environment where the tier
// itself cannot run.

// TestShouldDeclareTruthTable — all eight inputs, stated rather than sampled.
//
// The rows with teeth are the two `failedBefore=true, infraUp=false` ones: the
// test was ALREADY red when NewStack was called, so its redness is not evidence
// about bring-up. t.Failed() cannot tell those apart on its own, because
// (*common).Fail propagates to the parent the moment a subtest fails.
func TestShouldDeclareTruthTable(t *testing.T) {
	cases := []struct {
		infraUp, failedBefore, failedNow bool
		want                             bool
		why                              string
	}{
		{false, false, true, true, "the region turned a clean test red — the only case that is a stand-setup failure"},
		{false, false, false, false, "nothing failed; the keeper-binary t.Skipf leaves the region early and asserts nothing"},
		{false, true, true, false, "already red on entry: declaring would stamp an infra label over a finding"},
		{false, true, false, false, "cannot un-fail; unreachable in practice, and still not ours to declare"},
		{true, false, true, false, "infrastructure was up, so whatever failed after it is the code"},
		{true, false, false, false, "the happy path"},
		{true, true, true, false, "already red, and past the infrastructure besides"},
		{true, true, false, false, "already red, and nothing failed here"},
	}
	for _, c := range cases {
		got := shouldDeclare(c.infraUp, c.failedBefore, c.failedNow)
		if got != c.want {
			t.Errorf("shouldDeclare(infraUp=%v, failedBefore=%v, failedNow=%v) = %v, want %v — %s",
				c.infraUp, c.failedBefore, c.failedNow, got, c.want, c.why)
		}
	}
}

// A product entry point is a call inside the harness that runs code THIS REPO
// builds, as opposed to docker, Vault or the filesystem.
//
// Each must sit outside the declared bring-up region, because a failure in one
// of them is a finding. On this tier the stakes are the whole suite rather than
// one test: every L3a test starts with NewStack, so a region that swallowed
// `keeper init` would print STAND-SETUP on all forty of them at once, and a
// suite red in nothing but STAND-SETUP reads exactly like a bad day for docker.
//
// The set used to be written out by hand, and that was the defect NIM-547 names:
// a listed name that stops existing takes its teeth with it, a listed name that
// nothing calls gates zero while looking like coverage, and a NEW way to reach
// the product is simply absent. None of the three announces itself — the region
// check just stops matching and reports success. So the set is derived from the
// sources instead, through the doors below.

// productDoor — one way harness code reaches the product, and the identifier
// that gives it away.
//
// There are three, and they are three different subjects: the binary this repo
// builds (exec), the operator API it serves (HTTP), and the schema `keeper
// init` migrated (SQL). Going through any of them is the definition of "runs
// the product" — which means a helper written next year is covered on the day
// it is written, without anyone remembering a list exists.
type productDoor struct {
	key     string
	why     string
	matches func(*ast.BlockStmt) bool
}

var productDoors = []productDoor{
	{
		key:     "keeperBinaryPath",
		why:     "execs the binary this repo builds",
		matches: func(b *ast.BlockStmt) bool { return callsIdent(b, "keeperBinaryPath") },
	},
	{
		key:     "opClient",
		why:     "calls the keeper's operator API over HTTP",
		matches: func(b *ast.BlockStmt) bool { return callsIdent(b, "opClient") },
	},
	{
		key:     "db",
		why:     "runs SQL against the schema `keeper init` migrated",
		matches: func(b *ast.BlockStmt) bool { return callsThroughField(b, "db") },
	},
}

// productEntryPoints — every harness function that goes through one of the
// doors, re-derived on each run.
//
// Fatal, not skip, when a door finds nothing: a derivation that comes back
// empty makes every guard built on it pass by having nothing to check, which is
// the exact failure mode this replaced a hand-written list to escape.
func productEntryPoints(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	perDoor := map[string]int{}
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, d := range productDoors {
				if d.matches(fn.Body) {
					perDoor[d.key]++
					found[fn.Name.Name] = d.why
				}
			}
		}
	})
	for _, d := range productDoors {
		if perDoor[d.key] == 0 {
			t.Fatalf("no function in the harness goes through the %q door (%s). Either the harness "+
				"reaches the product some other way now — in which case this derivation is looking "+
				"for a door that is no longer there, and the region check below is running against "+
				"a set with a hole in it — or the identifier was renamed.", d.key, d.why)
		}
	}
	return found
}

// callsThroughField — a method call ON `.<field>`, e.g. `s.db.Exec(...)`.
//
// Not "mentions .db": NewStack and NewMultiKeeperStack ASSIGN `s.db` inside the
// declared region, which is the connection coming up and is infrastructure by
// any reading. Matching the mention would derive the two entry points as their
// own product calls.
func callsThroughField(body *ast.BlockStmt, field string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return !found
		}
		if recv, ok := method.X.(*ast.SelectorExpr); ok && recv.Sel.Name == field {
			found = true
		}
		return !found
	})
	return found
}

// knownProductCalls — a floor under the derivation, NOT its definition.
//
// One per shape the harness was known to use when NIM-547 was written. If a
// door narrows — a helper renamed, a client wrapped one level deeper, SQL moved
// behind an accessor — the derived set silently shrinks and every guard built on
// it goes on passing with less to check. This is what notices, and it names
// which shape went missing rather than reporting a count.
var knownProductCalls = map[string]string{
	"runKeeperInit":       "`keeper init` — ADR-013 bootstrap, schema migrations, the JWT signing key",
	"startKeeperRun":      "`keeper run` and the /readyz wait",
	"spawnKeeperProc":     "`keeper run` for one KID of the multi-keeper cluster",
	"assertOwnKeeper":     "an authenticated call proving the process on our port is ours (NIM-469)",
	"RegisterSoulPreAuth": "raw INSERTs into souls/soul_seeds — where a dropped column dies",
	"RegisterService":     "POST /v1/services over examples/ (NIM-211: examples are the subject, not scenery)",
}

// assertKeeperBinaryMatchesTree (NIM-490) is deliberately NOT here, and the
// omission is a claim rather than an oversight.
//
// It execs the keeper, which looks like door one, and it goes through none of
// them: it is handed a path rather than calling keeperBinaryPath. Left as is,
// that reads exactly like the silent shrink TestProductDoorsStayOpen exists to
// catch — so the reason is written down instead. `keeper version` reads the
// artifact's nameplate. It drives no behaviour this tier tests, and no
// regression in this repo can make it fail; what makes it fail is the binary
// being from another tree, which is a fact about the machine, which is what
// bring-up means. Listing it here would assert the opposite and force it below
// `infraUp`, where its refusal would reach the reader as forty unlabelled
// FAIL lines. See NewStack for the placement argument.

// TestDeclaredRegionsEndBeforeTheProductRuns — the bring-up declaration covers
// infrastructure only.
//
// This is not a hypothetical risk being pre-empted: it is the defect an
// independent validator found in NIM-406's first cut, where `infraUp` was set on
// the last line of the entry point and the declared region therefore swallowed
// every product call above it. The prose said "everything from here up is
// bring-up". Prose not holding is the thing this ticket is about.
func TestDeclaredRegionsEndBeforeTheProductRuns(t *testing.T) {
	entry := productEntryPoints(t)
	sites := declSites(t)
	for _, s := range sites {
		flag := regionFlagName(s.stmt)
		if flag == "" {
			t.Errorf("%s: %s passes something other than `&<flag>` as the third argument to "+
				"declareStandSetupFailure, so there is no variable whose assignment closes the "+
				"region. Everything the function does is then infrastructure by declaration.",
				s.file, s.fn.Name.Name)
			continue
		}
		checkRegion(t, s, flag, entry)
	}

	// Closed in the other direction: if the entry points stop deferring it at
	// all, everything above passes by finding nothing to check.
	if len(sites) < 2 {
		t.Fatalf("only %d function(s) defer declareStandSetupFailure. NewStack and "+
			"NewMultiKeeperStack both must, or their bring-up failures read as assertions "+
			"and a red suite is back to being forty indistinguishable FAIL lines.", len(sites))
	}
}

// TestEntrySnapshotIsReadAtRegistration — `failedBefore` is t.Failed(), passed
// as an argument.
//
// NIM-547. setupdecl.go states this in prose ("do not 'fix' it into a closure
// that re-reads it at exit"), and prose not holding is what this batch is about:
// nothing checked the ARGUMENTS, so `defer declareStandSetupFailure(t, false,
// &infraUp)` — a two-token edit, no syntax broken, every other guard still green
// — restores the exact case shouldDeclare's third boolean exists to prevent. A
// test that was already red on entry, calling a second stand or hitting the
// keeper-binary skip, gets STAND-SETUP stamped over its finding, and on this tier
// that reading propagates: a suite red in nothing but STAND-SETUP is read as a
// bad day for docker and rerun.
//
// The literal is the dangerous shape, not the only wrong one, so this insists on
// the right shape rather than banning the wrong one: a call to Failed(), which a
// literal, a variable holding an earlier snapshot, and a closure all fail.
//
// (The "keeper-binary skip" the motivating example names is now a Fatalf —
// NIM-533 — but the case is unchanged: an entry point reached by an
// already-red test is still the shape that stamps over a finding.)
func TestEntrySnapshotIsReadAtRegistration(t *testing.T) {
	sites := declSites(t)
	for _, s := range sites {
		if len(s.stmt.Call.Args) != 3 {
			t.Errorf("%s: %s calls declareStandSetupFailure with %d arguments, want 3 "+
				"(t, t.Failed(), &<flag>).", s.file, s.fn.Name.Name, len(s.stmt.Call.Args))
			continue
		}
		if !isFailedCall(s.stmt.Call.Args[1]) {
			t.Errorf("%s: %s passes %s as `failedBefore`. That argument is the ENTRY "+
				"SNAPSHOT — it is evaluated when the defer is registered, and it is the only "+
				"thing separating \"this region turned the test red\" from \"the test was "+
				"already red\". Anything but a t.Failed() call here makes the declaration "+
				"stamp an infra label over a finding that was recorded before the stand "+
				"was ever touched.",
				s.file, s.fn.Name.Name, exprText(s.stmt.Call.Args[1]))
		}
	}
	if len(sites) < 2 {
		t.Fatalf("found %d declaration site(s); NewStack and NewMultiKeeperStack both "+
			"declare. This guard is checking an empty set.", len(sites))
	}
}

// TestProductDoorsStayOpen — the derivation is checked against the shapes it is
// known to have to find.
//
// The derived set has one failure mode and it is silent: a door that stops
// matching returns fewer names, the region check goes on running against the
// remainder, and nothing anywhere says it is now checking less. productEntryPoints
// catches a door that matches NOTHING; this catches a door that matches LESS —
// SQL moved behind an accessor still leaves the four seeders matching `.db`, so
// the per-door count stays non-zero while RegisterSoulPreAuth quietly drops out.
//
// The floor is deliberately six names and deliberately not the definition. It
// asserts "these particular calls are still recognised as product calls", which
// a list of everything cannot say — that would only assert that the derivation
// equals itself.
//
// There is no count assertion here on purpose. `len(derived) > len(floor)` reads
// like a third layer, but no real edit reds it without also dropping a floor
// name, so it would report the same defect twice and nothing on its own — a
// claim that cannot be shown to fail, which is the shape this batch is about.
func TestProductDoorsStayOpen(t *testing.T) {
	entry := productEntryPoints(t)

	for name, what := range knownProductCalls {
		if _, ok := entry[name]; ok {
			continue
		}
		t.Errorf("%s (%s) is no longer derived as a product entry point. Either the function "+
			"was renamed or removed, or it stopped going through any of the doors — it now "+
			"reaches the product some way this derivation does not recognise. Until that is "+
			"fixed the region check cannot see it, so putting it inside a declared bring-up "+
			"region would print STAND-SETUP over a genuine regression and nothing would object.",
			name, what)
	}
}

// declSite — one `defer declareStandSetupFailure(...)` and the function it is
// in. The fset travels with it: a token.Pos is an offset into the FileSet that
// produced it and means nothing without one, and forEachHarnessFile makes a
// fresh one per call.
type declSite struct {
	file string
	fn   *ast.FuncDecl
	stmt *ast.DeferStmt
	fset *token.FileSet
}

// declSites collects every declaration site in the harness sources, so the two
// guards above ask their separate questions of the same set rather than each
// re-deriving it.
func declSites(t *testing.T) []declSite {
	t.Helper()
	var out []declSite
	forEachHarnessFile(t, func(base string, file *ast.File, fset *token.FileSet) {
		if base == "setupdecl.go" {
			return // its own declaration is the definition, not a use
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				d, ok := n.(*ast.DeferStmt)
				if !ok {
					return true
				}
				if id, ok := d.Call.Fun.(*ast.Ident); ok && id.Name == "declareStandSetupFailure" {
					out = append(out, declSite{file: base, fn: fn, stmt: d, fset: fset})
				}
				return true
			})
		}
	})
	return out
}

// regionFlagName — the variable whose `= true` closes the region, read out of
// the defer's own third argument rather than assumed to be called `infraUp`.
// Deriving it is what ties the two halves together: renaming the flag now moves
// the region boundary with it instead of leaving this guard looking for a name
// that no longer exists and reporting the absence as a defect.
func regionFlagName(d *ast.DeferStmt) string {
	if len(d.Call.Args) != 3 {
		return ""
	}
	u, ok := d.Call.Args[2].(*ast.UnaryExpr)
	if !ok || u.Op != token.AND {
		return ""
	}
	id, ok := u.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

func isFailedCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Failed"
}

// exprText renders an expression for an error message, so the report names what
// was actually written instead of "something else".
func exprText(e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, token.NewFileSet(), e); err != nil {
		return "<unprintable>"
	}
	return b.String()
}

// checkRegion: find where the region closes, then insist no product call falls
// between the defer and that point.
func checkRegion(t *testing.T, s declSite, flag string, entry map[string]string) {
	t.Helper()
	file, fn, fset, start := s.file, s.fn, s.fset, s.stmt.Pos()

	var closes []token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != flag {
			return true
		}
		if rhs, ok := as.Rhs[0].(*ast.Ident); ok && rhs.Name == "true" {
			closes = append(closes, as.Pos())
		}
		return true
	})
	if len(closes) == 0 {
		t.Errorf("%s: %s defers declareStandSetupFailure but never sets `%s = true`, so every "+
			"exit from it is declared a stand-setup failure — including a successful one in a test "+
			"that is red for its own reasons.", file, fn.Name.Name, flag)
		return
	}

	// One assignment, or the region is not a lexical fact and this guard is
	// guessing which one runs. It would guess the EARLIEST — every call below
	// the second `%s = true` silently stops being checked, and the guard reports
	// success on a region it shrank itself. Whether the runtime region is
	// actually shorter depends on a branch, which is precisely why it cannot be
	// decided here.
	if len(closes) > 1 {
		lines := make([]string, 0, len(closes))
		for _, p := range closes {
			lines = append(lines, fmt.Sprintf("%d", fset.Position(p).Line))
		}
		t.Errorf("%s: %s sets `%s = true` on %d lines (%s). The declared region has to end at "+
			"ONE place: with several, which one closes it depends on the branch taken, and this "+
			"check would silently take the first — leaving every product call below the others "+
			"unchecked while still reporting success. Set the flag once, at the point the "+
			"infrastructure is genuinely up.",
			file, fn.Name.Name, flag, len(closes), strings.Join(lines, ", "))
		return
	}
	end := closes[0]

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch f := call.Fun.(type) {
		case *ast.Ident:
			name = f.Name
		case *ast.SelectorExpr:
			name = f.Sel.Name
		}
		what, isProduct := entry[name]
		if !isProduct || call.Pos() < start || call.Pos() > end {
			return true
		}
		t.Errorf("%s:%d: %s calls %s (%s) INSIDE the declared bring-up region — the region runs from "+
			"line %d to line %d. A failure there would print STAND-SETUP under the words \"nothing "+
			"above is a finding about the code\", so a real regression would read as a bad day for "+
			"docker. Move `infraUp = true` above this call, or the call above the defer.",
			file, fset.Position(call.Pos()).Line, fn.Name.Name, name, what,
			fset.Position(start).Line, fset.Position(end).Line)
		return true
	})
}
