package config

import "time"

// StepSettings is the merged `settings:` of each step's barrier owner under linear
// dispatch (settings.md §2), indexed by step exactly as [LinearPassage] numbers
// them: the defaults, then the scenario level, then the owner's own chain — a
// default block's child carries its enclosing blocks' levels ([LinearBlockChild]).
// It is what a failure tied to no task is judged by, such as a lost agent stream
// (`soul_timeout`).
func StepSettings(tasks []Task, scenario *ScenarioSettings) []ScenarioSettings {
	base := DefaultScenarioSettings()
	if scenario != nil {
		base = MergeSettings(base, *scenario)
	}
	var out []ScenarioSettings
	for i := range tasks {
		out = appendStepSettings(out, tasks[i], scenario, base)
	}
	if len(out) == 0 {
		out = append(out, base)
	}
	return out
}

func appendStepSettings(out []ScenarioSettings, t Task, scenario *ScenarioSettings, base ScenarioSettings) []ScenarioSettings {
	if t.Block == nil || BlockRollsAsGroup(scenario, t) || len(t.Block.Block) == 0 {
		return append(out, MergeSettings(base, t.OwnSettings()))
	}
	for j := range t.Block.Block {
		out = appendStepSettings(out, LinearBlockChild(t, t.Block.Block[j]), scenario, base)
	}
	return out
}

// EffectiveTaskTimeout is the `task_timeout` bounding t — 0 for no limit: t's own
// level, its `timeout:` folded in ([Task.OwnSettings]), over the scenario level.
// t must already carry its enclosing blocks' settings ([InheritSettings]).
func EffectiveTaskTimeout(scenario *ScenarioSettings, t Task) time.Duration {
	eff := DefaultScenarioSettings()
	if scenario != nil {
		eff = MergeSettings(eff, *scenario)
	}
	eff = MergeSettings(eff, t.OwnSettings())
	return *eff.TaskTimeout
}
