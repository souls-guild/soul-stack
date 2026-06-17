package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// hostWithRole — HostFacts со SID/coven/role + soulprint (network/os) для тестов
// soulprint.hosts-проекции.
func hostWithRole(sid, role string, coven []string, network, os map[string]any) *topology.HostFacts {
	return &topology.HostFacts{
		SID:       sid,
		Coven:     coven,
		Role:      role,
		Soulprint: map[string]any{"network": network, "os": os},
	}
}

func hostsRunInput(manifest *config.ScenarioManifest) RenderInput {
	return RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "prod"},
		Hosts: []*topology.HostFacts{
			hostWithRole("web-1.example.com", "primary", []string{"prod", "web"},
				map[string]any{"primary_ip": "10.0.0.1"}, map[string]any{"family": "debian"}),
			hostWithRole("web-2.example.com", "replica", []string{"prod", "web"},
				map[string]any{"primary_ip": "10.0.0.2"}, map[string]any{"family": "debian"}),
			hostWithRole("db-1.example.com", "replica", []string{"prod", "db"},
				map[string]any{"primary_ip": "10.0.0.3"}, map[string]any{"family": "rhel"}),
		},
	}
}

// moduleScenario — сценарий из одной module-задачи с заданными params.
func moduleScenario(module string, params map[string]any) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{Name: "t", Module: &config.ModuleTask{Module: module, Params: params}},
		},
	}
}

// TestRender_SoulprintHosts_Projection — soulprint.hosts проецируется из in.Hosts
// в scenario-проходе: размер списка = числу хостов прогона.
func TestRender_SoulprintHosts_Projection(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": "echo ${ soulprint.hosts.size() }",
	}))
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo 3" {
		t.Fatalf("command = %q, want echo 3", got)
	}
}

// TestRender_SoulprintHostsWhere_FirstByIndex — фильтр + [0]: cross-host primary
// discovery (declared-роль), первый элемент через [0].
func TestRender_SoulprintHostsWhere_FirstByIndex(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": `redis-cli replicaof ${ soulprint.hosts.where("role == 'primary'")[0].network.primary_ip } 6379`,
	}))
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "redis-cli replicaof 10.0.0.1 6379" {
		t.Fatalf("command = %q", got)
	}
}

// TestRender_SoulprintWhere_Synonym — soulprint.where(...) ≡ soulprint.hosts.where(...).
func TestRender_SoulprintWhere_Synonym(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": `count=${ size(soulprint.where("'web' in covens")) }`,
	}))
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "count=2" {
		t.Fatalf("command = %q, want count=2", got)
	}
}

// TestRender_SoulprintHostsChoirs_CrossHost — soulprint.hosts[].choirs виден
// cross-host (ADR-044, S-T4): фильтр по choir-членству среди хостов прогона.
func TestRender_SoulprintHostsChoirs_CrossHost(t *testing.T) {
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": `count=${ size(soulprint.hosts.where("'voters' in choirs")) }`,
	}))
	in.Hosts[0].Choirs = []string{"primaries", "voters"}
	in.Hosts[1].Choirs = []string{"voters"}
	// in.Hosts[2] — без choir-членств.

	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "count=2" {
		t.Fatalf("command = %q, want count=2", got)
	}
}

// TestRender_LoopOverSoulprintHostsWhere — loop.items над soulprint.hosts.where(...):
// раскрывается в N задач по отфильтрованным хостам прогона (declared-роль).
func TestRender_LoopOverSoulprintHostsWhere(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{
				Name: "per replica",
				Loop: &config.LoopSpec{
					Items: `${ soulprint.hosts.where("role == 'replica'") }`,
					As:    "replica",
				},
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "echo ${ replica.network.primary_ip }"},
				},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(manifest)
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (две реплики)", len(tasks))
	}
	got := []string{
		tasks[0].Params.GetFields()["cmd"].GetStringValue(),
		tasks[1].Params.GetFields()["cmd"].GetStringValue(),
	}
	want := map[string]bool{"echo 10.0.0.2": false, "echo 10.0.0.3": false}
	for _, g := range got {
		if _, ok := want[g]; !ok {
			t.Fatalf("неожиданный command %q (got %v)", g, got)
		}
		want[g] = true
	}
	for cmd, seen := range want {
		if !seen {
			t.Fatalf("ожидали command %q среди %v", cmd, got)
		}
	}
}

// TestRender_DestinyIsolation_HostsForbidden — soulprint.hosts в destiny-проходе
// → ошибка валидации (изоляция, orchestration.md §4.1), не тихий пустой list.
func TestRender_DestinyIsolation_HostsForbidden(t *testing.T) {
	d := &ResolvedDestiny{
		Name:  "leaky",
		Input: config.InputSchemaMap{},
		Tasks: []config.Task{
			{
				Name: "leak hosts",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "echo ${ soulprint.hosts.size() }"},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)

	in := hostsRunInput(applyScenario("leaky", map[string]any{}))
	in.Destiny = res

	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatalf("ожидали ошибку изоляции для soulprint.hosts в destiny")
	}
	if !strings.Contains(err.Error(), "soulprint.hosts") {
		t.Fatalf("ожидали сообщение про soulprint.hosts, получили: %v", err)
	}
}

// TestRender_DestinyIsolation_SelfStillWorks — soulprint.self в destiny остаётся
// доступен (per-host факты — стабильный слой), изоляция касается только hosts.
func TestRender_DestinyIsolation_SelfStillWorks(t *testing.T) {
	d := &ResolvedDestiny{
		Name:  "ok",
		Input: config.InputSchemaMap{},
		Tasks: []config.Task{
			{
				Name: "use self",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"cmd": "echo ${ soulprint.self.os.family }"},
				},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)

	in := RenderInput{
		Scenario:    applyScenario("ok", map[string]any{}),
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "prod"},
		Hosts: []*topology.HostFacts{
			hostWithRole("web-1.example.com", "primary", []string{"prod"},
				map[string]any{"primary_ip": "10.0.0.1"}, map[string]any{"family": "debian"}),
		},
		Destiny: res,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo debian" {
		t.Fatalf("command = %q, want echo debian", got)
	}
}

// TestRender_EmptyFilterIndex0_StepError — where никого не отобрал → [0] над
// пустым списком → ошибка шага рендера (понятная, не паника). «primary не
// найден» — это ошибка шага, а не тихий null.
func TestRender_EmptyFilterIndex0_StepError(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": `replicaof ${ soulprint.hosts.where("role == 'no-such'")[0].network.primary_ip }`,
	}))
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatalf("ожидали ошибку рендера для [0] над пустым фильтром")
	}
}

// TestRender_HostMissingFactSection — qa: при отсутствии секции (network/os) в
// Soulprint хоста render подставляет пустой map, поэтому обращение к полю даёт
// «no such key» по самому полю (family/primary_ip), а не по секции (os/network).
// Закрепляем фактическое поведение: понятная ошибка рендера, не паника.
func TestRender_HostMissingFactSection(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hostsRunInput(moduleScenario("core.exec.run", map[string]any{
		"cmd": `fam=${ soulprint.hosts.where("os.family == 'debian'").size() }`,
	}))
	// Хост без секции os: Soulprint без ключа os → soulprintSection вернёт {}.
	in.Hosts = []*topology.HostFacts{
		{SID: "x.example.com", Coven: []string{"prod"}, Role: "primary", Soulprint: map[string]any{}},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatalf("ожидали ошибку рендера для where над хостом без факта os")
	}
	if !strings.Contains(err.Error(), "family") {
		t.Fatalf("ожидали no-such-key по полю family (секция os = пустой map), получили: %v", err)
	}
}
