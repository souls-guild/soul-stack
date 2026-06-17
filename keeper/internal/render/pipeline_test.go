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

// newEngine — общий cel.Engine для unit-тестов.
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

// TestRender_NoopScenario — обязательный тест ТЗ: рендер
// examples/service/service-noop/scenario/create/main.yml → один RenderedTask с
// core.exec.run и rendered command.
func TestRender_NoopScenario(t *testing.T) {
	path := filepath.FromSlash("../../../examples/service/service-noop/scenario/create/main.yml")
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

	// on: опущен → весь incarnation (оба хоста), отсортированы по SID.
	if len(plans) != 1 {
		t.Fatalf("len(plans) = %d, want 1", len(plans))
	}
	if got := plans[0].TargetSIDs; len(got) != 2 || got[0] != "a.example.com" || got[1] != "b.example.com" {
		t.Errorf("TargetSIDs = %v, want [a b]", got)
	}
}

// TestRender_InterpolatesInput — `${ input.x }` в params рендерится из input.
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

// TestRender_PropagatesTimeout — config.Task.Timeout доезжает до
// render.RenderedTask.Timeout (MAJOR #2: поле молча терялось на render-слое до
// появления RenderedTask.Timeout; этот тест ловит регресс обрыва протяжки).
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
		t.Errorf("tasks[1].Timeout = %q, want \"\" (timeout не задан)", tasks[1].Timeout)
	}
}

// TestRender_WhereFiltersHosts — where: оставляет только подходящие хосты.
// soulprint.self.os.family различает хосты; where: не делает params host-зависимыми.
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

// TestRender_OnCovenFilter — on: [coven] сужает roster по Coven-метке.
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

// TestRender_OnCovenFilter_MultiLabelAND — `on: [a, b]` = AND-пересечение
// (ADR-040 amendment 2026-05-27; orchestration.md §3): хост попадает только если
// несёт ВСЕ перечисленные метки. Регрессия security-инварианта: раньше OR-код
// возвращал бы host{prod} И host{eu}, теперь — только host{prod, eu}.
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
		t.Errorf("TargetSIDs = %v, want [prod-eu] (AND-пересечение prod ∩ eu)", got)
	}
}

// TestRender_OnCovenFilter_MultiLabelAND_NoMatch — два хоста, у каждого только
// одна из меток фильтра. AND fail-closed: target пуст (раньше OR давал обоих).
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
		t.Errorf("TargetSIDs = %v, want [] (ни один хост не несёт обе метки)", plans[0].TargetSIDs)
	}
}

// TestRender_OnIncarnationName — on: [${ incarnation.name }] = весь incarnation.
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

// TestRender_HostVariantParams_Error — host-зависимые params в pilot → ошибка.
// pipelineStubKV — герметичный KVReader для pipeline-тестов CEL vault().
// Реализует и render.KVReader (vault-resolve), и cel.KVReader (CEL vault()).
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

// vaultEngine собирает cel.Engine с зарегистрированной функцией vault() поверх kv.
func vaultEngine(t *testing.T, kv cel.KVReader) *cel.Engine {
	t.Helper()
	e, err := cel.New(cel.WithVault(kv))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	return e
}

