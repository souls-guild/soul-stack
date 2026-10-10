package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
}

func (e failureEntry) String() string {
	if e.radius == radiusCancelled {
		return fmt.Sprintf("step %d: host %s was cancelled: %s", e.step, e.sid, e.reason)
	}
	at := "with no task"
	if e.task != "" {
		at = fmt.Sprintf("at task %q", e.task)
	}
	return fmt.Sprintf("step %d: host %s failed %s [on_failure: %s]: %s", e.step, e.sid, at, e.radius, e.reason)
}

// runFailures is a linear run's failure list: append-only, closed when the run ends.
// Every failure the barriers judged is in it, including those of hosts that were still
// working when another host's failure ended the run.
type runFailures struct {
	entries []failureEntry
	closed  bool
	// dropped — the hosts the run dropped, by SID, with the roster facts they had
	// when they left: they stay in `soulprint.hosts`.
	dropped map[string]droppedHost
	// ended — a failure ended the run at the barrier of step endedAt, under endLead;
	// it goes on only to the rescues ahead (settings.md §4).
	ended   bool
	endedAt int
	endLead string
}

type droppedHost struct {
	entry failureEntry
	facts *topology.HostFacts
	// steps — every step whose failed row the host left: where it was dropped, and
	// each rescue that failed on it since.
	steps []int
}

// errRunEnded is a barrier's verdict that a failure ended the run; the failure list
// holds why ([runFailures.withEnd]).
var errRunEnded = errors.New("scenario: a failure ended the run")

// errCancelledAfterEnd is a Cancel judged at a rescue's barrier: it stops the run at
// once, and the failure list already names it.
var errCancelledAfterEnd = errors.New("scenario: a host was cancelled after a failure ended the run")

func newRunFailures() *runFailures {
	return &runFailures{dropped: map[string]droppedHost{}}
}

func (f *runFailures) append(entries ...failureEntry) error {
	if f.closed {
		return fmt.Errorf("scenario: %d failure(s) judged after the run's failure list was closed", len(entries))
	}
	f.entries = append(f.entries, entries...)
	return nil
}

func (f *runFailures) close() { f.closed = true }

// live is roster without the hosts the run dropped — what a step may target. A
// dropped host stays out even when a re-read roster still lists it.
func (f *runFailures) live(roster []*topology.HostFacts) []*topology.HostFacts {
	if f == nil || len(f.dropped) == 0 {
		return roster
	}
	out := make([]*topology.HostFacts, 0, len(roster))
	for _, h := range roster {
		if _, gone := f.dropped[h.SID]; !gone {
			out = append(out, h)
		}
	}
	return out
}

// droppedFacts are the dropped hosts for render, sorted by SID.
func (f *runFailures) droppedFacts() []*topology.HostFacts {
	if f == nil || len(f.dropped) == 0 {
		return nil
	}
	out := make([]*topology.HostFacts, 0, len(f.dropped))
	for _, d := range f.dropped {
		out = append(out, d.facts)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SID < out[j].SID })
	return out
}

// drops are the dropped hosts in SID order.
func (f *runFailures) drops() []droppedHost {
	out := make([]droppedHost, 0, len(f.dropped))
	for _, d := range f.dropped {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].entry.sid < out[j].entry.sid })
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
// end the run, or the entries whose hosts it drops. owner is the merged settings of
// the step's barrier owner, reached the hosts the run has targeted so far —
// `tolerate`'s denominator (user decision, NIM-948) — and roster the step's hosts,
// which hold the facts a dropped host keeps in `soulprint.hosts`. `tolerate` is
// checked at every barrier, one with no new failure included: a step may tighten it
// (settings.md §2.1). Drops are committed only when the run goes on.
//
// A host already dropped is out of the count: it is at this barrier for a rescue, and
// a rescue that fails on it under `host` only records that it left again. Once the run
// has ended a rescue's failure ends nothing more: it is one more entry, and under `host`
// its host's drop is recorded as well. Only a Cancel still ends the run at once.
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
		if _, out := f.dropped[v.sid]; !out {
			fresh++
		}
	}
	if fresh > 0 && len(f.live(roster))-fresh <= 0 {
		return nil, f.end(step, fmt.Sprintf("step %d: no host is left in the run", step))
	}
	if dropped := len(f.dropped) + fresh; owner.Tolerate != nil && dropped > 0 {
		if owner.Tolerate.Exceeded(dropped, reached) {
			return nil, f.end(step, fmt.Sprintf("step %d: %d hosts dropped under on_failure: host, more than tolerate allows (%d of %d)",
				step, dropped, owner.Tolerate.Limit(reached), reached))
		}
	}
	return f.drop(verdicts, roster), nil
}

