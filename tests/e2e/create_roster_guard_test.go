//go:build e2e

// L3a contract-e2e: a create scenario carrying a topology `assert:` is
// reachable through POST /v1/incarnations (NIM-235).
//
// THE BUG THIS PINS. The pre-flight assert gate (ADR-009 amendment 2026-06-23
// form A / ADR-027 amend (o)) runs on the request path, between ValidateInput
// and incarnation.Create, and evaluates the scenario's assert predicates. Its
// original contract read the run roster by the root Coven label, which souls
// carried in `souls.coven[]` — so a roster could pre-date its incarnation.
// NIM-124 (ADR-008 amendment 2026-07-17, migration 099) moved membership onto
// `incarnation_membership`, whose FK requires the incarnation row. From then on
// the roster at pre-flight was not empty by circumstance but IMPOSSIBLE, and
// `size(soulprint.hosts) == N` was false for every create: any service whose
// create scenario carried a topology guard answered 422 assert_failed to every
// request, with no input the operator could send to get past it.
//
// WHY THIS TIER. The gap is on the product path and nowhere else: the run-path
// helpers (Stack.CreateIncarnationOnRoster, NIM-192/NIM-210) seed the row with
// direct SQL and start `create` as an ordinary explicit run, which never
// touches the pre-flight gate. Only POST /v1/incarnations with an auto-started
// create run reaches it, so only that call can catch a regression here.
//
// Flow (examples/service/create-roster-guard):
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper + 3 soul-stubs.
//  2. Connect the stubs and seed their soulprint (the refresh_soulprint barrier
//     waits for presence AND first typed facts).
//  3. POST /v1/incarnations with create_scenario=create — the row is inserted
//     and the bootstrap run starts in the same call.
//  4. The run binds the three SIDs (keeper-side core.soul.registered,
//     refresh_soulprint) -> passage boundary -> roster re-resolves to 3 -> the
//     ungated size-guard passes -> the Soul-side echo lands on all three.
//  5. Asserts: apply_runs success, incarnation ready, membership = 3.
package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2ECreateRosterGuard_TopologyAssertOnCreatePath(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/create-roster-guard",
		Souls:       3,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "create-roster-guard", "examples/service/create-roster-guard")

	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	sids := make([]string, 0, len(ips))
	for i := range ips {
		stub := stack.ConnectSoulStub(t, i)
		stub.SetApplyDefaultSuccess(true)
		// The barrier is `refresh_soulprint: true`, so a SID counts as ready only
		// once typed facts are in PG — presence alone is not enough (ADR-0061
		// amendment). Seeding makes that deterministic instead of racing the
		// stub's first report.
		stack.SeedSoulprint(t, i, map[string]any{
			"os": map[string]any{
				"family":      "debian",
				"distro":      "debian",
				"version":     "12",
				"arch":        "amd64",
				"pkg_mgr":     "apt",
				"init_system": "systemd",
			},
			"network": map[string]any{"primary_ip": ips[i]},
		})
		sids = append(sids, stack.SoulSID(i))
	}

	const incName = "roster-guard"

	// THE ASSERTION UNDER TEST is this call returning 202 at all. Before the fix
	// it answered 422 assert_failed: the ungated size-guard was evaluated against
	// a roster that could not exist yet, so the run below never started and no
	// incarnation row was ever written.
	_, applyID := stack.CreateIncarnationWithApply(t, incName, "create-roster-guard@main", map[string]any{
		"roster_sids":  sids,
		"expect_hosts": 3,
	})

	// The guard still guards: it is evaluated at render, after the bind's refresh
	// boundary, against the roster the run itself produced. A success here means
	// it saw 3 hosts and agreed with expect_hosts — not that it was skipped.
	stack.WaitApplySuccess(t, applyID, 120)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 60)

	assertMemberCount(t, stack, incName, 3)
}

// assertMemberCount checks how many hosts the run bound to the incarnation —
// the roster the topology guard was measured against. Reading it from
// `incarnation_membership` (not from souls.coven) is the point: that relation is
// the one NIM-124 introduced and the one the guard could not see at pre-flight.
func assertMemberCount(t *testing.T, stack *harness.Stack, incName string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got int
	if err := stack.DB().QueryRow(ctx,
		`SELECT COUNT(*) FROM incarnation_membership WHERE incarnation_name = $1`, incName).Scan(&got); err != nil {
		t.Fatalf("count membership(%s): %v", incName, err)
	}
	if got != want {
		t.Errorf("incarnation_membership rows = %d, want %d — the create run did not bind the declared roster", got, want)
	}
}
