package render

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// newEngine — shared cel.Engine for unit tests.
func newEngine(t *testing.T) *cel.Engine {
	t.Helper()
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return e
}

func host(sid string, coven []string, soulprint map[string]any) *topology.HostFacts {
	return &topology.HostFacts{SID: sid, Coven: coven, Soulprint: soulprint}
}

// TestRender_NoopScenario — required spec test: renders
// examples/service/noop/scenario/create/main.yml into a single RenderedTask with
// core.exec.run and a rendered command.
func TestRender_NoopScenario(t *testing.T) {
	path := filepath.FromSlash("../../../examples/service/noop/scenario/create/main.yml")
	manifest, _, diags, err := config.LoadScenarioManifest(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifest: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("scenario diagnostics: %s", d.Message)
		}
	}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "noop-prod", Service: "noop", ServiceVersion: "v1.0.0"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"noop-prod"}, nil),
			host("b.example.com", []string{"noop-prod"}, nil),
		},
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	rt := tasks[0]
	if rt.Module != "core.exec.run" {
		t.Errorf("module = %q, want core.exec.run", rt.Module)
	}
	if rt.Index != 0 {
		t.Errorf("index = %d, want 0", rt.Index)
	}
	if cmd := rt.Params.GetFields()["cmd"].GetStringValue(); cmd != "echo" {
		t.Errorf("params.cmd = %q, want %q", cmd, "echo")
	}
	gotArgs := rt.Params.GetFields()["args"].GetListValue().GetValues()
	if len(gotArgs) != 1 || gotArgs[0].GetStringValue() != "hello" {
		t.Errorf("params.args = %v, want [hello]", gotArgs)
	}

	// on: omitted → whole incarnation (both hosts), sorted by SID.
	if len(plans) != 1 {
		t.Fatalf("len(plans) = %d, want 1", len(plans))
	}
	if got := plans[0].TargetSIDs; len(got) != 2 || got[0] != "a.example.com" || got[1] != "b.example.com" {
		t.Errorf("TargetSIDs = %v, want [a b]", got)
	}
}

// TestRender_InterpolatesInput — `${ input.x }` in params renders from input.
func TestRender_InterpolatesInput(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "echo",
		Tasks: []config.Task{
			{
				Name:   "echo user",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.user }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"user": "alice"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo alice" {
		t.Errorf("command = %q, want %q", got, "echo alice")
	}
}

// TestRender_PropagatesTimeout — config.Task.Timeout propagates to
// render.RenderedTask.Timeout (MAJOR #2: the field was silently dropped at the render layer before
// RenderedTask.Timeout existed; this test guards against that propagation regression).
func TestRender_PropagatesTimeout(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "slow",
		Tasks: []config.Task{
			{
				Name:    "slow step",
				Timeout: "30s",
				Module:  &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "sleep 1"}},
			},
			{
				Name:   "no timeout",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo hi"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if tasks[0].Timeout != "30s" {
		t.Errorf("tasks[0].Timeout = %q, want 30s", tasks[0].Timeout)
	}
	if tasks[1].Timeout != "" {
		t.Errorf("tasks[1].Timeout = %q, want \"\" (timeout not set)", tasks[1].Timeout)
	}
}

