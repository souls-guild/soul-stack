package scenario

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/applysink"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Records the Keeper writes itself under linear dispatch (draft ADR NIM-907,
// decision 4). A host the Keeper decided does not run a task is never sent it, so
// the SKIPPED event and register the Soul would have written are written here — a
// later `register.<task>.changed` reads false from it instead of raising. A host
// whose task failed before dispatch is sent nothing; it gets a record per task of its
// slice ([failedHost]) and a failed `apply_runs` row, and the step fails once the
// hosts sent beside it in its wave have finished ([Runner.dispatchPassage]). Both go
// through the same [applysink.Sink] a Soul's events do, so the audit fact the
// cross-step requisite gate and the changed-task rollup read is there too.

// keeperRecord is one task's outcome on one host, written by the Keeper.
type keeperRecord struct {
	task *render.RenderedTask
	// localIdx — the task's position in the host's slice as grouped, before the Keeper
	// settled it, or, for a host that is sent a message, a position after the tasks it
	// is sent ([numberedAfter]).
	localIdx int
	status   keeperv1.TaskStatus
	failure  *render.FlowFailure
}

// settledHost is what the Keeper decided for one host at one step before sending
// anything: the records it writes, and whether the host failed. A host left with
// nothing to send is closed by the Keeper; otherwise the records are written when its
// row is inserted at its wave.
type settledHost struct {
	records []keeperRecord
	failed  bool
}

// taskVerdict is one task's fate on one host before anything is sent.
type taskVerdict struct {
	// task — as the host would be sent it: the requisite gate may have rewritten its
	// requisites.
	task    *render.RenderedTask
	pos     int
	skip    bool
	failure *render.FlowFailure
}

// settleHosts applies, host by host, the flow control the render settled
// ([render.RenderedTask.FlowBySID]) and the cross-step requisite gate, in the order
// the Soul evaluates a task (applyrunner.go): a `when:` that failed, a false `when:`,
// the requisites, and only then a post-module predicate — which a task that does not
// run never evaluates. As far as the Keeper can decide that order: inside a group, a
// `when:` reading a sibling's register and a requisite on a sibling are the Soul's to
// evaluate, so a grouped child whose post-module predicate cannot be rendered fails
// its host even where the Soul would have skipped it — the Keeper cannot send it —
// and a grouped child the gate drops is sent `when: false` without that open `when:`
// ever being evaluated, as the staged gate drops it today.
//
//   - a task failed on a host fails the whole message there: the host is sent nothing,
//     and every task it would have carried is recorded FAILED, the cause first so the
//     host's error_summary names it — except a task that would not have run there,
//     which is recorded SKIPPED;
//   - a task that does not run is left out of the host's slice and recorded SKIPPED —
//     unless it is a grouped child, which is sent as `when: false` so its siblings can
//     read the register the Soul writes for it.
//
// The gate used to write nothing for a host it dropped; under staged dispatch it still
// runs alone ([crossPassageGate.applyGate]) and records nothing.
func settleHosts(perHost map[string][]*render.RenderedTask, gate *crossPassageGate, passage int) (map[string][]*render.RenderedTask, map[string]*settledHost) {
	out := make(map[string][]*render.RenderedTask, len(perHost))
	settled := make(map[string]*settledHost)
	for sid, tasks := range perHost {
		verdicts := make([]taskVerdict, 0, len(tasks))
		cause := -1
		for pos, t := range tasks {
			v := gate.verdict(t, sid, passage, pos)
			if v.failure != nil && cause < 0 {
				cause = pos
			}
			verdicts = append(verdicts, v)
		}
		if cause >= 0 {
			settled[sid] = failedHost(verdicts, cause)
			continue
		}
		var (
			kept    []*render.RenderedTask
			records []keeperRecord
		)
		for _, v := range verdicts {
			switch {
			case !v.skip:
				kept = append(kept, v.task)
			case v.task.Grouped:
				kept = append(kept, skippedOn(v.task, sid))
			default:
				records = append(records, keeperRecord{task: v.task, localIdx: v.pos, status: keeperv1.TaskStatus_TASK_STATUS_SKIPPED})
			}
		}
		if len(records) > 0 {
			settled[sid] = &settledHost{records: records}
		}
		if len(kept) > 0 {
			out[sid] = kept
		}
	}
	return out, settled
}

