package harness

// Docker-free guards for the L3c provenance pre-flight (NIM-490).
//
// Three ways the pre-flight can stop working, and each has its own case here,
// because a guard that only covers one of them reports green while the other
// two are broken:
//
//  1. the verdict is wrong - it calls a good image stale, or a stale one good;
//  2. nobody calls it - a perfect verdict function reached from no entry point
//     gates exactly nothing;
//  3. it stops being fatal - a skip is what the old behaviour amounted to, and
//     a skipped pre-flight reads as "fine".
//
// Running the tier itself proves none of this: L3c needs docker, kind, kubectl
// and helm, so on a machine without them every case above passes by not
// running. These are untagged and need none of it.

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

// TestImageStaleReasonTruthTable - every shape the verdict has to get right,
// stated rather than sampled.
func TestImageStaleReasonTruthTable(t *testing.T) {
	built := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	edited := built.Add(5 * time.Minute)
	older := built.Add(-5 * time.Minute)

	cases := []struct {
		name    string
		p       imageProvenance
		wantRed bool
		key     string
		why     string
	}{
		{
			name: "the image is this commit and nothing keeper-side is uncommitted",
			p: imageProvenance{
				image: "keeper:e2e-k8s", created: built,
				reported: "v0.1.0-200-gabc", expected: "v0.1.0-200-gabc",
			},
			why: "the ordinary `make e2e-k8s` run; a red here would be the check crying wolf on every use",
		},
		{
			name: "the image is from another commit",
			p: imageProvenance{
				image: "keeper:e2e-k8s", created: built,
				reported: "v0.1.0-190-gzzz", expected: "v0.1.0-200-gabc",
			},
			wantRed: true, key: "v0.1.0-190-gzzz",
			why: "the weeks-old image: `kind load` accepts it silently and the tier reports on it",
		},
		{
			name: "the image was built by hand, without the Makefile's ldflags",
			p: imageProvenance{
				image: "keeper:e2e-k8s", created: built,
				reported: "0.0.0-dev", expected: "v0.1.0-200-gabc",
			},
			wantRed: true, key: "0.0.0-dev",
			why: "an unstamped image cannot vouch for anything, so it is not allowed to try",
		},
		{
			name: "dirt outside the keeper's sources - a README edit",
			p: imageProvenance{
				image: "keeper:e2e-k8s", created: built,
				reported: "v0.1.0-200-gabc", expected: "v0.1.0-200-gabc-dirty",
				newestSrc: "",
			},
			why: "`-dirty` is repo-wide; reddening a byte-correct image over a README is how a gate gets disabled",
		},
		{
			name: "same commit, but a keeper source edited after the image was built",
			p: imageProvenance{
				image: "keeper:e2e-k8s", created: built,
				reported: "v0.1.0-200-gabc", expected: "v0.1.0-200-gabc-dirty",
				newestSrc: "keeper/internal/api/router.go", newestSrcMTime: edited,
			},
			wantRed: true, key: "keeper/internal/api/router.go",
			why: "the NIM-456 shape: the edit is not committed, so only the timestamps can tell",
		},
		{
			name: "same commit, and the edit predates the image",
			p: imageProvenance{
				image: "keeper:e2e-k8s", created: built,
				reported: "v0.1.0-200-gabc", expected: "v0.1.0-200-gabc-dirty",
				newestSrc: "keeper/internal/api/router.go", newestSrcMTime: older,
			},
			why: "docker-build-keeper ran after the edit, so the edit is inside the image",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.p.staleReason()
			switch {
			case c.wantRed && got == "":
				t.Fatalf("verdict is GREEN, want RED - %s", c.why)
			case !c.wantRed && got != "":
				t.Fatalf("verdict is RED, want GREEN - %s\nmessage:\n%s", c.why, got)
			}
			if !c.wantRed {
				return
			}
			if !strings.Contains(got, c.key) {
				t.Errorf("the message never names %q, so the reader cannot see what is wrong:\n%s", c.key, got)
			}
			if !strings.Contains(got, "docker-build-keeper") {
				t.Errorf("the message does not say how to fix it (`make docker-build-keeper`):\n%s", got)
			}
		})
	}
}

