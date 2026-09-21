//go:build e2e

package harness

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The provenance pre-flight has four independent ways to be worthless, and
// each needs its own known-bad case here:
//
//  1. it judges wrongly           -> TestStaleReasonTruthTable
//  2. nothing calls it            -> TestEveryKeeperSpawnerChecksProvenance
//  3. it calls it and then skips  -> TestProvenanceFailureIsFatalNotSkip
//  4. it judges the wrong files   -> TestKeeperSourceRootsAreTheModulesKeeperLinks
//
// Fixing any one of them leaves the other three open. That is the lesson
// NIM-420 paid for four times over, and this file is what it costs not to pay
// it again.

// TestStaleReasonTruthTable — the verdict, stated rather than sampled.
//
// Two rows carry the design. "same commit, uncommitted source younger than the
// binary" is the NIM-456 shape verbatim (edit a file, re-run the tier without
// building) and is invisible to the commit axis, which is why the second axis
// exists. "dirt outside the keeper's sources" is its price kept in check: a
// README edit must not be reported as a stale binary, or the next person to see
// this red will learn to ignore it.
func TestStaleReasonTruthTable(t *testing.T) {
	var (
		built  = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
		edited = built.Add(30 * time.Minute)
		older  = built.Add(-30 * time.Minute)
	)

	cases := []struct {
		name     string
		in       provenance
		wantStop bool
		wantSaid string // substring the message must carry; "" for none
		why      string
	}{
		{
			name:     "clean tree, same commit",
			in:       provenance{reported: "v0.1.0-200-gabc", expected: "v0.1.0-200-gabc"},
			wantStop: false,
			why:      "make build stamps git describe, so with nothing uncommitted a match IS the proof",
		},
		{
			name:     "another commit",
			in:       provenance{reported: "v0.1.0-169-gdef", expected: "v0.1.0-200-gabc"},
			wantStop: true, wantSaid: "v0.1.0-169-gdef",
			why: "the binary names the commit it came from and it is not this one",
		},
		{
			name:     "hand-built without ldflags",
			in:       provenance{reported: "0.0.0-dev", expected: "v0.1.0-200-gabc"},
			wantStop: true, wantSaid: "0.0.0-dev",
			why: "`go build ./cmd/keeper` leaves the default in place; it must not pass for the tree",
		},
		{
			name: "dirt outside the keeper's sources — a README edit",
			in: provenance{
				reported: "v0.1.0-200-gabc", expected: "v0.1.0-200-gabc-dirty",
				binaryMTime: built, newestSrc: "", // nothing uncommitted under keeper/shared/sdk/proto
			},
			wantStop: false,
			why: "the marker says only that SOMETHING in the repo is uncommitted. Reddening a " +
				"byte-correct binary over a docs edit is how a check earns a reputation for crying wolf",
		},
		{
			name: "same commit, an uncommitted source younger than the binary",
			in: provenance{
				reported: "v0.1.0-200-gabc-dirty", expected: "v0.1.0-200-gabc-dirty",
				binaryMTime: built, newestSrc: "keeper/internal/daemon/daemon.go", newestSrcMTime: edited,
			},
			wantStop: true, wantSaid: "keeper/internal/daemon/daemon.go",
			why: "NIM-456: the edit is uncommitted, so both sides describe the same and only time is left",
		},
		{
			name: "built clean, then a keeper source edited",
			in: provenance{
				reported: "v0.1.0-200-gabc", expected: "v0.1.0-200-gabc-dirty",
				binaryMTime: built, newestSrc: "keeper/internal/daemon/daemon.go", newestSrcMTime: edited,
			},
			wantStop: true, wantSaid: "keeper/internal/daemon/daemon.go",
			why: "the marker differs but the commit does not, so only the timestamp separates them — " +
				"and the same row with newestSrc empty above must stay green, or the axis is just the marker again",
		},
		{
			name: "same commit, binary younger than everything uncommitted",
			in: provenance{
				reported: "v0.1.0-200-gabc-dirty", expected: "v0.1.0-200-gabc-dirty",
				binaryMTime: built, newestSrc: "keeper/internal/daemon/daemon.go", newestSrcMTime: older,
			},
			wantStop: false,
			why:      "this is what `make build` just did — the ordinary green path, and it must stay quiet",
		},
		{
			name: "another commit, and a fresh binary",
			in: provenance{
				reported: "v0.1.0-169-gdef-dirty", expected: "v0.1.0-200-gabc-dirty",
				binaryMTime: edited, newestSrc: "keeper/internal/daemon/daemon.go", newestSrcMTime: older,
			},
			wantStop: true, wantSaid: "v0.1.0-200-gabc-dirty",
			why: "a recent build of the WRONG commit is still the wrong commit",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in.staleReason()
			if (got != "") != c.wantStop {
				t.Fatalf("staleReason() = %q, want stop=%v — %s", got, c.wantStop, c.why)
			}
			if c.wantSaid != "" && !strings.Contains(got, c.wantSaid) {
				t.Errorf("the message has to name %q, or the reader cannot tell what is wrong; got:\n%s", c.wantSaid, got)
			}
			if c.wantStop && !strings.Contains(got, "make build") {
				t.Errorf("the message has to say how to fix it (`make build`); got:\n%s", got)
			}
		})
	}
}

