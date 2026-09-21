package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/plugin"
)

// fakeCoreRegistry — a stamped core catalog. The embedded one carries no
// `introduced_in` (nothing has shipped past the baseline release), so the module
// axis is exercised against an injected registry instead of a fixture that would
// have to be re-stamped on every release.
type fakeCoreRegistry map[string]plugin.ModuleDef

func (r fakeCoreRegistry) Lookup(module string) (plugin.ModuleDef, bool) {
	m, ok := r[module]
	return m, ok
}

func (r fakeCoreRegistry) State(module, state string) (plugin.StateDef, bool) {
	m, ok := r[module]
	if !ok {
		return plugin.StateDef{}, false
	}
	def, ok := m.States[state]
	return def, ok
}

func stampedRegistry() fakeCoreRegistry {
	return fakeCoreRegistry{
		"core.file": {
			Name: "file",
			States: map[string]plugin.StateDef{
				"present": {Input: map[string]plugin.InputParamDef{
					"path":    {Type: "string"},
					"selinux": {Type: "string", IntroducedIn: "2.5.0"},
				}},
				"pruned": {IntroducedIn: "2.3.0"},
			},
		},
		"core.choir": {
			Name: "choir", IntroducedIn: "1.4.0",
			States: map[string]plugin.StateDef{"present": {}},
		},
	}
}

func moduleTask(addr string, params map[string]any) Task {
	return Task{Module: &ModuleTask{Module: addr, Params: params}}
}

// A parameter introduced after the state it hangs on is the case an author
// cannot see: the state has existed for releases, so nothing about the task
// looks new. The floor must come from the parameter.
func TestKeeperFeaturesOfTasks_ParamIntroducesFloor(t *testing.T) {
	used := keeperFeaturesOfTasks([]Task{
		moduleTask("core.file.present", map[string]any{"path": "/etc/motd", "selinux": "system_u"}),
	}, stampedRegistry())

	floor := InferKeeperFloor(used)
	if floor == nil {
		t.Fatal("floor = nil, want the selinux param to set it")
	}
	if floor.IntroducedIn != "2.5.0" || floor.ID != "core.file.present.params.selinux" {
		t.Fatalf("floor = %+v, want core.file.present.params.selinux @ 2.5.0", *floor)
	}
	if floor.Where != "$.tasks[0].params.selinux" {
		t.Fatalf("Where = %q, want the task attribution", floor.Where)
	}
}

// A parameter the task does not pass introduces nothing — otherwise every task
// touching a module would inherit the floor of its newest optional parameter.
func TestKeeperFeaturesOfTasks_UnusedParamIsNotAFloor(t *testing.T) {
	used := keeperFeaturesOfTasks([]Task{
		moduleTask("core.file.present", map[string]any{"path": "/etc/motd"}),
	}, stampedRegistry())
	if floor := InferKeeperFloor(used); floor != nil {
		t.Fatalf("floor = %+v, want nil (no stamped feature is used)", *floor)
	}
}

func TestKeeperFeaturesOfTasks_ModuleAndStateAndBlocks(t *testing.T) {
	used := keeperFeaturesOfTasks([]Task{
		moduleTask("core.file.pruned", nil),
		{Block: &BlockTask{Block: []Task{moduleTask("core.choir.present", nil)}}},
	}, stampedRegistry())

	got := map[string]string{}
	for _, f := range used {
		got[f.ID] = f.IntroducedIn
	}
	if got["core.file.pruned"] != "2.3.0" {
		t.Errorf("state feature = %q, want 2.3.0", got["core.file.pruned"])
	}
	if got["core.choir"] != "1.4.0" {
		t.Errorf("module feature inside a block = %q, want 1.4.0", got["core.choir"])
	}
	if floor := InferKeeperFloor(used); floor == nil || floor.IntroducedIn != "2.3.0" {
		t.Fatalf("floor = %+v, want the highest (2.3.0)", floor)
	}
}

// A plugin module ships on its own version line: its manifest says nothing about
// a keeper release, so it must never raise the keeper floor (ADR-0076(i), the
// same boundary the soul axis draws in NIM-161).
func TestKeeperFeaturesOfTasks_PluginModuleIsNotOnTheKeeperAxis(t *testing.T) {
	reg := stampedRegistry()
	reg["acme.file"] = plugin.ModuleDef{
		Name: "file", IntroducedIn: "9.9.9",
		States: map[string]plugin.StateDef{"present": {}},
	}
	used := keeperFeaturesOfTasks([]Task{moduleTask("acme.file.present", nil)}, reg)
	if len(used) != 0 {
		t.Fatalf("used = %+v, want nothing from a non-core namespace", used)
	}
}

