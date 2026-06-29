package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// TestRenderContext_PerHostSelf_Dispatch — ПРЯМОЙ regress-guard на CORE-баг
// per-host render_context.self (architect-traced, частичное закрытие open Q №25).
//
// Корень бага: per-host цикл renderTaskIter правильно собирал render_context
// КАЖДОГО хоста, но в rt.Params уезжал лишь первый по SID; один *RenderedTask
// (указатель) диспатчился КАЖДОМУ хосту (groupByHost/claim), поэтому
// self-вариативный шаблон (`{{ .self.network.primary_ip }}`) на ВСЕХ хостах тихо
// рендерился фактами ПЕРВОГО хоста (а render_context был исключён из
// host-инвариантной сверки → fail-closed не срабатывал).
//
// Фикс (Вариант A): renderTaskIter материализует render_context каждого хоста в
// RenderedTask.RenderContextBySID[SID]; ToProtoTasksForHost(tasks, sid)
// подставляет per-host вариант в ApplyRequest.tasks конкретного SID. Тест на 3
// хостах с РАЗНЫМИ primary_ip доказывает, что каждый из 3 wire-RenderedTask несёт
// СВОЙ IP (не первого) — в самом render_context И в реальном Soul-рендере.
func TestRenderContext_PerHostSelf_Dispatch(t *testing.T) {
	const tmplPath = "templates/iface.conf.tmpl"
	const tmplBody = "primary_ip {{ .self.network.primary_ip }}\n"

	manifest := &config.ScenarioManifest{
		Name: "iface",
		Tasks: []config.Task{
			{
				Name: "render iface conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/app/iface.conf",
						"template": tmplPath,
					},
				},
			},
		},
	}

	type hostCase struct {
		sid string
		ip  string
	}
	// Намеренно НЕ в лексикографическом порядке: первый по SID (db-1) — не первый
	// в списке. Баг подставил бы всем IP первого ПО SID хоста (db-1 → 10.0.0.99).
	hosts := []hostCase{
		{sid: "web-2.example.com", ip: "10.0.0.2"},
		{sid: "web-1.example.com", ip: "10.0.0.1"},
		{sid: "db-1.example.com", ip: "10.0.0.99"},
	}
	hostFacts := make([]*topology.HostFacts, 0, len(hosts))
	for _, h := range hosts {
		hostFacts = append(hostFacts, hostWithRole(h.sid, "primary", []string{"prod"},
			map[string]any{"primary_ip": h.ip}, map[string]any{"family": "debian"}))
	}

	reader := fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "prod"},
		Hosts:       hostFacts,
		Templates:   reader,
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("ожидалась одна задача, got %d", len(tasks))
	}
	rt := tasks[0]

	// (1) Per-host материализация: RenderContextBySID несёт СВОЙ IP каждого SID.
	if len(rt.RenderContextBySID) != len(hosts) {
		t.Fatalf("RenderContextBySID размер = %d, want %d (по числу хостов)", len(rt.RenderContextBySID), len(hosts))
	}
	for _, h := range hosts {
		rc := rt.RenderContextBySID[h.sid]
		if rc == nil {
			t.Fatalf("RenderContextBySID[%s] отсутствует", h.sid)
		}
		if got := selfPrimaryIP(rc.AsMap()); got != h.ip {
			t.Fatalf("RenderContextBySID[%s].self.network.primary_ip = %q, want %q", h.sid, got, h.ip)
		}
	}

	// (2) Wire-форма per-SID: ToProtoTasksForHost(sid) кладёт в render_context
	//     params ИМЕННО IP этого хоста (а не первого по SID). Это то, что реально
	//     едет в ApplyRequest.tasks конкретному Soul-у.
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	for _, h := range hosts {
		wire := ToProtoTasksForHost([]*RenderedTask{rt}, h.sid)
		if len(wire) != 1 {
			t.Fatalf("ToProtoTasksForHost(%s): want 1 task, got %d", h.sid, len(wire))
		}
		rc := wire[0].GetParams().GetFields()[paramRenderContext].GetStructValue().AsMap()
		if got := selfPrimaryIP(rc); got != h.ip {
			t.Fatalf("хост %s: wire render_context.self.network.primary_ip = %q, want %q (баг подставил бы первый по SID)", h.sid, got, h.ip)
		}
		// Реальный Soul-рендер шаблона с этим render_context: строка обязана нести
		// СВОЙ IP и НЕ нести чужие.
		out, rerr := engine.Render(wire[0].GetParams().GetFields()[paramTemplateContent].GetStringValue(), rc)
		if rerr != nil {
			t.Fatalf("хост %s: soul-render упал: %v", h.sid, rerr)
		}
		if !strings.Contains(out, "primary_ip "+h.ip) {
			t.Fatalf("хост %s: рендер не несёт свой primary_ip %s:\n%s", h.sid, h.ip, out)
		}
		for _, other := range hosts {
			if other.ip == h.ip {
				continue
			}
			if strings.Contains(out, "primary_ip "+other.ip) {
				t.Fatalf("хост %s рендерит ЧУЖОЙ primary_ip %s (CORE-баг вернулся):\n%s", h.sid, other.ip, out)
			}
		}
	}

	// (3) t.Params (golden-path) НЕ мутирован overlay-ем: остаётся render_context
	//     первого ПО SID хоста (db-1 → 10.0.0.99). Иначе один *RenderedTask,
	//     диспатчимый нескольким SID, протёк бы между хостами.
	if got := selfPrimaryIP(rt.Params.GetFields()[paramRenderContext].GetStructValue().AsMap()); got != "10.0.0.99" {
		t.Fatalf("t.Params.render_context.self.primary_ip = %q, want 10.0.0.99 (первый по SID, golden-path) — overlay не должен мутировать Params", got)
	}

	// План таргетит все 3 хоста (sanity: per-host карта согласована с dispatch).
	if len(plans) != 1 || len(plans[0].TargetSIDs) != len(hosts) {
		t.Fatalf("DispatchPlan: want 1 план на %d SID, got %d план(ов)", len(hosts), len(plans))
	}
}

