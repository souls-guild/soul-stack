package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func TestRun_HappyPath(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true, Message: "installed nginx"})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-1",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "install nginx", Module: "core.pkg.installed", Params: mustStruct(t, map[string]any{"name": "nginx"})},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d", len(sink.taskEvents))
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v", ev.GetStatus())
	}
	if ev.GetApplyId() != "apply-1" || ev.GetTaskIdx() != 0 {
		t.Errorf("apply_id / task_idx mismatch: %+v", ev)
	}
	if sink.runResult == nil || sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult = %+v", sink.runResult)
	}
}

// TestRun_EchoesAttemptInRunResult — gate-1 (ADR-027(g)): Soul echoes
// ApplyRequest.attempt into the final RunResult.attempt, so Keeper can reject
// a stale-attempt result on receipt (correlateRunResult epoch check).
func TestRun_EchoesAttemptInRunResult(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-1",
		Attempt: 4,
		Tasks: []*keeperv1.RenderedTask{
			{Name: "install nginx", Module: "core.pkg.installed"},
		},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.runResult == nil {
		t.Fatal("runResult is nil")
	}
	if got := sink.runResult.GetAttempt(); got != 4 {
		t.Errorf("RunResult.attempt = %d, want 4 (echo from ApplyRequest)", got)
	}

	// attempt unset (old Keeper) → echoes 0, forward-compat on receipt.
	zeroSink := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-2",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "install nginx", Module: "core.pkg.installed"},
		},
	}, zeroSink); err != nil {
		t.Fatalf("Run(zero attempt): %v", err)
	}
	if got := zeroSink.runResult.GetAttempt(); got != 0 {
		t.Errorf("RunResult.attempt = %d, want 0 (no attempt in the request)", got)
	}
}

// TestRun_NoLogEchoedToTaskEvent — [H] fix: Soul forwards RenderedTask.NoLog
// into TaskEvent.NoLog (echo flag for keeper-side audit suppression). Checks
// both outcomes — success (changed) and failure — the flag carries through in
// both, and doesn't when the task doesn't set it.
func TestRun_NoLogEchoedToTaskEvent(t *testing.T) {
	reg := mapRegistry{
		"core.exec": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				if req.GetState() == "fail" {
					return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "secret leaked here"})
				}
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-1",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "no_log task", Module: "core.exec.run", NoLog: true},
			{Name: "plain task", Module: "core.exec.run", NoLog: false},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d, want 2", len(sink.taskEvents))
	}
	if !sink.taskEvents[0].GetNoLog() {
		t.Errorf("task[0].no_log = false, want true (echoed from RenderedTask)")
	}
	if sink.taskEvents[1].GetNoLog() {
		t.Errorf("task[1].no_log = true, want false")
	}

	// failed branch: no_log carries through too (this is the main stderr-leak channel).
	failSink := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-2",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "no_log fail", Module: "core.exec.fail", NoLog: true},
		},
	}, failSink); err != nil {
		t.Fatalf("Run(fail): %v", err)
	}
	if len(failSink.taskEvents) != 1 {
		t.Fatalf("fail taskEvents = %d, want 1", len(failSink.taskEvents))
	}
	if failSink.taskEvents[0].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED", failSink.taskEvents[0].GetStatus())
	}
	if !failSink.taskEvents[0].GetNoLog() {
		t.Errorf("failed task no_log = false, want true")
	}
}

func TestRun_ModuleNotFound(t *testing.T) {
	reg := mapRegistry{}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "x",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "ghost", Module: "core.ghost.alive"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d", len(sink.taskEvents))
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "module.not_found" {
		t.Errorf("error.code = %q", ev.GetError().GetCode())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult.status = %v", sink.runResult.GetStatus())
	}
}

