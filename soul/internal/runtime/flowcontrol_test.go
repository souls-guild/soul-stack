package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// changedModule — модуль, всегда сообщающий changed=true и фиксирующий факт
// вызова Apply (для проверки «when:false → Apply не вызывался»).
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

// okOutputModule — OK-модуль (changed=false, failed=false), публикующий output-
// поля. Для проверки register.self.<output-поле> в changed_when/failed_when.
func okOutputModule(out map[string]any) *fakeModule {
	return &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			return stream.Send(&pluginv1.ApplyEvent{Output: mustStruct(nil, out)})
		},
	}
}

// failedModule — модуль, всегда возвращающий failed=true (для проверки
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

// TestWhen_False_Skips — when:"false" → задача SKIPPED, mod.Apply НЕ вызывается.
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

// TestWhen_True_Runs — when:"true" → задача выполняется (CHANGED).
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

// TestWhen_Empty_RunsUnconditionally — пустой when → безусловный запуск (regression:
// существующие destiny без when ведут себя как раньше).
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

// TestWhen_FromFlowContext — when ссылается на flow_context (input/self),
// доставленный Keeper-ом: input.do_restart && soulprint.self.os.family == 'debian'.
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

// TestWhen_RefsPreviousRegister — when второй задачи ссылается на register
// первой по ИМЕНИ (register.probe.changed). Первая (register: "probe") меняет
// state → вторая выполняется.
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