// TestRender_WhereFiltersHosts — where: keeps only matching hosts.
// soulprint.self.os.family differentiates hosts; where: doesn't make params host-dependent.
func TestRender_WhereFiltersHosts(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "patch",
		Tasks: []config.Task{
			{
				Name:   "debian only",
				Where:  "soulprint.self.os.family == 'debian'",
				Module: &config.ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "curl"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("deb.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
			host("rhel.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
		},
	}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := plans[0].TargetSIDs; len(got) != 1 || got[0] != "deb.example.com" {
		t.Errorf("TargetSIDs = %v, want [deb.example.com]", got)
	}
}

// TestRender_OnCovenFilter — on: [coven] narrows the roster by coven label.
func TestRender_OnCovenFilter(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "restart",
		Tasks: []config.Task{
			{
				Name:   "restart cache",
				On:     []any{"cache"},
				Module: &config.ModuleTask{Module: "core.service.running", Params: map[string]any{"name": "redis"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a", []string{"svc", "cache"}, nil),
			host("b", []string{"svc", "db"}, nil),
		},
	}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := plans[0].TargetSIDs; len(got) != 1 || got[0] != "a" {
		t.Errorf("TargetSIDs = %v, want [a]", got)
	}
}

// TestRender_OnCovenFilter_MultiLabelAND — `on: [a, b]` = AND intersection
// (ADR-040 amendment 2026-05-27; orchestration.md §3): a host matches only if it
// carries ALL listed labels. Security-invariant regression guard: the old OR code
// would have returned host{prod} AND host{eu}, now only host{prod, eu}.
func TestRender_OnCovenFilter_MultiLabelAND(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "eu-prod-restart",
		Tasks: []config.Task{
			{
				Name:   "restart eu prod only",
				On:     []any{"prod", "eu"},
				Module: &config.ModuleTask{Module: "core.service.running", Params: map[string]any{"name": "redis"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("only-prod", []string{"svc", "prod"}, nil),
			host("only-eu", []string{"svc", "eu"}, nil),
			host("prod-eu", []string{"svc", "prod", "eu"}, nil),
			host("prod-us", []string{"svc", "prod", "us"}, nil),
			host("cache", []string{"svc", "cache"}, nil),
		},
	}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := plans[0].TargetSIDs
	if len(got) != 1 || got[0] != "prod-eu" {
		t.Errorf("TargetSIDs = %v, want [prod-eu] (AND intersection prod n eu)", got)
	}
}

// TestRender_OnCovenFilter_MultiLabelAND_NoMatch — two hosts, each carrying only
// one of the filter labels. AND fail-closed: target is empty (OR used to give both).
func TestRender_OnCovenFilter_MultiLabelAND_NoMatch(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "db-cache-merge",
		Tasks: []config.Task{
			{
				Name:   "needs both labels",
				On:     []any{"db", "cache"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("db", []string{"svc", "db"}, nil),
			host("cache", []string{"svc", "cache"}, nil),
		},
	}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(plans[0].TargetSIDs) != 0 {
		t.Errorf("TargetSIDs = %v, want [] (no host carries both labels)", plans[0].TargetSIDs)
	}
}

// TestRender_OnIncarnationName — on: [${ incarnation.name }] = whole incarnation.
func TestRender_OnIncarnationName(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "all",
		Tasks: []config.Task{
			{
				Name:   "ping all",
				On:     []any{"${ incarnation.name }"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a", []string{"svc"}, nil),
			host("b", []string{"svc"}, nil),
		},
	}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := plans[0].TargetSIDs; len(got) != 2 {
		t.Errorf("TargetSIDs = %v, want both hosts", got)
	}
}

// TestRender_HostVariantParams_Error — host-dependent params in pilot → error.
// pipelineStubKV — hermetic KVReader for pipeline tests of CEL vault().
// Implements both render.KVReader (vault-resolve) and cel.KVReader (CEL vault()).
type pipelineStubKV struct {
	secrets map[string]map[string]any
}

func (s *pipelineStubKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	data, ok := s.secrets[path]
	if !ok {
		return nil, errors.New("vault: KV path not found: vault:" + path)
	}
	return data, nil
}

// vaultEngine builds a cel.Engine with the vault() function registered over kv.
func vaultEngine(t *testing.T, kv cel.KVReader) *cel.Engine {
	t.Helper()
	e, err := cel.New(cel.WithVault(kv))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	return e
}

// TestRender_CELVaultResolvesRealValue — CEL vault() resolves keeper-side to the
// REAL secret value in RenderedTask.Params (not left as a ref string).
// Closes a QA gap: vault() was covered at the cel.Engine level but not through
// render.Pipeline (the full keeper-side pipeline).
func TestRender_CELVaultResolvesRealValue(t *testing.T) {
	kv := &pipelineStubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "real-s3cr3t", "user": "admin"},
	}}
	manifest := &config.ScenarioManifest{
		Name: "vault-cel",
		Tasks: []config.Task{
			{
				Name: "render redis.conf",
				// core.exec.run, not core.file.rendered: this test is about keeper-side vault()
				// resolution in params, without template handoff (rendered injection needs
				// template/template_content — see template_test.go). Secret is passed
				// via env: — a legit core.exec.run input (OptStringMapParam),
				// vault() resolves keeper-side inside a nested map value.
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{
						"cmd": "redis-cli",
						"env": map[string]any{
							"REDIS_PASSWORD": "${ vault('secret/redis/admin#password') }",
						},
					},
				},
			},
		},
	}
	p := NewPipeline(kv, vaultEngine(t, kv), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	env := tasks[0].Params.GetFields()["env"].GetStructValue()
	got := env.GetFields()["REDIS_PASSWORD"].GetStringValue()
	if got != "real-s3cr3t" {
		t.Fatalf("params.env.REDIS_PASSWORD = %q, want the real secret value real-s3cr3t (CEL vault() resolved keeper-side)", got)
	}
}

// TestRender_CELVaultMissingSecret_ActionablePath — a missing secret in CEL vault()
// through the pipeline (NIM-73) gives an actionable error: the path in FLAT form
// (secret/…#password), which survives production status_details/
// error_summary masking. The operator sees WHAT to seed, not `***MASKED***`. The masking layer
// is untouched: a resolved secret VALUE is still masked (see
// TestRender_CELVaultResolvesRealValue above).
func TestRender_CELVaultMissingSecret_ActionablePath(t *testing.T) {
	kv := &pipelineStubKV{secrets: map[string]map[string]any{}}
	manifest := &config.ScenarioManifest{
		Name: "vault-miss",
		Tasks: []config.Task{
			{
				Name: "missing",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{
						"cmd": "redis-cli",
						"env": map[string]any{"REDIS_PASSWORD": "${ vault('secret/redis/admin#password') }"},
					},
				},
			},
		},
	}
	p := NewPipeline(kv, vaultEngine(t, kv), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected an error for a missing secret")
	}
	if !strings.Contains(err.Error(), "secret/redis/admin") {
		t.Fatalf("error text does not carry the secret path (actionable): %q", err.Error())
	}
	if strings.Contains(err.Error(), "vault:secret/redis/admin") {
		t.Fatalf("error text carries the vault:-ref form (masking would eat it whole): %q", err.Error())
	}
	// Same masking as status_details/error_summary in lockIncarnation, with a
	// seal set carrying the run's real sealed cell (env.REDIS_PASSWORD).
	// The error is under the error key → seal doesn't touch it, the path stays visible.
	masked := audit.MaskSecretsSealed(
		map[string]any{"error": err.Error()},
		audit.SealOpts{Sealed: map[string]bool{"env.REDIS_PASSWORD": true}},
	)
	got, _ := masked["error"].(string)
	if got == "***MASKED***" {
		t.Fatalf("actionable error was masked entirely: %q", got)
	}
	if !strings.Contains(got, "secret/redis/admin") {
		t.Fatalf("secret path disappeared after masking: %q", got)
	}
}

func TestRender_HostVariantParams_Error(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "hostvar",
		Tasks: []config.Task{
			{
				Name:   "echo hostname",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ soulprint.self.hostname }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a", []string{"svc"}, map[string]any{"hostname": "a"}),
			host("b", []string{"svc"}, map[string]any{"hostname": "b"}),
		},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected an error for host-variant params, got nil")
	}
}

// TestRender_FlowControlSoulprintMultiHost_Error — a host-variant
// flow-control predicate (soulprint.self) on a multi-host target → fail-closed
// render error (per-host dispatch is deferred). Covers all three fields
// when/changed_when/failed_when to pin down a single shared guard.
func TestRender_FlowControlSoulprintMultiHost_Error(t *testing.T) {
	multiHost := []*topology.HostFacts{
		host("a.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
		host("b.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
	}
	cases := map[string]config.Task{
		"when": {
			Name:   "t",
			When:   "soulprint.self.os.family == 'debian'",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
		},
		"changed_when": {
			Name:        "t",
			ChangedWhen: "soulprint.self.os.family == 'debian'",
			Module:      &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
		},
		"failed_when": {
			Name:       "t",
			FailedWhen: "soulprint.self.os.family == 'debian'",
			Module:     &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
		},
	}
	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			p := NewPipeline(nil, newEngine(t), nil, nil)
			in := RenderInput{
				Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
				Incarnation: IncarnationMeta{Name: "svc"},
				Hosts:       multiHost,
			}
			_, _, err := p.Render(context.Background(), in)
			if err == nil {
				t.Fatalf("Render: expected an error for host-variant %s on multi-host", name)
			}
			if !strings.Contains(err.Error(), "per-host dispatch deferred") {
				t.Errorf("error text is not about the pilot horizon: %q", err.Error())
			}
		})
	}
}

// TestRender_FlowControlSoulprintSingleHost_OK — soulprint.self in when: on a
// single-host target is allowed (flow_context.self is correct for the single
// host) — guard doesn't trigger.
func TestRender_FlowControlSoulprintSingleHost_OK(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "single",
		Tasks: []config.Task{
			{
				Name:   "t",
				When:   "soulprint.self.os.family == 'debian'",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}})},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: single-host soulprint.self in when: should pass, got %v", err)
	}
	if tasks[0].When != "soulprint.self.os.family == 'debian'" {
		t.Errorf("When = %q, want the predicate passed through as-is", tasks[0].When)
	}
}

