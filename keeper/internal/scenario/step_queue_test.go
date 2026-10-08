package scenario

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

type stepQueueReply struct {
	ok  bool
	err error
}

func (a stepQueueReply) AllLiveTakeStepRows(context.Context) (bool, error) { return a.ok, a.err }

// ★ GUARD (NIM-961: which steps of a linear run go through the work-queue). Only a
// step with no `serial:`, sent to no `transport: ssh` host, on an instance running
// Acolytes, while every live instance takes step rows. Anything else — including a
// Conclave that does not answer — goes inline.
//
// Mutations: drop any one condition → its row queues the step.
func TestQueuesStep(t *testing.T) {
	agent := map[string]hostDispatch{"a": {transport: config.TransportAgent}}
	cases := []struct {
		name      string
		acolytes  bool
		stepQueue StepQueueChecker
		plans     []render.DispatchPlan
		hosts     map[string]hostDispatch
		want      bool
	}{
		{name: "queued", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent, want: true},
		{name: "no acolytes", acolytes: false, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}}, hosts: agent},
		{name: "serial", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{SerialWidth: 1}}, hosts: agent},
		{name: "push host", acolytes: true, stepQueue: stepQueueReply{ok: true}, plans: []render.DispatchPlan{{}},
			hosts: map[string]hostDispatch{"a": {transport: config.TransportAgent}, "b": {transport: config.TransportSSH}}},
		{name: "no Conclave", acolytes: true, plans: []render.DispatchPlan{{}}, hosts: agent},
		{name: "an instance cannot", acolytes: true, stepQueue: stepQueueReply{ok: false}, plans: []render.DispatchPlan{{}}, hosts: agent},
		{name: "Conclave does not answer", acolytes: true, stepQueue: stepQueueReply{ok: true, err: errors.New("redis down")}, plans: []render.DispatchPlan{{}}, hosts: agent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{acolyteEnabled: tc.acolytes, deps: Deps{StepQueue: tc.stepQueue}}
			if got := r.queuesStep(context.Background(), discardLog(), 1, tc.plans, tc.hosts); got != tc.want {
				t.Errorf("queuesStep = %v, want %v", got, tc.want)
			}
		})
	}
}
