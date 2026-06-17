package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestOnFail_NoFailures_Skips — нормальный прогон без провалов: onfail-задача
// (onfail:[A]) ВСЕГДА SKIPPED — её источник OK, rescue не нужен.
func TestOnFail_NoFailures_Skips(t *testing.T) {
	var rescueCalled bool
	reg := mapRegistry{
		"core.exec":    changedModule(nil),           // A: OK (changed)
		"core.service": changedModule(&rescueCalled), // rescue по onfail:[A]
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
		t.Errorf("rescue выполнился, хотя источник A не упал (onfail в нормальном прогоне = SKIPPED)")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("rescue status = %v, want SKIPPED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestOnFail_SourceFailed_RescueRuns — A падает → rescue B (onfail:[A]) ИСПОЛНЯЕТСЯ,
// обычная C (после A, без onfail) SKIPPED, RunResult FAILED (rescue не отменяет провал).
func TestOnFail_SourceFailed_RescueRuns(t *testing.T) {
	var rescueCalled, plainCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),            // A: падает
		"core.service": changedModule(&rescueCalled), // B: rescue по onfail:[A]
		"core.cmd":     changedModule(&plainCalled),  // C: обычная, после A
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
		t.Errorf("rescue не выполнился, хотя источник A упал")
	}
	if plainCalled {
		t.Errorf("обычная C выполнилась после провала A (должна быть SKIPPED)")
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("A status = %v, want FAILED", got)
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("rescue status = %v, want CHANGED (исполнился)", got)
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("plain status = %v, want SKIPPED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED (rescue не отменяет провал прогона)", sink.runResult.GetStatus())
	}
}

// TestOnFail_MultiSource_AnyFailed — onfail:[A,B]: упал любой → rescue исполняется
// (any-семантика, зеркало onchanges). Здесь A OK, B падает → rescue запускается.
func TestOnFail_MultiSource_AnyFailed(t *testing.T) {
	var rescueCalled bool
	reg := mapRegistry{
		"core.exec":    changedModule(nil),           // A: OK
		"core.file":    failedModule(nil),            // B: падает
		"core.service": changedModule(&rescueCalled), // rescue по onfail:[A,B]
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
		t.Errorf("rescue не выполнился, хотя один из источников (B) упал (any-семантика)")
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("rescue status = %v, want CHANGED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestOnFail_TimedOutSource_RescueRuns — источник A истёк по timeout (TIMED_OUT —
// частный случай failed) → rescue B (onfail:[A]) исполняется.
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
		"core.service": changedModule(&rescueCalled), // rescue по onfail:[A]
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
		t.Errorf("rescue не выполнился на TIMED_OUT-источник (timed_out — частный случай failed)")
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestOnFail_IgnoredErrorSource_Skips — источник A падает в модуле, но failed_when:false
// (ignore_errors) делает её OK → onfail НЕ срабатывает (источник OK, не failed),
// rescue SKIPPED, прогон SUCCESS.
func TestOnFail_IgnoredErrorSource_Skips(t *testing.T) {
	var rescueCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),            // A: падает в модуле, но failed_when:false глушит
		"core.service": changedModule(&rescueCalled), // rescue по onfail:[A]
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
		t.Fatalf("A status = %v, want OK (failed_when:false проглотил ошибку)", got)
	}
	if rescueCalled {
		t.Errorf("rescue выполнился, хотя ошибка A проглочена ignore_errors (A не failed)")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("rescue status = %v, want SKIPPED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult = %v, want SUCCESS (ignore_errors не триггерит fail-stop/onfail)", sink.runResult.GetStatus())
	}
}

// TestOnFail_MultipleFailures_RescueTailRuns — несколько провалов: A падает (первый
// failed НЕ ломает цикл), обычная B SKIPPED, rescue C (onfail:[A]) исполняется,
// обычная D после C тоже SKIPPED. Подтверждает: первый failed не break-ает цикл,
// onfail-хвост отрабатывает, обычные задачи после провала пропускаются.
func TestOnFail_MultipleFailures_RescueTailRuns(t *testing.T) {
	var bCalled, cCalled, dCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),       // A: падает
		"core.file":    changedModule(&bCalled), // B: обычная после A → SKIPPED
		"core.service": changedModule(&cCalled), // C: rescue по onfail:[A] → исполняется
		"core.cmd":     changedModule(&dCalled), // D: обычная после C → SKIPPED
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
		t.Errorf("B исполнилась после провала A (должна быть SKIPPED)")
	}
	if !cCalled {
		t.Errorf("C (rescue) не исполнилась, хотя A упал")
	}
	if dCalled {
		t.Errorf("D исполнилась после провала (должна быть SKIPPED)")
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

// TestOnFail_CancelOverFailedRun_Cancelled — центральный «не ложный SUCCESS»:
// A падает (runFailed), rescue B (onfail:[A]) исполняется, во время отправки её
// TaskEvent приходит CancelApply (onTask-hook, как в TestRun_CancelBetweenTasks).
// Следующая итерация видит runCtx.Err() != nil → cancel-break: RunResult CANCELLED,
// НЕ FAILED и НЕ зависший. Cancel прерывает цикл безусловно, перекрывая уже
// зафиксированный FAILED (applyrunner.go: cancel-проверка в начале итерации
// перезаписывает runStatus). Хвостовая plain-задача нужна, чтобы дать циклу ещё
// одну итерацию — на ней и срабатывает break (она сама уже не исполняется).
func TestOnFail_CancelOverFailedRun_Cancelled(t *testing.T) {
	var rescueCalled, tailCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),            // A: падает → runFailed
		"core.service": changedModule(&rescueCalled), // B: rescue по onfail:[A], исполняется
		"core.cmd":     changedModule(&tailCalled),   // tail: следующая итерация (не дойдёт)
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	// CancelApply приходит во время rescue-хвоста — дёргаем Cancel при отправке
	// TaskEvent задачи B (симуляция CancelApply по EventStream, как в applyrunner_test).
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
		t.Errorf("rescue не исполнился, хотя A упал (cancel пришёл уже ПОСЛЕ его запуска)")
	}
	if tailCalled {
		t.Errorf("tail-задача исполнилась, хотя cancel прервал цикл до неё")
	}
	// Cancel сработал на итерации tail → событие tail не отправлено: ровно A + rescue.
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d, want 2 (A + rescue, tail не дошёл — cancel-break)", len(sink.taskEvents))
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("A status = %v, want FAILED", got)
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("rescue status = %v, want CHANGED (исполнился до cancel)", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Errorf("runResult = %v, want CANCELLED (cancel перекрывает FAILED, НЕ ложный SUCCESS)", sink.runResult.GetStatus())
	}
}