// TestRun_StopsOnFirstFailed — fail-stop with rescue (destiny/tasks.md §8):
// the first failed task marks the run FAILED, the next ORDINARY (no onfail)
// task does NOT run (mod.Apply isn't called), but now arrives as a SKIPPED
// event (not absent — the loop runs to completion for the rescue tail).
// RunResult is FAILED.
func TestRun_StopsOnFirstFailed(t *testing.T) {
	var secondCalled bool
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "boom"})
			},
		},
		"core.file": changedModule(&secondCalled),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "stop-test",
		Tasks: []*keeperv1.RenderedTask{
			{Module: "core.pkg.installed"},
			{Module: "core.file.present"}, // ordinary task after a failure — SKIPPED, not run
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if secondCalled {
		t.Errorf("second task executed even though the run had already failed (fail-stop)")
	}
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d (want 2: FAILED + SKIPPED)", len(sink.taskEvents))
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("first task status = %v, want FAILED", got)
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("second task status = %v, want SKIPPED (skipped after failure)", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult.status = %v", sink.runResult.GetStatus())
	}
}

func TestRun_ApplyReturnsError(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(*pluginv1.ApplyRequest, grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return errors.New("network down")
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)
	_ = r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "err",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed"}},
	}, sink)
	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d", len(sink.taskEvents))
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "module.error" {
		t.Errorf("error.code = %q", ev.GetError().GetCode())
	}
}

func TestRun_BadModuleAddress(t *testing.T) {
	sink := &recordingSink{}
	r := NewApplyRunner(mapRegistry{}, nil)
	_ = r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "bad",
		Tasks:   []*keeperv1.RenderedTask{{Module: ""}},
	}, sink)
	if sink.taskEvents[0].GetError().GetCode() != "module.bad_address" {
		t.Errorf("error.code = %q", sink.taskEvents[0].GetError().GetCode())
	}
}

func TestRun_SinkErrorPropagates(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{taskErr: errors.New("stream broken")}
	r := NewApplyRunner(reg, nil)
	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "x",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed"}},
	}, sink)
	if err == nil {
		t.Fatal("expected sink error to propagate")
	}
}

func TestRun_CancelBetweenTasks(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	// SendTaskEvent for the first task triggers Cancel — simulates CancelApply
	// arriving over the EventStream between ApplyEvents.
	sink.onTask = func(ev *keeperv1.TaskEvent) {
		if ev.GetTaskIdx() == 0 {
			r.Cancel(ev.GetApplyId())
		}
	}

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "cancel-1",
		Tasks: []*keeperv1.RenderedTask{
			{Module: "core.pkg.installed"},
			{Module: "core.pkg.installed"}, // must not run
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d (want 1)", len(sink.taskEvents))
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Errorf("runResult.status = %v, want CANCELLED", sink.runResult.GetStatus())
	}
}

