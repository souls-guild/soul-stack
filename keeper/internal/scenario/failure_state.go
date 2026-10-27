package scenario

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// A linear run's failure state (ADR-0091 amendment NIM-993, "What the run goroutine
// keeps"): each host's failed sources with the state its barriers decided for them,
// and each host's standing, as a step saw them ([runFailures.view]). A barrier
// replays what each host reported through the host's [config.RescueRule], the rule
// the Soul applies inside one message ([runFailures.barrier]): a failure a rescue
// unit ahead may handle is held (rule 2), a host whose every failure a unit handled
// returns (rule 3), and a failure no unit handled is judged at the barrier of its last
// unit (rule 4). The run goroutine's render, rescue routing and requisite gate read
// the states here. A claim's requisite gate reads the audit log's FAILED facts
// ([ClaimRunner.stepGate]); a plan in which a unit can hold a host sends every step
// inline from its first failure on ([needsRescueHold]).

// sourceKey is one failed source on one host: its SID and its first plan index.
type sourceKey struct {
	sid   string
	index int
}

// sourceState is a source's state as the barriers decided it.
type sourceState int

const (
	// sourceHeld — waiting for a rescue unit at a later step (rule 2).
	sourceHeld sourceState = iota + 1
	// sourceHandled — a unit of it succeeded (rule 3).
	sourceHandled
	// sourceJudged — judged at once or at its last unit's barrier (rule 4); final.
	sourceJudged
)

// failedSource is one failed task on one host as the barriers decided it.
type failedSource struct {
	task string
	// register — the task's own register; applierRegister — its applier's, for a task
	// of a destiny applied with `register:`. "" — none.
	register        string
	applierRegister string
	// members — the plan indices it failed at, in plan order: its task's, or each
	// iteration of a `loop:` that failed there, which share one register and are one
	// source (rule 2).
	members []int
	// failedAt — the step it failed at. changedAt — the step whose barrier last
	// decided state; until that barrier the source is held.
	failedAt  int
	state     sourceState
	changedAt int
	// radius — the effective `on_failure` it is judged by. Read when a source held for
	// a rescue unit is judged at its last unit's barrier ([runFailures.sweepHost]):
	// under `scenario` the run ends at that barrier, under `host` the host is dropped
	// there. Its task's own; for a host the Keeper failed before sending, the radius
	// its verdict took; for a failure tied to no task, the barrier owner's at the step
	// it failed at.
	radius string
	// reason — row-level: every source of a row carries the row's first failure
	// summary.
	reason string
	// lastUnit — the step at whose barrier the last rescue unit of it that covers its
	// host is decided ([render.RescueCover.DecidedAt]); nil — it had no unit ahead and
	// was judged where it failed. unitErrs — the errors of its units that failed on
	// the host, in the order they failed.
	lastUnit *int
	unitErrs []string
	// rows — the steps whose `apply_runs` rows hold it: its own, and those of its
	// units that failed.
	rows []int
}

// holdPeriod is one stretch of steps a host was held for ([render.HoldPeriod]).
type holdPeriod struct {
	heldFrom   int
	returnedAt *int
	// missed — register name → the register synthesized for a task the host missed
	// while held ([render.HoldPeriod.Synth]).
	missed map[string]any
}

// hostFailures is one host whose standing changed.
type hostFailures struct {
	// facts — the roster facts it had when it left: while it is held or dropped it
	// stays in `soulprint.hosts` with them, even when a roster read again lists it
	// otherwise or not at all.
	facts *topology.HostFacts
	// periods — every time it was held, in step order: a returned host can be held
	// again (rule 2).
	periods []holdPeriod
	// droppedAt — the first step it is out of; it overrides periods.
	droppedAt *int
}

// at is h as step saw it: a hold or a drop of a later step not come yet, a return
// not come yet still open. changed — its standing had changed by then.
func (h *hostFailures) at(step int) (out hostFailures, changed bool) {
	out.facts = h.facts
	for _, p := range h.periods {
		if p.heldFrom > step {
			break
		}
		p.returnedAt, p.missed = reachedAt(p.returnedAt, step), maps.Clone(p.missed)
		out.periods = append(out.periods, p)
	}
	out.droppedAt = reachedAt(h.droppedAt, step)
	return out, len(out.periods) > 0 || out.droppedAt != nil
}

// away reports whether h, as [hostFailures.at] returned it, is out of the run's live
// hosts: held or dropped.
func (h hostFailures) away() bool {
	return h.droppedAt != nil || len(h.periods) > 0 && h.periods[len(h.periods)-1].returnedAt == nil
}

// openPeriod is h's hold period that has not returned, nil when h is not held.
func (h *hostFailures) openPeriod() *holdPeriod {
	if h == nil || h.droppedAt != nil || len(h.periods) == 0 || h.periods[len(h.periods)-1].returnedAt != nil {
		return nil
	}
	return &h.periods[len(h.periods)-1]
}

