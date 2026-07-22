package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// readSafeModule is a mock SoulModule that DECLARES the read-safe capability
// (implements module.PlanReadSafe). Tracks whether Apply was called (to
// verify that dry_run never physically triggers Apply).
type readSafeModule struct {
	planChanged bool
	planErr     error
	applyCalled bool
}

func (m *readSafeModule) PlanReadSafe() {}

func (m *readSafeModule) Validate(context.Context, *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	return &pluginv1.ValidateReply{Ok: true}, nil
}

func (m *readSafeModule) Plan(_ *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	if m.planErr != nil {
		return m.planErr
	}
	return stream.Send(&pluginv1.PlanEvent{Changed: m.planChanged})
}

func (m *readSafeModule) Apply(_ *pluginv1.ApplyRequest, _ grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.applyCalled = true
	return nil
}

func dryRunTask(module string) *keeperv1.RenderedTask {
	return &keeperv1.RenderedTask{Name: "t", Module: module}
}

// TestDryRun_CallsPlanNotApply — with dry_run=true, Soul calls Plan for a
// read-safe module and Apply is NOT called (the read-only guarantee is
// structural).
func TestDryRun_CallsPlanNotApply(t *testing.T) {
	mod := &readSafeModule{planChanged: true}
	reg := mapRegistry{"core.pkg": mod}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "dry-1",
		DryRun:  true,
		Tasks:   []*keeperv1.RenderedTask{dryRunTask("core.pkg.installed")},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mod.applyCalled {
		t.Fatal("Apply called on dry_run (only Plan should be called)")
	}
	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d, want 1", len(sink.taskEvents))
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Fatalf("status = %v, want CHANGED (drift propagates from PlanEvent.changed)", got)
	}
}

// TestDryRun_ChangedFalse_OK — PlanEvent.changed=false → the task is OK (clean).
func TestDryRun_ChangedFalse_OK(t *testing.T) {
	mod := &readSafeModule{planChanged: false}
	reg := mapRegistry{"core.pkg": mod}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "dry-2",
		DryRun:  true,
		Tasks:   []*keeperv1.RenderedTask{dryRunTask("core.pkg.installed")},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mod.applyCalled {
		t.Fatal("Apply called on dry_run")
	}
	if got := sink.taskEvents[0].GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Fatalf("status = %v, want OK (clean)", got)
	}
}

// TestDryRun_DefaultDeny_NonReadSafe — a module without the read-safe
// capability (fakeModule on BaseModule) on dry_run → an EXPLICIT denial
// (FAILED, plan.unsupported), Apply is NOT called, and this is NOT a
// false-clean (not OK/CHANGED=false).
func TestDryRun_DefaultDeny_NonReadSafe(t *testing.T) {
	applyCalled := false
	mod := &fakeModule{
		applyFunc: func(*pluginv1.ApplyRequest, grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
			applyCalled = true
			return nil
		},
	}
	reg := mapRegistry{"vendor.custom": mod}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "dry-3",
		DryRun:  true,
		Tasks:   []*keeperv1.RenderedTask{dryRunTask("vendor.custom.foo")},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if applyCalled {
		t.Fatal("Apply called on dry_run of the default-deny path")
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED (explicit rejection, not false-clean)", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "plan.unsupported" {
		t.Fatalf("error.code = %q, want plan.unsupported", ev.GetError().GetCode())
	}
}

// TestDryRun_ModuleNotFound — an unknown module on dry_run → FAILED
// (module.not_found), not a false-clean.
func TestDryRun_ModuleNotFound(t *testing.T) {
	reg := mapRegistry{}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "dry-4",
		DryRun:  true,
		Tasks:   []*keeperv1.RenderedTask{dryRunTask("core.ghost.x")},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "module.not_found" {
		t.Fatalf("error.code = %q, want module.not_found", ev.GetError().GetCode())
	}
}

// TestDryRun_PlanError — a read-safe module returned an error from Plan →
// FAILED (plan.error), Apply is not called.
func TestDryRun_PlanError(t *testing.T) {
	mod := &readSafeModule{planErr: context.DeadlineExceeded}
	reg := mapRegistry{"core.pkg": mod}
	sink := &recordingSink{}
	r := NewApplyRunner(reg, nil)

	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "dry-5",
		DryRun:  true,
		Tasks:   []*keeperv1.RenderedTask{dryRunTask("core.pkg.installed")},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mod.applyCalled {
		t.Fatal("Apply called on dry_run")
	}
	ev := sink.taskEvents[0]
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED || ev.GetError().GetCode() != "plan.error" {
		t.Fatalf("status=%v code=%q, want FAILED/plan.error", ev.GetStatus(), ev.GetError().GetCode())
	}
}