// TestRender_FlowControlHostInvariantMultiHost_OK — a host-invariant predicate
// (register.*, no soulprint) on a multi-host target passes: one RenderedTask per
// group is correct.
func TestRender_FlowControlHostInvariantMultiHost_OK(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "invariant",
		Tasks: []config.Task{
			{
				Name:   "t",
				When:   "register.probe.changed",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, nil),
			host("b.example.com", []string{"svc"}, nil),
		},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: host-invariant when: on multi-host should pass, got %v", err)
	}
	if tasks[0].When != "register.probe.changed" {
		t.Errorf("When = %q, want the predicate passed through as-is", tasks[0].When)
	}
}

// TestRender_FlowContextVarsLaundering_Error — closes a hole (review, major):
// host-variant vars (`${ soulprint.self.os.family == 'debian' }`) + `when:
// vars.is_debian` on multi-host. The predicate text does NOT contain soulprint → the first
// regex guard lets it through; the SECOND guard catches it — the flow_context-minus-self check.
// Message is specifically about vars laundering, not a direct soulprint in the predicate.
func TestRender_FlowContextVarsLaundering_Error(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "laundering",
		Tasks: []config.Task{
			{
				Name:   "t",
				Vars:   map[string]any{"is_debian": "${ soulprint.self.os.family == 'debian' }"},
				When:   "vars.is_debian",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
			host("b.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
		},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected a fail-closed vars-laundering error, got nil")
	}
	if !strings.Contains(err.Error(), "host-variant flow_context") {
		t.Errorf("error text is not about vars-laundering flow_context: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "per-host dispatch deferred") {
		t.Errorf("error text is not about the pilot horizon: %q", err.Error())
	}
}

// TestRender_FlowContextHostInvariantVars_OK — host-INVARIANT vars
// (`${ input.x }`) + `when: vars.x` on multi-host passes: flow_context is identical
// across all hosts, the second guard doesn't trigger (legit case unbroken).
func TestRender_FlowContextHostInvariantVars_OK(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "invariant-vars",
		Tasks: []config.Task{
			{
				Name:   "t",
				Vars:   map[string]any{"x": "${ input.x }"},
				When:   "vars.x",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"x": true},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
			host("b.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
		},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: host-invariant vars + when: on multi-host should pass, got %v", err)
	}
	if tasks[0].When != "vars.x" {
		t.Errorf("When = %q, want the predicate passed through as-is", tasks[0].When)
	}
}

// TestRender_RenderedWithFlowControlHostInvariantVars_OK — QA gap: a
// core.file.rendered task with when: + host-INVARIANT vars on multi-host passes.
// Special params handling for rendered (render_context.self is host-variant and goes
// INTO PARAMS) must NOT leak into flow_context: flow_context.vars comes from the original
// task vars (host-invariant here), while render_context lives separately in params and
// is excluded from both checks (paramsHostInvariant + flowContextHostInvariant). Verifies
// rendered doesn't break the second fail-closed guard on a legit case.
func TestRender_RenderedWithFlowControlHostInvariantVars_OK(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "rendered-invariant-vars",
		Tasks: []config.Task{
			{
				Name: "cfg",
				Vars: map[string]any{"enabled": "${ input.enabled }"}, // host-invariant
				When: "vars.enabled",
				Module: &config.ModuleTask{
					Module: "core.file.rendered",
					Params: map[string]any{
						"path":     "/etc/app.conf",
						"template": "templates/app.conf.tmpl",
					},
				},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"enabled": true},
		Incarnation: IncarnationMeta{Name: "svc"},
		Templates: fakeReader{files: map[string][]byte{
			"templates/app.conf.tmpl": []byte("host {{ .self.hostname }}\n"),
		}},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
			host("b.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
		},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: rendered + host-invariant vars + when on multi-host should pass, got %v", err)
	}
	// flow_context.vars must not include render_context — otherwise the check would fail
	// on host-variant self. Since Render succeeded, the guard didn't false-trigger.
	if tasks[0].When != "vars.enabled" {
		t.Errorf("When = %q, want the predicate passed through as-is", tasks[0].When)
	}
	if _, ok := tasks[0].FlowContext.GetFields()["render_context"]; ok {
		t.Error("flow_context should not contain render_context (it lives in params)")
	}
}

// TestRender_HostVariantVarsNoFlowControl_FailsOnParams — a task WITHOUT a
// flow-control predicate, but with host-variant vars leaking into params
// (`args: [${ vars.x }]`, vars.x from soulprint.self). The hasFlowControl gate is false
// → the new flow_context check is NOT active; the error comes from paramsHostInvariant
// (host-dependent params), not the second guard. Verifies the gate didn't
// intercept an unrelated error — the message is about params, not flow_context.
func TestRender_HostVariantVarsNoFlowControl_FailsOnParams(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "novars-flowcontrol",
		Tasks: []config.Task{
			{
				Name:   "t",
				Vars:   map[string]any{"x": "${ soulprint.self.os.family }"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo", "args": []any{"${ vars.x }"}}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
			host("b.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
		},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected an error for host-dependent params, got nil")
	}
	if !strings.Contains(err.Error(), "host-dependent params") {
		t.Errorf("error is not from paramsHostInvariant (did the gate catch someone else's?): %q", err.Error())
	}
	if strings.Contains(err.Error(), "flow_context") {
		t.Errorf("the second circuit fired incorrectly without a flow-control predicate: %q", err.Error())
	}
}

// TestRender_FlowContextVarsLaunderingSingleHost_OK — single-host: host-variant
// vars + when: vars.x passes (flow_context.self is correct for the single
// host, the single-host golden path is unbroken, no check runs — len(renderHosts)==1).
func TestRender_FlowContextVarsLaunderingSingleHost_OK(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "single-laundering",
		Tasks: []config.Task{
			{
				Name:   "t",
				Vars:   map[string]any{"is_debian": "${ soulprint.self.os.family == 'debian' }"},
				When:   "vars.is_debian",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}})},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: single-host host-variant vars + when: should pass, got %v", err)
	}
	if tasks[0].When != "vars.is_debian" {
		t.Errorf("When = %q, want the predicate passed through as-is", tasks[0].When)
	}
}

// TestRender_FlowContextVarsLaunderingChangedWhen_Error — covers all three
// flow-control keys: host-variant vars leaks ONLY into changed_when
// (not when). The second guard must trigger (hasFlowControl gate checks all
// three fields).
func TestRender_FlowContextVarsLaunderingChangedWhen_Error(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "laundering-changedwhen",
		Tasks: []config.Task{
			{
				Name:        "t",
				Vars:        map[string]any{"is_debian": "${ soulprint.self.os.family == 'debian' }"},
				ChangedWhen: "vars.is_debian",
				Module:      &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
			host("b.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
		},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected a fail-closed vars-laundering error in changed_when, got nil")
	}
	if !strings.Contains(err.Error(), "host-variant flow_context") {
		t.Errorf("error text is not about vars-laundering flow_context: %q", err.Error())
	}
}

// TestRender_UnsupportedDSL — pilot guard rejects parallel.
// block is no longer included here (implemented, pilot C1 — render-time fan-out, see
// block_test.go); serial/run_once are also implemented (slice D); loop on a
// module task is implemented (slice E1) — positive tests in loop_test.go, and loop
// on apply is rejected there too (TestRenderLoop_OnApplyRejected).
func TestRender_UnsupportedDSL(t *testing.T) {
	cases := map[string]config.Task{
		// apply: with nil DestinyResolver (Destiny not configured) → ErrUnsupportedDSL.
		"apply":    {Name: "t", Apply: &config.ApplyTask{Destiny: "redis"}},
		"parallel": {Name: "t", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}, Parallel: true},
	}
	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			p := NewPipeline(nil, newEngine(t), nil, nil)
			in := RenderInput{
				Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
				Incarnation: IncarnationMeta{Name: "svc"},
				Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
			}
			_, _, err := p.Render(context.Background(), in)
			if !errors.Is(err, ErrUnsupportedDSL) {
				t.Fatalf("err = %v, want ErrUnsupportedDSL", err)
			}
		})
	}
}