func TestRun_CancelDuringTask(t *testing.T) {
	// Module respects ctx — blocks until cancel, returns ctx.Err().
	reg := mapRegistry{
		"core.exec": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				<-stream.Context().Done()
				return stream.Context().Err()
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	// Fire Cancel from a second goroutine — give Run time to enter Apply.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Small wait so Apply has a chance to be called.
		for i := 0; i < 50; i++ {
			if r.Cancel("cancel-2") {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("Cancel: apply-id never registered")
	}()

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "cancel-2",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.exec.run"}},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-done

	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d (want 1)", len(sink.taskEvents))
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CANCELLED {
		t.Errorf("status = %v, want CANCELLED", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "apply.cancelled" {
		t.Errorf("error.code = %q, want apply.cancelled", ev.GetError().GetCode())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Errorf("runResult.status = %v, want CANCELLED", sink.runResult.GetStatus())
	}
}

// TestRun_TaskTimeout_TimesOut — (a) a hanging module + RenderedTask{Timeout:
// "50ms"} → the task fails on the task timeout, not the scenario ceiling.
// Checks: status TIMED_OUT, register_data.timed_out, RunStatus_FAILED, and
// that the run finished in ~timeout (< 1s), not hung until the scenario
// ceiling (5 min).
func TestRun_TaskTimeout_TimesOut(t *testing.T) {
	reg := mapRegistry{
		"core.exec": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				// Hang until ctx is cancelled (the per-task timeout will cancel it).
				<-stream.Context().Done()
				return stream.Context().Err()
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	start := time.Now()
	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "to-1",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "hang", Module: "core.exec.run", Timeout: "50ms"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("run hung for %v - timed out on scenario-ceiling, not task-timeout", elapsed)
	}
	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d (want 1)", len(sink.taskEvents))
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT {
		t.Errorf("status = %v, want TIMED_OUT", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "task.timed_out" {
		t.Errorf("error.code = %q, want task.timed_out", ev.GetError().GetCode())
	}
	if !ev.GetRegisterData().GetFields()["timed_out"].GetBoolValue() {
		t.Errorf("register_data.timed_out != true")
	}
	if !ev.GetRegisterData().GetFields()["failed"].GetBoolValue() {
		t.Errorf("register_data.failed != true (timed_out is a special case of failed)")
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult.status = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestRun_TaskTimeout_EmptyNoLimit — (b) timeout="" → no per-task limit, a
// fast module runs normally (not cancelled).
func TestRun_TaskTimeout_EmptyNoLimit(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)
	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "to-empty",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed", Timeout: ""}},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.taskEvents[0].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED", sink.taskEvents[0].GetStatus())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult.status = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestRun_TaskTimeout_DaySuffix — (b') the `<N>d` suffix (Soul Stack's
// `duration` convention) is recognized by the same config.ParseDuration that
// keeper uses parsing destiny. `1d` = a valid large limit → the module runs
// normally, doesn't fail on "invalid duration" (as bare time.ParseDuration
// would, which doesn't understand `1d`). Confirms: Soul parser = keeper
// validator.
func TestRun_TaskTimeout_DaySuffix(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)
	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "to-day",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed", Timeout: "1d"}},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.taskEvents[0].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED (`1d` recognized as a valid limit)", sink.taskEvents[0].GetStatus())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult.status = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestRun_TaskTimeout_NonPositiveNoLimit — (b") `0s` (and any d <= 0) is
// treated as "no limit", NOT an instant deadline. Without the `d > 0` guard,
// WithTimeout(ctx, 0) would expire immediately → a false TIMED_OUT before the
// module even starts.
func TestRun_TaskTimeout_NonPositiveNoLimit(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)
	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "to-zero",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed", Timeout: "0s"}},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.taskEvents[0].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED (`0s` = no limit, not an instant TIMED_OUT)", sink.taskEvents[0].GetStatus())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult.status = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestRun_TaskTimeout_NotMaskedAsCancel — (c) an expired CHILD per-task ctx
// with a live PARENT ctx gives TIMED_OUT, not CANCELLED. This is the central
// invariant: a timeout must not fall into the cancel branch in Run
// (runCtx.Err() stays nil, since taskCtx is a child).
func TestRun_TaskTimeout_NotMaskedAsCancel(t *testing.T) {
	reg := mapRegistry{
		"core.exec": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				<-stream.Context().Done()
				return stream.Context().Err()
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	// Parent ctx is NOT cancelled — it stays alive for the whole run.
	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "to-cancel-distinct",
		Tasks:   []*keeperv1.RenderedTask{{Name: "hang", Module: "core.exec.run", Timeout: "30ms"}},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_CANCELLED {
		t.Fatalf("status = CANCELLED - timeout masqueraded as cancel")
	}
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT {
		t.Errorf("status = %v, want TIMED_OUT", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "task.timed_out" {
		t.Errorf("error.code = %q, want task.timed_out", ev.GetError().GetCode())
	}
	if sink.runResult.GetStatus() == keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Fatalf("runResult.status = CANCELLED - the run masqueraded timeout as cancel")
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult.status = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestRun_TaskTimeout_InvalidDuration — (d) an invalid duration string is
// treated as unset (no limit), Run doesn't fail with an internal error, a
// fast module runs normally.
func TestRun_TaskTimeout_InvalidDuration(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)
	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "to-bad",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed", Timeout: "not-a-duration"}},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v (must not fail on an invalid duration)", err)
	}
	if sink.taskEvents[0].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED (invalid timeout = not set)", sink.taskEvents[0].GetStatus())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult.status = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

func TestCancel_UnknownApplyID(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	if r.Cancel("nope") {
		t.Errorf("Cancel(unknown) = true, want false")
	}
}

func TestCancel_ConcurrentSafe(t *testing.T) {
	// Runs Cancel calls concurrently with one Run — the race detector should
	// stay quiet. This test doesn't validate timing logic (that's
	// CancelDuringTask), it only checks for no data race on the active map.
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	r := NewApplyRunner(reg, nil)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = r.Cancel("x")
			}
		}()
	}
	sink := &recordingSink{}
	_ = r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "x",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed"}},
	}, sink)
	wg.Wait()
}

func TestRun_RegisterDataIncludesOutput(t *testing.T) {
	reg := mapRegistry{
		"core.exec": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{
					Changed: true,
					Output: mustStruct(nil, map[string]any{
						"stdout":    "hello",
						"exit_code": 0,
					}),
				})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)
	_ = r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{{Module: "core.exec.run"}},
	}, sink)
	rd := sink.taskEvents[0].GetRegisterData().GetFields()
	if rd["changed"].GetBoolValue() != true {
		t.Errorf("register.changed != true")
	}
	if rd["stdout"].GetStringValue() != "hello" {
		t.Errorf("register.stdout = %q", rd["stdout"].GetStringValue())
	}
}

// TestRun_OnChanges_SourceChanged_Runs — source changed → the onchanges task
// runs (mod.Apply is called), status is not SKIPPED.
func TestRun_OnChanges_SourceChanged_Runs(t *testing.T) {
	restarted := false
	reg := mapRegistry{
		"core.file": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true, Message: "config rewritten"})
			},
		},
		"core.service": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				restarted = true
				return stream.Send(&pluginv1.ApplyEvent{Changed: true, Message: "restarted"})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "oc-changed",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "redis_conf", Module: "core.file.present"},
			{Name: "restart", Module: "core.service.restarted", OnchangesIdx: []int32{0}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !restarted {
		t.Fatal("restart task was NOT executed even though the source changed (mod.Apply should be called)")
	}
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d, want 2", len(sink.taskEvents))
	}
	if sink.taskEvents[1].GetStatus() == keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("restart status = SKIPPED, want executed (source changed)")
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult.status = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestRun_OnChanges_SourceUnchanged_Skipped — source unchanged (OK without
// changed) → the onchanges task is SKIPPED: status SKIPPED, mod.Apply NOT
// called, register.skipped == true, register.changed == false. This is the
// core of the restart-flap fix.
func TestRun_OnChanges_SourceUnchanged_Skipped(t *testing.T) {
	restarted := false
	reg := mapRegistry{
		"core.file": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				// OK with no changes — config is already in the desired state.
				return stream.Send(&pluginv1.ApplyEvent{Changed: false, Message: "no change"})
			},
		},
		"core.service": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				restarted = true
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "oc-unchanged",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "redis_conf", Module: "core.file.present"},
			{Name: "restart", Module: "core.service.restarted", OnchangesIdx: []int32{0}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if restarted {
		t.Fatal("restart task WAS EXECUTED (mod.Apply called) even though the source is unchanged - restart-flap not fixed")
	}
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d, want 2 (skipped task also sends TaskEvent)", len(sink.taskEvents))
	}
	skip := sink.taskEvents[1]
	if skip.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("restart status = %v, want SKIPPED", skip.GetStatus())
	}
	rd := skip.GetRegisterData().GetFields()
	if !rd["skipped"].GetBoolValue() {
		t.Errorf("register.skipped != true")
	}
	if rd["changed"].GetBoolValue() {
		t.Errorf("register.changed == true for a skipped task (skipped != changed)")
	}
	if rd["failed"].GetBoolValue() {
		t.Errorf("register.failed == true for a skipped task")
	}
	// SKIPPED is not a failure: the run succeeds.
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult.status = %v, want SUCCESS (skip ≠ fail)", sink.runResult.GetStatus())
	}
}