// verdict settles one task on one host. A nil gate is a step with no requisite
// source before it.
func (g *crossPassageGate) verdict(t *render.RenderedTask, sid string, passage, pos int) taskVerdict {
	hf := t.FlowBySID[sid]
	v := taskVerdict{task: t, pos: pos}
	switch {
	case hf.Failure.WhenFailed():
		v.failure = hf.Failure
	case hf.Skip:
		v.skip = true
	default:
		gated, include := t, true
		if g != nil {
			gated, include = g.resolveTask(t, sid, passage)
		}
		switch {
		case !include:
			v.skip = true
		case hf.Failure != nil:
			v.failure = hf.Failure
		default:
			v.task = gated
		}
	}
	return v
}

func failedHost(verdicts []taskVerdict, cause int) *settledHost {
	why := verdicts[cause]
	records := []keeperRecord{{task: why.task, localIdx: why.pos, status: keeperv1.TaskStatus_TASK_STATUS_FAILED, failure: why.failure}}
	sibling := &render.FlowFailure{
		Predicate: why.failure.Predicate,
		Code:      why.failure.Code,
		Message:   fmt.Sprintf("task %q failed on this host before dispatch: %s", why.task.Name, why.failure.Message),
	}
	for i, v := range verdicts {
		switch {
		case i == cause:
		case v.skip:
			records = append(records, keeperRecord{task: v.task, localIdx: v.pos, status: keeperv1.TaskStatus_TASK_STATUS_SKIPPED})
		case v.failure != nil:
			records = append(records, keeperRecord{task: v.task, localIdx: v.pos, status: keeperv1.TaskStatus_TASK_STATUS_FAILED, failure: v.failure})
		default:
			records = append(records, keeperRecord{task: v.task, localIdx: v.pos, status: keeperv1.TaskStatus_TASK_STATUS_FAILED, failure: sibling})
		}
	}
	return &settledHost{records: records, failed: true}
}

// skippedOn is t as host sid is sent it once the Keeper decided it does not run
// there: a clone, because the *RenderedTask is shared across hosts, whose only flow
// entry is this host's skip, and with no requisites — they gate a task that runs, and
// one into an earlier step would only travel as a dangling index.
func skippedOn(t *render.RenderedTask, sid string) *render.RenderedTask {
	clone := *t
	clone.FlowBySID = map[string]render.HostFlow{sid: {Skip: true}}
	clone.OnChangesIdx, clone.OnFailIdx, clone.RequireIdx, clone.RequireAll = nil, nil, nil, false
	return &clone
}

// numberedAfter renumbers the records of a host that is sent a message after the
// tasks it is sent: the Soul numbers task_idx by what it received, and a skipped
// task's would-be position is a position the Soul reports another task at.
func numberedAfter(records []keeperRecord, sent int) []keeperRecord {
	out := make([]keeperRecord, len(records))
	for i, rec := range records {
		rec.localIdx = sent + i
		out[i] = rec
	}
	return out
}

// sendsIn is wave without the hosts the Keeper failed in it.
func sendsIn(wave []string, failed map[string]bool) []string {
	if len(failed) == 0 {
		return wave
	}
	out := make([]string, 0, len(wave))
	for _, sid := range wave {
		if !failed[sid] {
			out = append(out, sid)
		}
	}
	return out
}

// failedBeforeSend are the closed hosts the Keeper failed.
func failedBeforeSend(settled map[string]*settledHost, closed []string) map[string]bool {
	var out map[string]bool
	for _, sid := range closed {
		if settled[sid].failed {
			if out == nil {
				out = make(map[string]bool)
			}
			out[sid] = true
		}
	}
	return out
}

// closedHosts are the settled hosts left with nothing to send, sorted by SID.
func closedHosts(settled map[string]*settledHost, perHost map[string][]*render.RenderedTask) []string {
	var out []string
	for sid := range settled {
		if _, sends := perHost[sid]; !sends {
			out = append(out, sid)
		}
	}
	sort.Strings(out)
	return out
}

