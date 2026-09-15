package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Docker-free guards over the out-of-tree subject pins (NIM-876). They run in
// `make e2e-live-gate`'s first step, before anything is brought up: a pin that cannot
// name its subject is worth catching in half a second, not twenty minutes in.

// TestServiceCatalogPinsAreFullCommits — a pin is a full 40-hex object name, and nothing
// else will do.
//
// A tag is movable, so "the gate runs v1.2.0" stops being a fact the moment someone
// retags; an abbreviated sha is unambiguous today and may not be against a repository
// that has grown. Both would read as a pin and neither is one.
func TestServiceCatalogPinsAreFullCommits(t *testing.T) {
	for _, svc := range serviceCatalog() {
		if !reFullSHA.MatchString(svc.commit) {
			t.Errorf("%s: commit %q is not a full 40-hex object name — a tag or an abbreviation "+
				"is not a pin", svc.alias, svc.commit)
		}
	}
}

// TestServiceCatalogEntriesAreAddressable — every entry can be fetched by someone who has
// only this repository: an https URL, a non-empty alias, and a line saying what the subject
// is FOR.
//
// The URL is checked for scheme rather than reachability on purpose — reachability is a
// fact about the machine, and this guard must not go red on a laptop in a tunnel. What it
// does catch is the pin whose URL is a local path, which works on the machine it was
// written on and nowhere else.
func TestServiceCatalogEntriesAreAddressable(t *testing.T) {
	seen := map[string]bool{}
	for _, svc := range serviceCatalog() {
		if svc.alias == "" {
			t.Errorf("an entry with url %q has no alias", svc.url)
			continue
		}
		if seen[svc.alias] {
			t.Errorf("%s: two entries share the alias — they would share a cache directory and a "+
				"registry id", svc.alias)
		}
		seen[svc.alias] = true
		if !strings.HasPrefix(svc.url, "https://") {
			t.Errorf("%s: url %q is not https — a pin nobody else can fetch is not reproducible, "+
				"which is the whole reason there is a pin", svc.alias, svc.url)
		}
		if strings.TrimSpace(svc.why) == "" {
			t.Errorf("%s: no `why` — it is what the cold-cache error tells a reader who has never "+
				"seen this catalog", svc.alias)
		}
	}
}

