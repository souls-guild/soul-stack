package render

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Rescue routing under linear dispatch (docs/scenario/rescues.md, ADR-0091
// fork 1 and amendment NIM-993). A host whose task failed still receives every
// `onfail:` task whose source failed on it, at that task's step — held for it, dropped
// under `on_failure: host`, or after a failure ended the run.
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
	// step it failed at.
	Failed map[string]map[string]int
	// HandledAt — by host, each failed register a rescue unit handled there (ADR-0091
	// amendment NIM-993, rule 3), with the step whose barrier handled it. nil — no
	// failure handled.
	HandledAt map[string]map[string]int
	// FailedUnits — by host, each rescue unit ([RenderedTask.RescueUnit]) a task of
	// which failed there, with the step whose barrier saw it fail. The unit can no
	// longer succeed there, so a cross-step unit's routing ([Pipeline.routeUnit]) sends
	// that host no later task of it (rule 1). Any other unit lies in one step
	// ([Pipeline.routeRescue]) and has no later task. nil — no unit failed.
	FailedUnits map[string]map[string]int
}

// unitFailed reports whether a task of the rescue unit keyed key failed on host sid
// before step. A failure at or after step is not read, so a step renders the same on
// every pass.
func (r *Rescue) unitFailed(sid, key string, step int) bool {
	if r == nil || key == "" {
		return false
	}
	at, failed := r.FailedUnits[sid][key]
	return failed && at < step
}

// fires reports whether an `onfail:` task of step naming names fires on host sid: one
// of them failed there at an earlier step and was not handled at an earlier step. A
// source failing after its rescue's step is not read, and one handled at or after it
// still fires there, so a step renders the same on every pass. A nil Rescue fires
// nothing.
func (r *Rescue) fires(sid string, names []string, step int) bool {
	if r == nil {
		return false
	}
	failed, handled := r.Failed[sid], r.HandledAt[sid]
	for _, name := range names {
		at, ok := failed[name]
		if !ok || at >= step {
			continue
		}
		if by, ok := handled[name]; !ok || by >= step {
			return true
		}
	}
	return false
}

// FiresAnywhere reports whether an `onfail:` task of step naming one of names fires on
// some host.
func (r *Rescue) FiresAnywhere(names []string, step int) bool {
	if r == nil {
		return false
	}
	for sid := range r.Failed {
		if r.fires(sid, names, step) {
			return true
		}
	}
	return false
}

// rescueRouting reports whether this pass routes rescues: a scenario pass of a
// linear run that holds or has dropped a host, or has ended.
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

// routeRescue is where rescue task goes at step once failures are known, for every
// rescue [RenderInput.crossStepUnit] rejects: a task of a unit with a source in its own
// message, a sibling before it in the group the unit sits in (ADR-0091 amendment
// NIM-993, rule 1). in is the view its `where:` reads — the live hosts it may reach and
// its share of `soulprint.hosts`; cover the run's hosts, live, held and dropped, its
// enclosing blocks' `on:` selects; notLive the hosts the run holds or dropped.
//
// While the run goes on, a live host is selected by `on:` and `where:` as always, the
// cross-step requisite gate dropping the rescue where it does not fire; a held or
// dropped host in cover joins where it fires, its `where:` not evaluated (user,
// 2026-10-08) — it would read registers the host stopped producing. Past the end every
// host of cover is selected that way, so a host moving from live to held or dropped
// changes nothing. A `run_once:` rescue goes to the first host by SID it fires on. Such
// a unit lies wholly in its group's one step, so no task of it failed at an earlier
// step ([Rescue.FailedUnits]) to keep a host from it. notReached is, while the run goes
// on, what it is for any task ([coveredDropped]): the held and dropped hosts of in its
// `on:` selects, less those it reaches; nil past the end.
func (p *Pipeline) routeRescue(in RenderInput, task config.Task, step int, cover []*topology.HostFacts, notLive map[string]bool) (targeted, notReached []*topology.HostFacts, err error) {
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
		switch {
		case !past && !notLive[h.SID]:
		case fires(h):
			targeted = append(targeted, h)
		}
	}
	sortBySID(targeted)
	if !past && !task.RunOnce {
		listed := sidSet(slices.Concat(targeted, notReached))
		for _, h := range filterByCovens(mergeHosts(in.Dropped, in.HeldHosts), covens) {
			if !listed[h.SID] {
				notReached = append(notReached, h)
			}
		}
	}
	sortBySID(notReached)
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
	if !in.rescueRouting() && !in.unitRouting() {
		return in, nil
	}
	cover, err := p.onCover(in, block, parent)
	in.rescueCover = cover
	return in, err
}

