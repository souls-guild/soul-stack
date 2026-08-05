package trial

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// examplesRoot — the shipped example trees, relative to this package.
const examplesRoot = "../../../examples"

// TestExamples_EveryCaseLoads — every `case.yml` shipped under examples/ parses
// and validates.
//
// Until this existed, the example trees had no gate of their own: a case file was
// only ever read if some hand-written test happened to name its path, and most of
// them are named by nothing. Out of the seventeen dragonfly cases exactly one was
// reachable that way. So a repo-wide edit to the fixtures — the vars migration is
// the immediate one, but any future rename lands the same way — could leave twelve
// of the thirteen service trees carrying a key the loader rejects, and the suite
// would stay green because it never opened them.
//
// Strict decode is what makes this worth having: LoadCase refuses unknown keys, so
// a fixture still writing a retired key name fails here by name and line rather
// than in front of whoever next runs that scenario.
//
// L1 (migration) and L2 (stand) cases have a different shape and are recognised by
// the same soft pre-parse the recursive runner uses; they are counted, not decoded
// as L0.
func TestExamples_EveryCaseLoads(t *testing.T) {
	root := requireExamples(t)

	var l0, other int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != caseFileName {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		l1, l1Err := isL1Case(path)
		if l1Err != nil {
			t.Errorf("examples/%s: pre-parse: %v", rel, l1Err)
			return nil
		}
		l2, l2Err := isL2Case(path)
		if l2Err != nil {
			t.Errorf("examples/%s: pre-parse: %v", rel, l2Err)
			return nil
		}
		if l1 || l2 {
			other++
			return nil
		}

		l0++
		if _, _, loadErr := LoadCase(path); loadErr != nil {
			t.Errorf("examples/%s does not load: %v", rel, loadErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// A walk that finds nothing would report success without having checked
	// anything — the same silent-skip this test exists to close.
	if l0 == 0 {
		t.Fatalf("no L0 cases found under %s (found %d L1/L2) — the walk matched nothing", root, other)
	}
	t.Logf("loaded %d L0 cases (%d L1/L2 skipped)", l0, other)
}

// requireExamples distinguishes "this checkout has no examples/ at all" — a task
// worktree may legitimately not carry them — from "examples/ is here but its
// layout disagrees with the loader", which is a failure and not a reason to skip.
func requireExamples(t *testing.T) string {
	t.Helper()

	info, err := os.Stat(examplesRoot)
	if os.IsNotExist(err) {
		t.Skipf("no %s in this checkout", examplesRoot)
	}
	if err != nil {
		t.Fatalf("stat %s: %v", examplesRoot, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s exists but is not a directory", examplesRoot)
	}

	for _, sub := range []string{"service", "destiny"} {
		if _, err := os.Stat(filepath.Join(examplesRoot, sub)); err != nil {
			t.Fatalf("%s/%s missing — examples/ is present but not in the expected layout: %v",
				examplesRoot, sub, err)
		}
	}
	return examplesRoot
}

// TestExamples_NoRetiredVarsKey — the fixtures do not carry the retired layer name.
//
// Separate from the load test on purpose. LoadCase catches `essence:` at the top
// level of a case because strict decode rejects it, but a fixture can also mention
// the retired name in a CEL expression (`default(essence.conf_dir, …)`) nested in
// an input value, where the decoder sees an ordinary string and passes it through.
// That renders to an error at run time, not at load time, so nothing above would
// notice it.
func TestExamples_NoRetiredVarsKey(t *testing.T) {
	root := requireExamples(t)

	var scanned int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == retiredVarsDir {
				t.Errorf("examples/%s: retired layer directory still present", relTo(root, path))
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".yml", ".yaml", ".tmpl":
		default:
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, retiredVarsRoot) {
				t.Errorf("examples/%s:%d references the retired root: %s",
					relTo(root, path), i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("no YAML or template files found under %s — the walk matched nothing", root)
	}
}

// retiredVarsDir / retiredVarsRoot — the pre-ADR-0082 names. A tree still carrying
// either one was migrated halfway.
const (
	retiredVarsDir  = "essence"
	retiredVarsRoot = "essence."
)

func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
