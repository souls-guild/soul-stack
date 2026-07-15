package render

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// TestRenderToSoulExecute_GoldenPath closes L0-gap (BUG-A): L0-Trial asserts
// the task PLAN but doesn't EXECUTE a real Soul-render, so drift in the
// text/template context root (flat vars vs §3.2 {vars,self,role,essence})
// slipped through to E2E. This test stitches both sides together: real
// keeper-render (Pipeline.Render builds render_context + injectTemplateContent
// delivers template_content) → execution through the same engine Soul uses
// (shared/tmpl.Engine.Render) with render_context as ROOT.
//
// Template is SYNTHETIC (not from examples/): after migrating the golden-path
// redis.conf to socket-only, no standalone destiny in examples/ touches
// `.self.network.primary_ip` from the text/template phase (cluster
// replication resolves the address in the CEL phase, not the template). To
// keep the BUG-A regression (".self missing / strict-mode on missing self")
// covered, this control template stays here — it touches
// `.self.network.primary_ip`, `.self.os.family`, and `.vars.*`, exactly what
// failed with `map has no entry for key "self"` on a flat root.
func TestRenderToSoulExecute_GoldenPath(t *testing.T) {
	const tmplPath = "templates/synthetic-self.conf.tmpl"
	const tmplBody = "bind {{ .self.network.primary_ip }} 127.0.0.1\n" +
		"family {{ .self.os.family }}\n" +
		"unixsocket {{ .vars.socket }}\n" +
		"maxmemory {{ .vars.maxmemory }}\n" +
		"requirepass {{ .vars.password }}\n"

	manifest := &config.ScenarioManifest{
		Name: "redis-configure",
		Tasks: []config.Task{
			{
				Name: "render redis.conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/redis/redis.conf",
						"template": tmplPath,
						// templating.md §6: the author lifts template values into
						// params.vars; the template reads .vars.<name>.
						"vars": map[string]any{
							"socket":    "/run/redis/redis.sock",
							"password":  "${ input.password }",
							"maxmemory": "${ essence.redis.maxmemory }",
						},
					},
				},
			},
		},
	}

	host := &topology.HostFacts{
		SID:   "redis-1.example.com",
		Coven: []string{"redis-prod"},
		Soulprint: map[string]any{
			"network": map[string]any{"primary_ip": "10.0.0.7"},
			"os":      map[string]any{"family": "debian"},
		},
		Role: "primary",
	}

	reader := fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"password": "s3cr3t"},
		Essence:     map[string]any{"redis": map[string]any{"maxmemory": "512mb"}},
		Incarnation: IncarnationMeta{Name: "redis-prod"},
		Hosts:       []*topology.HostFacts{host},
		Templates:   reader,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("keeper Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	fields := tasks[0].Params.GetFields()

	// Keeper delivered literal template_content (not a path).
	tc := fields[paramTemplateContent].GetStringValue()
	if tc == "" {
		t.Fatal("template_content пуст — Keeper не доставил содержимое .tmpl")
	}
	if _, ok := fields[paramTemplate]; ok {
		t.Error("template-путь должен быть удалён из params (Soul читает только template_content)")
	}

	// Keeper built render_context = §3.2 root {vars,self,role,essence}.
	rcVal, ok := fields[paramRenderContext]
	if !ok {
		t.Fatal("render_context отсутствует в params — Keeper не собрал корень §3.2")
	}
	renderContext := rcVal.GetStructValue().AsMap()

	// Execute with the same engine Soul uses (shared/tmpl), render_context as ROOT.
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	out, err := engine.Render(tc, renderContext)
	if err != nil {
		t.Fatalf("soul-render упал (это и есть BUG-A, если падает на missing self): %v", err)
	}

	// .self.network.primary_ip substituted (used to fail "no entry for key self").
	if !strings.Contains(out, "bind 10.0.0.7 127.0.0.1") {
		t.Errorf(".self.network.primary_ip не подставлен:\n%s", out)
	}
	if !strings.Contains(out, "family debian") {
		t.Errorf(".self.os.family не подставлен:\n%s", out)
	}
	// .vars.* substituted from CEL-rendered params.
	if !strings.Contains(out, "unixsocket /run/redis/redis.sock") {
		t.Errorf(".vars.socket не подставлен:\n%s", out)
	}
	if !strings.Contains(out, "maxmemory 512mb") {
		t.Errorf(".vars.maxmemory (из essence) не подставлен:\n%s", out)
	}
	if !strings.Contains(out, "requirepass s3cr3t") {
		t.Errorf(".vars.password (из input) не подставлен:\n%s", out)
	}
}

