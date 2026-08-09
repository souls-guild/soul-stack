package harness

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The catalog is three URLs, three versions and three digests copied out of
// examples/service/redis. Copies drift, and this one drifts in a direction with
// no symptom: a version bumped in the example and not here means the mirror holds
// a tarball nobody asks for, the create asks github for the new one, and the gate
// silently goes back to being decided by a network outside its slice — green all
// the way (NIM-542).
//
// So none of it is trusted. Every guard below re-reads the example and fails on
// the difference, docker-free, in `make e2e-live-gate`'s first step — twenty
// minutes before a live run would have had the chance to be wrong quietly.

// exampleService — the service the gate creates. Named once: these guards are
// about THIS example, and pointing them at another would make them pass by
// finding nothing.
const exampleService = "examples/service/redis"

// reScenarioBaseURLDefault is the production regexp, deliberately reused: these
// guards are about what the runtime scan will see, and a second copy of the
// pattern here could agree with the example while the runtime one no longer does.
var reAllowPrivateDefault = regexp.MustCompile(`default\(vars\.([A-Za-z0-9_]+)_allow_private,`)

// scenarioBaseURLDefaults — every `default(vars.<prefix>_base_url, '<url>')` in
// the service's scenarios, fixture cases excluded.
//
// All the scenarios, not just create/: an external fetch added to update_config
// would be just as outbound and just as invisible. `scenario/*/tests/` is
// excluded because those are render fixtures — they name URLs the scenario never
// dials.
func scenarioBaseURLDefaults(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join(repoRoot(t), exampleService, "scenario")

	out := map[string]string{}
	files := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "tests" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		for _, m := range reScenarioBaseURLDefault.FindAllStringSubmatch(string(body), -1) {
			out[m[1]] = m[2]
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	// A walk that read nothing would make every guard below pass by vacuity —
	// exactly the shape of green this ticket is about.
	if files == 0 {
		t.Fatalf("scanned %s and found no scenario YAML at all; the example moved and these guards are checking nothing", root)
	}
	if len(out) == 0 {
		t.Fatalf("scanned %d scenario file(s) under %s and matched no `default(vars.X_base_url, '…')`; "+
			"either the expression shape changed or the regexp did, and either way the catalog is now unguarded", files, root)
	}
	return out
}

// serviceVars — the example's assembled base layer, read as plain YAML.
func serviceVars(t *testing.T) map[string]any {
	t.Helper()
	path := filepath.Join(repoRoot(t), exampleService, "vars", "00-base.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(body, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s parsed to nothing", path)
	}
	return out
}

// effectiveBaseURLs — the base_url each prefix ACTUALLY resolves to on a default
// create: the service vars' value when it sets one, the scenario's fallback
// otherwise. Prefixes that resolve to "" are dropped — an empty base_url is a
// branch the operator has to switch on (`modules_base_url`, `binary_base_url`),
// and nothing is fetched until they do.
func effectiveBaseURLs(t *testing.T) map[string]string {
	t.Helper()
	vars := serviceVars(t)
	out := map[string]string{}
	for prefix, fallback := range scenarioBaseURLDefaults(t) {
		url := fallback
		if v, ok := vars[prefix+"_base_url"]; ok {
			s, isStr := v.(string)
			if !isStr {
				t.Fatalf("vars/00-base.yaml: %s_base_url is %T, want string", prefix, v)
			}
			url = s
		}
		if strings.TrimSpace(url) == "" {
			continue
		}
		out[prefix] = url
	}
	return out
}

// TestArtifactCatalogCoversEveryDefaultFetch — the catalog and the example name
// the same external fetches, at the same URLs.
//
// Both directions, and the second is the load-bearing one. A catalog entry with
// no fetch behind it is dead weight; a FETCH WITH NO CATALOG ENTRY is the gate
// quietly reaching the public internet again, which is the whole defect. Add a
// fourth `apply: destiny:` with a github base_url to create/main.yml and this
// goes red before anyone spends twenty minutes finding out the hard way.
func TestArtifactCatalogCoversEveryDefaultFetch(t *testing.T) {
	want := effectiveBaseURLs(t)
	got := map[string]string{}
	for _, a := range artifactCatalog() {
		got[a.varPrefix] = a.upstreamBase
	}

	for prefix, url := range want {
		switch have, ok := got[prefix]; {
		case !ok:
			t.Errorf("%s fetches %s_base_url = %s on a default create, and the catalog has no entry for it.\n"+
				"  The mirror will not serve it, the vars layer will not redirect it, and the gate will\n"+
				"  fetch it from the public internet — the dependency NIM-542 removed. Add it to\n"+
				"  artifactCatalog() and prime the cache (`make e2e-live-artifacts`).", exampleService, prefix, url)
		case have != url:
			t.Errorf("%s_base_url: the example says %s, the catalog says %s.\n"+
				"  The cache primer fetches the catalog's URL, so the mirror would hold the wrong\n"+
				"  release and the run would fetch the right one from upstream.", prefix, url, have)
		}
	}
	for prefix := range got {
		if _, ok := want[prefix]; !ok {
			t.Errorf("the catalog carries %s, which %s no longer fetches by default.\n"+
				"  Harmless to the run and misleading to the reader: drop it, or say here why it stays.",
				prefix, exampleService)
		}
	}
}

