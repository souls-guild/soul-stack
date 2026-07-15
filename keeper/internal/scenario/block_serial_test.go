package scenario

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestDispatch_BlockSerialWave (guard #3, integration part) proves
// block.serial:1 on 3 hosts rolls the WHOLE block in one wave per host: each
// host's ApplyRequest carries ALL of the block's children (groupByHost), and
// wave width = 1 → 3 waves of 1 (splitWaves). This "whole block, one wave"
// behavior is emergent (same SerialWidth+TargetSIDs on every child), no
// contract change needed.
func TestDispatch_BlockSerialWave(t *testing.T) {
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	p := render.NewPipeline(nil, engine, nil, nil)

	mod := func(name, module string) config.Task {
		return config.Task{Name: name, Module: &config.ModuleTask{Module: module, Params: map[string]any{}}}
	}
	task := config.Task{
		Name:   "rolling-restart",
		Serial: 1,
		Block: &config.BlockTask{Block: []config.Task{
			mod("Restart redis-server", "core.service.restarted"),
			mod("Wait until healthy", "core.exec.run"),
		}},
	}
	hosts := []*topology.HostFacts{
		{SID: "a.example.com", Coven: []string{"svc"}},
		{SID: "b.example.com", Coven: []string{"svc"}},
		{SID: "c.example.com", Coven: []string{"svc"}},
	}
	in := render.RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "restart", Tasks: []config.Task{task}},
		Incarnation: render.IncarnationMeta{Name: "svc"},
		Hosts:       hosts,
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (2 block children, fan-out)", len(tasks))
	}

	// Each host's ApplyRequest carries BOTH block children.
	perHost := groupByHost(tasks, plans)
	if len(perHost) != 3 {
		t.Fatalf("hosts = %d, want 3", len(perHost))
	}
	for _, sid := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		if len(perHost[sid]) != 2 {
			t.Errorf("host %s несёт %d задач, want 2 (весь блок)", sid, len(perHost[sid]))
		}
	}

	// Wave width = 1 (inherited from block.serial by all children) → 3 waves of 1.
	width := effectiveSerialWidth(plans)
	if width != 1 {
		t.Fatalf("effectiveSerialWidth = %d, want 1", width)
	}
	waves := splitWaves(sortedSIDs(perHost), width)
	if len(waves) != 3 {
		t.Fatalf("waves = %d, want 3 (serial:1 на 3 хостах)", len(waves))
	}
	for i, w := range waves {
		if len(w) != 1 {
			t.Errorf("wave[%d] = %v, want ровно 1 хост", i, w)
		}
	}
}

// TestDispatch_NestedBlockSerialMinWidth (QA gap #9) covers 3-level nesting of
// block-in-block-in-block with decreasing serial (L1=3 / L2=2 / L3=1) across 6
// hosts. Each level carries a module child, so plans with widths 3, 2, and 1
// coexist. effectiveSerialWidth takes the MINIMUM positive window across all
// Passage plans (fail-closed: the narrowest window wins) → leaf width = 1, and
// splitWaves rolls 6 waves of 1 host. Proves a nested serial:1 doesn't get
// swallowed by a wider outer serial:3.
func TestDispatch_NestedBlockSerialMinWidth(t *testing.T) {
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	p := render.NewPipeline(nil, engine, nil, nil)

	mod := func(name string) config.Task {
		return config.Task{Name: name, Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}}
	}
	// L1 (serial:3) → [module L1-step, L2-block]
	// L2 (serial:2) → [module L2-step, L3-block]
	// L3 (serial:1) → [module L3-step]
	task := config.Task{
		Name:   "L1",
		Serial: 3,
		Block: &config.BlockTask{Block: []config.Task{
			mod("L1-step"),
			{
				Name:   "L2",
				Serial: 2,
				Block: &config.BlockTask{Block: []config.Task{
					mod("L2-step"),
					{
						Name:   "L3",
						Serial: 1,
						Block:  &config.BlockTask{Block: []config.Task{mod("L3-step")}},
					},
				}},
			},
		}},
	}
	hosts := make([]*topology.HostFacts, 6)
	for i := range hosts {
		hosts[i] = &topology.HostFacts{SID: string(rune('a'+i)) + ".example.com", Coven: []string{"svc"}}
	}
	in := render.RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "nested", Tasks: []config.Task{task}},
		Incarnation: render.IncarnationMeta{Name: "svc"},
		Hosts:       hosts,
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3 (L1-step + L2-step + L3-step)", len(tasks))
	}

	// All three widths appear among the plans (3 from L1, 2 from L2, 1 from L3).
	widths := map[int]bool{}
	for _, pl := range plans {
		widths[pl.SerialWidth] = true
	}
	for _, w := range []int{1, 2, 3} {
		if !widths[w] {
			t.Errorf("ожидался план с SerialWidth=%d, got widths=%v", w, widths)
		}
	}

	// effectiveSerialWidth = minimum positive = 1 (the narrow L3 window wins).
	width := effectiveSerialWidth(plans)
	if width != 1 {
		t.Fatalf("effectiveSerialWidth = %d, want 1 (min среди {3,2,1} — самое узкое окно)", width)
	}

	perHost := groupByHost(tasks, plans)
	waves := splitWaves(sortedSIDs(perHost), width)
	if len(waves) != 6 {
		t.Fatalf("waves = %d, want 6 (leaf width=1 на 6 хостах)", len(waves))
	}
	for i, w := range waves {
		if len(w) != 1 {
			t.Errorf("wave[%d] = %v, want ровно 1 хост (leaf width=1)", i, w)
		}
	}
}
