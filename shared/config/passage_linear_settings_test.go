package config

import (
	"testing"
	"time"
)

func mod(name string) Task { return Task{Name: name, Module: &ModuleTask{Module: "core.exec.run"}} }

func withSoulTimeout(t Task, d time.Duration) Task {
	t.Settings = &ScenarioSettings{SoulTimeout: &d}
	return t
}

func blockOf(name string, children ...Task) Task {
	return Task{Name: name, Block: &BlockTask{Block: children}}
}

// ★ GUARD (NIM-949). StepSettings holds one entry per step, in LinearPassage's
// numbering, each the merged settings of that step's barrier owner: a default
// block's child carries the block's levels, a group is one step at the block's
// level, and an empty default block takes a step of its own.
//
// Mutations: skip the default-block descent (one entry per top-level task) → the
// length disagrees with Count → red; merge the child over the scenario only (drop
// LinearBlockChild) → step 2 answers the scenario's 30s instead of the block's 3m
// → red.
func TestStepSettings_AlignedWithLinearPassage(t *testing.T) {
	thirty := 30 * time.Second
	group := withSoulTimeout(blockOf("canary", mod("g0"), mod("g1")), 5*time.Minute)
	group.Serial = 1
	block := "block"
	group.Settings.SerialScope = &block

	tasks := []Task{
		withSoulTimeout(mod("probe"), 2*time.Minute),
		withSoulTimeout(blockOf("overlay",
			withSoulTimeout(mod("c0"), 4*time.Minute),
			mod("c1"),
			blockOf("nested", mod("n0")),
		), 3*time.Minute),
		group,
		blockOf("empty"),
	}
	scenario := &ScenarioSettings{SoulTimeout: &thirty}

	passage, _ := LinearPassage(tasks, scenario)
	got := StepSettings(tasks, scenario)
	if len(got) != passage.Count {
		t.Fatalf("StepSettings has %d steps, LinearPassage counts %d", len(got), passage.Count)
	}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 3 * time.Minute, 3 * time.Minute, 5 * time.Minute, thirty}
	for step, w := range want {
		if d := *got[step].SoulTimeout; d != w {
			t.Errorf("step %d soul_timeout = %s, want %s", step, d, w)
		}
	}
	if d := *got[0].ScenarioTimeout; d != 24*time.Hour {
		t.Errorf("step 0 scenario_timeout = %s, want the 24h default under every step", d)
	}
	if empty := StepSettings(nil, nil); len(empty) != 1 || *empty[0].SoulTimeout != time.Minute {
		t.Errorf("empty plan = %+v, want one step at the defaults", empty)
	}
}

// ★ GUARD (NIM-949). The effective task_timeout: the task's own level — its
// timeout: folded in, an explicit settings.task_timeout winning — over the
// scenario's, over no limit.
//
// Mutation: merge the scenario level over the task's → "own timeout" answers 30m
// → red.
func TestEffectiveTaskTimeout(t *testing.T) {
	thirty := 30 * time.Minute
	zero := time.Duration(0)
	hour := time.Hour
	scenario := &ScenarioSettings{TaskTimeout: &thirty}

	for _, tc := range []struct {
		name     string
		scenario *ScenarioSettings
		task     Task
		want     time.Duration
	}{
		{"no limit anywhere", nil, mod("a"), 0},
		{"scenario level", scenario, mod("a"), thirty},
		{"own timeout", scenario, Task{Name: "a", Timeout: "5m"}, 5 * time.Minute},
		{"own zero lifts the scenario's", scenario, Task{Name: "a", Settings: &ScenarioSettings{TaskTimeout: &zero}}, 0},
		{"explicit beats timeout:", nil, Task{Name: "a", Timeout: "5m", Settings: &ScenarioSettings{TaskTimeout: &hour}}, hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveTaskTimeout(tc.scenario, tc.task); got != tc.want {
				t.Errorf("EffectiveTaskTimeout = %s, want %s", got, tc.want)
			}
		})
	}
}
