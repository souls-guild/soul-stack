//go:build integration

// NIM-56 regression on destroy_failed: claiming a host removed by ITS OWN
// destroy cascade is a benign no_match terminal, not failed. Discriminator is
// `souls.status` ('destroyed' ⇒ benign; any other drop from the roster ⇒
// fail-closed failure). Infrastructure (TestMain/PG, seed*, newClaimRunner,
// insertPlannedFixture, applyOnlyDispatcher) is shared with
// integration_test.go / cutover_test.go.

package scenario

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestIntegration_Claim_DestroyCascade_BenignNoMatch — a cloud-destroy cascade
// (core.cloud.destroyed) marks `souls→destroyed` BEFORE host-fan-out; Acolyte
// then claims the removed host's planned row. RenderForHost doesn't find it in
// the connected roster → without the fix that's dispatch_failed → terminal
// destroy_failed. Fix: a host with `status=='destroyed'` (sole writer is
// CascadeDestroy) is benign no_match. A real disconnect stays failed
// (fail-closed).
func TestIntegration_Claim_DestroyCascade_BenignNoMatch(t *testing.T) {
	ctx := context.Background()

	// Positive: host removed by ITS OWN destroy cascade → no_match, ApplyRequest not sent.
	t.Run("destroyed_benign_no_match", func(t *testing.T) {
		resetAll(t)
		seedOperator(t, "archon-alice")
		seedIncarnation(t, "noop-prod")
		seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
		gitURL := noopServiceRepo(t)

		applyID := audit.NewULID()
		insertPlannedFixture(t, applyID, "host-a.example.com", gitURL)
		// Emulates CascadeDestroy: host removed by the run's own destroy cascade.
		mustSetSoulStatus(t, "host-a.example.com", "destroyed")

		disp := &applyOnlyDispatcher{}
		cr := newClaimRunner(t, disp)
		if err := cr.Claim(ctx); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if disp.calls.Load() != 0 {
			t.Errorf("SendApply calls = %d, want 0 (снесённый хост — no-op)", disp.calls.Load())
		}
		got, err := applyrun.SelectByApplyID(ctx, integrationPool, applyID, "host-a.example.com")
		if err != nil {
			t.Fatalf("SelectByApplyID: %v", err)
		}
		if got.Status != applyrun.StatusNoMatch {
			t.Errorf("status = %q, want no_match (destroy-каскад — benign-терминал, не failed)", got.Status)
		}
	})

	// Negative (fail-closed): a real disconnect (not destroyed) → stays failed.
	t.Run("disconnected_fail_closed", func(t *testing.T) {
		resetAll(t)
		seedOperator(t, "archon-alice")
		seedIncarnation(t, "noop-prod")
		seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
		gitURL := noopServiceRepo(t)

		applyID := audit.NewULID()
		insertPlannedFixture(t, applyID, "host-a.example.com", gitURL)
		// disconnected between dispatch and claim — NOT a destroy cascade: the failure holds.
		mustSetSoulStatus(t, "host-a.example.com", "disconnected")

		disp := &applyOnlyDispatcher{}
		cr := newClaimRunner(t, disp)
		if err := cr.Claim(ctx); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if disp.calls.Load() != 0 {
			t.Errorf("SendApply calls = %d, want 0 (хост вне roster-а)", disp.calls.Load())
		}
		got, err := applyrun.SelectByApplyID(ctx, integrationPool, applyID, "host-a.example.com")
		if err != nil {
			t.Fatalf("SelectByApplyID: %v", err)
		}
		if got.Status != applyrun.StatusFailed {
			t.Errorf("status = %q, want failed (disconnected — fail-closed, не benign)", got.Status)
		}
	})
}

// mustSetSoulStatus sets souls.status directly (emulates the cascade writer
// without running core.cloud.*). Exactly the state CascadeDestroy leaves in
// the registry before host-fan-out (destroyed) or reaper leaves (disconnected).
func mustSetSoulStatus(t *testing.T, sid, status string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE souls SET status = $2 WHERE sid = $1`, sid, status); err != nil {
		t.Fatalf("mustSetSoulStatus(%s=%s): %v", sid, status, err)
	}
}
