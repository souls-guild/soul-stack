//go:build e2e

// Per-section E2E: GET /v1/souls/{sid}/history aggregates a per-host
// timeline from two sources -- scenario runs (apply_runs) and ad-hoc exec
// (errands). The test runs, on ONE connected SID, first a scenario-apply
// (incarnation create), then a single Errand /exec (ADR-033), and then
// asserts that /history returns both records with correct discrimination
// (type=scenario carries incarnation/scenario, type=errand carries module),
// started_at DESC sorting, and a working ?type= query filter.
//
// Why it catches regressions:
//   - the merge query soul.SelectHistory is broken / does not merge sources
//     -> total<2 or one of the types is missing;
//   - the ?type= filter is ignored -> extra/missing records;
//   - started_at DESC sorting is lost -> order violated.
//
// Limitation (same as scenario_apply): soul-stub does not execute real
// modules (SetApplyDefaultSuccess + errand SUCCESS echo) -- we check the
// keeper-side history aggregation, not apply/exec execution realism (L3a contract).
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestSoulHistory_AggregatesScenarioAndErrand(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "noop", "examples/service/noop")

	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)
	sid := stack.SoulSID(0)

	stack.AddSoulToCoven(t, 0, "test-history")

	// Source #1 -- scenario: incarnation create auto-runs the scenario
	// `create` -> an apply_runs row under this SID.
	_, applyID := stack.CreateIncarnationWithApply(t, "test-history", "noop@main", nil)
	stack.WaitApplySuccess(t, applyID, 60)

	// Source #2 -- single Errand: an ad-hoc /exec on the same SID -> an errands row.
	res := stack.ExecErrand(t, sid, "core.cmd.shell", map[string]any{"cmd": "echo ok"})
	if res.Status != "success" {
		t.Fatalf("ExecErrand: status=%q, expected success", res.Status)
	}

	// /history without a filter -- both records.
	reply := stack.SoulHistory(t, sid, "")
	if reply.SID != sid {
		t.Fatalf("/history: sid echo=%q, expected %q", reply.SID, sid)
	}
	if reply.Total < 2 {
		t.Fatalf("/history: total=%d, expected >=2 (scenario+errand); items=%+v", reply.Total, reply.Items)
	}

	var scen, errItem *harness.SoulHistoryItem
	for i := range reply.Items {
		it := &reply.Items[i]
		switch it.Type {
		case "scenario":
			scen = it
		case "errand":
			errItem = it
		default:
			t.Fatalf("/history: unknown type=%q in item=%+v", it.Type, it)
		}
	}
	if scen == nil {
		t.Fatalf("/history: no type=scenario record; items=%+v", reply.Items)
	}
	if errItem == nil {
		t.Fatalf("/history: no type=errand record; items=%+v", reply.Items)
	}

	// type=scenario carries incarnation/scenario, does NOT carry module.
	if scen.Incarnation != "test-history" {
		t.Fatalf("scenario-item.incarnation=%q, expected test-history (%+v)", scen.Incarnation, scen)
	}
	if scen.Scenario == "" {
		t.Fatalf("scenario-item.scenario is empty (%+v)", scen)
	}
	if scen.Module != "" {
		t.Fatalf("scenario-item carries the errand field module=%q (%+v)", scen.Module, scen)
	}

	// type=errand carries module, does NOT carry incarnation.
	if errItem.Module != "core.cmd.shell" {
		t.Fatalf("errand-item.module=%q, expected core.cmd.shell (%+v)", errItem.Module, errItem)
	}
	if errItem.Incarnation != "" {
		t.Fatalf("errand-item carries the scenario field incarnation=%q (%+v)", errItem.Incarnation, errItem)
	}

	// started_at DESC sorting: the errand started AFTER the scenario -> it
	// must come first (or earlier) in items. We compare RFC3339 strings
	// (lexicographic order = chronological order for UTC RFC3339).
	for i := 1; i < len(reply.Items); i++ {
		if reply.Items[i-1].StartedAt < reply.Items[i].StartedAt {
			t.Fatalf("/history: sorting is not DESC by started_at: items[%d]=%q < items[%d]=%q",
				i-1, reply.Items[i-1].StartedAt, i, reply.Items[i].StartedAt)
		}
	}

	// Filter ?type=errand -- errand only.
	onlyErrand := stack.SoulHistory(t, sid, "errand")
	if len(onlyErrand.Items) == 0 {
		t.Fatalf("/history?type=errand: empty, expected >=1 errand record")
	}
	for _, it := range onlyErrand.Items {
		if it.Type != "errand" {
			t.Fatalf("/history?type=errand returned type=%q (%+v)", it.Type, it)
		}
	}

	// Filter ?type=scenario -- scenario only.
	onlyScenario := stack.SoulHistory(t, sid, "scenario")
	if len(onlyScenario.Items) == 0 {
		t.Fatalf("/history?type=scenario: empty, expected >=1 scenario record")
	}
	for _, it := range onlyScenario.Items {
		if it.Type != "scenario" {
			t.Fatalf("/history?type=scenario returned type=%q (%+v)", it.Type, it)
		}
	}
}