// TestStaleMessageSteersAwayFromPlainBuild - the L3c-specific trap.
//
// `make build` produces the host binary. The cluster runs an image. A developer
// who reads "stale keeper", rebuilds the obvious thing and re-runs gets the
// identical failure and no new information, so the message has to say which
// build is the wrong one before they spend the round trip.
func TestStaleMessageSteersAwayFromPlainBuild(t *testing.T) {
	built := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)

	mismatch := imageProvenance{
		image: "keeper:e2e-k8s", created: built,
		reported: "v0.1.0-190-gzzz", expected: "v0.1.0-200-gabc",
	}.staleReason()
	if !strings.Contains(mismatch, "`make build`") {
		t.Errorf("the version-mismatch message never mentions `make build`, so nothing warns the "+
			"reader that rebuilding the host binary does not touch the image. Message:\n%s", mismatch)
	}

	fresher := imageProvenance{
		image: "keeper:e2e-k8s", created: built,
		reported: "v0.1.0-200-gabc", expected: "v0.1.0-200-gabc-dirty",
		newestSrc: "keeper/internal/api/router.go", newestSrcMTime: built.Add(time.Minute),
	}.staleReason()
	if !strings.Contains(fresher, "`make build`") {
		t.Errorf("the freshness message never mentions `make build`, and this is the message a "+
			"developer mid-edit actually hits. Message:\n%s", fresher)
	}
}

// TestParseKeeperVersion - the parse is the one step between `docker run` and
// the verdict, and a silent misparse turns every run green.
func TestParseKeeperVersion(t *testing.T) {
	good := map[string]string{
		"keeper v0.1.0-beta.1-200-g66e7f0dc-dirty (go1.26.5)\n": "v0.1.0-beta.1-200-g66e7f0dc-dirty",
		"keeper 0.0.0-dev (go1.26.5)":                           "0.0.0-dev",
		"keeper v1.2.3 (go1.26.5)\nsomething else\n":            "v1.2.3",
	}
	for in, want := range good {
		got, err := parseKeeperVersion(in)
		if err != nil {
			t.Errorf("parseKeeperVersion(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseKeeperVersion(%q) = %q, want %q", in, got, want)
		}
	}

	// Each of these must ERROR rather than return a plausible-looking string:
	// a wrong version silently compared equal is the failure this whole file
	// exists to prevent.
	for _, in := range []string{"", "\n", "soul v1.2.3 (go1.26.5)", "keeper", "keeper  (go1.26.5)"} {
		if got, err := parseKeeperVersion(in); err == nil {
			t.Errorf("parseKeeperVersion(%q) returned %q with no error - an unparseable "+
				"version has to stop the run, not become one", in, got)
		}
	}
}