// TestRenderContext_PerHostSelf_SingleHostGoldenPath — N=1: RenderContextBySID НЕ
// заполняется (overlay не нужен), render_context едет в Params как раньше
// (бит-в-бит). Гарантирует, что фикс не раздувает single-host путь.
func TestRenderContext_PerHostSelf_SingleHostGoldenPath(t *testing.T) {
	const tmplPath = "templates/iface.conf.tmpl"
	manifest := &config.ScenarioManifest{
		Name: "iface",
		Tasks: []config.Task{
			{
				Name: "render iface conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{"path": "/etc/app/iface.conf", "template": tmplPath},
				},
			},
		},
	}
	reader := fakeReader{files: map[string][]byte{tmplPath: []byte("primary_ip {{ .self.network.primary_ip }}\n")}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "prod"},
		Hosts: []*topology.HostFacts{
			hostWithRole("solo.example.com", "primary", []string{"prod"},
				map[string]any{"primary_ip": "10.0.0.7"}, map[string]any{"family": "debian"}),
		},
		Templates: reader,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if tasks[0].RenderContextBySID != nil {
		t.Fatalf("N=1: RenderContextBySID должен быть nil (overlay не нужен), got %#v", tasks[0].RenderContextBySID)
	}
	// Params несёт render_context этого единственного хоста; ToProtoTasks (без SID)
	// едет как есть.
	wire := ToProtoTasks(tasks)
	rc := wire[0].GetParams().GetFields()[paramRenderContext].GetStructValue().AsMap()
	if got := selfPrimaryIP(rc); got != "10.0.0.7" {
		t.Fatalf("N=1 wire render_context.self.primary_ip = %q, want 10.0.0.7", got)
	}
}

// selfPrimaryIP достаёт render_context.self.network.primary_ip из AsMap()-формы
// (string при отсутствии — пустая, тест сравнит с ожиданием).
func selfPrimaryIP(rc map[string]any) string {
	self, _ := rc["self"].(map[string]any)
	network, _ := self["network"].(map[string]any)
	ip, _ := network["primary_ip"].(string)
	return ip
}
