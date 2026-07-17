package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// seqModule — a module with a pre-set sequence of outcomes by attempt number
// (1-based). attempts counts actual Apply calls. outcome sets the i-th
// attempt's result; if more attempts occur than outcome has entries, the last
// one is reused.
type attemptOutcome struct {
	failed  bool // failed=true → FAILED
	changed bool // changed=true (when failed=false) → CHANGED, otherwise OK
}

func seqModule(attempts *int32, seq []attemptOutcome) *fakeModule {
	return &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			n := atomic.AddInt32(attempts, 1)
			i := int(n) - 1
			if i >= len(seq) {
				i = len(seq) - 1
			}
			o := seq[i]
			return stream.Send(&pluginv1.ApplyEvent{Failed: o.failed, Changed: o.changed})
		},
	}
}

// TestRetry_NoUntil_SecondAttemptOK — 1st attempt FAILED, 2nd OK → task OK,
// exactly 2 Apply calls.
func TestRetry_NoUntil_SecondAttemptOK(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.exec": seqModule(&attempts, []attemptOutcome{
		{failed: true}, {failed: false, changed: false},
	})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "retry-2nd-ok",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "flaky", Module: "core.exec.run", RetryCount: 3, RetryDelay: "1ms"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("status = %v, want OK", got)
	}
	// One TaskEvent per task_idx — intermediate attempts aren't emitted outward.
	if len(sink.taskEvents) != 1 {
		t.Errorf("taskEvents = %d, want 1 (intermediate attempts are not emitted)", len(sink.taskEvents))
	}
}

// TestRetry_NoUntil_AllFail — all attempts FAILED → final FAILED, call count.
func TestRetry_NoUntil_AllFail(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.exec": seqModule(&attempts, []attemptOutcome{{failed: true}})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "retry-all-fail",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "doomed", Module: "core.exec.run", RetryCount: 3, RetryDelay: "1ms"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// timeoutThenOKModule — the first failUntil attempts block until the
// per-attempt timeout (TIMED_OUT), later ones send OK immediately. Used to
// verify that a TIMED_OUT attempt is retried.
func timeoutThenOKModule(attempts *int32, failUntil int32) *fakeModule {
	return &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			n := atomic.AddInt32(attempts, 1)
			if n <= failUntil {
				<-stream.Context().Done()
				return stream.Context().Err()
			}
			return stream.Send(&pluginv1.ApplyEvent{})
		},
	}
}

// TestRetry_TimedOutAttempt_Retries — a TIMED_OUT attempt is retried; the
// first blocks until timeout, the second succeeds → task OK.
func TestRetry_TimedOutAttempt_Retries(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.cmd": timeoutThenOKModule(&attempts, 1)}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "retry-timeout",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "slow", Module: "core.cmd.run", Timeout: "20ms", RetryCount: 3, RetryDelay: "1ms"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2 (TIMED_OUT retries once, 2nd OK)", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("status = %v, want OK", got)
	}
}

// TestRetry_AllTimedOut_FinalTimedOut — all attempts TIMED_OUT → final
// TIMED_OUT (does NOT collapse into FAILED).
func TestRetry_AllTimedOut_FinalTimedOut(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.cmd": timeoutThenOKModule(&attempts, 100)} // always times out
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "retry-all-timeout",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "stuck", Module: "core.cmd.run", Timeout: "20ms", RetryCount: 2, RetryDelay: "1ms"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT {
		t.Errorf("status = %v, want TIMED_OUT (must not collapse into FAILED)", got)
	}
}

// TestUntil_TrueFirstAttempt_Exits — until-true on the 1st attempt → exits, one attempt.
func TestUntil_TrueFirstAttempt_Exits(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.exec": seqModule(&attempts, []attemptOutcome{{changed: true}})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "until-true-1st",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "probe", Module: "core.exec.run", RetryCount: 5, RetryDelay: "1ms",
				Until: "register.self.changed"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (until-true immediately)", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED", got)
	}
}

// TestUntil_NeverTrue_Exhausted — until-false on every attempt → FAILED
// (until_exhausted), even when the attempt is OK.
func TestUntil_NeverTrue_Exhausted(t *testing.T) {
	var attempts int32
	// Every attempt is OK (changed=false) → until "register.self.changed" is always false.
	reg := mapRegistry{"core.exec": seqModule(&attempts, []attemptOutcome{{}})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "until-exhausted",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "wait-change", Module: "core.exec.run", RetryCount: 3, RetryDelay: "1ms",
				Until: "register.self.changed"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED (until never became truthy)", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "flowcontrol.until_exhausted" {
		t.Errorf("error.code = %q, want flowcontrol.until_exhausted", ev.GetError().GetCode())
	}
}

// TestUntil_TrueButFailed_FinalFailed — until-true, but the attempt is FAILED
// → final FAILED (until does NOT override failed).
func TestUntil_TrueButFailed_FinalFailed(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.exec": seqModule(&attempts, []attemptOutcome{{failed: true}})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "until-true-failed",
		Tasks: []*keeperv1.RenderedTask{
			// until "true" is a truism → exits on the 1st attempt, but the failed status remains.
			{Name: "fails", Module: "core.exec.run", RetryCount: 5, RetryDelay: "1ms",
				Until: "register.self.failed"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (until-true immediately - self.failed==true)", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED (until does not override failed)", got)
	}
}

