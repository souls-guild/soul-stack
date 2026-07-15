package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// changedModule — module that always reports changed=true and records
// whether Apply was called (for testing "when:false → Apply not invoked").
func changedModule(called *bool) *fakeModule {
	return &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			if called != nil {
				*called = true
			}
			return stream.Send(&pluginv1.ApplyEvent{Changed: true})
		},
	}
}

// okOutputModule — OK module (changed=false, failed=false) publishing output
// fields, for testing register.self.<field> in changed_when/failed_when.
func okOutputModule(out map[string]any) *fakeModule {
	return &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			return stream.Send(&pluginv1.ApplyEvent{Output: mustStruct(nil, out)})
		},
	}
}

// failedModule — module that always returns failed=true (for testing
// failed_when:false = ignore_errors).
func failedModule(called *bool) *fakeModule {
	return &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			if called != nil {
				*called = true
			}
			return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "boom"})
		},
	}
}

// TestWhen_False_Skips — when:"false" → task SKIPPED, mod.Apply NOT called.
func TestWhen_False_Skips(t *testing.T) {
	var called bool
	reg := mapRegistry{"core.pkg": changedModule(&called)}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "when-false",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "skip me", Module: "core.pkg.installed", When: "false"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Errorf("mod.Apply вызван, хотя when:false")
	}
	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d", len(sink.taskEvents))
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("status = %v, want SKIPPED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestWhen_True_Runs — when:"true" → task runs (CHANGED).
func TestWhen_True_Runs(t *testing.T) {
	var called bool
	reg := mapRegistry{"core.pkg": changedModule(&called)}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "when-true",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "run me", Module: "core.pkg.installed", When: "true"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Errorf("mod.Apply НЕ вызван, хотя when:true")
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED", got)
	}
}

// TestWhen_Empty_RunsUnconditionally — empty when → unconditional run
// (regression: existing destinies without when behave as before).
func TestWhen_Empty_RunsUnconditionally(t *testing.T) {
	var called bool
	reg := mapRegistry{"core.pkg": changedModule(&called)}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "when-empty",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed"}},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Errorf("mod.Apply НЕ вызван при пустом when")
	}
}

// TestWhen_FromFlowContext — when references flow_context (input/self)
// delivered by Keeper: input.do_restart && soulprint.self.os.family == 'debian'.
func TestWhen_FromFlowContext(t *testing.T) {
	var called bool
	reg := mapRegistry{"core.service": changedModule(&called)}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	flowCtx := mustStruct(t, map[string]any{
		"input": map[string]any{"do_restart": true},
		"self":  map[string]any{"os": map[string]any{"family": "debian"}},
	})

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "when-fc",
		Tasks: []*keeperv1.RenderedTask{
			{
				Name:        "restart",
				Module:      "core.service.restarted",
				When:        "input.do_restart && soulprint.self.os.family == 'debian'",
				FlowContext: flowCtx,
			},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Errorf("задача не выполнилась, хотя when по flow_context истинен")
	}
}

// TestWhen_RefsPreviousRegister — the second task's when references the first
// task's register by NAME (register.probe.changed). First task (register:
// "probe") changes state → second task runs.
func TestWhen_RefsPreviousRegister(t *testing.T) {
	var secondCalled bool
	reg := mapRegistry{
		"core.exec":    changedModule(nil), // probe: changed=true
		"core.service": changedModule(&secondCalled),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "when-register",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "probe", Module: "core.exec.run", Register: "probe"},
			{Name: "react", Module: "core.service.restarted", When: "register.probe.changed"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !secondCalled {
		t.Errorf("вторая задача не выполнилась, хотя register.probe.changed == true")
	}
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d, want 2", len(sink.taskEvents))
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("вторая задача status = %v, want CHANGED", got)
	}
}