// TestWhen_RefsPreviousRegister_False — register первой задачи не changed →
// when:register.probe.changed второй = false → SKIPPED.
func TestWhen_RefsPreviousRegister_False(t *testing.T) {
	var secondCalled bool
	okModule := &fakeModule{ // probe: changed=false (OK без изменений)
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

// TestWhen_AndOnchanges_WhenTrueButOnchangesNotFired — when:true, но onchanges-
// источник не changed → SKIPPED (связка AND: исполняется только при обоих).
func TestWhen_AndOnchanges_WhenTrueButOnchangesNotFired(t *testing.T) {
	var secondCalled bool
	okModule := &fakeModule{ // источник: changed=false
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
				OnchangesIdx: []int32{0}, // источник не changed → onchanges не сработал
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

// TestWhen_AndOnchanges_BothSatisfied — when:true И onchanges-источник changed →
// задача выполняется.
func TestWhen_AndOnchanges_BothSatisfied(t *testing.T) {
	var secondCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil), // источник: changed=true
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

// TestWhen_RuntimeError_Fails — when ссылается на несуществующий register →
// runtime-error CEL → задача FAILED, прогон останавливается (templating.md §10).
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

// --- changed_when (override changed ПОСЛЕ Apply) ---

// TestChangedWhen_False_OverridesChanged — changed_when:false на CHANGED-модуле →
// итог OK (changed снят). Классический probe-кейс: задача не считается изменяющей.
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

// TestChangedWhen_True_OverridesToChanged — changed_when:true на OK-модуле → CHANGED.
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

// TestChangedWhen_DoesNotTriggerOnchanges — changed_when:false на источнике
// onchanges → handler НЕ выполняется (триггерится по переопределённому changed,
// не сырому, destiny/tasks.md §9).
func TestChangedWhen_DoesNotTriggerOnchanges(t *testing.T) {
	var handlerCalled bool
	reg := mapRegistry{
		"core.exec":    changedModule(nil),            // сырой changed=true, но changed_when:false
		"core.service": changedModule(&handlerCalled), // handler по onchanges
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

// --- failed_when (override failed ПОСЛЕ Apply) ---

// TestFailedWhen_True_OnOkModule_Fails — failed_when:true на OK-модуле → FAILED
// (искусственный провал по бизнес-условию).
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

// TestFailedWhen_False_IgnoresError — failed_when:false на упавшем модуле →
// IGNORE_ERRORS: задача OK, RunStatus SUCCESS, исходная ошибка СОХРАНЕНА только в
// register.self.ignored_error. TaskEvent.error при этом ПУСТ — контракт apply.proto
// (error заполнен только при FAILED/TIMED_OUT).
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
	// Аудит: исходная ошибка модуля не потеряна.
	if got := ev.GetRegisterData().GetFields()["ignored_error"].GetStringValue(); got != "boom" {
		t.Errorf("register.ignored_error = %q, want %q", got, "boom")
	}
	// Контракт apply.proto: TaskEvent.error пуст при не-FAILED статусе.
	if ev.GetError() != nil {
		t.Errorf("TaskEvent.error = %v, want nil (статус OK — error должен быть пуст)", ev.GetError())
	}
}

// TestFailedWhen_False_DoesNotSwallowTimedOut — failed_when:false НЕ глушит
// TIMED_OUT: таймаут инфраструктурный, остаётся терминальным fail-stop.
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

// TestChangedWhenAndFailedWhen_FailedPrioritised — обе ветки вместе: changed_when:true
// дал бы CHANGED, но failed_when:true → итог FAILED (failed приоритетнее).
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

// TestFlowControl_RegisterSelf_RuntimeError_Fails — changed_when ссылается на
// несуществующее register.self-поле → runtime-error CEL → задача FAILED
// (опечатка в имени output-поля = ошибка, templating.md §10).
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

// TestIgnoreErrors_DoesNotStopRun — задача N с failed_when:false падает в модуле,
// но прогон НЕ останавливается: задача N+1 ВЫПОЛНЯЕТСЯ (fail-stop не сработал).
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

// --- QA-пробелы покрытия ---

// TestWhenSkipped_SubsequentSeesSkippedNotChanged — задача с register:X пропущена
// по when:false → последующая видит register.X.skipped == true и changed == false
// (skipped ≠ changed; не триггерит onchanges/не врёт во when последующей).
func TestWhenSkipped_SubsequentSeesSkippedNotChanged(t *testing.T) {
	var reactByWhenCalled bool
	var reactByOnchangesCalled bool
	reg := mapRegistry{
		"core.exec":    changedModule(nil), // если бы выполнился — changed=true
		"core.service": changedModule(&reactByWhenCalled),
		"core.cmd":     changedModule(&reactByOnchangesCalled),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		Tasks: []*keeperv1.RenderedTask{
			{Name: "probe", Module: "core.exec.run", Register: "probe", When: "false"},
			// видит register.probe.skipped == true
			{Name: "react skip", Module: "core.service.restarted", When: "register.probe.skipped"},
			// onchanges на пропущенной задаче НЕ срабатывает (skipped ≠ changed)
			{Name: "react onchanges", Module: "core.cmd.run", OnchangesIdx: []int32{0}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// probe скипнут.
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("probe status = %v, want SKIPPED", got)
	}
	if sink.taskEvents[0].GetRegisterData().GetFields()["changed"].GetBoolValue() {
		t.Errorf("probe register.changed = true, want false (skipped ≠ changed)")
	}
	// when:register.probe.skipped → true → react выполнился.
	if !reactByWhenCalled {
		t.Errorf("react-by-when не выполнился, хотя register.probe.skipped == true")
	}
	// onchanges по skipped-источнику НЕ срабатывает.
	if reactByOnchangesCalled {
		t.Errorf("react-by-onchanges выполнился, хотя источник skipped (не changed)")
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("react-by-onchanges status = %v, want SKIPPED", got)
	}
}

// TestFailedWhen_RegisterSelf_RuntimeError_Fails — failed_when ссылается на
// несуществующее register.self-поле → runtime-error CEL → задача FAILED
// (симметрично changed_when_error; templating.md §10).
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

// TestFailedWhen_SeesChangedWhenResult — заявленный порядок changed_when→failed_when:
// OK-модуль + changed_when:true (переводит в changed) + failed_when:register.self.changed
// → FAILED, т.к. failed_when читает уже применённый changed_when-ом changed=true.
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

// TestIgnoredError_VisibleDownstreamByName — downstream видит register.<name>.ignored_error:
// задача A (register:X, failed_when:false) проглатывает ошибку модуля → задача B со
// when по register.X.ignored_error выполняется (видимость ignored_error по register-имени,
// не только register.self в той же задаче).
func TestIgnoredError_VisibleDownstreamByName(t *testing.T) {
	var downstreamCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil), // падает с Message:"boom", failed_when:false глушит
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

// TestIgnoreThenRealFail — самый дорогой путь к ложному SUCCESS: задача A падает в
// модуле, но failed_when:false глушит её (OK + register.A.ignored_error); задача B
// падает в модуле БЕЗ failed_when → реальный FAILED; задача C НЕ исполняется
// (fail-stop), итог прогона RUN_STATUS_FAILED. Подтверждает, что ignore_errors в
// середине цепочки НЕ маскирует последующий реальный провал.
func TestIgnoreThenRealFail(t *testing.T) {
	var cCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),       // A: падает, failed_when:false → ignore
		"core.service": failedModule(nil),       // B: падает БЕЗ failed_when → реальный FAILED
		"core.cmd":     changedModule(&cCalled), // C: не должна исполниться
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
	// A: проглоченная ошибка, статус OK, ignored_error сохранён.
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("A status = %v, want OK (ignore_errors)", got)
	}
	if got := sink.taskEvents[0].GetRegisterData().GetFields()["ignored_error"].GetStringValue(); got != "boom" {
		t.Errorf("A register.ignored_error = %q, want %q", got, "boom")
	}
	// B: реальный провал, fail-stop.
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("B status = %v, want FAILED", got)
	}
	// C: не исполнена (fail-stop после B) — приходит как SKIPPED (rescue-проход
	// цикла), но mod.Apply не вызван.
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

// TestChangedWhenTrue_OnFailedModule_StaysFailed — changed_when:true на РЕАЛЬНО
// упавшем модуле (без failed_when) → задача FAILED. failed приоритетнее changed;
// changed_when:true НЕ маскирует провал модуля. Подтверждает приоритет статусов.
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
	// Источник ошибки — сам модуль (boom), не синтетика flow-control.
	if got := ev.GetError().GetMessage(); got != "boom" {
		t.Errorf("error.message = %q, want %q (исходная ошибка модуля)", got, "boom")
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}