// TestParseKeeperVersion — the format is a contract with cmd/keeper/main.go,
// which prints "keeper %s (%s)". If it ever changes shape this check would
// otherwise start comparing garbage against a version string and redden every
// run with the wrong reason — the failure mode that gets a gate switched off.
func TestParseKeeperVersion(t *testing.T) {
	ok := map[string]string{
		"keeper v0.1.0-beta.1-169-g3609a990-dirty (go1.26.5)\n": "v0.1.0-beta.1-169-g3609a990-dirty",
		"keeper 0.0.0-dev (go1.26.5)":                           "0.0.0-dev",
		"keeper v1.2.3 (go1.26.5)\nsome trailing noise\n":       "v1.2.3",
	}
	for in, want := range ok {
		got, err := parseKeeperVersion(in)
		if err != nil {
			t.Errorf("parseKeeperVersion(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseKeeperVersion(%q) = %q, want %q", in, got, want)
		}
	}

	// Silence and wrong-binary output must be errors, not an empty version that
	// then differs from everything and reddens for a reason nobody can act on.
	for _, in := range []string{"", "\n", "soul v1.2.3 (go1.26.5)", "keeper", "keeper  (go1.26.5)"} {
		if got, err := parseKeeperVersion(in); err == nil {
			t.Errorf("parseKeeperVersion(%q) = %q, want an error", in, got)
		}
	}
}

// TestNewestUncommittedKeeperSourceIsScopedToTheRepoAndItsRoots — the half of
// the freshness axis that a pure function cannot state.
//
// Two ways for it to find nothing and call that "clean", both silent:
//
//   - Pathspecs resolve against the WORKING DIRECTORY. `git status -- keeper`
//     run from tests/e2e/harness means tests/e2e/harness/keeper, which does not
//     exist, so git reports a clean tree and the axis switches itself off. The
//     temp repo below is deliberately somewhere else entirely, so a call that
//     forgot cmd.Dir would match nothing and this fails.
//
//   - The roots must still bound it. docs/README.md here is the youngest file
//     of the three: if the pathspecs were dropped it would win, and every
//     documentation edit would be reported as a stale keeper binary.
func TestNewestUncommittedKeeperSourceIsScopedToTheRepoAndItsRoots(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init in %s: %v (%s)", root, err, out)
	}

	write := func(rel string, age time.Duration) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/README.md", 0) // the youngest, and none of the binary's business
	write("keeper/internal/daemon/daemon.go", time.Hour)
	write("shared/log/log.go", 2*time.Hour)

	name, mtime, err := newestUncommittedKeeperSource(root)
	if err != nil {
		t.Fatalf("newestUncommittedKeeperSource: %v", err)
	}
	switch name {
	case "keeper/internal/daemon/daemon.go":
		// expected
	case "":
		t.Fatal("found nothing uncommitted in a repo with three uncommitted files. Untracked files " +
			"are the whole point of -uall (a new source is invisible to `git describe --dirty`), and " +
			"a pathspec resolved against the test's own directory matches nothing and reports clean.")
	case "docs/README.md":
		t.Fatal("picked docs/README.md — the pathspecs are not bounding the search, so every docs " +
			"edit now reports the keeper binary as stale")
	default:
		t.Fatalf("picked %q, want keeper/internal/daemon/daemon.go (the youngest under the roots)", name)
	}
	if mtime.IsZero() {
		t.Error("returned a path with a zero mtime, which compares younger than any binary and so " +
			"can never redden")
	}
}

