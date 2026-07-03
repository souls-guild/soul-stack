package config

import (
	"reflect"
	"testing"
)

// Guard-тесты синтеза install-шагов `core.module.installed` из
// `service.yml::modules[]` (ADR-065, NIM-8): Keeper вставляет Soul-side
// install-шаг НЕПОСРЕДСТВЕННО ПЕРЕД первым потребителем модуля; явный
// литеральный шаг оператора (takeover) подавляет синтез.

// synthTasks парсит YAML-план тем же парсером, что прод (плоский top-level
// список задач, как после ExpandIncludes).
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

// assertSynthTask сверяет форму синтез-задачи: имя, адрес модуля, params
// {name, ref} и отсутствие оркестрационных полей (on/where/serial = весь roster).
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

// (а)+(ж) Синтез перед ПЕРВЫМ потребителем, позиция точная; ref из записи
// манифеста попадает в params.
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

// (б) Потребитель внутри block: → вставка перед block-ом ЦЕЛИКОМ.
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

// (в) Модуль без потребителей в плане → НЕ синтезируется.
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

// (г) Takeover: явный top-level core.module.installed с ЛИТЕРАЛЬНЫМ params.name
// подавляет синтез этого имени (даже стоя ПОСЛЕ потребителя — оператор сам решил).
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

// (д) Takeover внутри block: тоже распознаётся.
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

// (е) Явный шаг с CEL `${…}` в params.name — НЕ takeover (статически неизвестен),
// синтез выполняется, дубль допустим.
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

// (е+) params.name НЕ строка — тоже не takeover, синтез не подавлен. Задачи
// строятся напрямую: парсер такой шаг отверг бы по схеме core.module (name:
// string), но рантайм-функция обязана быть fail-safe и на сыром плане.
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

// (з) Пустой/nil modules[] → вход бит-в-бит (тот же slice, без копий).
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

// (и) core.*-запись в modules[] пропускается (defense-in-depth: валидация
// service.yml её уже запрещает).
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

// (к) Несколько модулей: у каждого своя позиция (перед СВОИМ первым
// потребителем), порядок исходных задач не съезжает; при общем первом
// потребителе — порядок манифеста.
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

	// Общий первый потребитель (оба модуля внутри одного block) → вставки в
	// порядке манифеста перед block-ом.
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

// Stratify-интеграция (roster-ось ADR-0061 §S2): план [refresh-эмиттер,
// потребитель community.x] + синтез → синтез-шаг (roster-потребитель: on: опущен)
// попадает в Passage СТРОГО ПОСЛЕ refresh-границы, вместе со своим потребителем —
// НЕ в Passage 0 (иначе install поехал бы на до-онбординговый roster).
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
	want := []int{0, 1, 1} // эмиттер / синтез-install / потребитель
	for i, w := range want {
		if p.TaskPassage[i] != w {
			t.Errorf("task #%d passage = %d, want %d (синтез-шаг — roster-потребитель ПОСЛЕ refresh-границы)", i, p.TaskPassage[i], w)
		}
	}
}