// TestRun_OnChanges_MultiSource_AnyChanged — multiple sources, at least one
// changed → the onchanges task runs (any semantics, destiny/tasks.md §8).
func TestRun_OnChanges_MultiSource_AnyChanged(t *testing.T) {
	ran := false
	reg := mapRegistry{
		"core.file": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: false})
			},
		},
		"core.user": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true}) // this one changed
			},
		},
		"core.service": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				ran = true
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "oc-multi",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "conf_a", Module: "core.file.present"},                                    // idx 0: unchanged
			{Name: "conf_b", Module: "core.user.present"},                                    // idx 1: changed
			{Name: "restart", Module: "core.service.restarted", OnchangesIdx: []int32{0, 1}}, // any → run
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ran {
		t.Fatal("onchanges-task was NOT executed even though one of the sources changed (any-semantics)")
	}
	if sink.taskEvents[2].GetStatus() == keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("restart status = SKIPPED, want executed")
	}
}

// TestRun_OnChanges_SkippedDoesNotTrigger — a skipped source does NOT trigger
// onchanges downstream (skipped ≠ changed): task A is skipped by its own
// onchanges, task B with onchanges on A → also skipped (A gave no changed).
func TestRun_OnChanges_SkippedDoesNotTrigger(t *testing.T) {
	ran := map[string]bool{}
	reg := mapRegistry{
		"core.file": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: false}) // unchanged
			},
		},
		"core.service": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				ran["a"] = true
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
		"core.cmd": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				ran["b"] = true
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "oc-chain",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "conf", Module: "core.file.present"},                             // idx 0: unchanged
			{Name: "a", Module: "core.service.restarted", OnchangesIdx: []int32{0}}, // idx 1: skipped (source unchanged)
			{Name: "b", Module: "core.cmd.run", OnchangesIdx: []int32{1}},           // idx 2: onchanges on skipped a → also skip
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran["a"] {
		t.Error("task a executed even though its source is unchanged")
	}
	if ran["b"] {
		t.Error("task b executed - skipped-source a must NOT trigger onchanges (skipped != changed)")
	}
	if sink.taskEvents[1].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_SKIPPED ||
		sink.taskEvents[2].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("statuses = [%v %v], want [SKIPPED SKIPPED]",
			sink.taskEvents[1].GetStatus(), sink.taskEvents[2].GetStatus())
	}
}