// The intra-host concurrency keys are the first task-level rows of the keeper DSL
// registry (ADR-0075, NIM-150). Both must be DETECTED on a task body — `async:`
// because an older keeper rejects the unknown key, `require:` because an older
// keeper ACCEPTS it and silently drops the ordering invariant, which is the
// sharper of the two.
func TestKeeperFeaturesOfTasks_ConcurrencyKeys(t *testing.T) {
	async := moduleTask("core.exec.run", nil)
	async.Async = true
	requires := moduleTask("core.exec.run", nil)
	requires.Require = []string{"probe"}
	all := moduleTask("core.exec.run", nil)
	all.Require = RequireAll

	got := map[string]bool{}
	for _, f := range keeperFeaturesOfTasks([]Task{async, requires, all}, stampedRegistry()) {
		got[f.ID] = true
	}
	if !got[FeatureTaskAsync] {
		t.Errorf("async: did not register %s", FeatureTaskAsync)
	}
	if !got[FeatureTaskRequire] {
		t.Errorf("require: did not register %s", FeatureTaskRequire)
	}

	plain := keeperFeaturesOfTasks([]Task{moduleTask("core.exec.run", nil)}, stampedRegistry())
	for _, f := range plain {
		if f.ID == FeatureTaskAsync || f.ID == FeatureTaskRequire {
			t.Errorf("a task declaring neither key registered %s", f.ID)
		}
	}
}

func TestInferKeeperFloor(t *testing.T) {
	cases := []struct {
		name string
		used []KeeperFeature
		want string // "" = nil floor
		id   string
	}{
		{name: "empty"},
		{
			name: "highest wins",
			used: []KeeperFeature{{ID: "a", IntroducedIn: "0.2.0"}, {ID: "b", IntroducedIn: "0.10.0"}, {ID: "c", IntroducedIn: "0.3.1"}},
			want: "0.10.0", id: "b",
		},
		{
			// Attribution must not depend on traversal order.
			name: "tie broken by id",
			used: []KeeperFeature{{ID: "zeta", IntroducedIn: "1.0.0"}, {ID: "alpha", IntroducedIn: "1.0.0"}},
			want: "1.0.0", id: "alpha",
		},
		{
			name: "unreleased contributes nothing",
			used: []KeeperFeature{{ID: "a", IntroducedIn: Unreleased}},
		},
		{
			name: "malformed version is skipped, not guessed",
			used: []KeeperFeature{{ID: "a", IntroducedIn: "v1.2"}, {ID: "b", IntroducedIn: "0.4.0"}},
			want: "0.4.0", id: "b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InferKeeperFloor(tc.used)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("floor = %+v, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("floor = nil, want %s", tc.want)
			}
			if got.IntroducedIn != tc.want || got.ID != tc.id {
				t.Fatalf("floor = %+v, want %s @ %s", *got, tc.id, tc.want)
			}
		})
	}
}

// The headline case of ADR-0076(k): the author declared a floor the definition
// does not honor. The message has to name the feature and the version, or it
// tells the author nothing they can act on.
func TestCompatFloorDiagnostic_DeclaredMinBelowFloor(t *testing.T) {
	d := CompatFloorDiagnostic(
		&VersionWindow{Min: "1.0.0", Max: "3.0.0"},
		&KeeperFeature{ID: "core.file.present.params.selinux", IntroducedIn: "2.5.0", Where: "$.tasks[3].params.selinux"},
	)
	if d == nil {
		t.Fatal("diagnostic = nil, want compat_floor_too_low")
	}
	if d.Code != "compat_floor_too_low" {
		t.Fatalf("code = %q", d.Code)
	}
	for _, want := range []string{"2.5.0", "core.file.present.params.selinux", "$.tasks[3].params.selinux", "[1.0.0, 3.0.0)"} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("message %q does not mention %q", d.Message, want)
		}
	}
	if d.YAMLPath != "$.compat.keeper.min" {
		t.Errorf("yaml_path = %q, want the min bound", d.YAMLPath)
	}
}

func TestCompatFloorDiagnostic_Silent(t *testing.T) {
	floor := &KeeperFeature{ID: "destiny.validate", IntroducedIn: "2.0.0"}
	cases := []struct {
		name   string
		window *VersionWindow
		floor  *KeeperFeature
	}{
		{name: "honest declaration", window: &VersionWindow{Min: "2.0.0", Max: "3.0.0"}, floor: floor},
		{name: "author pinned higher than needed", window: &VersionWindow{Min: "2.4.0"}, floor: floor},
		{name: "no window declared", window: nil, floor: floor},
		{name: "empty window block", window: &VersionWindow{}, floor: floor},
		{name: "nothing above the baseline is used", window: &VersionWindow{Min: "0.1.0"}, floor: nil},
		{name: "malformed floor", window: &VersionWindow{Min: "1.0.0"}, floor: &KeeperFeature{ID: "x", IntroducedIn: "2.x"}},
		{name: "malformed min is reported elsewhere", window: &VersionWindow{Min: "v1"}, floor: floor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if d := CompatFloorDiagnostic(tc.window, tc.floor); d != nil {
				t.Fatalf("diagnostic = %+v, want silence", *d)
			}
		})
	}
}

