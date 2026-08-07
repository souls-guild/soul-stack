package scenario

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// writeServiceTree materializes a minimal service snapshot on disk: the survey
// reads a snapshot's LocalDir, so a real directory exercises the same path a git
// checkout produces without needing a repository.
func writeServiceTree(t *testing.T, scenarios map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "service.yml"),
		[]byte("name: demo\nstate_schema_version: 1\n"), 0o600); err != nil {
		t.Fatalf("write service.yml: %v", err)
	}
	for name, body := range scenarios {
		dir := filepath.Join(root, "scenario", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s/main.yml: %v", name, err)
		}
	}
	return root
}

func testScanner(t *testing.T) *DeprecationScanner {
	t.Helper()
	return NewDeprecationScanner(artifact.NewServiceLoader(t.TempDir(), nil), nil)
}

// A core module with a deprecated param exists nowhere in the shipped catalog
// yet, so the survey is exercised against `core.file.present` params that DO
// exist; the deprecation itself is asserted through the gap path below. This
// test pins the coverage bookkeeping and the grouping, which is what the
// endpoint reports even when nothing is deprecated.
func TestSurvey_GroupsDefinitionsAndCountsCoverage(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"create": `name: create
tasks:
  - name: place a file
    module: core.file.present
    params:
      path: /etc/demo.conf
      content: hello
`,
	})
	s := testScanner(t)
	art := &artifact.ServiceArtifact{LocalDir: root}

	byParam := map[string]*DeprecationUsage{}
	gaps := map[string]*DeprecationGap{}
	members := []incarnation.Incarnation{{Name: "demo-a"}, {Name: "demo-b"}}
	s.surveyDefinition(art, definitionKey{service: "demo", version: "v1"},
		members, incarnationNames(members), nil, byParam, gaps)

	if len(byParam) != 0 {
		t.Errorf("nothing in the catalog is deprecated yet, but the survey found %d: %+v", len(byParam), byParam)
	}
	if len(gaps) != 0 {
		t.Errorf("a fully core definition left coverage gaps: %+v", gaps)
	}
}

// ★ The invariant this surface hangs on: a definition built on plugin modules is
// NOT reported as clean when the catalog cannot resolve them. Here the survey is
// handed a nil catalog (the offline case — no Sigil service), which since
// NIM-228 is the only way a plugin goes unread. An empty finding list without a
// gap would read as "nothing to migrate" when it means "nothing was checked".
func TestSurvey_PluginModuleBecomesACoverageGap(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"create": `name: create
tasks:
  - name: configure redis
    module: community.redis.present
    params:
      address: 10.0.0.1
`,
	})
	s := testScanner(t)
	art := &artifact.ServiceArtifact{LocalDir: root}

	byParam := map[string]*DeprecationUsage{}
	gaps := map[string]*DeprecationGap{}
	members := []incarnation.Incarnation{{Name: "redis-prod"}}
	s.surveyDefinition(art, definitionKey{service: "redis", version: "v2"},
		members, incarnationNames(members), nil, byParam, gaps)

	if len(gaps) != 1 {
		t.Fatalf("gaps = %d, want 1 for the unreadable plugin contract: %+v", len(gaps), gaps)
	}
	var g DeprecationGap
	for _, v := range gaps {
		g = *v
	}
	if g.Scope != "module" || g.Reason != "plugin_namespace" {
		t.Errorf("gap = %+v, want module/plugin_namespace", g)
	}
	if len(g.Incarnations) != 1 || g.Incarnations[0] != "redis-prod" {
		t.Errorf("gap incarnations = %v, want [redis-prod] - a gap must name who it affects", g.Incarnations)
	}
}

// A scenario that will not parse must become a gap, not silence. Otherwise the
// one service whose definition is broken is exactly the one reported as fine.
func TestSurvey_UnparseableScenarioBecomesAGap(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"create": "tasks: [ this is not a task list\n",
	})
	s := testScanner(t)
	art := &artifact.ServiceArtifact{LocalDir: root}

	byParam := map[string]*DeprecationUsage{}
	gaps := map[string]*DeprecationGap{}
	members := []incarnation.Incarnation{{Name: "demo-a"}}
	s.surveyDefinition(art, definitionKey{service: "demo", version: "v1"},
		members, incarnationNames(members), nil, byParam, gaps)

	if len(gaps) == 0 {
		t.Fatal("a scenario that does not parse was silently skipped")
	}
	for _, g := range gaps {
		if g.Reason != GapParseFailed {
			t.Errorf("gap reason = %q, want %q", g.Reason, GapParseFailed)
		}
	}
}

