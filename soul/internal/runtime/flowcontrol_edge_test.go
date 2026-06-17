package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// M2 flow-control edge — комбинации requisite-ов и взаимодействие until↔changed_when,
// отсутствовавшие в flowcontrol_test/onfail_test/retry_test. Каждый тест закрывает
// конкретный пробел из qa-gaps; уже покрытые ветки (multi-source onfail частичный
// провал → TestOnFail_MultiSource_AnyFailed, retry+failed_when:false →
// TestRetry_FailedWhenFalse_SingleAttempt, skipped→onchanges →
// TestWhenSkipped_SubsequentSeesSkippedNotChanged) здесь НЕ дублируются.

// TestWhen_Onchanges_Onfail_AllThree_AllSatisfied — все три requisite на одной
// задаче в нормальном (без провалов) прогоне. onfail требует упавшего источника —
// его нет → задача SKIPPED по AND-связке, даже когда when:true И onchanges-источник
// changed. Подтверждает, что onfail участвует в AND наравне с when/onchanges:
// gating-цепочка `!when || skipOnChanges || skipOnFail` (applyrunner.go).
func TestWhen_Onchanges_Onfail_AllThree_AllSatisfied(t *testing.T) {
	var targetCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil),           // src onchanges: changed=true
		"core.exec":    changedModule(nil),           // src onfail: OK (НЕ упал)
		"core.service": changedModule(&targetCalled), // цель: when+onchanges+onfail
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "all-three-satisfied",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "cfg", Module: "core.file.present", Register: "cfg"},
			{Name: "probe", Module: "core.exec.run", Register: "probe"},
			{
				Name:         "target",
				Module:       "core.service.restarted",
				When:         "true",
				OnchangesIdx: []int32{0}, // cfg changed → onchanges OK
				OnfailIdx:    []int32{1}, // probe НЕ упал → onfail НЕ сработал
			},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if targetCalled {
		t.Errorf("target выполнился, хотя onfail-источник не упал (AND: onfail-not-satisfied → SKIPPED)")
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("target status = %v, want SKIPPED (onfail-ветка AND не пройдена)", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult = %v, want SUCCESS", sink.runResult.GetStatus())
	}
}

// TestWhen_Onchanges_Onfail_AllThree_OnfailFiresWhenSatisfied — те же три requisite,
// но onfail-источник упал И when:true И onchanges-источник changed → все три ветки
// AND пройдены → задача исполняется. Зеркало предыдущего: onfail-задача с
// дополнительными when/onchanges-ограничениями отрабатывает, когда ВСЕ выполнены.
func TestWhen_Onchanges_Onfail_AllThree_OnfailFiresWhenSatisfied(t *testing.T) {
	var targetCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil),           // src onchanges: changed=true
		"core.exec":    failedModule(nil),            // src onfail: УПАЛ
		"core.service": changedModule(&targetCalled), // цель-rescue
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "all-three-onfail-fires",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "cfg", Module: "core.file.present", Register: "cfg"},
			{Name: "probe", Module: "core.exec.run", Register: "probe"},
			{
				Name:         "target",
				Module:       "core.service.restarted",
				When:         "true",
				OnchangesIdx: []int32{0},
				OnfailIdx:    []int32{1}, // probe упал → onfail сработал
			},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !targetCalled {
		t.Errorf("target не исполнился, хотя все три requisite (when+onchanges+onfail) выполнены")
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("target status = %v, want CHANGED", got)
	}
	// probe упал → прогон FAILED; target — onfail-rescue, его исполнение провал не отменяет.
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED (источник onfail упал)", sink.runResult.GetStatus())
	}
}

// TestWhen_Onchanges_Onfail_AllThree_WhenFalseWins — все три заданы, onfail-источник
// упал И onchanges-источник changed, но when:false → SKIPPED. when ПЕРВЫМ в
// gating-цепочке: !when коротит AND до проверки requisite-ов. Подтверждает приоритет
// when над сработавшими onchanges/onfail.
func TestWhen_Onchanges_Onfail_AllThree_WhenFalseWins(t *testing.T) {
	var targetCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil), // onchanges-источник changed
		"core.exec":    failedModule(nil),  // onfail-источник упал
		"core.service": changedModule(&targetCalled),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "all-three-when-false",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "cfg", Module: "core.file.present", Register: "cfg"},
			{Name: "probe", Module: "core.exec.run", Register: "probe"},
			{
				Name:         "target",
				Module:       "core.service.restarted",
				When:         "false", // короткое замыкание AND
				OnchangesIdx: []int32{0},
				OnfailIdx:    []int32{1},
			},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if targetCalled {
		t.Errorf("target выполнился при when:false (when должен коротить AND даже при сработавших onchanges/onfail)")
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("target status = %v, want SKIPPED", got)
	}
}

