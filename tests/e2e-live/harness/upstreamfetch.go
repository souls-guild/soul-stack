package harness

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Whether a live subject downloads release tarballs from the public internet — and the
// tripwire that keeps the answer "no".
//
// THE HISTORY MATTERS, because this file is what is left of a mechanism. NIM-542: six of
// the then-nine gate tests created a service, every create fetched node_exporter,
// redis_exporter and vector from github.com — about 18 downloads per run — and NIM-406
// measured the cost: three consecutive runs on a slice that had not changed by a byte gave
// three different answers, one of them dying in `core.url` resolving github.com. So the
// harness grew a local mirror: a cache of the real tarballs, an https server over it, and a
// `vars/99-…yaml` layer written into the fixture's copy of the service pointing the fetches
// at it. `artifactCatalog()` held the three pins the mirror served.
//
// NIM-871 then cut the service those six tests created, and the six guards that had held
// the catalog against the service's own declarations went with it — leaving the catalog an
// unverified copy of a service that no longer existed anywhere. NIM-876 had to decide that
// copy's fate, and the answer follows from asking what would make it TRUE: it is a claim
// about what the subjects fetch, and no live subject fetches anything. The in-tree fixtures
// here download nothing, and the pinned service installs Redis from an apt repository,
// which is not this mechanism. A pin table with no consumer cannot be made true — it can
// only be removed, or left to be revived by someone pointing a new service at digests
// nothing ever checked.
//
// So the mirror is gone: ~900 lines, `make e2e-live-artifacts`, `cmd/artifact-cache`, the
// per-container CA install, and the three pins. What stays is the scanner plus a guard that
// fails the moment a live subject declares such a fetch again — because the alternative to
// dead machinery is not "no machinery", it is "an outbound dependency nobody notices". A
// subject that legitimately needs one gets the mirror back; it is in git, whole, at the
// commit that removed it.
//
// ⚠ WHAT THIS DOES NOT CLAIM: the gate is not hermetic and has never been. The soul
// container apt-installs Redis from packages.redis.io and nginx from Debian's own mirror,
// so a run needs a network. This file is about ONE class — a scenario naming a release root
// and pulling a tarball through `core.url` — which is the class that was measured making a
// blocking gate's verdict random.

// reScenarioBaseURLDefault — the shape a scenario uses to name an external download root:
// `${ default(vars.<prefix>_base_url, '<url>') }`. Captures the prefix and the fallback,
// which is empty for the vars an operator has to switch on (`modules_base_url`,
// `binary_base_url`) and a real URL for the fetches that happen by default.
var reScenarioBaseURLDefault = regexp.MustCompile(`default\(vars\.([A-Za-z0-9_]+)_base_url,\s*'([^']*)'\)`)

// scenarioArtifactPrefixes — the prefixes a service actually fetches over http by default,
// read out of its own scenario/ tree.
//
// Read from the tree rather than taken on trust, which is the point: an empty fallback is
// not a fetch (nothing is downloaded until an operator sets the var), and
// `scenario/*/tests/` is skipped because those are render fixtures whose URLs are never
// dialled.
func scenarioArtifactPrefixes(serviceDir string) (map[string]bool, error) {
	root := filepath.Join(serviceDir, "scenario")
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	out := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "tests" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(p); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range reScenarioBaseURLDefault.FindAllStringSubmatch(string(body), -1) {
			if strings.HasPrefix(m[2], "http://") || strings.HasPrefix(m[2], "https://") {
				out[m[1]] = true
			}
		}
		return nil
	})
	return out, err
}

// liveSubjectDirs — every service directory this tier can bring up, as paths.
//
// The two kinds, and neither may be dropped: the in-tree fixtures under `tests/e2e-live/`
// (a `service.yml` beside a `scenario/`), and the extracted pin of each catalogued
// out-of-tree service. A pin that is not in the cache yet is reported as unscanned rather
// than skipped silently — an unscanned subject is exactly the one whose fetches nobody
// looked at.
func liveSubjectDirs(repoRootDir string) (scanned []string, unscanned []string, err error) {
	tierDir := filepath.Join(repoRootDir, "tests", "e2e-live")
	entries, err := os.ReadDir(tierDir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(tierDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "service.yml")); err != nil {
			continue
		}
		scanned = append(scanned, dir)
	}

	cacheRoot, err := serviceCacheDir()
	if err != nil {
		return nil, nil, err
	}
	for _, svc := range serviceCatalog() {
		tree := serviceTreeDir(cacheRoot, svc)
		if ok, err := dirHasEntries(tree); err != nil {
			return nil, nil, err
		} else if ok {
			scanned = append(scanned, tree)
			continue
		}
		unscanned = append(unscanned, fmt.Sprintf("%s@%s (cache cold: %s)", svc.alias, svc.commit, tree))
	}
	sort.Strings(scanned)
	sort.Strings(unscanned)
	return scanned, unscanned, nil
}
