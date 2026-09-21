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

	"fmt"
	"github.com/souls-guild/soul-stack/tests/e2e/harness"
	"strings"
	"time"
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

	// Source #1 -- scenario: the bootstrap `create` run -> an apply_runs row
	// under this SID. Seed row -> bind roster -> run create, the order owned by
	// CreateIncarnationOnRoster (NIM-210): membership carries an FK on the
	// incarnation row, so the host cannot be bound first.
	_, applyID := stack.CreateIncarnationOnRoster(t, "test-history", "noop@main", "create", []int{0}, nil)
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
		t.Fatalf("/history: total=%d, expected >=2 (scenario+errand); items=%s", reply.Total, showItems(reply.Items))
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
			t.Fatalf("/history: unknown type=%q in item=%s", it.Type, showItem(it))
		}
	}
	if scen == nil {
		t.Fatalf("/history: no type=scenario record; items=%s", showItems(reply.Items))
	}
	if errItem == nil {
		t.Fatalf("/history: no type=errand record; items=%s", showItems(reply.Items))
	}

	// type=scenario carries incarnation/scenario, does NOT carry module.
	// The mutually-exclusive fields are optional on the wire, so "does not
	// carry" is a nil pointer — an absent key. The harness used to type them
	// as plain strings, which collapses an omitted key and an explicit ""
	// into the same value and makes the two negative checks below unable to
	// tell a cross-contaminated record from a correct one (NIM-776).
	if scen.Incarnation == nil || *scen.Incarnation != "test-history" {
		t.Fatalf("scenario-item.incarnation=%s, expected test-history (%s)", showPtr(scen.Incarnation), showItem(scen))
	}
	if scen.Scenario == nil || *scen.Scenario == "" {
		t.Fatalf("scenario-item.scenario is empty (%s)", showItem(scen))
	}
	if scen.Module != nil {
		t.Fatalf("scenario-item carries the errand field module=%q (%s)", *scen.Module, showItem(scen))
	}

	// type=errand carries module, does NOT carry incarnation.
	if errItem.Module == nil || *errItem.Module != "core.cmd.shell" {
		t.Fatalf("errand-item.module=%s, expected core.cmd.shell (%s)", showPtr(errItem.Module), showItem(errItem))
	}
	if errItem.Incarnation != nil {
		t.Fatalf("errand-item carries the scenario field incarnation=%q (%s)", *errItem.Incarnation, showItem(errItem))
	}

	// started_at DESC sorting: the errand started AFTER the scenario -> it
	// must come first (or earlier) in items. started_at is a timestamp on the
	// wire, so the comparison is one - the string form this used to compare
	// held only for UTC RFC3339 and would have mis-sorted an offset-bearing
	// one silently.
	for i := 1; i < len(reply.Items); i++ {
		if reply.Items[i-1].StartedAt.Before(reply.Items[i].StartedAt) {
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
			t.Fatalf("/history?type=errand returned type=%q (%s)", it.Type, showItem(&it))
		}
	}

	// Filter ?type=scenario -- scenario only.
	onlyScenario := stack.SoulHistory(t, sid, "scenario")
	if len(onlyScenario.Items) == 0 {
		t.Fatalf("/history?type=scenario: empty, expected >=1 scenario record")
	}
	for _, it := range onlyScenario.Items {
		if it.Type != "scenario" {
			t.Fatalf("/history?type=scenario returned type=%q (%s)", it.Type, showItem(&it))
		}
	}
}

// showItem renders one history item field by field. wire.SoulHistoryItem carries
// four *string and a *time.Time, and fmt does not dereference a pointer below
// the top level - so %+v on it prints hex addresses exactly where a failure
// message needs the values. The harness copy this replaced used plain strings
// and printed readably by accident (NIM-776).
func showItem(it *harness.SoulHistoryItem) string {
	if it == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{type:%s id:%s status:%s incarnation:%s scenario:%s module:%s started_at:%s finished_at:%s}",
		it.Type, it.ID, it.Status,
		showPtr(it.Incarnation), showPtr(it.Scenario), showPtr(it.Module),
		it.StartedAt.Format(time.RFC3339Nano), showTimePtr(it.FinishedAt))
}

// showItems renders a page of history items, one per line.
func showItems(items []harness.SoulHistoryItem) string {
	var b strings.Builder
	for i := range items {
		b.WriteString("\n  ")
		b.WriteString(showItem(&items[i]))
	}
	return b.String()
}

// showPtr renders an optional wire string: an absent key is <nil>, and an
// explicit empty string is "" - the distinction the negative assertions above
// turn on.
func showPtr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *s)
}

func showTimePtr(t *time.Time) string {
	if t == nil {
		return "<nil>"
	}
	return t.Format(time.RFC3339Nano)
}
