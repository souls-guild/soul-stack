package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRoot returns the root of this repository, located by walking up from the
// working directory to the `go.work` that sits at the top of the tree (ADR-011).
//
// It used to be `wd/../..`, which is the root only for a test binary whose
// package is `tests/e2e-live`. The docker-free guards run as the `harness`
// package's own binary, one directory deeper, so no fixed number of `..` is
// right for both; the marker is right for both and for any package added later.
//
// This function is also the name the bring-up declaration guard keys on
// (setupdecl_test.go::repoReadingFuncs): whatever reaches it is reading THIS
// REPOSITORY, and a failure raised from there is a finding about the code, never
// about the machine. That is why there is one of these rather than one per
// caller — a second way to find the root would be invisible to the guard, which
// is how a deleted file in this repo came to be reported as "the stand didn't
// come up" on all nine gate tests (NIM-515).
//
// It is deliberately untagged. `repoRoot` is not an e2e_live-only idea and the
// guards need it without containers in the picture.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("repoRoot: getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repoRoot: walked up from the working directory without finding go.work")
		}
		dir = parent
	}
}
