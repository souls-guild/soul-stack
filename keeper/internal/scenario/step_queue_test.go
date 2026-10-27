package scenario

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

type stepQueueReply struct {
	ok    bool
	err   error
	holds bool
	// holdErr — the Conclave fails the rescue_hold question only.
	holdErr error
}

func (a stepQueueReply) AllLiveTakeStepRows(context.Context) (bool, error) { return a.ok, a.err }

func (a stepQueueReply) AllLiveHoldRescues(context.Context) (bool, error) {
	return a.holds, a.holdErr
}

func queueTask(name, register string, onfail ...string) config.Task {
	return config.Task{Name: name, Register: register, OnFail: onfail, Module: &config.ModuleTask{Module: "core.exec.run"}}
}

// ★ GUARD (NIM-961: which steps of a linear run go through the work-queue;
// NIM-993: a step that needs `rescue_hold` as well — every step of a plan holding a
// cross-step rescue unit, one with no source before it in its own message, and every
// step from the run's first failure on in a plan holding a rescue of an earlier step's
// source, a stopgap ahead of the per-step gate of ADR-027 amend (v)). Only a step with
// no `serial:`, sent to no `transport: ssh` host, on an instance running Acolytes, while
// every live instance takes step rows — and, where a rescue can hold a host, holds
// rescues too. A plan holding no rescue queues as before, whatever failed; one whose
// only rescue has a source before it in its own group queues until its first failure. A unit whose sources stand
// at earlier and later steps, or at later steps only, is cross-step. Anything else —
// including a Conclave that does not answer — goes inline.
//
// Mutations: drop any one condition → its row queues the step; drop the cross-step
// half of needsRescueHold → "cross-step unit" queues; drop its first-failure half →
// "a source in its own group after the run's first failure" queues; in
// config.CrossStepUnits, take a unit by its sources at earlier steps only → "mixed
// sources" and "a later source only" queue. That a unit's plan holds every step, the
// ones before the unit included, is the run goroutine's side, guarded by
// TestIntegration_LinearUnitPlanGoesInlineWithoutRescueHold.
func TestQueuesStep(t *testing.T) {
	agent := map[string]hostDispatch{"a": {transport: config.TransportAgent}}
	plainPlan := []config.Task{queueTask("deploy", "deploy"), queueTask("verify", "")}
	unitPlan := []config.Task{
		queueTask("deploy", "deploy"), queueTask("report", ""), queueTask("rollback", "", "deploy"), queueTask("verify", ""),
	}
	blockPlan := []config.Task{
		queueTask("deploy", "deploy"), queueTask("report", ""),
		{Name: "roll back", OnFail: []string{"deploy"}, Block: &config.BlockTask{Block: []config.Task{queueTask("undo", "", "late")}}},
		queueTask("late", "late"),
	}
	mixedPlan := []config.Task{
		queueTask("deploy", "deploy"), queueTask("mend", "", "deploy", "verify"), queueTask("report", ""), queueTask("verify", "verify"),
	}
	laterPlan := []config.Task{queueTask("mend", "", "verify"), queueTask("report", ""), queueTask("verify", "verify")}
	groupScope := config.SerialScopeBlock
	groupPlan := []config.Task{
		queueTask("deploy", "deploy"),
		{Name: "pair", Serial: 2, Settings: &config.ScenarioSettings{SerialScope: &groupScope}, Block: &config.BlockTask{Block: []config.Task{
			queueTask("stage", "stage"), queueTask("unstage", "", "stage", "deploy"),
		}}},
		queueTask("report", ""),
	}
	cases := []struct {
		name      string
		acolytes  bool
		stepQueue StepQueueChecker
		plans     []render.DispatchPlan
		hosts     map[string]hostDispatch
		hold      bool
		// plan — the step's plan, and failed whether the run failed before the step:
		// hold is then [needsRescueHold]'s.
		plan   []config.Task
		failed bool
		want   bool
	}{
		{name: "queued", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, want: true},
		{name: "no acolytes", acolytes: false, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent},
		{name: "serial", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{SerialWidth: 1}}, hosts: agent},
		{name: "push host", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}},
			hosts: map[string]hostDispatch{"a": {transport: config.TransportAgent}, "b": {transport: config.TransportSSH}}},
		{name: "no Conclave", acolytes: true, plans: []render.DispatchPlan{{}}, hosts: agent},
		{name: "an instance cannot", acolytes: true, stepQueue: stepQueueReply{ok: false}, plans: []render.DispatchPlan{{}}, hosts: agent},
		{name: "Conclave does not answer", acolytes: true, stepQueue: stepQueueReply{ok: true, err: errors.New("redis down")}, plans: []render.DispatchPlan{{}}, hosts: agent},
		{name: "a rescue can hold, an instance cannot hold rescues", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, hold: true},
		{name: "a rescue can hold, every instance holds rescues", acolytes: true, stepQueue: stepQueueReply{ok: true, holds: true}, plans: []render.DispatchPlan{{}}, hosts: agent, hold: true, want: true},
		{name: "a rescue can hold, Conclave does not answer", acolytes: true, stepQueue: stepQueueReply{ok: true, holds: true, holdErr: errors.New("redis down")}, plans: []render.DispatchPlan{{}}, hosts: agent, hold: true},
		{name: "a rescue can hold, no instance takes step rows", acolytes: true, stepQueue: stepQueueReply{holds: true}, plans: []render.DispatchPlan{{}}, hosts: agent, hold: true},
		{name: "no rescue", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: plainPlan, want: true},
		{name: "no rescue, after the run's first failure", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: plainPlan, failed: true, want: true},
		{name: "cross-step unit", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: unitPlan},
		{name: "cross-step unit, every instance holds rescues", acolytes: true, stepQueue: stepQueueReply{ok: true, holds: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: unitPlan, want: true},
		{name: "block unit whose child also names a later source", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: blockPlan},
		{name: "mixed sources", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: mixedPlan},
		{name: "mixed sources, every instance holds rescues", acolytes: true, stepQueue: stepQueueReply{ok: true, holds: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: mixedPlan, want: true},
		{name: "a later source only", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: laterPlan},
		{name: "a source in its own group", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: groupPlan, want: true},
		{name: "a source in its own group after the run's first failure", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: groupPlan, failed: true},
		{name: "a source in its own group after the run's first failure, every instance holds rescues", acolytes: true, stepQueue: stepQueueReply{ok: true, holds: true}, plans: []render.DispatchPlan{{}}, hosts: agent, plan: groupPlan, failed: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{acolyteEnabled: tc.acolytes, deps: Deps{StepQueue: tc.stepQueue}}
			hold := tc.hold
			if tc.plan != nil {
				_, steps := config.LinearPassage(tc.plan, nil)
				fails := newRunFailures()
				if tc.failed {
					fails.sources[sourceKey{"a", 0}] = &failedSource{task: "deploy", members: []int{0}, state: sourceJudged}
				}
				hold = needsRescueHold(hasCrossStepUnit(tc.plan, steps), config.RescueSteps(tc.plan, steps), fails)
			}
			if got := r.queuesStep(context.Background(), discardLog(), 1, tc.plans, tc.hosts, hold); got != tc.want {
				t.Errorf("queuesStep = %v, want %v", got, tc.want)
			}
		})
	}
}
