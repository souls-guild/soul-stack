package config

import (
	"reflect"
	"testing"
)

// Guard tests for synthesizing `core.module.installed` install steps from
// `service.yml::modules[]` (ADR-065, NIM-8): Keeper inserts the Soul-side
// install step IMMEDIATELY BEFORE the module's first consumer; an explicit
// literal operator step (takeover) suppresses synthesis.

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
func assertSynthTask(t *testing.T, task Task, module, ref string) {
	t.Helper()
	wantName := "install " + module + " (service manifest)"
	if task.Name != wantName {
		t.Errorf("synth name = %q, want %q", task.Name, wantName)
	}
	if task.Module == nil || task.Module.Module != "core.module.installed" {
		t.Fatalf("synth module = %+v, want core.module.installed", task.Module)
	}
	wantParams := map[string]any{"name": module, "ref": ref}
	if !reflect.DeepEqual(task.Module.Params, wantParams) {
		t.Errorf("synth params = %v, want %v", task.Module.Params, wantParams)
	}
	if task.On != nil || task.Where != "" || task.Serial != nil || task.RunOnce {
		t.Errorf("synth несёт оркестрационные поля (on=%v where=%q serial=%v run_once=%v), want чистая roster-задача",
			task.On, task.Where, task.Serial, task.RunOnce)
	}
	if task.IncludeGroupID != 0 || task.IncludeWhen != "" {
		t.Errorf("synth привязан к include-группе (%d, %q), want вне групп", task.IncludeGroupID, task.IncludeWhen)
	}
}

