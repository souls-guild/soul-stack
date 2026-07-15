package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// M2 flow-control edge cases — requisite combinations and until↔changed_when
// interaction not covered by flowcontrol_test/onfail_test/retry_test. Each test
// closes a specific qa-gaps item; branches already covered elsewhere (partial
// multi-source onfail failure → TestOnFail_MultiSource_AnyFailed,
// retry+failed_when:false → TestRetry_FailedWhenFalse_SingleAttempt,
// skipped→onchanges → TestWhenSkipped_SubsequentSeesSkippedNotChanged) are NOT
// duplicated here.

// TestWhen_Onchanges_Onfail_AllThree_AllSatisfied — all three requisites on one
// task in a normal (no-failure) run. onfail requires a failed source — there is
// none → the task is SKIPPED by the AND chain, even though when:true AND the
// onchanges source changed. Confirms onfail participates in the AND alongside
// when/onchanges: gating chain `!when || skipOnChanges || skipOnFail`
// (applyrunner.go).
func TestWhen_Onchanges_Onfail_AllThree_AllSatisfied(t *testing.T) {
	var targetCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil),           // src onchanges: changed=true
		"core.exec":    changedModule(nil),           // src onfail: OK (did NOT fail)
		"core.service": changedModule(&targetCalled), // target: when+onchanges+onfail
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
				OnfailIdx:    []int32{1}, // probe did NOT fail → onfail did NOT trigger
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

// TestWhen_Onchanges_Onfail_AllThree_OnfailFiresWhenSatisfied — same three
// requisites, but the onfail source failed AND when:true AND the onchanges
// source changed → all three AND branches pass → the task runs. Mirror of the
// previous test: an onfail task with additional when/onchanges constraints
// fires when ALL of them are satisfied.
func TestWhen_Onchanges_Onfail_AllThree_OnfailFiresWhenSatisfied(t *testing.T) {
	var targetCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil),           // src onchanges: changed=true
		"core.exec":    failedModule(nil),            // src onfail: FAILED
		"core.service": changedModule(&targetCalled), // rescue target
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
				OnfailIdx:    []int32{1}, // probe failed → onfail triggered
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
	// probe failed → run is FAILED; target is the onfail rescue — its execution doesn't cancel the failure.
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("runResult = %v, want FAILED (источник onfail упал)", sink.runResult.GetStatus())
	}
}

// TestWhen_Onchanges_Onfail_AllThree_WhenFalseWins — all three set, the onfail
// source failed AND the onchanges source changed, but when:false → SKIPPED.
// when comes FIRST in the gating chain: !when short-circuits the AND before
// the requisites are checked. Confirms when takes priority over triggered
// onchanges/onfail.
func TestWhen_Onchanges_Onfail_AllThree_WhenFalseWins(t *testing.T) {
	var targetCalled bool
	reg := mapRegistry{
		"core.file":    changedModule(nil), // onchanges source changed
		"core.exec":    failedModule(nil),  // onfail source failed
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
				When:         "false", // short-circuits the AND
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

// TestUntil_WithChangedWhenOverride_Exhausted — until↔changed_when interaction:
// the module is genuinely changed, but changed_when:false clears changed →
// until `register.self.changed` sees the OVERRIDDEN changed=false on every
// attempt → never truthy → until_exhausted FAILED. Confirms until-eval works
// off register.self AFTER the override (changed_when applies before until),
// not the module's raw outcome. The existing TestUntil_ChangedWhenError only
// covers changed_when's error branch; this covers the happy-path override
// affecting until.
func TestUntil_WithChangedWhenOverride_Exhausted(t *testing.T) {
	var attempts int
	reg := mapRegistry{"core.exec": &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			attempts++
			return stream.Send(&pluginv1.ApplyEvent{Changed: true}) // raw changed=true
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
				ChangedWhen: "false",                 // clears the raw changed
				Until:       "register.self.changed", // sees the cleared changed → always false
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

// TestUntil_WithChangedWhenOverride_TrueExits — the flip side: changed_when:true
// raises changed on an OK module → until `register.self.changed` sees true on
// the 1st attempt → exits (one attempt, CHANGED). Proves the override also
// propagates into until-eval on the truthy side.
func TestUntil_WithChangedWhenOverride_TrueExits(t *testing.T) {
	var attempts int
	reg := mapRegistry{"core.exec": &fakeModule{
		applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			attempts++
			return stream.Send(&pluginv1.ApplyEvent{}) // OK, raw changed=false
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
				ChangedWhen: "true",                  // raises changed on an OK module
				Until:       "register.self.changed", // sees the raised changed → true immediately
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

// TestOnFail_SkippedSource_Skips — the onfail source is SKIPPED (via
// when:false), not failed. skipped ≠ failed → onfail does NOT trigger → rescue
// SKIPPED. Mirror of TestWhenSkipped (where a skipped source doesn't trigger
// onchanges); here — a skipped source doesn't trigger onfail either. Closes
// the downstream-onfail-on-skipped gap.
func TestOnFail_SkippedSource_Skips(t *testing.T) {
	var srcCalled, rescueCalled bool
	reg := mapRegistry{
		"core.exec":    failedModule(&srcCalled),     // A: would fail, but when:false → SKIPPED
		"core.service": changedModule(&rescueCalled), // rescue via onfail:[A]
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
	// a skipped source carries failed=false → onfail doesn't activate.
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