// drop records the hosts verdicts drop under radius `host` and returns their entries.
// A host already out keeps the facts it left with.
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
		d, known := f.dropped[v.sid]
		if !known {
			facts := bySID[v.sid]
			if facts == nil {
				facts = &topology.HostFacts{SID: v.sid}
			}
			d = droppedHost{entry: v, facts: facts}
		}
		d.steps = append(d.steps, v.step)
		f.dropped[v.sid] = d
		out = append(out, v)
	}
	return out
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
	// audit log. nil without an audit reader: the row's first failure then stands
	// alone.
	failedAt map[string]map[int]bool
	// judged — hosts already judged at this step, one wave at a time.
	judged map[string]bool
}

func newStepJudge(spec RunSpec, passage int, tasks []*render.RenderedTask, sent map[string][]*render.RenderedTask, settled map[string]*settledHost) *stepJudge {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	return &stepJudge{
		step:    passage,
		owner:   stepOwner(spec, passage),
		byIndex: byIndex,
		sent:    sent,
		settled: settled,
		judged:  map[string]bool{},
	}
}

// hasFailureToJudge reports whether the step holds a failed row not judged yet — the
// only case the audit log is read for.
func (j *stepJudge) hasFailureToJudge(statuses []applyrun.HostStatus) bool {
	for _, hs := range statuses {
		if hs.Passage == j.step && !j.judged[hs.SID] && (hs.Status == applyrun.StatusFailed || hs.Status == applyrun.StatusOrphaned) {
			return true
		}
	}
	return false
}

// verdicts judges every failed or cancelled row of the step not judged yet.
func (j *stepJudge) verdicts(statuses []applyrun.HostStatus) []failureEntry {
	var out []failureEntry
	for _, hs := range statuses {
		if hs.Passage != j.step || hs.SID == render.KeeperTargetSID || j.judged[hs.SID] {
			continue
		}
		switch hs.Status {
		case applyrun.StatusFailed, applyrun.StatusOrphaned, applyrun.StatusCancelled:
		default:
			continue
		}
		j.judged[hs.SID] = true
		out = append(out, j.verdict(hs))
	}
	return out
}