// TestNewestUncommittedKeeperSourceSeesADeletion — the axis holds when a source
// is removed rather than edited.
//
// A deletion is the one uncommitted change with no file left to stat, and
// skipping it silenced BOTH axes at once: the commit axis strips `-dirty` from
// both sides by design, so a binary built before the delete compares equal to
// the tree that no longer has the file. The run that follows executes code the
// tree does not contain, and says nothing.
//
// The reasoning that justified the skip was that a deletion stops the tree
// compiling, so the run would say so anyway. It does not. tests/e2e has no
// replace directive for keeper — it cannot, the packages are internal — so
// `go test -tags=e2e` never compiles keeper at all, and the hand-run path this
// whole pre-flight exists for notices nothing.
func TestNewestUncommittedKeeperSourceSeesADeletion(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q")

	victim := filepath.Join(root, "keeper", "internal", "daemon", "daemon.go")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("package daemon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("-c", "commit.gpgsign=false", "commit", "-qm", "seed")

	// Everything committed and older than the build: nothing to report.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Dir(victim), old, old); err != nil {
		t.Fatal(err)
	}
	if name, _, err := newestUncommittedKeeperSource(root); err != nil || name != "" {
		t.Fatalf("clean tree reported %q (err %v), want nothing uncommitted", name, err)
	}

	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}

	name, mtime, err := newestUncommittedKeeperSource(root)
	if err != nil {
		t.Fatalf("newestUncommittedKeeperSource after a delete: %v", err)
	}
	if name == "" {
		t.Fatal("a deleted keeper source reported nothing uncommitted. Both axes are now blind " +
			"to a delete-and-rerun: the commit axis strips -dirty from both sides, and this one " +
			"skipped the entry for having no file to stat. The parent directory has the " +
			"timestamp — unlink() updates it.")
	}
	if name != "keeper/internal/daemon/daemon.go" {
		t.Fatalf("reported %q, want the deleted path itself — the mtime comes from the parent "+
			"directory, but the name has to say which source is gone", name)
	}
	if !mtime.After(old) {
		t.Errorf("mtime %s is not after the pre-delete stamp %s. The timestamp is being read "+
			"from something the unlink did not touch, so a binary built a minute ago still "+
			"compares younger than the deletion and the run goes green.", mtime, old)
	}
}

// provenanceExempt — the functions that resolve the keeper binary's path and
// are deliberately NOT required to vet it, each with the reason.
//
// An exemption list rather than a hardcoded list of the ones that must check,
// because the two fail in opposite directions. A list of who must check goes
// stale silently: add a third entry point next month, forget to add it here, and
// this guard reports green about a spawner it never looked at — which is
// NIM-490's own shape, a check that certifies what it did not examine. An
// exemption list goes stale LOUDLY: the new function shows up as a failure until
// someone either wires the pre-flight in or writes down why it does not need it.
var provenanceExempt = map[string]string{
	"keeperBinaryPath": "resolves the path for exec calls inside a test whose NewStack has " +
		"already vetted the same binary; checking again would re-shell `keeper version` " +
		"per call for an answer that cannot have changed",
}

