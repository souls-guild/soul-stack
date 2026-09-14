package harness

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ★ artifactCatalog() IS NO LONGER CHECKED AGAINST ANY SERVICE, and that is the
// hazard NIM-542 named. Its three URLs, versions and digests were copied out of
// examples/service/redis, and six guards here re-read that tree and failed on the
// difference — because a copy drifts in a direction with no symptom: a version
// bumped in the service and not here means the mirror holds a tarball nobody asks
// for, the create asks github for the new one, and the gate is silently decided by
// a network outside its slice, green all the way.
//
// NIM-871 cut the service out of the engine, so those guards had no subject left
// and went with it. The catalog stays because the mirror machinery below it is
// still wired into every stand — but it is now an unverified copy, and the service
// repo that inherits the create has to bring its own version of these guards, or
// the drift returns with nothing watching for it.

// TestScenarioArtifactPrefixesLeavesUnrelatedServicesAlone — a service that
// fetches nothing gets no overlay.
//
// Without this the fixture would write an artifact layer into every materialized
// service, and the runtime "the mirror was never asked" assertion would then
// accuse smoke-nginx-live of failing to use a mirror it had no reason to touch —
// a red gate about nothing, which is how a gate stops being read.
func TestScenarioArtifactPrefixesLeavesUnrelatedServicesAlone(t *testing.T) {
	for _, svc := range []string{
		"examples/service/smoke-nginx-live",
		"tests/e2e-live/module-delivery-live",
	} {
		got, err := scenarioArtifactPrefixes(filepath.Join(repoRoot(t), svc))
		if err != nil {
			t.Fatalf("scan %s: %v", svc, err)
		}
		if len(got) != 0 {
			t.Errorf("%s fetches %v by default; either it grew an external download (add it to the\n"+
				"  catalog) or the scan is matching something that is not one", svc, got)
		}
	}

	// A directory that is not a service at all is not an error — NewStack must not
	// die because a fixture has no scenario/ tree.
	got, err := scenarioArtifactPrefixes(filepath.Join(t.TempDir(), "nothing-here"))
	if err != nil || len(got) != 0 {
		t.Errorf("scanning a non-service: got %v, %v; want empty and no error", got, err)
	}
}

// TestScenarioArtifactPrefixesIgnoresRenderFixtures — `scenario/*/tests/` names
// URLs that are never dialed.
//
// Those files are expected-render fixtures. Counting one would make the fixture
// write an overlay for a prefix no apply ever fetches, and the runtime assertion
// would then demand a hit that cannot happen.
func TestScenarioArtifactPrefixesIgnoresRenderFixtures(t *testing.T) {
	svc := t.TempDir()
	fixture := filepath.Join(svc, "scenario", "create", "tests")
	if err := os.MkdirAll(fixture, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "expected.yml"),
		[]byte("base_url: \"${ default(vars.promtail_base_url, 'https://example.com/x') }\"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(svc, "scenario", "create", "main.yml"),
		[]byte("base_url: \"${ default(vars.modules_base_url, '') }\"\n"), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}

	got, err := scenarioArtifactPrefixes(svc)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got["promtail"] {
		t.Errorf("a render fixture under scenario/create/tests/ was counted as a live fetch")
	}
	if got["modules"] {
		t.Errorf("an empty base_url default was counted as a fetch; nothing is downloaded until an "+
			"operator sets it (got %v)", got)
	}
}

// TestCatalogSubsetKeepsOnlyWhatWasAskedFor — the overlay and the runtime
// assertion are driven off the same subset.
func TestCatalogSubsetKeepsOnlyWhatWasAskedFor(t *testing.T) {
	cat := artifactCatalog()
	if got := catalogSubset(cat, map[string]bool{"vector": true}); len(got) != 1 || got[0].varPrefix != "vector" {
		t.Fatalf("catalogSubset(vector) = %v", artifactMirrorPrefixes(got))
	}
	if got := catalogSubset(cat, nil); len(got) != 0 {
		t.Fatalf("catalogSubset(nothing) = %v, want empty", artifactMirrorPrefixes(got))
	}
	if got := catalogSubset(cat, map[string]bool{"promtail": true}); len(got) != 0 {
		t.Fatalf("catalogSubset(unknown prefix) = %v, want empty", artifactMirrorPrefixes(got))
	}
}