// TestServiceEnvSuffixIsAShellIdentifier — the alias→env-var mapping produces a name a
// shell can set.
//
// The two overrides are read by name (`SOUL_STACK_E2E_SERVICE_DIR_<ALIAS>`), and an alias
// with a hyphen in it would produce a variable nobody can export — the override would then
// be silently unavailable for exactly the service that needed it.
func TestServiceEnvSuffixIsAShellIdentifier(t *testing.T) {
	cases := map[string]string{
		"redis":              "REDIS",
		"demo-service-redis": "DEMO_SERVICE_REDIS",
		"redis.legacy":       "REDIS_LEGACY",
	}
	for in, want := range cases {
		if got := serviceEnvSuffix(in); got != want {
			t.Errorf("serviceEnvSuffix(%q) = %q, want %q", in, got, want)
		}
	}
	for _, svc := range serviceCatalog() {
		suffix := serviceEnvSuffix(svc.alias)
		for _, r := range suffix {
			ok := (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
			if !ok {
				t.Errorf("%s: env suffix %q carries %q, which is not settable in sh", svc.alias, suffix, r)
			}
		}
	}
}

// TestServiceTreeDirCarriesTheCommit — the extracted path names the commit it holds.
//
// That is what makes "which bytes did this run read" answerable after the fact, and what
// makes a pin bump add a directory instead of rewriting one. A layout keyed by alias alone
// would be indistinguishable from the checkout-on-disk design the pin replaced: the
// directory would say `redis` and contain whatever was extracted into it last.
func TestServiceTreeDirCarriesTheCommit(t *testing.T) {
	for _, svc := range serviceCatalog() {
		got := serviceTreeDir("/cache", svc)
		if !strings.Contains(got, svc.commit) {
			t.Errorf("%s: tree dir %q does not carry commit %s", svc.alias, got, svc.commit)
		}
		if filepath.Base(got) != svc.commit {
			t.Errorf("%s: tree dir %q does not END in the commit — a sibling of a pin must not be "+
				"reachable by truncating the path", svc.alias, got)
		}
	}
}

// TestOverriddenServiceDirsReportsTheOverride — the gate's refusal has something to read.
//
// `make e2e-live-gate` asks this function whether a pin is overridden and refuses to start
// if one is. A function that answered "nothing overridden" while the harness happily used
// the override would turn the refusal into decoration.
func TestOverriddenServiceDirsReportsTheOverride(t *testing.T) {
	cat := serviceCatalog()
	if len(cat) == 0 {
		t.Skip("no catalogued services")
	}
	svc := cat[0]
	key := serviceDirEnvPrefix + serviceEnvSuffix(svc.alias)
	t.Setenv(key, "/tmp/some/working/tree")

	got := OverriddenServiceDirs()
	if got[svc.alias] != "/tmp/some/working/tree" {
		t.Fatalf("OverriddenServiceDirs() = %v, expected %s -> the path in %s", got, svc.alias, key)
	}
	if dir := serviceDirOverride(svc.alias); dir != "/tmp/some/working/tree" {
		t.Errorf("serviceDirOverride(%s) = %q — the reader the harness uses disagrees with the one "+
			"the gate asks", svc.alias, dir)
	}
}

// TestServiceFetchURLPrefersTheMirror — the remote override redirects the fetch and nothing
// else.
//
// It exists for a machine with no route to github, and it must stay distinguishable from
// the DIR override: this one keeps the pin (same commit, verified after the fetch), so the
// verdict is unchanged and the gate may run under it.
func TestServiceFetchURLPrefersTheMirror(t *testing.T) {
	cat := serviceCatalog()
	if len(cat) == 0 {
		t.Skip("no catalogued services")
	}
	svc := cat[0]
	// Cleared rather than assumed absent: a gate run on a machine with no route to github
	// exports this variable, and a guard that read the ambient environment would then fail
	// about the machine it is running on instead of about the code (NIM-406).
	t.Setenv(serviceRemoteEnvPrefix+serviceEnvSuffix(svc.alias), "")
	if got := serviceFetchURL(svc); got != svc.url {
		t.Fatalf("serviceFetchURL with no override = %q, want the published %q", got, svc.url)
	}
	t.Setenv(serviceRemoteEnvPrefix+serviceEnvSuffix(svc.alias), "/srv/mirror/redis.git")
	if got := serviceFetchURL(svc); got != "/srv/mirror/redis.git" {
		t.Errorf("serviceFetchURL under %s%s = %q, want the mirror", serviceRemoteEnvPrefix,
			serviceEnvSuffix(svc.alias), got)
	}
	if OverriddenServiceDirs()[svc.alias] != "" {
		t.Error("a remote override was reported as a DIR override — the gate would refuse to run " +
			"under a knob that does not weaken its verdict")
	}
}

// TestEnsureServiceTreeRefusesAColdCacheOffline — offline plus a cold cache is a named
// failure, not a fetch.
//
// This is how "the gate needs nothing from the network for its subject" is demonstrated
// rather than asserted. The check also pins the message: it has to say where the cache is
// and how to fill it, because the reader is on a machine that cannot.
func TestEnsureServiceTreeRefusesAColdCacheOffline(t *testing.T) {
	t.Setenv(serviceOfflineEnv, "1")
	root := t.TempDir()
	svc := externalService{
		alias:  "nosuch",
		url:    "https://example.invalid/nosuch.git",
		commit: "0123456789abcdef0123456789abcdef01234567",
		why:    "a guard",
	}
	_, err := ensureServiceTree(root, svc)
	if err == nil {
		t.Fatal("a cold cache under the offline switch returned a tree — the run would have reached " +
			"the network, or worse, registered an empty directory as a service")
	}
	for _, want := range []string{serviceOfflineEnv, "make e2e-live-services"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestEnsureServiceTreeRejectsAShortPin — the commit is validated before any directory is
// created.
//
// Without this the extraction would fail somewhere inside git with a message about an
// ambiguous argument, a page below the line that is actually wrong.
func TestEnsureServiceTreeRejectsAShortPin(t *testing.T) {
	root := t.TempDir()
	_, err := ensureServiceTree(root, externalService{alias: "short", url: "https://x/y.git", commit: "b0d933e"})
	if err == nil || !strings.Contains(err.Error(), "40-hex") {
		t.Fatalf("a short pin was accepted or refused for the wrong reason: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("the rejected pin left %d entries in the cache root — the check must stand before "+
			"anything is created", len(entries))
	}
}

// TestNoLiveSubjectFetchesReleaseTarballs — the tripwire the deleted mirror left behind.
//
// NIM-542 built a local mirror because six gate tests pulled 18 tarballs from github.com per
// run and the gate's verdict became partly a question about the weather (NIM-406: three runs
// on an unchanged slice, three answers). NIM-876 removed that mirror, because after NIM-871
// no live subject fetches anything and a pin table with no consumer cannot be held to its
// subject — see upstreamfetch.go.
//
// This is what keeps the removal honest. The moment a live subject declares such a fetch
// again, the choice has to be made deliberately instead of discovered in a flaky release
// gate months later.
func TestNoLiveSubjectFetchesReleaseTarballs(t *testing.T) {
	scanned, unscanned, err := liveSubjectDirs(repoRoot(t))
	if err != nil {
		t.Fatalf("liveSubjectDirs: %v", err)
	}
	if len(scanned) == 0 {
		t.Fatal("no live subject was found at all — this guard would then pass by scanning nothing, " +
			"which is the shape of a green test that checks nothing")
	}
	for _, dir := range scanned {
		prefixes, err := scenarioArtifactPrefixes(dir)
		if err != nil {
			t.Fatalf("%s: scan: %v", dir, err)
		}
		if len(prefixes) == 0 {
			continue
		}
		names := make([]string, 0, len(prefixes))
		for p := range prefixes {
			names = append(names, p)
		}
		t.Errorf("%s fetches release tarballs by default: %v.\n"+
			"  A blocking pre-tag gate whose subject downloads from a release CDN has a verdict that\n"+
			"  is partly about the CDN (NIM-406, measured). Two ways out, and the choice is\n"+
			"  deliberate either way:\n"+
			"    - point the subject's `<prefix>_base_url` vars at a mirror the run controls;\n"+
			"    - restore the harness's own mirror — cache, https server and the vars layer are in\n"+
			"      git whole, removed by NIM-876 (tests/e2e-live/harness/artifactmirror.go).",
			dir, names)
	}
	if len(unscanned) > 0 {
		t.Logf("not scanned (prime with `make e2e-live-services` to include): %v", unscanned)
	}
}
