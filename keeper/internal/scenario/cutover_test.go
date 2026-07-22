//go:build integration

// Integration tests for apply-execution cutover on Acolyte (ADR-027, Phase
// 1.4.2/1.4.3): dispatch branching (planned+recipe+Summons), RenderForHost
// (per-host render from the recipe), and claim-execute (render→SendApply→running, no-op
// success, render-error→failed). Reuses the shared harness of integration_test.go
// (TestMain/seed*/noopServiceRepo/mockDispatcher).

package scenario

import (
	"context"
	"strings"
	"sync/atomic"
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

// countingSummons — a stub SummonsPublisher: counts publications (best-effort path
// dispatchPlanned sends one Summons after all the Inserts).
type countingSummons struct{ n atomic.Int64 }

func (s *countingSummons) PublishSummons(context.Context) error {
	s.n.Add(1)
	return nil
}

// newAcolyteRunner builds a Runner with AcolyteEnabled (the new dispatch path) and
// the given Summons publisher. Outbound must NOT be called during dispatch on
// the new path (Acolyte calls it on claim) — pass fakeDispatcher.
func newAcolyteRunner(t *testing.T, summons SummonsPublisher) *Runner {
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
		Outbound:       fakeDispatcher{},
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-acolyte-test",
		Summons:        summons,
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     20 * time.Second,
	})
}

// newClaimRunner builds a ClaimRunner over integrationPool with the given
// Outbound (mockDispatcher simulates Soul via a direct UpdateStatus).
func newClaimRunner(t *testing.T, disp ApplyDispatcher) *ClaimRunner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewClaimRunner(ClaimDeps{
		Deps: Deps{
			Loader:   artifact.NewServiceLoader(t.TempDir(), nil),
			Topology: topology.NewResolver(integrationPool, nil, nil),
			Essence:  essence.NewResolver(nil),
			Render:   render.NewPipeline(nil, engine, nil, nil),
			Outbound: disp,
			DB:       integrationPool,
		},
		KID:   "keeper-acolyte-test",
		Lease: 30 * time.Second,
		Batch: 10,
	})
}

// TestIntegration_DispatchPlanned_WritesPlannedAndSummons — new path: dispatch
// writes planned+recipe for all roster hosts (Option B), sends Summons, does NOT call
// SendApply. Acolyte (ClaimRunner) then drives the tasks → barrier → ready.
func TestIntegration_DispatchPlanned_WritesPlannedAndSummons(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	summons := &countingSummons{}
	r := newAcolyteRunner(t, summons)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"db_password": "vault:secret/db-creds#password"},
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for planned rows to appear on BOTH hosts (Option B — whole roster).
	waitForPlanned(t, applyID, 2)

	// recipe carries the vault-ref AS-IS (invariant A) on every row.
	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		got, err := applyrun.SelectByApplyID(context.Background(), integrationPool, applyID, sid)
		if err != nil {
			t.Fatalf("SelectByApplyID(%s): %v", sid, err)
		}
		if got.Recipe == nil {
			t.Fatalf("planned %s without recipe", sid)
		}
		if got.Recipe.Input["db_password"] != "vault:secret/db-creds#password" {
			t.Errorf("%s recipe carries a revealed secret instead of vault-ref: %v", sid, got.Recipe.Input["db_password"])
		}
	}

	if summons.n.Load() != 1 {
		t.Errorf("PublishSummons called %d times, want 1", summons.n.Load())
	}

	// Acolyte drives both planned → running → success (mockDispatcher simulates Soul).
	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	cr := newClaimRunner(t, disp)
	driveClaims(t, cr, applyID, 2)

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if inc.StatusDetails != nil {
		t.Errorf("status_details = %+v, want nil on success", inc.StatusDetails)
	}
	if disp.calls != 2 {
		t.Errorf("SendApply (Acolyte) calls = %d, want 2", disp.calls)
	}
}