// TestRender_CELVaultResolvesRealValue — CEL vault() резолвится keeper-side в
// РЕАЛЬНОЕ значение секрета в RenderedTask.Params (а не остаётся ref-строкой).
// Закрывает qa-пробел: vault() покрыт на уровне cel.Engine, но не через
// render.Pipeline (полный keeper-side pipeline).
func TestRender_CELVaultResolvesRealValue(t *testing.T) {
	kv := &pipelineStubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "real-s3cr3t", "user": "admin"},
	}}
	manifest := &config.ScenarioManifest{
		Name: "vault-cel",
		Tasks: []config.Task{
			{
				Name: "render redis.conf",
				// core.exec.run, не core.file.rendered: тест про keeper-side vault()-
				// резолв в params, без template-handoff (для rendered инъекция требует
				// template/template_content — см. template_test.go). Секрет передаём
				// через env: — легитимный input core.exec.run (OptStringMapParam),
				// vault() резолвится keeper-side во вложенном значении map.
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
		t.Fatalf("params.env.REDIS_PASSWORD = %q, want реальное значение секрета real-s3cr3t (CEL vault() резолвлен keeper-side)", got)
	}
}

// TestRender_CELVaultMissingSecret_PathMasked — missing-secret в CEL vault()
// через pipeline даёт ошибку, текст которой несёт путь в ref-форме vault:secret/
// → audit.MaskSecrets маскирует её целиком (после фикса leak-а пути в тексте
// ошибки ReadKV). Гарантирует, что путь не утечёт в status_details/error_summary.
func TestRender_CELVaultMissingSecret_PathMasked(t *testing.T) {
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
		t.Fatal("Render: ожидалась ошибка отсутствующего секрета")
	}
	if !strings.Contains(err.Error(), "vault:secret/redis/admin") {
		t.Fatalf("текст ошибки не несёт путь в ref-форме: %q", err.Error())
	}
	masked := audit.MaskSecrets(map[string]any{"error_summary": err.Error()})
	if got, _ := masked["error_summary"].(string); got != "***MASKED***" {
		t.Fatalf("audit.MaskSecrets не замаскировал ошибку с vault-путём: %q", got)
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
		t.Fatal("Render: ожидалась ошибка host-вариативных params, got nil")
	}
}

// TestRender_FlowControlSoulprintMultiHost_Error — host-вариативный
// flow-control-предикат (soulprint.self) на multi-host таргете → fail-closed
// ошибка рендера (per-host dispatch отложен). Покрыты все три поля
// when/changed_when/failed_when, чтобы зафиксировать единый guard.
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
				t.Fatalf("Render: ожидалась ошибка host-вариативного %s на multi-host", name)
			}
			if !strings.Contains(err.Error(), "per-host dispatch отложен") {
				t.Errorf("текст ошибки не про горизонт pilot: %q", err.Error())
			}
		})
	}
}

// TestRender_FlowControlSoulprintSingleHost_OK — soulprint.self в when: на
// single-host таргете допустим (flow_context.self корректен для единственного
// хоста) — guard не срабатывает.
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
		t.Fatalf("Render: single-host soulprint.self в when: должен проходить, got %v", err)
	}
	if tasks[0].When != "soulprint.self.os.family == 'debian'" {
		t.Errorf("When = %q, want протянутый as-is предикат", tasks[0].When)
	}
}

// TestRender_FlowControlHostInvariantMultiHost_OK — host-инвариантный предикат
// (register.*, без soulprint) на multi-host таргете проходит: один RenderedTask на
// группу корректен.
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
		t.Fatalf("Render: host-инвариантный when: на multi-host должен проходить, got %v", err)
	}
	if tasks[0].When != "register.probe.changed" {
		t.Errorf("When = %q, want протянутый as-is предикат", tasks[0].When)
	}
}

// TestRender_FlowContextVarsLaundering_Error — закрытие дыры (review, major):
// host-вариативный vars (`${ soulprint.self.os.family == 'debian' }`) + `when:
// vars.is_debian` на multi-host. Текст предиката НЕ содержит soulprint → первый
// regex-guard пропускает; ловит ВТОРОЙ контур — сверка flow_context-минус-self.
// Сообщение явно про vars-laundering, не про прямой soulprint в предикате.
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
		t.Fatal("Render: ожидалась fail-closed ошибка vars-laundering, got nil")
	}
	if !strings.Contains(err.Error(), "host-вариативный flow_context") {
		t.Errorf("текст ошибки не про vars-laundering flow_context: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "per-host dispatch отложен") {
		t.Errorf("текст ошибки не про горизонт pilot: %q", err.Error())
	}
}

