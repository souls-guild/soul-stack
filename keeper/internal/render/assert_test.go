package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// assertTask строит assert-задачу (ADR-009 amendment 2026-06-23) с предикатом по
// размеру roster-а — тот же инвариант, что cluster size-guard в redis create.
func assertTask(when, pred, message string) config.Task {
	return config.Task{
		Name:   "topology guard",
		When:   when,
		Assert: &config.AssertSpec{That: []string{pred}, Message: message},
	}
}

// TestAssert_PassEmitsNoTask — assert-предикат true → render проходит, задача в
// план НЕ эмитится (assert — проверка, не задача): план остаётся пустым, индекс не
// растёт. Рядом обычная module-задача проверяет, что её Index не съезжает под assert.
func TestAssert_PassEmitsNoTask(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		assertTask("", "size(soulprint.hosts) == 3", "want 3 hosts"),
		{
			Name:   "install",
			Module: &config.ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "redis-server"}},
		},
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, nil),
			host("b.example.com", []string{"svc"}, nil),
			host("c.example.com", []string{"svc"}, nil),
		},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: assert-pass не должен ронять render, got %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (assert не эмитит задачу, остаётся только install)", len(tasks))
	}
	if tasks[0].Index != 0 {
		t.Errorf("install Index = %d, want 0 (assert не резервирует индекс)", tasks[0].Index)
	}
	if tasks[0].Module != "core.pkg.installed" {
		t.Errorf("Module = %q, want core.pkg.installed", tasks[0].Module)
	}
}

// TestAssert_FailAbortsRenderWithMessage — assert-предикат false → render
// ОБРЫВАЕТСЯ ErrAssertFailed (ни одной задачи не возвращается, прогон не стартует),
// ошибка несёт авторский message + индекс/текст непрошедшего предиката.
func TestAssert_FailAbortsRenderWithMessage(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		assertTask("", "size(soulprint.hosts) == 3", "topology mismatch: want 3 hosts"),
		{
			Name:   "install",
			Module: &config.ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "redis-server"}},
		},
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{ // 2 хоста против ожидаемых 3 → assert false.
			host("a.example.com", []string{"svc"}, nil),
			host("b.example.com", []string{"svc"}, nil),
		},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatalf("Render: assert-fail должен оборвать render, got tasks=%d err=nil", len(tasks))
	}
	if !errors.Is(err, ErrAssertFailed) {
		t.Errorf("err не ErrAssertFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "topology mismatch: want 3 hosts") {
		t.Errorf("ошибка не несёт message автора: %v", err)
	}
	if !strings.Contains(err.Error(), "that[0]") {
		t.Errorf("ошибка не указывает индекс непрошедшего предиката: %v", err)
	}
	if tasks != nil {
		t.Errorf("tasks = %v, want nil (ни одной задачи на dispatch при abort)", tasks)
	}
}

// TestAssert_DefaultMessageOnEmpty — message опущен → ошибка несёт дефолт по имени
// задачи (а не пустую строку).
func TestAssert_DefaultMessageOnEmpty(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		assertTask("", "size(soulprint.hosts) == 5", ""),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: assert-fail должен оборвать render")
	}
	if !strings.Contains(err.Error(), "topology guard") {
		t.Errorf("дефолт-сообщение не несёт имя задачи: %v", err)
	}
}

// TestAssert_StaticWhenFalseSkips — assert гейтится `when:`: статически-false when
// (input-only предикат, неактивный режим) → assert НЕ вычисляется, render проходит,
// даже если предикат that[] был бы false. Зеркало cluster-assert на standalone-прогоне.
func TestAssert_StaticWhenFalseSkips(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		// when: режим != cluster → assert неактивен; that[] вычислился бы в false
		// (1 хост != 99), но до него дело не доходит.
		assertTask("input.redis_type == 'cluster'", "size(soulprint.hosts) == 99", "never reached"),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"redis_type": "standalone"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: static-when:false должен пропустить assert без вычисления, got %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0 (assert не эмитит задачу, when:false → не вычислялся)", len(tasks))
	}
}