// TestIntegration_SerialGuard_FallsBackToOldPath — a scenario with a serial task under
// AcolyteEnabled takes the OLD path: dispatch writes running + SendApply right away,
// no planned rows appear (distributed serial is Phase 3).
func TestIntegration_SerialGuard_FallsBackToOldPath(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, serialGuardScenario)

	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	// AcolyteEnabled, but the serial guard must force the old path → Outbound
	// (mockDispatcher) is called directly during dispatch.
	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := NewRunner(Deps{
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		Essence:        essence.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       disp,
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-acolyte-test",
		Summons:        &countingSummons{},
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     20 * time.Second,
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

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	// Old path: SendApply is called directly (no Acolyte), the row went through running.
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (old serial-guard path)", disp.calls)
	}
	// The old inline path (dispatchWave) does NOT set attempt — fencing epoch
	// degenerates there (no Ward-claim/recovery), on the wire attempt=0 (= old Keeper
	// without fencing, ADR-027(g), S-P2.2). This is intentional, not a bug.
	if disp.gotAttempt != 0 {
		t.Errorf("ApplyRequest.Attempt = %d, want 0 (old dispatchWave path does not fence)", disp.gotAttempt)
	}
}

// TestIntegration_RenderForHost_SingleHost — RenderForHost renders a run from a
// recipe (load→parse→essence→full-roster render) and filters down to its own SID. On
// a single-host roster, full-roster == single-host; multi-host parity is
// checked by TestIntegration_TargetingParity_AcolyteVsOldPath.
func TestIntegration_RenderForHost_SingleHost(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	deps := Deps{
		Loader:   artifact.NewServiceLoader(t.TempDir(), nil),
		Topology: topology.NewResolver(integrationPool, nil, nil),
		Essence:  essence.NewResolver(nil),
		Render:   render.NewPipeline(nil, engine, nil, nil),
		Outbound: fakeDispatcher{},
		DB:       integrationPool,
	}
	recipe := &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "create",
	}
	tasks, plans, err := RenderForHost(context.Background(), deps, recipe,
		"noop-prod", audit.NewULID(), "host-a.example.com")
	if err != nil {
		t.Fatalf("RenderForHost: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].Module != "core.exec.run" {
		t.Errorf("module = %q, want core.exec.run", tasks[0].Module)
	}
	// The plan targets exactly this SID.
	host := groupByHost(tasks, plans)["host-a.example.com"]
	if len(host) != 1 {
		t.Errorf("host-a tasks = %d, want 1", len(host))
	}
}

// TestIntegration_RenderForHost_HostNotInRoster — a host outside the roster (disconnected
// between dispatch and claim) → error (nothing to render against).
func TestIntegration_RenderForHost_HostNotInRoster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	gitURL := noopServiceRepo(t)

	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	deps := Deps{
		Loader:   artifact.NewServiceLoader(t.TempDir(), nil),
		Topology: topology.NewResolver(integrationPool, nil, nil),
		Essence:  essence.NewResolver(nil),
		Render:   render.NewPipeline(nil, engine, nil, nil),
		Outbound: fakeDispatcher{},
		DB:       integrationPool,
	}
	recipe := &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "create",
	}
	_, _, err = RenderForHost(context.Background(), deps, recipe,
		"noop-prod", audit.NewULID(), "ghost.example.com")
	if err == nil {
		t.Fatalf("RenderForHost for a host outside the roster succeeded, want an error")
	}
}

// TestIntegration_Claim_HappyPath — claiming a single planned task: render →
// MarkDispatched (claimed→dispatched) → SendApply (via applyOnlyDispatcher).
// The row goes claimed → dispatched, attempt 0→1. The dispatched mark now happens
// STRICTLY BEFORE SendApply (ADR-027 amend S3).
func TestIntegration_Claim_HappyPath(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ctx := context.Background()

	// Prepare a planned task directly (InsertPlanned), bypassing the run goroutine.
	insertPlannedFixture(t, "01HCLAIMOK", "host-a.example.com", gitURL)

	// applyOnlyDispatcher simulates Soul, BUT only counts SendApply and does NOT
	// terminate the row — this way we check exactly claimed→dispatched.
	disp := &applyOnlyDispatcher{}
	cr := newClaimRunner(t, disp)
	if err := cr.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.calls.Load() != 1 {
		t.Fatalf("SendApply calls = %d, want 1", disp.calls.Load())
	}
	// Fencing propagation (ADR-027(g)): claim puts run.Attempt into
	// ApplyRequest.Attempt. ClaimNext incremented 0→1, so on the wire attempt=1.
	if got := disp.lastAttempt.Load(); got != 1 {
		t.Errorf("ApplyRequest.Attempt = %d, want 1 (claim forwards run.Attempt)", got)
	}
	// SendApply saw the row already dispatched — the mark happens strictly BEFORE send.
	if disp.statusAtSend != string(applyrun.StatusDispatched) {
		t.Errorf("status at SendApply time = %q, want dispatched (MarkDispatched strictly BEFORE send)", disp.statusAtSend)
	}

	got, err := applyrun.SelectByApplyID(ctx, integrationPool, "01HCLAIMOK", "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != applyrun.StatusDispatched {
		t.Errorf("status = %q, want dispatched (claimed->dispatched before SendApply)", got.Status)
	}
	if got.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 (claim increments it)", got.Attempt)
	}
}

