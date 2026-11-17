//go:build integration

package api

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/applysink"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// NIM-1172 over the real tables: a host still at a task is in hosts[] as running,
// the TaskEvent that ends the task turns it into the result, and a host whose run
// ended without reporting the task is not listed as running.
func TestIntegration_RunTasks_RunningHosts(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedIncarnation(t, "redis-running", "redis", "archon-alice")
	ctx := context.Background()

	applyID := audit.NewULID()
	if err := applyrun.InsertRunPlan(ctx, integrationPool, applyID, []applyrun.RunPlanTask{
		{PlanIndex: 0, Name: "install", Module: "core.pkg.present"},
		{PlanIndex: 1, Name: "restart", Module: "core.service.restarted"},
	}); err != nil {
		t.Fatalf("InsertRunPlan: %v", err)
	}
	for _, sid := range []string{"a.example.com", "b.example.com"} {
		if err := applyrun.Insert(ctx, integrationPool, &applyrun.ApplyRun{
			ApplyID: applyID, SID: sid, IncarnationName: "redis-running", Scenario: "create", Status: applyrun.StatusRunning,
		}); err != nil {
			t.Fatalf("Insert(%s): %v", sid, err)
		}
		if err := applyrun.StartHostTasks(ctx, integrationPool, applyID, sid, []int{0, 1}); err != nil {
			t.Fatalf("StartHostTasks(%s): %v", sid, err)
		}
	}

	h := handlers.NewIncarnationHandler(integrationPool, nil, nil, nil, nil, nil, nil, nil)
	h.SetRunTasksAuditReader(auditpg.NewReader(integrationPool))
	read := func(step string) map[int]map[string]handlers.RunTaskHostView {
		t.Helper()
		v, err := h.RunTasksTyped(ctx, "redis-running", applyID, func(*incarnation.Incarnation) bool { return true })
		if err != nil {
			t.Fatalf("%s: RunTasksTyped: %v", step, err)
		}
		out := map[int]map[string]handlers.RunTaskHostView{}
		for _, task := range v.Tasks {
			out[task.PlanIndex] = map[string]handlers.RunTaskHostView{}
			for i, hv := range task.Hosts {
				if i > 0 && task.Hosts[i-1].SID >= hv.SID {
					t.Errorf("%s: task %d hosts out of sid order: %+v", step, task.PlanIndex, task.Hosts)
				}
				out[task.PlanIndex][hv.SID] = hv
			}
		}
		return out
	}
	requireHosts := func(step string, got map[int]map[string]handlers.RunTaskHostView, want map[int]map[string]string) {
		t.Helper()
		for idx, hosts := range want {
			if len(got[idx]) != len(hosts) {
				t.Errorf("%s: task %d hosts = %+v, want %v", step, idx, got[idx], hosts)
				continue
			}
			for sid, status := range hosts {
				hv, ok := got[idx][sid]
				if !ok || hv.Status != status || hv.StartedAt == nil {
					t.Errorf("%s: task %d host %s = %+v, want %s with a start", step, idx, sid, hv, status)
				}
				if status == handlers.TaskStatusRunning && (hv.FinishedAt != nil || hv.Output != nil || hv.Error != nil) {
					t.Errorf("%s: task %d running host %s = %+v, want its start alone", step, idx, sid, hv)
				}
			}
		}
	}

	requireHosts("sent", read("sent"), map[int]map[string]string{
		0: {"a.example.com": handlers.TaskStatusRunning, "b.example.com": handlers.TaskStatusRunning},
		1: {},
	})

	sink := applysink.New(applysink.Deps{DB: integrationPool, Audit: auditpg.NewWriter(integrationPool)})
	sink.TaskEvent(ctx, "a.example.com", &keeperv1.TaskEvent{
		ApplyId: applyID, TaskIdx: 0, PlanIndex: 0, Status: keeperv1.TaskStatus_TASK_STATUS_OK,
	})
	got := read("a reported")
	requireHosts("a reported", got, map[int]map[string]string{
		0: {"a.example.com": "TASK_STATUS_OK", "b.example.com": handlers.TaskStatusRunning},
		1: {"a.example.com": handlers.TaskStatusRunning},
	})
	if got[0]["a.example.com"].FinishedAt == nil {
		t.Errorf("a reported: task 0 result %+v, want its finish", got[0]["a.example.com"])
	}

	// b's run ends without reporting: neither its task nor the one it never reached
	// reads as running.
	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, "b.example.com", 0, applyrun.StatusFailed, nil); err != nil {
		t.Fatalf("UpdateStatus(b): %v", err)
	}
	requireHosts("b's run ended", read("b's run ended"), map[int]map[string]string{
		0: {"a.example.com": "TASK_STATUS_OK"},
		1: {"a.example.com": handlers.TaskStatusRunning},
	})
}