// closeSettledHost writes a closed host's step: its `apply_runs` row, its records and
// its terminal — success when every task was skipped, failed when one failed. The
// row is what the register's foreign key needs and what the barrier counts.
func (r *Runner) closeSettledHost(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, sid string, sh *settledHost) error {
	if err := applyrun.Insert(ctx, r.deps.DB, &applyrun.ApplyRun{
		ApplyID:         spec.ApplyID,
		SID:             sid,
		IncarnationName: spec.IncarnationName,
		Scenario:        spec.ScenarioName,
		Status:          applyrun.StatusRunning,
		StartedByAID:    startedByPtr(spec.StartedByAID),
		Passage:         passage,
		Input:           spec.inputSnapshot,
		// Nothing travelled to the host, so there is no soul version to record.
		KeeperVersion: r.deps.KeeperVersion,
	}); err != nil {
		return fmt.Errorf("scenario: insert apply_run (%s): %w", sid, err)
	}
	r.writeKeeperRecords(ctx, spec.ApplyID, log, passage, sid, sh.records)
	status := applyrun.StatusSuccess
	if sh.failed {
		status = applyrun.StatusFailed
	}
	// The failure reason is the cause's record, already on the row: UpdateStatus
	// COALESCEs, and a summary here would replace it.
	if err := applyrun.UpdateStatus(ctx, r.deps.DB, spec.ApplyID, sid, passage, status, nil); err != nil {
		return fmt.Errorf("scenario: close apply_run (%s): %w", sid, err)
	}
	log.Info("scenario: host settled on the Keeper - nothing sent",
		slog.String("sid", sid), slog.Int("passage", passage), slog.String("status", string(status)), slog.Int("records", len(sh.records)))
	return nil
}

func (r *Runner) writeKeeperRecords(ctx context.Context, applyID string, log *slog.Logger, passage int, sid string, records []keeperRecord) {
	writeKeeperRecords(ctx, applysink.Deps{
		DB:     r.applyDB,
		Audit:  r.deps.Audit,
		Bus:    r.deps.ApplyBus,
		Source: audit.SourceKeeperInternal,
		Logger: log,
	}, applyID, passage, sid, records)
}

// writeKeeperRecords writes the records through the sink a Soul's events go through.
// The host's `apply_runs` row for this step must already exist.
func writeKeeperRecords(ctx context.Context, deps applysink.Deps, applyID string, passage int, sid string, records []keeperRecord) {
	if len(records) == 0 {
		return
	}
	sink := applysink.New(deps)
	for _, rec := range records {
		sink.TaskEvent(ctx, sid, rec.event(applyID, passage))
	}
}

func (rec keeperRecord) event(applyID string, passage int) *keeperv1.TaskEvent {
	ev := &keeperv1.TaskEvent{
		ApplyId:      applyID,
		TaskIdx:      int32(rec.localIdx),
		PlanIndex:    int32(rec.task.Index),
		Status:       rec.status,
		Passage:      int32(passage),
		SecretOutput: rec.task.SecretOutput,
		RegisterData: keeperRecordRegister(rec.status),
	}
	if rec.failure != nil {
		ev.Error = &keeperv1.TaskError{Code: rec.failure.Code, Module: rec.task.Module, Message: rec.failure.Message}
	}
	return ev
}

// keeperRecordRegister is the register a Soul writes for a task it skipped or failed
// without running its module (applyrunner.go buildRegisterData with no module event).
func keeperRecordRegister(status keeperv1.TaskStatus) *structpb.Struct {
	return &structpb.Struct{Fields: map[string]*structpb.Value{
		"changed":   structpb.NewBoolValue(false),
		"failed":    structpb.NewBoolValue(status == keeperv1.TaskStatus_TASK_STATUS_FAILED),
		"timed_out": structpb.NewBoolValue(false),
		"skipped":   structpb.NewBoolValue(status == keeperv1.TaskStatus_TASK_STATUS_SKIPPED),
	}}
}
