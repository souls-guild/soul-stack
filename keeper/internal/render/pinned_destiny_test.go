package render

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type pinnedDestiny struct {
	dir string
	err error
}

var (
	pinnedDestiniesMu sync.Mutex
	pinnedDestinies   = map[string]pinnedDestiny{}
)

// pinnedDestinyDir is the tree of a destiny that left this repository, at the commit
// scripts/destiny-source.sh pins (NIM-1062). A cold cache is filled from the network;
// under SOUL_STACK_DESTINY_OFFLINE=1 it fails the test instead of skipping it, because a
// skipped render test over a destiny nobody fetched is a pass about nothing. A failure is
// remembered as well as a success: twenty tests must not each wait out an unreachable
// github.com.
func pinnedDestinyDir(t *testing.T, alias string) string {
	t.Helper()
	pinnedDestiniesMu.Lock()
	defer pinnedDestiniesMu.Unlock()
	got, ok := pinnedDestinies[alias]
	if !ok {
		got = resolvePinnedDestiny(alias)
		pinnedDestinies[alias] = got
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	return got.dir
}

func resolvePinnedDestiny(alias string) pinnedDestiny {
	cmd := exec.Command(filepath.FromSlash("../../../scripts/destiny-source.sh"), "dir", alias)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	dir := strings.TrimSpace(string(out))
	if err != nil || dir == "" {
		return pinnedDestiny{err: fmt.Errorf("scripts/destiny-source.sh dir %s: %v\n%s\n\t`make destiny-sources` fills the cache.", alias, err, stderr.String())}
	}
	return pinnedDestiny{dir: dir}
}