// TestEveryKeeperSpawnerChecksProvenance — the pre-flight is wired in.
//
// A verdict nobody asks for gates nothing. The set under inspection is DERIVED —
// everything that calls locateKeeperBinary — so it cannot fall behind the code
// it guards. Pinned by count as well: a guard that walks a package and finds no
// candidates passes by vacuum, which is how this class of test usually dies.
func TestEveryKeeperSpawnerChecksProvenance(t *testing.T) {
	const (
		assertName = "assertKeeperBinaryMatchesTree"
		locateName = "locateKeeperBinary"
	)

	var checked, exempt int
	for name, fn := range harnessFuncs(t) {
		if name == locateName || !callsIdent(fn.Body, locateName) {
			continue
		}
		if why, ok := provenanceExempt[name]; ok {
			t.Logf("%s: exempt — %s", name, why)
			exempt++
			continue
		}
		if !callsIdent(fn.Body, assertName) {
			t.Errorf("%s resolves the keeper binary and does not call %s. It spawns code that "+
				"may not be in the tree — the whole of NIM-490. Wire the pre-flight in, or add "+
				"it to provenanceExempt with the reason it does not need one.", name, assertName)
			continue
		}
		checked++
	}

	if checked == 0 {
		t.Errorf("no function in the harness both calls %s and vets it. Either the pre-flight "+
			"is wired in nowhere, or %s was renamed and this guard now walks the package "+
			"finding nothing — which passes by vacuum and gates zero.", locateName, locateName)
	}
	if exempt != len(provenanceExempt) {
		t.Errorf("provenanceExempt names %d function(s), %d of which were found. An exemption for "+
			"a function that no longer exists is a hole waiting for the name to come back.",
			len(provenanceExempt), exempt)
	}
}

// TestProvenanceFailureIsFatalNotSkip — how it refuses.
//
// This is the ticket's own defect one level up. The old pre-flight DID notice
// something (a missing binary) and answered with t.Skipf, and a skip is read as
// "fine here" by every human and every classifier above it. An edit that
// softened this refusal back into a skip would restore the false green while
// leaving both checks above green, so the shape of the refusal is pinned too.
func TestProvenanceFailureIsFatalNotSkip(t *testing.T) {
	fn, ok := harnessFuncs(t)["assertKeeperBinaryMatchesTree"]
	if !ok {
		t.Fatal("assertKeeperBinaryMatchesTree is gone from the harness — if it was renamed, " +
			"rename it here too; if it was deleted, NIM-490 is undone")
	}
	for _, soft := range []string{"Skip", "Skipf", "SkipNow", "Errorf"} {
		if callsIdent(fn.Body, soft) {
			t.Errorf("assertKeeperBinaryMatchesTree calls %s. A stale binary has to stop the run: "+
				"anything short of Fatalf lets the stand come up anyway and the tier goes on to "+
				"report a verdict about the wrong code.", soft)
		}
	}
	if !callsIdent(fn.Body, "Fatalf") {
		t.Error("assertKeeperBinaryMatchesTree never calls Fatalf, so it cannot stop anything")
	}
}

