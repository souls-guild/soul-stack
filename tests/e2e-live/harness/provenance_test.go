package harness

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The provenance pre-flight has four independent ways to be worthless on this
// tier, and each needs its own known-bad case here:
//
//  1. it judges wrongly                  -> TestStaleReasonTruthTable
//  2. nothing calls it                   -> TestKeeperSpawnerChecksProvenance
//  3. it calls it and then skips         -> TestProvenanceFailureIsFatalNotSkip
//  4. its red is labelled STAND-SETUP    -> TestProvenancePrecedesTheBringUpDeclaration
//
// The fourth is this tier's own, and it is the one that would hurt most: a
// refusal filed under "the stand didn't come up" is a refusal the reader has
// been told to ignore. Fixing any one of the four leaves the other three open.

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

// TestStaleMessageSteersAwayFromBuildLinux — L3b's own trap, spelled out.
//
// `make e2e-live` depends on `build-linux`, which produces
// keeper/bin/keeper-linux-amd64 — while this tier spawns the NATIVE
// keeper/bin/keeper on the host. Someone who reads "rebuild" and reaches for
// the target the tier already ran gets the same red again with no new
// information, and concludes the check is broken rather than the binary.
//
// BOTH stale messages, not just the version one. They are separate format
// strings, and the freshness message is the one a developer mid-edit actually
// reaches; pinning only the other would cover the rarer half while reading, by
// its own name, as if it covered the trap.
func TestStaleMessageSteersAwayFromBuildLinux(t *testing.T) {
	older := time.Now().Add(-time.Hour)
	for _, c := range []struct {
		axis string
		p    provenance
	}{
		{"version-mismatch", provenance{reported: "v0.1.0-169-gdef", expected: "v0.1.0-200-gabc"}},
		{"uncommitted-source freshness", provenance{
			reported: "v0.1.0-200-gabc", expected: "v0.1.0-200-gabc",
			binaryMTime: older, newestSrc: "keeper/internal/api/server.go", newestSrcMTime: time.Now(),
		}},
	} {
		msg := c.p.staleReason()
		if msg == "" {
			t.Fatalf("the %s case is not stale at all, so this test would be checking the wording "+
				"of a message it never produced", c.axis)
		}
		if !strings.Contains(msg, "build-linux") {
			t.Errorf("the %s message never mentions build-linux, so nothing warns the reader that "+
				"`make e2e-live`'s own build step does not fix this. Message:\n%s", c.axis, msg)
		}
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
//     run from tests/e2e-live/harness means tests/e2e-live/harness/keeper, which
//     does not exist, so git reports a clean tree and the axis switches itself
//     off. The temp repo below is deliberately somewhere else entirely, so a
//     call that forgot cmd.Dir would match nothing and this fails.
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
// compiling, so the run would say so anyway. It does not. tests/e2e-live has no
// replace directive for keeper — it cannot, the packages are internal — so
// `go test -tags=e2e_live` never compiles keeper at all, and the hand-run path this
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
// stale silently: add a second entry point next month, forget to add it here,
// and this guard reports green about a spawner it never looked at — which is
// NIM-490's own shape, a check that certifies what it did not examine. An
// exemption list goes stale LOUDLY: the new function shows up as a failure until
// someone either wires the pre-flight in or writes down why it does not need it.
var provenanceExempt = map[string]string{
	"keeperBinaryPath": "resolves the path for exec calls inside a test whose NewStack has " +
		"already vetted the same binary; checking again would re-shell `keeper version` " +
		"per call for an answer that cannot have changed",
}

// TestKeeperSpawnerChecksProvenance — the pre-flight is wired in.
//
// A verdict nobody asks for gates nothing. NewStack is today's single door onto
// this tier — every L3b test goes through it — but the set under inspection is
// DERIVED from "calls locateKeeperBinary" rather than named, so a second door
// cannot be added without this guard noticing.
func TestKeeperSpawnerChecksProvenance(t *testing.T) {
	const (
		assertName = "assertKeeperBinaryMatchesTree"
		locateName = "locateKeeperBinary"
	)

	var checked, exempt int
	for name, fn := range harnessFuncs(t) {
		if name == locateName || !callsNamed(fn.Body, locateName) {
			continue
		}
		if why, ok := provenanceExempt[name]; ok {
			t.Logf("%s: exempt — %s", name, why)
			exempt++
			continue
		}
		if !callsNamed(fn.Body, assertName) {
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
		if callsNamed(fn.Body, soft) {
			t.Errorf("assertKeeperBinaryMatchesTree calls %s. A stale binary has to stop the run: "+
				"anything short of Fatalf lets the stand come up anyway and the tier goes on to "+
				"report a verdict about the wrong code.", soft)
		}
	}
	if !callsNamed(fn.Body, "Fatalf") {
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
// — while OUTSIDE the region the refusal reaches the reader as a bare
// `--- FAIL:` on every test in the tier at once, and a wall of those is read as
// a wall of findings about the code.
//
// The lower end is the one setupdecl_test.go cannot see: a call placed after
// `infraUp = true` is outside the region too, and by then three containers,
// Vault and a keeper are already up — the check would refuse a run that has
// already spent its cost, and refuse it after the very keeper it was meant to
// vet had started.
func TestProvenancePrecedesTheBringUpDeclaration(t *testing.T) {
	fn, ok := harnessFuncs(t)["NewStack"]
	if !ok {
		t.Fatal("NewStack is gone from the harness")
	}
	d := standSetupDefer(fn)
	if d == nil {
		t.Fatal("NewStack no longer defers declareStandSetupFailure — setupdecl_test.go covers what " +
			"that costs; this guard cannot place the pre-flight relative to a region that is gone")
	}
	declare := d.Pos()
	flag := regionFlagName(d)
	if flag == "" {
		t.Fatal("NewStack defers declareStandSetupFailure without handing it `&<flag>`, so the " +
			"region has no closing variable to find and this guard has no lower end")
	}
	regionEnd := assignTruePos(fn, flag)
	if regionEnd == token.NoPos {
		t.Fatalf("NewStack has no `%s = true`, so the declared region never closes and this guard "+
			"has no lower end to check against", flag)
	}

	pos := token.NoPos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != "assertKeeperBinaryMatchesTree" {
			return true
		}
		if pos == token.NoPos || call.Pos() < pos {
			pos = call.Pos()
		}
		return true
	})
	if pos == token.NoPos {
		t.Fatal("NewStack does not call assertKeeperBinaryMatchesTree at all")
	}
	if pos < declare {
		t.Error("NewStack checks the keeper binary BEFORE opening the bring-up declaration, so the " +
			"refusal carries no STAND-SETUP marker. Every test in this tier goes through NewStack, " +
			"so an unlabelled refusal is not one red line but one per test, and a suite red in " +
			"unlabelled FAILs reads as findings about the code rather than one fact about the " +
			"build. Move the call below the defer.")
	}
	if pos > regionEnd {
		t.Errorf("NewStack checks the keeper binary AFTER `%s = true`. That is outside the declared "+
			"region at the far end: the containers, Vault and the keeper are already up, so the run "+
			"has spent its whole cost before the check refuses it — and it refuses it after starting "+
			"the very binary it was meant to vet.", flag)
	}
}

// regionFlagName — the variable whose `= true` closes the region, read out of
// the defer's own third argument rather than assumed to be called `infraUp`.
// Deriving it means a rename moves the boundary with it, instead of leaving this
// guard looking for a name that no longer exists.
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

// assignTruePos — where `<flag> = true` closes the region. Matching any
// `<ident> = true` would be looser than the question deserves: the first
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

// callsNamed — one name, through the same call-position property callsAny
// encodes: an identifier only counts where it is being CALLED, so a local
// variable that happens to share the name is not a match. setupdecl_test.go
// explains what that cost when the guard there tried it the other way. Both
// call shapes count — `Fatalf(...)` and `t.Fatalf(...)` are matched on the
// final name, so the receiver is not part of the property.
func callsNamed(body *ast.BlockStmt, name string) bool {
	return callsAny(body, map[string]bool{name: true})
}
