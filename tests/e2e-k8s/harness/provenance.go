package harness

// Untagged on purpose (the setupdecl.go pattern in tests/e2e-live): a check
// this load-bearing needs guards that run without docker, kind or the e2e_k8s
// tag, so `go test ./harness/` in check-e2e-set can execute them.

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

// L3c has the same hole as L3a and L3b (NIM-490), one artifact over: nothing
// here builds keeper, the tier deploys the `keeper:e2e-k8s` IMAGE, and
// `kind load docker-image` loads whatever image of that name the local daemon
// happens to hold. `make e2e-k8s` is safe by construction - it depends on
// docker-build-keeper -> build-linux, and the Dockerfile COPYs the artifact
// that step just produced - but a bare `go test -tags=e2e_k8s`, which is how
// a single L3c test gets re-run, deploys an image that may predate the tree by
// weeks. Nothing said so.
//
// The trap here is worse than L3a's in one respect: `make build` does not fix
// it. The native binary is not what runs in the cluster, so a developer who
// rebuilds the obvious thing and re-runs is still testing the old image. The
// message below says which command actually rebuilds the subject.

// keeperSourceRoots - the directories whose contents end up inside the keeper
// image (keeper/go.mod plus the workspace modules it requires, ADR-011). Not
// `soul`, `soul-lint`, `tests` or `manifests`: an edit there cannot change what
// the keeper container executes, and a check that reddens on unrelated edits is
// a check people learn to bypass.
var keeperSourceRoots = []string{"keeper", "shared", "sdk", "proto"}

// keeperE2EImage - the image L3c deploys, named once. The pre-flight and
// `kind load docker-image` MUST agree on it: a check that vouches for one tag
// while the cluster runs another is worse than no check, because it reports
// having looked.
const keeperE2EImage = "keeper:e2e-k8s"

// imageProvenance - what the harness can learn about the image it is about to
// deploy and the tree it is supposed to be testing. Separated from its
// gathering so the verdict is a pure function with its own known-bad cases.
type imageProvenance struct {
	image   string
	created time.Time

	// reported - the version the image's keeper prints; expected - the
	// version the tree would stamp into an image built right now.
	reported string
	expected string

	// newestSrc - the youngest UNCOMMITTED file under keeperSourceRoots, and
	// its mtime. Empty when nothing there differs from the commit.
	newestSrc      string
	newestSrcMTime time.Time
}

// staleReason returns "" when the image provably carries the code in the tree,
// and otherwise the reason it does not. The two axes are the ones L3a uses and
// they split for the same reasons - see the long comment in
// tests/e2e/harness/provenance.go; only the remedy differs, because rebuilding
// the subject here means rebuilding an image.
func (p imageProvenance) staleReason() string {
	if commitOf(p.reported) != commitOf(p.expected) {
		return fmt.Sprintf(
			"the %s image is STALE - this run would test code that is not in the tree.\n"+
				"  image:    %s  (built %s)\n"+
				"  it says:  %s\n"+
				"  tree is:  %s\n"+
				"Rebuild the IMAGE: `make docker-build-keeper`. `make e2e-k8s` does that for you;\n"+
				"a bare `go test -tags=e2e_k8s` does not, and neither does `make build` - that one\n"+
				"builds the host binary, which is not what runs in the cluster.",
			p.image, p.image, p.created.Format(time.RFC3339), p.reported, p.expected)
	}

	if p.newestSrc != "" && p.newestSrcMTime.After(p.created) {
		return fmt.Sprintf(
			"the %s image is STALE - an uncommitted source file is younger than it is.\n"+
				"  image:         %s  (built %s)\n"+
				"  newer source:  %s  (edited %s)\n"+
				"The version cannot tell these apart - the change is not committed, so an image\n"+
				"with it and an image without it both describe as %s. The timestamps can, and\n"+
				"they say the edit came after the build.\n"+
				"Rebuild the IMAGE: `make docker-build-keeper` (NOT `make build` - that builds the\n"+
				"host binary, and the cluster runs the image).",
			p.image, p.image, p.created.Format(time.RFC3339),
			p.newestSrc, p.newestSrcMTime.Format(time.RFC3339),
			commitOf(p.expected))
	}

	return ""
}

// commitOf drops the `-dirty` marker before comparing. The marker is
// repo-wide: it says only that SOMETHING is uncommitted, which a README edit
// satisfies, and reddening a correct image over a README is how a check earns
// its reputation for crying wolf. What the marker hides - an uncommitted change
// to the keeper's own sources - is what the freshness axis is for.
func commitOf(version string) string { return strings.TrimSuffix(version, "-dirty") }

// assertKeeperImageMatchesTree - the pre-flight. Fatal, never Skip: the tier's
// entire claim is that it exercises this tree in kubernetes, and a run that
// cannot stand behind that claim is worth less than no run, because it reports
// a verdict anyway.
//
// A missing image is fatal too, and that is not a change of stance: today the
// absence surfaces two minutes later as `kind load docker-image` failing, after
// a kind cluster has been built for nothing. Saying it here, by name, is the
// same verdict delivered earlier.
func assertKeeperImageMatchesTree(t *testing.T, tier, image string) {
	t.Helper()

	p, err := collectKeeperImageProvenance(image)
	if err != nil {
		t.Fatalf("%s: cannot establish which code the %s image carries: %v\n"+
			"  Hard failure on purpose - a tier that cannot name its own subject proves nothing.\n"+
			"  If the image is simply not built yet: `make docker-build-keeper`.\n"+
			"  If this environment has no git (a tarball, a container), state the build explicitly:\n"+
			"  KEEPER_EXPECTED_VERSION=<the version you believe you built> go test -tags=e2e_k8s ...\n"+
			"  Write the version out. Deriving it from the image (`$(docker run ... version)`)\n"+
			"  makes the comparison compare the image with itself, which passes always.",
			tier, image, err)
	}
	if reason := p.staleReason(); reason != "" {
		t.Fatalf("%s: %s", tier, reason)
	}
}