// Cross-step rescue units (ADR-0091 amendment NIM-993, rule 1). A unit with no source
// before it in its own message ([config.CrossStepUnits]) never fires on a live host: a
// source at an earlier step fires it only on a held or a judged host, neither of them
// live, one at a later step cannot have failed when it runs, and a sibling after it in
// its own message has not run yet when it does. On every linear pass while the
// run goes on it is settled SKIPPED on each live host its `on:` chain selects, none of
// its `where:`, `params:` or `when:` evaluated there — they may read an output of the
// unit's own tasks, which ran nowhere. The exception is an applier unit whose static
// `when:` is false: it leaves nothing on a live host, as a static-false applier that is
// no unit does. Only what fixes the plan's layout is evaluated: a `loop:`'s items and
// an `apply:`'s destiny, over its host-invariant inputs, and the static `when:` of
// either. A scenario one counts as false where it cannot be decided without a host
// (user, 2026-10-20); in a destiny such a `when:` is refused by name, laid out as
// routed ([ADR-0082] amendment 2026-10-15, point 7; user, 2026-10-27).

// unitRouting reports whether this pass routes cross-step rescue units: a linear
// scenario pass whose plan holds one.
func (in RenderInput) unitRouting() bool {
	return in.crossUnits != nil && in.keeperFlow()
}

// crossStepUnit reports whether task, at the pass's position, belongs to a cross-step
// rescue unit: the outermost unit holding it is one, which decides every source it
// holds, a nested unit's included. A child of a block that is one is read the same way:
// it carries the block's `onfail:` merged into its own, so it is a rescue, and the
// outermost unit holding it is the block's. No keeper-side task sits in a block
// (block_on_keeper_invalid), so none of them is left out as no rescue.
func (in RenderInput) crossStepUnit(task config.Task) bool {
	return in.unitRouting() && isRescue(task) && in.crossUnits[in.units.keyAt(in.path)]
}

// unitStaticWhen is [Pipeline.emitStaticWhenSkip] for a cross-step unit's task of step,
// run before the DSL guard, whose verdict on it is refusal. Routed to some host, its
// static `when:` is decided over the hosts it renders for. Routed to none past the end
// of the run, it is one placeholder, its `when:` not read, whose plan names notReached
// — the hosts [Pipeline.routeUnit] left out — so each is recorded SKIPPED. Routed to
// none while the run goes on, it is decided only where it fixes the layout — the
// places of a `loop:`, the destiny of an `apply:` — and host-free, refusal standing
// unless that gates it off ([Pipeline.layoutStaticWhen]); that gives the plan indices
// a routed pass gives only for a `when:` that reads neither a `vars:` entry reading a
// per-host name nor `incarnation.host_count`. A task it skips is settled SKIPPED,
// unrendered, on skip — save an applier, which leaves on a live host what a
// static-false applier that is no unit leaves there: nothing.
func (p *Pipeline) unitStaticWhen(ctx context.Context, in RenderInput, task config.Task, step int, routed, skip, notReached []*topology.HostFacts, refusal error, tasks *[]*RenderedTask, plans *[]DispatchPlan, idx *int) (bool, error) {
	if task.Block != nil {
		return false, nil
	}
	start := len(*tasks)
	var skipped bool
	var err error
	// Within a run this split is the same on every pass at or after step, what is routed
	// here being fixed by the barriers before it: a laid-out and a routed render of the
	// unit, whose plan indices may differ, meet only across runs or against a pass before
	// step, which dispatches none of it.
	if len(routed) == 0 {
		if in.pastEnd(step) {
			*tasks = append(*tasks, futurePlaceholder(task, *idx, step))
			*plans = append(*plans, DispatchPlan{TaskIndex: *idx})
			stampNotReached((*plans)[len(*plans)-1:], notReached)
			*idx++
			return true, nil
		}
		if task.Loop == nil && task.Apply == nil {
			return false, nil
		}
		skipped, err = p.layoutStaticWhen(ctx, in, task, routed, refusal, tasks, plans, idx)
	} else {
		skipped, err = p.emitStaticWhenSkip(ctx, in, task, routed, tasks, plans, idx)
	}
	if skipped && task.Apply == nil {
		withSkipTargets((*tasks)[start:], (*plans)[start:], skip)
	}
	return skipped, err
}

