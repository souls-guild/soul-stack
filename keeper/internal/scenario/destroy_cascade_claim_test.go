//go:build integration

// NIM-56 регресс на destroy_failed: claim снесённого СВОИМ destroy-каскадом
// хоста — benign-терминал no_match, а не failed. Discriminator — `souls.status`
// ('destroyed' ⇒ benign; любой другой выпад из roster-а ⇒ fail-closed отказ).
// Инфраструктура (TestMain/PG, seed*, newClaimRunner, insertPlannedFixture,
// applyOnlyDispatcher) общая с integration_test.go / cutover_test.go.

package scenario

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestIntegration_Claim_DestroyCascade_BenignNoMatch — cloud-destroy каскад
// (core.cloud.destroyed) метит `souls→destroyed` ДО host-fan-out; Acolyte затем
// клеймит planned-строку снесённого хоста. RenderForHost не находит его в
// connected-roster-е → без фикса это dispatch_failed → терминал destroy_failed.
// Фикс: хост со `status=='destroyed'` (единственный писатель — CascadeDestroy) —
// benign no_match. Реальный disconnect остаётся failed (fail-closed).
func TestIntegration_Claim_DestroyCascade_BenignNoMatch(t *testing.T) {
	ctx := context.Background()

	// Позитив: хост снят СВОИМ destroy-каскадом → no_match, ApplyRequest не уходит.
	t.Run("destroyed_benign_no_match", func(t *testing.T) {
		resetAll(t)
		seedOperator(t, "archon-alice")
		seedIncarnation(t, "noop-prod")
		seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
		gitURL := noopServiceRepo(t)

		applyID := audit.NewULID()
		insertPlannedFixture(t, applyID, "host-a.example.com", gitURL)
		// Эмуляция CascadeDestroy: хост снят своим destroy-каскадом прогона.
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

	// Негатив (fail-closed): реальный disconnect (не destroyed) → остаётся failed.
	t.Run("disconnected_fail_closed", func(t *testing.T) {
		resetAll(t)
		seedOperator(t, "archon-alice")
		seedIncarnation(t, "noop-prod")
		seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
		gitURL := noopServiceRepo(t)

		applyID := audit.NewULID()
		insertPlannedFixture(t, applyID, "host-a.example.com", gitURL)
		// disconnected между dispatch и claim — НЕ destroy-каскад: отказ сохраняется.
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

// mustSetSoulStatus переводит souls.status напрямую (эмуляция каскадного writer-а
// без прогона core.cloud.*). Ровно то состояние, что CascadeDestroy оставляет в
// реестре перед host-fan-out-ом (destroyed) либо reaper (disconnected).
func mustSetSoulStatus(t *testing.T, sid, status string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE souls SET status = $2 WHERE sid = $1`, sid, status); err != nil {
		t.Fatalf("mustSetSoulStatus(%s=%s): %v", sid, status, err)
	}
}
