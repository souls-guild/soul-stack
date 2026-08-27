//go:build e2e

package harness

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The harness does not build keeper — it runs a file. Which file, and which
// code is inside it, was until NIM-490 nobody's question: `locateKeeperBinary`
// answers "does the path exist", `make e2e` did not depend on `build`, and a
// bare `go test -tags=e2e` never built anything at all. So the tier ran
// whatever the last `make build` happened to leave behind, and reported the
// verdict as if it were about the tree.
//
// That is not a hypothetical. On NIM-456 it fired twice in one session: wiring
// deliberately deleted from daemon.go left the tier GREEN, and `make build`
// then turned the same tier RED; and an L3a case "reproduced" a defect that
// source had already fixed, because the response it read came from yesterday's
// binary. Neither run said anything unusual. Absence of the binary is loud
// (t.Skipf in NewStack), staleness was silent — the same asymmetry NIM-342
// found in the dev stand.
//
// This file is the missing half: before a single container comes up, the
// harness asks the binary which code it carries and refuses to run if the
// answer is not this tree.

// keeperSourceRoots — the directories whose contents end up inside the keeper
// binary: its own module plus the workspace modules it requires (ADR-011,
// keeper/go.mod). `soul`, `soul-lint`, `soulctl` and `tests` are deliberately
// absent — editing them cannot change what `keeper/bin/keeper` does, and a
// check that reddens on unrelated edits is a check people learn to bypass.
var keeperSourceRoots = []string{"keeper", "shared", "sdk", "proto"}

// provenance — everything the harness could learn about the binary it is about
// to run and the tree it is supposed to be testing. Split out from its
// gathering so the verdict is a pure function with its own known-bad cases
// (provenance_test.go): a rule this load-bearing cannot be tested only by
// standing up three containers and hoping.
type provenance struct {
	binaryPath  string
	binaryMTime time.Time

	// reported — the version the binary prints; expected — the version the
	// tree would stamp into a binary built right now.
	reported string
	expected string

	// newestSrc — the youngest UNCOMMITTED file under keeperSourceRoots, and
	// its mtime. Empty when nothing there differs from the commit.
	newestSrc      string
	newestSrcMTime time.Time
}

// staleReason returns "" when the binary provably carries the code in the
// tree, and otherwise the reason it does not, phrased for whoever is about to
// read a red they did not expect.
//
// Two axes, because either one alone is blind where the other sees:
//
//   - Commit. `make build` stamps `git describe` into the binary
//     (KEEPER_LDFLAGS), so a binary whose version names another commit — or a
//     bare `go build` with no ldflags, which reports `0.0.0-dev` — is visibly
//     not from here. The `-dirty` marker is stripped off both sides before
//     comparing: it says only that SOMETHING in the repo is uncommitted, which
//     a README edit satisfies, and reddening a correct binary over a README is
//     how a check earns its reputation for crying wolf.
//
//   - Freshness of what is uncommitted. Once the marker is off, two binaries
//     from the same commit compare equal no matter which uncommitted edits
//     were in them — and that is precisely the state a developer is in when
//     they change a file and re-run the tier, the NIM-456 shape. Committed
//     files need no timestamp: the commit already vouches for them. So the
//     only files worth timing are the ones git reports as differing from it,
//     and there the evidence is conclusive in the direction that matters — an
//     edit younger than the binary cannot be inside it.
//
// Scoping the timestamps to uncommitted files is what keeps this from crying
// wolf: `git checkout` and `git rebase` stamp "now" onto every file they
// rewrite, so a whole-tree comparison would call a byte-for-byte correct binary
// stale after an ordinary branch switch.
//
// The one shape this does not catch: an edit that was built INTO the binary and
// then reverted. Nothing is uncommitted afterwards, so nothing is timed, and
// the commit matches. `make e2e` closes it by building; a bare `go test` after
// a revert is the residue, and it is a narrower hole than the one it replaced.
func (p provenance) staleReason() string {
	if commitOf(p.reported) != commitOf(p.expected) {
		return fmt.Sprintf(
			"the keeper binary is STALE - this run would test code that is not in the tree.\n"+
				"  binary:   %s\n"+
				"  it says:  %s\n"+
				"  tree is:  %s\n"+
				"Rebuild and re-run: `make build`. `make e2e` does that for you;\n"+
				"a bare `go test -tags=e2e` does not, which is how this is normally hit.\n"+
				"(A binary built by hand without -ldflags reports 0.0.0-dev - build through the Makefile.)",
			p.binaryPath, p.reported, p.expected)
	}

	if p.newestSrc != "" && p.newestSrcMTime.After(p.binaryMTime) {
		return fmt.Sprintf(
			"the keeper binary is STALE - an uncommitted source file is younger than it is.\n"+
				"  binary:        %s  (built %s)\n"+
				"  newer source:  %s  (edited %s)\n"+
				"The version cannot tell these apart - the change is not committed, so a binary\n"+
				"with it and a binary without it both describe as %s. The timestamps can, and\n"+
				"they say the edit came after the build.\n"+
				"Rebuild and re-run: `make build`.",
			p.binaryPath, p.binaryMTime.Format(time.RFC3339),
			p.newestSrc, p.newestSrcMTime.Format(time.RFC3339),
			commitOf(p.expected))
	}

	return ""
}

