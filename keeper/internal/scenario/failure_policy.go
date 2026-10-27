package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/applysink"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// How a linear run's barrier judges failures (docs/scenario/settings.md §4,
// NIM-948). The barrier waits for every host it sent the step to — a host still
// working is never failed on another's account — and judges each failed host by the
// effective `on_failure` of the task it failed at. Radius `scenario` ends the run once
// the step has finished; radius `host` drops the host: it receives no later ordinary
// task, only its rescues, and the run goes on without it until `tolerate` is exceeded or
// no host is left. A run that reaches its end without some hosts ends `partial_failed`.
// A failure a rescue unit ahead may handle is held instead, and judged only if none
// does (ADR-0091 amendment NIM-993, rules 2-4; failure_state.go).

// radiusCancelled is the verdict on a cancelled row: it ends the run whatever the
// policy says — a Cancel is an instruction, not a failure.
const radiusCancelled = "cancelled"

// reasonPartialFailed is the status_details reason of a run that finished without the
// hosts it dropped — the run status it ends with.
const reasonPartialFailed = "partial_failed"

// failureEntry is one host's failure at one step, as the barrier judged it.
type failureEntry struct {
	step int
	sid  string
	// task is the name of the task the host failed at; "" — a failure tied to no task
	// (a send that failed, a lost agent), judged by the step's barrier owner.
	task   string
	radius string
	reason string
	// failedAt — a source judged at a later barrier than the step it failed at, at its
	// last rescue unit's (rule 4): that step. nil — judged where it failed.
	failedAt *int
	// rescues — the errors of its rescue units that failed, in the order they failed.
	rescues []string
	// held — a source still waiting for a rescue unit when the list closed (rule 2).
	held bool
}

func (e failureEntry) String() string {
	if e.radius == radiusCancelled {
		return fmt.Sprintf("step %d: host %s was cancelled: %s", e.step, e.sid, e.reason)
	}
	at := "with no task"
	if e.task != "" {
		at = fmt.Sprintf("at task %q", e.task)
	}
	if e.failedAt != nil {
		at += fmt.Sprintf(" of step %d, which no rescue handled", *e.failedAt)
	}
	if e.held {
		at += ", held for a rescue that did not run"
	}
	out := fmt.Sprintf("step %d: host %s failed %s [on_failure: %s]: %s", e.step, e.sid, at, e.radius, e.reason)
	for _, r := range e.rescues {
		out += "; its rescue failed: " + r
	}
	return out
}

// runFailures is a linear run's failure list: append-only, closed when the run ends.
// Every failure the barriers judged is in it, including those of hosts that were still
// working when another host's failure ended the run. It also holds the run's failure
// state (failure_state.go).
type runFailures struct {
	entries []failureEntry
	closed  bool
	// roster — the run's hosts as last read ([runFailures.useRoster]).
	roster []*topology.HostFacts
	// hosts — the standing: by SID, every host whose standing changed.
	hosts map[string]*hostFailures
	// sources — every failed source of every host; bySID — their keys by host, in the
	// order they failed.
	sources map[sourceKey]*failedSource
	bySID   map[string][]sourceKey
	// sourceless — by host, the steps whose failed rows held a failure judged at once
	// that no source stands for: a failure tied to no task with nothing recorded.
	sourceless map[string][]int
	// failedRows — the rows the barriers judged failed or orphaned: the only rows the
	// `apply_runs.handled` mark can go on ([runFailures.handledRows]).
	failedRows map[runRow]bool
	// rules — by host, the in-message rule the barriers replay its messages through
	// (rule 6).
	rules map[string]*config.RescueRule
	// The plan's rescue units ([runFailures.setPlan]): covers — where each scenario
	// unit reaches and when it is decided; steps — the step shape their keys are read
	// by; blocks — the default blocks' step spans, which a returning host waits out
	// (rule 3); unitIDs — each unit key's rule id, and unitKeys and unitSteps by id - 1
	// its key and the step it is decided at.
	covers    []render.RescueCover
	steps     []config.StepNode
	blocks    [][2]int
	unitIDs   map[string]int
	unitKeys  []string
	unitSteps []int
	// unitFails — by host, each unit key a task of which failed there, with the step
	// whose barrier saw it fail: the unit can no longer succeed there (rule 1).
	unitFails map[string]map[string]int
	// ended — a failure ended the run at the barrier of step endedAt, under endLead;
	// it goes on only to the rescues ahead (rescues.md).
	ended   bool
	endedAt int
	endLead string
}

// errRunEnded is a barrier's verdict that a failure ended the run; the failure list
// holds why ([runFailures.withEnd]).
var errRunEnded = errors.New("scenario: a failure ended the run")

// errCancelledAfterEnd is a Cancel judged at a rescue's barrier: it stops the run at
// once, and the failure list already names it.
var errCancelledAfterEnd = errors.New("scenario: a host was cancelled after a failure ended the run")