// heldAt is h's hold period that holds it at step, nil when h is not held there or is
// dropped. A return decided already may lie ahead of step: a host handled inside a
// default block chosen while it was held rejoins only after the block
// ([runFailures.rejoin]), and the block's later steps still miss it.
func (h *hostFailures) heldAt(step int) *holdPeriod {
	if h == nil || h.droppedAt != nil || len(h.periods) == 0 {
		return nil
	}
	p := &h.periods[len(h.periods)-1]
	if p.heldFrom > step || p.returnedAt != nil && *p.returnedAt <= step {
		return nil
	}
	return p
}

// reachedAt is a copy of at when step has reached it, nil otherwise.
func reachedAt(at *int, step int) *int {
	if at == nil || *at > step {
		return nil
	}
	v := *at
	return &v
}

// useRoster sets the run's roster: its hosts at the start, or the roster read again
// at a refresh boundary.
func (f *runFailures) useRoster(roster []*topology.HostFacts) {
	f.roster = roster
}

// setPlan records the plan's rescue units: covers — every scenario unit, where it can
// reach and when it is decided ([render.Pipeline.RescueCovers]) — and the step shape
// their keys are read by.
func (f *runFailures) setPlan(covers []render.RescueCover, steps []config.StepNode) {
	f.covers, f.steps = covers, steps
	var walk func(nodes []config.StepNode)
	walk = func(nodes []config.StepNode) {
		for _, n := range nodes {
			if len(n.Children) == 0 {
				continue
			}
			tail := n
			for len(tail.Children) > 0 {
				tail = tail.Children[len(tail.Children)-1]
			}
			f.blocks = append(f.blocks, [2]int{n.Step, tail.Step})
			walk(n.Children)
		}
	}
	walk(steps)
}

// failed reports whether the run has failed anywhere yet.
func (f *runFailures) failed() bool {
	return f != nil && (len(f.sources) > 0 || len(f.entries) > 0)
}

// anyHeld reports whether some host holds a source for a rescue unit.
func (f *runFailures) anyHeld() bool {
	if f == nil {
		return false
	}
	for _, s := range f.sources {
		if s.state == sourceHeld {
			return true
		}
	}
	return false
}

// holdsUnits reports whether the plan holds a scenario rescue unit: only such a unit
// holds a host across steps or is decided on a host its step did not reach.
func (f *runFailures) holdsUnits() bool {
	return f != nil && len(f.covers) > 0
}

// decidesAt reports whether a scenario rescue unit is decided at step's barrier.
func (f *runFailures) decidesAt(step int) bool {
	return f != nil && slices.ContainsFunc(f.covers, func(c render.RescueCover) bool { return c.DecidedAt == step })
}

// unitID is the rule id of the rescue unit keyed key ([render.RenderedTask.RescueUnit]),
// 0 for none.
func (f *runFailures) unitID(key string) int {
	if key == "" {
		return 0
	}
	if id, ok := f.unitIDs[key]; ok {
		return id
	}
	f.unitKeys, f.unitSteps = append(f.unitKeys, key), append(f.unitSteps, f.decidedStep(key))
	f.unitIDs[key] = len(f.unitSteps)
	return len(f.unitSteps)
}

// unitDecidedAt is the step at whose barrier the unit keyed key is decided.
func (f *runFailures) unitDecidedAt(key string) int {
	return f.unitSteps[f.unitID(key)-1]
}

// decidedStep is the last step of the unit keyed key, and for a destiny's own unit,
// keyed "<applier position>@…", its applier's: a destiny is one message. -1 for a key
// outside the step shape.
func (f *runFailures) decidedStep(key string) int {
	path, _, _ := strings.Cut(key, "@")
	if _, last, ok := config.StepSpan(f.steps, path); ok {
		return last
	}
	return -1
}

// recordUnitFails records that a task of each unit of ids failed on host sid at step
// ([runFailures.unitFails]); a unit's first failure there stands.
func (f *runFailures) recordUnitFails(sid string, ids []int, step int) {
	for _, u := range ids {
		key := f.unitKeys[u-1]
		if _, seen := f.unitFails[sid][key]; seen {
			continue
		}
		if f.unitFails[sid] == nil {
			f.unitFails[sid] = map[string]int{}
		}
		f.unitFails[sid][key] = step
	}
}

func (f *runFailures) ruleOf(sid string) *config.RescueRule {
	r := f.rules[sid]
	if r == nil {
		r = config.NewRescueRule()
		f.rules[sid] = r
	}
	return r
}

