package render

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// optionalInputApplyTask — задача неактивной ветки multi-action destiny: params
// читают optional-input (`${ input.maxmemory }`), который при текущем action не
// передан, а `when: input.action == 'apply'` режет её. Eager-рендер params упал бы
// на no-such-key; static-when placeholder-skip их пропускает (ADR-012(d), Var b).
func optionalInputApplyTask() config.Task {
	return config.Task{
		Name:   "apply-config",
		When:   "input.action == 'apply'",
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-cli config set maxmemory ${ input.maxmemory }"}},
	}
}

// TestStaticWhenFalse_SkipsParamRender — ★ ключевой кейс ТЗ: задача с optional-input
// в params + `when: input.action=='apply'` при action!=apply → Render НЕ падает
// (params пропущены, no-such-key на `${ input.maxmemory }` не достигается).
func TestStaticWhenFalse_SkipsParamRender(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "multi-action", Tasks: []config.Task{optionalInputApplyTask()}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"}, // НЕ apply → maxmemory не передан
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: static-when:false должен скипнуть рендер params (optional-input не достигается), got %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (placeholder остаётся в плане)", len(tasks))
	}
	rt := tasks[0]
	if rt.Params != nil {
		t.Errorf("Params = %v, want nil (рендер пропущен)", rt.Params.AsMap())
	}
	if rt.When != "input.action == 'apply'" {
		t.Errorf("When = %q, want протянутый as-is предикат", rt.When)
	}
	if rt.Module != "core.exec.run" {
		t.Errorf("Module = %q, want core.exec.run (плейсхолдер несёт модуль)", rt.Module)
	}
	if rt.FlowContext == nil {
		t.Error("FlowContext = nil — Soul должен получить flow_context для собственного evalWhen")
	}
}

// TestStaticWhenTrue_RendersParams — контроль обратного: тот же предикат при
// action==apply → static-when:true → НЕ skip, params рендерятся обычным путём.
func TestStaticWhenTrue_RendersParams(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "multi-action", Tasks: []config.Task{optionalInputApplyTask()}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "apply", "maxmemory": "256mb"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := tasks[0].Params.GetFields()["cmd"].GetStringValue()
	if got != "redis-cli config set maxmemory 256mb" {
		t.Errorf("cmd = %q, want отрендеренную команду (static-when:true → params рендерятся)", got)
	}
}

// TestStaticSkip_OnChangesIndicesIntact — задача-источник onchanges static-skipped:
// её Index/remap целы, потребитель onchanges трактует «не changed» (источник в
// registerByIdx с changed=false ⇒ потребитель пропустится, что корректно). Здесь
// проверяем render-инвариант: skip НЕ ломает сквозную нумерацию и резолв onchanges.
func TestStaticSkip_OnChangesIndicesIntact(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "onchanges-skip",
		Tasks: []config.Task{
			{
				Name:     "src",
				When:     "input.action == 'apply'", // static-false при update_acls
				Register: "src",
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "touch ${ input.path }"}},
			},
			{
				Name:      "consumer",
				OnChanges: []string{"src"},
				Module:    &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo reload"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (skip не выкидывает задачу из плана)", len(tasks))
	}
	if tasks[0].Index != 0 || tasks[1].Index != 1 {
		t.Errorf("Index = %d,%d, want 0,1 (сквозная нумерация цела)", tasks[0].Index, tasks[1].Index)
	}
	if tasks[0].Params != nil {
		t.Error("источник static-skipped → Params должны быть nil")
	}
	// onchanges резолвлен в Index источника (0): потребитель ссылается на src.
	if len(tasks[1].OnChangesIdx) != 1 || tasks[1].OnChangesIdx[0] != 0 {
		t.Errorf("OnChangesIdx = %v, want [0] (резолв register-имени в Index источника цел)", tasks[1].OnChangesIdx)
	}
	if plans[1].TaskIndex != 1 {
		t.Errorf("plans[1].TaskIndex = %d, want 1", plans[1].TaskIndex)
	}
}