// layoutGuard is what refusal, a DSL guard's verdict on a task a layout-only pass lays
// out unrendered, comes to: it stands unless the task's static `when:`, decided
// host-free, is false — an inactive branch's DSL does not block the active one
// (ADR-012(d)), laid out or rendered. A `when:` that cannot be decided there keeps it —
// one reading a per-host `vars:` entry, which a scenario layout with no refusal reads as
// false ([Pipeline.layoutStaticWhen]) — and so does one reading `incarnation.host_count`:
// decided host-free it reads 0, and routed the count of the hosts it is routed to.
//
// With no refusal it is the refusal a routed render meets first: in a destiny pass a
// static `when:` reading a per-host name is refused by name, over the task's whole view
// before any entry is left out ([Pipeline.guardStaticWhenHost]; user, 2026-10-27). So a
// layout counts such a `when:` false only on a scenario pass.
func (p *Pipeline) layoutGuard(in RenderInput, task config.Task, refusal error) error {
	if !isStaticWhen(task.When) {
		return refusal
	}
	if refusal == nil {
		return p.guardStaticWhenHost(in, task)
	}
	if p.whenReadsHostCount(task) {
		return refusal
	}
	in, free, undecidable := p.hostFreeWhen(in, task)
	if undecidable {
		return refusal
	}
	if skip, err := p.staticWhenSkips(in, free, []*topology.HostFacts{{}}, 0, nil); err != nil || skip == nil {
		return refusal
	}
	return nil
}

// layoutStaticWhen is [Pipeline.emitStaticWhenSkip] for a task a layout-only pass lays
// out, its static `when:` decided host-free ([Pipeline.hostFreeWhen]). refusal, the DSL
// guard's verdict on task, comes to what [Pipeline.layoutGuard] makes of it before any
// placeholder is laid: a routed pass may find true a `when:` the layout cannot decide,
// or one reading `incarnation.host_count`, 0 here. With no refusal, one that cannot be
// decided there counts as false (user, 2026-10-20): its value is not read, and it fails
// the render only where its context cannot be built or it does not compile. That is a
// scenario unit's alone — a scenario `loop:` or an `apply:` unit's own `when:`: in a
// destiny pass layoutGuard has refused such a `when:` by name already.
//
// Every [cel.ErrEval] of such a `when:` is taken for the entry left out, so a misspelt
// var beside it, as in `vars.at == 'eu' || vars.att == 'x'`, counts as false here and
// fails only routed, on a host where at is not 'eu'. Partial evaluation with the entry
// unknown would not catch that one either — CEL's `||` and `&&` let an unknown absorb
// an error — and shared/cel exports no entry for it.
func (p *Pipeline) layoutStaticWhen(ctx context.Context, in RenderInput, task config.Task, hosts []*topology.HostFacts, refusal error, tasks *[]*RenderedTask, plans *[]DispatchPlan, idx *int) (bool, error) {
	if err := p.layoutGuard(in, task, refusal); err != nil {
		return false, err
	}
	in, free, undecidable := p.hostFreeWhen(in, task)
	if !undecidable || task.Block != nil || !isStaticWhen(task.When) {
		return p.emitStaticWhenSkip(ctx, in, free, hosts, tasks, plans, idx)
	}
	renderHosts := hosts
	if len(renderHosts) == 0 {
		renderHosts = []*topology.HostFacts{{}}
	}
	skip, err := p.staticWhenContext(in, free, renderHosts, len(hosts), nil)
	if err != nil {
		return false, err
	}
	var evalErr *cel.ErrEval
	if _, err := evalStaticWhen(task.When, skip); err != nil && !errors.As(err, &evalErr) {
		return false, &staticWhenEvalError{task: task.Name, when: task.When, err: err}
	}
	if err := p.emitStaticSkip(in, free, skip, tasks, plans, idx); err != nil {
		return false, err
	}
	return true, nil
}