// TestWhen_RefsPreviousRegister_False — first task's register not changed →
// second task's when:register.probe.changed = false → SKIPPED.
func TestWhen_RefsPreviousRegister_False(t *testing.T) {
	var secondCalled bool
	okModule := &fakeModule{ // probe: changed=false (OK, no changes)
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			return stream.Send(&pluginv1.ApplyEvent{Changed: false})
		},
	}
	reg := mapRegistry{
		"core.exec":    okModule,
		"core.service": changedModule(&secondCalled),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "when-register-false",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "probe", Module: "core.exec.run", Register: "probe"},
			{Name: "react", Module: "core.service.restarted", When: "register.probe.changed"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if secondCalled {
		t.Errorf("вторая задача выполнилась, хотя register.probe.changed == false")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("вторая задача status = %v, want SKIPPED", got)
	}
}

// TestWhen_AndOnchanges_WhenTrueButOnchangesNotFired — when:true, but the
// onchanges source is not changed → SKIPPED (AND: runs only if both hold).
func TestWhen_AndOnchanges_WhenTrueButOnchangesNotFired(t *testing.T) {
	var secondCalled bool
	okModule := &fakeModule{ // source: changed=false
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			return stream.Send(&pluginv1.ApplyEvent{Changed: false})
		},
	}
	reg := mapRegistry{
		"core.file":    okModule,
		"core.service": changedModule(&secondCalled),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "when-and-onchanges",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "config", Module: "core.file.present"},
			{
				Name:         "restart",
				Module:       "core.service.restarted",
				When:         "true",
				OnchangesIdx: []int32{0}, // source not changed → onchanges didn't fire
			},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if secondCalled {
		t.Errorf("задача выполнилась, хотя onchanges не сработал (when:true && onchanges-not-satisfied)")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("status = %v, want SKIPPED", got)
	}
}

// TestWhen_AndOnchanges_BothSatisfied — when:true AND onchanges source
// changed → task runs.
func TestWhen_AndOnchanges_BothSatisfied(t *testing.T) {
	var secondCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil), // source: changed=true
		"core.service": changedModule(&secondCalled),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "when-and-onchanges-ok",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "config", Module: "core.file.present"},
			{
				Name:         "restart",
				Module:       "core.service.restarted",
				When:         "true",
				OnchangesIdx: []int32{0},
			},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !secondCalled {
		t.Errorf("задача не выполнилась, хотя when:true && onchanges сработал")
	}
}