// TestNewestUncommittedKeeperSourceIsScopedToTheRepoAndItsRoots - the pathspec
// and the working directory, exercised against a real repo.
//
// Both are silent when wrong. A pathspec is resolved against the process's
// working directory, so `keeper` from tests/e2e-k8s/harness means
// tests/e2e-k8s/harness/keeper, matches nothing, and git reports a clean tree -
// which reads here as "nothing uncommitted" and switches the axis off. Dropping
// the pathspec fails the other way: every docs edit reports the image stale.
func TestNewestUncommittedKeeperSourceIsScopedToTheRepoAndItsRoots(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(rel string, age time.Duration) string {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatalf("chtimes %s: %v", rel, err)
		}
		return p
	}

	run("init", "-q")
	// Youngest of the three, and outside the roots: if it ever wins, the
	// pathspecs are not bounding the search.
	write("docs/README.md", 0)
	write("keeper/internal/daemon/daemon.go", time.Hour)
	write("shared/log/log.go", 2*time.Hour)

	got, mtime, err := newestUncommittedKeeperSource(root)
	if err != nil {
		t.Fatalf("newestUncommittedKeeperSource: %v", err)
	}
	switch got {
	case "":
		t.Fatalf("found nothing uncommitted in a repo with three uncommitted files. Untracked " +
			"files are the whole point of -uall (a new source is invisible to `git describe " +
			"--dirty`), and a pathspec resolved against the test's own directory matches nothing " +
			"and reports clean.")
	case "keeper/internal/daemon/daemon.go":
	default:
		t.Fatalf("picked %s - want keeper/internal/daemon/daemon.go, the youngest file inside "+
			"keeperSourceRoots. docs/README.md is younger still and must not win, or every docs "+
			"edit reports the image as stale.", got)
	}
	if mtime.IsZero() {
		t.Errorf("returned a zero mtime for %s; the freshness comparison would then never fire", got)
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
// compiling, so the run would say so anyway. It does not, and here it could not
// possibly: the subject is an image built earlier by docker, and
// `go test -tags=e2e_k8s` compiles no keeper source at all.
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

// TestStackChecksImageProvenance - the verdict is reached from the tier's entry
// point. Without this the whole file is a well-tested function nobody calls.
func TestStackChecksImageProvenance(t *testing.T) {
	fn := harnessFuncs(t)["NewStack"]
	if fn == nil {
		t.Fatalf("NewStack not found in the harness sources - if it was renamed, this guard has to follow it")
	}
	if !callsNamed(fn.Body, "assertKeeperImageMatchesTree") {
		t.Errorf("NewStack does not call assertKeeperImageMatchesTree. It is the entry point of a " +
			"tier that deploys the keeper image, so without the pre-flight L3c can report on code " +
			"that is not in the tree - the whole of NIM-490.")
	}
}

// TestPickBuildTimePrefersTheTagStamp - the freshness axis reads a clock that
// still moves.
//
// `.Created` is the obvious field and the wrong one. Under BuildKit it belongs
// to the image config and is reproduced from the build inputs, so three
// consecutive `make docker-build-keeper` runs - different image IDs, a
// deliberately changing --label - all reported the SAME `.Created`, to the
// nanosecond. A freshness axis on that field compares an uncommitted edit
// against a stopped clock and never reddens: the defect this check exists to
// prevent, rebuilt inside the check.
//
// `Metadata.LastTagTime` is daemon-local and does move, because `docker build
// -t` re-tags. It is zero for a pulled image, hence the fallback - and the
// fallback is only correct in that direction, so both rows are here.
func TestPickBuildTimePrefersTheTagStamp(t *testing.T) {
	const (
		tagged  = "2026-08-09 04:50:40.53337643 +0000 UTC" // Go's default time.Time rendering
		created = "2026-08-09T04:50:21.029655832Z"         // RFC3339, as .Created prints
		zeroTag = "0001-01-01 00:00:00 +0000 UTC"          // what an unset LastTagTime renders as
	)
	wantTagged := time.Date(2026, 8, 9, 4, 50, 40, 533376430, time.UTC)
	wantCreated := time.Date(2026, 8, 9, 4, 50, 21, 29655832, time.UTC)

	for _, tc := range []struct {
		name string
		raw  string
		want time.Time
		why  string
	}{
		{
			"both present -> the tag stamp wins",
			tagged + "|" + created,
			wantTagged,
			"reading .Created here is the BuildKit trap: it is frozen across rebuilds, so an " +
				"image rebuilt after an edit still reports the old creation time and the " +
				"freshness axis stays quiet forever",
		},
		{
			"tag stamp unset (a pulled image) -> fall back to .Created",
			zeroTag + "|" + created,
			wantCreated,
			"a pulled image was never tagged by this daemon, so LastTagTime is zero. Using it " +
				"would date every pulled image to year 1 and call every tree younger than it - " +
				"a red on every machine that pulls rather than builds",
		},
		{
			"tag stamp older than .Created -> still the tag stamp",
			created + "|" + tagged,
			wantCreated,
			"the tag stamp is not a maximum. It is the answer to `when did this tag last point " +
				"somewhere new`, which is the question the axis asks; picking whichever is later " +
				"would resurrect the frozen .Created the moment it happened to win",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickBuildTime(tc.raw)
			if err != nil {
				t.Fatalf("pickBuildTime(%q): %v", tc.raw, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("pickBuildTime(%q) = %s, want %s\n%s", tc.raw, got, tc.want, tc.why)
			}
		})
	}

	for _, bad := range []struct{ name, raw string }{
		{"no separator", created},
		{"neither stamp readable", "not-a-time|also-not-a-time"},
		{"tag stamp unset and .Created unreadable", zeroTag + "|garbage"},
	} {
		t.Run("refuses: "+bad.name, func(t *testing.T) {
			if got, err := pickBuildTime(bad.raw); err == nil {
				t.Errorf("pickBuildTime(%q) = %s with no error. A zero or invented timestamp "+
					"compares older than every source file, which turns the freshness axis "+
					"off silently - exactly the shape this check refuses everywhere else.",
					bad.raw, got)
			}
		})
	}
}

// TestBuildTimeComesFromPickBuildTime - the join.
//
// pickBuildTime can be perfect and reached by nobody: the previous version of
// this file parsed `docker image inspect -f {{.Created}}` inline, and putting
// that back is a two-line edit that no truth table above would notice. The
// format string is checked here too, because it is where the field name
// actually lives.
func TestBuildTimeComesFromPickBuildTime(t *testing.T) {
	if !strings.Contains(buildTimeFormat, ".Metadata.LastTagTime") {
		t.Errorf("buildTimeFormat is %q and does not ask for .Metadata.LastTagTime. Whatever it "+
			"asks for instead, if it is .Created alone the axis reads a timestamp BuildKit "+
			"reproduces across rebuilds and can never redden.", buildTimeFormat)
	}
	if !strings.Contains(buildTimeFormat, ".Created") {
		t.Errorf("buildTimeFormat is %q and has no .Created fallback. A pulled image has no "+
			"LastTagTime, and without the fallback L3c cannot date it at all.", buildTimeFormat)
	}

	fset := token.NewFileSet()
	fn, file := harnessFuncIn(t, fset, "collectKeeperImageProvenance")
	if fn == nil {
		t.Fatal("collectKeeperImageProvenance not found - if it was renamed, this guard has to follow it")
	}
	if !callsNamed(fn.Body, "pickBuildTime") {
		t.Errorf("%s: collectKeeperImageProvenance does not call pickBuildTime. The choice between "+
			"the two stamps is being made somewhere this test cannot see, and the BuildKit-frozen "+
			"`.Created` is the field an inline parse reaches for.", file)
	}
}

// TestTheCheckedImageIsTheDeployedImage - one tag, named once.
//
// The failure this forecloses reports having looked: a pre-flight that vouches
// for `keeper:e2e-k8s` while DeployKeeper loads `keeper:latest` is green,
// silent, and about the wrong artifact - NIM-490 reintroduced by its own fix,
// and harder to spot the second time because a check now exists. The two sites
// must name the same constant; two string literals agree until one is edited.
func TestTheCheckedImageIsTheDeployedImage(t *testing.T) {
	fset := token.NewFileSet()
	newStack, _ := harnessFuncIn(t, fset, "NewStack")
	deployKeeper := harnessMethod(t, fset, "DeployKeeper")

	for _, c := range []struct {
		fn   *ast.FuncDecl
		what string // the call whose image argument is under inspection
		site string
	}{
		{newStack, "assertKeeperImageMatchesTree", "the pre-flight"},
		{deployKeeper, "LoadDockerImage", "the deploy step"},
	} {
		args := argsOfCall(c.fn.Body, c.what)
		if args == nil {
			t.Errorf("%s does not call %s at all", c.fn.Name.Name, c.what)
			continue
		}
		if !mentionsIdent(args, "keeperE2EImage") {
			t.Errorf("%s: %s passes %s an image that is not keeperE2EImage. Both sites have to "+
				"name the one constant - otherwise the pre-flight can vouch for one tag while "+
				"kind loads another, and the tier reports a verdict about an image it never "+
				"examined.", c.fn.Name.Name, c.site, c.what)
		}
	}
}

// TestTheDeploymentManifestNamesTheCheckedImage - the third site, the one the
// constant cannot reach.
//
// Go code can be made to name keeperE2EImage once; committed YAML cannot
// reference a Go const, so manifests/keeper/deployment.yaml carries the tag as a
// literal and nothing but this test ties the two together.
//
// Weaker than its neighbour above, and the difference is worth stating rather
// than glossing: with `imagePullPolicy: Never` a drifted tag usually surfaces as
// ErrImageNeverPull and a stand that never comes up - loud, not silent. It turns
// quiet only when the other tag also happens to be loaded into the node, and
// then the tier vets one image and runs another. The guard is here for that
// case and to keep the coupling written down; it is not the headline defect.
func TestTheDeploymentManifestNamesTheCheckedImage(t *testing.T) {
	rel := filepath.Join("..", "manifests", "keeper", "deployment.yaml")
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v. If the manifest moved, move this guard with it - a guard that "+
			"cannot find its subject proves nothing about the subject.", rel, err)
	}

	repo, _, _ := strings.Cut(keeperE2EImage, ":")
	var named []string
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.TrimSpace(line)
		if !strings.HasPrefix(f, "image:") {
			continue
		}
		img := strings.TrimSpace(strings.TrimPrefix(f, "image:"))
		if r, _, _ := strings.Cut(img, ":"); r == repo {
			named = append(named, img)
		}
	}

	switch {
	case len(named) == 0:
		t.Errorf("%s names no %s image at all. Either the deployment stopped running the keeper "+
			"under test, or the repository was renamed and this guard now scans for a prefix "+
			"nothing matches - which passes by vacuum.", rel, repo)
	case len(named) != 1 || named[0] != keeperE2EImage:
		t.Errorf("%s names %v; keeperE2EImage is %q. The pre-flight vets keeperE2EImage and kind "+
			"loads keeperE2EImage, so a manifest pointing anywhere else deploys an artifact this "+
			"tier never examined.", rel, named, keeperE2EImage)
	}
}

