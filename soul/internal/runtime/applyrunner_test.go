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

// TestRun_EchoesAttemptInRunResult — gate-1 (ADR-027(g)): Soul эхает
// ApplyRequest.attempt в итоговый RunResult.attempt, чтобы Keeper на приёме мог
// отвергнуть результат устаревшей попытки (correlateRunResult epoch-check).
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

	// attempt не задан (старый Keeper) → эхо 0, forward-compat на приёме.
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
		t.Errorf("RunResult.attempt = %d, want 0 (нет attempt в запросе)", got)
	}
}

// TestRun_NoLogEchoedToTaskEvent — [H]-фикс: Soul пробрасывает RenderedTask.NoLog
// в TaskEvent.NoLog (эхо-флаг для keeper-side audit-suppression). Проверяем оба
// исхода — успех (changed) и провал — флаг едет в обоих, и не едет, когда задача
// его не несёт.
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

	// failed-ветка: no_log тоже едет (это и есть основной канал утечки stderr).
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

// TestRun_StopsOnFirstFailed — fail-stop с rescue (destiny/tasks.md §8): первая
// failed-задача помечает прогон FAILED, последующая ОБЫЧНАЯ (без onfail) задача
// НЕ исполняется (mod.Apply не вызывается), но теперь приходит как SKIPPED-событие
// (а не отсутствует — цикл проходит до конца ради rescue-хвоста). RunResult FAILED.
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
			{Module: "core.file.present"}, // обычная задача после провала — SKIPPED, не исполняется
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if secondCalled {
		t.Errorf("вторая задача исполнилась, хотя прогон уже провален (fail-stop)")
	}
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d (want 2: FAILED + SKIPPED)", len(sink.taskEvents))
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("первая задача status = %v, want FAILED", got)
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("вторая задача status = %v, want SKIPPED (пропущена после провала)", got)
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

	// SendTaskEvent для первой задачи дёргает Cancel — это симулирует
	// CancelApply, пришедший по EventStream-у между ApplyEvent-ами.
	sink.onTask = func(ev *keeperv1.TaskEvent) {
		if ev.GetTaskIdx() == 0 {
			r.Cancel(ev.GetApplyId())
		}
	}

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "cancel-1",
		Tasks: []*keeperv1.RenderedTask{
			{Module: "core.pkg.installed"},
			{Module: "core.pkg.installed"}, // не должна выполниться
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
	// Модуль уважает ctx — блокируется до cancel, возвращает ctx.Err().
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

	// Запускаем Cancel из второй горутины — даём Run время войти в Apply.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Маленький wait, чтобы Apply успел вызваться.
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

// TestRun_TaskTimeout_TimesOut — (a) висящий модуль + RenderedTask{Timeout:"50ms"}
// → задача отваливается ПО task-timeout-у, не по scenario-ceiling. Проверяем:
// статус TIMED_OUT, register_data.timed_out, RunStatus_FAILED, и что прогон
// завершился ~timeout (< 1s), а не висел до scenario-ceiling (5 мин).
func TestRun_TaskTimeout_TimesOut(t *testing.T) {
	reg := mapRegistry{
		"core.exec": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				// Висим до отмены ctx (per-task timeout его отменит).
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
		t.Fatalf("прогон завис на %v — отвалился по scenario-ceiling, не по task-timeout", elapsed)
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
		t.Errorf("register_data.failed != true (timed_out — частный случай failed)")
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult.status = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestRun_TaskTimeout_EmptyNoLimit — (b) timeout="" → per-task лимита нет, быстрый
// модуль выполняется штатно (не отменяется).
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

// TestRun_TaskTimeout_DaySuffix — (b') суффикс `<N>d` (convention `duration`
// Soul Stack) распознаётся тем же config.ParseDuration, что keeper применяет при
// парсе destiny. `1d` = валидный большой лимит → модуль выполняется штатно, не
// падает на «невалидной duration» (как было бы с голым time.ParseDuration,
// который `1d` не понимает). Подтверждает: Soul-парсер = keeper-валидатор.
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
		t.Errorf("status = %v, want CHANGED (`1d` распознан как валидный лимит)", sink.taskEvents[0].GetStatus())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult.status = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestRun_TaskTimeout_NonPositiveNoLimit — (b”) `0s` (и любой d <= 0) трактуется
// как «лимита нет», а НЕ как мгновенный дедлайн. Без guard `d > 0` WithTimeout(ctx, 0)
// истёк бы немедленно → ложный TIMED_OUT ещё до запуска модуля.
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
		t.Errorf("status = %v, want CHANGED (`0s` = лимита нет, не мгновенный TIMED_OUT)", sink.taskEvents[0].GetStatus())
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult.status = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestRun_TaskTimeout_NotMaskedAsCancel — (c) истёкший ДОЧЕРНИЙ per-task ctx при
// живом РОДИТЕЛЬСКОМ ctx даёт TIMED_OUT, а не CANCELLED. Это центральный
// инвариант: timeout не должен попасть под ветку cancel в Run (runCtx.Err()
// остаётся nil, т.к. taskCtx дочерний).
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

	// Родительский ctx НЕ отменяем — он остаётся живым весь прогон.
	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "to-cancel-distinct",
		Tasks:   []*keeperv1.RenderedTask{{Name: "hang", Module: "core.exec.run", Timeout: "30ms"}},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_CANCELLED {
		t.Fatalf("status = CANCELLED — timeout замаскировался под cancel")
	}
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT {
		t.Errorf("status = %v, want TIMED_OUT", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "task.timed_out" {
		t.Errorf("error.code = %q, want task.timed_out", ev.GetError().GetCode())
	}
	if sink.runResult.GetStatus() == keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Fatalf("runResult.status = CANCELLED — прогон замаскировал timeout под cancel")
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult.status = %v, want FAILED", sink.runResult.GetStatus())
	}
}

// TestRun_TaskTimeout_InvalidDuration — (d) невалидная duration-строка трактуется
// как «не задан» (лимита нет), Run не падает на служебной ошибке, быстрый модуль
// выполняется штатно.
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
		t.Fatalf("Run: %v (не должен падать на невалидной duration)", err)
	}
	if sink.taskEvents[0].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED (невалидный timeout = не задан)", sink.taskEvents[0].GetStatus())
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
	// Гоняем Cancel-ы параллельно с одним Run-ом — race-детектор должен
	// промолчать. Тест не валидирует timing-логику (это в CancelDuringTask),
	// здесь только проверяем отсутствие data race на map active.
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

// TestRun_OnChanges_SourceChanged_Runs — источник changed → onchanges-задача
// выполняется (mod.Apply вызван), статус не SKIPPED.
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
		t.Fatal("restart task НЕ выполнена, хотя источник changed (mod.Apply должен быть вызван)")
	}
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d, want 2", len(sink.taskEvents))
	}
	if sink.taskEvents[1].GetStatus() == keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("restart status = SKIPPED, want выполнена (источник changed)")
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult.status = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestRun_OnChanges_SourceUnchanged_Skipped — источник unchanged (OK без changed)
// → onchanges-задача ПРОПУСКАЕТСЯ: статус SKIPPED, mod.Apply НЕ вызван,
// register.skipped == true, register.changed == false. Это центр фикса
// restart-flap.
func TestRun_OnChanges_SourceUnchanged_Skipped(t *testing.T) {
	restarted := false
	reg := mapRegistry{
		"core.file": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				// OK без changes — конфиг уже в нужном состоянии.
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
		t.Fatal("restart task ВЫПОЛНЕНА (mod.Apply вызван), хотя источник unchanged — restart-flap не исправлен")
	}
	if len(sink.taskEvents) != 2 {
		t.Fatalf("taskEvents = %d, want 2 (skipped задача тоже шлёт TaskEvent)", len(sink.taskEvents))
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
		t.Errorf("register.changed == true у skipped задачи (skipped ≠ changed)")
	}
	if rd["failed"].GetBoolValue() {
		t.Errorf("register.failed == true у skipped задачи")
	}
	// SKIPPED — не fail: прогон успешен.
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult.status = %v, want SUCCESS (skip ≠ fail)", sink.runResult.GetStatus())
	}
}