// (a)+(g) Synthesis before the FIRST consumer, exact position; ref from the
// manifest entry lands in params.
func TestSynthesizeModuleInstalls_BeforeFirstConsumer(t *testing.T) {
	tasks := synthTasks(t, `
name: create
state_changes: {}
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
	out, names := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.2.3"}})
	if len(out) != 4 {
		t.Fatalf("len(out) = %d, want 4", len(out))
	}
	if !reflect.DeepEqual(names, []string{"community.redis"}) {
		t.Errorf("names = %v, want [community.redis]", names)
	}
	assertSynthTask(t, out[1], "community.redis", "v1.2.3")
	if out[0].Name != "Warmup" || out[2].Name != "Configure redis" || out[3].Name != "ACL redis" {
		t.Errorf("порядок задач съехал: %q %q %q", out[0].Name, out[2].Name, out[3].Name)
	}
}

// (b) Consumer inside a block: → insertion before the WHOLE block.
func TestSynthesizeModuleInstalls_ConsumerInsideBlock(t *testing.T) {
	tasks := synthTasks(t, `
name: create
state_changes: {}
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
	assertSynthTask(t, out[1], "community.redis", "v1.0.0")
	if out[2].Block == nil {
		t.Fatalf("out[2] должен остаться block-ом (вставка перед block-ом целиком)")
	}
}

// (c) A module with no consumers in the plan → NOT synthesized.
func TestSynthesizeModuleInstalls_NoConsumerNoSynth(t *testing.T) {
	tasks := synthTasks(t, `
name: create
state_changes: {}
tasks:
  - name: Warmup
    module: core.exec.run
    changed_when: false
    params:
      cmd: "true"
`)
	out, names := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if len(names) != 0 {
		t.Errorf("names = %v, want пусто", names)
	}
	if len(out) != 1 || out[0].Name != "Warmup" {
		t.Errorf("план изменён без потребителей: %+v", out)
	}
}

// (d) Takeover: an explicit top-level core.module.installed with a LITERAL
// params.name suppresses synthesis of that name (even standing AFTER the
// consumer — the operator decided).
func TestSynthesizeModuleInstalls_TakeoverTopLevel(t *testing.T) {
	tasks := synthTasks(t, `
name: create
state_changes: {}
tasks:
  - name: Operator installs plugin explicitly
    module: core.module.installed
    params:
      name: community.redis
  - name: Configure redis
    module: community.redis.config
    params:
      settings: {}
`)
	out, names := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if len(names) != 0 {
		t.Errorf("names = %v, want пусто (takeover)", names)
	}
	if len(out) != 2 {
		t.Errorf("len(out) = %d, want 2 (без синтеза)", len(out))
	}
}

// (e) Takeover inside a block: also recognized.
func TestSynthesizeModuleInstalls_TakeoverInsideBlock(t *testing.T) {
	tasks := synthTasks(t, `
name: create
state_changes: {}
tasks:
  - name: Provision group
    block:
      - name: Install plugin
        module: core.module.installed
        params:
          name: community.redis
  - name: Configure redis
    module: community.redis.config
    params:
      settings: {}
`)
	out, names := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if len(names) != 0 || len(out) != 2 {
		t.Errorf("takeover в block не распознан: names=%v len=%d, want пусто/2", names, len(out))
	}
}

// (f) An explicit step with CEL `${…}` in params.name — NOT a takeover
// (statically unknown), synthesis runs, a duplicate is acceptable.
func TestSynthesizeModuleInstalls_CELNameNotTakeover(t *testing.T) {
	tasks := synthTasks(t, `
name: create
state_changes: {}
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
	out, names := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if !reflect.DeepEqual(names, []string{"community.redis"}) {
		t.Fatalf("names = %v, want [community.redis] (CEL-имя не подавляет синтез)", names)
	}
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	assertSynthTask(t, out[1], "community.redis", "v1.0.0")
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
	out, names := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if !reflect.DeepEqual(names, []string{"community.redis"}) {
		t.Fatalf("names = %v, want [community.redis] (не-строковое имя не подавляет синтез)", names)
	}
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	assertSynthTask(t, out[1], "community.redis", "v1.0.0")
}

// (h) Empty/nil modules[] → input byte-for-byte (the same slice, no copies).
func TestSynthesizeModuleInstalls_EmptyModules(t *testing.T) {
	tasks := synthTasks(t, `
name: create
state_changes: {}
tasks:
  - name: Configure redis
    module: community.redis.config
    params:
      settings: {}
`)
	for _, modules := range [][]DependencyRef{nil, {}} {
		out, names := SynthesizeModuleInstalls(tasks, modules)
		if names != nil {
			t.Errorf("names = %v, want nil", names)
		}
		if len(out) != len(tasks) || &out[0] != &tasks[0] {
			t.Errorf("modules=%v: вход должен вернуться бит-в-бит (тот же slice)", modules)
		}
	}
}

// (i) A core.* entry in modules[] is skipped (defense-in-depth: service.yml
// validation already forbids it).
func TestSynthesizeModuleInstalls_CorePrefixSkipped(t *testing.T) {
	tasks := synthTasks(t, `
name: create
state_changes: {}
tasks:
  - name: Install package
    module: core.pkg.installed
    params:
      name: curl
`)
	out, names := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "core.pkg", Ref: "v1.0.0"}})
	if len(names) != 0 || len(out) != 1 {
		t.Errorf("core.*-запись не пропущена: names=%v len=%d", names, len(out))
	}
}

// (j) Several modules: each gets its own position (before ITS OWN first
// consumer), the order of the source tasks does not shift; with a shared first
// consumer — manifest order.
func TestSynthesizeModuleInstalls_MultipleModules(t *testing.T) {
	tasks := synthTasks(t, `
name: create
state_changes: {}
tasks:
  - name: Use b
    module: community.b.setup
    params: {}
  - name: Warmup
    module: core.exec.run
    changed_when: false
    params:
      cmd: "true"
  - name: Use a
    module: community.a.setup
    params: {}
`)
	out, names := SynthesizeModuleInstalls(tasks, []DependencyRef{
		{Name: "community.a", Ref: "v1"},
		{Name: "community.b", Ref: "v2"},
	})
	if !reflect.DeepEqual(names, []string{"community.a", "community.b"}) {
		t.Fatalf("names = %v, want [community.a community.b] (порядок манифеста)", names)
	}
	if len(out) != 5 {
		t.Fatalf("len(out) = %d, want 5", len(out))
	}
	assertSynthTask(t, out[0], "community.b", "v2")
	if out[1].Name != "Use b" || out[2].Name != "Warmup" {
		t.Errorf("позиции съехали: %q %q", out[1].Name, out[2].Name)
	}
	assertSynthTask(t, out[3], "community.a", "v1")
	if out[4].Name != "Use a" {
		t.Errorf("out[4] = %q, want Use a", out[4].Name)
	}

	// Shared first consumer (both modules inside one block) → insertions in
	// manifest order before the block.
	shared := synthTasks(t, `
name: create
state_changes: {}
tasks:
  - name: Deploy group
    block:
      - name: Use a
        module: community.a.setup
        params: {}
      - name: Use b
        module: community.b.setup
        params: {}
`)
	out2, names2 := SynthesizeModuleInstalls(shared, []DependencyRef{
		{Name: "community.a", Ref: "v1"},
		{Name: "community.b", Ref: "v2"},
	})
	if !reflect.DeepEqual(names2, []string{"community.a", "community.b"}) {
		t.Fatalf("names2 = %v, want [community.a community.b]", names2)
	}
	if len(out2) != 3 {
		t.Fatalf("len(out2) = %d, want 3", len(out2))
	}
	assertSynthTask(t, out2[0], "community.a", "v1")
	assertSynthTask(t, out2[1], "community.b", "v2")
	if out2[2].Block == nil {
		t.Errorf("out2[2] должен остаться block-ом")
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
state_changes: {}
tasks:
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Configure redis on grown roster
    module: community.redis.config
    on: ["${ incarnation.name }"]
    params:
      settings: {}
`)
	out, names := SynthesizeModuleInstalls(tasks, []DependencyRef{{Name: "community.redis", Ref: "v1.0.0"}})
	if !reflect.DeepEqual(names, []string{"community.redis"}) {
		t.Fatalf("names = %v, want [community.redis]", names)
	}
	p, err := Stratify(out)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (refresh-граница)", p.Count)
	}
	want := []int{0, 1, 1} // emitter / synth-install / consumer
	for i, w := range want {
		if p.TaskPassage[i] != w {
			t.Errorf("task #%d passage = %d, want %d (синтез-шаг — roster-потребитель ПОСЛЕ refresh-границы)", i, p.TaskPassage[i], w)
		}
	}
}