// TestSkipOnChanges — table-driven unit test for the gating predicate.
func TestSkipOnChanges(t *testing.T) {
	t.Parallel()
	changed := buildRegisterData(keeperv1.TaskStatus_TASK_STATUS_CHANGED, nil)
	unchanged := buildRegisterData(keeperv1.TaskStatus_TASK_STATUS_OK, nil)
	skipped := buildRegisterData(keeperv1.TaskStatus_TASK_STATUS_SKIPPED, nil)

	tests := []struct {
		name string
		idx  []int32
		reg  map[int32]*structpb.Struct
		want bool
	}{
		{"empty onchanges -> run", nil, nil, false},
		{"source changed -> run", []int32{0}, map[int32]*structpb.Struct{0: changed}, false},
		{"source unchanged -> skip", []int32{0}, map[int32]*structpb.Struct{0: unchanged}, true},
		{"any changed → run", []int32{0, 1}, map[int32]*structpb.Struct{0: unchanged, 1: changed}, false},
		{"all unchanged -> skip", []int32{0, 1}, map[int32]*structpb.Struct{0: unchanged, 1: unchanged}, true},
		{"skipped-source -> skip", []int32{0}, map[int32]*structpb.Struct{0: skipped}, true},
		{"missing source -> skip", []int32{0}, map[int32]*structpb.Struct{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skipOnChanges(tt.idx, tt.reg); got != tt.want {
				t.Errorf("skipOnChanges = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- helpers ---

type mapRegistry map[string]module.SoulModule

func (m mapRegistry) Lookup(name string) (module.SoulModule, bool) {
	mod, ok := m[name]
	return mod, ok
}

type fakeModule struct {
	module.BaseModule
	applyFunc func(*pluginv1.ApplyRequest, grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error
}

func (f *fakeModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	return f.applyFunc(req, stream)
}

type recordingSink struct {
	taskEvents []*keeperv1.TaskEvent
	runResult  *keeperv1.RunResult
	taskErr    error
	// onTask — hook called before the event is recorded in taskEvents. Used by
	// cancel-logic tests to simulate CancelApply between tasks.
	onTask func(*keeperv1.TaskEvent)
}

func (s *recordingSink) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	if s.taskErr != nil {
		return s.taskErr
	}
	if s.onTask != nil {
		s.onTask(ev)
	}
	s.taskEvents = append(s.taskEvents, ev)
	return nil
}

func (s *recordingSink) SendRunResult(r *keeperv1.RunResult) error {
	s.runResult = r
	return nil
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	st, err := structpb.NewStruct(m)
	if err != nil {
		if t != nil {
			t.Fatalf("structpb.NewStruct: %v", err)
		}
		panic(err)
	}
	return st
}