func newRunFailures() *runFailures {
	return &runFailures{
		hosts:      map[string]*hostFailures{},
		sources:    map[sourceKey]*failedSource{},
		bySID:      map[string][]sourceKey{},
		sourceless: map[string][]int{},
		failedRows: map[runRow]bool{},
		rules:      map[string]*config.RescueRule{},
		unitIDs:    map[string]int{},
		unitFails:  map[string]map[string]int{},
	}
}

func (f *runFailures) append(entries ...failureEntry) error {
	if f.closed {
		return fmt.Errorf("scenario: %d failure(s) judged after the run's failure list was closed", len(entries))
	}
	f.entries = append(f.entries, entries...)
	return nil
}

// close closes the list. A source still held for a rescue unit is appended first, so
// the run's error names it (rule 2): a Cancel or an abort stopped the run before the
// barrier of its last unit.
func (f *runFailures) close() {
	if f.closed {
		return
	}
	for _, k := range f.sourceKeys() {
		if s := f.sources[k]; s.state == sourceHeld {
			f.entries = append(f.entries, failureEntry{
				step: s.failedAt, sid: k.sid, task: s.task, radius: s.radius, reason: s.reason,
				rescues: slices.Clone(s.unitErrs), held: true,
			})
		}
	}
	f.closed = true
}

// isDropped reports whether the run dropped host sid, at any step so far.
func (f *runFailures) isDropped(sid string) bool {
	h := f.hosts[sid]
	return h != nil && h.droppedAt != nil
}

// dropCount is how many hosts the run dropped so far.
func (f *runFailures) dropCount() int {
	n := 0
	for _, h := range f.hosts {
		if h.droppedAt != nil {
			n++
		}
	}
	return n
}

// live is roster without the hosts the run dropped — what a step may target. A
// dropped host stays out even when a re-read roster still lists it.
func (f *runFailures) live(roster []*topology.HostFacts) []*topology.HostFacts {
	if f == nil || f.dropCount() == 0 {
		return roster
	}
	out := make([]*topology.HostFacts, 0, len(roster))
	for _, h := range roster {
		if !f.isDropped(h.SID) {
			out = append(out, h)
		}
	}
	return out
}

// drops are the SIDs of the dropped hosts, sorted.
func (f *runFailures) drops() []string {
	var out []string
	for sid, h := range f.hosts {
		if h.droppedAt != nil {
			out = append(out, sid)
		}
	}
	sort.Strings(out)
	return out
}

// errorOf is the run's failure list as one error, every entry in the order it was
// judged, under lead.
func (f *runFailures) errorOf(lead string) error {
	parts := make([]string, 0, len(f.entries))
	for _, e := range f.entries {
		parts = append(parts, e.String())
	}
	return fmt.Errorf("scenario: %s: %s", lead, strings.Join(parts, "; "))
}

// settle appends a barrier's verdicts and applies the policy: [errRunEnded] when they
// end the run, or the entries whose hosts it drops now. owner is the merged settings
// of the step's barrier owner, reached the hosts the run has targeted so far —
// `tolerate`'s denominator (user decision, NIM-948) — and roster the step's hosts,
// the held ones among them, which hold the facts a dropped host keeps in
// `soulprint.hosts`. `tolerate` is checked at every barrier, one with no new failure
// included: a step may tighten it (settings.md §2.1). Drops are committed only when
// the run goes on.
//
// A held host has no verdict, is not counted by `tolerate` and counts as still in the
// run for "no host is left" (rule 2). A host already dropped is out of the count: a
// source of it held when it was dropped is judged at its own unit's barrier, and under
// `host` that records nothing more. Once the run has ended a failure ends nothing
// more: it is one more entry, and under `host` its host's drop is recorded as well.
// Only a Cancel still ends the run at once.
func (f *runFailures) settle(step int, verdicts []failureEntry, owner config.ScenarioSettings, reached int, roster []*topology.HostFacts) ([]failureEntry, error) {
	if err := f.append(verdicts...); err != nil {
		return nil, err
	}
	for _, v := range verdicts {
		if v.radius == radiusCancelled && f.ended {
			return nil, errCancelledAfterEnd
		}
		if v.radius == radiusCancelled {
			return nil, f.errorOf(fmt.Sprintf("step %d failed", v.step))
		}
	}
	if f.ended {
		return f.drop(verdicts, roster), nil
	}
	fresh := 0
	for _, v := range verdicts {
		if v.radius != config.OnFailureHost {
			return nil, f.end(step, fmt.Sprintf("step %d failed", v.step))
		}
		if !f.isDropped(v.sid) {
			fresh++
		}
	}
	if fresh > 0 && len(f.live(roster))-fresh <= 0 {
		return nil, f.end(step, fmt.Sprintf("step %d: no host is left in the run", step))
	}
	if dropped := f.dropCount() + fresh; owner.Tolerate != nil && dropped > 0 {
		if owner.Tolerate.Exceeded(dropped, reached) {
			return nil, f.end(step, fmt.Sprintf("step %d: %d hosts dropped under on_failure: host, more than tolerate allows (%d of %d)",
				step, dropped, owner.Tolerate.Limit(reached), reached))
		}
	}
	return f.drop(verdicts, roster), nil
}

