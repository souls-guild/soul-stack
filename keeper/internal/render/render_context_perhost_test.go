package render

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// TestRenderContext_PerHostSelf_Dispatch — a DIRECT regression guard for the
// CORE bug in per-host render_context.self (architect-traced, partially closes
// open Q #25).
//
// Bug root cause: the per-host loop renderTaskIter correctly built each host's
// render_context, but only the first-by-SID one ended up in rt.Params; a single
// *RenderedTask (pointer) was dispatched to EVERY host (groupByHost/claim), so a
// self-varying template (`{{ .self.network.primary_ip }}`) silently rendered
// with the FIRST host's facts on ALL hosts (and render_context was excluded from
// the host-invariance check → fail-closed never triggered).
//
// Fix (Variant A): renderTaskIter materializes each host's render_context into
// RenderedTask.RenderContextBySID[SID]; ToProtoTasksForHost(tasks, sid)
// substitutes the per-host variant into a specific SID's ApplyRequest.tasks. A
// test with 3 hosts with DIFFERENT primary_ip proves each of the 3 wire
// RenderedTasks carries ITS OWN IP (not the first's) — both in render_context
// itself AND in the real Soul render.
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
	// Intentionally NOT in lexicographic order: the first by SID (db-1) is not
	// first in the list. The bug would substitute the IP of the first-by-SID host
	// (db-1 → 10.0.0.99) everywhere.
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
		t.Fatalf("expected one task, got %d", len(tasks))
	}
	rt := tasks[0]

	// (1) Per-host materialization: RenderContextBySID carries EACH SID's OWN IP.
	if len(rt.RenderContextBySID) != len(hosts) {
		t.Fatalf("RenderContextBySID size = %d, want %d (matching host count)", len(rt.RenderContextBySID), len(hosts))
	}
	for _, h := range hosts {
		rc := rt.RenderContextBySID[h.sid]
		if rc == nil {
			t.Fatalf("RenderContextBySID[%s] missing", h.sid)
		}
		if got := selfPrimaryIP(rc.AsMap()); got != h.ip {
			t.Fatalf("RenderContextBySID[%s].self.network.primary_ip = %q, want %q", h.sid, got, h.ip)
		}
	}

	// (2) Per-SID wire form: ToProtoTasksForHost(sid) puts EXACTLY this host's IP
	//     into render_context params (not the first-by-SID one). This is what
	//     actually goes into a specific Soul's ApplyRequest.tasks.
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
			t.Fatalf("host %s: wire render_context.self.network.primary_ip = %q, want %q (a bug would substitute the first by SID)", h.sid, got, h.ip)
		}
		// The real Soul render of the template with this render_context: the
		// string must carry ITS OWN IP and NOT anyone else's.
		out, rerr := engine.Render(wire[0].GetParams().GetFields()[paramTemplateContent].GetStringValue(), rc)
		if rerr != nil {
			t.Fatalf("host %s: soul-render failed: %v", h.sid, rerr)
		}
		if !strings.Contains(out, "primary_ip "+h.ip) {
			t.Fatalf("host %s: render does not carry its own primary_ip %s:\n%s", h.sid, h.ip, out)
		}
		for _, other := range hosts {
			if other.ip == h.ip {
				continue
			}
			if strings.Contains(out, "primary_ip "+other.ip) {
				t.Fatalf("host %s renders ANOTHER host's primary_ip %s (CORE bug returned):\n%s", h.sid, other.ip, out)
			}
		}
	}

	// (3) t.Params (golden path) is NOT mutated by the overlay: it stays the
	//     render_context of the first-by-SID host (db-1 → 10.0.0.99). Otherwise a
	//     single *RenderedTask dispatched to multiple SIDs would leak between
	//     hosts.
	if got := selfPrimaryIP(rt.Params.GetFields()[paramRenderContext].GetStructValue().AsMap()); got != "10.0.0.99" {
		t.Fatalf("t.Params.render_context.self.primary_ip = %q, want 10.0.0.99 (first by SID, golden-path) - overlay must not mutate Params", got)
	}

	// The plan targets all 3 hosts (sanity: the per-host map agrees with dispatch).
	if len(plans) != 1 || len(plans[0].TargetSIDs) != len(hosts) {
		t.Fatalf("DispatchPlan: want 1 plan for %d SID, got %d plan(s)", len(hosts), len(plans))
	}
}

// TestRenderContext_PerHostSelf_SingleHostGoldenPath — N=1: RenderContextBySID
// is NOT populated (no overlay needed), render_context travels in Params as
// before (bit-for-bit). Guarantees the fix doesn't bloat the single-host path.
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
		t.Fatalf("N=1: RenderContextBySID should be nil (overlay not needed), got %#v", tasks[0].RenderContextBySID)
	}
	// Params carries this single host's render_context; ToProtoTasks (without
	// SID) passes through as-is.
	wire := ToProtoTasks(tasks)
	rc := wire[0].GetParams().GetFields()[paramRenderContext].GetStructValue().AsMap()
	if got := selfPrimaryIP(rc); got != "10.0.0.7" {
		t.Fatalf("N=1 wire render_context.self.primary_ip = %q, want 10.0.0.7", got)
	}
}

// selfPrimaryIP extracts render_context.self.network.primary_ip from the
// AsMap() form (empty string if absent — the test compares against expected).
func selfPrimaryIP(rc map[string]any) string {
	self, _ := rc["self"].(map[string]any)
	network, _ := self["network"].(map[string]any)
	ip, _ := network["primary_ip"].(string)
	return ip
}