// TestToolingSkipPrecedesTheProvenanceFailure - environment first, subject second.
//
// The order carries the whole distinction between "cannot run this tier" and
// "must not trust this run". A machine with no docker has to SKIP: it cannot
// execute L3c at all, and reddening it would turn every laptop and every
// non-k8s CI lane permanently red. A machine that has docker and a stale image
// has to FAIL. Put the image check first and the first case collapses into the
// second, because collecting provenance itself needs docker.
func TestToolingSkipPrecedesTheProvenanceFailure(t *testing.T) {
	fset := token.NewFileSet()
	fn, file := harnessFuncIn(t, fset, "NewStack")

	pos := callPositions(fn.Body)
	tooling, ok := pos["requireClusterTooling"]
	if !ok {
		t.Fatalf("%s: NewStack does not call requireClusterTooling. Without it a machine with no "+
			"docker gets a provenance failure instead of a skip - the tier stops being skippable.", file)
	}
	check, ok := pos["assertKeeperImageMatchesTree"]
	if !ok {
		t.Fatalf("%s: NewStack does not call assertKeeperImageMatchesTree at all", file)
	}
	if tooling > check {
		t.Errorf("%s: NewStack checks the image (line %d) before checking for docker/kind (line %d). "+
			"On a machine without docker that is a FAILURE where the contract says SKIP, and the "+
			"failure text talks about a stale image when the real answer is `docker is not installed`.",
			file, fset.Position(check).Line, fset.Position(tooling).Line)
	}
}

