package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// deletedArtifactFilenames — filenames a plugin artifact once published and no
// longer does. NIM-377 replaced the hand-written manifest.yaml with the generated
// schema document, which rides in the artifact as a stamped trailer and is
// published beside it as [schema.SchemaFileName].
//
// Matched as a substring on purpose: the same file travelled to a staging host
// under a decorated name (`mod-manifest.yaml`), and a consumer requiring THAT is
// wrong for exactly the same reason.
//
// The match is still literal, so a script assembling the name from parts at run
// time slips through. Every consumer of the artifact model in this tree spells it
// out — the sweep covers the idiom the repository actually writes, not every one
// imaginable.
var deletedArtifactFilenames = []string{"manifest.yaml"}

// committedScriptGlobs — the surfaces this sweep covers: recipes and scripts,
// where a filename lives as an unchecked string in a file no test binary loads
// and no compiler reads, which is the whole mechanism of the bug below. CI
// workflows and the Makefile are in for the same reason as shell — a `run:` block
// and a recipe line are shell that happens to live in another file. Go consumers
// of the artifact model are held by the guards in this package that read
// `sdk/schema` instead of restating it.
var committedScriptGlobs = []string{
	"*.sh",
	"*.sh.tmpl",
	"*.py",
	"Makefile",
	".github/workflows/*.yml",
}

// sweptAnchors — one real file per surface in [committedScriptGlobs], asserted to
// be inside the corpus.
//
// The glob list cannot check itself: drop `*.sh` and the remaining surfaces keep
// the corpus non-empty, so a count-based check stays green while the shell files —
// where every instance of this bug has actually lived — fall outside it. Naming
// real paths is what makes that visible. `dev/provision.sh` and
// `scripts/e2e-cloud/lib/preflight.sh` carried the defect; the rest are ordinary
// representatives of their surface, picked because they are load-bearing enough
// that retiring one is a deliberate act, which a silent narrowing is not.
var sweptAnchors = []string{
	"dev/provision.sh",
	"scripts/e2e-cloud/lib/preflight.sh",
	"examples/destiny/node-exporter/templates/smartmon.sh.tmpl",
	"scripts/check-doc-links.py",
	"Makefile",
	".github/workflows/ci.yml",
}

// TestNoCommittedScriptNamesADeletedArtifactFile — no committed script requires a
// file the artifact model has deleted.
//
// This is the shared gate NIM-520 asks for, and the reason it is repo-wide rather
// than per-consumer. NIM-377 deleted manifest.yaml; the consumers that named it
// were a LIST, and the list had no single gate. Two of them were found by hand,
// months apart, each time by somebody who had lost a day to it: dev/provision.sh
// killed `make dev-provision` before the service registry was ever seeded
// (NIM-516), and the L3b harness killed three live tests at setup while reporting
// it as "the stand didn't come up" (NIM-515). Each fix came with a guard over the
// consumer that had just been found, so the class stayed open and the third
// consumer stayed broken: scripts/e2e-cloud/lib/preflight.sh went on requiring
// `mod-manifest.yaml` in $ARTIFACTS_DIR, a hard preflight FAIL on a file that
// cannot exist any more.
//
// A per-consumer guard cannot close this. The defect is not in any one script —
// it is that renaming a file in the artifact reddens NOBODY until a person runs
// the right thing by hand. So the sweep is over every committed script, and a new
// consumer is covered the day it is committed rather than the day it breaks. Not
// the day it is written: the corpus comes from the index (see [committedScripts]),
// so a new script stays invisible here until it is staged.
func TestNoCommittedScriptNamesADeletedArtifactFile(t *testing.T) {
	root := repoRoot(t)

	// The needle list is the other half of the corpus check below: emptied, or
	// over-narrowed while somebody chases a false positive, the loop over the
	// scripts runs and asserts nothing while the tier still reads green.
	if len(deletedArtifactFilenames) == 0 {
		t.Fatal("deletedArtifactFilenames is empty — the sweep would read every committed script and " +
			"check none of them, which is the same green as a sweep that found nothing wrong")
	}

	scripts := committedScripts(t, root)

	swept := make(map[string]bool, len(scripts))
	for _, script := range scripts {
		swept[script] = true
	}
	for _, anchor := range sweptAnchors {
		if !swept[anchor] {
			t.Fatalf("the sweep does not reach %s — a script this guard exists to cover is outside the "+
				"corpus %v matched, so the check below runs over less than it claims while reading green",
				anchor, committedScriptGlobs)
		}
	}

	for _, script := range scripts {
		raw, err := os.ReadFile(filepath.Join(root, script))
		if err != nil {
			t.Errorf("read %s: %v", script, err)
			continue
		}
		for i, line := range strings.Split(string(raw), "\n") {
			// Comments are not acts, and every surface here marks them with `#`.
			// dev/provision.sh:621,629 say in prose that they no longer touch the
			// deleted file — the exact sentence that would otherwise fail the check
			// asserting it.
			//
			// Only whole-line comments are dropped: a trailing `#` cannot be told from
			// a `#` inside a string without parsing the shell, and Python's other
			// comment form — a docstring — is not marked per line at all, so prose
			// inside one is read as code. This guard fails toward noise rather than
			// toward silence; the answer to a false positive is to reword the line,
			// never to shorten [deletedArtifactFilenames], which weakens it for every
			// file at once.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			for _, dead := range deletedArtifactFilenames {
				if !strings.Contains(line, dead) {
					continue
				}
				t.Errorf("%s:%d contains %q, a name no plugin artifact publishes since NIM-377:\n\t%s\n"+
					"\tthe module's contract is the generated document — stamped into the artifact as a "+
					"trailer and published beside it as %s — so a consumer requiring the old name fails on "+
					"a file that cannot exist, and does it at the worst moment: this is a script, so nothing "+
					"compiles it and the failure waits until somebody runs it by hand",
					script, i+1, dead, strings.TrimSpace(line), schema.SchemaFileName)
			}
		}
	}
}

// committedScripts returns the repo-relative paths of every committed script the
// sweep covers, in a stable order (git ls-files sorts, the globs are asked in a
// fixed order, and this gate's output must not move between runs on an unchanged
// tree).
//
// Committed rather than walked: the subject is what this repository ships to the
// next person, and a walk would also read build output and an operator's
// untracked scratch files.
//
// Each glob is required to match something SEPARATELY. A check over the union
// only catches total collapse: drop `*.sh` and the Python files still make the
// total non-zero, so the sweep goes on reading green with the entire shell
// surface — where this bug actually lived — silently outside it.
func committedScripts(t *testing.T, root string) []string {
	t.Helper()

	scripts := make([]string, 0, 64)
	for _, glob := range committedScriptGlobs {
		matched := committedFilesMatching(t, root, glob)
		if len(matched) == 0 {
			t.Fatalf("git ls-files %q matched no committed file under %s — either that surface moved or "+
				"this guard stopped matching it, and both leave the sweep green while it covers less than "+
				"it claims", glob, root)
		}
		scripts = append(scripts, matched...)
	}
	return scripts
}

// committedFilesMatching asks git for one pathspec. The pattern reaches git
// literally — there is no shell here to expand it — and a git pathspec without
// `:(glob)` magic matches across directory separators, so `*.sh` covers every
// depth rather than the repo root alone.
func committedFilesMatching(t *testing.T, root, glob string) []string {
	t.Helper()

	cmd := exec.Command("git", "ls-files", "-z", "--", glob)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files %q in %s: %v — this guard reads the committed script corpus and "+
			"cannot report on one it could not enumerate", glob, root, err)
	}

	names := make([]string, 0, 32)
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}