// TestArtifactCatalogPrefixesAreStable — cacheRel keeps the three release roots
// apart.
//
// The mirror hands each destiny `<mirror>/<varPrefix>` as its base_url, so two
// artifacts sharing a prefix would land on each other's paths, and one of them
// would be served the other's bytes — a checksum failure inside the container,
// far from here.
func TestArtifactCatalogPrefixesAreStable(t *testing.T) {
	cat := artifactCatalog()
	seenPrefix := map[string]bool{}
	seenRel := map[string]bool{}
	for _, a := range cat {
		if seenPrefix[a.varPrefix] {
			t.Errorf("duplicate varPrefix %q in the catalog", a.varPrefix)
		}
		seenPrefix[a.varPrefix] = true
		if seenRel[a.cacheRel()] {
			t.Errorf("duplicate cache path %q in the catalog", a.cacheRel())
		}
		seenRel[a.cacheRel()] = true
		if !strings.HasPrefix(a.cacheRel(), a.varPrefix+"/") {
			t.Errorf("%s: cacheRel %q is not under its own prefix", a.varPrefix, a.cacheRel())
		}
		if want := a.upstreamBase + "/v" + a.version + "/" + a.file; a.upstreamURL() != want {
			t.Errorf("%s: upstreamURL %q != %q", a.varPrefix, a.upstreamURL(), want)
		}
	}

	got := artifactMirrorPrefixes(cat)
	if !sort.StringsAreSorted(got) || len(got) != len(cat) {
		t.Errorf("artifactMirrorPrefixes(%d entries) = %v", len(cat), got)
	}
}

