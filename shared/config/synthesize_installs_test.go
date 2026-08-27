package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/plugin"
)

// Guard tests for synthesizing `core.module.installed` install steps from
// `service.yml::modules[]` (ADR-065, NIM-8): Keeper inserts the Soul-side
// install step IMMEDIATELY BEFORE the module's first consumer; an explicit
// literal operator step (takeover) suppresses synthesis.
//
// The step names the ALIAS — address level 1 — not the `<alias>.<module>` entry
// (NIM-524). Every assertion below therefore states the alias it expects, and
// TestSynthesizeModuleInstalls_ParamNameIsAnAliasNotAnAddress states the property
// once for the whole file.

// synthTasks parses the YAML plan with the same parser as prod (a flat top-level
// task list, as after ExpandIncludes).
func synthTasks(t *testing.T, src string) []Task {
	t.Helper()
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	for _, d := range diags {
		if d.Level == "error" {
			t.Fatalf("scenario invalid: %s: %s", d.Code, d.Message)
		}
	}
	return m.Tasks
}

// assertSynthTask checks the shape of a synthesized task: name, module address,
// params {name, ref}, and the absence of orchestration fields (on/where/serial =
// the whole roster).
func assertSynthTask(t *testing.T, task Task, alias, ref string) {
	t.Helper()
	wantName := "install " + alias + " (service manifest)"
	if task.Name != wantName {
		t.Errorf("synth name = %q, want %q", task.Name, wantName)
	}
	if task.Module == nil || task.Module.Module != "core.module.installed" {
		t.Fatalf("synth module = %+v, want core.module.installed", task.Module)
	}
	wantParams := map[string]any{"name": alias, "ref": ref}
	if !reflect.DeepEqual(task.Module.Params, wantParams) {
		t.Errorf("synth params = %v, want %v", task.Module.Params, wantParams)
	}
	if task.On != nil || task.Where != "" || task.Serial != nil || task.RunOnce {
		t.Errorf("synth carries orchestration fields (on=%v where=%q serial=%v run_once=%v), want a clean roster task",
			task.On, task.Where, task.Serial, task.RunOnce)
	}
	if task.IncludeGroupID != 0 || task.IncludeWhen != "" {
		t.Errorf("synth is bound to an include group (%d, %q), want outside any group", task.IncludeGroupID, task.IncludeWhen)
	}
}

// TestSynthesizeModuleInstalls_ParamNameIsAnAliasNotAnAddress — the regression
// guard for NIM-524, stated as a property over every synthesized step rather than
// as one expected string.
//
// `modules[].name` is `<alias>.<module>`; `core.module.installed` takes the alias,
// and rejects anything with a dot (soul/internal/coremod/module: reAlias). Between
// NIM-377 and this fix the synthesizer passed the entry's name through verbatim, so
// EVERY service declaring `modules:` failed at apply on EVERY host — with the two
// ends of the contradiction sitting in the same repository, each with its own
// passing tests.
func TestSynthesizeModuleInstalls_ParamNameIsAnAliasNotAnAddress(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Configure redis
    module: community.redis.config
    params:
      settings: {}
  - name: Probe with the other artifact
    module: acme-tools.probe.run
    params: {}
`)
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{
		{Name: "community.redis", Ref: "v1.0.0"},
		{Name: "acme-tools.probe", Ref: "v2.0.0"},
	})
	if !reflect.DeepEqual(aliases, []string{"community", "acme-tools"}) {
		t.Fatalf("aliases = %v, want [community acme-tools]", aliases)
	}
	synthesized := 0
	for _, task := range out {
		if task.Module == nil || task.Module.Module != moduleInstalledAddr {
			continue
		}
		synthesized++
		name, _ := task.Module.Params["name"].(string)
		if strings.Contains(name, ".") {
			t.Errorf("params.name = %q — a dotted address, not a registration alias; "+
				"core.module.installed rejects it and the service cannot be applied on any host (NIM-524)", name)
		}
		// The same predicate the registration end applies, from the same list —
		// asserting a hand-written shape here is how the two ends drift.
		if !plugin.ValidAlias(name) {
			t.Errorf("params.name = %q is not a well-formed registration alias", name)
		}
	}
	if synthesized != 2 {
		t.Fatalf("synthesized %d install steps, want 2", synthesized)
	}
}

// (a)+(g) Synthesis before the FIRST consumer, exact position; ref from the
// manifest entry lands in params.
func TestSynthesizeModuleInstalls_BeforeFirstConsumer(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Warmup
    module: core.exec.run
    changed_when: false
    params:
      cmd: "true"
  - name: Configure redis
    module: community.redis.config
    params:
      settings: {}
  - name: ACL redis
    module: community.redis.acl
    params:
      users: []
`)
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.2.3"}})
	if len(out) != 4 {
		t.Fatalf("len(out) = %d, want 4", len(out))
	}
	if !reflect.DeepEqual(aliases, []string{"community"}) {
		t.Errorf("aliases = %v, want [community]", aliases)
	}
	assertSynthTask(t, out[1], "community", "v1.2.3")
	if out[0].Name != "Warmup" || out[2].Name != "Configure redis" || out[3].Name != "ACL redis" {
		t.Errorf("task order shifted: %q %q %q", out[0].Name, out[2].Name, out[3].Name)
	}
}