// TestProvenancePrecedesTheBringUpDeclaration — where the refusal happens.
//
// Two ends, and the name is now half a lie: the call belongs INSIDE the declared
// bring-up region, so it must come after the defer and before `infraUp = true`.
//
// The upper end is a reversal, and worth stating because the first cut of
// NIM-490 pinned the opposite. The argument for going above the defer was that
// "your binary is from another tree" is an instruction, and STAND-SETUP tells
// the reader to ignore what it labels. What that argument missed is that the
// label does not replace the message — the refusal's own text prints either way
// — while OUTSIDE the region the refusal reaches the reader as forty unlabelled
// `--- FAIL:` lines, one per test in the tier, and forty of those are read as
// forty findings about the code. Trading one clear message for forty false ones
// is the wrong direction, and NIM-547 had already settled it for the presence
// check one line above (see NewStack).
//
// The lower end is unchanged and is the one setupdecl_test.go cannot see: a call
// after `infraUp = true` is outside the region too, and by then postgres, redis,
// Vault and a keeper are up — the check would refuse a run that has already
// spent its cost, and refuse it after the very keeper it was meant to vet had
// started.
func TestProvenancePrecedesTheBringUpDeclaration(t *testing.T) {
	funcs := harnessFuncs(t)
	for _, name := range []string{"NewStack", "NewMultiKeeperStack"} {
		fn, ok := funcs[name]
		if !ok {
			t.Errorf("%s is gone from the harness", name)
			continue
		}
		d := standSetupDefer(fn)
		if d == nil {
			t.Errorf("%s no longer defers declareStandSetupFailure — setupdecl_test.go covers what "+
				"that costs; this guard cannot place the pre-flight relative to a region that is gone", name)
			continue
		}
		declare := d.Pos()
		flag := regionFlagName(d)
		if flag == "" {
			t.Errorf("%s defers declareStandSetupFailure without handing it `&<flag>`, so the region "+
				"has no closing variable to find. setupdecl_test.go owns what that costs; here it "+
				"leaves this guard with no lower end.", name)
			continue
		}
		regionEnd := assignTruePos(fn, flag)
		if regionEnd == token.NoPos {
			t.Errorf("%s has no `%s = true`, so the declared region never closes and this guard has "+
				"no lower end to check against. setupdecl_test.go covers what an unclosed region "+
				"costs; without it the pre-flight could sit anywhere below.", name, flag)
			continue
		}

		pos := token.NoPos
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "assertKeeperBinaryMatchesTree" {
				return true
			}
			if pos == token.NoPos || call.Pos() < pos {
				pos = call.Pos()
			}
			return true
		})
		if pos == token.NoPos {
			t.Errorf("%s does not call assertKeeperBinaryMatchesTree at all", name)
			continue
		}
		if pos < declare {
			t.Errorf("%s checks the keeper binary BEFORE opening the bring-up declaration, so the "+
				"refusal carries no STAND-SETUP marker. Every test in this tier calls %s, so an "+
				"unlabelled refusal is not one red line, it is one per test — and a suite red in "+
				"forty unlabelled FAILs reads as forty findings about the code rather than one fact "+
				"about the build. Move the call below the defer.", name, name)
		}
		if pos > regionEnd {
			t.Errorf("%s checks the keeper binary AFTER `%s = true`. That is outside the declared "+
				"region at the far end: the containers, Vault and the keeper are already up, so the "+
				"run has spent its whole cost before the check refuses it — and it refuses it after "+
				"starting the very binary it was meant to vet.", name, flag)
		}
	}
}

// standSetupDefer — the `defer declareStandSetupFailure(...)` a function opens
// its region with, as the statement rather than as a position, because both ends
// of the region are read off it: it opens where the defer sits, and it closes
// where the variable in its third argument is set (regionFlagName).
func standSetupDefer(fn *ast.FuncDecl) *ast.DeferStmt {
	var out *ast.DeferStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		if id, ok := d.Call.Fun.(*ast.Ident); ok && id.Name == "declareStandSetupFailure" {
			if out == nil || d.Pos() < out.Pos() {
				out = d
			}
		}
		return true
	})
	return out
}

// assignTruePos — where `<flag> = true` closes the region.
//
// The flag comes from the defer, not from a literal "infraUp", for the reason
// regionFlagName gives: a renamed flag must move this boundary with it rather
// than make the guard report a name it can no longer find as a defect. Matching
// any `<ident> = true` would be looser than the question deserves — the first
// unrelated boolean set inside an entry point would move the boundary up and
// redden a correctly placed call.
func assignTruePos(fn *ast.FuncDecl, flag string) token.Pos {
	pos := token.NoPos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if lhs, ok := as.Lhs[0].(*ast.Ident); !ok || lhs.Name != flag {
			return true
		}
		if rhs, ok := as.Rhs[0].(*ast.Ident); !ok || rhs.Name != "true" {
			return true
		}
		if pos == token.NoPos || as.Pos() < pos {
			pos = as.Pos()
		}
		return true
	})
	return pos
}

// harnessFuncs — the package's functions by name, parsed from source rather
// than reflected: the properties above are about what the code SAYS, and a
// compiled function cannot be asked whether it skips.
func harnessFuncs(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse harness sources: %v", err)
	}

	out := map[string]*ast.FuncDecl{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
					out[fn.Name.Name] = fn
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("parsed no functions out of the harness — the walk is broken, not the code")
	}
	return out
}

