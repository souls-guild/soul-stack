//go:build e2e

// L3a E2E: full Vigil/Oracle/Decree execution-loop (ADR-030, beacons reactor)
// on a real stack. Turns the skeleton TestE2EOracleTypedPortent_FileChangedFireFlow
// (t.Skip) into a working test: soul-stub.SendPortent (V5-1) already sends
// PortentEvent over the mTLS EventStream, so the real path is covered without
// the PG-stub bypass EmitPortent.
//
// Flow (real, no keeper-side mocks):
//  1. RegisterService + CreateIncarnationWithApply (incarnation must exist --
//     the Oracle enqueuer resolves ServiceRef from incarnation.service).
//  2. AddMember -- binds the host to the incarnation (incarnation_membership,
//     NIM-124) so the auto-create + reactor roster is non-empty.
//  3. CreateVigil (core.beacon.file_changed) + CreateDecree (typed-payload
//     where-CEL `event.file_changed.path.startsWith("/etc/")`, action_scenario
//     DIFFERENT from auto-create -- `converge`, so the reactor run is
//     distinguishable from the auto-create run by scenario+started_by_aid).
//  4. soul-stub.SendPortent(FileChangedPortent{path:/etc/...}) over the live
//     stream.
//  5. ASSERT via direct PG queries (real DB, real flows):
//     - WaitForOracleFires -- oracle_fires cooldown-state (decree, subject);
//     - audit_log `oracle.fired` (decree + scenario + sid);
//     - WaitForOracleReaction -- apply_runs(scenario=converge, started_by_aid=NULL)
//     enqueued by the reactor (EnqueueScenario -> InsertPlanned).
//
// Limitation (documented): soul-stub does not run a real beacon scheduler --
// the caller manually assembles PortentEvent (L3a contract: verifying the
// keeper-side reactor pipeline match/where/membership/cooldown/enqueue, not
// the realism of the Soul-side Check). Real inotify/scheduler is L3b territory.
package e2e_test

import (
	"context"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestOracle_FileChanged_FiresScenario(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const incName = "oracle-fire-target"

	stack.RegisterService(t, "noop", "examples/service/noop")

	// Live EventStream: Redis SID-lease -> dispatch is routed locally;
	// SetApplyDefaultSuccess -- SUCCESS for any task (what matters is the
	// apply_runs lifecycle, not per-task realism, see scenario_apply_test.go).
	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)
	sid := stack.SoulSID(0)

	// Membership binds the host to the incarnation (incarnation_membership,
	// NIM-124): non-empty roster for both the auto-create and the reactor run.
	stack.AddMember(t, 0, incName)

	// The incarnation must exist BEFORE the Portent: the enqueuer resolves
	// ServiceRef from incarnation.service (oracle_enqueuer.go). Auto-create
	// runs scenario `create` -- wait for success so the incarnation leaves
	// applying and doesn't conflict with the reactor run.
	_, createApplyID := stack.CreateIncarnationWithApply(t, incName, "noop@main", nil)
	stack.WaitApplySuccess(t, createApplyID, 60)

	vigilName := stack.CreateVigil(ctx, t, harness.CreateVigilOpts{
		Name:     "oracle-fire-vigil",
		Interval: "30s",
		Check:    "core.beacon.file_changed",
		Coven:    []string{incName},
		Params:   map[string]any{"path": "/etc/nginx.conf"},
	})

	// action_scenario = converge (exists in service-noop, different from
	// create): the reactor run is distinguishable from auto-create by
	// (scenario, started_by_aid IS NULL). where-CEL -- typed-field-access
	// V5-1 over FileChangedPortent.
	decreeName := stack.CreateDecree(ctx, t, harness.CreateDecreeOpts{
		Name:            "oracle-fire-decree",
		OnBeacon:        vigilName,
		WhereCEL:        `event.file_changed.path.startsWith("/etc/")`,
		Coven:           []string{incName},
		IncarnationName: incName,
		ActionScenario:  "converge",
		Cooldown:        "5m",
	})

	// Cutoff point for the auto-create run: the reactor run starts later.
	beforeFire := time.Now().UTC()

	// Real emit: soul-stub sends a typed FileChangedPortent over the mTLS
	// EventStream. path under /etc/ -> where-CEL true -> reactor fires.
	if err := stub.SendPortent(&keeperv1.PortentEvent{
		BeaconName: vigilName,
		Payload: &keeperv1.PortentEvent_FileChanged{
			FileChanged: &keeperv1.FileChangedPortent{
				Path:   "/etc/nginx.conf",
				Sha256: "deadbeef",
			},
		},
	}); err != nil {
		t.Fatalf("SendPortent: %v", err)
	}

	// 1. oracle_fires cooldown-state: one row (decree, subject=sid).
	fires := stack.WaitForOracleFires(ctx, t, decreeName, 1, 15*time.Second)
	if got := fires[0].Subject; got != sid {
		t.Fatalf("oracle_fires.subject = %q, expected authoritative SID %q (mTLS peer cert)", got, sid)
	}

	// 2. audit `oracle.fired` with decree/scenario/sid (payload subset).
	stack.AssertAuditEvent(t, "oracle.fired", map[string]any{
		"decree":   decreeName,
		"scenario": "converge",
		"sid":      sid,
	})

	// 3. The reactor enqueued the scenario into the work queue: planned
	// apply_run scenario=converge, started_by_aid IS NULL (Soul-initiated
	// reaction without an Archon identity).
	reactionApplyID := stack.WaitForOracleReaction(ctx, t, incName, "converge", beforeFire, 15*time.Second)
	if reactionApplyID == createApplyID {
		t.Fatalf("reactor run matched auto-create apply_id %q -- filter did not distinguish the runs", createApplyID)
	}
}