// sourceKeys is every source's key, sorted by host and plan index.
func (f *runFailures) sourceKeys() []sourceKey {
	keys := slices.Collect(maps.Keys(f.sources))
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].sid != keys[b].sid {
			return keys[a].sid < keys[b].sid
		}
		return keys[a].index < keys[b].index
	})
	return keys
}

// barrier applies to the failure state what the hosts of j's step reported in one
// wave — the terminal rows of the step not replayed yet — and returns the entries the
// barrier judges: a host judged at once by its failures at the step, or by a held
// failure its last unit did not handle. last — the step's last barrier: every host
// whose row no wave replayed has the units decided at the step decided too, and its
// failures held for them judged ("after the last wave if no unit reached the host").
func (f *runFailures) barrier(j *stepJudge, statuses []applyrun.HostStatus, roster []*topology.HostFacts, last bool) []failureEntry {
	facts := make(map[string]*topology.HostFacts, len(roster))
	for _, h := range roster {
		facts[h.SID] = h
	}
	var rows []applyrun.HostStatus
	for _, hs := range statuses {
		if hs.Passage != j.step || hs.SID == render.KeeperTargetSID || hs.SID == render.RunSentinelSID || j.replayed[hs.SID] ||
			!rowTerminal(hs.Status) || hs.Status == applyrun.StatusNoMatch {
			continue
		}
		rows = append(rows, hs)
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].SID < rows[b].SID })
	var out []failureEntry
	for _, hs := range rows {
		j.replayed[hs.SID] = true
		if e, ok := f.judgeRow(j, hs, facts[hs.SID]); ok {
			out = append(out, e)
		}
	}
	if last {
		j.swept = true
		for _, sid := range slices.Sorted(maps.Keys(f.rules)) {
			if j.decided[sid] {
				continue
			}
			j.decided[sid] = true
			f.decideUnits(f.rules[sid], j.step)
			f.applyHandled(sid, j.step)
			f.returnHost(sid, j.step)
			if e, ok := f.sweepHost(sid, j.step); ok {
				out = append(out, e)
			}
		}
	}
	return out
}

// judgeRow is [runFailures.barrier] for one host's terminal row.
func (f *runFailures) judgeRow(j *stepJudge, hs applyrun.HostStatus, facts *topology.HostFacts) (failureEntry, bool) {
	sid := hs.SID
	if hs.Status == applyrun.StatusCancelled {
		return j.verdict(hs, nil), true
	}
	failedHere := rowFailed(hs.Status)
	if failedHere {
		f.failedRows[runRow{sid: sid, step: j.step}] = true
	}
	if j.failedAt == nil && j.executed == nil && !failedHere {
		// Nothing failed and nothing is held: the replay could find nothing.
		return failureEntry{}, false
	}
	rule := f.ruleOf(sid)
	rp := f.replay(j, rule, hs)
	if failedHere && rp.unitRow && len(rp.unitsFailed) == 0 && len(rp.sources) == 0 {
		// A unit row's failure is its unit's where the unit fires (rule 6, fail-closed):
		// the message held no source, and the replay read no failure. Where no unit of
		// it fires the failure names no task, so it is tied to none ([runFailures.decideHost]).
		for _, u := range rp.units {
			if rule.Fires(u) {
				rule.Fail(rp.unitFirst[u], failureReason(hs))
				rp.unitsFailed = append(rp.unitsFailed, u)
			}
		}
	}
	f.decideUnits(rule, j.step)
	j.decided[sid] = true
	var positions []int
	for _, pos := range rp.sources {
		// A row the Soul finished `success` holds no failure its message left
		// unhandled; one that would be is no source of the run.
		if failedHere || !rule.Held(pos) {
			positions = append(positions, pos)
		}
	}
	fresh := f.newSources(j, hs, positions)
	if failedHere && len(fresh) > 0 && !slices.ContainsFunc(fresh, func(s *failedSource) bool { return slices.ContainsFunc(s.members, rule.Held) }) {
		// Fail-closed (rule 6): the Soul reported the row failed although every source
		// of its message reads handled — an older binary under fail-stop, say. Its
		// first source is not handled.
		for _, m := range fresh[0].members {
			rule.Reopen(m)
		}
	}
	f.applyHandled(sid, j.step)
	f.attachUnitFailures(sid, rule, rp.unitsFailed, j.step, failureReason(hs))
	f.recordUnitFails(sid, rp.unitsFailed, j.step)
	var entry failureEntry
	judged := false
	if failedHere {
		entry, judged = f.decideHost(j, hs, facts, fresh, rp.unitRow && len(rp.unitsFailed) > 0)
	}
	f.returnHost(sid, j.step)
	if e, ok := f.sweepHost(sid, j.step); ok && (!judged || entry.radius == config.OnFailureHost && e.radius != config.OnFailureHost) {
		entry, judged = e, true
	}
	return entry, judged
}