// commitOf drops the `-dirty` marker. See staleReason on why the two axes split
// exactly here.
func commitOf(version string) string { return strings.TrimSuffix(version, "-dirty") }

// assertKeeperBinaryMatchesTree — the pre-flight itself. Fatal, never Skip:
// a skip is what the old behaviour amounted to and it is read as "fine". The
// caller has asked for a tier whose entire claim is that it exercises this
// tree; if the harness cannot stand behind that claim, the run is worthless
// and must say so.
func assertKeeperBinaryMatchesTree(t *testing.T, tier, binaryPath string) {
	t.Helper()

	p, err := collectKeeperProvenance(binaryPath)
	if err != nil {
		t.Fatalf("%s: cannot establish which code the keeper binary carries: %v\n"+
			"  Hard failure on purpose - a tier that cannot name its own subject proves nothing.\n"+
			"  If this environment has no git (a tarball, a container), state the build explicitly:\n"+
			"  KEEPER_EXPECTED_VERSION=<the version you believe you built> go test -tags=e2e ...\n"+
			"  Write the version out. Deriving it from the binary (`$(keeper version | ...)`)\n"+
			"  makes the comparison compare the binary with itself, which passes always.",
			tier, err)
	}
	if reason := p.staleReason(); reason != "" {
		t.Fatalf("%s: %s", tier, reason)
	}
}

// collectKeeperProvenance gathers the facts staleReason judges.
//
// KEEPER_EXPECTED_VERSION is not an escape hatch in the usual sense: it does
// not switch the check off, it makes the caller SAY which build they mean. The
// Makefile sets it from $(VERSION) so that `make e2e VERSION=v1.2.3` - the
// release pipeline's shape - keeps one definition of the version instead of two
// that drift. Set by hand it also covers the no-git case, and there it costs
// the freshness axis: without git there is no way to know what is uncommitted.
func collectKeeperProvenance(binaryPath string) (provenance, error) {
	p := provenance{binaryPath: binaryPath}

	st, err := os.Stat(binaryPath)
	if err != nil {
		return p, fmt.Errorf("stat keeper binary: %w", err)
	}
	p.binaryMTime = st.ModTime()

	out, err := exec.Command(binaryPath, "version").CombinedOutput()
	if err != nil {
		return p, fmt.Errorf("run `%s version`: %w (output: %s)", binaryPath, err, strings.TrimSpace(string(out)))
	}
	if p.reported, err = parseKeeperVersion(string(out)); err != nil {
		return p, err
	}

	root, rootErr := gitTopLevel()
	described, describeErr := gitDescribe()
	gitOK := rootErr == nil && describeErr == nil

	switch v := os.Getenv("KEEPER_EXPECTED_VERSION"); {
	case v != "":
		p.expected = v
	case !gitOK:
		return p, errors.Join(rootErr, describeErr)
	default:
		p.expected = described
	}
	if !gitOK {
		// The caller named the build, so the commit axis still holds. The
		// freshness axis has nothing to stand on without git and stays off
		// rather than guessing - see staleReason.
		return p, nil
	}

	if p.newestSrc, p.newestSrcMTime, err = newestUncommittedKeeperSource(root); err != nil {
		return p, err
	}
	return p, nil
}

