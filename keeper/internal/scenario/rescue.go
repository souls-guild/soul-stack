package scenario

import (
	"context"
	"fmt"
	"slices"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// Rescue routing of a linear run (docs/scenario/settings.md §4, draft ADR NIM-907
// fork 1): what the run goroutine tells render of its failures before each step, so
// that a dropped host still receives the `onfail:` tasks whose source failed on it and
// a run that a failure ended renders only those.

// rescueFor is a step's view of the run's failures: nil while no host has been dropped
// and the run goes on — the render is then exactly what it was before rescue routing.
// tasks are the last pass's: every task that has run is resolved there, with its plan
// index, register and step. Without an audit reader the run refused every cross-step
// requisite before its first step, so there is no rescue to route.
func (r *Runner) rescueFor(ctx context.Context, spec RunSpec, fails *runFailures, tasks []*render.RenderedTask) (*render.Rescue, error) {
	if fails == nil || (len(fails.dropped) == 0 && !fails.ended) || r.deps.AuditReader == nil {
		return nil, nil
	}
	keys, err := r.deps.AuditReader.SelectFailedTaskKeys(ctx, spec.ApplyID)
	if err != nil {
		return nil, fmt.Errorf("scenario: rescue routing: failed facts: %w", err)
	}
	return &render.Rescue{Ended: fails.ended, EndedAt: fails.endedAt, Failed: failedRegisters(keys, tasks)}, nil
}

// failedRegisters folds the run's FAILED/TIMED_OUT task keys into the registers that
// failed, by host, each with the first step it failed at: a task's own register, and
// an applier's for any of its children — the fold crosspassage.go reads an applier
// source by. A keeper-side task's stays under render.KeeperTargetSID.
func failedRegisters(keys map[auditpg.ChangedTaskKey]struct{}, tasks []*render.RenderedTask) map[string]map[string]int {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	appliers := map[int][]*render.RenderedTask{}
	for _, t := range tasks {
		byIndex[t.Index] = t
		for _, child := range t.AggregateOf {
			appliers[child] = append(appliers[child], t)
		}
	}
	out := map[string]map[string]int{}
	note := func(sid string, t *render.RenderedTask) {
		if t.Register == "" {
			return
		}
		if out[sid] == nil {
			out[sid] = map[string]int{}
		}
		if at, ok := out[sid][t.Register]; !ok || t.Passage < at {
			out[sid][t.Register] = t.Passage
		}
	}
	for k := range keys {
		t := byIndex[k.PlanIndex]
		if t == nil {
			continue
		}
		note(k.SID, t)
		for _, applier := range appliers[k.PlanIndex] {
			note(k.SID, applier)
		}
	}
	return out
}

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
