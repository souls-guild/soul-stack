//go:build e2e

// L3a-MK: standalone-orphan applying-reconcile on a REAL keeper crash.
//
// Proves END-TO-END closure of finding #1 (reconcile_orphan_applying, ADR-027
// amend (m)) on a real SIGKILL of the keeper-owner process for a DIRECT
// (standalone, NOT through Voyage) incarnation.run -- not a unit-mocked
// reconciler:
//
//  1. Cluster of 2 keepers over SHARED PG/Redis/Vault, acolytes>0 (apply is
//     written via the Acolyte path claimed->dispatched->SendApply),
//     reconcile_orphan_applying with a SHORT stale_after (3s by default in
//     the harness, NOT prod-90s).
//  2. soul-stub in hold-apply mode connected to primary keeper-A (= owner of
//     the standalone run: HTTP incarnation.run arrives at it, lockRun writes
//     applying_by_kid=A; SendApply is routed to its EventStream).
//  3. incarnation.run(create) -> wait for apply_runs.status='dispatched' for
//     SID (stub holds ApplyRequest, does not send RunResult). incarnation
//     hangs `applying` with an epoch (applying_by_kid=A, applying_apply_id=run,
//     applying_since).
//  4. star SIGKILL keeper-A (owner of the applying-lock + run-barrier). The
//     barrier dies, the standalone-run has NO reclaim (reclaim_voyages is
//     Voyage-only) -- without finding #1 the incarnation would stay `applying`
//     FOREVER.
//  5. Wait for expiry of Conclave-presence A (~30s DefaultConclaveTTL) --
//     then InstanceAlive(applying_by_kid=A)=false.
//  6. star ASSERT: reconcile_orphan_applying on live keeper-B releases the
//     orphaned applying-lock (applying->ready), epoch columns cleared to
//     NULL, audit-event reaper.reconcile_orphan_applying.executed recorded
//     {incarnation, prev_kid=A, apply_id}. incarnation stops being "forever
//     applying".
//
// FENCING-1 (presence-dead owner, but live-rival apply_run with a different
// apply_id -> rule does NOT release) is NOT duplicated here: it is already
// proven by the integration test orphan_applying_reconcile_integration_test.go
// (ReleaseApplyingOrphan FENCING-1 inside). A live-rival on the crash stand
// would require a second concurrent run of the same incarnation --
// disproportionately complex for the marginal value over the existing
// integration coverage. See slice S3 report.
package e2e_test

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2E_MultiKeeper_StandaloneOrphanReconcileAfterCrash(t *testing.T) {
	const (
		keepers     = 2
		serviceName = "service-noop"
		examplePath = "examples/service/noop"
		incarnation = "so-orphan-inc"
		scenario    = "create"
		// staleAfter is SHORT -- the rule should fire soon after the killed
		// owner's presence expires, not wait for the by-design 90s prod default.
		staleAfter = 3 * time.Second
	)

	stack := harness.NewMultiKeeperStack(t, harness.MultiKeeperConfig{
		Keepers:                   keepers,
		Souls:                     1,
		VoyageLeaseTTL:            4 * time.Second,
		ReconcileOrphanStaleAfter: staleAfter,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, serviceName, examplePath)

	// Stub in hold-apply mode (does not send RunResult -> the row hangs
	// dispatched, incarnation stays applying). reconnect is NOT needed here:
	// the goal is reconciliation of the applying-lock by the Reaper, not stub
	// stream reconnect. The stub is connected to primary (keeper-A) -- it is
	// the owner of the standalone run.
	soulStub := stack.ConnectSoulStub(t, 0)
	soulStub.SetHoldApply(true)
	sid := stack.SoulSID(0)
	ownerKID := stack.StreamHolderKID(t)
	t.Logf("SO-orphan: soul-stub %s holds stream to %s (= owner of the standalone run)", sid, ownerKID)

	// Ready incarnation with a single connected host in its coven.
	stack.SeedIncarnationReady(t, incarnation, serviceName, "main", map[string]any{})
	stack.AddMember(t, 0, incarnation)

	// star DIRECT incarnation.run(create) -- standalone path (NOT Voyage; no
	// voyage_targets back-link). lockRun on keeper-A writes applying + epoch
	// (applying_by_kid=A). Acolyte claims planned->dispatched->SendApply into
	// A's stream. Stub holds ApplyRequest -> the row hangs dispatched,
	// incarnation is applying.
	applyID := stack.RunScenario(t, incarnation, scenario, nil)
	t.Logf("SO-orphan: standalone incarnation.run(%s) apply_id=%s", scenario, applyID)

	// (1) Wait for dispatched -- the job is physically handed to the stub,
	// RunResult withheld.
	got := stack.WaitApplyRunStatusForSID(t, applyID, sid, []string{"dispatched"}, 30*time.Second)
	t.Logf("SO-orphan: apply_runs(%s,%s).status=%q -- job dispatched, RunResult withheld", applyID, sid, got)

	// (2) incarnation is applying with owner A's epoch filled in -- the point
	// from which the owner's crash will leave an orphaned lock. Check the
	// epoch BEFORE the crash: the rule detects the orphan exactly via
	// applying_by_kid (presence witness of death).
	incStatus, _ := stack.IncarnationStatusDetails(t, incarnation)
	if incStatus != "applying" {
		t.Fatalf("SO-orphan: incarnation %s status=%q before crash, expected applying (lockRun did not take the lock?)", incarnation, incStatus)
	}
	epoch := stack.IncarnationApplyingEpochSnapshot(t, incarnation)
	if epoch.ByKID == nil || *epoch.ByKID != ownerKID {
		gotKID := "<nil>"
		if epoch.ByKID != nil {
			gotKID = *epoch.ByKID
		}
		t.Fatalf("SO-orphan: applying_by_kid=%q before crash, expected %q (epoch not set by lockRun)", gotKID, ownerKID)
	}
	if epoch.ApplyID == nil || *epoch.ApplyID == "" || !epoch.SinceSet {
		t.Fatalf("SO-orphan: incomplete epoch before crash (apply_id=%v since_set=%v) -- reconcile predicate won't fire",
			epoch.ApplyID, epoch.SinceSet)
	}
	t.Logf("SO-orphan: incarnation %s applying with epoch{by_kid=%s, apply_id=%s, since_set=%v} -- owner-lock armed",
		incarnation, *epoch.ByKID, *epoch.ApplyID, epoch.SinceSet)

	// (3) star REAL SIGKILL of the standalone run's owner. The run-goroutine
	// barrier on A dies; the applying-lock stays in PG with
	// applying_by_kid=A. reclaim_voyages does NOT touch it (no
	// voyage_targets back-link -- this is standalone). Without finding #1 the
	// incarnation would hang applying forever.
	stack.KillKeeperByKID(t, ownerKID)
	t.Logf("SO-orphan: SIGKILL %s (owner of the applying-lock) sent", ownerKID)

	live := stack.LiveKeeperKIDs()
	if len(live) == 0 {
		t.Fatalf("SO-orphan: no live keepers left after kill (need >=1 for the reconcile Reaper)")
	}
	t.Logf("SO-orphan: live keepers (running reconcile_orphan_applying): %v", live)

	// (4) star ASSERT END-TO-END: after the killed A's Conclave-presence
	// expires (~30s DefaultConclaveTTL -- by-design, not configurable),
	// reconcile_orphan_applying on live keeper-B detects the presence-dead
	// owner and releases the orphaned lock applying->ready. Window:
	// presence-TTL (~30s) + stale_after (3s) + reaper-interval (500ms) +
	// margin. Wait up to 70s -- presence-TTL is mandatory, stale_after is
	// SHORT (the rule fires almost immediately after presence expiry).
	finalStatus := stack.WaitIncarnationStatus(t, incarnation, []string{"ready"}, 70*time.Second)
	t.Logf("SO-orphan: star incarnation %s.status=%q -- orphaned applying-lock released by reconcile_orphan_applying (END-TO-END)", incarnation, finalStatus)

	// (5) epoch columns cleared to NULL -- ReleaseApplyingOrphan clears them
	// in the same tx as status->ready (otherwise a stale applying_by_kid
	// would trigger a repeat orphan detection on the next tick).
	epochAfter := stack.IncarnationApplyingEpochSnapshot(t, incarnation)
	if !epochAfter.EpochCleared() {
		t.Fatalf("SO-orphan: epoch NOT cleared after orphan-lock release: by_kid=%v apply_id=%v attempt=%v since_set=%v",
			epochAfter.ByKID, epochAfter.ApplyID, epochAfter.Attempt, epochAfter.SinceSet)
	}
	t.Logf("SO-orphan: epoch columns zeroed to NULL -- lock released cleanly")

	// (6) audit-event reaper.reconcile_orphan_applying.executed recorded with
	// the correct payload {incarnation, prev_kid=killed owner}. Proves the
	// release happened via the reconcile rule (not a side path), and carries
	// a security trail.
	if !stack.WaitAuditEventByPayload(t, "reaper.reconcile_orphan_applying.executed",
		"incarnation", incarnation, 10*time.Second) {
		t.Fatalf("SO-orphan: audit reaper.reconcile_orphan_applying.executed for %s NOT recorded", incarnation)
	}
	prevKIDinAudit := stack.AuditPayloadField(t, "reaper.reconcile_orphan_applying.executed", "incarnation", incarnation, "prev_kid")
	if prevKIDinAudit != ownerKID {
		t.Fatalf("SO-orphan: audit prev_kid=%q, expected killed owner %q (reconcile attributed to the wrong KID)", prevKIDinAudit, ownerKID)
	}
	t.Logf("SO-orphan: star audit reaper.reconcile_orphan_applying.executed{incarnation=%s, prev_kid=%s} recorded -- finding #1 PROVEN live",
		incarnation, prevKIDinAudit)

	// (7) Durability: incarnation does NOT roll back into applying (single-
	// winner CAS applying->ready). Wait with a pause to catch a hypothetical
	// repeat orphan detection (would be a bug: epoch is already NULL -> won't
	// become a candidate).
	time.Sleep(2 * time.Second)
	if st, _ := stack.IncarnationStatusDetails(t, incarnation); st != "ready" {
		t.Fatalf("SO-orphan: incarnation rolled back to %q after ready -- orphan-lock release durability violated", st)
	}
}