// drop records the hosts verdicts drop under radius `host` and returns the entries of
// those it drops now, each out from the step after its barrier: a host is audited
// `apply.host_dropped` once. A host already out keeps the facts it left with and the
// step it left at; a held host keeps the facts it was held with.
func (f *runFailures) drop(verdicts []failureEntry, roster []*topology.HostFacts) []failureEntry {
	bySID := make(map[string]*topology.HostFacts, len(roster))
	for _, h := range roster {
		bySID[h.SID] = h
	}
	var out []failureEntry
	for _, v := range verdicts {
		if v.radius != config.OnFailureHost {
			continue
		}
		h := f.hosts[v.sid]
		if h == nil {
			h = &hostFailures{facts: factsOr(bySID[v.sid], v.sid)}
			f.hosts[v.sid] = h
		}
		if h.droppedAt != nil {
			continue
		}
		from := v.step + 1
		h.droppedAt = &from
		out = append(out, v)
	}
	return out
}

// droppedRows are the steps whose failed rows a dropped host left the run by, sorted:
// the rows holding a judged source of it — the source's own and those of its rescue
// units that failed (rule 4) — and those of a failure judged at once that no source
// stands for. Each is marked `dropped` when the run finishes without the host.
func (f *runFailures) droppedRows(sid string) []int {
	rows := slices.Clone(f.sourceless[sid])
	for _, k := range f.bySID[sid] {
		if s := f.sources[k]; s.state == sourceJudged {
			rows = append(rows, s.rows...)
		}
	}
	slices.Sort(rows)
	return slices.Compact(rows)
}

// runRow is host sid's `apply_runs` row of step.
type runRow struct {
	sid  string
	step int
}

// handledRows are the failed rows whose every failure a rescue unit handled (rule 5),
// sorted by host and step: each holds a source — as the source's own row, or as the
// row of a unit of it that failed before one succeeded — and every source it holds is
// handled. A row that also holds a failure no source stands for is not one, and
// neither is a row that holds no source. nil on a nil list.
func (f *runFailures) handledRows() []runRow {
	if f == nil {
		return nil
	}
	handled := map[runRow]bool{}
	for _, k := range f.sourceKeys() {
		s := f.sources[k]
		for _, step := range s.rows {
			row := runRow{sid: k.sid, step: step}
			if !f.failedRows[row] {
				continue
			}
			all, seen := handled[row]
			handled[row] = (all || !seen) && s.state == sourceHandled
		}
	}
	for sid, steps := range f.sourceless {
		for _, step := range steps {
			delete(handled, runRow{sid: sid, step: step})
		}
	}
	var out []runRow
	for row, all := range handled {
		if all {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].sid != out[b].sid {
			return out[a].sid < out[b].sid
		}
		return out[a].step < out[b].step
	})
	return out
}

func factsOr(h *topology.HostFacts, sid string) *topology.HostFacts {
	if h == nil {
		return &topology.HostFacts{SID: sid}
	}
	return h
}

// end marks the run ended at step, its failure list under lead.
func (f *runFailures) end(step int, lead string) error {
	f.ended, f.endedAt, f.endLead = true, step, lead
	return errRunEnded
}

// withEnd closes the list of a run that a failure ended and returns the error it
// finishes with: the whole list, the rescues' failures included, beside whatever else
// stopped it — a Cancel, an error of a rescue step.
func (f *runFailures) withEnd(cause error) error {
	f.close()
	list := f.errorOf(f.endLead)
	if errors.Is(cause, errRunEnded) || errors.Is(cause, errCancelledAfterEnd) {
		return list
	}
	return errors.Join(cause, list)
}

// withHeld closes the list of a run that stopped before a failure ended it, or that
// reached its end with cause nil, and returns the error it finishes with: cause,
// joined with the whole list when a failure was still held for a rescue unit (rule 2)
// — a Cancel stops the run at once, and no rescue runs (rule 9).
func (f *runFailures) withHeld(cause error) error {
	if f.closed {
		return cause
	}
	n := len(f.entries)
	f.close()
	if len(f.entries) == n {
		return cause
	}
	return errors.Join(cause, f.errorOf("the run stopped with failures held for a rescue"))
}

// stepJudge judges the failed rows of one step.
type stepJudge struct {
	step int
	// owner — the merged settings of the step's barrier owner.
	owner config.ScenarioSettings
	// byIndex — the step's tasks by plan index.
	byIndex map[int]*render.RenderedTask
	// sent — what each host was sent, in order: a row's local task_idx indexes it.
	sent map[string][]*render.RenderedTask
	// settled — what the Keeper decided before sending; a host it failed carries
	// the cause first in its records.
	settled map[string]*settledHost
	// failedAt — the plan indices each host reported FAILED or TIMED_OUT, from the
	// audit log; executed — those it reported with any status but SKIPPED, read only
	// for a step carrying a task of a rescue unit ([stepJudge.carriesUnit]). nil
	// without an audit reader, or while nothing at the step calls for a replay: the
	// row's first failure then stands alone.
	failedAt map[string]map[int]bool
	executed map[string]map[int]bool
	// recorded — the FAILED records [Runner.recordUnreported] wrote at this barrier
	// for a host that stopped reporting: a failure tied to no task, which takes the
	// barrier owner's radius.
	recorded map[string]map[int]bool
	// replayed — the hosts whose row at this step the barrier has replayed, one wave
	// at a time; decided — the hosts on which it has decided the units ending at this
	// step ([runFailures.barrier]); swept — the step's last barrier ran.
	replayed map[string]bool
	decided  map[string]bool
	swept    bool
}

