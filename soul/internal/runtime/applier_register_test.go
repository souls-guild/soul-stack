package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Материализация applier-register (orchestration.md §2.1.1, Вариант B): терминальная
// core.noop.run с непустым aggregate_of несёт СВОДНЫЙ итог дочерних destiny-задач.
// Её register_data Soul строит НЕ из ApplyEvent (noop → changed=false), а агрегатом
// (changed/failed/timed_out = OR по дочерним из registerByIdx). Тесты ниже —
// Soul-side половина инварианта (keeper-side материализация — keeper/internal/render).

// okNoopModule — core.noop-эмуляция: OK, changed=false, failed=false (как реальный
// core.noop). Терминальная задача всё равно ВЫЗЫВАЕТ Apply, но её register_data
// перезаписывается агрегатом.
func okNoopModule(called *bool) *fakeModule {
	return &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			if called != nil {
				*called = true
			}
			return stream.Send(&pluginv1.ApplyEvent{Changed: false, Failed: false})
		},
	}
}

// TestApplierRegister_AggregateChangedTrue — ★ guard (a): хотя бы одна дочерняя
// changed → агрегат .changed=true. Терминальный core.noop сам changed=false, но
// его register_data перезаписан агрегатом по дочерним [0,1].
func TestApplierRegister_AggregateChangedTrue(t *testing.T) {
	var noopCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil), // child0: changed
		"core.exec":    okOutputModule(nil),
		"core.noop":    okNoopModule(&noopCalled),
		"core.service": changedModule(nil),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "agg-changed",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "child0", Module: "core.file.present"},                                                    // 0: changed
			{Name: "child1", Module: "core.exec.run"},                                                        // 1: OK no-change
			{Name: "applier-register r", Module: "core.noop.run", Register: "r", AggregateOf: []int32{0, 1}}, // 2: терминал
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !noopCalled {
		t.Errorf("core.noop Apply не вызывался (терминал всё равно исполняется)")
	}
	term := sink.taskEvents[2]
	// Статус терминала — OK (core.noop changed=false), агрегат живёт в register_data.
	if got := term.GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("терминал status = %v, want OK (core.noop тривиален; агрегат в register_data)", got)
	}
	rd := term.GetRegisterData().GetFields()
	if !rd["changed"].GetBoolValue() {
		t.Errorf("агрегат .changed = false, want true (child0 changed)")
	}
	if rd["failed"].GetBoolValue() {
		t.Errorf("агрегат .failed = true, want false (ни одна дочерняя не упала)")
	}
	if rd["timed_out"].GetBoolValue() {
		t.Errorf("агрегат .timed_out = true, want false")
	}
	if rd["skipped"].GetBoolValue() {
		t.Errorf("агрегат .skipped = true, want false (агрегат — реальный исход, не пропуск)")
	}
}

// TestApplierRegister_AggregateChangedFalse — ★ guard (a) обратная сторона: все
// дочерние no-op/OK → агрегат .changed=false (нет ложного changed от перезаписи).
func TestApplierRegister_AggregateChangedFalse(t *testing.T) {
	reg := mapRegistry{
		"core.exec": okOutputModule(nil), // child0/child1: OK changed=false
		"core.noop": okNoopModule(nil),
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "agg-nochange",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "child0", Module: "core.exec.run"},
			{Name: "child1", Module: "core.exec.run"},
			{Name: "applier-register r", Module: "core.noop.run", Register: "r", AggregateOf: []int32{0, 1}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rd := sink.taskEvents[2].GetRegisterData().GetFields()
	if rd["changed"].GetBoolValue() {
		t.Errorf("агрегат .changed = true, want false (все дочерние no-op)")
	}
}

// TestApplierRegister_OnChangesResolvesAndReloads — ★ guard (b): внешняя задача
// onchanges:[терминал] ИСПОЛНЯЕТСЯ при changed-агрегате (раньше — недостижимо, т.к.
// register applier-а не материализовался: ErrOnChangesUnknownRegister на Keeper-е).
// При changed=false — SKIPPED (reload только при изменении).
func TestApplierRegister_OnChangesResolvesAndReloads(t *testing.T) {
	// Случай 1: дочерняя changed → агрегат changed → reload исполняется.
	var reloadCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil),           // child0: changed
		"core.noop":    okNoopModule(nil),            // терминал
		"core.service": changedModule(&reloadCalled), // reload onchanges:[терминал idx 1]
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onchanges-reload",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "child0", Module: "core.file.present"},
			{Name: "applier-register r", Module: "core.noop.run", Register: "r", AggregateOf: []int32{0}},
			{Name: "reload", Module: "core.service.restarted", OnchangesIdx: []int32{1}}, // onchanges на терминал
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reloadCalled {
		t.Errorf("reload не исполнился, хотя агрегат applier-register changed=true (onchanges на терминал не сработал)")
	}
	if got := sink.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("reload status = %v, want CHANGED", got)
	}

	// Случай 2: дочерняя no-op → агрегат changed=false → reload SKIPPED (нет flap).
	reloadCalled = false
	reg2 := mapRegistry{
		"core.exec":    okOutputModule(nil),          // child0: OK no-change
		"core.noop":    okNoopModule(nil),            // терминал
		"core.service": changedModule(&reloadCalled), // reload onchanges:[терминал]
	}
	sink2 := &recordingSink{}
	r2 := NewApplyRunner(reg2, nil)
	err = r2.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onchanges-noflap",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "child0", Module: "core.exec.run"},
			{Name: "applier-register r", Module: "core.noop.run", Register: "r", AggregateOf: []int32{0}},
			{Name: "reload", Module: "core.service.restarted", OnchangesIdx: []int32{1}},
		},
	}, sink2)
	if err != nil {
		t.Fatalf("Run (noflap): %v", err)
	}
	if reloadCalled {
		t.Errorf("reload исполнился, хотя агрегат changed=false (reload должен срабатывать ТОЛЬКО при changed)")
	}
	if got := sink2.taskEvents[2].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("reload status = %v, want SKIPPED (агрегат не changed)", got)
	}
}