// TestRender_FlowContextHostInvariantVars_OK — host-ИНВАРИАНТНЫЙ vars
// (`${ input.x }`) + `when: vars.x` на multi-host проходит: flow_context одинаков
// на всех хостах, второй контур не срабатывает (легитимный кейс не ломается).
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
		t.Fatalf("Render: host-инвариантный vars + when: на multi-host должен проходить, got %v", err)
	}
	if tasks[0].When != "vars.x" {
		t.Errorf("When = %q, want протянутый as-is предикат", tasks[0].When)
	}
}

// TestRender_RenderedWithFlowControlHostInvariantVars_OK — QA-пробел: задача
// core.file.rendered с when: + host-ИНВАРИАНТНЫМ vars на multi-host проходит.
// Особая обработка params у rendered (render_context.self host-вариативен и кладётся
// В PARAMS) НЕ должна тянуться во flow_context: flow_context.vars берётся из исходных
// task-vars (host-инвариантных здесь), а render_context живёт отдельно в params и
// исключён из обеих сверок (paramsHostInvariant + flowContextHostInvariant). Контроль,
// что rendered не ломает второй контур fail-closed на легитимном кейсе.
func TestRender_RenderedWithFlowControlHostInvariantVars_OK(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "rendered-invariant-vars",
		Tasks: []config.Task{
			{
				Name: "cfg",
				Vars: map[string]any{"enabled": "${ input.enabled }"}, // host-инвариантен
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
		t.Fatalf("Render: rendered + host-инвариантный vars + when на multi-host должен проходить, got %v", err)
	}
	// flow_context.vars не должен включать render_context — иначе сверка упала бы
	// на host-вариативном self. Раз Render прошёл, контур не сработал ложно.
	if tasks[0].When != "vars.enabled" {
		t.Errorf("When = %q, want протянутый as-is предикат", tasks[0].When)
	}
	if _, ok := tasks[0].FlowContext.GetFields()["render_context"]; ok {
		t.Error("flow_context не должен содержать render_context (он живёт в params)")
	}
}

// TestRender_HostVariantVarsNoFlowControl_FailsOnParams — задача БЕЗ
// flow-control-предиката, но с host-вариативным vars, протёкшим в params
// (`args: [${ vars.x }]`, vars.x из soulprint.self). Гейт hasFlowControl ложен
// → новая сверка flow_context НЕ активна; ошибка приходит от paramsHostInvariant
// (host-зависимые params), не от второго контура. Проверяем, что гейт не
// перехватил чужую ошибку — текст про params, а не про flow_context.
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
		t.Fatal("Render: ожидалась ошибка host-зависимых params, got nil")
	}
	if !strings.Contains(err.Error(), "host-зависимые params") {
		t.Errorf("ошибка не от paramsHostInvariant (гейт перехватил чужую?): %q", err.Error())
	}
	if strings.Contains(err.Error(), "flow_context") {
		t.Errorf("второй контур ошибочно сработал без flow-control-предиката: %q", err.Error())
	}
}

// TestRender_FlowContextVarsLaunderingSingleHost_OK — single-host: host-вариативный
// vars + when: vars.x проходит (flow_context.self корректен для единственного
// хоста, golden-path single-host не сломан, сверки нет — len(renderHosts)==1).
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
		t.Fatalf("Render: single-host host-вариативный vars + when: должен проходить, got %v", err)
	}
	if tasks[0].When != "vars.is_debian" {
		t.Errorf("When = %q, want протянутый as-is предикат", tasks[0].When)
	}
}

// TestRender_FlowContextVarsLaunderingChangedWhen_Error — покрытие всех трёх
// flow-control-ключей: host-вариативный vars протёк ТОЛЬКО в changed_when
// (не when). Второй контур обязан сработать (гейт hasFlowControl смотрит на все
// три поля).
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
		t.Fatal("Render: ожидалась fail-closed ошибка vars-laundering в changed_when, got nil")
	}
	if !strings.Contains(err.Error(), "host-вариативный flow_context") {
		t.Errorf("текст ошибки не про vars-laundering flow_context: %q", err.Error())
	}
}