func newStepJudge(spec RunSpec, passage int, tasks []*render.RenderedTask, sent map[string][]*render.RenderedTask, settled map[string]*settledHost) *stepJudge {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	return &stepJudge{
		step:     passage,
		owner:    stepOwner(spec, passage),
		byIndex:  byIndex,
		sent:     sent,
		settled:  settled,
		recorded: map[string]map[int]bool{},
		replayed: map[string]bool{},
		decided:  map[string]bool{},
	}
}

// hasFailureToJudge reports whether the step holds a failed row not replayed yet.
func (j *stepJudge) hasFailureToJudge(statuses []applyrun.HostStatus) bool {
	for _, hs := range statuses {
		if hs.Passage == j.step && !j.replayed[hs.SID] && rowFailed(hs.Status) {
			return true
		}
	}
	return false
}

// carriesUnit reports whether a task of the step belongs to a rescue unit: the replay
// reads whether a task executed only for such a task ([config.RescueRule.Ran]), so a
// plan holding no unit reads no executed task.
func (j *stepJudge) carriesUnit() bool {
	for _, t := range j.byIndex {
		if t.RescueUnit != "" {
			return true
		}
	}
	return false
}

func rowFailed(s applyrun.Status) bool {
	return s == applyrun.StatusFailed || s == applyrun.StatusOrphaned
}

// keeperFailed is what the Keeper settled for host sid when it failed the host before
// sending anything, the cause first in its records; nil for any other host.
func (j *stepJudge) keeperFailed(sid string) *settledHost {
	if sh := j.settled[sid]; sh != nil && sh.failed && len(sh.records) > 0 {
		return sh
	}
	return nil
}

// keeperFailedGroup reports whether the Keeper failed host sid before sending it a
// group rolled as one: such a host is judged at once, not held (rule 2).
func (j *stepJudge) keeperFailedGroup(sid string) bool {
	sh := j.keeperFailed(sid)
	return sh != nil && len(sh.records) > 1 && sh.records[0].task.Grouped
}

// verdict is the entry of a failed row whose host is judged at once, by open — its
// sources at the step no unit handled. A host the Keeper failed before sending is
// judged by the first of them among its records — the cause, unless a task of a unit
// that fired there caused it — and the FAILED records it wrote for the rest of the
// message are not failures of their own. A host the Soul failed is judged by every one of them: one
// under radius `scenario` ends the run. A failure tied to no task — a record written
// for a host that stopped reporting included — takes the barrier owner's radius.
func (j *stepJudge) verdict(hs applyrun.HostStatus, open []*failedSource) failureEntry {
	e := failureEntry{step: j.step, sid: hs.SID, reason: failureReason(hs)}
	if hs.Status == applyrun.StatusCancelled {
		e.radius = radiusCancelled
		return e
	}
	failed := j.judgedTasks(hs, open)
	e.radius = *j.owner.OnFailure
	if len(failed) == 0 {
		return e
	}
	// The entry names the first failure, unless a later one ends the run.
	e.task, e.radius = failed[0].Name, j.radiusOf(failed[0])
	for _, t := range failed {
		if j.radiusOf(t) != config.OnFailureHost {
			e.task, e.radius = t.Name, j.radiusOf(t)
			break
		}
	}
	return e
}

// judgedTasks are the tasks [stepJudge.verdict] judges a host by, in that order.
func (j *stepJudge) judgedTasks(hs applyrun.HostStatus, open []*failedSource) []*render.RenderedTask {
	member := map[int]bool{}
	for _, s := range open {
		for _, m := range s.members {
			if !j.recorded[hs.SID][m] {
				member[m] = true
			}
		}
	}
	if sh := j.keeperFailed(hs.SID); sh != nil {
		for _, rec := range sh.records {
			if member[rec.task.Index] {
				return []*render.RenderedTask{rec.task}
			}
		}
		return nil
	}
	var failed []*render.RenderedTask
	first := -1
	if t := j.reportedFailure(hs); t != nil && member[t.Index] {
		failed, first = append(failed, t), t.Index
	}
	for _, idx := range slices.Sorted(maps.Keys(member)) {
		if t := j.byIndex[idx]; t != nil && idx != first {
			failed = append(failed, t)
		}
	}
	return failed
}

// reportedFailure is the task a row names as its first failure.
func (j *stepJudge) reportedFailure(hs applyrun.HostStatus) *render.RenderedTask {
	if hs.FailedPlanIndex != nil {
		return j.byIndex[*hs.FailedPlanIndex]
	}
	if hs.TaskIdx != nil {
		if sent := j.sent[hs.SID]; *hs.TaskIdx >= 0 && *hs.TaskIdx < len(sent) {
			return sent[*hs.TaskIdx]
		}
	}
	return nil
}

