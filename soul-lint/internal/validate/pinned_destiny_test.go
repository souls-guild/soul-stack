package validate

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedDestinyDir is the tree of a destiny that left this repository, at the commit
// scripts/destiny-source.sh pins (NIM-1062). It fails the test rather than skipping
// it when the cache can be neither read nor filled.
func pinnedDestinyDir(t *testing.T, alias string) string {
	t.Helper()
	cmd := exec.Command(filepath.FromSlash("../../../scripts/destiny-source.sh"), "dir", alias)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	dir := strings.TrimSpace(string(out))
	if err != nil || dir == "" {
		t.Fatalf("scripts/destiny-source.sh dir %s: %v\n%s\n\t`make destiny-sources` fills the cache.", alias, err, stderr.String())
	}
	return dir
}