// TestIntegration_Claim_MarkDispatchedBeforeSend — order invariant: by the time
// SendApply is called, the row is already dispatched (the mark happens strictly BEFORE send). If the
// mark happened after, the dispatcher would have seen claimed.
func TestIntegration_Claim_MarkDispatchedBeforeSend(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ctx := context.Background()

	insertPlannedFixture(t, "01HORDER", "host-a.example.com", gitURL)

	disp := &applyOnlyDispatcher{}
	cr := newClaimRunner(t, disp)
	if err := cr.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.calls.Load() != 1 {
		t.Fatalf("SendApply calls = %d, want 1", disp.calls.Load())
	}
	if disp.statusAtSend != string(applyrun.StatusDispatched) {
		t.Errorf("status at SendApply time = %q, want dispatched", disp.statusAtSend)
	}
}

// TestIntegration_Claim_SendApplyFails_Failed — SendApply returned an error (Keeper
// is alive, knows delivery failed): the task terminates failed with a safe summary.
func TestIntegration_Claim_SendApplyFails_Failed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ctx := context.Background()

	insertPlannedFixture(t, "01HSENDFAIL", "host-a.example.com", gitURL)

	disp := &failingDispatcher{}
	cr := newClaimRunner(t, disp)
	if err := cr.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.calls.Load() != 1 {
		t.Fatalf("SendApply calls = %d, want 1 (a delivery attempt was made)", disp.calls.Load())
	}
	got, err := applyrun.SelectByApplyID(ctx, integrationPool, "01HSENDFAIL", "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != applyrun.StatusFailed {
		t.Errorf("status = %q, want failed (SendApply error terminalizes from dispatched)", got.Status)
	}
	if got.ErrorSummary == nil || *got.ErrorSummary != "send_apply_failed" {
		t.Errorf("error_summary = %v, want send_apply_failed", got.ErrorSummary)
	}
}

// TestIntegration_Claim_NoOpHost — where: filtered out all tasks on the host →
// claim closes the task as `no_match` without an ApplyRequest (FINDING-01 (b): a no-op
// host gets a separate benign terminal, not `success`).
func TestIntegration_Claim_NoOpHost(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, whereFalseScenario)
	ctx := context.Background()

	insertPlannedFixture(t, "01HCLAIMNOOP", "host-a.example.com", gitURL)

	disp := &applyOnlyDispatcher{}
	cr := newClaimRunner(t, disp)
	if err := cr.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.calls.Load() != 0 {
		t.Errorf("SendApply calls = %d, want 0 (no-op host)", disp.calls.Load())
	}
	got, err := applyrun.SelectByApplyID(ctx, integrationPool, "01HCLAIMNOOP", "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != applyrun.StatusNoMatch {
		t.Errorf("status = %q, want no_match (FINDING-01 (b): no-op host - separate benign terminal)", got.Status)
	}
}

// TestIntegration_Claim_RenderError_FailedMasked — a nonexistent scenario in
// the recipe → render error → failed with a masked summary (no leaked secret).
func TestIntegration_Claim_RenderError_FailedMasked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ctx := context.Background()

	// A recipe with an input secret + a reference to a NONEXISTENT scenario → render
	// fails; the summary must not leak the vault-ref.
	insertPlannedFixtureFull(t, "01HCLAIMERR", "host-a.example.com", &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "does_not_exist",
		Input:        map[string]any{"db_password": "vault:secret/db-creds#password"},
	})

	disp := &applyOnlyDispatcher{}
	cr := newClaimRunner(t, disp)
	if err := cr.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.calls.Load() != 0 {
		t.Errorf("SendApply calls = %d, want 0 (render failed before sending)", disp.calls.Load())
	}
	got, err := applyrun.SelectByApplyID(ctx, integrationPool, "01HCLAIMERR", "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != applyrun.StatusFailed {
		t.Errorf("status = %q, want failed (render error)", got.Status)
	}
	if got.ErrorSummary == nil {
		t.Fatalf("error_summary nil, want masked-summary")
	}
	if strings.Contains(*got.ErrorSummary, "vault:secret/db-creds") {
		t.Errorf("error_summary carries a bare vault-ref: %q", *got.ErrorSummary)
	}
}