func (j *stepJudge) radiusOf(t *render.RenderedTask) string {
	if t.OnFailure == "" {
		return *j.owner.OnFailure
	}
	return t.OnFailure
}

// failedTasksByHost narrows the run's FAILED/TIMED_OUT task keys to the given hosts.
func failedTasksByHost(keys map[auditpg.ChangedTaskKey]struct{}) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for k := range keys {
		if out[k.SID] == nil {
			out[k.SID] = map[int]bool{}
		}
		out[k.SID][k.PlanIndex] = true
	}
	return out
}

// reachedHosts counts the hosts the run has targeted with at least one task: a host
// with a row, whether it was sent the task, skipped it on the Keeper's word or failed
// before the send. Keeper and sentinel rows aside, and a `no_match` row — the
// work-queue path's record of a host no task targeted.
func reachedHosts(statuses []applyrun.HostStatus) int {
	seen := map[string]bool{}
	for _, hs := range statuses {
		if hs.SID == render.KeeperTargetSID || hs.SID == render.RunSentinelSID || hs.Status == applyrun.StatusNoMatch {
			continue
		}
		seen[hs.SID] = true
	}
	return len(seen)
}

// waitStep reads the run's rows, when [Runner.waitBarrier] would, until wantHosts
// rows of step passage are terminal and returns the run's rows. Unlike
// [Runner.waitBarrier] a failed row does not end the wait: the hosts still working
// finish, and every failure they report is judged. A row Cancel and the
// run's context end it at once; the lock's Cancel waits while only a dispatched row is
// open. watch fails a host whose stream stays lost ([stepWatch.check]).
func (r *Runner) waitStep(ctx context.Context, applyID string, passage, wantHosts int, log *slog.Logger, watch *stepWatch) ([]applyrun.HostStatus, error) {
	wake, unwatch := r.deps.ApplyWake.Watch(applyID)
	defer unwatch()
	ticker := time.NewTicker(r.barrierPoll())
	defer ticker.Stop()
	for {
		statuses, err := applyrun.SelectStatusesByApplyID(ctx, r.deps.DB, applyID)
		if err != nil {
			return nil, fmt.Errorf("scenario: barrier poll: %w", err)
		}
		// The applying lock's flag (ADR-027 amend (r)) is deferred while only a
		// dispatched row is open, as in [Runner.waitBarrierExcept].
		cancelled := cancelRequested(statuses)
		if !cancelled && !onlyDispatchedOpen(statuses) {
			var cerr error
			if cancelled, cerr = incarnation.RunCancelRequested(ctx, r.deps.DB, applyID); cerr != nil {
				return nil, fmt.Errorf("scenario: barrier poll: %w", cerr)
			}
		}
		if cancelled {
			log.Info("scenario: barrier - received cluster-wide Cancel (cancel_requested), run is being cancelled")
			return nil, fmt.Errorf("scenario: barrier interrupted: %w", errCancelRequested)
		}
		terminal := 0
		for _, hs := range statuses {
			if hs.Passage == passage && hs.SID != render.KeeperTargetSID && rowTerminal(hs.Status) {
				terminal++
			}
		}
		if terminal >= wantHosts {
			return statuses, nil
		}
		watch.check(ctx, statuses)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("scenario: barrier interrupted: %w", ctx.Err())
		case <-wake:
		case <-ticker.C:
		}
	}
}

// withStepTargets is plans with each task that already ran keeping the plan of its
// own step: a later step re-renders it against a roster without the hosts dropped
// since, and the changed-task rollup reads its targets.
func withStepTargets(plans []render.DispatchPlan, ran map[int]render.DispatchPlan) []render.DispatchPlan {
	if len(ran) == 0 {
		return plans
	}
	out := make([]render.DispatchPlan, len(plans))
	for i, pl := range plans {
		if own, ok := ran[pl.TaskIndex]; ok {
			pl = own
		}
		out[i] = pl
	}
	return out
}

// failAfterSent ends a step that failed to dispatch once the wantHosts rows it
// already has are terminal — a host already sent finishes, or the abort would close
// a row it is applying. The run ends with cause, joined with a cluster-wide Cancel
// that arrived meanwhile, so the abort still reads the Cancel; any other wait error
// is only logged.
func (r *Runner) failAfterSent(ctx context.Context, spec RunSpec, log *slog.Logger, passage, wantHosts int, cause error, watch *stepWatch) error {
	if wantHosts == 0 {
		return cause
	}
	if _, werr := r.waitStep(ctx, spec.ApplyID, passage, wantHosts, log, watch); werr != nil {
		if errors.Is(werr, errCancelRequested) {
			return errors.Join(cause, werr)
		}
		log.Warn("scenario: waiting for the hosts sent before a step failed did not finish",
			slog.Int("passage", passage), slog.Any("error", werr))
	}
	return cause
}