// replayed is what one host's message at a step came to in its rule.
type replayed struct {
	// sources — the positions of the message's tasks that failed as sources: neither
	// an applier's terminal nor a task of a rescue unit, save one whose unit did not
	// fire ([config.RescueRule.Fail]).
	sources []int
	// units — the units the message carries, in message order, each with its first
	// position there (unitFirst); unitsFailed — those a failure of this message failed.
	units       []int
	unitFirst   map[int]int
	unitsFailed []int
	// unitRow — the message carries tasks of units only, beside any applier's terminal.
	unitRow bool
}

// replay feeds host sid's message at j's step into its rule in the Soul's order, plan
// order (rule 6): every task registered with what the plan's `onfail:` names — the
// rendered task's, not the copy the requisite gate rewrote — then what each executed
// and failed, a unit that is decided at this step and ends before a task of the
// message decided before that task runs, as the Soul decides it. A unit row decides
// nothing here: its failure, if it failed, is its units' ([runFailures.judgeRow]).
//
// A host the Keeper failed before sending failed by its cause alone: the records it
// wrote FAILED for the rest of the message, on either side of the cause, are of tasks
// that never ran (rule 2). The cause is fed first, and of the rest only the sources —
// tasks of no unit, terminals aside — which the host is judged with. A unit task among
// them neither runs, fails its unit nor stands as a net source, and no sibling fails
// before the cause.
func (f *runFailures) replay(j *stepJudge, rule *config.RescueRule, hs applyrun.HostStatus) replayed {
	sid := hs.SID
	msg := j.replayMessage(sid)
	failed := j.failedOn(hs)
	rp := replayed{unitFirst: map[int]int{}}
	lastAt := map[int]int{}
	isSource := map[int]bool{}
	for i, t := range msg {
		terminal := len(t.AggregateOf) > 0
		u := f.unitID(t.RescueUnit)
		rule.Task(t.Index, u, t.OnFailIdx, terminal, t.AggregateOf)
		switch {
		case u != 0:
			if _, seen := rp.unitFirst[u]; !seen {
				rp.unitFirst[u] = t.Index
				rp.units = append(rp.units, u)
			}
			if f.unitDecidedAt(t.RescueUnit) == j.step {
				lastAt[u] = i
			}
		case !terminal:
			isSource[t.Index] = true
		}
	}
	rp.unitRow = len(rp.units) > 0 && len(isSource) == 0
	before := map[int]bool{}
	for _, u := range rp.units {
		before[u] = rule.UnitFailed(u)
	}
	fail := func(idx int) {
		rule.Fail(idx, failureReason(hs))
		if isSource[idx] || rule.UnitOf(idx) != 0 && rule.Held(idx) {
			rp.sources = append(rp.sources, idx)
		}
	}
	cause := -1
	if sh := j.keeperFailed(sid); sh != nil && failed[sh.records[0].task.Index] {
		cause = sh.records[0].task.Index
		rule.Ran(cause, true)
		fail(cause)
	}
	for i, t := range msg {
		if !rp.unitRow {
			for _, u := range rp.units {
				if l, ok := lastAt[u]; ok && l < i {
					rule.Decide(u, false)
				}
			}
		}
		if t.Index == cause || cause >= 0 && !isSource[t.Index] {
			continue
		}
		rule.Ran(t.Index, failed[t.Index] || j.executed[sid][t.Index])
		if failed[t.Index] {
			fail(t.Index)
		}
	}
	for _, u := range rp.units {
		if !before[u] && rule.UnitFailed(u) {
			rp.unitsFailed = append(rp.unitsFailed, u)
		}
	}
	return rp
}