// TestRender_UnsupportedDSL — pilot-guard отвергает block/parallel.
// serial/run_once сюда больше НЕ входят (реализованы, slice D); loop на
// module-задаче тоже реализован (slice E1, render-time fan-out) — для него
// позитивные тесты в loop_test.go, а loop на apply отвергается там же
// (TestRenderLoop_OnApplyRejected).
func TestRender_UnsupportedDSL(t *testing.T) {
	cases := map[string]config.Task{
		// apply: с nil-DestinyResolver (Destiny не сконфигурирован) → ErrUnsupportedDSL.
		"apply":    {Name: "t", Apply: &config.ApplyTask{Destiny: "redis"}},
		"block":    {Name: "t", Block: &config.BlockTask{}},
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

// TestRender_UnexpandedInclude — include должен раскрываться ДО render
// (config.ExpandIncludes); дошедший до render нераскрытым → ErrUnexpandedInclude
// (баг раскрытия, не «вне pilot-объёма»).
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

// TestRender_OnKeeper_KeeperTarget — on: keeper рендерится в keeper-контексте
// (без per-host soulprint) и даёт единичный keeper-target c Keeper=true. params
// читают input/incarnation; soulprint в keeper-задаче недоступен.
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
		t.Fatalf("params.sid = %q, want node-1.example.com (keeper-контекст input)", sid)
	}
}

// TestRender_OnKeeper_SoulprintUnavailable — soulprint.self в params keeper-
// задачи недоступен (хостов нет): CEL даёт ошибку no-such-key, не молчит.
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
		t.Fatalf("Render: err = nil, want CEL-ошибка (soulprint недоступен в keeper-задаче)")
	}
}

// TestRender_WhereExcludesAll — where: отфильтровал всех → пустой DispatchPlan,
// но RenderedTask присутствует.
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

// TestRender_WhereNonBool_Error — where: с не-bool результатом → ошибка.
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
		t.Fatal("Render: ожидалась ошибка non-bool where")
	}
}

// TestRenderStateChanges_Literal — литерал в sets присваивается как есть.
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

// TestRenderStateChanges_FromInput — sets берёт значение из input.* через CEL.
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

// TestRenderStateChanges_LastWins — per-host значение (soulprint.self) сворачивается
// last-wins по сортировке SID: побеждает хост с лексикографически последним SID.
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
		// Передаём в неотсортированном порядке — свёртка обязана сортировать сама.
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
	// c.example.com — последний по SID → gamma.
	if got["leader"] != "gamma" {
		t.Errorf("leader = %v, want gamma (last SID wins)", got["leader"])
	}
}

// TestRenderStateChanges_Empty — nil StateChanges и пустой sets → пустой map.
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

// TestRenderStateChanges_RegisterFromHost — register.* в sets читается из
// per-host RegisterByHost (слайс 2), НЕ из глобального RenderInput.Register
// (тот — cross-task chaining фазы Render, в sets не виден). Положительный путь:
// probe-задача дала register.probe.stdout → попадает в sets.
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
		t.Errorf("sets.x = %v, want \"hello\" (из register.probe.stdout)", got["x"])
	}
}

// TestRenderStateChanges_GlobalRegisterNotLeaked — глобальный
// RenderInput.Register (фаза Render) НЕ виден в sets: sets читает только
// RegisterByHost[sid]. При пустом RegisterByHost обращение к register.* даёт
// детерминированную eval-ошибку ("no such key"), несмотря на заполненный
// глобальный Register.
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
		t.Fatalf("RenderStateChanges: ожидалась eval-ошибка (got=%v): глобальный Register не должен протекать в sets", got)
	}
	if got != nil {
		t.Errorf("при ошибке рендера ожидался nil-результат, got = %v", got)
	}
}

