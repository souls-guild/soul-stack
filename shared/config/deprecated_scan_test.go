package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/plugin"
)

// deprecatingRegistry — a core catalog where one param of one state is on its
// way out. Injected rather than fixtured off the embedded catalog: no shipped
// core manifest declares `deprecated:` yet, so a fixture would test nothing
// today and would have to be re-stamped the moment one does.
func deprecatingRegistry() fakeCoreRegistry {
	return fakeCoreRegistry{
		"core.file": {
			Name: "file",
			States: map[string]plugin.StateDef{
				"present": {Input: map[string]plugin.InputParamDef{
					"path": {Type: "string"},
					"mode": {Type: "string"},
					"owner": {Type: "string", Deprecated: &plugin.DeprecatedDef{
						Since: "0.4.0", RemovedIn: "0.6.0", Use: "user",
					}},
					"user": {Type: "string"},
				}},
			},
		},
	}
}

// The finding is what a definition ACTUALLY passes. A deprecated param the
// definition never mentions is not a migration it has to make, and listing it
// would bury the ones that are.
func TestScanDeprecated_ReportsOnlyWhatThePlanPasses(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("core.file.present", map[string]any{"path": "/etc/x", "owner": "root"}),
		moduleTask("core.file.present", map[string]any{"path": "/etc/y", "mode": "0644"}),
	}, deprecatingRegistry(), nil)

	if len(scan.Uses) != 1 {
		t.Fatalf("uses = %d, want 1 (only the task that passes `owner`): %+v", len(scan.Uses), scan.Uses)
	}
	u := scan.Uses[0]
	if u.Param != "owner" || u.Module != "core.file.present" {
		t.Errorf("use = %+v, want owner on core.file.present", u)
	}
	if u.Deprecated.RemovedIn != "0.6.0" || u.Deprecated.Use != "user" {
		t.Errorf("deprecation block = %+v, want removed_in 0.6.0 / use user", u.Deprecated)
	}
	// The location is what turns "this service has a problem" into "this line
	// does" — an operator planning a migration edits a line, not a service.
	if u.Where != "$.tasks[0].params.owner" {
		t.Errorf("where = %q, want $.tasks[0].params.owner", u.Where)
	}
	if scan.Clean() {
		t.Error("Clean() is true although a deprecated param was found")
	}
}

// ★ The invariant the whole design hangs on: a plugin module is NOT silently
// clean. Its manifest ships beside the binary, not in the definition's repo, so
// a definition scan cannot read its contract (NIM-228). Reporting an empty
// finding list for such a service would let an operator read "nothing to do"
// off "nothing was checked" — the exact confusion this scan exists to prevent.
func TestScanDeprecated_PluginModuleIsUnresolvedNotClean(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("community.redis.present", map[string]any{"address": "10.0.0.1"}),
	}, deprecatingRegistry(), nil)

	if len(scan.Uses) != 0 {
		t.Fatalf("uses = %+v, want none - the contract was never read", scan.Uses)
	}
	if len(scan.Unresolved) != 1 {
		t.Fatalf("unresolved = %d, want 1", len(scan.Unresolved))
	}
	if got := scan.Unresolved[0].Reason; got != ReasonPluginNamespace {
		t.Errorf("reason = %q, want %q", got, ReasonPluginNamespace)
	}
	if scan.Clean() {
		t.Error("Clean() is true for a scan that could not read the module it was asked about")
	}
}

// A definition using only core modules it fully resolved, with nothing
// deprecated, is the one case that may be reported as clean.
func TestScanDeprecated_FullyResolvedAndCleanIsClean(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("core.file.present", map[string]any{"path": "/etc/x", "user": "root"}),
	}, deprecatingRegistry(), nil)

	if !scan.Clean() {
		t.Errorf("Clean() is false for a fully-resolved clean plan: uses=%+v unresolved=%+v",
			scan.Uses, scan.Unresolved)
	}
}

// A core module this engine does not carry is a different gap from a plugin one
// — the definition is newer than the binary reading it — and it must not be
// mistaken for a clean result either.
func TestScanDeprecated_UnknownCoreModuleIsUnresolved(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("core.futuremod.present", map[string]any{"x": "1"}),
	}, deprecatingRegistry(), nil)

	if len(scan.Unresolved) != 1 || scan.Unresolved[0].Reason != ReasonUnknownCoreModule {
		t.Fatalf("unresolved = %+v, want one unknown_core_module", scan.Unresolved)
	}
	if scan.Clean() {
		t.Error("Clean() is true although a core module could not be resolved")
	}
}