var hostCountRoots = []string{"incarnation", varsRootName}

// whenReadsHostCount reports whether task's `when:` reads `incarnation.host_count`,
// directly or through its `vars:`. A root read whole counts as reading it.
func (p *Pipeline) whenReadsHostCount(task config.Task) bool {
	counting := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name, val := range task.Vars {
			if !counting[name] && p.valueReadsHostCount(val, counting) {
				counting[name], changed = true, true
			}
		}
	}
	return readsHostCount(p.cel.PredicateReads(task.When, hostCountRoots), counting)
}

func (p *Pipeline) valueReadsHostCount(val any, counting map[string]bool) bool {
	switch v := val.(type) {
	case string:
		return readsHostCount(p.cel.InterpolationReads(v, hostCountRoots), counting)
	case map[string]any:
		for _, sub := range v {
			if p.valueReadsHostCount(sub, counting) {
				return true
			}
		}
	case []any:
		for _, sub := range v {
			if p.valueReadsHostCount(sub, counting) {
				return true
			}
		}
	}
	return false
}

// readsHostCount reports whether reads reach `incarnation.host_count`, or a var of
// counting — the names that read it.
func readsHostCount(reads map[string]cel.RootReads, counting map[string]bool) bool {
	if inc := reads["incarnation"]; inc.Whole || inc.Fields["host_count"] {
		return true
	}
	return readsAny(reads[varsRootName], counting)
}

// hostFreeWhen is task with only the `vars:` entries that read no per-host name of in
// — `soulprint.self`, a register the host keeps, an `apply: input:` rendered per host,
// or another entry that does — for its `when:` decided over no host, and in with those
// names left out ([RenderInput.leftOut]). The entries are the task's whole view's: under
// late binding a scenario or `vars.yml` definition reading the task's override is one
// too (ADR-0082 amendment 2026-10-15, point 16). undecidable — the `when:` reads an
// entry left out, and so no host-free evaluation decides it.
func (p *Pipeline) hostFreeWhen(in RenderInput, task config.Task) (_ RenderInput, _ config.Task, undecidable bool) {
	inputs := make(map[string]bool, len(in.hostVariantInputs))
	for _, name := range in.hostVariantInputs {
		inputs[name] = true
	}
	variant := hostVariantVarNames(p.cel, p.taskVarLayers(in, task), keeperRegisterNames(in.Scenario), inputs)
	undecidable = readsAny(p.cel.PredicateReads(task.When, []string{varsRootName})[varsRootName], variant)
	task.Vars = withoutNames(task.Vars, slices.Collect(maps.Keys(variant)))
	in.leftOut = variant
	return in, task, undecidable
}