// TestUntil_WithChangedWhenOverride_Exhausted — взаимодействие until↔changed_when:
// модуль реально changed, но changed_when:false снимает changed → until
// `register.self.changed` видит ПЕРЕОПРЕДЕЛЁННЫЙ changed=false на каждой попытке →
// никогда не truthy → until_exhausted FAILED. Подтверждает, что until-eval работает
// с register.self ПОСЛЕ override (changed_when применён до until), а не с сырым
// исходом модуля. Существующий TestUntil_ChangedWhenError покрывает только
// error-ветку changed_when; здесь — happy-override влияет на until.
func TestUntil_WithChangedWhenOverride_Exhausted(t *testing.T) {
	var attempts int
	reg := mapRegistry{"core.exec": &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			attempts++
			return stream.Send(&pluginv1.ApplyEvent{Changed: true}) // сырой changed=true
		},
	}}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "until-over-changed-when",
		Tasks: []*keeperv1.RenderedTask{
			{
				Name:        "probe",
				Module:      "core.exec.run",
				RetryCount:  3,
				RetryDelay:  "1ms",
				ChangedWhen: "false",                 // снимает сырой changed
				Until:       "register.self.changed", // видит снятый changed → всегда false
			},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (until ни разу не truthy — все попытки исчерпаны)", attempts)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED (until_exhausted)", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "flowcontrol.until_exhausted" {
		t.Errorf("error.code = %q, want flowcontrol.until_exhausted", ev.GetError().GetCode())
	}
}

// TestUntil_WithChangedWhenOverride_TrueExits — обратная сторона: changed_when:true
// поднимает changed на OK-модуле → until `register.self.changed` видит true на 1-й
// попытке → выход (одна попытка, CHANGED). Доказывает, что override и в truthy-
// сторону прокидывается в until-eval.
func TestUntil_WithChangedWhenOverride_TrueExits(t *testing.T) {
	var attempts int
	reg := mapRegistry{"core.exec": &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			attempts++
			return stream.Send(&pluginv1.ApplyEvent{}) // OK, сырой changed=false
		},
	}}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "until-over-changed-when-true",
		Tasks: []*keeperv1.RenderedTask{
			{
				Name:        "probe",
				Module:      "core.exec.run",
				RetryCount:  5,
				RetryDelay:  "1ms",
				ChangedWhen: "true",                  // поднимает changed на OK-модуле
				Until:       "register.self.changed", // видит поднятый changed → true сразу
			},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (changed_when:true → until-true на 1-й попытке)", attempts)
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v, want CHANGED", got)
	}
}

// TestOnFail_SkippedSource_Skips — источник onfail SKIPPED (по when:false), а не
// failed. skipped ≠ failed → onfail НЕ срабатывает → rescue SKIPPED. Зеркало
// TestWhenSkipped (там skipped-источник не триггерит onchanges); здесь — что
// skipped-источник не триггерит и onfail. Закрывает downstream-onfail-на-skipped.
func TestOnFail_SkippedSource_Skips(t *testing.T) {
	var srcCalled, rescueCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(&srcCalled),     // A: упал бы, но when:false → SKIPPED
		"core.service": changedModule(&rescueCalled), // rescue по onfail:[A]
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onfail-skipped-source",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "A", Module: "core.exec.run", Register: "A", When: "false"},
			{Name: "rescue", Module: "core.service.restarted", OnfailIdx: []int32{0}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if srcCalled {
		t.Errorf("источник A исполнился, хотя when:false (должен быть SKIPPED)")
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Fatalf("A status = %v, want SKIPPED", got)
	}
	// skipped-источник несёт failed=false → onfail не активируется.
	if rescueCalled {
		t.Errorf("rescue исполнился на SKIPPED-источник (skipped ≠ failed — onfail не должен срабатывать)")
	}
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("rescue status = %v, want SKIPPED", got)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("runResult = %v, want SUCCESS (skipped-источник не провал)", sink.runResult.GetStatus())
	}
}