// TestWhen_RuntimeError_Fails — when references a nonexistent register →
// CEL runtime error → task FAILED, run stops (templating.md §10).
func TestWhen_RuntimeError_Fails(t *testing.T) {
	var called bool
	reg := mapRegistry{"core.pkg": changedModule(&called)}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "when-err",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "bad when", Module: "core.pkg.installed", When: "register.ghost.changed"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Errorf("mod.Apply вызван, хотя when упал runtime-error")
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "flowcontrol.when_error" {
		t.Errorf("error.code = %q, want flowcontrol.when_error", ev.GetError().GetCode())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// --- changed_when (override changed AFTER Apply) ---

// TestChangedWhen_False_OverridesChanged — changed_when:false on a CHANGED
// module → result OK (changed cleared). Classic probe case: task not counted
// as changing.
func TestChangedWhen_False_OverridesChanged(t *testing.T) {
	reg := mapRegistry{"core.exec": changedModule(nil)}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "probe", Module: "core.exec.run", ChangedWhen: "false"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("status = %v, want OK (changed_when:false снял changed)", ev.GetStatus())
	}
	if ev.GetRegisterData().GetFields()["changed"].GetBoolValue() {
		t.Errorf("register.changed = true, want false")
	}
}

// TestChangedWhen_True_OverridesToChanged — changed_when:true on an OK module → CHANGED.
func TestChangedWhen_True_OverridesToChanged(t *testing.T) {
	reg := mapRegistry{"core.exec": okOutputModule(map[string]any{"exit_code": 0})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "force changed", Module: "core.exec.run", ChangedWhen: "true"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED (changed_when:true)", got)
	}
}

// TestChangedWhen_DoesNotTriggerOnchanges — changed_when:false on the onchanges
// source → handler does NOT run (triggers on the overridden changed, not the
// raw one, destiny/tasks.md §9).
func TestChangedWhen_DoesNotTriggerOnchanges(t *testing.T) {
	var handlerCalled bool
	reg := mapRegistry{
		"core.exec":    changedModule(nil),            // raw changed=true, but changed_when:false
		"core.service": changedModule(&handlerCalled), // handler via onchanges
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "probe", Module: "core.exec.run", ChangedWhen: "false"},
			{Name: "handler", Module: "core.service.restarted", OnchangesIdx: []int32{0}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if handlerCalled {
		t.Errorf("handler выполнился, хотя changed_when:false снял changed источника")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("handler status = %v, want SKIPPED", got)
	}
}

// --- failed_when (override failed AFTER Apply) ---

// TestFailedWhen_True_OnOkModule_Fails — failed_when:true on an OK module →
// FAILED (synthetic failure from a business condition).
func TestFailedWhen_True_OnOkModule_Fails(t *testing.T) {
	reg := mapRegistry{"core.exec": okOutputModule(map[string]any{"exit_code": 3})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "biz fail", Module: "core.exec.run", FailedWhen: "register.self.exit_code != 0"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED (failed_when:true)", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "flowcontrol.failed_when" {
		t.Errorf("error.code = %q, want flowcontrol.failed_when", ev.GetError().GetCode())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestFailedWhen_False_IgnoresError — failed_when:false on a failing module →
// IGNORE_ERRORS: task OK, RunStatus SUCCESS, original error preserved only in
// register.self.ignored_error. TaskEvent.error stays EMPTY — apply.proto
// contract (error is set only for FAILED/TIMED_OUT).
func TestFailedWhen_False_IgnoresError(t *testing.T) {
	reg := mapRegistry{"core.exec": failedModule(nil)}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "best effort", Module: "core.exec.run", FailedWhen: "false"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("status = %v, want OK (failed_when:false проглотил ошибку)", ev.GetStatus())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult = %v, want SUCCESS", sink.runResult.GetStatus())
	}
	// Audit: the original module error isn't lost.
	if got := ev.GetRegisterData().GetFields()["ignored_error"].GetStringValue(); got != "boom" {
		t.Errorf("register.ignored_error = %q, want %q", got, "boom")
	}
	// apply.proto contract: TaskEvent.error is empty for non-FAILED status.
	if ev.GetError() != nil {
		t.Errorf("TaskEvent.error = %v, want nil (статус OK — error должен быть пуст)", ev.GetError())
	}
}

// TestFailedWhen_False_DoesNotSwallowTimedOut — failed_when:false does NOT
// suppress TIMED_OUT: the timeout is infrastructural, stays a terminal fail-stop.
func TestFailedWhen_False_DoesNotSwallowTimedOut(t *testing.T) {
	slowModule := &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			<-stream.Context().Done()
			return stream.Context().Err()
		},
	}
	reg := mapRegistry{"core.exec": slowModule}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "slow", Module: "core.exec.run", Timeout: "10ms", FailedWhen: "false"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT {
		t.Errorf("status = %v, want TIMED_OUT (failed_when:false не глушит таймаут)", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED (TIMED_OUT — fail-stop)", sink.runResult.GetStatus())
	}
}

// TestChangedWhenAndFailedWhen_FailedPrioritised — both branches together:
// changed_when:true would give CHANGED, but failed_when:true → result FAILED
// (failed takes priority).
func TestChangedWhenAndFailedWhen_FailedPrioritised(t *testing.T) {
	reg := mapRegistry{"core.exec": okOutputModule(map[string]any{"exit_code": 1})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{
				Name:        "both",
				Module:      "core.exec.run",
				ChangedWhen: "true",
				FailedWhen:  "register.self.exit_code != 0",
			},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED (failed приоритетнее changed)", got)
	}
}

// TestFlowControl_RegisterSelf_RuntimeError_Fails — changed_when references a
// nonexistent register.self field → CEL runtime error → task FAILED (a typo
// in an output field name is an error, templating.md §10).
func TestFlowControl_RegisterSelf_RuntimeError_Fails(t *testing.T) {
	reg := mapRegistry{"core.exec": okOutputModule(map[string]any{"exit_code": 0})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "typo", Module: "core.exec.run", ChangedWhen: "register.self.no_such_field"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED (runtime-error в changed_when)", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "flowcontrol.changed_when_error" {
		t.Errorf("error.code = %q, want flowcontrol.changed_when_error", ev.GetError().GetCode())
	}
}

// TestIgnoreErrors_DoesNotStopRun — task N with failed_when:false fails in the
// module, but the run does NOT stop: task N+1 RUNS (fail-stop didn't trigger).
func TestIgnoreErrors_DoesNotStopRun(t *testing.T) {
	var nextCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),
		"core.service": changedModule(&nextCalled),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "best effort", Module: "core.exec.run", FailedWhen: "false"},
			{Name: "next", Module: "core.service.restarted"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !nextCalled {
		t.Errorf("задача N+1 не выполнилась — fail-stop сработал, хотя failed_when:false")
	}
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d, want 2", len(sink.taskEvents))
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// --- QA coverage gaps ---

// TestWhenSkipped_SubsequentSeesSkippedNotChanged — a task with register:X is
// skipped by when:false → the next task sees register.X.skipped == true and
// changed == false (skipped ≠ changed; doesn't trigger onchanges or lie in a
// downstream when).
func TestWhenSkipped_SubsequentSeesSkippedNotChanged(t *testing.T) {
	var reactByWhenCalled bool
	var reactByOnchangesCalled bool
	reg := mapRegistry{
		"core.exec":    changedModule(nil), // would report changed=true if it ran
		"core.service": changedModule(&reactByWhenCalled),
		"core.cmd":     changedModule(&reactByOnchangesCalled),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "probe", Module: "core.exec.run", Register: "probe", When: "false"},
			// sees register.probe.skipped == true
			{Name: "react skip", Module: "core.service.restarted", When: "register.probe.skipped"},
			// onchanges on a skipped task does NOT fire (skipped ≠ changed)
			{Name: "react onchanges", Module: "core.cmd.run", OnchangesIdx: []int32{0}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// probe was skipped.
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("probe status = %v, want SKIPPED", got)
	}
	if sink.taskEvents[0].GetRegisterData().GetFields()["changed"].GetBoolValue() {
		t.Errorf("probe register.changed = true, want false (skipped ≠ changed)")
	}
	// when:register.probe.skipped → true → react ran.
	if !reactByWhenCalled {
		t.Errorf("react-by-when не выполнился, хотя register.probe.skipped == true")
	}
	// onchanges on a skipped source does NOT fire.
	if reactByOnchangesCalled {
		t.Errorf("react-by-onchanges выполнился, хотя источник skipped (не changed)")
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("react-by-onchanges status = %v, want SKIPPED", got)
	}
}

// TestFailedWhen_RegisterSelf_RuntimeError_Fails — failed_when references a
// nonexistent register.self field → CEL runtime error → task FAILED
// (symmetric with changed_when_error; templating.md §10).
func TestFailedWhen_RegisterSelf_RuntimeError_Fails(t *testing.T) {
	reg := mapRegistry{"core.exec": okOutputModule(map[string]any{"exit_code": 0})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "typo", Module: "core.exec.run", FailedWhen: "register.self.no_such_field"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED (runtime-error в failed_when)", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "flowcontrol.failed_when_error" {
		t.Errorf("error.code = %q, want flowcontrol.failed_when_error", ev.GetError().GetCode())
	}
}

// TestFailedWhen_SeesChangedWhenResult — documented order changed_when→failed_when:
// OK module + changed_when:true (flips to changed) + failed_when:register.self.changed
// → FAILED, because failed_when reads the changed=true already applied by changed_when.
func TestFailedWhen_SeesChangedWhenResult(t *testing.T) {
	reg := mapRegistry{"core.exec": okOutputModule(map[string]any{"exit_code": 0})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{
				Name:        "order",
				Module:      "core.exec.run",
				ChangedWhen: "true",
				FailedWhen:  "register.self.changed",
			},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED (failed_when увидел changed=true от changed_when)", got)
	}
}

// TestIgnoredError_VisibleDownstreamByName — downstream sees register.<name>.ignored_error:
// task A (register:X, failed_when:false) swallows the module error → task B with
// when on register.X.ignored_error runs (ignored_error is visible by register name,
// not just register.self within the same task).
func TestIgnoredError_VisibleDownstreamByName(t *testing.T) {
	var downstreamCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil), // fails with Message:"boom", failed_when:false suppresses it
		"core.service": changedModule(&downstreamCalled),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "best effort", Module: "core.exec.run", Register: "X", FailedWhen: "false"},
			{Name: "react", Module: "core.service.restarted", When: "register.X.ignored_error == 'boom'"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !downstreamCalled {
		t.Errorf("downstream-задача не выполнилась — register.X.ignored_error не виден по имени")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("downstream status = %v, want CHANGED", got)
	}
}

// TestIgnoreThenRealFail — the costliest path to a false SUCCESS: task A fails
// in the module but failed_when:false suppresses it (OK + register.A.ignored_error);
// task B fails in the module WITHOUT failed_when → real FAILED; task C does NOT
// run (fail-stop), run result RUN_STATUS_FAILED. Confirms ignore_errors mid-chain
// does NOT mask a later real failure.
func TestIgnoreThenRealFail(t *testing.T) {
	var cCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),       // A: fails, failed_when:false → ignore
		"core.service": failedModule(nil),       // B: fails WITHOUT failed_when → real FAILED
		"core.cmd":     changedModule(&cCalled), // C: must not run
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A best effort", Module: "core.exec.run", Register: "A", FailedWhen: "false"},
			{Name: "B real fail", Module: "core.service.restarted"},
			{Name: "C never", Module: "core.cmd.run"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A: swallowed error, status OK, ignored_error recorded.
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("A status = %v, want OK (ignore_errors)", got)
	}
	if got := sink.taskEvents[0].GetRegisterData().GetFields()["ignored_error"].GetStringValue(); got != "boom" {
		t.Errorf("A register.ignored_error = %q, want %q", got, "boom")
	}
	// B: real failure, fail-stop.
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("B status = %v, want FAILED", got)
	}
	// C: not executed (fail-stop after B) — arrives as SKIPPED (loop's rescue
	// pass), but mod.Apply is never called.
	if cCalled {
		t.Errorf("C исполнилась, хотя B реально упал (fail-stop не сработал)")
	}
	if len(sink.taskEvents) != 3 {
		t.Fatalf("taskEvents = %d, want 3 (A OK + B FAILED + C SKIPPED)", len(sink.taskEvents))
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("C status = %v, want SKIPPED (пропущена после провала B)", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED (ignore не маскирует последующий провал)", sink.runResult.GetStatus())
	}
}

// TestChangedWhenTrue_OnFailedModule_StaysFailed — changed_when:true on a module
// that REALLY fails (no failed_when) → task FAILED. failed outranks changed;
// changed_when:true does NOT mask a module failure. Confirms status priority.
func TestChangedWhenTrue_OnFailedModule_StaysFailed(t *testing.T) {
	reg := mapRegistry{"core.exec": failedModule(nil)}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "force changed on fail", Module: "core.exec.run", ChangedWhen: "true"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED (failed приоритетнее changed_when:true)", ev.GetStatus())
	}
	// Error source is the module itself (boom), not flow-control synthetics.
	if got := ev.GetError().GetMessage(); got != "boom" {
		t.Errorf("error.message = %q, want %q (исходная ошибка модуля)", got, "boom")
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}
