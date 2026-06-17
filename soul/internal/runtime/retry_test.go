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

// seqModule — модуль с заранее заданной последовательностью исходов по номеру
// попытки (1-based). attempts считает фактические вызовы Apply. outcome задаёт
// исход i-й попытки; если попыток больше длины outcome — берётся последний.
type attemptOutcome struct {
	failed  bool // failed=true → FAILED
	changed bool // changed=true (при failed=false) → CHANGED, иначе OK
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

// TestRetry_NoUntil_SecondAttemptOK — 1-я попытка FAILED, 2-я OK → задача OK,
// ровно 2 вызова Apply.
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
	// Один TaskEvent на task_idx — промежуточные попытки наружу не эмитятся.
	if len(sink.taskEvents) != 1 {
		t.Errorf("taskEvents = %d, want 1 (промежуточные попытки не эмитятся)", len(sink.taskEvents))
	}
}

// TestRetry_NoUntil_AllFail — все попытки FAILED → финал FAILED, count вызовов.
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

// timeoutThenOKModule — первые failUntil попыток блокируются до per-attempt timeout
// (TIMED_OUT), последующие шлют OK немедленно. Для проверки «TIMED_OUT-попытка
// ретраится».
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

// TestRetry_TimedOutAttempt_Retries — попытка TIMED_OUT ретраится; первая
// блокируется до timeout, вторая успевает → задача OK.
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
		t.Errorf("attempts = %d, want 2 (TIMED_OUT 1-я ретраится, 2-я OK)", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("status = %v, want OK", got)
	}
}

// TestRetry_AllTimedOut_FinalTimedOut — все попытки TIMED_OUT → финал TIMED_OUT
// (НЕ схлопывается в FAILED).
func TestRetry_AllTimedOut_FinalTimedOut(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.cmd": timeoutThenOKModule(&attempts, 100)} // всегда timeout
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
		t.Errorf("status = %v, want TIMED_OUT (не схлопывать в FAILED)", got)
	}
}

// TestUntil_TrueFirstAttempt_Exits — until-true на 1-й попытке → выход, одна попытка.
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
		t.Errorf("attempts = %d, want 1 (until-true сразу)", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED", got)
	}
}

// TestUntil_NeverTrue_Exhausted — until-false на всех попытках → FAILED
// (until_exhausted), даже когда попытка OK.
func TestUntil_NeverTrue_Exhausted(t *testing.T) {
	var attempts int32
	// Каждая попытка OK (changed=false) → until "register.self.changed" всегда false.
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
		t.Errorf("status = %v, want FAILED (until не стал truthy)", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "flowcontrol.until_exhausted" {
		t.Errorf("error.code = %q, want flowcontrol.until_exhausted", ev.GetError().GetCode())
	}
}

// TestUntil_TrueButFailed_FinalFailed — until-true, но попытка FAILED → финал FAILED
// (until НЕ override-ит failed).
func TestUntil_TrueButFailed_FinalFailed(t *testing.T) {
	var attempts int32
	reg := mapRegistry{"core.exec": seqModule(&attempts, []attemptOutcome{{failed: true}})}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "until-true-failed",
		Tasks: []*keeperv1.RenderedTask{
			// until "true" труизм → выход на 1-й попытке, но статус failed остаётся.
			{Name: "fails", Module: "core.exec.run", RetryCount: 5, RetryDelay: "1ms",
				Until: "register.self.failed"},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (until-true сразу — self.failed==true)", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED (until не override-ит failed)", got)
	}
}

// TestUntil_TrueOnOK_OK — until-true и попытка OK → OK.
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

// TestUntil_ChangedWhenError_PreservesCode — changed_when упал runtime-error-ом CEL,
// при этом задан until:. runTask вернул терминальную ветку (selfRegister==nil) с кодом
// flowcontrol.changed_when_error. until-eval НЕ должен запускаться (нет register.self)
// и затирать исходный код на flowcontrol.until_error: одна попытка, FAILED, код
// changed_when_error сохранён (nit-фикс until-ветки runTaskWithRetry).
func TestUntil_ChangedWhenError_PreservesCode(t *testing.T) {
	var attempts int32
	// OK-исход → доходим до changed_when override; ссылка на несуществующее
	// register.self-поле → runtime-error → changed_when_error. attempts считает
	// фактические вызовы Apply (повтора быть не должно).
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
		t.Errorf("attempts = %d, want 1 (терминальная ошибка changed_when не ретраится)", got)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "flowcontrol.changed_when_error" {
		t.Errorf("error.code = %q, want flowcontrol.changed_when_error (until не должен затирать его на until_error)", ev.GetError().GetCode())
	}
}

// TestRetry_FailedWhenFalse_SingleAttempt — failed_when:false (ignore_errors) делает
// упавшую попытку OK → «не-FAILED исход» → выход на первой попытке (retry не сработал).
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
		t.Errorf("attempts = %d, want 1 (ignore_errors побеждает retry)", got)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("status = %v, want OK", got)
	}
}

// TestRetry_CancelDuringDelay_NotBlocked — cancel во время delay → выход из петли,
// задача CANCELLED, прогон не висит на полный delay.
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
	// Отменяем прогон вскоре после старта — попадём в delay (он длинный: 30s).
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
		t.Fatal("Run завис на полный retry_delay — cancel во время delay не прервал петлю")
	}

	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CANCELLED {
		t.Errorf("status = %v, want CANCELLED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Errorf("runResult = %v, want CANCELLED", sink.runResult.GetStatus())
	}
	// Должна была пройти только одна попытка (delay прервался, до 2-й не дошло).
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (cancel в первом delay)", got)
	}
}

// TestRetry_ExhaustedTriggersOnfail — retries-exhausted FAILED триггерит onfail-задачу
// (rescue), как обычный FAILED.
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
		t.Errorf("A attempts = %d, want 2 (retry исчерпан)", got)
	}
	if atomic.LoadInt32(&rescueCalled) != 1 {
		t.Errorf("rescue не сработал на retries-exhausted FAILED")
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("A status = %v, want FAILED", got)
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("rescue status = %v, want CHANGED", got)
	}
}

// TestRetry_BackwardCompat_NoRetryNoUntil — retry_count пусто/1 + until пусто → одна
// попытка, поведение как раньше.
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