// routeUnit is where cross-step unit task goes at step, out of cover — the run's
// hosts its enclosing constructs' `on:` selects — narrowed by its own `on:`. routed
// are the held and dropped hosts a source fires on, which render it; skip the live
// hosts, settled SKIPPED unrendered. Past the end of the run every covered host a
// source fires on is routed and no live host gets anything, as for any rescue. A
// `run_once:` task goes to the first routed host by SID; a block's `run_once:` is not
// read (rule 1). A host on which an earlier task of the unit failed is not routed: the
// unit can no longer succeed there, and nothing of the task is evaluated for it.
// notReached is those hosts, the hosts of pool — the dropped hosts in reach, nil under
// a `run_once:` block — and the held hosts of cover that it does not reach. No
// `where:` is evaluated, and no host is held by [holdTargets].
func (p *Pipeline) routeUnit(in RenderInput, task config.Task, step int, cover, pool []*topology.HostFacts) (routed, skip, notReached []*topology.HostFacts, err error) {
	covens, err := resolveOn(p.cel, in, task.On)
	if err != nil {
		return nil, nil, nil, err
	}
	past := in.pastEnd(step)
	live := sidSet(in.Hosts)
	held := sidSet(in.HeldHosts)
	key := in.units.keyAt(in.path)
	covered := filterByCovens(cover, covens)
	for _, h := range covered {
		switch {
		case !past && live[h.SID]:
			skip = append(skip, h)
		case in.Rescue.unitFailed(h.SID, key, step):
			notReached = append(notReached, h)
		case in.Rescue.fires(h.SID, task.OnFail, step):
			routed = append(routed, h)
		}
	}
	sortBySID(routed)
	sortBySID(skip)
	sortBySID(notReached)
	runOnce := task.RunOnce && task.Block == nil
	if runOnce && len(routed) > 1 {
		routed = routed[:1]
	}
	if past || runOnce {
		return routed, skip, notReached, nil
	}
	listed := sidSet(slices.Concat(routed, notReached))
	for _, h := range filterByCovens(pool, covens) {
		if !listed[h.SID] {
			notReached = append(notReached, h)
		}
	}
	for _, h := range covered {
		if held[h.SID] && !listed[h.SID] {
			notReached = append(notReached, h)
		}
	}
	sortBySID(notReached)
	return routed, skip, notReached, nil
}

// unitSkipPlaceholder stands in for a cross-step unit's task that nothing was routed
// to: its plan position, register and requisites, nothing rendered.
func unitSkipPlaceholder(task config.Task, idx, step int, modules config.ModuleManifestResolver) *RenderedTask {
	rt := futurePlaceholder(task, idx, step)
	rt.onChangesNames = task.OnChanges
	rt.onFailNames = task.OnFail
	applyConcurrency(rt, task)
	if task.Module != nil {
		rt.SecretOutput = config.SecretOutputFields(rt.Module, modules)
	}
	return rt
}

// withSkipTargets adds skip to the targets of every plan of a unit's fan-out, each
// task to be settled SKIPPED there ([Pipeline.settleFlow]).
func withSkipTargets(tasks []*RenderedTask, plans []DispatchPlan, skip []*topology.HostFacts) {
	if len(skip) == 0 {
		return
	}
	sids := sidSet(skip)
	byIndex := make(map[int]*RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	for i := range plans {
		targets := slices.Clone(plans[i].TargetSIDs)
		for _, h := range skip {
			if !slices.Contains(targets, h.SID) {
				targets = append(targets, h.SID)
			}
		}
		slices.Sort(targets)
		plans[i].TargetSIDs = targets
		byIndex[plans[i].TaskIndex].skipSIDs = sids
	}
}

// mergeHosts is a and b without repeats, sorted by SID.
func mergeHosts(a, b []*topology.HostFacts) []*topology.HostFacts {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]*topology.HostFacts, 0, len(a)+len(b))
	for _, h := range append(slices.Clone(a), b...) {
		if !seen[h.SID] {
			seen[h.SID] = true
			out = append(out, h)
		}
	}
	sortBySID(out)
	return out
}