// argsOfCall returns the arguments of the first call to `name` in this body,
// or nil when there is none. A zero-argument call returns a non-nil empty
// slice, so "not called" and "called with nothing" stay distinguishable.
func argsOfCall(body *ast.BlockStmt, name string) []ast.Expr {
	var args []ast.Expr
	ast.Inspect(body, func(n ast.Node) bool {
		if args != nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != name {
			return true
		}
		if args = call.Args; args == nil {
			args = []ast.Expr{}
		}
		return false
	})
	return args
}

// mentionsIdent - is `name` among these arguments?
func mentionsIdent(args []ast.Expr, name string) bool {
	for _, a := range args {
		if id, ok := a.(*ast.Ident); ok && id.Name == name {
			return true
		}
	}
	return false
}

// callPositions maps each called name to the position of its FIRST call.
func callPositions(body *ast.BlockStmt) map[string]token.Pos {
	out := map[string]token.Pos{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name := calleeName(call); name != "" {
			if _, seen := out[name]; !seen {
				out[name] = call.Pos()
			}
		}
		return true
	})
	return out
}

// calleeName - the called function's own name, bare (f) or selected (x.f).
func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// TestProvenanceFailureIsFatalNotSkip - a stale image must stop the run.
//
// Skipf here would recreate the exact defect NIM-490 is about, one level up: a
// skipped test prints a line nobody reads and the suite still exits 0.
func TestProvenanceFailureIsFatalNotSkip(t *testing.T) {
	fn := harnessFuncs(t)["assertKeeperImageMatchesTree"]
	if fn == nil {
		t.Fatalf("assertKeeperImageMatchesTree not found in the harness sources")
	}
	// Errorf belongs in this list, not in a branch of its own. Guarding it as
	// `Errorf && !Fatalf` passes the moment the function does both - reports the
	// stale image with Errorf and Fatalfs for some unrelated reason - and the
	// cluster still comes up on the wrong image. The property is that the
	// refusal is Fatal, so anything softer is wrong regardless of what else the
	// body does. L3a and L3b spell it exactly this way.
	for _, soft := range []string{"Skip", "Skipf", "SkipNow", "Errorf"} {
		if callsNamed(fn.Body, soft) {
			t.Errorf("assertKeeperImageMatchesTree calls %s. A stale image has to stop the run: "+
				"anything short of Fatalf lets the cluster come up anyway and the tier goes on to "+
				"report a verdict about the wrong code.", soft)
		}
	}
	if !callsNamed(fn.Body, "Fatalf") {
		t.Errorf("assertKeeperImageMatchesTree never calls Fatalf - nothing stops the run")
	}
}