// (b) Consumer inside a block: → insertion before the WHOLE block.
func TestSynthesizeModuleInstalls_ConsumerInsideBlock(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Warmup
    module: core.exec.run
    changed_when: false
    params:
      cmd: "true"
  - name: Deploy group
    block:
      - name: Place file
        module: core.file.present
        params:
          path: /tmp/x
      - name: Configure redis
        module: community.redis.config
        params:
          settings: {}
`)
	out, _ := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	assertSynthTask(t, out[1], "community", "v1.0.0")
	if out[2].Block == nil {
		t.Fatalf("out[2] must remain a block (insertion before the whole block)")
	}
}

// (c) A module with no consumers in the plan → NOT synthesized.
func TestSynthesizeModuleInstalls_NoConsumerNoSynth(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Warmup
    module: core.exec.run
    changed_when: false
    params:
      cmd: "true"
`)
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if len(aliases) != 0 {
		t.Errorf("aliases = %v, want empty", aliases)
	}
	if len(out) != 1 || out[0].Name != "Warmup" {
		t.Errorf("plan changed with no consumers: %+v", out)
	}
}

// (d) Takeover: an explicit top-level core.module.installed with a LITERAL
// params.name suppresses synthesis for that alias (even standing AFTER the
// consumer — the operator decided).
//
// The literal is an ALIAS, because that is what the step takes. Before NIM-524 the
// takeover map was filled with aliases and read with `<alias>.<module>` keys, so
// the documented escape hatch silently did nothing and the operator got a second,
// unasked-for install step next to their own.
func TestSynthesizeModuleInstalls_TakeoverTopLevel(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Operator installs plugin explicitly
    module: core.module.installed
    params:
      name: community
  - name: Configure redis
    module: community.redis.config
    params:
      settings: {}
`)
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if len(aliases) != 0 {
		t.Errorf("aliases = %v, want empty (takeover)", aliases)
	}
	if len(out) != 2 {
		t.Errorf("len(out) = %d, want 2 (no synthesis)", len(out))
	}
}

// (e) Takeover inside a block: also recognized.
func TestSynthesizeModuleInstalls_TakeoverInsideBlock(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Provision group
    block:
      - name: Install plugin
        module: core.module.installed
        params:
          name: community
  - name: Configure redis
    module: community.redis.config
    params:
      settings: {}
`)
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if len(aliases) != 0 || len(out) != 2 {
		t.Errorf("takeover in a block not recognized: aliases=%v len=%d, want empty/2", aliases, len(out))
	}
}