// An unknown STATE of a known module is the same kind of gap: its params were
// never checked. Without this branch the walk would return "clean" for a task
// whose contract it never opened.
func TestScanDeprecated_UnknownStateIsUnresolved(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("core.file.vanished", map[string]any{"path": "/etc/x"}),
	}, deprecatingRegistry(), nil)

	if len(scan.Unresolved) != 1 || scan.Unresolved[0].Reason != ReasonUnknownCoreModule {
		t.Fatalf("unresolved = %+v, want one unknown_core_module for the missing state", scan.Unresolved)
	}
}

// Tasks nested in a block pass ordinary params and are no less due for
// migration; a walk that stopped at top level would under-report a definition
// that groups its work.
func TestScanDeprecated_DescendsIntoBlocks(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{{
		Block: &BlockTask{Block: []Task{
			moduleTask("core.file.present", map[string]any{"path": "/etc/x", "owner": "root"}),
		}},
	}}, deprecatingRegistry(), nil)

	if len(scan.Uses) != 1 {
		t.Fatalf("uses = %d, want the one inside the block: %+v", len(scan.Uses), scan.Uses)
	}
	if scan.Uses[0].Where != "$.tasks[0].block[0].params.owner" {
		t.Errorf("where = %q, want the nested path", scan.Uses[0].Where)
	}
}

// Twenty tasks using the same plugin module are ONE gap in coverage, not
// twenty. A caller listing what it could not check wants the module named once.
func TestScanDeprecated_UnresolvedIsDeduplicated(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("community.redis.present", map[string]any{"a": "1"}),
		moduleTask("community.redis.present", map[string]any{"b": "2"}),
		moduleTask("community.mongo.present", map[string]any{"c": "3"}),
	}, deprecatingRegistry(), nil)

	if len(scan.Unresolved) != 2 {
		t.Fatalf("unresolved = %d, want 2 distinct modules: %+v", len(scan.Unresolved), scan.Unresolved)
	}
	if scan.Unresolved[0].Module != "community.mongo.present" {
		t.Errorf("unresolved is not ordered by module: %+v", scan.Unresolved)
	}
}

// Two scans of the same text must agree, or a caller diffing yesterday's report
// against today's sees churn that is not there. Map iteration over params is not
// ordered, so this is a property of the walk, not of the input.
func TestScanDeprecated_OrderIsStable(t *testing.T) {
	reg := fakeCoreRegistry{
		"core.file": {
			Name: "file",
			States: map[string]plugin.StateDef{
				"present": {Input: map[string]plugin.InputParamDef{
					"alpha": {Deprecated: &plugin.DeprecatedDef{Since: "0.4.0", RemovedIn: "0.6.0"}},
					"beta":  {Deprecated: &plugin.DeprecatedDef{Since: "0.4.0", RemovedIn: "0.6.0"}},
					"gamma": {Deprecated: &plugin.DeprecatedDef{Since: "0.4.0", RemovedIn: "0.6.0"}},
				}},
			},
		},
	}
	params := map[string]any{"alpha": 1, "beta": 2, "gamma": 3}
	first := scanTasksForDeprecated([]Task{moduleTask("core.file.present", params)}, reg, nil)
	for i := 0; i < 8; i++ {
		again := scanTasksForDeprecated([]Task{moduleTask("core.file.present", params)}, reg, nil)
		for j := range first.Uses {
			if first.Uses[j].Param != again.Uses[j].Param {
				t.Fatalf("order drifted between scans: %q vs %q", first.Uses[j].Param, again.Uses[j].Param)
			}
		}
	}
}

// fakeModuleCatalog — the plugin half of the catalog, the shape NIM-228's
// resolver supplies (keeper from Sigil grants, soul-lint from --modules).
type fakeModuleCatalog map[string]plugin.ModuleDef

func (f fakeModuleCatalog) ResolveModule(namespace, name string) (plugin.ModuleDef, bool) {
	m, ok := f[namespace+"."+name]
	return m, ok
}

func redisCatalog() fakeModuleCatalog {
	return fakeModuleCatalog{
		"community.redis": {
			Name: "redis",
			States: map[string]plugin.StateDef{
				"present": {Input: map[string]plugin.InputParamDef{
					"addr": {Type: "string"},
					"address": {Type: "string", Deprecated: &plugin.DeprecatedDef{
						Since: "0.4.0", RemovedIn: "0.6.0", Use: "addr",
					}},
				}},
			},
		},
	}
}

// ★ What NIM-228 unblocked: a plugin module's deprecation is now a FINDING, not
// a coverage gap. Before its resolver existed, the survey could only say "I
// could not look here" — which is exactly why ADR-0076(v) deferred the fleet
// rollup rather than shipping a half-blind one.
func TestScanDeprecated_ResolvesPluginModules(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("community.redis.present", map[string]any{"address": "10.0.0.1"}),
	}, deprecatingRegistry(), redisCatalog())

	if len(scan.Unresolved) != 0 {
		t.Fatalf("a resolvable plugin still reported a gap: %+v", scan.Unresolved)
	}
	if len(scan.Uses) != 1 {
		t.Fatalf("uses = %d, want the deprecated plugin param: %+v", len(scan.Uses), scan.Uses)
	}
	u := scan.Uses[0]
	if u.Module != "community.redis.present" || u.Param != "address" {
		t.Errorf("use = %+v, want address on community.redis.present", u)
	}
	if u.Deprecated.Use != "addr" || u.Deprecated.RemovedIn != "0.6.0" {
		t.Errorf("deprecation = %+v, want the manifest's window", u.Deprecated)
	}
}