// TestRender_UnexpandedInclude — include must be expanded BEFORE render
// (config.ExpandIncludes); reaching render unexpanded → ErrUnexpandedInclude
// (an expansion bug, not "outside pilot scope").
func TestRender_UnexpandedInclude(t *testing.T) {
	task := config.Task{Name: "t", Include: &config.IncludeTask{Include: "x.yml"}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{task}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnexpandedInclude) {
		t.Fatalf("err = %v, want ErrUnexpandedInclude", err)
	}
}

// TestRender_OnKeeper_KeeperTarget — on: keeper renders in keeper context
// (no per-host soulprint) and yields a single keeper target with Keeper=true. params
// read input/incarnation; soulprint is unavailable in a keeper task.
func TestRender_OnKeeper_KeeperTarget(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "k",
		Tasks: []config.Task{
			{Name: "t", On: "keeper", Register: "registered", Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{
				"sid":   "${ input.sid }",
				"coven": []any{"${ incarnation.name }"},
			}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Input:       map[string]any{"sid": "node-1.example.com"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 || len(plans) != 1 {
		t.Fatalf("len(tasks)=%d len(plans)=%d, want 1/1", len(tasks), len(plans))
	}
	if !plans[0].Keeper {
		t.Fatalf("plans[0].Keeper = false, want true")
	}
	if len(plans[0].TargetSIDs) != 1 || plans[0].TargetSIDs[0] != KeeperTargetSID {
		t.Fatalf("plans[0].TargetSIDs = %v, want [%q]", plans[0].TargetSIDs, KeeperTargetSID)
	}
	if tasks[0].Module != "core.soul.registered" || tasks[0].Register != "registered" {
		t.Fatalf("task module/register = %q/%q", tasks[0].Module, tasks[0].Register)
	}
	sid := tasks[0].Params.GetFields()["sid"].GetStringValue()
	if sid != "node-1.example.com" {
		t.Fatalf("params.sid = %q, want node-1.example.com (keeper-context input)", sid)
	}
}

// TestRender_OnKeeper_SoulprintUnavailable — soulprint.self in a keeper task's
// params is unavailable (no hosts): CEL raises a no-such-key error, doesn't stay silent.
func TestRender_OnKeeper_SoulprintUnavailable(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "k",
		Tasks: []config.Task{
			{Name: "t", On: "keeper", Module: &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{
				"sid": "${ soulprint.self.sid }",
			}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	if _, _, err := p.Render(context.Background(), in); err == nil {
		t.Fatalf("Render: err = nil, want a CEL error (soulprint unavailable in a keeper task)")
	}
}

// TestRender_OnKeeper_StateReadable — a keeper-side task reads incarnation.state.<path>
// in params: pre-run snapshot (RenderInput.State), symmetric with Soul-side. Unblocks
// core.cloud.destroyed (on: keeper) reading incarnation.state.provisioned_*.
func TestRender_OnKeeper_StateReadable(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "k",
		Tasks: []config.Task{
			{Name: "t", On: "keeper", Module: &config.ModuleTask{Module: "core.cloud.provisioned", Params: map[string]any{
				"vm_id": "${ incarnation.state.provisioned_vm_id }",
			}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		State:       map[string]any{"provisioned_vm_id": "vm-42"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	got := tasks[0].Params.GetFields()["vm_id"].GetStringValue()
	if got != "vm-42" {
		t.Fatalf("params.vm_id = %q, want vm-42 (a keeper task sees incarnation.state)", got)
	}
}

// TestRender_OnKeeper_StateNilNoSuchKey — without RenderInput.State, accessing
// incarnation.state.<path> in a keeper task's params is a regular no-such-key (push/trial
// back-compat), not a silent empty result. Symmetric with Soul-side nil State.
func TestRender_OnKeeper_StateNilNoSuchKey(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "k",
		Tasks: []config.Task{
			{Name: "t", On: "keeper", Module: &config.ModuleTask{Module: "core.cloud.provisioned", Params: map[string]any{
				"vm_id": "${ incarnation.state.provisioned_vm_id }",
			}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"}, // State == nil
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	if _, _, err := p.Render(context.Background(), in); err == nil {
		t.Fatalf("Render: err = nil, want no-such-key (incarnation.state without State)")
	}
}

// TestRender_WhereExcludesAll — where: filtered out everyone → empty DispatchPlan,
// but RenderedTask is still present.
func TestRender_WhereExcludesAll(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "none",
		Tasks: []config.Task{
			{Name: "never", Where: "false", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if len(plans[0].TargetSIDs) != 0 {
		t.Errorf("TargetSIDs = %v, want empty", plans[0].TargetSIDs)
	}
}

// TestRender_WhereNonBool_Error — where: with a non-bool result → error.
func TestRender_WhereNonBool_Error(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "bad",
		Tasks: []config.Task{
			{Name: "t", Where: "input.x", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"x": "notbool"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected a non-bool where error")
	}
}

// TestRenderStateChanges_Literal — a literal in sets is assigned as-is.
func TestRenderStateChanges_Literal(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		StateChanges: &config.StateChanges{
			Sets: map[string]string{"greeting_file": "/tmp/soul-stack-hello"},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "hello-world"},
		Hosts:       []*topology.HostFacts{host("a", []string{"hello-world"}, nil)},
	}
	got, err := p.RenderStateChanges(in)
	if err != nil {
		t.Fatalf("RenderStateChanges: %v", err)
	}
	if got["greeting_file"] != "/tmp/soul-stack-hello" {
		t.Errorf("greeting_file = %v, want literal", got["greeting_file"])
	}
}

// TestRenderStateChanges_FromInput — sets takes a value from input.* via CEL.
func TestRenderStateChanges_FromInput(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		StateChanges: &config.StateChanges{
			Sets: map[string]string{"version": "${ input.version }"},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"version": "7.2.0"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	got, err := p.RenderStateChanges(in)
	if err != nil {
		t.Fatalf("RenderStateChanges: %v", err)
	}
	if got["version"] != "7.2.0" {
		t.Errorf("version = %v, want 7.2.0", got["version"])
	}
}

// TestRenderStateChanges_LastWins — a per-host value (soulprint.self) collapses
// last-wins by SID sort order: the host with the lexicographically last SID wins.
func TestRenderStateChanges_LastWins(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		StateChanges: &config.StateChanges{
			Sets: map[string]string{"leader": "${ soulprint.self.hostname }"},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		// Passed in unsorted order — the fold must sort it itself.
		Hosts: []*topology.HostFacts{
			host("b.example.com", []string{"svc"}, map[string]any{"hostname": "beta"}),
			host("a.example.com", []string{"svc"}, map[string]any{"hostname": "alpha"}),
			host("c.example.com", []string{"svc"}, map[string]any{"hostname": "gamma"}),
		},
	}
	got, err := p.RenderStateChanges(in)
	if err != nil {
		t.Fatalf("RenderStateChanges: %v", err)
	}
	// c.example.com — last by SID → gamma.
	if got["leader"] != "gamma" {
		t.Errorf("leader = %v, want gamma (last SID wins)", got["leader"])
	}
}

// TestRenderStateChanges_Empty — nil StateChanges and empty sets → empty map.
func TestRenderStateChanges_Empty(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	base := RenderInput{
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}

	for name, sc := range map[string]*config.StateChanges{
		"nil_block": nil,
		"nil_sets":  {Sets: nil},
		"empty_set": {Sets: map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			in.Scenario = &config.ScenarioManifest{Name: "s", StateChanges: sc}
			got, err := p.RenderStateChanges(in)
			if err != nil {
				t.Fatalf("RenderStateChanges: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("got = %v, want empty", got)
			}
		})
	}
}

// TestRenderStateChanges_RegisterFromHost — register.* in sets is read from
// per-host RegisterByHost (slice 2), NOT from the global RenderInput.Register
// (that's cross-task chaining in the Render phase, invisible to sets). Positive path:
// a probe task produced register.probe.stdout → it lands in sets.
func TestRenderStateChanges_RegisterFromHost(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		StateChanges: &config.StateChanges{
			Sets: map[string]string{"x": "${ register.probe.stdout }"},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		RegisterByHost: map[string]map[string]any{
			"a": {"probe": map[string]any{"stdout": "hello"}},
		},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	got, err := p.RenderStateChanges(in)
	if err != nil {
		t.Fatalf("RenderStateChanges: %v", err)
	}
	if got["x"] != "hello" {
		t.Errorf("sets.x = %v, want \"hello\" (from register.probe.stdout)", got["x"])
	}
}

// TestRenderStateChanges_GlobalRegisterNotLeaked — the global
// RenderInput.Register (the Render phase) is NOT visible in sets: sets only reads
// RegisterByHost[sid]. With empty RegisterByHost, accessing register.* gives a
// deterministic eval error ("no such key"), regardless of a populated
// global Register.
func TestRenderStateChanges_GlobalRegisterNotLeaked(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		StateChanges: &config.StateChanges{
			Sets: map[string]string{"x": "${ register.probe.stdout }"},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Register:    map[string]any{"probe": map[string]any{"stdout": "leaked"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	got, err := p.RenderStateChanges(in)
	if err == nil {
		t.Fatalf("RenderStateChanges: expected an eval error (got=%v): the global Register must not leak into sets", got)
	}
	if got != nil {
		t.Errorf("expected a nil result on a render error, got = %v", got)
	}
}

// TestRenderStateChanges_RegisterLastWinsCrossHost — last-wins fold for sets with
// register: when register values differ across hosts, the value from the
// host with the lexicographically last SID lands in state.
func TestRenderStateChanges_RegisterLastWinsCrossHost(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		StateChanges: &config.StateChanges{
			Sets: map[string]string{"leader": "${ register.probe.stdout }"},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		RegisterByHost: map[string]map[string]any{
			"a": {"probe": map[string]any{"stdout": "from-a"}},
			"b": {"probe": map[string]any{"stdout": "from-b"}},
		},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a", []string{"svc"}, nil),
			host("b", []string{"svc"}, nil),
		},
	}
	got, err := p.RenderStateChanges(in)
	if err != nil {
		t.Fatalf("RenderStateChanges: %v", err)
	}
	if got["leader"] != "from-b" {
		t.Errorf("sets.leader = %v, want \"from-b\" (last-wins by SID)", got["leader"])
	}
}

// TestRender_HostCountInCEL — incarnation.host_count = number of targeted hosts.
func TestRender_HostCountInCEL(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "count",
		Tasks: []config.Task{
			{Name: "t", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"n": "${ incarnation.host_count }"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a", []string{"svc"}, nil),
			host("b", []string{"svc"}, nil),
			host("c", []string{"svc"}, nil),
		},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := tasks[0].Params.GetFields()["n"].GetNumberValue(); got != 3 {
		t.Errorf("host_count = %v, want 3", got)
	}
}

// TestRender_RunOnce_PicksFirstBySID — run_once: true → the task goes to exactly
// one host, deterministically the first by SID, when N>1 hosts are targeted
// (orchestration.md §2.2.2).
func TestRender_RunOnce_PicksFirstBySID(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "once",
		Tasks: []config.Task{
			{Name: "t", RunOnce: true, Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		// Unsorted roster — resolution must pick the first by SID itself.
		Hosts: []*topology.HostFacts{
			host("c.example.com", []string{"svc"}, nil),
			host("a.example.com", []string{"svc"}, nil),
			host("b.example.com", []string{"svc"}, nil),
		},
	}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := plans[0].TargetSIDs; len(got) != 1 || got[0] != "a.example.com" {
		t.Errorf("TargetSIDs = %v, want [a.example.com] (run_once -> first by SID)", got)
	}
	if plans[0].SerialWidth != 0 {
		t.Errorf("SerialWidth = %d, want 0 (run_once without serial)", plans[0].SerialWidth)
	}
}

// TestRender_RunOnce_ZeroHosts — run_once: with an empty target (where: filtered
// everyone out) doesn't panic and gives an empty DispatchPlan (general §5 semantics,
// with no policy of its own for an empty target).
func TestRender_RunOnce_ZeroHosts(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "once-empty",
		Tasks: []config.Task{
			{Name: "t", RunOnce: true, Where: "false", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil), host("b", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if len(plans[0].TargetSIDs) != 0 {
		t.Errorf("TargetSIDs = %v, want empty", plans[0].TargetSIDs)
	}
}

// TestRender_Serial_WidthInPlan — serial: N and "<N>%" compute the plan's SerialWidth
// against the target count (orchestration.md §2.2.1). Wave slicing itself is
// scenario-dispatch (see scenario/dispatch_test.go); here we only check that
// the computed width is correct.
func TestRender_Serial_WidthInPlan(t *testing.T) {
	hosts := []*topology.HostFacts{
		host("a", []string{"svc"}, nil),
		host("b", []string{"svc"}, nil),
		host("c", []string{"svc"}, nil),
		host("d", []string{"svc"}, nil),
	}
	tests := []struct {
		name      string
		serial    any
		wantWidth int
	}{
		{"int 1", 1, 1},
		{"int 2", 2, 2},
		{"int wider than target", 10, 10},
		{"25% of 4 → 1", "25%", 1},
		{"50% of 4 → 2", "50%", 2},
		{"30% of 4 → ceil 2", "30%", 2},
		{"1% of 4 → min 1", "1%", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := &config.ScenarioManifest{
				Name: "s",
				Tasks: []config.Task{
					{Name: "t", Serial: tt.serial, Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}}},
				},
			}
			p := NewPipeline(nil, newEngine(t), nil, nil)
			in := RenderInput{Scenario: manifest, Incarnation: IncarnationMeta{Name: "svc"}, Hosts: hosts}
			_, plans, err := p.Render(context.Background(), in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if plans[0].SerialWidth != tt.wantWidth {
				t.Errorf("SerialWidth = %d, want %d", plans[0].SerialWidth, tt.wantWidth)
			}
			if got := len(plans[0].TargetSIDs); got != 4 {
				t.Errorf("TargetSIDs len = %d, want 4 (serial does not cut the target, only the wave width)", got)
			}
		})
	}
}

// TestSerialWidth — pure wave-width computation function: int/percent/nil forms,
// round-up percent, minimum 1.
func TestSerialWidth(t *testing.T) {
	tests := []struct {
		name   string
		serial any
		n      int
		want   int
	}{
		{"nil → 0", nil, 5, 0},
		{"int 3", 3, 5, 3},
		{"int64 2", int64(2), 5, 2},
		{"uint64 4", uint64(4), 5, 4},
		{"100% of 7 → 7", "100%", 7, 7},
		{"50% of 7 → ceil 4", "50%", 7, 4},
		{"33% of 3 → ceil 1", "33%", 3, 1},
		{"34% of 3 → ceil 2", "34%", 3, 2},
		{"1% of 10 → min 1", "1%", 10, 1},
		{"10% of 1 → min 1", "10%", 1, 1},
		{"bad string → 0", "abc", 5, 0},
		{"unknown type → 0", 1.5, 5, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serialWidth(tt.serial, tt.n); got != tt.want {
				t.Errorf("serialWidth(%v, %d) = %d, want %d", tt.serial, tt.n, got, tt.want)
			}
		})
	}
}

// TestRender_Serial_PercentAfterWhere — serial: percent is computed from the host
// count AFTER the where filter, not the whole roster (orchestration.md §2.2.1):
// roster of 4 hosts, where: leaves 2 (by a stable soulprint fact), serial:
// "50%" → ceil(2*50/100)=1, not ceil(4*50/100)=2. TargetSIDs is also 2 (where
// narrows the target, serial only affects wave width).
func TestRender_Serial_PercentAfterWhere(t *testing.T) {
	hosts := []*topology.HostFacts{
		host("a", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
		host("b", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
		host("c", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
		host("d", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
	}
	manifest := &config.ScenarioManifest{
		Name: "s",
		Tasks: []config.Task{
			{
				Name:   "t",
				Serial: "50%",
				Where:  "soulprint.self.os.family == 'debian'",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{Scenario: manifest, Incarnation: IncarnationMeta{Name: "svc"}, Hosts: hosts}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := len(plans[0].TargetSIDs); got != 2 {
		t.Fatalf("TargetSIDs len = %d, want 2 (where: left debian hosts a,c)", got)
	}
	if plans[0].SerialWidth != 1 {
		t.Errorf("SerialWidth = %d, want 1 (50%% of 2 where-hosts = ceil 1, NOT 2 of the full roster)", plans[0].SerialWidth)
	}
}

// TestApplyRunOnce — slices the target down to the first host by SID.
func TestApplyRunOnce(t *testing.T) {
	mk := func(sids ...string) []*topology.HostFacts {
		out := make([]*topology.HostFacts, len(sids))
		for i, s := range sids {
			out[i] = host(s, nil, nil)
		}
		return out
	}

	t.Run("run_once false -> no change", func(t *testing.T) {
		in := mk("c", "a", "b")
		got := applyRunOnce(in, false)
		if len(got) != 3 {
			t.Errorf("len = %d, want 3", len(got))
		}
	})
	t.Run("run_once true N>1 -> first by SID", func(t *testing.T) {
		got := applyRunOnce(mk("c", "a", "b"), true)
		if len(got) != 1 || got[0].SID != "a" {
			t.Errorf("got = %v, want [a]", sidsOf(got))
		}
	})
	t.Run("run_once true 1 host -> that same host", func(t *testing.T) {
		got := applyRunOnce(mk("only"), true)
		if len(got) != 1 || got[0].SID != "only" {
			t.Errorf("got = %v, want [only]", sidsOf(got))
		}
	})
	t.Run("run_once true 0 hosts -> empty", func(t *testing.T) {
		got := applyRunOnce(mk(), true)
		if len(got) != 0 {
			t.Errorf("got = %v, want empty", sidsOf(got))
		}
	})
}
