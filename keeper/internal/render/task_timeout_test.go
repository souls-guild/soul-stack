package render

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ★ GUARD (NIM-949). The scenario level's task_timeout reaches every task on the
// wire, a keeper-side one included; a task's own level beats it either way, `0`
// lifting it; a block's settings: is its children's level, their own timeout:
// winning; an applier hands its effective value to the destiny's tasks, a block's
// children among them, whose own timeout: still wins.
//
// Mutations: wireTimeout ignores the scenario level → plain carries none; the
// block's settings: is not merged into its children → b-plain carries 30m; drop
// the Timeout row from mergeApplierInheritance → d-plain carries none; drop it
// from mergeBlockInheritance → d-blk-child carries none; format 1h as
// time.Duration.String → "1h0m0s".
func TestRender_ScenarioTaskTimeoutReachesTheWire(t *testing.T) {
	thirty := 30 * time.Minute
	zero := time.Duration(0)
	twoHours := 2 * time.Hour

	own := moduleTask("own", "core.exec.run")
	own.Timeout = "5m"
	lifted := moduleTask("lifted", "core.exec.run")
	lifted.Settings = &config.ScenarioSettings{TaskTimeout: &zero}
	hour := moduleTask("hour", "core.exec.run")
	hour.Settings = &config.ScenarioSettings{TaskTimeout: new(time.Hour)}
	keeper := moduleTask("keeper", "core.state.set")
	keeper.Module.Params = map[string]any{"path": "x", "value": 1}
	dOwn := moduleTask("d-own", "core.exec.run")
	dOwn.Timeout = "1m"
	bOwn := moduleTask("b-own", "core.exec.run")
	bOwn.Timeout = "2m"
	dBlkOwn := moduleTask("d-blk-own", "core.exec.run")
	dBlkOwn.Timeout = "3m"
	dBlock := config.Task{
		Name:  "d-grp",
		Block: &config.BlockTask{Block: []config.Task{moduleTask("d-blk-child", "core.exec.run"), dBlkOwn}},
	}
	block := config.Task{
		Name:     "grp",
		Settings: &config.ScenarioSettings{TaskTimeout: new(10 * time.Minute)},
		Block:    &config.BlockTask{Block: []config.Task{moduleTask("b-plain", "core.exec.run"), bOwn}},
	}

	in := RenderInput{
		Scenario: &config.ScenarioManifest{
			Name:     "s",
			Settings: &config.ScenarioSettings{TaskTimeout: &thirty},
			Tasks: []config.Task{
				moduleTask("plain", "core.exec.run"),
				own,
				lifted,
				hour,
				keeper,
				block,
				{Name: "apply-step", Apply: &config.ApplyTask{Destiny: "d"}, Settings: &config.ScenarioSettings{TaskTimeout: &twoHours}},
			},
		},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     staticResolver{&ResolvedDestiny{Name: "d", Tasks: []config.Task{moduleTask("d-plain", "core.exec.run"), dOwn, dBlock}}},
	}
	tasks, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := map[string]string{}
	for _, rt := range tasks {
		got[rt.Name] = rt.Timeout
	}
	for name, want := range map[string]string{
		"plain":       "30m",
		"own":         "5m",
		"lifted":      "",
		"hour":        "1h",
		"keeper":      "30m",
		"b-plain":     "10m",
		"b-own":       "2m",
		"d-plain":     "2h",
		"d-own":       "1m",
		"d-blk-child": "2h",
		"d-blk-own":   "3m",
	} {
		if got[name] != want {
			t.Errorf("%s timeout = %q, want %q (all: %v)", name, got[name], want, got)
		}
	}
}

// ★ GUARD (NIM-949). A scenario that writes no settings: anywhere puts every
// task's timeout: on the wire verbatim — the render of every scenario predating
// settings: is unchanged.
//
// Mutation: always format the effective duration (drop the own-timeout
// comparison) → "90s" goes out as "1m30s" → red.
func TestRender_NoSettingsKeepsTimeoutVerbatim(t *testing.T) {
	odd := moduleTask("odd", "core.exec.run")
	odd.Timeout = "90s"
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: []config.Task{odd, moduleTask("none", "core.exec.run")}},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, rt := range tasks {
		want := map[string]string{"odd": "90s", "none": ""}[rt.Name]
		if rt.Timeout != want {
			t.Errorf("%s timeout = %q, want %q", rt.Name, rt.Timeout, want)
		}
	}
}