// parseKeeperVersion reads the version out of `keeper version`, whose format is
// fixed by cmd/keeper/main.go: "keeper <version> (<goversion>)".
func parseKeeperVersion(out string) (string, error) {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	rest, ok := strings.CutPrefix(line, "keeper ")
	if !ok {
		return "", fmt.Errorf("`keeper version` printed %q, which is not the documented "+
			"`keeper <version> (<goversion>)` - is this a keeper binary at all?", line)
	}
	if i := strings.LastIndex(rest, " ("); i >= 0 {
		rest = rest[:i]
	}
	if rest = strings.TrimSpace(rest); rest == "" {
		return "", fmt.Errorf("`keeper version` printed %q - no version in it", line)
	}
	return rest, nil
}

// gitDescribe mirrors the Makefile's VERSION, expression for expression. The
// two are one fact stated twice, and the duplication is the point of the whole
// check - if they ever disagree, the disagreement must be about the binary and
// nothing else.
func gitDescribe() (string, error) {
	out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return "", fmt.Errorf("git describe --tags --always --dirty: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitTopLevel — the repo root, asked rather than derived. `locateKeeperBinary`
// climbs a fixed number of levels from the working directory, which is right
// for a test in tests/e2e and wrong for anything else.
func gitTopLevel() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// newestUncommittedKeeperSource returns the youngest file under
// keeperSourceRoots that git reports as differing from the commit - modified,
// added, or untracked - relative to root. Empty name when there is none.
//
// Untracked counts: a brand-new file is the one edit `git describe --dirty`
// cannot see at all (it consults the index, not the worktree's strays), so
// without -uall a new keeper source would leave both axes quiet.
//
// Deletions count too, and reaching that took a correction. A deleted file has
// no timestamp of its own, so the obvious reading is that there is nothing to
// compare and the entry should be skipped. That is right about the file and
// wrong about the axis: an uncommitted deletion is invisible to the commit axis
// as well (`-dirty` is stripped from both sides on purpose), so skipping it here
// leaves BOTH axes quiet on a binary that still contains code the tree no longer
// has - the exact shape this check exists to refuse. The timestamp is the
// containing directory's: unlink() updates it. See mtimeOfEntry.
func newestUncommittedKeeperSource(root string) (string, time.Time, error) {
	args := append([]string{"status", "--porcelain", "-z", "--untracked-files=all", "--"}, keeperSourceRoots...)
	cmd := exec.Command("git", args...)
	// From root, so the pathspecs above mean the repo's keeper/ and not
	// tests/e2e/harness/keeper/. A pathspec is resolved against the working
	// directory, and one that matches nothing reports a clean tree - which here
	// would read as "nothing uncommitted" and switch the axis off in silence.
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("git status --porcelain in %s: %w", root, err)
	}

	var (
		newest     time.Time
		newestPath string
	)
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue // the trailing empty field, and anything malformed
		}
		x, y, path := e[0], e[1], e[3:]
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++ // a rename/copy carries its origin path as the next field
		}
		mt, skip, err := mtimeOfEntry(root, path)
		if err != nil {
			return "", time.Time{}, err
		}
		if skip {
			continue
		}
		if mt.After(newest) {
			newest, newestPath = mt, path
		}
	}
	return newestPath, newest, nil
}

// mtimeOfEntry - when the entry git listed last changed. skip is true for the
// one entry shape that is not an edit to a source file.
//
// Lstat, not Stat: git lists a symlink when the LINK changed, and the target's
// mtime answers a question nobody asked. It also means a dangling symlink is
// read as itself rather than falling through to the deleted case.
//
// When the entry is gone, the containing directory is the timestamp - unlink()
// updates the parent's mtime, which is precisely the fact needed here. The climb
// exists for a deleted directory, where the parent may be gone too; it ends at
// root, and a root that cannot be stat'd is a broken checkout rather than an
// edit, so it fails closed instead of returning a zero time that can never
// redden.
func mtimeOfEntry(root, path string) (mt time.Time, skip bool, err error) {
	full := filepath.Join(root, filepath.FromSlash(path))
	if info, err := os.Lstat(full); err == nil {
		// -uall lists files, but a submodule shows up as one entry, and its
		// directory mtime moves for reasons that are not edits to our sources.
		return info.ModTime(), info.IsDir(), nil
	}
	for p := filepath.Dir(full); len(p) >= len(root); p = filepath.Dir(p) {
		if info, err := os.Lstat(p); err == nil {
			return info.ModTime(), false, nil
		}
		if len(p) == len(root) {
			break
		}
	}
	return time.Time{}, false, fmt.Errorf(
		"%s is gone and neither it nor any parent up to %s can be stat'd; "+
			"cannot tell whether the keeper binary predates the change", path, root)
}
