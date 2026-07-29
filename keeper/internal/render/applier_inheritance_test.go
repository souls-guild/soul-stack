package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// An applier expands into a group of destiny tasks, and until NIM-245 the
// function that rendered that group never saw the applier task — so its own
// keys reached nothing and nothing said so. These pin what now travels, what
// deliberately does not, and that the working `when:` path is untouched.

// applierEnv builds a one-host run: an async probe with `register: cfg`,
// followed by the applier under test.
func applierEnv(t *testing.T, applier config.Task) ([]*RenderedTask, error) {
	t.Helper()
	res := &ResolvedDestiny{
		Name:  "d",
		Tasks: []config.Task{moduleTask("d-step", "core.exec.run")},
	}
	probe := moduleTask("probe", "core.exec.run")
	probe.Register = "cfg"
	probe.Async = true
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{probe, applier}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     staticResolver{res},
	}
	tasks, _, err := p.Render(context.Background(), in)
	return tasks, err
}

func TestApplier_OnChangesReachesTheGroup(t *testing.T) {
	tasks, err := applierEnv(t, config.Task{
		Name:      "apply-step",
		Apply:     &config.ApplyTask{Destiny: "d"},
		OnChanges: []string{"cfg"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (probe + one destiny task)", len(tasks))
	}
	if len(tasks[1].OnChangesIdx) != 1 || tasks[1].OnChangesIdx[0] != 0 {
		t.Errorf("destiny task OnChangesIdx = %v, want [0] (the applier's onchanges: [cfg])", tasks[1].OnChangesIdx)
	}
}

func TestApplier_OnFailReachesTheGroup(t *testing.T) {
	tasks, err := applierEnv(t, config.Task{
		Name:   "apply-step",
		Apply:  &config.ApplyTask{Destiny: "d"},
		OnFail: []string{"cfg"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks[1].OnFailIdx) != 1 || tasks[1].OnFailIdx[0] != 0 {
		t.Errorf("destiny task OnFailIdx = %v, want [0] (the applier's onfail: [cfg])", tasks[1].OnFailIdx)
	}
}

func TestApplier_RequireReachesTheGroup(t *testing.T) {
	tasks, err := applierEnv(t, config.Task{
		Name:    "apply-step",
		Apply:   &config.ApplyTask{Destiny: "d"},
		Require: []string{"cfg"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks[1].RequireIdx) != 1 || tasks[1].RequireIdx[0] != 0 {
		t.Errorf("destiny task RequireIdx = %v, want [0] (the applier's require: [cfg])", tasks[1].RequireIdx)
	}
}

// Union with the destiny task's own requisite, exactly as a block does it —
// the destiny keeps naming its own source and gains the applier's.
func TestApplier_RequisitesUnionWithTheDestinysOwn(t *testing.T) {
	inner := moduleTask("d-step", "core.exec.run")
	inner.OnChanges = []string{"inner_reg"}
	innerSrc := moduleTask("inner-src", "core.exec.run")
	innerSrc.Register = "inner_reg"
	res := &ResolvedDestiny{Name: "d", Tasks: []config.Task{innerSrc, inner}}

	probe := moduleTask("probe", "core.exec.run")
	probe.Register = "cfg"
	applier := config.Task{
		Name:      "apply-step",
		Apply:     &config.ApplyTask{Destiny: "d"},
		OnChanges: []string{"cfg"},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{probe, applier}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     staticResolver{res},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// 0=probe(cfg) 1=inner-src(inner_reg) 2=d-step.
	got := tasks[2].OnChangesIdx
	if len(got) != 2 {
		t.Fatalf("d-step.OnChangesIdx = %v, want union of 2 (applier cfg + own inner_reg)", got)
	}
	seen := map[int]bool{got[0]: true, got[1]: true}
	if !seen[0] || !seen[1] {
		t.Errorf("d-step.OnChangesIdx = %v, want {0,1}", got)
	}
}

// Negative — an applier without requisites leaves the destiny exactly as it
// was authored.
func TestApplier_NoRequisitesLeavesTheDestinyAlone(t *testing.T) {
	tasks, err := applierEnv(t, config.Task{
		Name:  "apply-step",
		Apply: &config.ApplyTask{Destiny: "d"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	rt := tasks[1]
	if len(rt.OnChangesIdx) != 0 || len(rt.OnFailIdx) != 0 || len(rt.RequireIdx) != 0 || rt.RequireAll {
		t.Errorf("destiny task carries requisites it was not given: onchanges=%v onfail=%v require=%v/%v",
			rt.OnChangesIdx, rt.OnFailIdx, rt.RequireIdx, rt.RequireAll)
	}
}

// A `when:` reading register/soulprint cannot be decided Keeper-side and cannot
// be pushed onto a group rendered in another env — refused rather than dropped.
func TestApplier_DynamicWhenRejected(t *testing.T) {
	for _, when := range []string{
		"register.cfg.changed",
		"soulprint.self.os.family == 'debian'",
	} {
		t.Run(when, func(t *testing.T) {
			_, err := applierEnv(t, config.Task{
				Name:  "apply-step",
				Apply: &config.ApplyTask{Destiny: "d"},
				When:  when,
			})
			if !errors.Is(err, ErrUnsupportedDSL) {
				t.Fatalf("Render err = %v, want ErrUnsupportedDSL", err)
			}
			if !strings.Contains(err.Error(), "where:") {
				t.Errorf("the refusal must name the alternative that works; got %v", err)
			}
		})
	}
}

// The working form is untouched: a static-true `when:` renders the group
// normally...
func TestApplier_StaticTrueWhenRendersTheGroup(t *testing.T) {
	tasks, err := applierEnv(t, config.Task{
		Name:  "apply-step",
		Apply: &config.ApplyTask{Destiny: "d"},
		When:  "incarnation.name == 'svc'",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 || tasks[1].Module != "core.exec.run" {
		t.Fatalf("static-true when: must render the destiny group, got %d tasks", len(tasks))
	}
}

// ...and a static-false one still collapses it into a single skip placeholder.
func TestApplier_StaticFalseWhenCollapsesToPlaceholder(t *testing.T) {
	tasks, err := applierEnv(t, config.Task{
		Name:     "apply-step",
		Apply:    &config.ApplyTask{Destiny: "d"},
		Register: "applied",
		When:     "incarnation.name == 'other'",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (probe + one skip placeholder)", len(tasks))
	}
	if tasks[1].Register != "applied" {
		t.Errorf("placeholder Register = %q, want the applier's own %q", tasks[1].Register, "applied")
	}
	if tasks[1].Params != nil {
		t.Errorf("placeholder must carry no rendered params, got %v", tasks[1].Params)
	}
}

// A block ANDs its own when: into an applier descendant, so a block gated on
// register turns the child's when: dynamic — the same boundary, caught by the
// keeper-side guard rather than by the offline validator, which never sees the
// merged predicate.
func TestApplier_DynamicWhenInheritedFromBlockRejected(t *testing.T) {
	res := &ResolvedDestiny{Name: "d", Tasks: []config.Task{moduleTask("d-step", "core.exec.run")}}
	probe := moduleTask("probe", "core.exec.run")
	probe.Register = "cfg"
	grp := config.Task{
		Name: "grp",
		When: "register.cfg.changed",
		Block: &config.BlockTask{Block: []config.Task{
			{Name: "apply-step", Apply: &config.ApplyTask{Destiny: "d"}},
		}},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{probe, grp}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     staticResolver{res},
	}
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("Render err = %v, want ErrUnsupportedDSL for a block-inherited dynamic when: on an applier", err)
	}
}
