//go:build integration

package scenario

import (
	"context"
	"log/slog"
	"testing"

	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
)

// heldKeeperModule holds its Apply until released, so a test reads what the run
// recorded while the task is still running.
type heldKeeperModule struct {
	module.BaseModule
	entered chan struct{}
	release chan struct{}
}

func (m *heldKeeperModule) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	close(m.entered)
	<-m.release
	return stream.Send(&pluginv1.ApplyEvent{Changed: true})
}

// NIM-1170: a Keeper task reads as running from its first second — the hang the
// times exist to show was a Keeper task (core.ssh.apply) — and as finished once
// its module returns.
func TestIntegration_KeeperTaskTimes_StartedWhileItRuns(t *testing.T) {
	resetAll(t)
	seedIncarnation(t, "redis-prod")
	ctx := context.Background()

	const applyID = "apply-keeper-times"
	if err := applyrun.InsertRunPlan(ctx, integrationPool, applyID, []applyrun.RunPlanTask{
		{PlanIndex: 0, Name: "provision_vm", Module: "core.cloud.created"},
	}); err != nil {
		t.Fatalf("InsertRunPlan: %v", err)
	}
	mod := &heldKeeperModule{entered: make(chan struct{}), release: make(chan struct{})}
	r := keeperDispatchRunner(fakeKeeperRegistry{"core.cloud": mod})
	r.applyDB = integrationPool

	spec := RunSpec{ApplyID: applyID, IncarnationName: "redis-prod", ScenarioName: "create"}
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "provision_vm", Module: "core.cloud.created", Params: mustStruct(t, map[string]any{"provider": "fake"})},
	}
	plans := []render.DispatchPlan{{TaskIndex: 0, Keeper: true, TargetSIDs: []string{render.KeeperTargetSID}}}

	done := make(chan error, 1)
	go func() {
		done <- r.dispatchKeeperTasks(ctx, spec, nil, slog.New(slog.DiscardHandler), 0, tasks, plans, nil)
	}()
	<-mod.entered

	rows, err := applyrun.SelectRunTaskTimes(ctx, integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectRunTaskTimes: %v", err)
	}
	if len(rows) != 1 || rows[0].SID != render.KeeperTargetSID || rows[0].StartedAt == nil || rows[0].FinishedAt != nil {
		close(mod.release)
		t.Fatalf("while the task runs: rows = %+v, want one keeper row started and not finished", rows)
	}

	close(mod.release)
	if err := <-done; err != nil {
		t.Fatalf("dispatchKeeperTasks: %v", err)
	}
	rows, err = applyrun.SelectRunTaskTimes(ctx, integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectRunTaskTimes: %v", err)
	}
	if len(rows) != 1 || rows[0].FinishedAt == nil || rows[0].FinishedAt.Before(*rows[0].StartedAt) {
		t.Fatalf("after the task returned: rows = %+v, want it finished, not before it started", rows)
	}
}

// A Cancel or the run timeout ends the run's ctx while a Keeper task hangs; its
// finish is still written — it is the length the operator came to see.
func TestIntegration_KeeperTaskTimes_FinishedAfterTheRunCtxEnds(t *testing.T) {
	resetAll(t)
	seedIncarnation(t, "redis-prod")

	const applyID = "apply-keeper-times-cancel"
	if err := applyrun.InsertRunPlan(context.Background(), integrationPool, applyID, []applyrun.RunPlanTask{
		{PlanIndex: 0, Name: "provision_vm", Module: "core.cloud.created"},
	}); err != nil {
		t.Fatalf("InsertRunPlan: %v", err)
	}
	mod := &heldKeeperModule{entered: make(chan struct{}), release: make(chan struct{})}
	r := keeperDispatchRunner(fakeKeeperRegistry{"core.cloud": mod})
	r.applyDB = integrationPool

	ctx, cancel := context.WithCancel(context.Background())
	spec := RunSpec{ApplyID: applyID, IncarnationName: "redis-prod", ScenarioName: "create"}
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "provision_vm", Module: "core.cloud.created", Params: mustStruct(t, map[string]any{"provider": "fake"})},
	}
	plans := []render.DispatchPlan{{TaskIndex: 0, Keeper: true, TargetSIDs: []string{render.KeeperTargetSID}}}

	done := make(chan error, 1)
	go func() {
		done <- r.dispatchKeeperTasks(ctx, spec, nil, slog.New(slog.DiscardHandler), 0, tasks, plans, nil)
	}()
	<-mod.entered
	cancel()
	close(mod.release)
	<-done

	rows, err := applyrun.SelectRunTaskTimes(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectRunTaskTimes: %v", err)
	}
	if len(rows) != 1 || rows[0].StartedAt == nil || rows[0].FinishedAt == nil {
		t.Fatalf("rows = %+v, want the task started and finished", rows)
	}
}