// firstPartyPrefix — the module path every workspace module shares. An import
// that starts with it is source in this repo; anything else is a dependency the
// binary pins by version, which a rebuild does not silently change.
const firstPartyPrefix = "github.com/souls-guild/soul-stack/"

// TestKeeperSourceRootsAreTheModulesKeeperLinks — the fourth way to be
// worthless, and the one this branch is least entitled to leave open.
//
// keeperSourceRoots decides which files the mtime axis even looks at. It is a
// hardcoded list, and a hardcoded list of what-to-check goes stale in the
// SILENT direction: the day keeper starts importing a new workspace module,
// every uncommitted edit in it becomes invisible to the check, which goes on
// reporting green about a binary it no longer covers. That is the ticket's own
// defect wearing a different hat, so the list gets derived here and compared,
// rather than trusted.
//
// The derivation walks the first-party import closure from `keeper/cmd/keeper`
// with go/parser — no toolchain resolution, no module cache, no network, so it
// behaves the same on a laptop and on a CI runner with a cold cache.
//
// Build constraints are deliberately NOT applied, and the cost of that is worth
// stating plainly rather than waving at: a package reachable only behind a tag
// the default build never sets still counts as a root, so this test would go
// red until someone adds it to keeperSourceRoots, after which the runtime check
// watches a directory an edit to which cannot change the binary. That is a
// false red, the direction this branch argues against everywhere else. It is
// accepted here because the alternative error is the silent one — a root that
// IS linked and is not watched makes every edit under it invisible — and
// because the two are not equally recoverable: a spurious root is one line to
// delete once someone looks, while a missing one is never looked at.
//
// The comparison covers every tier at once. L3a/L3b/L3c are separate Go modules
// and cannot share this list, so it is copy-pasted three times; a check that
// pinned only its own copy would let the other two drift. Which files those are
// is DISCOVERED, for the same reason the roots themselves are: naming the three
// here would make a fourth tier's copy invisible on the day it is added, and
// this test would keep reporting green about a list it never opened.
func TestKeeperSourceRootsAreTheModulesKeeperLinks(t *testing.T) {
	root := repoTopLevel(t)

	start := filepath.Join("keeper", "cmd", "keeper")
	derived := firstPartyClosure(t, root, start)
	if len(derived) < 2 {
		t.Fatalf("the import walk found %d first-party root(s) %v starting from %s. keeper is known to "+
			"link its own module plus several workspace siblings, so a result this small means the walk "+
			"broke — and a broken walk agrees with any list at all", len(derived), derived, start)
	}

	for _, file := range tierProvenanceFiles(t, root) {
		configured := sourceRootsLiteral(t, filepath.Join(root, file))

		if missing := difference(derived, configured); len(missing) > 0 {
			t.Errorf("%s: keeper links %v, which keeperSourceRoots does not list. An uncommitted "+
				"edit under those directories changes the binary and the mtime axis will not see it, so "+
				"the tier reports green while testing something else. Add them to the list.",
				file, missing)
		}
		if extra := difference(configured, derived); len(extra) > 0 {
			t.Errorf("%s: keeperSourceRoots lists %v, which nothing reachable from keeper/cmd/keeper "+
				"imports. Editing those cannot change the binary, so the tier reddens on unrelated work — "+
				"which is how a check earns the reputation that gets it bypassed.",
				file, extra)
		}
	}
}

// tierProvenanceFiles — every tier's provenance.go, repo-relative and sorted.
//
// Globbed rather than listed, because a list of who-must-be-checked is the
// thing this whole file argues against: add a fourth tier, forget the list, and
// the guard passes without ever reading the new copy. The glob's own failure
// mode is the opposite one and is handled by the floor below — three tiers
// exist today, so fewer than three means the pattern stopped matching, not that
// the repo shrank, and a silent empty set would make this test vacuous.
//
// A tier whose provenance.go deliberately watches different roots — a soul
// artifact rather than the keeper one, say (NIM-636) — will fail loudly in
// sourceRootsLiteral instead of being skipped. That is the right direction: it
// is a decision for whoever adds it, not something to discover later from a
// green run.
func tierProvenanceFiles(t *testing.T, root string) []string {
	t.Helper()

	pattern := filepath.Join(root, "tests", "*", "harness", "provenance.go")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("tierProvenanceFiles: glob %s: %v", pattern, err)
	}

	out := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(root, m)
		if err != nil {
			t.Fatalf("tierProvenanceFiles: %s is not under %s: %v", m, root, err)
		}
		out = append(out, rel)
	}
	sort.Strings(out)

	if len(out) < 3 {
		t.Fatalf("tierProvenanceFiles: %s matched %d file(s) %v, want at least the three tiers "+
			"(e2e, e2e-live, e2e-k8s). A pattern that matches too few makes this comparison pass "+
			"by looking at nothing", pattern, len(out), out)
	}
	return out
}