// A plugin the catalog does not carry stays a gap. The resolver made plugins
// checkABLE, not automatically checkED: a keeper whose allow-list lacks the
// module knows nothing about it, and saying "clean" there would be the same lie
// the gap list exists to prevent.
func TestScanDeprecated_UnresolvablePluginIsStillAGap(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("community.mongo.present", map[string]any{"address": "10.0.0.1"}),
	}, deprecatingRegistry(), redisCatalog())

	if len(scan.Uses) != 0 {
		t.Fatalf("claimed findings for a module it never resolved: %+v", scan.Uses)
	}
	if len(scan.Unresolved) != 1 || scan.Unresolved[0].Reason != ReasonPluginNamespace {
		t.Fatalf("unresolved = %+v, want one plugin_namespace gap", scan.Unresolved)
	}
	if scan.Clean() {
		t.Error("Clean() is true for a scan that could not resolve the module it was asked about")
	}
}

// A nil catalog is the offline case (soul-lint without --modules, or a keeper
// with no Sigil service). It must degrade to "unchecked", never to "clean".
func TestScanDeprecated_NilCatalogLeavesPluginsUnchecked(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("community.redis.present", map[string]any{"address": "10.0.0.1"}),
	}, deprecatingRegistry(), nil)

	if len(scan.Unresolved) != 1 || scan.Unresolved[0].Reason != ReasonPluginNamespace {
		t.Fatalf("unresolved = %+v, want the plugin reported unchecked", scan.Unresolved)
	}
}

// A resolved manifest that declares no such state is its own gap: the task's
// params were compared against nothing, which is not the same as comparing them
// and finding nothing.
func TestScanDeprecated_UnknownPluginStateIsAGap(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("community.redis.vanished", map[string]any{"address": "10.0.0.1"}),
	}, deprecatingRegistry(), redisCatalog())

	if len(scan.Unresolved) != 1 || scan.Unresolved[0].Reason != ReasonUnknownPluginState {
		t.Fatalf("unresolved = %+v, want one unknown_plugin_state", scan.Unresolved)
	}
}

// A reserved name is answered by the compiled-in registry or by nobody (NIM-377).
//
// The arm used to be chosen by `ns != "core"`, so `keeper.push` went to the plugin
// catalog on the strength of not being spelled `core` — and anything a cluster had
// registered under a reserved alias would have described what that address accepts.
func TestScanDeprecated_ReservedNameNeverReachesTheCatalog(t *testing.T) {
	r := &spyManifests{table: fakeManifests{
		"keeper.push": {Name: "push", States: map[string]plugin.StateDef{
			"run": {Input: map[string]plugin.InputParamDef{
				"host": {Type: "string", Deprecated: &plugin.DeprecatedDef{Since: "0.4.0", RemovedIn: "0.6.0"}},
			}},
		}},
	}}
	scan := scanTasksForDeprecated([]Task{
		moduleTask("keeper.push.run", map[string]any{"host": "h1"}),
	}, deprecatingRegistry(), r)

	if len(r.asked) != 0 {
		t.Errorf("the catalog was consulted for a reserved name: %v", r.asked)
	}
	if len(scan.Uses) != 0 {
		t.Errorf("a reserved address produced findings from a foreign schema: %+v", scan.Uses)
	}
	if len(scan.Unresolved) != 1 || scan.Unresolved[0].Reason != ReasonReservedNamespace {
		t.Fatalf("unresolved = %+v, want one %s", scan.Unresolved, ReasonReservedNamespace)
	}
	if scan.Clean() {
		t.Error("Clean() is true although a module went unread — that is the lie this type exists to prevent")
	}
}

// `core.<unknown>` keeps its own reason: the author's move is "upgrade the engine",
// not "that name cannot exist".
func TestScanDeprecated_UnknownBuiltinKeepsItsOwnReason(t *testing.T) {
	scan := scanTasksForDeprecated([]Task{
		moduleTask("core.haproxy.present", map[string]any{"x": 1}),
	}, deprecatingRegistry(), nil)

	if len(scan.Unresolved) != 1 || scan.Unresolved[0].Reason != ReasonUnknownCoreModule {
		t.Fatalf("unresolved = %+v, want one %s", scan.Unresolved, ReasonUnknownCoreModule)
	}
}