// TestStaticSkip_ConsistentAcrossPassages — static-when даёт ОДИНАКОВЫЙ skip в
// Passage 0 и Passage 1 при одном input/state-снимке (ADR-056: State инвариантен по
// passages, static-when register-/soulprint-независим ⇒ детерминирован). Один и тот
// же RenderInput, рендеренный как разные ActivePassage, скипает одинаково.
func TestStaticSkip_ConsistentAcrossPassages(t *testing.T) {
	task := optionalInputApplyTask()
	manifest := &config.ScenarioManifest{Name: "passages", Tasks: []config.Task{task}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	base := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		TaskPassage: []int{0},
	}

	in0 := base
	in0.ActivePassage = 0
	t0, _, err := p.Render(context.Background(), in0)
	if err != nil {
		t.Fatalf("Render P0: %v", err)
	}

	in1 := base
	in1.ActivePassage = 0 // та же активная стадия — повторный рендер того же снимка
	t1, _, err := p.Render(context.Background(), in1)
	if err != nil {
		t.Fatalf("Render P0 (повтор): %v", err)
	}

	if (t0[0].Params == nil) != (t1[0].Params == nil) {
		t.Errorf("skip непоследователен между прогонами: P0.Params==nil=%v, повтор.Params==nil=%v",
			t0[0].Params == nil, t1[0].Params == nil)
	}
	if t0[0].Params != nil {
		t.Error("оба прогона должны скипнуть (static-when:false на том же снимке)")
	}
}