// replayMessage is what host sid was sent or settled at the step, as rendered, in plan
// order: a host the Keeper failed before sending, its records; any other,
// [stepJudge.message].
func (j *stepJudge) replayMessage(sid string) []*render.RenderedTask {
	recs := j.message(sid)
	if sh := j.keeperFailed(sid); sh != nil {
		recs = sh.records
	}
	out := make([]*render.RenderedTask, 0, len(recs))
	for _, rec := range recs {
		t := rec.task
		if orig := j.byIndex[t.Index]; orig != nil {
			t = orig
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out
}

// failedOn is the plan indices of the step's tasks that failed on hs's host: what the
// audit log holds FAILED or TIMED_OUT, what the barrier recorded for a host that
// stopped reporting, what the Keeper recorded FAILED for a host it failed before
// sending, and the row's first failure.
func (j *stepJudge) failedOn(hs applyrun.HostStatus) map[int]bool {
	out := map[int]bool{}
	for idx := range j.failedAt[hs.SID] {
		if j.byIndex[idx] != nil {
			out[idx] = true
		}
	}
	for idx := range j.recorded[hs.SID] {
		out[idx] = true
	}
	if sh := j.keeperFailed(hs.SID); sh != nil {
		for _, rec := range sh.records {
			if rec.status == keeperv1.TaskStatus_TASK_STATUS_FAILED {
				out[rec.task.Index] = true
			}
		}
	}
	if t := j.reportedFailure(hs); t != nil {
		out[t.Index] = true
	}
	return out
}

// decideUnits decides on rule every unit decided at step's barrier, in the order the
// run first met them — those that reached the host at an earlier step only included:
// such a unit is decided by what it did there.
func (f *runFailures) decideUnits(rule *config.RescueRule, step int) {
	for i, at := range f.unitSteps {
		if at == step {
			rule.Decide(i+1, false)
		}
	}
}

// newSources records the sources hs's host failed at j's step at positions, one per
// register — a `loop:`'s failed iterations are one source (rule 2) — each with the
// radius it is judged by.
func (f *runFailures) newSources(j *stepJudge, hs applyrun.HostStatus, positions []int) []*failedSource {
	if len(positions) == 0 {
		return nil
	}
	sid := hs.SID
	appliers := map[int]string{}
	for _, t := range j.byIndex {
		for _, child := range t.AggregateOf {
			appliers[child] = t.Register
		}
	}
	whole := ""
	if sh := j.keeperFailed(sid); sh != nil {
		whole = *j.owner.OnFailure
		for _, rec := range sh.records {
			if t := j.byIndex[rec.task.Index]; t != nil && slices.Contains(positions, t.Index) {
				whole = j.radiusOf(t)
				break
			}
		}
	}
	byRegister := map[string]*failedSource{}
	var out []*failedSource
	for _, idx := range slices.Sorted(slices.Values(positions)) {
		t := j.byIndex[idx]
		if t == nil {
			continue
		}
		if s := byRegister[t.Register]; t.Register != "" && s != nil {
			s.members = append(s.members, idx)
			continue
		}
		key := sourceKey{sid: sid, index: idx}
		if f.sources[key] != nil {
			continue
		}
		radius := j.radiusOf(t)
		switch {
		case whole != "":
			radius = whole
		case j.recorded[sid][idx]:
			radius = *j.owner.OnFailure
		}
		s := &failedSource{
			task:            t.Name,
			register:        t.Register,
			applierRegister: appliers[idx],
			members:         []int{idx},
			failedAt:        j.step,
			changedAt:       j.step,
			radius:          radius,
			reason:          failureReason(hs),
			rows:            []int{j.step},
		}
		f.sources[key] = s
		f.bySID[sid] = append(f.bySID[sid], key)
		if t.Register != "" {
			byRegister[t.Register] = s
		}
		out = append(out, s)
	}
	return out
}

// applyHandled marks handled at step every source of host sid not judged whose every
// member a unit handled on its rule (rule 3). Judged is final: a unit that succeeds
// afterwards, as compensation, handles nothing.
func (f *runFailures) applyHandled(sid string, step int) {
	rule := f.rules[sid]
	if rule == nil {
		return
	}
	for _, k := range f.bySID[sid] {
		s := f.sources[k]
		if s.state == sourceJudged || s.state == sourceHandled || slices.ContainsFunc(s.members, rule.Held) {
			continue
		}
		s.state, s.changedAt = sourceHandled, step
	}
}

// attachUnitFailures gives every source of host sid that a failed unit holds that
// unit's error and its row at step: the entry of a source judged after its units
// carries their errors, and a dropped host's rows they failed on are marked with it
// (rule 4).
func (f *runFailures) attachUnitFailures(sid string, rule *config.RescueRule, units []int, step int, reason string) {
	for _, u := range units {
		seen := map[*failedSource]bool{}
		for _, pos := range rule.Sources(u) {
			s := f.sourceAt(sid, pos)
			if s == nil || s.state == sourceHandled || seen[s] {
				continue
			}
			seen[s] = true
			s.unitErrs = append(s.unitErrs, reason)
			if !slices.Contains(s.rows, step) {
				s.rows = append(s.rows, step)
			}
		}
	}
}

// sourceAt is host sid's source with a member at plan index pos, nil for none.
func (f *runFailures) sourceAt(sid string, pos int) *failedSource {
	for _, k := range f.bySID[sid] {
		if s := f.sources[k]; slices.Contains(s.members, pos) {
			return s
		}
	}
	return nil
}

// decideHost decides the failure of a host's row by its sources new at the step, fresh
// (rule 2). Unless the Keeper failed the host before sending it a group rolled as one,
// they are held when every one a unit did not handle has a rescue unit ahead that
// covers the host: nothing is judged, and the host is held from the next step on.
// Otherwise the host is judged at once by every one of them, and its units reach it
// only as compensation. A row whose failure failed a unit that fired there (unitsOwn),
// and one with no source left unhandled, is judged at once by nothing. A failure tied
// to no task with nothing recorded is judged at once — a unit row where no unit fired
// included — by the barrier owner's radius (rule 4).
func (f *runFailures) decideHost(j *stepJudge, hs applyrun.HostStatus, facts *topology.HostFacts, fresh []*failedSource, unitsOwn bool) (failureEntry, bool) {
	var open []*failedSource
	for _, s := range fresh {
		if s.state != sourceHandled {
			open = append(open, s)
		}
	}
	if len(open) == 0 {
		if unitsOwn || len(fresh) > 0 {
			return failureEntry{}, false
		}
		f.sourceless[hs.SID] = append(f.sourceless[hs.SID], j.step)
		return j.verdict(hs, nil), true
	}
	if !j.keeperFailedGroup(hs.SID) {
		lasts := make([]*int, len(open))
		for i, s := range open {
			if lasts[i] = f.lastUnit(s, j.step, facts); lasts[i] == nil {
				lasts = nil
				break
			}
		}
		if lasts != nil {
			for i, s := range open {
				s.state, s.changedAt, s.lastUnit = sourceHeld, j.step, lasts[i]
			}
			f.hold(hs.SID, facts, j.step+1)
			return failureEntry{}, false
		}
	}
	for _, s := range open {
		s.state, s.changedAt = sourceJudged, j.step
	}
	return j.verdict(hs, open), true
}

// lastUnit is the step at whose barrier the last rescue unit of s ahead of step that
// covers host h is decided, nil when no unit of it is ahead there: a unit naming its
// register or its applier's, with a step after step, whose `on:` chain selects h.
func (f *runFailures) lastUnit(s *failedSource, step int, h *topology.HostFacts) *int {
	if h == nil {
		return nil
	}
	var last *int
	for _, c := range f.covers {
		if c.Last <= step || !render.HostCovered(h, c.Covens) {
			continue
		}
		if !slices.Contains(c.Sources, s.register) && (s.applierRegister == "" || !slices.Contains(c.Sources, s.applierRegister)) {
			continue
		}
		if last == nil || c.DecidedAt > *last {
			at := c.DecidedAt
			last = &at
		}
	}
	return last
}

// hold holds host sid from step from on (rule 2): a hold period opens with facts as
// the host's, unless one is open or the host is dropped — a dropped host keeps its
// sources held, but it is not held ("What the run goroutine keeps").
func (f *runFailures) hold(sid string, facts *topology.HostFacts, from int) {
	h := f.hosts[sid]
	if h == nil {
		h = &hostFailures{}
		f.hosts[sid] = h
	}
	if h.droppedAt != nil || h.openPeriod() != nil {
		return
	}
	if facts != nil || h.facts == nil {
		h.facts = factsOr(facts, sid)
	}
	h.periods = append(h.periods, holdPeriod{heldFrom: from})
}

// returnHost returns held host sid once no source of it is held or judged (rule 3): it
// is back from the step after step, the barrier of the unit that handled the last of
// them — or after a default block whose hosts were chosen at its first step while the
// host was held, since fork 3 holds them. A dropped host never returns.
func (f *runFailures) returnHost(sid string, step int) {
	p := f.hosts[sid].openPeriod()
	if p == nil {
		return
	}
	for _, k := range f.bySID[sid] {
		if st := f.sources[k].state; st == sourceHeld || st == sourceJudged {
			return
		}
	}
	at := f.rejoin(step+1, p.heldFrom)
	p.returnedAt = &at
}

// rejoin is the step a host held from heldFrom rejoins at when its failures are
// handled in time for step from: from, or past every default block whose first step
// came while the host was held and that is still going on at from.
func (f *runFailures) rejoin(from, heldFrom int) int {
	for moved := true; moved; {
		moved = false
		for _, b := range f.blocks {
			if b[0] >= heldFrom && b[0] < from && b[1] >= from {
				from, moved = b[1]+1, true
			}
		}
	}
	return from
}

// sweepHost judges, at step's barrier, every source host sid holds whose last unit is
// decided there and did not handle it (rule 4): by its own radius, from the held entry
// — the host may have no row at the step. The entry names the source that ends the
// run, if one does, or the first; a host dropped already only gains the entry.
func (f *runFailures) sweepHost(sid string, step int) (failureEntry, bool) {
	var due []*failedSource
	for _, k := range f.bySID[sid] {
		if s := f.sources[k]; s.state == sourceHeld && s.lastUnit != nil && *s.lastUnit <= step {
			due = append(due, s)
		}
	}
	if len(due) == 0 {
		return failureEntry{}, false
	}
	sort.Slice(due, func(a, b int) bool { return due[a].members[0] < due[b].members[0] })
	pick := due[0]
	for _, s := range due {
		if s.radius != config.OnFailureHost {
			pick = s
			break
		}
	}
	for _, s := range due {
		s.state, s.changedAt = sourceJudged, step
	}
	at := pick.failedAt
	return failureEntry{step: step, sid: sid, task: pick.task, radius: pick.radius, reason: pick.reason, failedAt: &at, rescues: slices.Clone(pick.unitErrs)}, true
}

// recordMissed keeps, for every host held at step, the register a task of the step it
// does not receive would have left it (rule 3): a skipped task's, and for an applier's
// terminal all four `false`, what a Soul's fail-stop folds an applier over skipped
// children to. Render reads them beneath the host's own registers once it returns
// ([render.HoldPeriod.Synth]). The tasks are those plans record it as not reached.
func (f *runFailures) recordMissed(step int, tasks []*render.RenderedTask, plans []render.DispatchPlan) {
	if f == nil {
		return
	}
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	for _, pl := range plans {
		t := byIndex[pl.TaskIndex]
		if t == nil || t.Register == "" {
			continue
		}
		for _, sid := range pl.NotReached {
			p := f.hosts[sid].heldAt(step)
			if p == nil {
				continue
			}
			if p.missed == nil {
				p.missed = map[string]any{}
			}
			p.missed[t.Register] = missedRegister(t)
		}
	}
}

func missedRegister(t *render.RenderedTask) map[string]any {
	reg := keeperRecordRegister(keeperv1.TaskStatus_TASK_STATUS_SKIPPED).AsMap()
	if len(t.AggregateOf) > 0 {
		reg["skipped"] = false
	}
	return reg
}

// failureView is the failure state as one step saw it when it was dispatched: what
// the barriers before it had decided. Plain data: a later barrier does not change it.
// What it shares with the run is never written through: its roster facts, pointers
// into the run's roster and its state; roster itself, which while no host's standing
// has changed is the run's roster slice ([runRoster]) — the backing array of the
// render input's Hosts; and the registers in each period's missed, cloned one level
// deep, which [failureView.standing] hands render as [render.HoldPeriod.Synth].
type failureView struct {
	step    int
	ended   bool
	endedAt int
	// roster — every host of the run ([runRoster], [render.Standing.Roster]).
	roster []*topology.HostFacts
	// hosts — the hosts whose standing had changed by step.
	hosts map[string]hostFailures
	// sources — the sources failed before step, each in its state at step.
	sources map[sourceKey]failedSource
	// unitFails — [runFailures.unitFails] narrowed to the units failed before step.
	unitFails map[string]map[string]int
}

// view is the state as step saw it. A source failed at or after step is not in it,
// and one whose state a barrier at or after step decided is held in it. A nil list
// — a staged run — has an empty view.
func (f *runFailures) view(step int) failureView {
	v := failureView{step: step}
	if f == nil {
		return v
	}
	v.ended, v.endedAt = f.ended && f.endedAt < step, f.endedAt
	v.hosts = make(map[string]hostFailures, len(f.hosts))
	away := map[string]*topology.HostFacts{}
	for sid, h := range f.hosts {
		at, changed := h.at(step)
		if !changed {
			continue
		}
		v.hosts[sid] = at
		if at.away() {
			away[sid] = at.facts
		}
	}
	v.roster = runRoster(f.roster, away)
	v.sources = make(map[sourceKey]failedSource, len(f.sources))
	for k, s := range f.sources {
		if s.failedAt >= step {
			continue
		}
		at := *s
		if at.changedAt >= step {
			at.state = sourceHeld
		}
		at.members, at.unitErrs, at.rows = slices.Clone(s.members), slices.Clone(s.unitErrs), slices.Clone(s.rows)
		if s.lastUnit != nil {
			u := *s.lastUnit
			at.lastUnit = &u
		}
		v.sources[k] = at
	}
	for sid, units := range f.unitFails {
		for key, at := range units {
			if at >= step {
				continue
			}
			if v.unitFails == nil {
				v.unitFails = map[string]map[string]int{}
			}
			if v.unitFails[sid] == nil {
				v.unitFails[sid] = map[string]int{}
			}
			v.unitFails[sid][key] = at
		}
	}
	return v
}

// unitFailed reports whether a task of the rescue unit keyed key failed on host sid
// before the view's step: the unit can no longer succeed there, so its later tasks are
// not sent to the host (rule 1), as the Soul skips them inside one message. Render
// routes no such task there ([render.Rescue.FailedUnits]); the requisite gate reads it
// too, as a defence.
func (v failureView) unitFailed(sid, key string) bool {
	_, failed := v.unitFails[sid][key]
	return failed
}

// runRoster is every host of the run: roster's in their order, and merged in by SID
// each host held or dropped at the step, away, with the facts it left with in place of
// roster's — one a roster read again no longer lists included. A host that returned
// takes roster's entry, or is gone with it. The roster is read sorted by SID, so the
// result is too.
func runRoster(roster []*topology.HostFacts, away map[string]*topology.HostFacts) []*topology.HostFacts {
	if len(away) == 0 {
		return roster
	}
	left := slices.Collect(maps.Values(away))
	sort.Slice(left, func(a, b int) bool { return left[a].SID < left[b].SID })
	out := make([]*topology.HostFacts, 0, len(roster)+len(left))
	next := 0
	for _, h := range roster {
		if away[h.SID] != nil {
			continue
		}
		for next < len(left) && left[next].SID < h.SID {
			out = append(out, left[next])
			next++
		}
		out = append(out, h)
	}
	return append(out, left[next:]...)
}

// split is the run's hosts live, held and dropped at the view's step, each in the
// order of [failureView.roster].
func (v failureView) split() (live, held, dropped []*topology.HostFacts) {
	if len(v.hosts) == 0 {
		return v.roster, nil, nil
	}
	for _, h := range v.roster {
		hf, changed := v.hosts[h.SID]
		switch {
		case !changed:
			live = append(live, h)
		case hf.droppedAt != nil:
			dropped = append(dropped, h)
		case hf.periods[len(hf.periods)-1].returnedAt == nil:
			held = append(held, h)
		default:
			live = append(live, h)
		}
	}
	return live, held, dropped
}

// allHosts is every host of the run at the view's step, live, held and dropped.
func allHosts(v failureView) []*topology.HostFacts {
	live, held, dropped := v.split()
	return slices.Concat(live, held, dropped)
}

// standing is the hosts as every step up to the view's saw them, for render; nil
// while no host's standing has changed — every step then renders over one roster.
func (v failureView) standing() *render.Standing {
	if len(v.hosts) == 0 {
		return nil
	}
	by := make(map[string]render.HostStanding, len(v.hosts))
	for sid, h := range v.hosts {
		hs := render.HostStanding{DroppedAt: h.droppedAt}
		for _, p := range h.periods {
			hs.Periods = append(hs.Periods, render.HoldPeriod{HeldFrom: p.heldFrom, ReturnedAt: p.returnedAt, Synth: p.missed})
		}
		by[sid] = hs
	}
	return &render.Standing{Roster: v.roster, BySID: by}
}

// rescue is the failures as rescue routing reads them: by host, each failed
// register — a source's own and its applier's — with the first step it failed at,
// those whose every source a unit handled, with the step that handled the last, and
// the units that failed there. nil while no host has left the run and it goes on: the
// render is then exactly what it was before rescue routing, and no unit has fired, so
// none has failed.
func (v failureView) rescue() *render.Rescue {
	if !v.ended && len(v.hosts) == 0 {
		return nil
	}
	out := &render.Rescue{Ended: v.ended, EndedAt: v.endedAt, Failed: map[string]map[string]int{}, FailedUnits: v.unitFails}
	type hostRegister struct{ sid, register string }
	handled := map[hostRegister]int{}
	unhandled := map[hostRegister]bool{}
	for k, s := range v.sources {
		for _, name := range []string{s.register, s.applierRegister} {
			if name == "" {
				continue
			}
			if out.Failed[k.sid] == nil {
				out.Failed[k.sid] = map[string]int{}
			}
			if at, ok := out.Failed[k.sid][name]; !ok || s.failedAt < at {
				out.Failed[k.sid][name] = s.failedAt
			}
			hr := hostRegister{k.sid, name}
			if s.state != sourceHandled {
				unhandled[hr] = true
			} else if by, ok := handled[hr]; !ok || s.changedAt > by {
				handled[hr] = s.changedAt
			}
		}
	}
	for hr, by := range handled {
		if unhandled[hr] {
			continue
		}
		if out.HandledAt == nil {
			out.HandledAt = map[string]map[string]int{}
		}
		if out.HandledAt[hr.sid] == nil {
			out.HandledAt[hr.sid] = map[string]int{}
		}
		out.HandledAt[hr.sid][hr.register] = by
	}
	return out
}

// unhandledKeys are the sources no unit handled, held or judged, as (sid, plan index)
// for every member — what the requisite gate reads where it read the FAILED facts.
func (v failureView) unhandledKeys() map[auditpg.ChangedTaskKey]struct{} {
	out := make(map[auditpg.ChangedTaskKey]struct{}, len(v.sources))
	for k, s := range v.sources {
		if s.state == sourceHandled {
			continue
		}
		for _, m := range s.members {
			out[auditpg.ChangedTaskKey{SID: k.sid, PlanIndex: m}] = struct{}{}
		}
	}
	return out
}
