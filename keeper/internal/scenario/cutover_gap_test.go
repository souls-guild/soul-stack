//go:build integration

// Integration gap tests for the Strategy Y cutover (ADR-027, Phase 1
// cutover): full-roster render at claim + groupByHost SID filter. Proves Y
// closed BUG-1 (run_once on ALL hosts instead of one) and BUG-2
// (soulprint.hosts sees a single-host roster), plus the minor fixes for
// Cancel-in-planned-window and barrier-timeout of an unclaimed planned host.
// Reuses the harness from integration_test.go + cutover_test.go
// (TestMain/seed*/newAcolyteRunner/newClaimRunner/driveClaims/...).

package scenario

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// perHostCountDispatcher simulates a Soul and records the NUMBER of tasks in
// each ApplyRequest by SID (sid → tasks) + the order of SIDs. Completes the
// barrier with success status (like mockDispatcher). Concurrency-safe (the
// Acolyte pool calls SendApply from multiple workers).
type perHostCountDispatcher struct {
	t      *testing.T
	mu     sync.Mutex
	counts map[string]int
}

func newPerHostCountDispatcher(t *testing.T) *perHostCountDispatcher {
	return &perHostCountDispatcher{t: t, counts: map[string]int{}}
}

func (d *perHostCountDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	d.counts[sid] = len(req.GetTasks())
	d.mu.Unlock()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("perHostCountDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

func (d *perHostCountDispatcher) snapshot() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int, len(d.counts))
	for k, v := range d.counts {
		out[k] = v
	}
	return out
}

// newAcolyteRunnerWith is newAcolyteRunner with a given Outbound (instead of
// fakeDispatcher). Needed by the parity test: the old path calls SendApply at
// dispatch, but the new path does NOT (the Acolyte does it at claim) —
// Outbound is harmless here, we pass a counting one.
func newAcolyteRunnerWith(t *testing.T, summons SummonsPublisher, disp ApplyDispatcher) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		Essence:        essence.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       disp,
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-acolyte-test",
		Summons:        summons,
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     20 * time.Second,
	})
}

// runOnceScenario is scenario/create with a run_once task (executes on ONE
// host) + an ordinary task (on all hosts). Before Y, single-host render
// bypassed applyRunOnce (len(targeted)≤1) → the run_once task ran on every
// host.
const runOnceScenario = `name: create
description: run_once + per-host fixture
state_changes: {}
tasks:
  - name: Run once on first host
    module: core.exec.run
    run_once: true
    params:
      cmd: echo
      args: ["once"]
    changed_when: "false"
  - name: Run on every host
    module: core.exec.run
    params:
      cmd: echo
      args: ["all"]
    changed_when: "false"
`

// soulprintHostsScenario is scenario/create whose params reference
// soulprint.hosts.size() (number of hosts in the run's roster). Before Y,
// single-host render collapsed the size to 1 on every host.
const soulprintHostsScenario = `name: create
description: soulprint.hosts.size fixture
state_changes: {}
tasks:
  - name: Echo roster size
    module: core.exec.run
    params:
      cmd: "echo ${ soulprint.hosts.size() }"
    changed_when: "false"
`

// perHostWhereScenario is scenario/create with a where: task keyed on SID.
// Passes on host-a, filtered out on host-b in the same run (per-host where
// resolution in RenderForHost).
const perHostWhereScenario = `name: create
description: per-host where fixture
state_changes: {}
tasks:
  - name: Only on host-a
    module: core.exec.run
    where: "soulprint.self.sid == 'host-a.example.com'"
    params:
      cmd: echo
      args: ["a-only"]
    changed_when: "false"
`

// driveAcolyteRun starts a Runner on the new path and drives all planned
// assignments through the Acolyte to a terminal, returning the incarnation.
// n is the expected number of planned hosts.
func driveAcolyteRun(t *testing.T, r *Runner, spec RunSpec, disp ApplyDispatcher, nHosts int) *incarnation.Incarnation {
	t.Helper()
	if err := r.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForPlanned(t, spec.ApplyID, nHosts)
	cr := newClaimRunner(t, disp)
	driveClaims(t, cr, spec.ApplyID, nHosts)
	return waitRunDone(t, spec.IncarnationName, spec.ApplyID, incarnation.StatusReady)
}