// repoTopLevel — the repository root, as the harness package sees it. Asserted
// rather than assumed: a wrong root would make every lookup below miss, and
// missing files are exactly how this kind of test passes while checking nothing.
func repoTopLevel(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("repoTopLevel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		t.Fatalf("repoTopLevel resolved %s, which has no go.work: this test is no longer where it thinks "+
			"it is, and everything it reads below would silently be missing. err=%v", root, err)
	}
	return root
}

// firstPartyClosure — the set of top-level repo directories reachable from the
// given package by following first-party imports. Returned sorted so failures
// read the same twice.
func firstPartyClosure(t *testing.T, root, startPkgDir string) []string {
	t.Helper()

	seen := map[string]bool{}
	roots := map[string]bool{}
	queue := []string{filepath.ToSlash(startPkgDir)}

	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if seen[dir] {
			continue
		}
		seen[dir] = true
		roots[strings.SplitN(dir, "/", 2)[0]] = true

		abs := filepath.Join(root, filepath.FromSlash(dir))
		entries, err := os.ReadDir(abs)
		if err != nil {
			t.Fatalf("firstPartyClosure: read package dir %s: %v (an unreadable package is not an empty "+
				"one — the closure below would be missing whatever it imports)", dir, err)
		}

		fset := token.NewFileSet()
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(abs, name), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("firstPartyClosure: parse %s/%s: %v", dir, name, err)
			}
			for _, imp := range f.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil || !strings.HasPrefix(path, firstPartyPrefix) {
					continue
				}
				queue = append(queue, strings.TrimPrefix(path, firstPartyPrefix))
			}
		}
	}

	out := make([]string, 0, len(roots))
	for r := range roots {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// sourceRootsLiteral — the keeperSourceRoots value as written in a tier's
// provenance.go, read from source rather than imported: the other two tiers are
// different Go modules, so this package cannot reference their variables.
func sourceRootsLiteral(t *testing.T, path string) []string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("sourceRootsLiteral: parse %s: %v", path, err)
	}

	var out []string
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if name.Name != "keeperSourceRoots" || i >= len(spec.Values) {
				continue
			}
			lit, ok := spec.Values[i].(*ast.CompositeLit)
			if !ok {
				t.Fatalf("sourceRootsLiteral: %s: keeperSourceRoots is not a composite literal, so this "+
					"test can no longer read it and would compare against nothing", path)
			}
			found = true
			for _, el := range lit.Elts {
				bl, ok := el.(*ast.BasicLit)
				if !ok {
					t.Fatalf("sourceRootsLiteral: %s: keeperSourceRoots holds a non-literal element; the "+
						"list must stay readable from source for this comparison to mean anything", path)
				}
				s, err := strconv.Unquote(bl.Value)
				if err != nil {
					t.Fatalf("sourceRootsLiteral: %s: unquote %s: %v", path, bl.Value, err)
				}
				out = append(out, s)
			}
		}
		return true
	})

	if !found {
		t.Fatalf("sourceRootsLiteral: %s declares no keeperSourceRoots. Either the tier stopped scoping "+
			"the mtime axis at all, or it renamed the variable and this comparison silently stopped "+
			"covering that tier", path)
	}
	sort.Strings(out)
	return out
}

// difference — elements of a not present in b, both assumed sorted.
func difference(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	var out []string
	for _, s := range a {
		if !inB[s] {
			out = append(out, s)
		}
	}
	return out
}
