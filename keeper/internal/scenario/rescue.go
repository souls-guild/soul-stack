package scenario

import (
	"slices"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Rescue routing of a linear run (docs/scenario/rescues.md, ADR-0091
// fork 1 and amendment NIM-993): what the run goroutine tells render of its failures
// before each step ([failureView.rescue]), so that a held or dropped host still
// receives the `onfail:` tasks whose source failed on it and a run that a failure
// ended renders only those.

// rescueIndices are the rescues among a step's tasks, as rendered: the tasks whose
// `onfail:` names a task of an earlier step ([crossPassageGate.splitRequisite]), each
// true, and the terminal of an applier one, false, which carries its register. A nil
// gate — the first step, or no audit reader — has no earlier step to read.
func rescueIndices(tasks []*render.RenderedTask, gate *crossPassageGate, passage int) map[int]bool {
	out := map[int]bool{}
	if gate == nil {
		return out
	}
	for _, t := range tasks {
		if cross, _ := gate.splitRequisite(t.OnFailIdx, passage); len(cross) > 0 {
			out[t.Index] = true
		}
	}
	for _, t := range tasks {
		if slices.ContainsFunc(t.AggregateOf, func(child int) bool { return out[child] }) {
			out[t.Index] = false
		}
	}
	return out
}

// rescueFires reports whether the rescue at plan index idx fires on host sid: one of
// its sources at an earlier step failed there, as the requisite gate reads it.
func rescueFires(gate *crossPassageGate, passage int, tasks []*render.RenderedTask) func(sid string, idx int) bool {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	return func(sid string, idx int) bool {
		t := byIndex[idx]
		if gate == nil || t == nil {
			return false
		}
		cross, _ := gate.splitRequisite(t.OnFailIdx, passage)
		return gate.anyKey(gate.failed, sid, cross)
	}
}

// rescueWave is a wave after a failure ended the run at its step: the hosts a rescue
// fires on, each sent its rescues alone — a `when: false` copy among them, whose
// register its siblings read — as a step past the end is rendered. A host the Keeper
// failed before sending stays when what failed is a rescue that fires there.
func rescueWave(wave []string, perHost map[string][]*render.RenderedTask, failedEarly map[string]bool, settled map[string]*settledHost, rescues map[int]bool, fires func(sid string, idx int) bool) []string {
	var out []string
	for _, sid := range wave {
		if failedEarly[sid] {
			if recs := settled[sid].records; len(recs) > 0 && rescues[recs[0].task.Index] && fires(sid, recs[0].task.Index) {
				out = append(out, sid)
			}
			continue
		}
		var keep []*render.RenderedTask
		firing := false
		for _, t := range perHost[sid] {
			own, rescue := rescues[t.Index]
			if !rescue {
				continue
			}
			keep = append(keep, t)
			firing = firing || (own && fires(sid, t.Index))
		}
		if firing {
			perHost[sid] = keep
			out = append(out, sid)
		}
	}
	return out
}

// targetedTasks is tasks without the ones their plans send to no host.
func targetedTasks(tasks []*render.RenderedTask, plans []render.DispatchPlan) []*render.RenderedTask {
	sent := make(map[int]bool, len(plans))
	for _, pl := range plans {
		if len(pl.TargetSIDs) > 0 {
			sent[pl.TaskIndex] = true
		}
	}
	out := make([]*render.RenderedTask, 0, len(tasks))
	for _, t := range tasks {
		if sent[t.Index] {
			out = append(out, t)
		}
	}
	return out
}

// rescueAhead reports whether a step after step holds a rescue.
func rescueAhead(rescueSteps map[int][]string, step int) bool {
	for s := range rescueSteps {
		if s > step {
			return true
		}
	}
	return false
}

// needsRescueHold reports whether a step of a linear run goes through the work-queue
// only while every live instance holds rescues — a stopgap ahead of the per-step gate
// of ADR-027 amend (v), until the work-queue recipe carries the failure state and
// claimants announce `rescue_hold`. Every step of a plan holding a cross-step unit
// ([hasCrossStepUnit]) needs it, and in a plan holding a rescue of an earlier step's
// source ([config.RescueSteps]) every step from the run's first failure on: a unit
// with a source in its own message is still a unit of its earlier source and can hold
// a host (ADR-0091 amendment NIM-993, rule 7), and a claim renders over the recipe's
// one roster, which holds no held host. The run goroutine renders such a step with
// each host's standing as each step saw it under the same condition.
func needsRescueHold(crossStepUnit bool, rescueSteps map[int][]string, fails *runFailures) bool {
	return crossStepUnit || len(rescueSteps) > 0 && fails.failed()
}

// hasCrossStepUnit reports whether a linear plan holds a cross-step rescue unit, one
// with no source before it in its own message, told as render tells one
// ([config.CrossStepUnits]). Every step of such a plan, the ones before the unit
// included, goes through the work-queue only while every live instance holds rescues
// ([needsRescueHold]). A claim renders the whole plan, so an instance from before
// NIM-993's rule-1 routing claiming any of its steps evaluates the unit's static
// `when:` and `vars:` over the live hosts, which the run goroutine does not, and fails
// the row where they read the unit's own register. A unit with a source before it in
// its own message is no such unit: before the run's first failure a plan whose units are
// all of that kind queues as before. steps is [config.LinearPassage]'s.
func hasCrossStepUnit(tasks []config.Task, steps []config.StepNode) bool {
	return len(config.CrossStepUnits(tasks, steps)) > 0
}
