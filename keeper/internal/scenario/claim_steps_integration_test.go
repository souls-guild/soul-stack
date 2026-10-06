//go:build integration

package scenario

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Guards for ADR-027 amend (p)(b)6: the claim path addresses the step it
// claimed, on the wire and in its own writes.

type stepDispatcher struct {
	mu  sync.Mutex
	req *keeperv1.ApplyRequest
}

func (d *stepDispatcher) SendApply(_ context.Context, _ string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.req = req
	return nil
}

func stepRow(t *testing.T, applyID, sid string, passage int) (string, int) {
	t.Helper()
	var (
		status  string
		attempt int
	)
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT status, attempt FROM apply_runs WHERE apply_id = $1 AND sid = $2 AND passage = $3`,
		applyID, sid, passage).Scan(&status, &attempt); err != nil {
		t.Fatalf("read %s/%s/%d: %v", applyID, sid, passage, err)
	}
	return status, attempt
}

// Step 0 of the host finished at attempt 2; the Acolyte claims step 1. Its
// ApplyRequest names step 1 — the Soul echoes passage, and that echo is how the
// step's TaskEvents and RunResult find their row — and carries an attempt above
// step 0's, which the Soul's apply_id-keyed fence requires.
func TestIntegration_Claim_StepRowCarriesItsPassage(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ctx := context.Background()
	const applyID = "01HSTEPCLAIM"

	if _, err := integrationPool.Exec(ctx, `INSERT INTO apply_runs
		(apply_id, sid, incarnation_name, scenario, status, passage, attempt)
		VALUES ($1, 'host-a.example.com', 'noop-prod', 'create', 'success', 0, 2)`, applyID); err != nil {
		t.Fatalf("seed step 0: %v", err)
	}
	if err := applyrun.InsertPlanned(ctx, integrationPool, &applyrun.ApplyRun{
		ApplyID: applyID, SID: "host-a.example.com", IncarnationName: "noop-prod", Scenario: "create",
		Passage: 1,
		Recipe: &applyrun.Recipe{
			ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
			ScenarioName: "create",
		},
	}); err != nil {
		t.Fatalf("InsertPlanned step 1: %v", err)
	}

	disp := &stepDispatcher{}
	if err := newClaimRunner(t, disp).Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.req == nil {
		t.Fatal("no ApplyRequest sent")
	}
	if disp.req.GetPassage() != 1 {
		t.Errorf("ApplyRequest.Passage = %d, want 1", disp.req.GetPassage())
	}
	if disp.req.GetAttempt() != 3 {
		t.Errorf("ApplyRequest.Attempt = %d, want 3 (above step 0's 2)", disp.req.GetAttempt())
	}
	if status, attempt := stepRow(t, applyID, "host-a.example.com", 1); status != string(applyrun.StatusDispatched) || attempt != 3 {
		t.Errorf("step 1 = %s/attempt %d, want dispatched/3", status, attempt)
	}
	if status, attempt := stepRow(t, applyID, "host-a.example.com", 0); status != string(applyrun.StatusSuccess) || attempt != 2 {
		t.Errorf("step 0 = %s/attempt %d, want success/2 (untouched)", status, attempt)
	}
}

// An Acolyte that lost its Ward to a reclaim and then fails (a slow render, a
// failed send) must not close the row the new owner has already handed to the
// Soul: its failure would overwrite the run's real outcome.
func TestIntegration_Claim_StaleOwnerFailureDoesNotCloseTheLiveRow(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ctx := context.Background()
	const applyID = "01HSTALEOWNER"

	insertPlannedFixture(t, applyID, "host-a.example.com", gitURL)
	claim := func(kid string) *applyrun.ApplyRun {
		t.Helper()
		got, err := applyrun.ClaimNext(ctx, integrationPool, kid, 30*time.Second, 10, "")
		if err != nil || len(got) != 1 {
			t.Fatalf("ClaimNext(%s): %d rows, err %v", kid, len(got), err)
		}
		return got[0]
	}
	stale := claim("keeper-1")
	if _, err := integrationPool.Exec(ctx, `UPDATE apply_runs
		SET status = 'planned', claim_by_kid = NULL, claim_at = NULL, claim_expires_at = NULL
		WHERE apply_id = $1`, applyID); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	live := claim("keeper-2")
	if err := applyrun.MarkDispatched(ctx, integrationPool, applyID, live.SID, live.Passage, live.Attempt, ""); err != nil {
		t.Fatalf("live mark: %v", err)
	}

	cr := newClaimRunner(t, &stepDispatcher{})
	cr.markFailed(ctx, stale, "send_apply_failed", slog.Default())

	if status, _ := stepRow(t, applyID, "host-a.example.com", 0); status != string(applyrun.StatusDispatched) {
		t.Errorf("row = %q after the stale owner's failure, want dispatched (the live hand-off stands)", status)
	}
}