// waitAndJudge is one wave's barrier under linear dispatch: every one of wantHosts
// rows of the step terminal, then [Runner.judgeWave]. last — no wave of the step comes
// after this one.
func (r *Runner) waitAndJudge(ctx context.Context, spec RunSpec, log *slog.Logger, j *stepJudge, wantHosts int, fails *runFailures, roster []*topology.HostFacts, watch *stepWatch, last bool) error {
	statuses, err := r.waitStep(ctx, spec.ApplyID, j.step, wantHosts, log, watch)
	if err != nil {
		return err
	}
	return r.judgeWave(ctx, spec, log, j, statuses, fails, roster, last)
}

// sweepStep is the barrier of a step after its last wave, for a step whose last wave
// sent nothing or that sends nothing at all: the units the step decides are decided on
// every host, and the failures held for them judged ([runFailures.barrier]). A no-op
// once the step's last barrier ran, and in a plan holding no scenario rescue unit,
// where nothing is held and no unit is decided across steps.
func (r *Runner) sweepStep(ctx context.Context, spec RunSpec, log *slog.Logger, j *stepJudge, fails *runFailures, roster []*topology.HostFacts) error {
	if j.swept || !fails.holdsUnits() {
		return nil
	}
	statuses, err := applyrun.SelectStatusesByApplyID(ctx, r.deps.DB, spec.ApplyID)
	if err != nil {
		return fmt.Errorf("scenario: step %d: barrier read: %w", j.step, err)
	}
	return r.judgeWave(ctx, spec, log, j, statuses, fails, roster, true)
}

// judgeWave judges what the hosts of step passage reported so far and applies the
// policy: the error ending the run, or nil with the dropped hosts recorded in fails and
// the audit log. What each host reported is replayed into the failure state
// ([runFailures.barrier]); last — the step's last barrier, which decides its units on
// the hosts no wave reached as well. roster is the step's hosts, the held and the
// dropped among them.
func (r *Runner) judgeWave(ctx context.Context, spec RunSpec, log *slog.Logger, j *stepJudge, statuses []applyrun.HostStatus, fails *runFailures, roster []*topology.HostFacts, last bool) error {
	if r.deps.AuditReader != nil && (j.hasFailureToJudge(statuses) || fails.anyHeld() || fails.failed() && fails.decidesAt(j.step)) {
		keys, err := r.deps.AuditReader.SelectFailedTaskKeys(ctx, spec.ApplyID)
		if err != nil {
			return fmt.Errorf("scenario: step %d: reading the failed tasks: %w", j.step, err)
		}
		j.failedAt = failedTasksByHost(keys)
		if j.carriesUnit() {
			executed, err := r.deps.AuditReader.SelectExecutedTaskKeys(ctx, spec.ApplyID)
			if err != nil {
				return fmt.Errorf("scenario: step %d: reading the executed tasks: %w", j.step, err)
			}
			j.executed = failedTasksByHost(executed)
		}
	}
	for sid, idxs := range r.recordUnreported(ctx, spec, log, j, statuses) {
		j.recorded[sid] = idxs
	}
	verdicts := fails.barrier(j, statuses, roster, last)
	dropped, err := fails.settle(j.step, verdicts, j.owner, reachedHosts(statuses), roster)
	if err != nil {
		return err
	}
	for _, e := range dropped {
		r.writeHostDropped(ctx, spec, e, log)
		log.Warn("scenario: host dropped from the run under on_failure: host - it receives no later ordinary task",
			slog.String("sid", e.sid), slog.Int("step", e.step), slog.String("task", e.task), slog.String("reason", e.reason))
	}
	return nil
}

// recordUnreported writes, for every host whose failure is tied to no task — its step
// never reached it, or it stopped reporting partway — what a Soul's fail-stop writes
// for the rest of a message (settings.md §4, user 2026-10-08): the first task it did
// not report FAILED, where it stopped, and every later one SKIPPED, not taken. Such a
// host's row is failed or orphaned, names no failed task, and the audit log holds no
// FAILED fact of the step for it; the Keeper did not fail it before sending. A task
// already recorded keeps its record, and a task the Keeper decided to skip on the host
// is SKIPPED. An applier's resolution reads these records. It runs before the
// barrier's replay and returns, by host, only the FAILED records whose audit write was
// confirmed: the replay reads them as the host's failure, and a source made of one is
// judged by the barrier owner's radius (rule 4) — held, when a unit of it is ahead
// (rule 2). A claim's requisite gate reads the audit log instead
// ([ClaimRunner.stepGate]): a write that commits but reports an error is dropped here
// while a claim still sees it, an accepted gap.
// Without an audit reader nothing reads them, and nothing says what was reported.
func (r *Runner) recordUnreported(ctx context.Context, spec RunSpec, log *slog.Logger, j *stepJudge, statuses []applyrun.HostStatus) map[string]map[int]bool {
	if r.deps.AuditReader == nil {
		return nil
	}
	var stopped []failureEntry
	for _, hs := range statuses {
		if hs.Passage != j.step || hs.SID == render.KeeperTargetSID || j.replayed[hs.SID] || !rowFailed(hs.Status) {
			continue
		}
		if j.keeperFailed(hs.SID) != nil || j.reportedFailure(hs) != nil || j.failedAtStep(hs.SID) || len(j.message(hs.SID)) == 0 {
			continue
		}
		stopped = append(stopped, failureEntry{step: j.step, sid: hs.SID, reason: failureReason(hs)})
	}
	if len(stopped) == 0 {
		return nil
	}
	reported, err := r.deps.AuditReader.SelectReportedTaskKeys(ctx, spec.ApplyID)
	if err != nil {
		log.Warn("scenario: reading the reported tasks failed - no record is written for the hosts that stopped reporting",
			slog.Int("step", j.step), slog.Any("error", err))
		return nil
	}
	sink := applysink.New(applysink.Deps{
		DB:     r.applyDB,
		Audit:  r.deps.Audit,
		Bus:    r.deps.ApplyBus,
		Source: audit.SourceKeeperInternal,
		Logger: log,
	})
	failed := map[string]map[int]bool{}
	for _, v := range stopped {
		for _, rec := range unreported(j.message(v.sid), v, reported) {
			written := sink.JudgedTaskEvent(ctx, v.sid, rec.event(spec.ApplyID, j.step))
			if written && rec.status == keeperv1.TaskStatus_TASK_STATUS_FAILED {
				if failed[v.sid] == nil {
					failed[v.sid] = map[int]bool{}
				}
				failed[v.sid][rec.task.Index] = true
			}
		}
	}
	return failed
}