// (f) An explicit step with CEL `${…}` in params.name — NOT a takeover
// (statically unknown), synthesis runs, a duplicate is acceptable.
func TestSynthesizeModuleInstalls_CELNameNotTakeover(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Install computed plugin
    module: core.module.installed
    params:
      name: "${ input.plugin }"
  - name: Configure redis
    module: community.redis.config
    params:
      settings: {}
`)
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if !reflect.DeepEqual(aliases, []string{"community"}) {
		t.Fatalf("aliases = %v, want [community] (a CEL name does not suppress synthesis)", aliases)
	}
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	assertSynthTask(t, out[1], "community", "v1.0.0")
}

// (f+) params.name NOT a string — also not a takeover, synthesis is not
// suppressed. Tasks are built directly: the parser would reject such a step by
// the core.module schema (name: string), but the runtime function must be
// fail-safe even on a raw plan.
func TestSynthesizeModuleInstalls_NonStringNameNotTakeover(t *testing.T) {
	tasks := []Task{
		{Name: "Weird install", Module: &ModuleTask{Module: "core.module.installed", Params: map[string]any{"name": 42}}},
		{Name: "Configure redis", Module: &ModuleTask{Module: "community.redis.config", Params: map[string]any{}}},
	}
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if !reflect.DeepEqual(aliases, []string{"community"}) {
		t.Fatalf("aliases = %v, want [community] (a non-string name does not suppress synthesis)", aliases)
	}
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	assertSynthTask(t, out[1], "community", "v1.0.0")
}

// (h) Empty/nil modules[] → input byte-for-byte (the same slice, no copies).
func TestSynthesizeModuleInstalls_EmptyModules(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Configure redis
    module: community.redis.config
    params:
      settings: {}
`)
	for _, modules := range [][]DependencyRef{nil, {}} {
		out, aliases := SynthesizeModuleInstalls(tasks, modules)
		if aliases != nil {
			t.Errorf("aliases = %v, want nil", aliases)
		}
		if len(out) != len(tasks) || &out[0] != &tasks[0] {
			t.Errorf("modules=%v: input must return byte-for-byte (the same slice)", modules)
		}
	}
}

// (i) A core.* entry in modules[] is skipped (defense-in-depth: service.yml
// validation already forbids it).
func TestSynthesizeModuleInstalls_CorePrefixSkipped(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Install package
    module: core.pkg.installed
    params:
      name: curl
`)
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "core.pkg", Ref: "v1.0.0"}})
	if len(aliases) != 0 || len(out) != 1 {
		t.Errorf("core.* entry not skipped: aliases=%v len=%d", aliases, len(out))
	}
}

// (j) Several ARTIFACTS: each gets its own position (before ITS OWN first
// consumer), the order of the source tasks does not shift; with a shared first
// consumer — manifest order.
func TestSynthesizeModuleInstalls_MultipleArtifacts(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Use b
    module: beta.tool.setup
    params: {}
  - name: Warmup
    module: core.exec.run
    changed_when: false
    params:
      cmd: "true"
  - name: Use a
    module: alpha.tool.setup
    params: {}
`)
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{
		{Name: "alpha.tool", Ref: "v1"},
		{Name: "beta.tool", Ref: "v2"},
	})
	if !reflect.DeepEqual(aliases, []string{"alpha", "beta"}) {
		t.Fatalf("aliases = %v, want [alpha beta] (manifest order)", aliases)
	}
	if len(out) != 5 {
		t.Fatalf("len(out) = %d, want 5", len(out))
	}
	assertSynthTask(t, out[0], "beta", "v2")
	if out[1].Name != "Use b" || out[2].Name != "Warmup" {
		t.Errorf("positions shifted: %q %q", out[1].Name, out[2].Name)
	}
	assertSynthTask(t, out[3], "alpha", "v1")
	if out[4].Name != "Use a" {
		t.Errorf("out[4] = %q, want Use a", out[4].Name)
	}

	// Shared first consumer (both artifacts inside one block) → insertions in
	// manifest order before the block.
	shared := synthTasks(t, `
name: create
tasks:
  - name: Deploy group
    block:
      - name: Use a
        module: alpha.tool.setup
        params: {}
      - name: Use b
        module: beta.tool.setup
        params: {}
`)
	out2, aliases2 := SynthesizeModuleInstalls(shared, []DependencyRef{
		{Name: "alpha.tool", Ref: "v1"},
		{Name: "beta.tool", Ref: "v2"},
	})
	if !reflect.DeepEqual(aliases2, []string{"alpha", "beta"}) {
		t.Fatalf("aliases2 = %v, want [alpha beta]", aliases2)
	}
	if len(out2) != 3 {
		t.Fatalf("len(out2) = %d, want 3", len(out2))
	}
	assertSynthTask(t, out2[0], "alpha", "v1")
	assertSynthTask(t, out2[1], "beta", "v2")
	if out2[2].Block == nil {
		t.Errorf("out2[2] must remain a block")
	}
}