// verdict is one failed row's entry. A host the Keeper failed before sending is judged
// by the cause alone — the FAILED records it wrote for the rest of the message are not
// failures of their own. A host the Soul failed is judged by every task it reported
// failed: one of them under radius `scenario` ends the run. A failure tied to no task
// takes the barrier owner's radius.
func (j *stepJudge) verdict(hs applyrun.HostStatus) failureEntry {
	e := failureEntry{step: j.step, sid: hs.SID, reason: failureReason(hs)}
	if hs.Status == applyrun.StatusCancelled {
		e.radius = radiusCancelled
		return e
	}
	var failed []*render.RenderedTask
	if sh := j.settled[hs.SID]; sh != nil && sh.failed && len(sh.records) > 0 {
		failed = append(failed, sh.records[0].task)
	} else {
		if t := j.reportedFailure(hs); t != nil {
			failed = append(failed, t)
		}
		var more []*render.RenderedTask
		for idx := range j.failedAt[hs.SID] {
			if t := j.byIndex[idx]; t != nil {
				more = append(more, t)
			}
		}
		sort.Slice(more, func(a, b int) bool { return more[a].Index < more[b].Index })
		failed = append(failed, more...)
	}
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
// rows of the step terminal, then [Runner.judgeWave].
func (r *Runner) waitAndJudge(ctx context.Context, spec RunSpec, log *slog.Logger, j *stepJudge, wantHosts int, fails *runFailures, roster []*topology.HostFacts, watch *stepWatch) error {
	statuses, err := r.waitStep(ctx, spec.ApplyID, j.step, wantHosts, log, watch)
	if err != nil {
		return err
	}
	return r.judgeWave(ctx, spec, log, j, statuses, fails, roster)
}

// judgeWave judges what the hosts of step passage reported so far and applies the
// policy: the error ending the run, or nil with the dropped hosts recorded in fails and
// the audit log. roster is the step's roster.
func (r *Runner) judgeWave(ctx context.Context, spec RunSpec, log *slog.Logger, j *stepJudge, statuses []applyrun.HostStatus, fails *runFailures, roster []*topology.HostFacts) error {
	if r.deps.AuditReader != nil && j.hasFailureToJudge(statuses) {
		keys, err := r.deps.AuditReader.SelectFailedTaskKeys(ctx, spec.ApplyID)
		if err != nil {
			return fmt.Errorf("scenario: step %d: reading the failed tasks: %w", j.step, err)
		}
		j.failedAt = failedTasksByHost(keys)
	}
	verdicts := j.verdicts(statuses)
	r.recordUnreported(ctx, spec, log, j, verdicts)
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
// not report FAILED, where it stopped, and every later one SKIPPED, not taken. A task
// already recorded keeps its record, and a task the Keeper decided to skip on the host
// is SKIPPED. Rescue routing and an applier's resolution read these records; the
// barrier does not judge them again — the host was judged by the owner's radius.
// Without an audit reader nothing reads them, and nothing says what was reported.
func (r *Runner) recordUnreported(ctx context.Context, spec RunSpec, log *slog.Logger, j *stepJudge, verdicts []failureEntry) {
	if r.deps.AuditReader == nil {
		return
	}
	var stopped []failureEntry
	for _, v := range verdicts {
		if v.task == "" && v.radius != radiusCancelled && len(j.message(v.sid)) > 0 {
			stopped = append(stopped, v)
		}
	}
	if len(stopped) == 0 {
		return
	}
	reported, err := r.deps.AuditReader.SelectReportedTaskKeys(ctx, spec.ApplyID)
	if err != nil {
		log.Warn("scenario: reading the reported tasks failed - no record is written for the hosts that stopped reporting",
			slog.Int("step", j.step), slog.Any("error", err))
		return
	}
	sink := applysink.New(applysink.Deps{
		DB:     r.applyDB,
		Audit:  r.deps.Audit,
		Bus:    r.deps.ApplyBus,
		Source: audit.SourceKeeperInternal,
		Logger: log,
	})
	for _, v := range stopped {
		for _, rec := range unreported(j.message(v.sid), v, reported) {
			sink.JudgedTaskEvent(ctx, v.sid, rec.event(spec.ApplyID, j.step))
		}
	}
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

// writeNotReached records SKIPPED, on every host the run dropped, each task of the step
// that would have reached it — what a Soul's fail-stop records for the rest of its
// message. Audit only: the host has no row at this step, and a register it never
// produced is not written for it.
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
// their rows are marked so the run reads `partial_failed`, the incarnation goes
// error_locked with the state its captures wrote, and run_completed says
// partial_failed. Returns whether this instance recorded the terminal.
func (r *Runner) finishPartial(ctx context.Context, spec RunSpec, fails *runFailures, sealedPaths map[string]bool, log *slog.Logger) (finalized bool, cause error) {
	fails.close()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, d := range fails.drops() {
		for _, step := range d.steps {
			if err := applyrun.MarkDropped(wctx, r.deps.DB, spec.ApplyID, d.entry.sid, step); err != nil {
				log.Error("scenario: marking a dropped host's row failed - the run reads failed, not partial_failed",
					slog.String("sid", d.entry.sid), slog.Int("step", step), slog.Any("error", err))
			}
		}
	}
	cause = fails.errorOf(fmt.Sprintf("the run finished without %d dropped host(s)", len(fails.dropped)))
	log.Warn("scenario: run finished without the hosts it dropped - incarnation locked",
		slog.Int("dropped", len(fails.dropped)), slog.String("error", maskErrText(cause, sealedPaths)))
	return r.lockIncarnation(ctx, spec, failureStatus(spec.TerminalMode), reasonPartialFailed, cause, sealedPaths, log), cause
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