// TestArtifactCatalogPinsWhatTheExamplePins — versions and digests agree.
//
// The version is the sharp one: it is in the URL path on both sides
// (`/v<version>/<file>`), so a bump in the example that is not made here does not
// break — it MISSES. The mirror serves the old tarball at the old path, the
// destiny asks for the new one at the new path, gets a 404 from the mirror, and
// the failure reads as a fixture bug rather than as the version drift it is.
func TestArtifactCatalogPinsWhatTheExamplePins(t *testing.T) {
	vars := serviceVars(t)
	for _, a := range artifactCatalog() {
		v, ok := vars[a.varPrefix+"_version"]
		if !ok {
			t.Errorf("%s/vars/00-base.yaml sets no %s_version; the catalog pins %s and nothing checks it",
				exampleService, a.varPrefix, a.version)
			continue
		}
		if s, _ := v.(string); s != a.version {
			t.Errorf("%s_version: the example pins %v, the catalog pins %s.\n"+
				"  Both go into the URL path — the mirror would serve v%s at a path nobody requests.",
				a.varPrefix, v, a.version, a.version)
		}

		// The file name carries the version too, and a hand-edited catalog
		// entry that bumps one and not the other 404s in the same silent way.
		if !strings.Contains(a.file, a.version) {
			t.Errorf("%s: file %q does not contain version %s", a.varPrefix, a.file, a.version)
		}

		// Not every artifact is pinned by digest in the example — node_exporter
		// is not — but where the example DOES pin one, it is the same bytes the
		// mirror serves, and a disagreement means the product's own fail-closed
		// checksum step rejects what the fixture handed it.
		raw, ok := vars[a.varPrefix+"_sha256"]
		if !ok {
			continue
		}
		s, _ := raw.(string)
		if want := strings.TrimPrefix(s, "sha256:"); want != "" && want != a.sha256 {
			t.Errorf("%s_sha256: the example pins %s, the catalog pins %s.\n"+
				"  core.url.fetched verifies the example's value against the mirror's bytes, so this\n"+
				"  fails INSIDE the container, on a create, looking like a defect in the product.",
				a.varPrefix, want, a.sha256)
		}
	}
}

// TestArtifactMirrorOverlaySortsLast — the layer the fixture adds wins.
//
// `vars/*.yaml` merge in lexical order and a later file wins (ADR-0082 §3), so
// the overlay overriding anything at all is a fact about the SIBLINGS it is
// sorted against, not about the `99-` it starts with. `_` is 0x5F and every
// lower-case letter is above every digit — a sibling named `base.yaml` would sort
// after `99-…` and take the base_url back to github, with a green run to show for
// it.
func TestArtifactMirrorOverlaySortsLast(t *testing.T) {
	dir := filepath.Join(repoRoot(t), exampleService, "vars")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	overlay := filepath.Base(artifactMirrorVarsFile)
	siblings := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "_stack.yaml" {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(name)); ext != ".yaml" && ext != ".yml" {
			continue
		}
		siblings++
		if name >= overlay {
			t.Errorf("%s/vars/%s sorts at or after the fixture's %s, so it wins the merge and the\n"+
				"  mirror override is lost — the gate fetches from github again, silently. Rename the\n"+
				"  fixture layer to sort after it.", exampleService, name, overlay)
		}
	}
	if siblings == 0 {
		t.Fatalf("%s/vars/ holds no YAML layers; this guard compared the overlay against nothing", exampleService)
	}
}

