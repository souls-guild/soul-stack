//go:build e2e_live

// L3b E2E staged-render capstone (ADR-056 S5). S3 proved staged probe->where on
// a soul-STUB (scripted RunResult); this test - on a GENUINE soul binary in a real
// fleet of two Debian-12 containers.
//
// Service tests/e2e-live/staged-probe-live (NOT examples/** - user's WIP zone):
// scenario create is stratified by keeper into N=2 Passages by register dependency:
//   - Passage 0: probe each host's role (core.cmd.shell printf master|slave by
//     hostname) -> register: role. The REAL soul executes the command and emits
//     TaskEvent.register_data; keeper collects per-host register into apply_task_register.
//   - Passage 1: core.file.present creates a marker /tmp/acted-on-master ONLY
//     where register.role.stdout == 'master' - keeper resolves where against the FRESH per-host
//     register collected at the Passage 0 barrier.
//
// ASSERT (* capstone proof):
//  1. apply_runs success on both hosts (the staged run converged end-to-end).
//  2. register collection: apply_task_register carries stdout=master for soul-live-a and
//     stdout=slave for soul-live-b - the real soul returned register, keeper collected it.
//  3. passage-1 targeting: the marker EXISTS on the master host (soul-live-a) and is ABSENT
//     on the slave host (soul-live-b) - the Passage-1 ApplyRequest went ONLY to master.
//  4. host_state apply_runs: soul-live-a has two passage rows (0 and 1), soul-live-b's
//     passage-1 row is either no_match or absent (where filtered out the slave).
package e2e_live_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bStagedProbeLive_WhereTargetsOnlyMaster(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "tests/e2e-live/staged-probe-live",
		ServiceName: "staged-probe-live",
		Souls:       2,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 2 {
		t.Fatalf("expected 2 soul containers, got %d", got)
	}
	const (
		masterSID = "soul-live-a.example.com" // probe printf master
		slaveSID  = "soul-live-b.example.com" // probe printf slave
	)
	if got := stack.SoulContainers[0].SID; got != masterSID {
		t.Fatalf("SoulContainers[0].SID = %q, expected %q", got, masterSID)
	}
	if got := stack.SoulContainers[1].SID; got != slaveSID {
		t.Fatalf("SoulContainers[1].SID = %q, expected %q", got, slaveSID)
	}

	const incName = "test-staged-probe"

	// Membership BEFORE Create: the roster resolves members via incarnation_membership
	// (ADR-008 amendment/NIM-124). Without it, scenario sees no_hosts -> zero apply_runs rows.
	for i := range stack.SoulContainers {
		stack.AddMember(t, i, incName)
	}

	// POST /v1/incarnations auto-launches the create scenario (= the staged scenario) and
	// returns its apply_id. Both hosts announce the passage capability
	// (one beta binary), so the forward-compat gate lets the run through.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "staged-probe-live@main", nil)

	// probe (echo role) is fast; the staged loop adds one barrier. 120s with
	// margin for container cold-start and rendering two Passages.
	stack.WaitApplySuccess(t, applyID, 120)
	stack.WaitIncarnationReady(t, inc, 30)

	// -- (2) register collection for slave (probe register survived) --------
	// probe (Passage 0) - task_idx 0 in the Passage 0 ApplyRequest. register_data->>
	// 'stdout' - what the REAL soul printed and emitted in TaskEvent.register_data.
	//
	// We read register ONLY for slave: its probe role (slave) did NOT pass the Passage 1 where,
	// so slave did not execute the Passage 1 action -> its passage 0 probe register
	// (task_idx 0) was not overwritten, and we can read it post-run. master's passage 0 probe
	// register is OVERWRITTEN by the Passage 1 action (task_idx collision - finding below), so we
	// can't read it post-run. master is proven by targeting (3): where would not have selected
	// master without a correct probe register collected by keeper.
	//
	// FINDING (flagged in the report): apply_task_register PK = (apply_id, sid, task_idx) WITHOUT
	// passage, while soul emits task_idx = the task's POSITION within its OWN Passage's
	// ApplyRequest (not a global plan index). probe (P0, pos.0) and action (P1, pos.0) share
	// task_idx=0 -> ON CONFLICT overwrites the probe register with the action.
	assertRegisterStdout(t, stack, applyID, slaveSID, "slave")

	// -- (3) passage-1 targeting: marker ONLY on master -----------------------
	// This is the transitive proof that the real soul returned the probe
	// register AND keeper collected it: without a correct per-host Passage 0 register,
	// where: register.role.stdout=='master' would not have selected master for Passage 1.
	const marker = "/tmp/acted-on-master"
	stack.AssertHostFileExists(t, 0, marker) // soul-live-a (master) - present.
	stack.AssertHostFileContent(t, 0, marker, "passage-1 ran here")
	assertHostFileAbsent(t, stack, 1, marker) // soul-live-b (slave) - NOT present.

	// -- (4) apply_runs per-passage: master carried Passage 0 and 1, slave - without P1 --
	// where filtered slave out of the Passage-1 target: either there's no passage=1 row,
	// or it is no_match. master has both passage rows success.
	assertPassageStatuses(t, stack, applyID, masterSID, map[int]string{0: "success", 1: "success"})
	assertSlaveNoPassage1Apply(t, stack, applyID, slaveSID)
}

