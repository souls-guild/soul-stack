package render

import (
	"slices"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// What render does for a linear run's failure policy (docs/scenario/settings.md §4,
// NIM-948): every task carries the `on_failure` its failures are judged by, a host
// the run dropped is targeted by nothing yet still listed in `soulprint.hosts`, and a
// task records which dropped hosts it would have reached.

// stampOnFailure sets task's effective `on_failure` on every RenderedTask added for it
// (tasks[from:]) that carries none yet. A block's child stamps its own range first and
// its block the rest — the [stampTransport] rule — so a grouped child that writes its
// own radius keeps it. A destiny's tasks take their applier's: a destiny has no
// `settings:` (settings.md §3, rule 6). Linear dispatch only.
func stampOnFailure(in RenderInput, tasks []*RenderedTask, from int, task config.Task) {
	if in.Steps == nil || in.destinyIsolated || from >= len(tasks) {
		return
	}
	var scenario *config.ScenarioSettings
	if in.Scenario != nil {
		scenario = in.Scenario.Settings
	}
	radius := config.EffectiveOnFailure(scenario, task)
	for _, rt := range tasks[from:] {
		if rt.OnFailure == "" {
			rt.OnFailure = radius
		}
	}
}

// coveredDropped is the hosts among roster — dropped or held hosts, narrowed by the
// enclosing blocks — that task's `on:` selects. Its `where:` is not evaluated: it would
// read the registers of a host that left the run, which stop at the step it failed. A
// `run_once:` task reaches one live host, so it covers none, and the children of a
// `run_once:` block none either. A held host gets there what a dropped one gets: for
// each task it does not receive, "the Keeper writes the audit SKIPPED it writes for a
// dropped host today" (ADR-0091 amendment NIM-993, rule 3) — so under `run_once:` no
// record, and no register is synthesized for it.
func coveredDropped(engine *cel.Engine, in RenderInput, task config.Task, roster []*topology.HostFacts) ([]*topology.HostFacts, error) {
	if len(roster) == 0 || task.RunOnce || in.destinyIsolated {
		return nil, nil
	}
	covens, err := resolveOn(engine, in, task.On)
	if err != nil {
		return nil, err
	}
	return filterByCovens(roster, covens), nil
}

// stampNotReached records on every plan of a task's fan-out the dropped hosts it would
// have reached.
func stampNotReached(plans []DispatchPlan, covered []*topology.HostFacts) {
	if len(covered) == 0 {
		return
	}
	sids := sidsOf(covered)
	for i := range plans {
		if !plans[i].Keeper {
			plans[i].NotReached = sids
		}
	}
}

// runHosts is every host of the run for `soulprint.hosts`: the roster and the hosts
// the run holds or dropped, in SID order, so a host keeps its position and
// `soulprint.hosts[0]` names the same host before and after it leaves the roster.
func runHosts(in RenderInput) []*topology.HostFacts {
	if len(in.Dropped) == 0 && len(in.HeldHosts) == 0 {
		return in.Hosts
	}
	out := make([]*topology.HostFacts, 0, len(in.Hosts)+len(in.HeldHosts)+len(in.Dropped))
	out = append(out, in.Hosts...)
	out = append(out, in.HeldHosts...)
	out = append(out, in.Dropped...)
	sortBySID(out)
	return out
}

// droppedSIDs is the set of in.Dropped.
func droppedSIDs(in RenderInput) map[string]bool {
	return sidSet(in.Dropped)
}

// notLiveSIDs is the set of the hosts in.Dropped and in.HeldHosts hold: no ordinary
// task targets them, and a rescue reaches them where it fires.
func notLiveSIDs(in RenderInput) map[string]bool {
	return sidSet(slices.Concat(in.Dropped, in.HeldHosts))
}

// coveredAway is [coveredDropped] over dropped and held, the two covers of an enclosing
// scope, merged: the hosts a task of it would have reached that it may not reach.
func coveredAway(engine *cel.Engine, in RenderInput, task config.Task, dropped, held []*topology.HostFacts) (dropCover, heldCover, both []*topology.HostFacts, err error) {
	if dropCover, err = coveredDropped(engine, in, task, dropped); err != nil {
		return nil, nil, nil, err
	}
	if heldCover, err = coveredDropped(engine, in, task, held); err != nil {
		return nil, nil, nil, err
	}
	return dropCover, heldCover, mergeHosts(dropCover, heldCover), nil
}