// TestArtifactMirrorOverlayIsNotSilencedByAStack — the example declares no
// `vars/_stack.yaml`.
//
// A different silence from the one above, and a completer one. Under a stack the
// order is declared rather than scanned, and a file the stack does not list
// contributes NOTHING (keeper/internal/servicevars/pipeline.go: "an unreferenced
// one contributes nothing"). The fixture writes its layer into a throwaway copy
// and cannot be listed by a stack that lives in the tree, so the day this example
// grows one, the overlay stops being read entirely — no error, no warning, and a
// create that quietly fetches from upstream.
func TestArtifactMirrorOverlayIsNotSilencedByAStack(t *testing.T) {
	stack := filepath.Join(repoRoot(t), exampleService, "vars", "_stack.yaml")
	if _, err := os.Stat(stack); err == nil {
		t.Fatalf("%s exists. A stack ignores every layer it does not list, and it cannot list one that\n"+
			"  only exists inside the fixture's copy — so %s now contributes nothing and the gate is\n"+
			"  back on the public internet. Teach the fixture to append a step to the copied stack\n"+
			"  before re-enabling this.", stack, artifactMirrorVarsFile)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", stack, err)
	}
}

// TestArtifactMirrorOverlaySetsEveryKeyTheScenarioReads — the overlay answers
// both halves of every redirect: where to fetch from, and permission to dial it.
//
// Driven off the SCENARIO rather than off the catalog, so it is a second,
// independent reading of the same example. A base_url pointed at the mirror
// without `allow_private: true` does not fall back to github — it fails, in
// core.url's SSRF guard, because the mirror is on the host's LAN address. Both
// keys or neither.
func TestArtifactMirrorOverlaySetsEveryKeyTheScenarioReads(t *testing.T) {
	const base = "http://192.0.2.10:41111"
	var overlay map[string]any
	if err := yaml.Unmarshal(artifactMirrorOverlay(base, artifactCatalog()), &overlay); err != nil {
		t.Fatalf("the fixture's own vars layer does not parse as YAML: %v", err)
	}

	privateOptOuts := map[string]bool{}
	root := filepath.Join(repoRoot(t), exampleService, "scenario", "create", "main.yml")
	body, err := os.ReadFile(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	for _, m := range reAllowPrivateDefault.FindAllStringSubmatch(string(body), -1) {
		privateOptOuts[m[1]] = true
	}

	for prefix := range effectiveBaseURLs(t) {
		got, ok := overlay[prefix+"_base_url"].(string)
		if !ok || !strings.HasPrefix(got, base+"/") {
			t.Errorf("overlay sets %s_base_url = %v, want a path under %s", prefix, overlay[prefix+"_base_url"], base)
		}
		if !privateOptOuts[prefix] {
			t.Errorf("%s reads no `default(vars.%s_allow_private, …)`, so there is no way to let the\n"+
				"  fetch dial the mirror's private address and the redirect cannot work at all.", root, prefix)
			continue
		}
		if v, _ := overlay[prefix+"_allow_private"].(bool); !v {
			t.Errorf("overlay sets %s_allow_private = %v, want true: the mirror is on the host's LAN\n"+
				"  address and core.url's SSRF guard refuses RFC1918 unless this is on.",
				prefix, overlay[prefix+"_allow_private"])
		}
	}
}

// TestScenarioArtifactPrefixesSeesWhatTheGateActuallyFetches — the runtime scan
// that decides whether an overlay is written at all agrees with the catalog.
//
// This is the switch, not a description of it: the fixture writes the vars layer
// only for the prefixes this function reports, so a scan that reports nothing
// writes nothing, the create falls back to the scenario's github defaults, and
// every test still passes — the exact silence NIM-542 is about. Checked against
// the real example tree, not a synthetic one.
func TestScenarioArtifactPrefixesSeesWhatTheGateActuallyFetches(t *testing.T) {
	got, err := scenarioArtifactPrefixes(filepath.Join(repoRoot(t), exampleService))
	if err != nil {
		t.Fatalf("scan %s: %v", exampleService, err)
	}
	for _, prefix := range artifactMirrorPrefixes(artifactCatalog()) {
		if !got[prefix] {
			t.Errorf("the runtime scan does not see %s in %s, so no mirror override is written for it\n"+
				"  and the create fetches it from github — silently, with a green run.", prefix, exampleService)
		}
	}
	// Whatever else it finds must be in the catalog: the overlay would point a
	// prefix at a mirror that holds nothing for it, and the fetch would 404.
	// Membership, not substring: joining the catalog into one string and asking
	// whether the prefix appears in it would accept `exporter` and `node` alike,
	// and a scanned prefix that is merely a substring of a catalogued one gets no
	// mirror entry at all — the fetch goes to github with the guard still green.
	for prefix := range got {
		if !slices.Contains(artifactMirrorPrefixes(artifactCatalog()), prefix) {
			t.Errorf("the runtime scan reports %s, which the catalog does not carry: the overlay would\n"+
				"  redirect it to a mirror with nothing behind that path", prefix)
		}
	}
}

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