// TestProvenancePrecedesTheClusterBuild - the check runs before kind does.
//
// Not a style point: building the kind cluster takes minutes, and the verdict
// is already known before any of it. A pre-flight placed after it is correct
// and useless, which is how it ends up disabled.
func TestProvenancePrecedesTheClusterBuild(t *testing.T) {
	fset := token.NewFileSet()
	fn, file := harnessFuncIn(t, fset, "NewStack")

	pos := callPositions(fn.Body)
	check, ok := pos["assertKeeperImageMatchesTree"]
	if !ok {
		t.Fatalf("%s: NewStack does not call assertKeeperImageMatchesTree at all", file)
	}
	cluster, ok := pos["NewCluster"]
	if !ok {
		// Fatal, not Skip. "Nothing to order against" is indistinguishable from
		// "the anchor was renamed", and a skip here prints `ok` while the only
		// guard on the ordering silently stops existing - the same false green
		// one level up that this whole file is about. Renaming NewCluster is a
		// one-line fix to this line; discovering months later that the check
		// never ran is not.
		t.Fatalf("%s: NewStack no longer calls NewCluster, so nothing anchors the ordering "+
			"guard. Point it at whatever builds the cluster now - until then the pre-flight "+
			"can drift below the cluster build with no test to say so.", file)
	}
	if check > cluster {
		t.Errorf("%s: NewStack builds the kind cluster (line %d) before checking the image "+
			"(line %d). The answer is known before kind starts, so this spends minutes to "+
			"reach a refusal it could have printed immediately.",
			file, fset.Position(cluster).Line, fset.Position(check).Line)
	}
}

// harnessFuncs parses the harness's non-test sources. Tagged files are invisible
// to the untagged parse used elsewhere, so this reads them as plain source: the
// guards must see NewStack even though it carries //go:build e2e_k8s.
func harnessFuncs(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	out, _ := harnessFuncsFset(t, fset)
	return out
}

func harnessFuncIn(t *testing.T, fset *token.FileSet, name string) (*ast.FuncDecl, string) {
	t.Helper()
	funcs, files := harnessFuncsFset(t, fset)
	fn := funcs[name]
	if fn == nil {
		t.Fatalf("%s not found in the harness sources - if it was renamed, this guard has to follow it", name)
	}
	return fn, files[name]
}

// harnessMethod finds a METHOD by name. harnessFuncsFset deliberately collects
// only free functions - the other guards are about those - and DeployKeeper
// hangs off *Stack, so it needs its own lookup rather than a looser shared one.
func harnessMethod(t *testing.T, fset *token.FileSet, name string) *ast.FuncDecl {
	t.Helper()
	for _, file := range harnessSourceFiles(t) {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Body != nil && fn.Recv != nil && fn.Name.Name == name {
				return fn
			}
		}
	}
	t.Fatalf("method %s not found in the harness sources - if it was renamed or moved, this "+
		"guard has to follow it rather than pass by finding nothing", name)
	return nil
}

func harnessFuncsFset(t *testing.T, fset *token.FileSet) (map[string]*ast.FuncDecl, map[string]string) {
	t.Helper()
	funcs := map[string]*ast.FuncDecl{}
	files := map[string]string{}
	for _, name := range harnessSourceFiles(t) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil {
				continue
			}
			funcs[fn.Name.Name] = fn
			files[fn.Name.Name] = name
		}
	}
	if len(funcs) == 0 {
		t.Fatalf("parsed no functions out of the harness sources - the guards below would then " +
			"pass by finding nothing to check")
	}
	return funcs, files
}

// harnessSourceFiles - the harness's non-test .go files, tagged ones included.
func harnessSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read harness dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		t.Fatalf("no harness sources found in the working directory - every guard below would " +
			"then pass by having nothing to inspect")
	}
	return out
}

// callsNamed - does this body call `name`, bare or as a selector (t.Fatalf)?
func callsNamed(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			if f.Name == name {
				found = true
			}
		case *ast.SelectorExpr:
			if f.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}