// TestAssert_StaticWhenTrueEvaluates — обратный контроль: статически-true when →
// assert вычисляется; false-предикат на активном режиме обрывает render.
func TestAssert_StaticWhenTrueEvaluates(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		assertTask("input.redis_type == 'cluster'", "size(soulprint.hosts) == 99", "topology mismatch"),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"redis_type": "cluster"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: when:true + false-предикат должен оборвать render")
	}
	if !errors.Is(err, ErrAssertFailed) {
		t.Errorf("err не ErrAssertFailed: %v", err)
	}
}

// TestEvalAsserts_SameSourceAsRender — REUSE-ИНВАРИАНТ (ADR-009 amendment,
// двухточечная eval): pre-flight (EvalAsserts) и render-ветка (Render →
// evalAssertTask) вычисляют assert ОДНИМ источником (evalAssertTask), без
// диалекта. Тест прогоняет ОДИН и тот же input через обе точки и сверяет, что
// вердикт совпадает (оба проходят / оба роняют с тем же message и тем же
// ErrAssertFailed). Расхождение = диалект (баг).
func TestEvalAsserts_SameSourceAsRender(t *testing.T) {
	cases := []struct {
		name      string
		predicate string
		hosts     int // число хостов roster-а
	}{
		{"pass_3_hosts", "size(soulprint.hosts) == 3", 3},
		{"fail_2_vs_3", "size(soulprint.hosts) == 3", 2},
		{"fail_0_vs_3", "size(soulprint.hosts) == 3", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
				assertTask("", tc.predicate, "topology mismatch"),
			}}
			hosts := make([]*topology.HostFacts, tc.hosts)
			for i := range hosts {
				hosts[i] = host(string(rune('a'+i))+".example.com", []string{"svc"}, nil)
			}
			in := RenderInput{
				Scenario:    manifest,
				Incarnation: IncarnationMeta{Name: "svc"},
				Hosts:       hosts,
			}

			pRender := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, renderErr := pRender.Render(context.Background(), in)

			pPre := NewPipeline(nil, newEngine(t), nil, nil)
			preErr := pPre.EvalAsserts(context.Background(), in)

			// Оба либо nil, либо оба ErrAssertFailed — вердикт обязан совпасть.
			if (renderErr == nil) != (preErr == nil) {
				t.Fatalf("вердикт расходится: render=%v, EvalAsserts=%v (диалект)", renderErr, preErr)
			}
			if renderErr != nil {
				if !errors.Is(renderErr, ErrAssertFailed) || !errors.Is(preErr, ErrAssertFailed) {
					t.Fatalf("не ErrAssertFailed на обеих точках: render=%v, pre=%v", renderErr, preErr)
				}
				if renderErr.Error() != preErr.Error() {
					t.Errorf("текст ошибки расходится:\n render=%q\n pre=   %q", renderErr.Error(), preErr.Error())
				}
			}
		})
	}
}

// TestEvalAsserts_NoAssertNoOp — сценарий без assert-задач → EvalAsserts no-op
// (nil), даже с module-задачами в плане: pre-flight не должен ломать сценарии без
// assert (большинство).
func TestEvalAsserts_NoAssertNoOp(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		{Name: "install", Module: &config.ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "redis-server"}}},
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	if err := p.EvalAsserts(context.Background(), in); err != nil {
		t.Fatalf("EvalAsserts: сценарий без assert должен быть no-op, got %v", err)
	}
}

// TestEvalAsserts_StaticWhenFalseSkips — when-гейт соблюдён в pre-flight тоже:
// статически-false when → assert не вычисляется (зеркало render-ветки и
// TestAssert_StaticWhenFalseSkips).
func TestEvalAsserts_StaticWhenFalseSkips(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		assertTask("input.redis_type == 'cluster'", "size(soulprint.hosts) == 99", "never reached"),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"redis_type": "standalone"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	if err := p.EvalAsserts(context.Background(), in); err != nil {
		t.Fatalf("EvalAsserts: static-when:false должен пропустить assert, got %v", err)
	}
}