// TestRun_OnChanges_MultiSource_AnyChanged — несколько источников, хотя бы один
// changed → onchanges-задача выполняется (any-семантика, destiny/tasks.md §8).
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
				return stream.Send(&pluginv1.ApplyEvent{Changed: true}) // этот изменился
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
		t.Fatal("onchanges-задача НЕ выполнена, хотя один из источников changed (any-семантика)")
	}
	if sink.taskEvents[2].GetStatus() == keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("restart status = SKIPPED, want выполнена")
	}
}

// TestRun_OnChanges_SkippedDoesNotTrigger — skipped-источник НЕ триггерит
// onchanges дальше (skipped ≠ changed): задача A пропущена по своему onchanges,
// задача B с onchanges на A → тоже пропущена (А не дала changed).
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
			{Name: "a", Module: "core.service.restarted", OnchangesIdx: []int32{0}}, // idx 1: skipped (источник unchanged)
			{Name: "b", Module: "core.cmd.run", OnchangesIdx: []int32{1}},           // idx 2: onchanges на skipped a → тоже skip
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran["a"] {
		t.Error("задача a выполнена, хотя её источник unchanged")
	}
	if ran["b"] {
		t.Error("задача b выполнена — skipped-источник a НЕ должен триггерить onchanges (skipped ≠ changed)")
	}
	if sink.taskEvents[1].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_SKIPPED ||
		sink.taskEvents[2].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("statuses = [%v %v], want [SKIPPED SKIPPED]",
			sink.taskEvents[1].GetStatus(), sink.taskEvents[2].GetStatus())
	}
}

// TestSkipOnChanges — unit табличный тест gating-предиката.
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
		{"пусто onchanges → run", nil, nil, false},
		{"источник changed → run", []int32{0}, map[int32]*structpb.Struct{0: changed}, false},
		{"источник unchanged → skip", []int32{0}, map[int32]*structpb.Struct{0: unchanged}, true},
		{"any changed → run", []int32{0, 1}, map[int32]*structpb.Struct{0: unchanged, 1: changed}, false},
		{"все unchanged → skip", []int32{0, 1}, map[int32]*structpb.Struct{0: unchanged, 1: unchanged}, true},
		{"skipped-источник → skip", []int32{0}, map[int32]*structpb.Struct{0: skipped}, true},
		{"отсутствует источник → skip", []int32{0}, map[int32]*structpb.Struct{}, true},
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
	// onTask — hook, вызывается до записи event-а в taskEvents. Используется
	// тестами cancel-логики для симуляции CancelApply между задачами.
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