// TestIntegration_TargetingParity_AcolyteVsOldPath is the KEY gap test: the
// per-host task set on the NEW path (Acolyte full-roster render +
// groupByHost) == the old path, for a scenario with run_once AND
// soulprint.hosts. Proves Y closed BUG-1 and BUG-2: single-host render would
// give run_once on both hosts and soulprint.hosts.size()==1 — full-roster
// gives a picture identical to the old path.
func TestIntegration_TargetingParity_AcolyteVsOldPath(t *testing.T) {
	const parityScenario = `name: create
description: run_once + soulprint.hosts parity fixture
state_changes: {}
tasks:
  - name: Run once on first host
    module: core.exec.run
    run_once: true
    params:
      cmd: echo
      args: ["once"]
    changed_when: "false"
  - name: Echo roster size on every host
    module: core.exec.run
    params:
      cmd: echo
      args: ["${ soulprint.hosts.size() }"]
    changed_when: "false"
`

	// --- old path (run-goroutine, direct render of the whole roster) ---
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, parityScenario)

	oldDisp := newPerHostCountDispatcher(t)
	oldRunner := newRunner(t, oldDisp, gitURL)
	oldApplyID := audit.NewULID()
	if err := oldRunner.Start(context.Background(), RunSpec{
		ApplyID:         oldApplyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("old-path Start: %v", err)
	}
	waitRunDone(t, "noop-prod", oldApplyID, incarnation.StatusReady)
	oldCounts := oldDisp.snapshot()

	// --- new path (Acolyte full-roster render + groupByHost) ---
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	// gitURL is reused — same service snapshot.

	newDisp := newPerHostCountDispatcher(t)
	r := newAcolyteRunnerWith(t, &countingSummons{}, fakeDispatcher{})
	driveAcolyteRun(t, r, RunSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}, newDisp, 2)
	newCounts := newDisp.snapshot()

	// Parity: identical task set per host.
	// Old path: host-a (run_once picks the first by SID) → 2 tasks; host-b → 1 (roster-size only).
	if oldCounts["host-a.example.com"] != 2 {
		t.Errorf("old-path host-a tasks = %d, want 2 (run_once-first + roster-size)", oldCounts["host-a.example.com"])
	}
	if oldCounts["host-b.example.com"] != 1 {
		t.Errorf("old-path host-b tasks = %d, want 1 (roster-size only, run_once trimmed)", oldCounts["host-b.example.com"])
	}
	for sid, want := range oldCounts {
		if newCounts[sid] != want {
			t.Errorf("PARITY break on %s: new path %d tasks, old %d (Y did not close BUG-1/2)",
				sid, newCounts[sid], want)
		}
	}
	if len(newCounts) != len(oldCounts) {
		t.Errorf("PARITY break: new path covered %d hosts, old %d", len(newCounts), len(oldCounts))
	}
}

// TestIntegration_RunOnce_SingleHostUnderAcolyte is a BUG-1 regression:
// run_once with acolytes>0 and roster≥2 executes on ONE host (first by SID),
// not all. Before Y, single-host render returned the run_once task for every
// host.
func TestIntegration_RunOnce_SingleHostUnderAcolyte(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, runOnceScenario)

	disp := newPerHostCountDispatcher(t)
	r := newAcolyteRunnerWith(t, &countingSummons{}, fakeDispatcher{})
	driveAcolyteRun(t, r, RunSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}, disp, 2)

	counts := disp.snapshot()
	// host-a (first by SID): run_once task + shared = 2. host-b: shared only = 1.
	if counts["host-a.example.com"] != 2 {
		t.Errorf("host-a tasks = %d, want 2 (run_once-first + common)", counts["host-a.example.com"])
	}
	if counts["host-b.example.com"] != 1 {
		t.Errorf("host-b tasks = %d, want 1 (BUG-1: run_once must NOT land on host-b)", counts["host-b.example.com"])
	}
}

// TestIntegration_SoulprintHosts_FullRosterUnderAcolyte is a BUG-2
// regression: soulprint.hosts under the new path sees the FULL roster.
// Proof: soulprint.hosts.size() renders as "2" on a two-host roster (not "1",
// which single-host render would have produced).
func TestIntegration_SoulprintHosts_FullRosterUnderAcolyte(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, soulprintHostsScenario)

	// Captures the rendered command per SID: should be "echo 2" on both hosts.
	disp := newPerHostCommandDispatcher(t)
	r := newAcolyteRunnerWith(t, &countingSummons{}, fakeDispatcher{})
	driveAcolyteRun(t, r, RunSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}, disp, 2)

	cmds := disp.snapshot()
	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		if cmds[sid] != "echo 2" {
			t.Errorf("%s rendered cmd = %q, want \"echo 2\" (BUG-2: soulprint.hosts.size() sees the full roster)", sid, cmds[sid])
		}
	}
}