// Findings are ordered by what breaks FIRST — an operator reads this to decide
// what to migrate now, so `removed_in` is the sort key, not the module name.
func TestFlattenUsages_OrdersByDeadline(t *testing.T) {
	byParam := map[string]*DeprecationUsage{
		"b": {Module: "core.b.present", Param: "x", Deprecated: depDef("0.9.0")},
		"a": {Module: "core.a.present", Param: "y", Deprecated: depDef("0.6.0")},
		"c": {Module: "core.c.present", Param: "z", Deprecated: depDef("0.7.0")},
	}
	got := flattenUsages(byParam)
	want := []string{"0.6.0", "0.7.0", "0.9.0"}
	for i, w := range want {
		if got[i].Deprecated.RemovedIn != w {
			t.Errorf("position %d removed_in = %q, want %q", i, got[i].Deprecated.RemovedIn, w)
		}
	}
}

// One plugin module used across many incarnations is ONE coverage gap listing
// them all, not one gap per incarnation — the operator has a single thing to fix
// (resolve that manifest), affecting several places.
func TestAddGap_MergesAndListsAffectedIncarnations(t *testing.T) {
	gaps := map[string]*DeprecationGap{}
	addGap(gaps, "module", "community.redis.present", "plugin_namespace", "", []string{"b", "a"})
	addGap(gaps, "module", "community.redis.present", "plugin_namespace", "", []string{"a", "c"})

	if len(gaps) != 1 {
		t.Fatalf("gaps = %d, want 1 merged", len(gaps))
	}
	for _, g := range gaps {
		if len(g.Incarnations) != 3 {
			t.Fatalf("incarnations = %v, want a,b,c merged and deduplicated", g.Incarnations)
		}
		if g.Incarnations[0] != "a" || g.Incarnations[2] != "c" {
			t.Errorf("incarnations = %v, want sorted", g.Incarnations)
		}
	}
}

// depDef builds a deprecation block with the given deadline; since is a
// consistent two-minors-earlier value so the fixture stays policy-valid.
func depDef(removedIn string) plugin.DeprecatedDef {
	return plugin.DeprecatedDef{Since: "0.4.0", RemovedIn: removedIn}
}

// pluginCatalog — the resolver shape NIM-228 supplies (keeper snapshots it from Sigil
// grants). Keyed `<alias>.<module>`: level 1 is the operator's registration, level 2
// the module the artifact declares.
type pluginCatalog map[string]plugin.ModuleDef

func (c pluginCatalog) ResolveModule(alias, module string) (plugin.ModuleDef, bool) {
	m, ok := c[alias+"."+module]
	return m, ok
}

// ★ The end-to-end proof that NIM-228 unblocked this surface: with a catalog in
// hand, a plugin module's deprecation comes back as a FINDING with its
// incarnations attached, where before it could only be reported as unreadable.
// This is the whole reason ADR-0076(v) waited for (x) instead of shipping a
// survey that was blind to exactly the modules an estate runs most.
func TestSurvey_ResolvesPluginDeprecationThroughTheCatalog(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"create": `name: create
tasks:
  - name: configure redis
    module: community.redis.present
    params:
      address: 10.0.0.1
`,
	})
	catalog := pluginCatalog{
		"community.redis": {
			Name: "redis",
			States: map[string]plugin.StateDef{
				"present": {Input: plugin.Input{
					"addr": {Type: "string"},
					"address": {Type: "string", Deprecated: &plugin.DeprecatedDef{
						Since: "0.4.0", RemovedIn: "0.6.0", Use: "addr",
					}},
				}},
			},
		},
	}
	s := testScanner(t)
	art := &artifact.ServiceArtifact{LocalDir: root}

	byParam := map[string]*DeprecationUsage{}
	gaps := map[string]*DeprecationGap{}
	members := []incarnation.Incarnation{{Name: "redis-prod"}, {Name: "redis-stage"}}
	s.surveyDefinition(art, definitionKey{service: "redis", version: "v2"},
		members, incarnationNames(members), catalog, byParam, gaps)

	if len(gaps) != 0 {
		t.Fatalf("a resolvable plugin still produced gaps: %+v", gaps)
	}
	if len(byParam) != 1 {
		t.Fatalf("findings = %d, want the deprecated plugin param: %+v", len(byParam), byParam)
	}
	for _, u := range byParam {
		if u.Module != "community.redis.present" || u.Param != "address" {
			t.Errorf("finding = %s/%s, want community.redis.present/address", u.Module, u.Param)
		}
		// Both incarnations of that service are on the hook: the survey exists to
		// answer "who do I have to fix", and one site per definition would hide
		// half the work.
		if len(u.Sites) != 2 {
			t.Errorf("sites = %d, want both incarnations of the service: %+v", len(u.Sites), u.Sites)
		}
	}
}
