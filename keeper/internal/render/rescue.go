package render

import (
	"slices"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Rescue routing under linear dispatch (docs/scenario/settings.md §4, ADR-0091
// fork 1). A host whose task failed still receives every `onfail:` task
// whose source failed on it, at that task's step — dropped under `on_failure: host`,
// or after a failure ended the run.
//
// Past the end of a run, every host's registers stop where it ended, so nothing there
// evaluates a `where:`: a rescue goes to the run's hosts its `on:` selects where it
// fires, a block holding one narrows by its `on:` alone, and every other task stays a
// placeholder — rendered, it could fail the run on a field the failed source never
// produced before the rescue is reached. Whether a task of a step past the end
// renders is fixed once the run has ended, so the plan indices of the steps that ran
// do not move between passes.

// Rescue is what a linear run knows of its failures when it renders a step.
type Rescue struct {
	// Ended — a failure ended the run at the barrier of step EndedAt.
	Ended   bool
	EndedAt int
	// Failed — by host, each register of a task that failed there, with the first
	// step it failed at; under [KeeperTargetSID], a keeper-side task's, failed for
	// every host.
	Failed map[string]map[string]int
}

// fires reports whether an `onfail:` task of step naming names fires on host sid: one
// of them failed there at an earlier step. A source failing after its rescue's step
// is not read, so a step renders the same on every pass.
func (r *Rescue) fires(sid string, names []string, step int) bool {
	for _, failed := range []map[string]int{r.Failed[sid], r.Failed[KeeperTargetSID]} {
		for _, name := range names {
			if at, ok := failed[name]; ok && at < step {
				return true
			}
		}
	}
	return false
}

// FiresAnywhere reports whether an `onfail:` task of step naming one of names fires on
// some host.
func (r *Rescue) FiresAnywhere(names []string, step int) bool {
	for sid := range r.Failed {
		if r.fires(sid, names, step) {
			return true
		}
	}
	return false
}

// rescueRouting reports whether this pass routes rescues: a scenario pass of a
// linear run that has dropped a host or ended.
func (in RenderInput) rescueRouting() bool {
	return in.Rescue != nil && in.keeperFlow()
}

// isRescue reports whether task is an `onfail:` task. A keeper-side one is not: the
// Keeper evaluates no requisite of its own tasks (docs/keeper/modules.md).
func isRescue(task config.Task) bool {
	return len(task.OnFail) > 0 && !IsKeeperTask(task)
}

// pastEnd reports whether step comes after the barrier where a failure ended the run.
func (in RenderInput) pastEnd(step int) bool {
	return in.rescueRouting() && in.Rescue.Ended && step > in.Rescue.EndedAt
}

// routeRescue is where rescue task goes at step once failures are known. in is the
// view its `where:` reads — the live hosts it may reach and its share of
// `soulprint.hosts`; cover the run's hosts, live and dropped, its enclosing blocks'
// `on:` selects; dropped the hosts the run dropped.
//
// While the run goes on, a live host is selected by `on:` and `where:` as always, the
// cross-step requisite gate dropping the rescue where it does not fire; a dropped host
// in cover joins where it fires, its `where:` not evaluated (user, 2026-10-08) — it
// would read registers the host stopped producing. Past the end every host of cover is
// selected that way, so a host moving from live to dropped changes nothing. A
// `run_once:` rescue goes to the first host by SID it fires on. notReached is what it
// is for any task while the run goes on ([coveredDropped]): the dropped hosts of in
// its `on:` selects, less those it reaches.
func (p *Pipeline) routeRescue(in RenderInput, task config.Task, step int, cover []*topology.HostFacts, dropped map[string]bool) (targeted, notReached []*topology.HostFacts, err error) {
	past := in.pastEnd(step)
	covens, err := resolveOn(p.cel, in, task.On)
	if err != nil {
		return nil, nil, err
	}
	fires := func(h *topology.HostFacts) bool { return in.Rescue.fires(h.SID, task.OnFail, step) }
	if !past {
		live, err := p.resolveTargets(in, task)
		if err != nil {
			return nil, nil, err
		}
		// resolveTargets can hand back in.Hosts itself, the run's roster.
		targeted = slices.Clone(live)
	}
	for _, h := range filterByCovens(cover, covens) {
		if (past || dropped[h.SID]) && fires(h) {
			targeted = append(targeted, h)
		}
	}
	sortBySID(targeted)
	if !past && !task.RunOnce {
		reached := make(map[string]bool, len(targeted))
		for _, h := range targeted {
			reached[h.SID] = true
		}
		for _, h := range filterByCovens(in.Dropped, covens) {
			if !reached[h.SID] {
				notReached = append(notReached, h)
			}
		}
	}
	if task.RunOnce {
		if len(targeted) > 1 {
			pick := targeted[0]
			for _, h := range targeted {
				if fires(h) {
					pick = h
					break
				}
			}
			targeted = []*topology.HostFacts{pick}
		}
	}
	return targeted, notReached, nil
}

// onCover is the hosts among hosts that task's `on:` selects, its `where:` and
// `run_once:` aside: what a block whose first step comes after the end holds, and
// each block's link of the rescue cover.
func (p *Pipeline) onCover(in RenderInput, task config.Task, hosts []*topology.HostFacts) ([]*topology.HostFacts, error) {
	covens, err := resolveOn(p.cel, in, task.On)
	if err != nil {
		return nil, err
	}
	return filterByCovens(hosts, covens), nil
}

// withRescueCover is in for rendering block's children, its rescue cover narrowed by
// the block's `on:` out of parent.
func (p *Pipeline) withRescueCover(in RenderInput, block config.Task, parent []*topology.HostFacts) (RenderInput, error) {
	if !in.rescueRouting() {
		return in, nil
	}
	cover, err := p.onCover(in, block, parent)
	in.rescueCover = cover
	return in, err
}