// TestUntil_TrueOnOK_OK — until-true and the attempt is OK → OK.
func TestUntil_TrueOnOK_OK(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.exec": seqModule(&attempts, []attemptOutcome{{changed: true}})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "until-true-ok",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "done", Module: "core.exec.run", RetryCount: 5, RetryDelay: "1ms",
				Until: "register.self.changed"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED", got)
	}
}

// TestUntil_ChangedWhenError_PreservesCode — changed_when failed with a CEL
// runtime error, and until: is also set. runTask returned the terminal branch
// (selfRegister==nil) with code flowcontrol.changed_when_error. until-eval must
// NOT run (no register.self) and overwrite the original code with
// flowcontrol.until_error: one attempt, FAILED, changed_when_error code
// preserved (nit-fix for runTaskWithRetry's until branch).
func TestUntil_ChangedWhenError_PreservesCode(t *testing.T) {
	var attempts int32
	// OK outcome → we reach the changed_when override; a reference to a
	// nonexistent register.self field → runtime error → changed_when_error.
	// attempts counts actual Apply calls (there should be no retry).
	reg := mapRegistry{"core.exec": &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			atomic.AddInt32(&attempts, 1)
			return stream.Send(&pluginv1.ApplyEvent{Output: mustStruct(t, map[string]any{"exit_code": 0})})
		},
	}}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "until-over-changed-when-error",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "typo-with-until", Module: "core.exec.run", RetryCount: 3, RetryDelay: "1ms",
				ChangedWhen: "register.self.no_such_field", Until: "register.self.changed"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (terminal changed_when error is not retried)", got)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "flowcontrol.changed_when_error" {
		t.Errorf("error.code = %q, want flowcontrol.changed_when_error (until must not overwrite it with until_error)", ev.GetError().GetCode())
	}
}

// TestRetry_FailedWhenFalse_SingleAttempt — failed_when:false (ignore_errors)
// turns a failed attempt into OK → a "non-FAILED outcome" → exits on the first
// attempt (retry never triggers).
func TestRetry_FailedWhenFalse_SingleAttempt(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.exec": seqModule(&attempts, []attemptOutcome{{failed: true}})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "retry-ignore-errors",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "ignored", Module: "core.exec.run", RetryCount: 3, RetryDelay: "1ms",
				FailedWhen: "false"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (ignore_errors wins over retry)", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("status = %v, want OK", got)
	}
}

// TestRetry_CancelDuringDelay_NotBlocked — cancel during delay → exits the
// loop, task CANCELLED, the run doesn't hang for the full delay.
func TestRetry_CancelDuringDelay_NotBlocked(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.exec": &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			atomic.AddInt32(&attempts, 1)
			return stream.Send(&pluginv1.ApplyEvent{Failed: true})
		},
	}}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the run shortly after start — lands inside the delay (it's long: 30s).
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx, &keeperv1.ApplyRequest{
			ApplyId: "retry-cancel-delay",
			Tasks: []*keeperv1.RenderedTask{
				{Name: "flaky", Module: "core.exec.run", RetryCount: 5, RetryDelay: "30s"},
			},
		}, sink)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run hung for the full retry_delay - cancel during delay did not interrupt the loop")
	}

	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CANCELLED {
		t.Errorf("status = %v, want CANCELLED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Errorf("runResult = %v, want CANCELLED", sink.runResult.GetStatus())
	}
	// Only one attempt should have run (delay was interrupted before the 2nd).
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (cancel during first delay)", got)
	}
}

// TestRetry_ExhaustedTriggersOnfail — retries-exhausted FAILED triggers the
// onfail task (rescue), same as an ordinary FAILED.
func TestRetry_ExhaustedTriggersOnfail(t *testing.T) {
	var attempts, rescueCalled int32
	reg := mapRegistry{
		"core.exec":    seqModule(&attempts, []attemptOutcome{{failed: true}}),
		"core.service": seqModule(&rescueCalled, []attemptOutcome{{changed: true}}),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "retry-exhausted-onfail",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A", RetryCount: 2, RetryDelay: "1ms"},
			{Name: "rescue", Module: "core.service.restarted", OnfailIdx: []int32{0}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("A attempts = %d, want 2 (retry exhausted)", got)
	}
	if atomic.LoadInt32(&rescueCalled) != 1 {
		t.Errorf("rescue did not fire on retries-exhausted FAILED")
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("A status = %v, want FAILED", got)
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("rescue status = %v, want CHANGED", got)
	}
}

// TestRetry_BackwardCompat_NoRetryNoUntil — retry_count empty/1 + until empty
// → one attempt, behaves as before.
func TestRetry_BackwardCompat_NoRetryNoUntil(t *testing.T) {
	for _, count := range []int32{0, 1} {
		var attempts int32
		reg := mapRegistry{"core.exec": seqModule(&attempts, []attemptOutcome{{failed: true}})}
		sink := &recordingSink{}
		r := NewApplyRunner(reg, nil)

		err := r.Run(context.Background(), &keeperv1.ApplyRequest{
			ApplyId: "retry-compat",
			Tasks: []*keeperv1.RenderedTask{
				{Name: "once", Module: "core.exec.run", RetryCount: count},
			},
		}, sink)
		if err != nil {
			t.Fatalf("Run (count=%d): %v", count, err)
		}
		if got := atomic.LoadInt32(&attempts); got != 1 {
			t.Errorf("count=%d: attempts = %d, want 1", count, got)
		}
		if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
			t.Errorf("count=%d: status = %v, want FAILED", count, got)
		}
	}
}
