//go:build e2e

// L3a E2E: 2-passage staged-render proof (ADR-056, S3).
//
// A minimal staged probe->where scenario at contract-tier scale over live
// gRPC-mTLS streams of two soul-stubs:
//   - Passage 0 -- role probe (register: role): host-0 emits role='master',
//     host-1 -- role='slave' (per-host register via soul-stub.SetTaskRegister
//     -> TaskEvent.RegisterData with echo passage);
//   - Passage 1 -- the action `where: register.role.stdout == 'master'`.
//
// ASSERT: the Passage-1 ApplyRequest arrived ONLY at the master host
// (host-0) -- the where clause was resolved by Passage 0's per-host register
// end-to-end (render->dispatch->barrier->register->render). This proves
// staged-render at contract-tier scale: the drift "register in where is
// always empty" (ADR-056 §Context) is closed.
//
// Full live redis-cluster (cloud/vault scope) is NOT here (S4/S5). soul-stub
// simulates the Soul (per-task register + RunResult, echo passage), there is
// no real apply (L3a contract).
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
	"github.com/souls-guild/soul-stack/tests/e2e/internal/soulstub"
)

func TestE2EStagedFailover_2Passage(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "tests/e2e/staged-failover", // NOT examples/** (WIP zone)
		Souls:       2,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "staged-failover", "tests/e2e/staged-failover")

	// Live streams for both soul-stubs: the ApplyRequest of each Passage is
	// routed to the local Outbound of the target SID.
	master := stack.ConnectSoulStub(t, 0)
	slave := stack.ConnectSoulStub(t, 1)
	master.SetApplyDefaultSuccess(true)
	slave.SetApplyDefaultSuccess(true)

	// Per-host probe-register: the probe task "Probe role" emits role.stdout =
	// 'master' on host-0, 'slave' on host-1 (TaskEvent.RegisterData, echo passage 0).
	master.SetTaskRegister("Probe role", map[string]any{"stdout": "master", "changed": false, "failed": false})
	slave.SetTaskRegister("Probe role", map[string]any{"stdout": "slave", "changed": false, "failed": false})

	// Membership: both hosts are bound to the incarnation (roster via incarnation_membership, NIM-124).
	stack.AddMember(t, 0, "test-failover")
	stack.AddMember(t, 1, "test-failover")

	inc, createApply := stack.CreateIncarnationWithApply(t, "test-failover", "staged-failover@main", nil)
	// The auto-create run (POST /v1/incarnations) must FINISH BEFORE the
	// failover starts -- otherwise failover would be rejected as "already
	// applying". We wait for the create-apply_id terminal specifically,
	// then for ready (state commit).
	stack.WaitApplySuccess(t, createApply, 60)
	stack.WaitIncarnationReady(t, inc, 30)

	applyID := stack.RunScenario(t, inc, "failover", nil)

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")

	// * Passage 0 (probe): ApplyRequest arrived at BOTH hosts (probe without where).
	masterP0 := applyRequestsForPassage(master, 0)
	slaveP0 := applyRequestsForPassage(slave, 0)
	if masterP0 == 0 || slaveP0 == 0 {
		t.Fatalf("Passage 0: master ApplyRequest=%d, slave ApplyRequest=%d -- both hosts must receive the probe", masterP0, slaveP0)
	}

	// * Passage 1 (action): ApplyRequest arrived ONLY at the master host.
	// where: register.role.stdout == 'master' was resolved by Passage 0's
	// per-host register.
	masterP1 := applyRequestsForPassage(master, 1)
	slaveP1 := applyRequestsForPassage(slave, 1)
	if masterP1 == 0 {
		t.Fatalf("* Passage 1: master did NOT receive an ApplyRequest -- staged where did not target master")
	}
	if slaveP1 != 0 {
		t.Fatalf("* Passage 1: slave received %d ApplyRequest(s) -- where: register.role=='master' must NOT target slave (drift not closed)", slaveP1)
	}
}

// applyRequestsForPassage counts the ApplyRequest frames received by the
// stub for a given passage (ADR-056 echo ApplyRequest.passage). Source --
// stub.Messages() (recorded FromKeeper frames over the stream's lifetime).
func applyRequestsForPassage(stub *soulstub.Stub, passage int32) int {
	n := 0
	for _, m := range stub.Messages() {
		if req := m.Frame.GetApplyRequest(); req != nil && req.GetPassage() == passage {
			n++
		}
	}
	return n
}
