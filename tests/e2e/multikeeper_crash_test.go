//go:build e2e

// L3a-MK: multi-keeper live-crash recovery (GA proof of reclaim_voyages).
//
// The scenario proves END-TO-END recovery on a REAL keeper process crash
// (SIGKILL, not the SQL emulation of TestIntegration_Runner_ReclaimApplyRuns_*):
//
//  1. Cluster of 3 keepers over SHARED PG/Redis/Vault:
//     - keeper-mk-00 -- soul-holder (voyage.workers=0, holds soul streams);
//     - keeper-mk-01/02 -- VoyageWorkers (voyage.workers=2), contend for the Voyage.
//  2. Scenario-Voyage over N ready incarnations with batch_size=1 (serial
//     waves -- the run is stretched out, giving a wide kill window).
//  3. Wait for voyages.status=running + claimed_by_kid=<owner> (always
//     mk-01/02, since the soul-holder does not contend).
//  4. SIGKILL EXACTLY the <owner> process (the live souls on the soul-holder
//     are unaffected).
//  5. ASSERT recovery:
//     (a) reclaim_voyages returned the Voyage to pending -> re-claimed by
//     another live KID (attempt increased, claimed_by_kid != killed);
//     (b) the Voyage reached the succeeded terminal on a live keeper;
//     (c) all voyage_targets succeeded -- the run REALLY completed for every
//     incarnation (not "formally succeeded on an empty scope");
//     (d) each incarnation's incarnation.state is consistent (status=ready,
//     not stuck in applying).
package e2e_test

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2E_MultiKeeper_VoyageReclaimAfterCrash(t *testing.T) {
	const (
		keepers     = 3
		incarnCount = 6
		serviceName = "service-noop"
		examplePath = "examples/service/noop"
	)

	stack := harness.NewMultiKeeperStack(t, harness.MultiKeeperConfig{
		Keepers:        keepers,
		Souls:          1,
		VoyageLeaseTTL: 4 * time.Second,
	})
	defer stack.Cleanup()

	// Service registry + one connected soul on the soul-holder (primary).
	stack.RegisterService(t, serviceName, examplePath)
	soulStub := stack.ConnectSoulStub(t, 0)
	// noop create carries the host task core.exec.run (echo hello) -- without
	// default-success mode, an unscripted task would give FAILED. We enable
	// "success on any ApplyRequest": we're testing the recovery seam
	// (orphan-lock + re-run), not per-task fixture scripting. The stub holds
	// the stream until Cleanup.
	soulStub.SetApplyDefaultSuccess(true)

	// Seed N ready incarnations; the single soul is in the coven of EVERY
	// incarnation (roster resolves via incarnation.name in souls.coven[],
	// ADR-008), so each per-incarnation scenario-run of the Voyage has a
	// connected host.
	incNames := make([]string, incarnCount)
	for i := 0; i < incarnCount; i++ {
		name := incName(i)
		incNames[i] = name
		stack.SeedIncarnationReady(t, name, serviceName, "main", map[string]any{})
		stack.AddSoulToCoven(t, 0, name)
	}

	// Scenario-Voyage over all incarnations, batch_size=1 -> serial waves.
	voyageID := stack.CreateScenarioVoyage(t, "create", incNames, 1)

	// Wait for the running owner. Owner in {keeper-mk-01, keeper-mk-02}
	// (soul-holder mk-00 does not run a voyage pool).
	owner := stack.WaitVoyageRunningOwner(t, voyageID, 20*time.Second)
	if owner == "keeper-mk-00" {
		t.Fatalf("unexpected Voyage owner %q -- the soul-holder must not contend for the Voyage", owner)
	}
	beforeKill := stack.VoyageState(t, voyageID)
	t.Logf("MK-crash: Voyage %s claimed by %s (attempt=%d, batch=%d/%d) -- killing the process",
		voyageID, owner, beforeKill.Attempt, beforeKill.BatchIndex, beforeKill.TotalBatch)

	// * REAL kill of the owner keeper process (not an SQL emulation).
	stack.KillKeeperByKID(t, owner)

	// (a) reclaim_voyages -> re-claimed by another live KID (attempt increased).
	reclaimedBy := stack.WaitVoyageReclaimed(t, voyageID, owner, beforeKill.Attempt, 30*time.Second)
	t.Logf("MK-crash: Voyage %s re-claimed by %s (was %s) -- reclaim worked", voyageID, reclaimedBy, owner)
	if reclaimedBy == owner {
		t.Fatalf("re-claim returned the same (killed) KID %q -- reclaim did not change the owner", owner)
	}

	// * SEAM DETECTION (closed by the ADR-027(k) fix). After reclaim, give
	// the live keeper time to finish the run. If the owner's crash left the
	// per-incarnation scenario-run orphaned in `applying`, the reclaimed
	// VoyageWorker, BEFORE re-spawning, detects ITS orphan applying-lock
	// (back-link apply_id from voyage_targets of THIS Voyage from the
	// previous attempt) and releases it FENCED (apply_id-match +
	// VerifyOwnership + single-winner CAS applying->ready,
	// voyageorch.reconcileOrphanLock -> incarnation.ReleaseApplyingOrphan).
	// After the lock is released, re-run proceeds and voyage_targets
	// completes.
	//
	// Before the fix, reclaim_voyages returned the Voyage to pending and
	// changed the owner, but did NOT reconcile the dangling `applying` ->
	// re-run was rejected ("incarnation already applying"), voyage_targets
	// stuck at batch 0 FOREVER. This ASSERT block proves that the
	// orphan-lock is now released (the incarnation does NOT stay in
	// applying), and the Voyage reaches succeeded.
	time.Sleep(12 * time.Second)
	stack.DumpRecoveryState(t, voyageID)

	// Variant A (regression guard): the incarnation must NOT remain orphaned
	// in `applying` -- the ADR-027(k) fix releases the lock before re-run. If
	// it stayed there, the recovery seam is broken again.
	if orphaned := stack.IncarnationsInStatus(t, incNames, "applying"); len(orphaned) > 0 {
		applyRuns := stack.CountApplyRunsForIncarnation(t, orphaned[0])
		t.Fatalf("REGRESSION of the ADR-027(k) recovery seam (applying-orphan): after the owner %s "+
			"crashed and reclaim_voyages succeeded (re-claimed by %s), incarnation(s) %v REMAINED in "+
			"`applying` (apply_runs for %s = %d). The reclaimed VoyageWorker should have released "+
			"the orphaned applying-lock (reconcileOrphanLock -> ReleaseApplyingOrphan) before "+
			"re-run, but did not -- orphan detection/fencing is broken.",
			owner, reclaimedBy, orphaned, orphaned[0], applyRuns)
	}

	// Variant B (re-run cleanliness guard): after the orphan-lock is
	// released, re-dispatch must complete into ready. error_locked here
	// means re-run failed for another reason (not the orphan seam) -- a
	// separate defect, not masked as succeeded.
	if locked := stack.IncarnationsInStatus(t, incNames, "error_locked"); len(locked) > 0 {
		_, details := stack.IncarnationStatusDetails(t, locked[0])
		t.Fatalf("recovery re-run fell into error_locked (variant B): after the owner %s crashed "+
			"and reclaim_voyages succeeded (re-claimed by %s, orphan-lock released, re-dispatch "+
			"happened), incarnation(s) %v ended up in `error_locked` (status_details %s = %s) "+
			"instead of succeeded -- the per-incarnation re-run did not complete cleanly "+
			"(defect outside the orphan seam).",
			owner, reclaimedBy, locked, locked[0], details)
	}

	// (b) The Voyage reached the succeeded terminal on a live keeper
	// (achievable ONLY if there is no seam defect -- e.g. the crash landed
	// BETWEEN per-incarnation runs, not inside one).
	final := stack.WaitVoyageSucceeded(t, voyageID, 60*time.Second)
	t.Logf("MK-crash: Voyage %s reached succeeded (attempt=%d, finished=%v)",
		voyageID, final.Attempt, final.Finished)

	// (c) All voyage_targets succeeded -- the run REALLY completed.
	got := stack.AssertVoyageTargetsTerminal(t, voyageID)
	if got != incarnCount {
		t.Fatalf("voyage_targets succeeded=%d, expected %d (the run did not complete for every incarnation)", got, incarnCount)
	}

	// (d) incarnation.state is consistent: every incarnation is ready (not
	// stuck in applying after the Voyage owner crashed).
	for _, name := range incNames {
		stack.WaitIncarnationReady(t, name, 30)
	}

	// Bonus proof: a per-row audit `voyage.reclaimed` was emitted on reclaim.
	if n := stack.CountAuditEvents(t, "voyage.reclaimed", voyageID); n < 1 {
		t.Errorf("audit `voyage.reclaimed` for %s = %d, expected >=1 (reclaim must emit a per-row event)", voyageID, n)
	}
}

// incName -- deterministic name of incarnation i.
func incName(i int) string {
	return "mk-inc-" + string(rune('a'+i))
}
