package scenario

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// TestClaim_HostTaskFilter tests filtering a run's tasks by the claimed
// host's SID (claim.execute reuses groupByHost). An on:/where:-filtered host
// (empty TargetSIDs) → no tasks → no-op no_match (FINDING-01 variant (b));
// otherwise only its own tasks.
func TestClaim_HostTaskFilter(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "t0", Module: "core.exec.run"},
		{Index: 1, Name: "t1", Module: "core.file.present"},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"host-a", "host-b"}},
		{TaskIndex: 1, TargetSIDs: []string{"host-b"}},
	}
	perHost := groupByHost(tasks, plans)

	if got := perHost["host-a"]; len(got) != 1 || got[0].Name != "t0" {
		t.Errorf("host-a tasks = %+v, want [t0]", got)
	}
	if got := perHost["host-b"]; len(got) != 2 {
		t.Errorf("host-b tasks = %d, want 2", len(got))
	}
	// on:/where: filtered out everything on host-c → no tasks → claim closes
	// it as a no-op with no_match terminal (FINDING-01 variant (b)), not success.
	if got := perHost["host-c"]; len(got) != 0 {
		t.Errorf("host-c tasks = %d, want 0 (no-op no_match)", len(got))
	}
}

// TestClaim_AbortedGuard tests the drain guard (graceful drain of the
// Acolyte pool, ADR-027 Phase 2): execute considers an assignment
// drain-aborted exactly when the claim ctx is cancelled. On a cancelled ctx,
// a render/SendApply error does NOT lead to markFailed — the Ward stays in
// the DB (claimed) for recovery; on a live ctx, a domain error normally
// leads to failed.
func TestClaim_AbortedGuard(t *testing.T) {
	c := &ClaimRunner{}

	live := context.Background()
	if c.aborted(live) {
		t.Error("a live ctx should not be considered drain-interrupted")
	}

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !c.aborted(cctx) {
		t.Error("a cancelled claim-ctx should be considered drain-interrupted")
	}
}

// TestClaim_FailedSummaryMasksSecret tests invariant A: the failed summary
// built from a render error (maskErrText) carries neither a revealed secret
// nor a bare vault-ref. This ensures a failed claim assignment doesn't leak
// a secret into operator-facing status_details / error_summary.
func TestClaim_FailedSummaryMasksSecret(t *testing.T) {
	// A render error whose text carries a vault-ref in transit.
	err := errors.New("scenario: RenderForHost: render redis-prod/create: vault:secret/db-creds#password missing")
	summary := maskErrText(err, nil) // Acolyte path without a seal set → vault+regex layers

	if strings.Contains(summary, "vault:secret/db-creds") {
		t.Errorf("summary carries a bare vault-ref: %q", summary)
	}
	if strings.Contains(summary, "***MASKED***") == false {
		t.Errorf("summary is not masked: %q", summary)
	}
}

// TestClaim_RecipeCarriesVaultRefAsIs tests invariant A at the recipe level:
// the recipe's Input carries the vault-ref AS A STRING (as-is), the secret
// is NOT revealed. This is what dispatchPlanned puts into the planned row
// and what the Acolyte receives at claim time, before
// ResolveInputValuesVault.
func TestClaim_RecipeCarriesVaultRefAsIs(t *testing.T) {
	recipe := &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "redis", Git: "https://example.test/redis.git", Ref: "main"},
		ScenarioName: "create",
		Input:        map[string]any{"db_password": "vault:secret/db-creds#password"},
	}
	b, err := applyrun.MarshalRecipe(recipe)
	if err != nil {
		t.Fatalf("MarshalRecipe: %v", err)
	}
	// The persisted recipe carries exactly the vault-ref string, not a revealed value.
	if !strings.Contains(string(b), "vault:secret/db-creds#password") {
		t.Errorf("recipe does not carry the vault-ref as-is: %s", b)
	}

	back, err := applyrun.UnmarshalRecipe(b)
	if err != nil {
		t.Fatalf("UnmarshalRecipe: %v", err)
	}
	if back.Input["db_password"] != "vault:secret/db-creds#password" {
		t.Errorf("round-trip lost the vault-ref: %v", back.Input["db_password"])
	}
}