// TestOnFail_RescueItselfFails_RunStaysFailed — провал rescue не «чинит» прогон:
// A падает → B(onfail:[A]) исполняется и САМА возвращает FAILED → B = FAILED,
// RunResult остаётся FAILED, последующая обычная C SKIPPED. runFailed уже выставлен
// провалом A, провал B его не меняет (он уже терминально true).
func TestOnFail_RescueItselfFails_RunStaysFailed(t *testing.T) {
	var rescueCalled, plainCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),           // A: падает
		"core.service": failedModule(&rescueCalled), // B: rescue по onfail:[A], тоже падает
		"core.cmd":     changedModule(&plainCalled), // C: обычная после B
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
		t.Errorf("rescue не исполнился, хотя источник A упал")
	}
	if plainCalled {
		t.Errorf("обычная C исполнилась после провала (должна быть SKIPPED)")
	}
	want := []keeperv1.TaskStatus{
		keeperv1.TaskStatus_TASK_STATUS_FAILED,  // A
		keeperv1.TaskStatus_TASK_STATUS_FAILED,  // B rescue — исполнился И упал
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
		t.Errorf("runResult = %v, want FAILED (провал rescue не отменяет провал прогона)", sink.runResult.GetStatus())
	}
}

// TestOnFail_RescueChain_RescueOfRescueRuns — onfail-цепочка (rescue на rescue):
// A падает → B(onfail:[A]) исполняется и САМА падает → C(onfail:[B]) исполняется
// (rescue rescue-а). Подтверждает композицию: register.failed задачи B (записанный
// при её провале) активирует onfail задачи C, хотя B сама — onfail-задача.
func TestOnFail_RescueChain_RescueOfRescueRuns(t *testing.T) {
	var bCalled, cCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),       // A: падает
		"core.service": failedModule(&bCalled),  // B: rescue по onfail:[A], падает
		"core.cmd":     changedModule(&cCalled), // C: rescue по onfail:[B]
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
		t.Errorf("B (rescue A) не исполнился, хотя A упал")
	}
	if !cCalled {
		t.Errorf("C (rescue B) не исполнился, хотя B упал — onfail-композиция сломана")
	}
	want := []keeperv1.TaskStatus{
		keeperv1.TaskStatus_TASK_STATUS_FAILED,  // A
		keeperv1.TaskStatus_TASK_STATUS_FAILED,  // B — исполнился И упал
		keeperv1.TaskStatus_TASK_STATUS_CHANGED, // C — rescue rescue-а исполнился
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

// TestOnFail_ForwardRef_Skips — forward-ref onfail (onfail на задачу ПОЗЖЕ по плану):
// X(onfail:[Y]), где Y идёт ПОСЛЕ X. Soul применяет последовательно — на момент X
// registerByIdx[Y] пуст, skipOnFail трактует отсутствующий источник как failed=false
// → X молча SKIPPED (no-op). Поведение НАМЕРЕННОЕ (не баг): destiny/tasks.md фиксирует
// onfail как backward-requisite. Регрессия закрепляет no-op, чтобы его случайно не
// «починили» в forward-резолв (это потребовало бы двухпроходного прогона).
func TestOnFail_ForwardRef_Skips(t *testing.T) {
	var xCalled, yCalled bool
	reg := mapRegistry{
		"core.service": changedModule(&xCalled), // X: onfail на Y (позже) → SKIPPED
		"core.exec":    failedModule(&yCalled),  // Y: падает, но уже ПОСЛЕ X
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-forward-ref",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "X", Module: "core.service.restarted", OnfailIdx: []int32{1}}, // onfail на idx 1 (Y, позже)
			{Name: "Y", Module: "core.exec.run", Register: "Y"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if xCalled {
		t.Errorf("X исполнился, хотя источник Y ещё не выполнялся (forward-ref onfail = no-op SKIPPED)")
	}
	if !yCalled {
		t.Errorf("Y не исполнился (он обычная задача, идёт первой по факту провалов)")
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("X status = %v, want SKIPPED (forward-ref onfail — намеренный no-op)", got)
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("Y status = %v, want FAILED", got)
	}
	// Y упал → прогон FAILED; X — SKIPPED, не «спас» провал Y (он шёл раньше Y).
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED (Y упал, forward-ref X его не покрыл)", sink.runResult.GetStatus())
	}
}

// TestOnFail_AndWhen_BothApplied — onfail-задача с when: gating-уется по AND:
// источник упал (onfail сработал) НО when:false → SKIPPED. Подтверждает связку
// requisites AND when (destiny/tasks.md §9).
func TestOnFail_AndWhen_BothApplied(t *testing.T) {
	var rescueCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(nil),            // A: падает → onfail сработал бы
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
		t.Errorf("rescue выполнился, хотя when:false (связка onfail && when по AND)")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("rescue status = %v, want SKIPPED (when:false)", got)
	}
}