// TestRenderToSoulExecute_CompositeSelfKeys_SnakeCase — targeted regression
// for E2E BUG-A: composite `.self.*` keys must be snake_case
// (pkg_mgr/init_system/primary_ip), canon ADR-018 / templating.md §3.2 (single
// source of truth shared with CEL soulprint.self.<path>). Key difference from
// GoldenPath: the Soulprint map is built from a REAL proto SoulprintFacts via
// the same path as the keeper handler (protojson UseProtoNames=true), not by
// hand — that exact path used to produce camelCase (pkgMgr/initSystem/
// primaryIp) and break the template `{{ .self.os.pkg_mgr }}` with
// `map has no entry for key "pkg_mgr"`.
func TestRenderToSoulExecute_CompositeSelfKeys_SnakeCase(t *testing.T) {
	const tmplPath = "templates/self-composite.conf.tmpl"
	const tmplBody = "pkg_mgr {{ .self.os.pkg_mgr }}\n" +
		"init_system {{ .self.os.init_system }}\n" +
		"primary_ip {{ .self.network.primary_ip }}\n" +
		"family {{ .self.os.family }}\n"

	manifest := &config.ScenarioManifest{
		Name: "self-composite",
		Tasks: []config.Task{
			{
				Name: "render conf",
				Module: &config.ModuleTask{
					Module: moduleFileRendered,
					Params: map[string]any{
						"path":     "/etc/app/app.conf",
						"template": tmplPath,
					},
				},
			},
		},
	}

	// Build Soulprint from proto SoulprintFacts exactly like the keeper handler:
	// protojson with UseProtoNames=true → snake_case JSONB → unmarshal into map.
	facts := &keeperv1.SoulprintFacts{
		Sid:      "app-1.example.com",
		Hostname: "app-1",
		Os:       &keeperv1.OsFacts{Family: "debian", Distro: "ubuntu", PkgMgr: "apt", InitSystem: "systemd"},
		Network:  &keeperv1.NetworkFacts{PrimaryIp: "10.0.0.7"},
	}
	factsJSON, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(facts)
	if err != nil {
		t.Fatalf("marshal facts: %v", err)
	}
	var soulprint map[string]any
	if err := json.Unmarshal(factsJSON, &soulprint); err != nil {
		t.Fatalf("unmarshal facts: %v", err)
	}

	host := &topology.HostFacts{
		SID:       "app-1.example.com",
		Coven:     []string{"app-prod"},
		Soulprint: soulprint,
	}

	reader := fakeReader{files: map[string][]byte{tmplPath: []byte(tmplBody)}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "app-prod"},
		Hosts:       []*topology.HostFacts{host},
		Templates:   reader,
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("keeper Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	fields := tasks[0].Params.GetFields()
	tc := fields[paramTemplateContent].GetStringValue()
	renderContext := fields[paramRenderContext].GetStructValue().AsMap()

	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	out, err := engine.Render(tc, renderContext)
	if err != nil {
		t.Fatalf("soul-render упал на composite snake-ключе (BUG-A): %v", err)
	}

	for _, want := range []string{
		"pkg_mgr apt",
		"init_system systemd",
		"primary_ip 10.0.0.7",
		"family debian",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ожидалось %q в рендере (snake-канон .self.*):\n%s", want, out)
		}
	}
}