// TestStaticSkipEqualsSoulSkip — ★ static-when-false на Keeper == when-false на
// Soul. Keeper скипнул (Params==nil) ровно тогда, когда Soul-side evalWhen по тому
// же flow_context (NewFlowControl, та же sandbox) вернул false. Бит-в-бит гарантия
// (ADR-012(d)): один env, один flow_context.
func TestStaticSkipEqualsSoulSkip(t *testing.T) {
	task := optionalInputApplyTask()
	manifest := &config.ScenarioManifest{Name: "equiv", Tasks: []config.Task{task}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	keeperSkipped := tasks[0].Params == nil

	// Soul-side воспроизведение evalWhen: тот же flow-control-движок + flow_context
	// из RenderedTask (то, что реально поедет на Soul).
	soulEngine, err := cel.NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	soulWhen, err := soulEngine.EvalPredicate(tasks[0].When, flowControlVarsFromStruct(tasks[0].FlowContext, nil))
	if err != nil {
		t.Fatalf("Soul-side evalWhen: %v", err)
	}
	soulSkipped := !soulWhen

	if keeperSkipped != soulSkipped {
		t.Errorf("Keeper static-skip=%v != Soul when-skip=%v — расхождение sandbox/flow_context", keeperSkipped, soulSkipped)
	}
	if !keeperSkipped {
		t.Error("ожидался skip обеими сторонами (action=update_acls != apply)")
	}
}

// TestRegisterWhen_StaysSoulSide — `when: register.X` НЕ static-skipped: register
// известен только Soul-у, предикат остаётся Soul-side, params рендерятся прежним
// путём (Keeper его не вычисляет).
func TestRegisterWhen_StaysSoulSide(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "register-when",
		Tasks: []config.Task{
			{
				Name:   "t",
				When:   "register.probe.changed",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.user }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"user": "alice"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if tasks[0].Params == nil {
		t.Fatal("register-when НЕ должен скипать рендер params (остаётся Soul-side)")
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo alice" {
		t.Errorf("cmd = %q, want отрендеренную команду (params рендерятся прежним путём)", got)
	}
	if tasks[0].When != "register.probe.changed" {
		t.Errorf("When = %q, want протянутый as-is предикат", tasks[0].When)
	}
}

// TestMixedWhen_NotStatic — ★ реверс: `when: input.a && register.b` смешан
// (есть register) → НЕ статический → прежний путь (params рендерятся). Если бы
// детектор ошибочно посчитал его статическим, он попытался бы вычислить
// register-зависимый предикат на Keeper-е (register пуст) и/или скипнуть по неполному
// контексту. isStaticWhen обязан вернуть false из-за register-ссылки.
func TestMixedWhen_NotStatic(t *testing.T) {
	if isStaticWhen("input.a && register.b.changed") {
		t.Fatal("isStaticWhen(input.a && register.b) = true, want false (register-зависимый)")
	}
	manifest := &config.ScenarioManifest{
		Name: "mixed-when",
		Tasks: []config.Task{
			{
				Name:   "t",
				When:   "input.a && register.b.changed",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.user }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"a": true, "user": "bob"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: смешанный when НЕ должен вычисляться Keeper-side, got %v", err)
	}
	if tasks[0].Params == nil {
		t.Fatal("смешанный when (с register) НЕ должен скипать рендер params")
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo bob" {
		t.Errorf("cmd = %q, want echo bob (прежний путь)", got)
	}
}

// TestStaticWhenFalse_UnsupportedDSL_PrecedesGuard — ★ 12-й слой: задача неактивной
// ветки с unsupported-DSL (`parallel: true`) + статически-false when: → static-when
// ПРЕДШЕСТВУЕТ guardPilotDSL: задача gated off и скипается ДО guard, не отвергаясь
// ErrUnsupportedDSL. Активная ветка (другая задача) рендерится. Реверс: до фикса
// guardPilotDSL отвергал parallel: даже в неактивной ветке и ронял весь Render.
func TestStaticWhenFalse_UnsupportedDSL_PrecedesGuard(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "multi-action-parallel",
		Tasks: []config.Task{
			{
				Name:     "diagnose (parallel, gated off)",
				When:     "input.action == 'diagnose'", // static-false при action=update_acls
				Parallel: true,                         // unsupported-DSL — guard отверг бы его ДО фикса
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-cli ping"}},
			},
			{
				Name:   "active update_acls",
				When:   "input.action == 'update_acls'",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.user }"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "update_acls", "user": "alice"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: static-when ДОЛЖЕН предшествовать guardPilotDSL — gated-off parallel-задача не должна ронять Render, got %v", err)
	}
	if len(tasks) != 2 || len(plans) != 2 {
		t.Fatalf("len(tasks)=%d len(plans)=%d, want 2,2 (skip-placeholder + активная)", len(tasks), len(plans))
	}
	// Задача 0 — gated-off parallel: skip-placeholder, params не рендерились.
	if tasks[0].Params != nil {
		t.Errorf("tasks[0].Params != nil — gated-off parallel должен быть skip-placeholder")
	}
	if tasks[0].When != "input.action == 'diagnose'" {
		t.Errorf("tasks[0].When = %q, want протянутый предикат", tasks[0].When)
	}
	if tasks[0].FlowContext == nil {
		t.Error("tasks[0].FlowContext == nil — Soul нужен flow_context для собственного evalWhen → SKIPPED")
	}
	// Задача 1 — активная: рендерится обычным путём.
	if tasks[1].Params == nil {
		t.Fatal("tasks[1].Params == nil — активная update_acls должна отрендериться")
	}
	if got := tasks[1].Params.GetFields()["cmd"].GetStringValue(); got != "echo alice" {
		t.Errorf("tasks[1].cmd = %q, want echo alice (активная ветка рендерится)", got)
	}
}

// TestStaticWhenTrue_UnsupportedDSL_StillRejected — реверс на over-skip: тот же
// parallel: + when: при action==diagnose → static-TRUE → задача АКТИВНА → guard
// ОТВЕРГАЕТ ErrUnsupportedDSL. Per-action валидация: unsupported-DSL отвергается
// ровно при активации ветки, не маскируется.
func TestStaticWhenTrue_UnsupportedDSL_StillRejected(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "active-parallel",
		Tasks: []config.Task{
			{
				Name:     "diagnose (parallel, ACTIVE)",
				When:     "input.action == 'diagnose'", // static-TRUE при action=diagnose
				Parallel: true,
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-cli ping"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"action": "diagnose"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (активная parallel-задача отвергается per-action)", err)
	}
}

// TestNonStaticWhen_UnsupportedDSL_StillRejected — не-static when: (`register.x`)
// + parallel: → задача НЕ статически-false (register известен только Soul-у),
// минует ранний static-skip → guard отвергает ErrUnsupportedDSL прежним путём.
// Гарантия, что ранний skip не ослабляет guard для register-/mixed-веток.
func TestNonStaticWhen_UnsupportedDSL_StillRejected(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "register-parallel",
		Tasks: []config.Task{
			{
				Name:     "parallel gated by register",
				When:     "register.probe.changed", // не статический → Soul-side
				Parallel: true,
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-cli ping"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (register-when parallel не статичен → guard отвергает)", err)
	}
}

// TestStaticWhenFalse_UnsupportedDSL_PrecedesGuard_Destiny — тот же инвариант в
// destiny-render-проходе (apply:destiny): gated-off parallel-задача destiny
// неактивной ветки минует guardDestinyTask, активная задача destiny рендерится.
// Зеркало диагностики redis-destiny (manage.yml update_acls активна, diagnostic.yml
// diagnose gated off с parallel:).
func TestStaticWhenFalse_UnsupportedDSL_PrecedesGuard_Destiny(t *testing.T) {
	d := &ResolvedDestiny{
		Name: "multi-action",
		Input: config.InputSchemaMap{
			"action": {Type: "string", Required: true},
			"user":   {Type: "string", Required: false},
		},
		Tasks: []config.Task{
			{
				Name:     "diagnose (parallel, gated off)",
				When:     "input.action == 'diagnose'", // static-false при action=update_acls
				Parallel: true,
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-cli ping"}},
			},
			{
				Name:   "active update_acls",
				When:   "input.action == 'update_acls'",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.user }"}},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: applyScenario("multi-action", map[string]any{
			"action": "update_acls",
			"user":   "alice",
		}),
		Input:       map[string]any{"action": "update_acls", "user": "alice"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: static-when ДОЛЖЕН предшествовать guardDestinyTask в destiny-проходе, got %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (skip-placeholder destiny + активная destiny)", len(tasks))
	}
	if tasks[0].Params != nil {
		t.Errorf("tasks[0].Params != nil — gated-off parallel destiny должен быть skip-placeholder")
	}
	if tasks[1].Params == nil {
		t.Fatal("tasks[1].Params == nil — активная update_acls destiny должна отрендериться")
	}
	if got := tasks[1].Params.GetFields()["cmd"].GetStringValue(); got != "echo alice" {
		t.Errorf("tasks[1].cmd = %q, want echo alice", got)
	}
}

// TestStaticWhenTrue_UnsupportedDSL_StillRejected_Destiny — реверс destiny:
// активная parallel-задача destiny отвергается guardDestinyTask per-action.
func TestStaticWhenTrue_UnsupportedDSL_StillRejected_Destiny(t *testing.T) {
	d := &ResolvedDestiny{
		Name:  "active",
		Input: config.InputSchemaMap{"action": {Type: "string", Required: true}},
		Tasks: []config.Task{
			{
				Name:     "diagnose (parallel, ACTIVE)",
				When:     "input.action == 'diagnose'",
				Parallel: true,
				Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-cli ping"}},
			},
		},
	}
	res := &stubDestinyResolver{resolved: d}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyScenario("active", map[string]any{"action": "diagnose"}),
		Input:       map[string]any{"action": "diagnose"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     res,
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("err = %v, want ErrUnsupportedDSL (активная parallel destiny отвергается per-action)", err)
	}
}

// TestIsStaticWhen_Classification — таблица классификатора: какие when статичны.
func TestIsStaticWhen_Classification(t *testing.T) {
	cases := []struct {
		when string
		want bool
	}{
		{"", false},                       // пусто — нечего вычислять Keeper-side
		{"input.action == 'apply'", true}, // только input — статичен
		{"essence.enabled && incarnation.name != ''", true}, // essence+incarnation — статичен
		{"vars.flag", true},                             // vars — статичен (host-инвариантность ловит второй контур)
		{"register.probe.changed", false},               // register — Soul-side
		{"input.a && register.b.ok", false},             // смешанный с register — Soul-side
		{"soulprint.self.os.family == 'debian'", false}, // soulprint — host-вариативен
		{"input.a && soulprint.self.x", false},          // смешанный с soulprint — host-вариативен
	}
	for _, c := range cases {
		if got := isStaticWhen(c.when); got != c.want {
			t.Errorf("isStaticWhen(%q) = %v, want %v", c.when, got, c.want)
		}
	}
}