// TestIntegration_PerHostWhere_TwoHosts tests per-host where on the new path:
// a task with where keyed on SID passes on host-a, is filtered out on
// host-b, in a single run. host-b → no-op no_match (no tasks, FINDING-01
// (b)), host-a → 1 task.
func TestIntegration_PerHostWhere_TwoHosts(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, perHostWhereScenario)

	disp := newPerHostCountDispatcher(t)
	r := newAcolyteRunnerWith(t, &countingSummons{}, fakeDispatcher{})
	applyID := audit.NewULID()
	driveAcolyteRun(t, r, RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}, disp, 2)

	counts := disp.snapshot()
	if counts["host-a.example.com"] != 1 {
		t.Errorf("host-a tasks = %d, want 1 (where let it through)", counts["host-a.example.com"])
	}
	// host-b: where filtered everything out → no-op success, SendApply wasn't called.
	if _, ok := counts["host-b.example.com"]; ok {
		t.Errorf("host-b received SendApply, want no-op success (where trimmed everything)")
	}
	// But the host-b row must be success (the barrier counted it as a no-op).
	st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	byStatus := map[string]applyrun.Status{}
	for _, hs := range st {
		byStatus[hs.SID] = hs.Status
	}
	if byStatus["host-b.example.com"] != applyrun.StatusNoMatch {
		t.Errorf("host-b status = %q, want no_match (FINDING-01 (b): where trimmed everything on the host)", byStatus["host-b.example.com"])
	}
}

// TestIntegration_CancelInPlannedWindow_Cancels is minor-fix (a): a Cancel
// set during the planned window (between dispatch and claim) actually
// cancels. RequestCancel now hits planned/claimed rows; the Acolyte sees the
// flag at claim time before SendApply and moves the assignment to cancelled
// (apply is NOT sent to the Soul). Run → error_locked (barrier counted
// cancelled as non-success).
func TestIntegration_CancelInPlannedWindow_Cancels(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	r := newAcolyteRunnerWith(t, &countingSummons{}, fakeDispatcher{})
	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the planned row and SET Cancel BEFORE claim (planned window).
	waitForPlanned(t, applyID, 1)
	affected, err := applyrun.RequestCancel(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected == 0 {
		t.Fatal("RequestCancel affected = 0, want >=1 (planned row) - the filter did not cover planned")
	}

	// Acolyte claims planned, sees cancel_requested before SendApply → cancelled.
	disp := &applyOnlyDispatcher{}
	cr := newClaimRunner(t, disp)
	driveClaims(t, cr, applyID, 1)

	if disp.calls.Load() != 0 {
		t.Errorf("SendApply calls = %d, want 0 (Cancel before apply was sent)", disp.calls.Load())
	}
	got, err := applyrun.SelectByApplyID(context.Background(), integrationPool, applyID, "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != applyrun.StatusCancelled {
		t.Errorf("status = %q, want cancelled (Cancel within the planned window)", got.Status)
	}
	// Run cancelled → incarnation error_locked (barrier saw a non-success terminal).
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
}

// TestIntegration_NoClaim_BarrierTimeout: a planned host that NOBODY claims
// (Acolyte pool not running) doesn't hang forever — the barrier completes on
// a short RunTimeout → error_locked (timeout). This keeps the new dispatch
// path from blocking a run forever when no Acolyte is alive.
func TestIntegration_NoClaim_BarrierTimeout(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	// Short RunTimeout: the barrier will complete on it (ClaimRunner is NOT
	// started — nobody picks up the planned assignment).
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	r := NewRunner(Deps{
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		Essence:        essence.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       fakeDispatcher{},
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-acolyte-test",
		Summons:        &countingSummons{},
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     2 * time.Second,
	})

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// No claim at all: the planned assignment sits idle, the barrier hits RunTimeout.
	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("status_details = %+v, want reason=dispatch_failed (barrier timeout)", inc.StatusDetails)
	}
}

// perHostCommandDispatcher is like perHostCountDispatcher, but records the
// rendered command of the first task per SID (to verify
// soulprint.hosts.size()).
type perHostCommandDispatcher struct {
	t    *testing.T
	mu   sync.Mutex
	cmds map[string]string
}

func newPerHostCommandDispatcher(t *testing.T) *perHostCommandDispatcher {
	return &perHostCommandDispatcher{t: t, cmds: map[string]string{}}
}

func (d *perHostCommandDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	if tasks := req.GetTasks(); len(tasks) > 0 {
		d.cmds[sid] = renderedExecCommand(tasks[0].GetParams())
	}
	d.mu.Unlock()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("perHostCommandDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

func (d *perHostCommandDispatcher) snapshot() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]string, len(d.cmds))
	for k, v := range d.cmds {
		out[k] = v
	}
	return out
}
