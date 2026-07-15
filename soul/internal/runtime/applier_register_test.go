package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Applier-register materialization (orchestration.md §2.1.1, Option B): a
// terminal core.noop.run with a non-empty aggregate_of carries the AGGREGATE
// outcome of its child destiny tasks. Soul builds its register_data NOT from
// ApplyEvent (noop → changed=false) but as an aggregate (changed/failed/
// timed_out = OR over children from registerByIdx). Tests below are the
// Soul-side half of the invariant (keeper-side materialization is
// keeper/internal/render).

// okNoopModule — core.noop emulation: OK, changed=false, failed=false (like
// the real core.noop). The terminal task still CALLS Apply, but its
// register_data gets overwritten by the aggregate.
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

// TestApplierRegister_AggregateChangedTrue — guard (a): at least one child
// changed → aggregate .changed=true. The terminal core.noop itself is
// changed=false, but its register_data is overwritten by the aggregate over
// children [0,1].
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
			{Name: "applier-register r", Module: "core.noop.run", Register: "r", AggregateOf: []int32{0, 1}}, // 2: terminal
		},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !noopCalled {
		t.Errorf("core.noop Apply не вызывался (терминал всё равно исполняется)")
	}
	term := sink.taskEvents[2]
	// Terminal status is OK (core.noop changed=false), the aggregate lives in register_data.
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

// TestApplierRegister_AggregateChangedFalse — guard (a) inverse: all children
// no-op/OK → aggregate .changed=false (no false changed from the overwrite).
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

// TestApplierRegister_OnChangesResolvesAndReloads — guard (b): an external
// task onchanges:[terminal] RUNS when the aggregate is changed (previously
// unreachable, since the applier's register never materialized:
// ErrOnChangesUnknownRegister on Keeper). With changed=false — SKIPPED
// (reload only on change).
func TestApplierRegister_OnChangesResolvesAndReloads(t *testing.T) {
	// Case 1: child changed → aggregate changed → reload runs.
	var reloadCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil),           // child0: changed
		"core.noop":    okNoopModule(nil),            // terminal
		"core.service": changedModule(&reloadCalled), // reload onchanges:[terminal idx 1]
	}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "onchanges-reload",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "child0", Module: "core.file.present"},
			{Name: "applier-register r", Module: "core.noop.run", Register: "r", AggregateOf: []int32{0}},
			{Name: "reload", Module: "core.service.restarted", OnchangesIdx: []int32{1}}, // onchanges on terminal
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

	// Case 2: child no-op → aggregate changed=false → reload SKIPPED (no flap).
	reloadCalled = false
	reg2 := mapRegistry{
		"core.exec":    okOutputModule(nil),          // child0: OK no-change
		"core.noop":    okNoopModule(nil),            // terminal
		"core.service": changedModule(&reloadCalled), // reload onchanges:[terminal]
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

// TestApplierRegister_AggregateFailedOR — guard (c): .failed = OR(children
// failed). One child fails → aggregate .failed=true. The terminal runs AFTER
// the failure — but it's an aggregate-of task, not an ordinary one; checks
// that register_data carries OR-failed.
func TestApplierRegister_AggregateFailedOR(t *testing.T) {
	reg := mapRegistry{
		"core.exec": okOutputModule(nil), // child0: OK
		"core.file": failedModule(nil),   // child1: fails
		"core.noop": okNoopModule(nil),   // aggregate terminal
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
	// child1 fails → the run is FAILED, but the aggregate terminal (aggregate_of,
	// no onfail_idx) is an EXCEPTION to runFailed-skip: its register.<applier>
	// must reflect the real destiny outcome, or an external onfail:[<applier>]
	// would break. Aggregate .failed=true.
	if got := sink.taskEvents[1].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Fatalf("child1 status = %v, want FAILED", got)
	}
	rd := sink.taskEvents[2].GetRegisterData().GetFields()
	if !rd["failed"].GetBoolValue() {
		t.Errorf("агрегат .failed = false, want true (child1 упал — OR(failed дочерних)); терминал НЕ должен молча skip-нуться после провала прогона")
	}
}

// TestAggregateRegisterData_OR — direct unit test of the agg function: OR
// over changed/failed/timed_out, sentinel/missing source = zero contribution,
// skipped is always false.
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

	// All three plus a missing sentinel index (-1): changed||failed||timed → all
	// true except timed_out (no source is timed_out).
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

	// Empty aggregate / all no-op → all false.
	none := aggregateRegisterData([]int32{2}, registerByIdx).GetFields()
	if none["changed"].GetBoolValue() || none["failed"].GetBoolValue() || none["timed_out"].GetBoolValue() {
		t.Errorf("no-op агрегат = %v, want все false", none)
	}

	// timed_out source → aggregate timed_out=true AND failed=false (timed_out
	// isn't failed in the registerByIdx source — buildRegisterData also sets
	// failed=true on TIMED_OUT, but here we check the pure timed_out bit).
	registerByIdx[3] = rd(false, false, true)
	timed := aggregateRegisterData([]int32{3}, registerByIdx).GetFields()
	if !timed["timed_out"].GetBoolValue() {
		t.Errorf(".timed_out = false, want true (idx 3 timed_out)")
	}
}