// assertRegisterStdout checks that keeper collected the Passage 0 probe task register
// (task_idx 0) for host sid with stdout=want - proof that
// the real soul returned TaskEvent.register_data, and keeper persisted it. Filtering by
// passage=0 is mandatory (see the task_idx collision finding in the main test).
func assertRegisterStdout(t *testing.T, stack *harness.Stack, applyID, sid, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout string
	err := stack.DB().QueryRow(ctx,
		`SELECT COALESCE(register_data->>'stdout','<null>') FROM apply_task_register
		 WHERE apply_id = $1 AND sid = $2 AND task_idx = 0 AND passage = 0`, applyID, sid).Scan(&stdout)
	if err != nil {
		t.Fatalf("assertRegisterStdout(%s): no register probe task passage=0 (real soul didn't return register?): %v", sid, err)
	}
	if stdout != want {
		t.Fatalf("assertRegisterStdout(%s): register stdout = %q, expected %q", sid, stdout, want)
	}
}

// assertHostFileAbsent - a negative host check: the file does NOT exist (stat exit != 0).
// Proves that the Passage-1 ApplyRequest did NOT reach this host (where filtered it out).
func assertHostFileAbsent(t *testing.T, stack *harness.Stack, soulIdx int, path string) {
	t.Helper()
	sc := stack.SoulContainers[soulIdx]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := sc.Exec(ctx, []string{"stat", "-c", "%F", path})
	if err != nil {
		t.Fatalf("assertHostFileAbsent(soulIdx=%d path=%s): exec: %v\noutput=%s", soulIdx, path, err, out)
	}
	if code == 0 {
		t.Fatalf("* assertHostFileAbsent(soulIdx=%d path=%s): file EXISTS - Passage-1 targeted the slave, where didn't work (silent-wrong-target)", soulIdx, path)
	}
}

// assertPassageStatuses checks the apply_runs row status of the host for each
// expected passage. Proves that master got BOTH probe (Passage 0) AND
// action (Passage 1) - both rows success.
func assertPassageStatuses(t *testing.T, stack *harness.Stack, applyID, sid string, want map[int]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := stack.DB().Query(ctx,
		`SELECT passage, status FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, sid)
	if err != nil {
		t.Fatalf("assertPassageStatuses(%s): query: %v", sid, err)
	}
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var p int
		var status string
		if err := rows.Scan(&p, &status); err != nil {
			t.Fatalf("assertPassageStatuses(%s): scan: %v", sid, err)
		}
		got[p] = status
	}
	for p, ws := range want {
		gs, ok := got[p]
		if !ok {
			t.Fatalf("assertPassageStatuses(%s): no apply_runs row passage=%d (got %v)", sid, p, got)
		}
		if gs != ws {
			t.Fatalf("assertPassageStatuses(%s): passage=%d status=%q, expected %q", sid, p, gs, ws)
		}
	}
}

// assertSlaveNoPassage1Apply - slave did NOT execute the Passage-1 action: where:
// register.role.stdout=='master' filtered it out. Acceptable: either there's no
// passage=1 row at all, or it's no_match (where selected 0 hosts for slave). slave's
// passage=0 (probe) row must be success.
func assertSlaveNoPassage1Apply(t *testing.T, stack *harness.Stack, applyID, sid string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := stack.DB().Query(ctx,
		`SELECT passage, status FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, sid)
	if err != nil {
		t.Fatalf("assertSlaveNoPassage1Apply(%s): query: %v", sid, err)
	}
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var p int
		var status string
		if err := rows.Scan(&p, &status); err != nil {
			t.Fatalf("assertSlaveNoPassage1Apply(%s): scan: %v", sid, err)
		}
		got[p] = status
	}
	if got[0] != "success" {
		t.Fatalf("assertSlaveNoPassage1Apply(%s): probe passage=0 status=%q, expected success", sid, got[0])
	}
	// Passage-1 row is either absent (where filtered slave out of the target), or
	// no_match. success/changed on passage=1 would mean the action ran
	// on slave - silent-wrong-target.
	if p1, ok := got[1]; ok && p1 != "no_match" {
		t.Fatalf("* assertSlaveNoPassage1Apply(%s): passage=1 status=%q - the action ran on slave (where didn't filter it out, silent-wrong-target)", sid, p1)
	}
}