// collectKeeperImageProvenance gathers the facts staleReason judges.
//
// KEEPER_EXPECTED_VERSION does not switch the check off, it makes the caller
// SAY which build they mean. Set by hand it also covers the no-git case, and
// there it costs the freshness axis: without git there is no way to know what
// is uncommitted.
func collectKeeperImageProvenance(image string) (imageProvenance, error) {
	p := imageProvenance{image: image}

	stampsRaw, err := dockerInspectBuildTime(image)
	if err != nil {
		return p, err
	}
	if p.created, err = pickBuildTime(stampsRaw); err != nil {
		return p, err
	}

	if p.reported, err = keeperVersionFromImage(image); err != nil {
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
		// rather than guessing.
		return p, nil
	}

	if p.newestSrc, p.newestSrcMTime, err = newestUncommittedKeeperSource(root); err != nil {
		return p, err
	}
	return p, nil
}

// buildTimeFormat - the two stamps, asked for in one call so pickBuildTime can
// choose between them without a second round trip to the daemon.
const buildTimeFormat = "{{.Metadata.LastTagTime}}|{{.Created}}"

// dockerInspectBuildTime - when this image last became the thing that tag names.
//
// NOT `.Created` alone, and this is the one place in the file where the obvious
// field is the wrong one. Under BuildKit `.Created` is part of the image CONFIG
// and is reproduced from the build inputs, so rebuilding an unchanged-enough
// Dockerfile hands back the SAME creation timestamp - measured here across three
// consecutive `make docker-build-keeper` runs whose image IDs all differed and
// whose `.Created` was identical to the nanosecond, including with a
// deliberately changing `--label`. A freshness axis reading that field compares
// an edit against a clock that stopped, which is the whole defect this check
// exists to prevent, reproduced inside the check.
//
// `Metadata.LastTagTime` is daemon-local bookkeeping - when the local daemon last
// pointed this tag at an image - and it does move on every rebuild, because
// `docker build -t` re-tags. It is zero for an image that was pulled rather than
// built and tagged here, and pickBuildTime falls back to `.Created` there: for a
// pulled image the registry's creation time is the only stamp there is, and it
// is not being frozen by a local rebuild.
func dockerInspectBuildTime(image string) (string, error) {
	out, err := exec.Command("docker", "image", "inspect", "-f", buildTimeFormat, image).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker image inspect %s: %w (output: %s)",
			image, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// pickBuildTime turns dockerInspectBuildTime's two stamps into the one this
// harness compares against. Pure, so the choice has its own known-bad cases
// rather than being provable only by building images.
//
// Go's zero time is what the daemon prints for an unset LastTagTime, in either
// of the two shapes Go's template printer produces for a time.Time.
func pickBuildTime(raw string) (time.Time, error) {
	lastTag, created, ok := strings.Cut(raw, "|")
	if !ok {
		return time.Time{}, fmt.Errorf(
			"`docker image inspect -f %s` printed %q, which has no separator in it - "+
				"the format and this parser have drifted apart", buildTimeFormat, raw)
	}

	if t, err := parseDockerTime(lastTag); err == nil && !t.IsZero() {
		return t, nil
	}
	t, err := parseDockerTime(created)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"neither stamp in %q is a time this harness can read (%w). Without one there is "+
				"no freshness axis, and an uncommitted edit made after the build would go "+
				"unnoticed", raw, err)
	}
	return t, nil
}

// parseDockerTime reads the two shapes the daemon's `-f` output can take: RFC3339
// for a real stamp, and Go's default time.Time rendering for a zero one.
func parseDockerTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", s)
}

// keeperVersionFromImage asks the image's own keeper which code it carries.
// The entrypoint is overridden rather than relying on CMD, because the default
// CMD is `run --config ...` and would start a daemon.
func keeperVersionFromImage(image string) (string, error) {
	out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "/keeper", image, "version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker run %s /keeper version: %w (output: %s)",
			image, err, strings.TrimSpace(string(out)))
	}
	return parseKeeperVersion(string(out))
}

// parseKeeperVersion reads the version out of `keeper version`, whose format is
// fixed by keeper/cmd/keeper/main.go: "keeper <version> (<goversion>)".
func parseKeeperVersion(out string) (string, error) {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	rest, ok := strings.CutPrefix(line, "keeper ")
	if !ok {
		return "", fmt.Errorf("`keeper version` printed %q, which is not the documented "+
			"`keeper <version> (<goversion>)` - is this a keeper image at all?", line)
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
// check - if they ever disagree, the disagreement must be about the image and
// nothing else.
func gitDescribe() (string, error) {
	out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return "", fmt.Errorf("git describe --tags --always --dirty: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitTopLevel - the repo root, asked rather than derived.
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
// leaves BOTH axes quiet on an image that still contains code the tree no
// longer has - the exact shape this check exists to refuse. The timestamp is the
// containing directory's: unlink() updates it. See mtimeOfEntry.
func newestUncommittedKeeperSource(root string) (string, time.Time, error) {
	args := append([]string{"status", "--porcelain", "-z", "--untracked-files=all", "--"}, keeperSourceRoots...)
	cmd := exec.Command("git", args...)
	// From root, so the pathspecs above mean the repo's keeper/ and not
	// tests/e2e-k8s/harness/keeper/. A pathspec is resolved against the working
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
			"cannot tell whether the keeper image predates the change", path, root)
}