// (j+) One artifact serving TWO modules is ONE install, before the earliest of
// their consumers.
//
// This is the documented shape — "an artifact serving `acl`, `config` and `info`
// registers three addresses" (shared/pluginhost) — and the two entries name one
// slot, so a second step would fetch the same bytes over the same directory. The
// position is the EARLIER consumer's: installing before the second one would leave
// the first unresolvable.
func TestSynthesizeModuleInstalls_OneArtifactManyModules(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Warmup
    module: core.exec.run
    changed_when: false
    params:
      cmd: "true"
  - name: Configure redis
    module: community.redis.config
    params:
      settings: {}
  - name: Set up sentinel
    module: community.sentinel.present
    params: {}
`)
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{
		{Name: "community.sentinel", Ref: "v1.0.0"},
		{Name: "community.redis", Ref: "v1.0.0"},
	})
	if !reflect.DeepEqual(aliases, []string{"community"}) {
		t.Fatalf("aliases = %v, want [community] — two modules of one artifact are ONE install", aliases)
	}
	if len(out) != 4 {
		t.Fatalf("len(out) = %d, want 4 (one synthesized step)", len(out))
	}
	// The sentinel entry comes first in the manifest and would have planted the
	// install before task #3; the redis consumer at #2 pulls it earlier.
	assertSynthTask(t, out[1], "community", "v1.0.0")
	if out[2].Name != "Configure redis" || out[3].Name != "Set up sentinel" {
		t.Errorf("install landed after a consumer: %q %q", out[2].Name, out[3].Name)
	}
}

// Stratify integration (roster axis ADR-0061 §S2): a plan [refresh-emitter,
// community.x consumer] + synthesis → the synthesized step (roster consumer:
// on: omitted) lands in a Passage STRICTLY AFTER the refresh boundary, together
// with its consumer — NOT in Passage 0 (otherwise install would go to the
// pre-onboarding roster).
func TestSynthesizeModuleInstalls_StratifyAfterRefreshBoundary(t *testing.T) {
	tasks := synthTasks(t, `
name: create
tasks:
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
  - name: Configure redis on grown roster
    module: community.redis.config
    params:
      settings: {}
`)
	out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if !reflect.DeepEqual(aliases, []string{"community"}) {
		t.Fatalf("aliases = %v, want [community]", aliases)
	}
	p, err := Stratify(out)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (refresh boundary)", p.Count)
	}
	want := []int{0, 1, 1} // emitter / synth-install / consumer
	for i, w := range want {
		if p.TaskPassage[i] != w {
			t.Errorf("task #%d passage = %d, want %d (synth step -- roster consumer AFTER the refresh boundary)", i, p.TaskPassage[i], w)
		}
	}
}

// TestSynthesizeModuleInstalls_TakeoverKeyIsAddressLevel1 — NIM-543, stated as the
// property that the two halves of the takeover comparison meet.
//
// The manifest entry is `<alias>.<module>`, the explicit step writes a bare alias, so
// the only place they can agree is address level 1. Keying the map on the literal as
// written and reading it by alias made the documented escape hatch silently
// conditional on spelling: `name: community` worked, `name: community.redis` — the
// pre-NIM-377 form still shown in older material — took over nothing, and the
// synthesizer inserted a SECOND install of the same artifact beside the operator's,
// with both steps failing on the host for the value's own sake.
//
// Tasks are built directly rather than parsed: since the offline half of this fix a
// dotted `params.name` is a validation error, and the point here is that the runtime
// function is right on a plan that reached it anyway (render_host and the trial
// harness replay a stored artifact, they do not re-validate).
func TestSynthesizeModuleInstalls_TakeoverKeyIsAddressLevel1(t *testing.T) {
	for _, literal := range []string{"community", "community.redis", "community.redis.config"} {
		tasks := []Task{
			{Name: "Operator installs plugin explicitly", Module: &ModuleTask{
				Module: moduleInstalledAddr, Params: map[string]any{"name": literal}}},
			{Name: "Configure redis", Module: &ModuleTask{
				Module: "community.redis.config", Params: map[string]any{}}},
		}
		out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
		if len(aliases) != 0 || len(out) != 2 {
			t.Errorf("explicit install name %q: aliases=%v len(out)=%d, want empty/2 — "+
				"the operator's own step was not recognized and a second install of slot %q was inserted beside it (NIM-543)",
				literal, aliases, len(out), "community")
		}
	}
}

// TestSynthesizeModuleInstalls_TakeoverIsAnotherSlot — the other side of the same
// property: level 1 must MATCH. A step installing a different slot suppresses
// nothing, or one explicit install would silence the whole manifest.
func TestSynthesizeModuleInstalls_TakeoverIsAnotherSlot(t *testing.T) {
	for _, literal := range []string{"acme-tools", "acme-tools.probe", "communityx"} {
		tasks := []Task{
			{Name: "Install something else", Module: &ModuleTask{
				Module: moduleInstalledAddr, Params: map[string]any{"name": literal}}},
			{Name: "Configure redis", Module: &ModuleTask{
				Module: "community.redis.config", Params: map[string]any{}}},
		}
		out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
		if !reflect.DeepEqual(aliases, []string{"community"}) || len(out) != 3 {
			t.Errorf("explicit install name %q: aliases=%v len(out)=%d, want [community]/3 — "+
				"a step naming another slot suppressed synthesis for community", literal, aliases, len(out))
		}
	}
}

// TestSynthesizeModuleInstalls_TakeoverKeyedOnTheBaseAddress — the negative case that
// pins WHAT makes a task a takeover.
//
// `core.pkg.installed` also has state `installed` and also takes `name`. Only
// `core.module.installed` installs a module slot, so only it can take one over;
// recognizing a takeover by the state suffix or by the presence of a `name` param
// would let an unrelated package step delete the install the service depends on.
func TestSynthesizeModuleInstalls_TakeoverKeyedOnTheBaseAddress(t *testing.T) {
	for _, addr := range []string{"core.pkg.installed", "core.service.running", "community.redis.installed"} {
		tasks := []Task{
			{Name: "Not an install step", Module: &ModuleTask{
				Module: addr, Params: map[string]any{"name": "community"}}},
			{Name: "Configure redis", Module: &ModuleTask{
				Module: "community.redis.config", Params: map[string]any{}}},
		}
		out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
		if !reflect.DeepEqual(aliases, []string{"community"}) {
			t.Errorf("a %s task was read as a takeover of the community slot: aliases=%v, want [community]", addr, aliases)
		}
		if len(out) != 3 {
			t.Errorf("%s: len(out) = %d, want 3", addr, len(out))
		}
	}
}

// TestSynthesizeModuleInstalls_EveryReservedNameIsSkipped — the second-line defence
// covers the whole reserved list, not just `core.`.
//
// `service.yml` validation rejects all of them (`plugin.IsReserved`, 19 names); this
// end used to re-check `core.` alone, so `keeper.*` and `soul.*` — Tier 1 engine
// namespaces sitting beside `core` on the same list — would have been synthesized
// into an install step by any path that reached the synthesizer without that
// validation. Both ends now read the one list.
func TestSynthesizeModuleInstalls_EveryReservedNameIsSkipped(t *testing.T) {
	for _, reserved := range plugin.ReservedNames() {
		dep := reserved + ".thing"
		tasks := []Task{{Name: "Consumer", Module: &ModuleTask{
			Module: dep + ".present", Params: map[string]any{}}}}
		out, aliases := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: dep, Ref: "v1.0.0"}})
		if len(aliases) != 0 || len(out) != 1 {
			t.Errorf("reserved name %q was synthesized an install step: aliases=%v len(out)=%d, want empty/1 — "+
				"no plugin can be registered under it, so the step could only fail", dep, aliases, len(out))
		}
	}
}
