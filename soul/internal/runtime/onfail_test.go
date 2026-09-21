package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestOnFail_NoFailures_Skips — normal run with no failures: the onfail task
// (onfail:[A]) is ALWAYS SKIPPED — its source is OK, no rescue needed.
func TestOnFail_NoFailures_Skips(t *testing.T) {
	var rescueCalled bool
	reg := mapRegistry{
		"core.exec":    changedModule(nil),           // A: OK (changed)
		"core.service": changedModule(&rescueCalled), // rescue via onfail:[A]
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-no-fail",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A"},
			{Name: "rescue", Module: "core.service.restarted", OnfailIdx: []int32{0}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rescueCalled {
		t.Errorf("rescue ran even though source A did not fail (onfail in a normal run = SKIPPED)")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("rescue status = %v, want SKIPPED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestOnFail_SourceFailed_RescueRuns — A fails → rescue B (onfail:[A]) RUNS,
// plain C (after A, no onfail) SKIPPED, RunResult FAILED (rescue doesn't cancel the failure).
func TestOnFail_SourceFailed_RescueRuns(t *testing.T) {
	var rescueCalled, plainCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),            // A: fails
		"core.service": changedModule(&rescueCalled), // B: rescue via onfail:[A]
		"core.cmd":     changedModule(&plainCalled),  // C: plain, after A
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-rescue",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A"},
			{Name: "rescue", Module: "core.service.restarted", OnfailIdx: []int32{0}},
			{Name: "plain", Module: "core.cmd.run"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rescueCalled {
		t.Errorf("rescue did not run even though source A failed")
	}
	if plainCalled {
		t.Errorf("plain C ran after A's failure (should be SKIPPED)")
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("A status = %v, want FAILED", got)
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("rescue status = %v, want CHANGED (it ran)", got)
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("plain status = %v, want SKIPPED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED (rescue does not cancel the run's failure)", sink.runResult.GetStatus())
	}
}

// TestOnFail_MultiSource_AnyFailed — onfail:[A,B]: if either fails → rescue runs
// (any semantics, mirrors onchanges). Here A is OK, B fails → rescue triggers.
func TestOnFail_MultiSource_AnyFailed(t *testing.T) {
	var rescueCalled bool
	reg := mapRegistry{
		"core.exec":    changedModule(nil),           // A: OK
		"core.file":    failedModule(nil),            // B: fails
		"core.service": changedModule(&rescueCalled), // rescue via onfail:[A,B]
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-multi",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A"},
			{Name: "B", Module: "core.file.present", Register: "B"},
			{Name: "rescue", Module: "core.service.restarted", OnfailIdx: []int32{0, 1}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rescueCalled {
		t.Errorf("rescue did not run even though one of the sources (B) failed (any semantics)")
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("rescue status = %v, want CHANGED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestOnFail_TimedOutSource_RescueRuns — source A times out (TIMED_OUT is a
// special case of failed) → rescue B (onfail:[A]) runs.
func TestOnFail_TimedOutSource_RescueRuns(t *testing.T) {
	var rescueCalled bool
	slowModule := &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			<-stream.Context().Done()
			return stream.Context().Err()
		},
	}
	reg := mapRegistry{
		"core.exec":    slowModule,                   // A: TIMED_OUT
		"core.service": changedModule(&rescueCalled), // rescue via onfail:[A]
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-timeout",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A", Timeout: "10ms"},
			{Name: "rescue", Module: "core.service.restarted", OnfailIdx: []int32{0}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT {
		t.Fatalf("A status = %v, want TIMED_OUT", got)
	}
	if !rescueCalled {
		t.Errorf("rescue did not run for a TIMED_OUT source (timed_out is a special case of failed)")
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestOnFail_IgnoredErrorSource_Skips — source A fails in the module, but
// failed_when:false (ignore_errors) makes it OK → onfail does NOT fire (source
// is OK, not failed), rescue SKIPPED, run SUCCESS.
func TestOnFail_IgnoredErrorSource_Skips(t *testing.T) {
	var rescueCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),            // A: fails in the module, but failed_when:false suppresses it
		"core.service": changedModule(&rescueCalled), // rescue via onfail:[A]
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-ignored",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A", FailedWhen: "false"},
			{Name: "rescue", Module: "core.service.restarted", OnfailIdx: []int32{0}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Fatalf("A status = %v, want OK (failed_when:false swallowed the error)", got)
	}
	if rescueCalled {
		t.Errorf("rescue ran even though A's error was swallowed by ignore_errors (A is not failed)")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("rescue status = %v, want SKIPPED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult = %v, want SUCCESS (ignore_errors does not trigger fail-stop/onfail)", sink.runResult.GetStatus())
	}
}

// TestOnFail_MultipleFailures_RescueTailRuns — multiple failures: A fails (the
// first failed does NOT break the loop), plain B SKIPPED, rescue C (onfail:[A])
// runs, plain D after C is also SKIPPED. Confirms: the first failed doesn't
// break the loop, the onfail tail still runs, plain tasks after a failure are
// skipped.
func TestOnFail_MultipleFailures_RescueTailRuns(t *testing.T) {
	var bCalled, cCalled, dCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),       // A: fails
		"core.file":    changedModule(&bCalled), // B: plain after A → SKIPPED
		"core.service": changedModule(&cCalled), // C: rescue via onfail:[A] → runs
		"core.cmd":     changedModule(&dCalled), // D: plain after C → SKIPPED
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-tail",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A"},
			{Name: "B plain", Module: "core.file.present"},
			{Name: "C rescue", Module: "core.service.restarted", OnfailIdx: []int32{0}},
			{Name: "D plain", Module: "core.cmd.run"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if bCalled {
		t.Errorf("B ran after A's failure (should be SKIPPED)")
	}
	if !cCalled {
		t.Errorf("C (rescue) did not run even though A failed")
	}
	if dCalled {
		t.Errorf("D ran after the failure (should be SKIPPED)")
	}
	want := []keeperv1.TaskStatus{
		keeperv1.TaskStatus_TASK_STATUS_FAILED,
		keeperv1.TaskStatus_TASK_STATUS_SKIPPED,
		keeperv1.TaskStatus_TASK_STATUS_CHANGED,
		keeperv1.TaskStatus_TASK_STATUS_SKIPPED,
	}
	if len(sink.taskEvents) != len(want) {
		t.Fatalf("taskEvents = %d, want %d", len(sink.taskEvents), len(want))
	}
	for i, w := range want {
		if got := sink.taskEvents[i].GetStatus(); got != w {
			t.Errorf("taskEvents[%d] status = %v, want %v", i, got, w)
		}
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestOnFail_CancelOverFailedRun_Cancelled — the central "not a false SUCCESS"
// case: A fails (runFailed), rescue B (onfail:[A]) runs, and while sending its
// TaskEvent a CancelApply arrives (onTask hook, as in TestRun_CancelBetweenTasks).
// The next iteration sees runCtx.Err() != nil → cancel-break: RunResult CANCELLED,
// NOT FAILED and NOT hung. Cancel interrupts the loop unconditionally, overriding
// the already-recorded FAILED (applyrunner.go: the cancel check at the start of
// each iteration overwrites runStatus). The trailing plain task exists just to
// give the loop one more iteration — that's where the break fires (it never runs
// itself).
func TestOnFail_CancelOverFailedRun_Cancelled(t *testing.T) {
	var rescueCalled, tailCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),            // A: fails → runFailed
		"core.service": changedModule(&rescueCalled), // B: rescue via onfail:[A], runs
		"core.cmd":     changedModule(&tailCalled),   // tail: next iteration (never reached)
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	// CancelApply arrives during the rescue tail — trigger Cancel when sending
	// task B's TaskEvent (simulates CancelApply over EventStream, as in applyrunner_test).
	sink.onTask = func(ev *keeperv1.TaskEvent) {
		if ev.GetTaskIdx() == 1 {
			r.Cancel(ev.GetApplyId())
		}
	}

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-cancel-over-failed",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A"},
			{Name: "rescue", Module: "core.service.restarted", OnfailIdx: []int32{0}},
			{Name: "tail", Module: "core.cmd.run"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rescueCalled {
		t.Errorf("rescue did not run even though A failed (cancel arrived AFTER it started)")
	}
	if tailCalled {
		t.Errorf("tail task ran even though cancel broke the loop before it")
	}
	// Cancel fired on the tail iteration → tail's event never sent: exactly A + rescue.
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d, want 2 (A + rescue, tail never reached - cancel-break)", len(sink.taskEvents))
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("A status = %v, want FAILED", got)
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("rescue status = %v, want CHANGED (it ran before cancel)", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Errorf("runResult = %v, want CANCELLED (cancel overrides FAILED, NOT a false SUCCESS)", sink.runResult.GetStatus())
	}
}

// TestOnFail_RescueItselfFails_RunStaysFailed — a failing rescue doesn't "fix"
// the run: A fails → B(onfail:[A]) runs and ITSELF returns FAILED → B = FAILED,
// RunResult stays FAILED, the following plain C is SKIPPED. runFailed is already
// set by A's failure; B's failure doesn't change it (already terminally true).
func TestOnFail_RescueItselfFails_RunStaysFailed(t *testing.T) {
	var rescueCalled, plainCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),           // A: fails
		"core.service": failedModule(&rescueCalled), // B: rescue via onfail:[A], also fails
		"core.cmd":     changedModule(&plainCalled), // C: plain after B
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-rescue-fails",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A"},
			{Name: "B rescue", Module: "core.service.restarted", OnfailIdx: []int32{0}},
			{Name: "C plain", Module: "core.cmd.run"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rescueCalled {
		t.Errorf("rescue did not run even though source A failed")
	}
	if plainCalled {
		t.Errorf("plain C ran after the failure (should be SKIPPED)")
	}
	want := []keeperv1.TaskStatus{
		keeperv1.TaskStatus_TASK_STATUS_FAILED,  // A
		keeperv1.TaskStatus_TASK_STATUS_FAILED,  // B rescue — ran AND failed
		keeperv1.TaskStatus_TASK_STATUS_SKIPPED, // C
	}
	if len(sink.taskEvents) != len(want) {
		t.Fatalf("taskEvents = %d, want %d", len(sink.taskEvents), len(want))
	}
	for i, w := range want {
		if got := sink.taskEvents[i].GetStatus(); got != w {
			t.Errorf("taskEvents[%d] status = %v, want %v", i, got, w)
		}
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED (rescue's failure does not cancel the run's failure)", sink.runResult.GetStatus())
	}
}

// TestOnFail_RescueChain_RescueOfRescueRuns — onfail chain (rescue of a rescue):
// A fails → B(onfail:[A]) runs and ITSELF fails → C(onfail:[B]) runs (rescue of
// the rescue). Confirms composition: register.failed recorded on B's failure
// activates C's onfail, even though B is itself an onfail task.
func TestOnFail_RescueChain_RescueOfRescueRuns(t *testing.T) {
	var bCalled, cCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),       // A: fails
		"core.service": failedModule(&bCalled),  // B: rescue via onfail:[A], fails
		"core.cmd":     changedModule(&cCalled), // C: rescue via onfail:[B]
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-chain",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A"},
			{Name: "B", Module: "core.service.restarted", Register: "B", OnfailIdx: []int32{0}},
			{Name: "C", Module: "core.cmd.run", OnfailIdx: []int32{1}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bCalled {
		t.Errorf("B (rescue A) did not run even though A failed")
	}
	if !cCalled {
		t.Errorf("C (rescue B) did not run even though B failed - onfail composition is broken")
	}
	want := []keeperv1.TaskStatus{
		keeperv1.TaskStatus_TASK_STATUS_FAILED,  // A
		keeperv1.TaskStatus_TASK_STATUS_FAILED,  // B — ran AND failed
		keeperv1.TaskStatus_TASK_STATUS_CHANGED, // C — rescue of the rescue ran
	}
	if len(sink.taskEvents) != len(want) {
		t.Fatalf("taskEvents = %d, want %d", len(sink.taskEvents), len(want))
	}
	for i, w := range want {
		if got := sink.taskEvents[i].GetStatus(); got != w {
			t.Errorf("taskEvents[%d] status = %v, want %v", i, got, w)
		}
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestOnFail_ForwardRef_Skips — forward-ref onfail (onfail on a task LATER in the
// plan): X(onfail:[Y]), where Y runs AFTER X. Soul applies sequentially — at the
// time X runs, registerByIdx[Y] is empty, so skipOnFail treats the missing source
// as failed=false → X is silently SKIPPED (no-op). This is INTENTIONAL, not a bug:
// destiny/tasks.md documents onfail as a backward-requisite. The regression pins
// down the no-op so it doesn't get accidentally "fixed" into a forward resolve
// (which would require a two-pass run).
func TestOnFail_ForwardRef_Skips(t *testing.T) {
	var xCalled, yCalled bool
	reg := mapRegistry{
		"core.service": changedModule(&xCalled), // X: onfail on Y (later) → SKIPPED
		"core.exec":    failedModule(&yCalled),  // Y: fails, but AFTER X
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-forward-ref",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "X", Module: "core.service.restarted", OnfailIdx: []int32{1}}, // onfail on idx 1 (Y, later)
			{Name: "Y", Module: "core.exec.run", Register: "Y"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if xCalled {
		t.Errorf("X ran even though source Y had not run yet (forward-ref onfail = no-op SKIPPED)")
	}
	if !yCalled {
		t.Errorf("Y did not run (it's a plain task - the first actual failure in the run)")
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("X status = %v, want SKIPPED (forward-ref onfail is an intentional no-op)", got)
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("Y status = %v, want FAILED", got)
	}
	// Y failed → run FAILED; X — SKIPPED, didn't "rescue" Y's failure (it ran before Y).
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED (Y failed, forward-ref X did not cover it)", sink.runResult.GetStatus())
	}
}

// TestOnFail_AndWhen_BothApplied — an onfail task with when: gated by AND:
// source failed (onfail fires) BUT when:false → SKIPPED. Confirms the
// requisites AND when combination (destiny/tasks.md §9).
func TestOnFail_AndWhen_BothApplied(t *testing.T) {
	var rescueCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),            // A: fails → onfail would fire
		"core.service": changedModule(&rescueCalled), // rescue: onfail:[A] + when:false
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-and-when",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A"},
			{Name: "rescue", Module: "core.service.restarted", OnfailIdx: []int32{0}, When: "false"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rescueCalled {
		t.Errorf("rescue ran even though when:false (onfail && when combine via AND)")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("rescue status = %v, want SKIPPED (when:false)", got)
	}
}