// failedAtStep reports whether the audit log holds a FAILED fact of one of the step's
// tasks for host sid.
func (j *stepJudge) failedAtStep(sid string) bool {
	for idx := range j.failedAt[sid] {
		if j.byIndex[idx] != nil {
			return true
		}
	}
	return false
}

// message is what host sid was to be sent at this step, in the Soul's order: the
// tasks sent — a grouped child the Keeper skipped among them, sent `when: false`,
// marked SKIPPED — then the tasks the Keeper skipped outside the message. A record
// with no status is a task the host was to run.
func (j *stepJudge) message(sid string) []keeperRecord {
	sent := j.sent[sid]
	out := make([]keeperRecord, 0, len(sent))
	for i, t := range sent {
		rec := keeperRecord{task: t, localIdx: i}
		if t.FlowBySID[sid].Skip {
			rec.status = keeperv1.TaskStatus_TASK_STATUS_SKIPPED
		}
		out = append(out, rec)
	}
	if sh := j.settled[sid]; sh != nil && !sh.failed {
		out = append(out, numberedAfter(sh.records, len(sent))...)
	}
	return out
}

// unreported is the records message leaves for host v.sid once it stopped: those not
// in reported, the first task it was to run FAILED with the row's reason and the rest
// SKIPPED.
func unreported(message []keeperRecord, v failureEntry, reported map[auditpg.ChangedTaskKey]struct{}) []keeperRecord {
	var out []keeperRecord
	stopped := false
	for _, rec := range message {
		if _, ok := reported[auditpg.ChangedTaskKey{SID: v.sid, PlanIndex: rec.task.Index}]; ok {
			continue
		}
		if rec.status == keeperv1.TaskStatus_TASK_STATUS_UNSPECIFIED {
			rec.status = keeperv1.TaskStatus_TASK_STATUS_SKIPPED
			if !stopped {
				stopped = true
				rec.status = keeperv1.TaskStatus_TASK_STATUS_FAILED
				rec.failure = &render.FlowFailure{
					Code:    stopCode(v.reason),
					Message: "the host stopped reporting at this task: " + v.reason,
				}
			}
		}
		out = append(out, rec)
	}
	return out
}

// stopCodes are the reason codes a row's summary opens with for a failure tied to no
// task, written by the claim, the send wait, the push branch and the WardRoster sweep.
var stopCodes = map[string]bool{
	soulUnavailable:                 true,
	"send_apply_failed":             true,
	"recipe_missing":                true,
	reasonPushTransportFailed:       true,
	reasonProviderNotRouted:         true,
	reasonSoulPassageUnsupported:    true,
	string(applyrun.StatusOrphaned): true,
}

// stopCode is the reason code heading a row's summary, or "" for a summary that opens
// with none — a claim's render error, say.
func stopCode(reason string) string {
	head, _, _ := strings.Cut(reason, ":")
	if stopCodes[head] {
		return head
	}
	return ""
}