// TestRenderStateChanges_RegisterLastWinsCrossHost — last-wins свёртка sets с
// register: при разных register-значениях по хостам в state попадает значение
// хоста с лексикографически последним SID.
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
		t.Errorf("sets.leader = %v, want \"from-b\" (last-wins по SID)", got["leader"])
	}
}

// TestRender_HostCountInCEL — incarnation.host_count = число targeted-хостов.
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

// TestRender_RunOnce_PicksFirstBySID — run_once: true → задача уходит ровно на
// один хост, детерминированно первый по SID, при N>1 хостов в таргете
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
		// Неотсортированный roster — резолв обязан выбрать первый по SID сам.
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
		t.Errorf("TargetSIDs = %v, want [a.example.com] (run_once → первый по SID)", got)
	}
	if plans[0].SerialWidth != 0 {
		t.Errorf("SerialWidth = %d, want 0 (run_once без serial)", plans[0].SerialWidth)
	}
}

// TestRender_RunOnce_ZeroHosts — run_once: при пустом таргете (where: отфильтровал
// всех) не паникует и даёт пустой DispatchPlan (общая семантика §5, без своей
// политики пустого таргета).
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

// TestRender_Serial_WidthInPlan — serial: N и "<N>%" вычисляются в SerialWidth
// плана против числа таргетов (orchestration.md §2.2.1). Сам нарезка на волны —
// scenario-dispatch (см. scenario/dispatch_test.go); здесь проверяем только
// корректность вычисленной ширины.
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
				t.Errorf("TargetSIDs len = %d, want 4 (serial не режет таргет, только ширину волны)", got)
			}
		})
	}
}

// TestSerialWidth — чистая функция вычисления ширины волны: int/percent/nil-формы,
// округление вверх процента, минимум 1.
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

// TestRender_Serial_PercentAfterWhere — процент serial: считается от числа
// хостов ПОСЛЕ where-фильтра, не от всего roster-а (orchestration.md §2.2.1):
// roster 4 хоста, where: оставляет 2 (по стабильному soulprint-факту), serial:
// "50%" → ceil(2*50/100)=1, а не ceil(4*50/100)=2. TargetSIDs тоже = 2 (where
// сужает таргет, serial — только ширину волны).
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
		t.Fatalf("TargetSIDs len = %d, want 2 (where: оставил debian-хосты a,c)", got)
	}
	if plans[0].SerialWidth != 1 {
		t.Errorf("SerialWidth = %d, want 1 (50%% от 2 where-хостов = ceil 1, НЕ 2 от полного roster-а)", plans[0].SerialWidth)
	}
}

// TestApplyRunOnce — резка таргета до первого по SID хоста.
func TestApplyRunOnce(t *testing.T) {
	mk := func(sids ...string) []*topology.HostFacts {
		out := make([]*topology.HostFacts, len(sids))
		for i, s := range sids {
			out[i] = host(s, nil, nil)
		}
		return out
	}

	t.Run("run_once false → без изменений", func(t *testing.T) {
		in := mk("c", "a", "b")
		got := applyRunOnce(in, false)
		if len(got) != 3 {
			t.Errorf("len = %d, want 3", len(got))
		}
	})
	t.Run("run_once true N>1 → первый по SID", func(t *testing.T) {
		got := applyRunOnce(mk("c", "a", "b"), true)
		if len(got) != 1 || got[0].SID != "a" {
			t.Errorf("got = %v, want [a]", sidsOf(got))
		}
	})
	t.Run("run_once true 1 хост → он же", func(t *testing.T) {
		got := applyRunOnce(mk("only"), true)
		if len(got) != 1 || got[0].SID != "only" {
			t.Errorf("got = %v, want [only]", sidsOf(got))
		}
	})
	t.Run("run_once true 0 хостов → пусто", func(t *testing.T) {
		got := applyRunOnce(mk(), true)
		if len(got) != 0 {
			t.Errorf("got = %v, want empty", sidsOf(got))
		}
	})
}