// A `{max: …}`-only window is unbounded below, so it promises every keeper that
// ever existed — including the ones without the feature.
func TestCompatFloorDiagnostic_MaxOnlyWindow(t *testing.T) {
	d := CompatFloorDiagnostic(&VersionWindow{Max: "9.0.0"}, &KeeperFeature{ID: "destiny.validate", IntroducedIn: "2.0.0"})
	if d == nil {
		t.Fatal("diagnostic = nil, want compat_floor_too_low for an unbounded-below window")
	}
	if d.YAMLPath != "$.compat.keeper" {
		t.Errorf("yaml_path = %q, want the block (there is no min to point at)", d.YAMLPath)
	}
}

// When max is at or below the floor, no keeper in the window can render the
// definition at all — raising min alone would leave an empty window, so the hint
// has to say that.
func TestCompatFloorDiagnostic_WindowExcludesFeatureEntirely(t *testing.T) {
	d := CompatFloorDiagnostic(&VersionWindow{Min: "1.0.0", Max: "2.0.0"}, &KeeperFeature{ID: "destiny.validate", IntroducedIn: "2.5.0"})
	if d == nil {
		t.Fatal("diagnostic = nil")
	}
	if !strings.Contains(d.Hint, "no keeper in [1.0.0, 2.0.0)") {
		t.Fatalf("hint = %q, want it to say the whole window is excluded", d.Hint)
	}
}

func TestKeeperFeaturesOfDestiny_ManifestGrammar(t *testing.T) {
	m := &DestinyManifest{
		Compat:   &CompatConfig{Keeper: &VersionWindow{Min: "0.1.0"}},
		Validate: []ValidateRule{{That: "input.a != ''"}},
		Input: InputSchemaMap{
			"a": &InputSchema{Type: "string", RequiredWhen: "input.b"},
			"b": &InputSchema{Type: "string", Pattern: "^x"},
			"c": &InputSchema{Type: "string"},
		},
	}
	got := map[string]string{}
	for _, f := range KeeperFeaturesOfDestiny(m, nil) {
		got[f.ID] = f.Where
	}
	for id, where := range map[string]string{
		FeatureDestinyCompat:            "$.compat",
		FeatureDestinyValidate:          "$.validate",
		FeatureDestinyInputRequiredWhen: "$.input.a",
		FeatureDestinyInputConstraints:  "$.input.b",
	} {
		if got[id] != where {
			t.Errorf("feature %s found at %q, want %q", id, got[id], where)
		}
	}
	if len(got) != 4 {
		t.Errorf("used = %v, want exactly the four stamped grammar features (c declares nothing)", got)
	}
}

func TestKeeperFeaturesOfService_CompatBlock(t *testing.T) {
	if got := KeeperFeaturesOfService(&ServiceManifest{}); len(got) != 0 {
		t.Fatalf("used = %+v, want nothing for a manifest without compat:", got)
	}
	got := KeeperFeaturesOfService(&ServiceManifest{Compat: &CompatConfig{Keeper: &VersionWindow{Min: "0.1.0"}}})
	if len(got) != 1 || got[0].ID != FeatureServiceCompat {
		t.Fatalf("used = %+v, want the compat: block itself", got)
	}
}

// TestKeeperFeaturesOfService_ModuleAliasName — the single-segment `modules[].name`
// carries a floor (NIM-829).
//
// A keeper that predates the change answers `name_invalid_format` on the manifest,
// and an error there is an UNLOADABLE service, not a degraded render — so a window
// declared below this release promises compatibility with keepers that refuse the
// file outright. The deprecated two-level form is what every such keeper understands
// and carries no floor.
func TestKeeperFeaturesOfService_ModuleAliasName(t *testing.T) {
	old := KeeperFeaturesOfService(&ServiceManifest{Modules: []DependencyRef{{Name: "redis.instance", Ref: "v1"}}})
	if len(old) != 0 {
		t.Fatalf("used = %+v, want nothing — the two-level form is what the baseline understands", old)
	}

	got := KeeperFeaturesOfService(&ServiceManifest{Modules: []DependencyRef{
		{Name: "acme.probe", Ref: "v1"},
		{Name: "redis", Ref: "v1"},
		{Name: "mongo", Ref: "v1"},
	}})
	if len(got) != 1 || got[0].ID != FeatureServiceModuleAliasName {
		t.Fatalf("used = %+v, want exactly one %s — the feature is the grammar, not the row count",
			got, FeatureServiceModuleAliasName)
	}
	if got[0].Where != "$.modules[1].name" {
		t.Errorf("Where = %q, want $.modules[1].name (the first entry in the new form)", got[0].Where)
	}
}

// Every registry row must be either a released MAJOR.MINOR.PATCH or explicitly
// Unreleased. A typo here would silently disable the row (an unparseable version
// is skipped by InferKeeperFloor).
func TestKeeperDSLFeatures_VersionsAreWellFormed(t *testing.T) {
	for id, f := range keeperDSLFeatures {
		if id != f.id {
			t.Errorf("registry key %q does not match the row id %q", id, f.id)
		}
		if f.introducedIn == Unreleased {
			continue
		}
		if !reCompatVersion.MatchString(f.introducedIn) {
			t.Errorf("feature %s: introduced_in=%q is not a plain MAJOR.MINOR.PATCH", id, f.introducedIn)
		}
	}
}