// TestApplierRegister_AggregateFailedOR — ★ guard (c): .failed = OR(failed дочерних).
// Одна дочерняя падает → агрегат .failed=true. Терминал идёт ПОСЛЕ провала — но он
// onchanges-агрегат, а не обычная задача; проверяем, что register_data несёт OR-failed.
func TestApplierRegister_AggregateFailedOR(t *testing.T) {
	reg := mapRegistry{
		"core.exec": okOutputModule(nil), // child0: OK
		"core.file": failedModule(nil),   // child1: падает
		"core.noop": okNoopModule(nil),   // терминал-агрегат
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "agg-failed",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "child0", Module: "core.exec.run"},
			{Name: "child1", Module: "core.file.present"},
			{Name: "applier-register r", Module: "core.noop.run", Register: "r", AggregateOf: []int32{0, 1}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// child1 упал → прогон FAILED, но терминал-агрегат (aggregate_of, без onfail_idx)
	// — ИСКЛЮЧЕНИЕ из runFailed-skip: его register.<applier> обязан отражать реальный
	// итог destiny, иначе внешний onfail:[<applier>] разорвётся. Агрегат .failed=true.
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Fatalf("child1 status = %v, want FAILED", got)
	}
	rd := sink.taskEvents[2].GetRegisterData().GetFields()
	if !rd["failed"].GetBoolValue() {
		t.Errorf("агрегат .failed = false, want true (child1 упал — OR(failed дочерних)); терминал НЕ должен молча skip-нуться после провала прогона")
	}
}

// TestAggregateRegisterData_OR — прямой юнит agg-функции: OR по changed/failed/
// timed_out, sentinel/отсутствующий источник = нулевой вклад, skipped всегда false.
func TestAggregateRegisterData_OR(t *testing.T) {
	rd := func(changed, failed, timedOut bool) *structpb.Struct {
		return mustStruct(t, map[string]any{
			"changed": changed, "failed": failed, "timed_out": timedOut, "skipped": false,
		})
	}
	registerByIdx := map[int32]*structpb.Struct{
		0: rd(true, false, false),  // changed
		1: rd(false, true, false),  // failed
		2: rd(false, false, false), // no-op
	}

	// Все три + отсутствующий sentinel-индекс (-1): changed||failed||timed → все true
	// кроме timed_out (ни один источник не timed_out).
	got := aggregateRegisterData([]int32{0, 1, 2, -1}, registerByIdx).GetFields()
	if !got["changed"].GetBoolValue() {
		t.Errorf(".changed = false, want true (idx 0 changed)")
	}
	if !got["failed"].GetBoolValue() {
		t.Errorf(".failed = false, want true (idx 1 failed)")
	}
	if got["timed_out"].GetBoolValue() {
		t.Errorf(".timed_out = true, want false (ни один источник)")
	}
	if got["skipped"].GetBoolValue() {
		t.Errorf(".skipped = true, want false (агрегат всегда не-skipped)")
	}

	// Пустой агрегат / все no-op → все false.
	none := aggregateRegisterData([]int32{2}, registerByIdx).GetFields()
	if none["changed"].GetBoolValue() || none["failed"].GetBoolValue() || none["timed_out"].GetBoolValue() {
		t.Errorf("no-op агрегат = %v, want все false", none)
	}

	// timed_out источник → агрегат timed_out=true И failed=false (timed_out не failed
	// в источнике registerByIdx — buildRegisterData ставит и failed=true при TIMED_OUT,
	// но проверяем чистый timed_out-бит здесь).
	registerByIdx[3] = rd(false, false, true)
	timed := aggregateRegisterData([]int32{3}, registerByIdx).GetFields()
	if !timed["timed_out"].GetBoolValue() {
		t.Errorf(".timed_out = false, want true (idx 3 timed_out)")
	}
}
