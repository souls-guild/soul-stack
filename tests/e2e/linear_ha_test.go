//go:build e2e

// L3a-MK: a linear run across two real Keeper processes (NIM-952; moved here from
// NIM-961, whose in-process test covers the same path over one PG).
//
// keeper-mk-00 serves the Operator API and so owns the run goroutine; every Soul
// holds its stream on keeper-mk-01. Both run Acolytes. The keeper.yml carries no
// scenario.execution_model, so the run is linear by default. Proven:
//   - steps 0-2 go through the work-queue (every sent row carries claim_by_kid),
//     step 3, rolled in serial: waves, goes inline — routed to the other
//     instance's streams;
//   - no row of step k starts before the last row of step k-1 finished;
//   - step 0's registers, written by keeper-mk-01 from its streams, decide step
//     1's and step 2's targets in the run goroutine on keeper-mk-00: only the
//     master is sent step 1, and only the lagging replica step 2.
package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
	"github.com/souls-guild/soul-stack/tests/e2e/internal/soulstub"
)

func TestE2E_MultiKeeper_LinearRunStepsAcrossInstances(t *testing.T) {
	const (
		serviceName = "linear-ha"
		incarnation = "linear-ha-inc"
	)

	stack := harness.NewMultiKeeperStack(t, harness.MultiKeeperConfig{Keepers: 2, Souls: 3})
	defer stack.Cleanup()

	stack.RegisterService(t, serviceName, "tests/e2e/linear-ha")

	roles := []string{"master", "lagging", "replica"}
	stubs := make([]*soulstub.Stub, len(roles))
	for i, role := range roles {
		stubs[i] = stack.ConnectSoulStubToKeeper(t, i, 1)
		stubs[i].SetApplyDefaultSuccess(true)
		stubs[i].SetTaskRegister("Probe role", map[string]any{"stdout": role, "changed": false, "failed": false})
	}

	stack.SeedIncarnationReady(t, incarnation, serviceName, "main", map[string]any{})
	for i := range roles {
		stack.AddMember(t, i, incarnation)
	}

	applyID := stack.RunScenario(t, incarnation, "steps", nil)
	stack.WaitAuditEventByPayload(t, "incarnation.run_completed", "apply_id", applyID, 90*time.Second)
	if st := stack.AuditPayloadField(t, "incarnation.run_completed", "apply_id", applyID, "status"); st != "success" {
		t.Fatalf("run %s ended %q, want success", applyID, st)
	}
	if m := stack.AuditPayloadField(t, "incarnation.run_completed", "apply_id", applyID, "execution_model"); m != "linear" {
		t.Fatalf("run %s executed %q with no model in keeper.yml, want linear", applyID, m)
	}
	stack.WaitIncarnationReady(t, incarnation, 30)

	type row struct {
		sid, status, claimBy string
		passage              int
		started, finished    time.Time
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rs, err := stack.DB().Query(ctx, `
		SELECT sid, passage, status, COALESCE(claim_by_kid, ''), started_at, finished_at
		FROM apply_runs WHERE apply_id = $1 ORDER BY passage, sid`, applyID)
	if err != nil {
		t.Fatalf("query apply_runs: %v", err)
	}
	byStep := map[int][]row{}
	for rs.Next() {
		var r row
		var finished *time.Time
		if err := rs.Scan(&r.sid, &r.passage, &r.status, &r.claimBy, &r.started, &finished); err != nil {
			rs.Close()
			t.Fatalf("scan apply_runs: %v", err)
		}
		if finished == nil {
			rs.Close()
			t.Fatalf("row %s step %d has no finished_at after the run completed", r.sid, r.passage)
		}
		r.finished = *finished
		byStep[r.passage] = append(byStep[r.passage], r)
		t.Logf("step %d %s %s claim_by_kid=%q", r.passage, r.sid, r.status, r.claimBy)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		t.Fatalf("iterate apply_runs: %v", err)
	}
	if len(byStep) != 4 {
		t.Fatalf("steps with rows = %d, want 4", len(byStep))
	}

	for k := 1; k < 4; k++ {
		var lastFinished time.Time
		for _, r := range byStep[k-1] {
			if r.finished.After(lastFinished) {
				lastFinished = r.finished
			}
		}
		for _, r := range byStep[k] {
			if r.started.Before(lastFinished) {
				t.Errorf("step %d row %s started at %s, before step %d's last row finished at %s - the barrier did not hold",
					k, r.sid, r.started.Format(time.RFC3339Nano), k-1, lastFinished.Format(time.RFC3339Nano))
			}
		}
	}

	sent := func(i int, passage int32) bool {
		for _, m := range stubs[i].Messages() {
			if req := m.Frame.GetApplyRequest(); req != nil && req.GetPassage() == passage {
				return true
			}
		}
		return false
	}
	want := map[int32][]bool{
		0: {true, true, true},
		1: {true, false, false},
		2: {false, true, false},
		3: {true, true, true},
	}
	for passage, perHost := range want {
		for i, w := range perHost {
			if got := sent(i, passage); got != w {
				t.Errorf("step %d sent to %s (%s) = %v, want %v", passage, stack.SoulSID(i), roles[i], got, w)
			}
		}
	}

	claimedBy := map[string]bool{}
	for k := 0; k < 3; k++ {
		for _, r := range byStep[k] {
			if r.status != "success" {
				t.Errorf("step %d row %s status %q, want success", k, r.sid, r.status)
			}
			sentToHost := false
			for i := range roles {
				if stack.SoulSID(i) == r.sid {
					sentToHost = sent(i, int32(k))
				}
			}
			if !sentToHost {
				continue
			}
			if r.claimBy == "" {
				t.Errorf("step %d row %s was sent without a claim - the step did not go through the work-queue", k, r.sid)
			}
			claimedBy[r.claimBy] = true
		}
	}
	for _, r := range byStep[3] {
		if r.claimBy != "" {
			t.Errorf("step 3 row %s claimed by %q - a serial: step goes inline, not through the queue", r.sid, r.claimBy)
		}
	}
	t.Logf("queued steps claimed by %v; run goroutine on %s, streams on %s", claimedBy, stack.KeeperKID(0), stack.KeeperKID(1))
}