// TestArtifactMirrorURLIsOneTheDestinyWillAccept — the overlay writes a base_url
// the destinies actually admit.
//
// This one has already happened. The first cut of the mirror served plain http,
// which every guard here was happy with — and the create died in the container on
// `input $.base_url = "http://…" does not match pattern "^https://…"`. The
// destinies declare https-only because `core.url` refuses plain http without an
// opt-out, i.e. it is a security property of the artifact under test, not an
// accident to be relaxed for the fixture's convenience (NIM-211: the example is
// the subject, and the subject does not bend). The mirror was moved to TLS.
//
// So the two ends get tied together, docker-free: the URL this fixture generates
// is matched against the pattern the destiny declares, both read at run time.
// Hardcoding "https" on either side would only assert that the test agrees with
// itself.
func TestArtifactMirrorURLIsOneTheDestinyWillAccept(t *testing.T) {
	// An IP with a port — the WSL2 shape (E2E_KEEPER_HOST), and the harshest
	// input for a character-class pattern.
	m, err := startArtifactMirror(t.TempDir(), "172.27.122.166")
	if err != nil {
		t.Fatalf("start mirror: %v", err)
	}
	t.Cleanup(m.close)

	cat := artifactCatalog()
	var overlay map[string]any
	if err := yaml.Unmarshal(artifactMirrorOverlay(m.baseURL, cat), &overlay); err != nil {
		t.Fatalf("the overlay this fixture writes is not valid YAML: %v", err)
	}

	for _, a := range cat {
		dir := filepath.Join(repoRoot(t), "examples", "destiny", strings.ReplaceAll(a.varPrefix, "_", "-"))
		spec := readDestinyInput(t, dir)

		// First, and before anything that can `continue`. The two checks below bail
		// out when they cannot find what they match against — correctly, that is the
		// finding — but they used to take this one with them, so a renamed base_url
		// input silently stopped anyone asserting anything about the SSRF opt-out
		// as well. Two independent properties, two independent verdicts.
		//
		// The overlay turns off the SSRF guard for this fetch, and that opt-out only
		// exists because the destiny declares it. Delete `allow_private` from the
		// destiny and the overlay starts passing an input nothing accepts — a red
		// create, for a var that reads as harmless.
		if _, ok := spec["allow_private_declared"]; !ok {
			t.Errorf("%s: destiny no longer declares input `allow_private`, but the overlay still sets %s_allow_private.\n"+
				"  Either the SSRF opt-out moved or it was dropped; the fixture cannot dial a\n"+
				"  private mirror without it.", dir, a.varPrefix)
		}

		got, _ := overlay[a.varPrefix+"_base_url"].(string)
		if got == "" {
			t.Errorf("%s: the overlay sets no %s_base_url", a.varPrefix, a.varPrefix)
			continue
		}
		pattern, _ := spec[a.varPrefix].(string)
		if pattern == "" {
			// Skipping here would be the failure this test exists to prevent. An
			// empty pattern does not mean "the destiny is relaxed about base_url";
			// it means this test could not find the declaration it matches against
			// — the input was renamed, nested, or lost its `pattern:` — and from
			// then on the only thing left asserting anything about the generated
			// URL is nothing at all.
			t.Errorf("%s: no `pattern:` found for input base_url, so this test can no longer check that the\n"+
				"  fixture generates a URL the destiny accepts. Either the input moved (and this test must\n"+
				"  follow it) or the destiny dropped its validation (and that is the finding).", dir)
			continue
		}
		{
			re, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatalf("%s: destiny declares an uncompilable base_url pattern %q: %v", dir, pattern, err)
			}
			if !re.MatchString(got) {
				t.Errorf("%s: the mirror advertises %q, which destiny %s rejects: input $.base_url must match %q.\n"+
					"  The gate would die on this inside a container, twenty minutes in, as a\n"+
					"  render failure blamed on the product (NIM-542).",
					a.varPrefix, got, dir, pattern)
			}
			// And the reason startArtifactMirror refuses an advertise host that comes
			// out bracketed: this pattern is a character class with no brackets in
			// it. The refusal is a guess about the subject until it is read off the
			// subject, so read it — if a destiny ever accepts the bracketed form, the
			// refusal became an over-restriction and this says so instead of quietly
			// outliving its reason.
			if re.MatchString("https://[fd00::1]:8443/" + a.varPrefix) {
				t.Errorf("%s: destiny %s now accepts a bracketed base_url (pattern %q), but\n"+
					"  startArtifactMirror still refuses an advertise host that would produce one. The\n"+
					"  refusal exists only because of this pattern, so it is now guarding a reason that\n"+
					"  no longer holds — for THIS destiny. The mirror advertises one base_url to all\n"+
					"  three, so the refusal can only be dropped once every catalogued destiny accepts\n"+
					"  the bracketed form; until then, relax this test's expectation, not the refusal.",
					a.varPrefix, dir, pattern)
			}
		}
	}
}

// readDestinyInput reads <dir>/destiny.yml and reports two things the fixture
// depends on: the `pattern:` declared for base_url (under the artifact's own
// prefix key, for the caller's convenience) and whether `allow_private` is
// declared at all.
func readDestinyInput(t *testing.T, dir string) map[string]any {
	t.Helper()
	path := filepath.Join(dir, "destiny.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the catalog names an artifact whose destiny is not at %s: %v", path, err)
	}
	var doc struct {
		Input map[string]struct {
			Pattern string `yaml:"pattern"`
		} `yaml:"input"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]any{}
	out[filepath.Base(dir)] = doc.Input["base_url"].Pattern
	// Key the pattern by the artifact prefix too — the caller has the prefix, the
	// directory is the prefix with dashes.
	out[strings.ReplaceAll(filepath.Base(dir), "-", "_")] = doc.Input["base_url"].Pattern
	if _, ok := doc.Input["allow_private"]; ok {
		out["allow_private_declared"] = true
	}
	return out
}