// writeHostDropped writes the audit fact of a drop: which host left at which step,
// at which task and why. Best-effort, as every audit write of the run.
func (r *Runner) writeHostDropped(ctx context.Context, spec RunSpec, e failureEntry, log *slog.Logger) {
	if r.deps.Audit == nil {
		return
	}
	payload := map[string]any{
		"sid":         e.sid,
		"apply_id":    spec.ApplyID,
		"incarnation": spec.IncarnationName,
		"scenario":    spec.ScenarioName,
		"step":        e.step,
		"reason":      e.reason,
	}
	if e.task != "" {
		payload["task"] = e.task
	}
	if err := r.deps.Audit.Write(ctx, &audit.Event{
		EventType:     audit.EventApplyHostDropped,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: spec.ApplyID,
		Payload:       audit.MaskSecrets(payload),
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		log.Warn("scenario: writing audit apply.host_dropped failed", slog.String("sid", e.sid), slog.Any("error", err))
	}
}

// writeNotReached records SKIPPED, on every host the run dropped or holds, each task of
// the step that would have reached it — what a Soul's fail-stop records for the rest
// of its message (rule 3). Audit only: the host has no row at this step, and a register
// it never produced is not written for it; a held host that returns reads the one
// synthesized for it instead ([runFailures.recordMissed]).
func (r *Runner) writeNotReached(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, tasks []*render.RenderedTask, plans []render.DispatchPlan) {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	perHost := map[string][]*render.RenderedTask{}
	for _, p := range plans {
		t := byIndex[p.TaskIndex]
		if t == nil {
			continue
		}
		for _, sid := range p.NotReached {
			perHost[sid] = append(perHost[sid], t)
		}
	}
	if len(perHost) == 0 {
		return
	}
	sink := applysink.New(applysink.Deps{
		DB:     r.applyDB,
		Audit:  r.deps.Audit,
		Bus:    r.deps.ApplyBus,
		Source: audit.SourceKeeperInternal,
		Logger: log,
	})
	sids := make([]string, 0, len(perHost))
	for sid := range perHost {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	for _, sid := range sids {
		for i, t := range perHost[sid] {
			sink.TaskEvent(ctx, sid, &keeperv1.TaskEvent{
				ApplyId:   spec.ApplyID,
				TaskIdx:   int32(i),
				PlanIndex: int32(t.Index),
				Status:    keeperv1.TaskStatus_TASK_STATUS_SKIPPED,
				Passage:   int32(passage),
			})
		}
	}
}

// finishPartial ends a run that reached its end without the hosts it dropped:
// their rows are marked so the run reads `partial_failed`, and the rows a rescue
// handled so they read as no failure, the incarnation goes error_locked with the
// state its captures wrote, and run_completed says partial_failed. Returns whether
// this instance recorded the terminal.
func (r *Runner) finishPartial(ctx context.Context, spec RunSpec, fails *runFailures, sealedPaths map[string]bool, log *slog.Logger) (finalized bool, cause error) {
	fails.close()
	r.markHandled(ctx, spec, fails.handledRows(), log)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	drops := fails.drops()
	for _, sid := range drops {
		for _, step := range fails.droppedRows(sid) {
			if err := applyrun.MarkDropped(wctx, r.deps.DB, spec.ApplyID, sid, step); err != nil {
				log.Error("scenario: marking a dropped host's row failed - the run reads failed, not partial_failed",
					slog.String("sid", sid), slog.Int("step", step), slog.Any("error", err))
			}
		}
	}
	cause = fails.errorOf(fmt.Sprintf("the run finished without %d dropped host(s)", len(drops)))
	log.Warn("scenario: run finished without the hosts it dropped - incarnation locked",
		slog.Int("dropped", len(drops)), slog.String("error", maskErrText(cause, sealedPaths)))
	return r.lockIncarnation(ctx, spec, failureStatus(spec.TerminalMode), reasonPartialFailed, cause, runFailureOf(reasonPartialFailed, -1, cause), sealedPaths, log), cause
}

// markHandled marks rows `apply_runs.handled` (rule 5), best-effort as the drop marks
// are: a row left unmarked reads as a failure of the run. A run that commits `ready`
// marks them in its commit's transaction instead ([Runner.commitSuccess]).
func (r *Runner) markHandled(ctx context.Context, spec RunSpec, rows []runRow, log *slog.Logger) {
	if len(rows) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, row := range rows {
		err := applyrun.MarkHandled(wctx, r.deps.DB, spec.ApplyID, row.sid, row.step)
		switch {
		case err == nil:
		case spec.TerminalMode == TerminalDestroy && errors.Is(err, applyrun.ErrApplyRunNotFound):
			// A teardown's rows go with its incarnation (ON DELETE CASCADE): one
			// destroyed meanwhile left no row to read as a failure.
			log.Warn("scenario: a row whose failures a rescue handled is gone - the teardown's rows went with its incarnation",
				slog.String("sid", row.sid), slog.Int("step", row.step), slog.Any("error", err))
		default:
			log.Error("scenario: marking a row whose failures a rescue handled failed - the run reads it as a failure",
				slog.String("sid", row.sid), slog.Int("step", row.step), slog.Any("error", err))
		}
	}
}

func rowTerminal(s applyrun.Status) bool {
	switch s {
	case applyrun.StatusSuccess, applyrun.StatusNoMatch, applyrun.StatusFailed, applyrun.StatusOrphaned, applyrun.StatusCancelled:
		return true
	}
	return false
}

// stepOwner is the merged settings of step passage's barrier owner; the defaults for
// a run that has none recorded.
func stepOwner(spec RunSpec, passage int) config.ScenarioSettings {
	if passage >= 0 && passage < len(spec.stepSettings) {
		return spec.stepSettings[passage]
	}
	return config.DefaultScenarioSettings()
}