// --- helpers ----------------------------------------------------------

// applyOnlyDispatcher — an Outbound that only counts SendApply and does NOT write a
// terminal status (unlike mockDispatcher): for checking exactly
// claimed→dispatched, leaving the row dispatched. Also records the row's status
// at send time (statusAtSend) — to check the "mark BEFORE send" ordering.
type applyOnlyDispatcher struct {
	calls        atomic.Int64
	lastAttempt  atomic.Int32 // attempt of the last sent ApplyRequest (fencing propagation)
	statusAtSend string       // apply_runs row status at the moment SendApply is entered
}

func (d *applyOnlyDispatcher) SendApply(ctx context.Context, _ string, req *keeperv1.ApplyRequest) error {
	d.calls.Add(1)
	d.lastAttempt.Store(req.GetAttempt())
	// Snapshot of the row status on entering send: if MarkDispatched ran BEFORE
	// SendApply (invariant S3), we'll see 'dispatched' here.
	var status string
	if err := integrationPool.QueryRow(ctx,
		`SELECT status FROM apply_runs WHERE apply_id = $1`, req.GetApplyId()).Scan(&status); err == nil {
		d.statusAtSend = status
	}
	return nil
}

// failingDispatcher — an Outbound whose SendApply always returns an error: for
// checking the failed terminal on delivery failure (claim after MarkDispatched).
type failingDispatcher struct {
	calls atomic.Int64
}

func (d *failingDispatcher) SendApply(_ context.Context, _ string, _ *keeperv1.ApplyRequest) error {
	d.calls.Add(1)
	return errSendApply
}

var errSendApply = errSendApplyT("send apply boom")

type errSendApplyT string

func (e errSendApplyT) Error() string { return string(e) }

// waitForPlanned waits for n planned rows of run applyID to appear.
func waitForPlanned(t *testing.T, applyID string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
		if err != nil {
			t.Fatalf("SelectStatusesByApplyID: %v", err)
		}
		planned := 0
		for _, s := range st {
			if s.Status == applyrun.StatusPlanned {
				planned++
			}
		}
		if planned >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d planned rows did not appear within 10s for %s", n, applyID)
}

// driveClaims loops ClaimRunner.Claim until all n tasks reach
// running/terminal (mockDispatcher terminates on SendApply).
func driveClaims(t *testing.T, cr *ClaimRunner, applyID string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := cr.Claim(context.Background()); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
		if err != nil {
			t.Fatalf("SelectStatusesByApplyID: %v", err)
		}
		terminal := 0
		for _, s := range st {
			switch s.Status {
			case applyrun.StatusSuccess, applyrun.StatusFailed, applyrun.StatusCancelled, applyrun.StatusNoMatch:
				terminal++
			}
		}
		if terminal >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("claims did not reach a terminal state within 10s for %s", applyID)
}

// insertPlannedFixture writes one planned task with a recipe for the noop scenario.
func insertPlannedFixture(t *testing.T, applyID, sid, gitURL string) {
	t.Helper()
	insertPlannedFixtureFull(t, applyID, sid, &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "create",
	})
}

func insertPlannedFixtureFull(t *testing.T, applyID, sid string, recipe *applyrun.Recipe) {
	t.Helper()
	if err := applyrun.InsertPlanned(context.Background(), integrationPool, &applyrun.ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: "noop-prod",
		Scenario: recipe.ScenarioName, Recipe: recipe,
	}); err != nil {
		t.Fatalf("InsertPlanned(%s,%s): %v", applyID, sid, err)
	}
}

// serialGuardScenario — scenario/create with a task carrying serial: (the serial guard
// forces it onto the old path even under AcolyteEnabled). The scenario name is create
// (writeServiceRepo writes exactly that).
const serialGuardScenario = `name: create
description: serial-guard fixture
state_changes: {}
tasks:
  - name: Echo with serial
    module: core.exec.run
    serial: 1
    params:
      cmd: echo
      args: ["hello"]
    changed_when: "false"
`

// whereFalseScenario — scenario/create with a task where: false → no host is
// targeted (claim performs a no-op success).
const whereFalseScenario = `name: create
description: where-false fixture
state_changes: {}
tasks:
  - name: Never targets any host
    module: core.exec.run
    where: "false"
    params:
      cmd: echo
      args: ["never"]
    changed_when: "false"
`
