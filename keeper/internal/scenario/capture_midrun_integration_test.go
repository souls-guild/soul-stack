//go:build integration

// Guards for the run↔capture interop ([ADR-0084]). A `core.state.*` step commits
// its field DURING the run; the terminals at the end of the run must not undo it.
// Both terminals used to write back the state snapshot taken when the run locked
// the incarnation, which silently reverted every capture the run had made — and
// silently is the whole problem: the step reported success, the run reported
// success, and the field was gone.
//
// The capture here goes through the real [incarnation.CaptureState] on the live
// pool, from inside a keeper-side module, exactly as `core.state.set` does.

package scenario

import (
	"context"
	"fmt"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// capturingStateModule writes a state field mid-run through the production
// capture path, then optionally fails. It reads the run scope out of the stream
// context, so it also guards that the dispatcher puts it there — a capture that
// cannot name its run is refused by the real module.
type capturingStateModule struct {
	module.BaseModule
	field    string
	value    any
	failWith string // non-empty: fail the step AFTER the capture commits
	gotScope util.RunScope
}

func (m *capturingStateModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	scope, ok := util.RunScopeFrom(ctx)
	if !ok {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "no run scope in the module context"})
	}
	m.gotScope = scope

	name := util.IncarnationFrom(ctx)
	_, err := incarnation.CaptureState(ctx, integrationPool, incarnation.CaptureSpec{
		ID:        name,
		Scenario:  scope.Scenario,
		ApplyID:   scope.ApplyID,
		HistoryID: audit.NewULID(),
	}, func(before map[string]any) (map[string]any, error) {
		after := map[string]any{}
		for k, v := range before {
			after[k] = v
		}
		after[m.field] = m.value
		return after, nil
	})
	if err != nil {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: fmt.Sprintf("capture: %v", err)})
	}
	if m.failWith != "" {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: m.failWith})
	}
	return stream.Send(&pluginv1.ApplyEvent{Changed: true})
}

// captureThenSetRepo — a keeper task that captures `captured` mid-run, and a
// `core.state.set` step that records an unrelated field after the host work.
// The two writers must compose: the later capture writes onto the row AS IT
// STANDS, not onto the pre-run snapshot, and the terminal keeps both.
func captureThenSetRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: a mid-run capture plus a later core.state.set
tasks:
  - name: capture a field mid-run
    module: core.probe.captured
    on: keeper
    params: {}
  - name: work on the host
    module: core.exec.run
    changed_when: "false"
    params:
      cmd: echo
      args: ["ok"]
  - name: record the outcome
    module: core.state.set
    params:
      field: recorded
      value: "at-the-end"
`)
}

// TestIntegration_Capture_SurvivesTheSuccessTerminal: a captured field is still
// there after the run commits, alongside what the later capture recorded. Before
// the fix the success terminal wrote back the pre-run snapshot, so `captured`
// vanished at the moment the run reported Ready.
func TestIntegration_Capture_SurvivesTheSuccessTerminal(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := captureThenSetRepo(t)

	probe := &capturingStateModule{field: "captured", value: "mid-run"}
	keepers := keeperRegistryWith(map[string]module.SoulModule{"core.probe": probe})

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if got := inc.State["captured"]; got != "mid-run" {
		t.Errorf("state.captured = %v, want mid-run: the success terminal reverted the capture", got)
	}
	if got := inc.State["recorded"]; got != "at-the-end" {
		t.Errorf("state.recorded = %v, want at-the-end: the later capture was lost", got)
	}

	// The run scope reached the module intact — a history row that cannot name
	// its run is an audit hole, not a cosmetic one.
	if probe.gotScope.ApplyID != applyID || probe.gotScope.Scenario != "create" {
		t.Errorf("run scope in the module = %+v, want apply %s / scenario create", probe.gotScope, applyID)
	}
	if probe.gotScope.StartedByAID != "archon-alice" {
		t.Errorf("run scope StartedByAID = %q, want archon-alice", probe.gotScope.StartedByAID)
	}
}

// captureThenFailRepo — the capture lands, a later host task fails the run, and
// a capture scheduled after that task never gets to run.
func captureThenFailRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: a mid-run capture followed by a failing task
tasks:
  - name: capture a field mid-run
    module: core.probe.captured
    on: keeper
    params: {}
  - name: work on the host
    module: core.exec.run
    changed_when: "false"
    params:
      cmd: echo
      args: ["ok"]
  - name: record the outcome
    module: core.state.set
    params:
      field: after_the_failure
      value: "never-reached"
`)
}

// TestIntegration_Capture_SurvivesTheFailureTerminal is the [ADR-0084] sentence
// that makes capture worth having: "a failed run leaves state describing exactly
// what was actually captured before it died". Before the fix the failure terminal
// wrote back the pre-run snapshot — a host provisioned and recorded mid-run
// disappeared from state the moment a later task failed, and the operator had no
// record that it exists.
func TestIntegration_Capture_SurvivesTheFailureTerminal(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationWithState(t, "noop-prod", map[string]any{"pre": "existing"})
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := captureThenFailRepo(t)

	probe := &capturingStateModule{field: "captured", value: "mid-run"}
	keepers := keeperRegistryWith(map[string]module.SoulModule{"core.probe": probe})

	disp := &mockDispatcher{t: t, result: applyrun.StatusFailed}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if got := inc.State["captured"]; got != "mid-run" {
		t.Errorf("state.captured = %v, want mid-run: the failure terminal reverted the capture", got)
	}
	if got := inc.State["pre"]; got != "existing" {
		t.Errorf("state.pre = %v, want existing: the failure terminal dropped untouched state", got)
	}
	// A capture standing after the failure point never runs, so its field is
	// absent — state describes what was actually reached, not what was planned.
	if _, present := inc.State["after_the_failure"]; present {
		t.Errorf("state.after_the_failure is set on a run that died before it: %v", inc.State["after_the_failure"])
	}
}

// TestIntegration_Capture_HistoryKeepsTheTerminalLast: a capture writes its own
// `state_history` row mid-run. Rerun-last picks the newest row by history_id, so
// a capture row must never outrank the run's terminal row — otherwise a rerun
// would replay a half-finished snapshot as if it were the last attempt.
func TestIntegration_Capture_HistoryKeepsTheTerminalLast(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := captureThenSetRepo(t)

	keepers := keeperRegistryWith(map[string]module.SoulModule{"core.probe": &capturingStateModule{field: "captured", value: "mid-run"}})
	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// `run` / `run_status` are terminal-only columns and are not exposed on
	// HistoryEntry, so the guard reads them where rerun-last reads them.
	const rowsSQL = `
SELECT history_id, run_status IS NOT NULL
FROM state_history
WHERE incarnation_name = $1 AND apply_id = $2
ORDER BY history_id DESC
`
	rows, err := integrationPool.Query(context.Background(), rowsSQL, "noop-prod", applyID)
	if err != nil {
		t.Fatalf("history probe: %v", err)
	}
	defer rows.Close()

	var (
		ids       []string
		terminals int
		newestIsT bool
		first     = true
	)
	for rows.Next() {
		var id string
		var terminal bool
		if err := rows.Scan(&id, &terminal); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
		if terminal {
			terminals++
		}
		if first {
			newestIsT = terminal
			first = false
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(ids) < 2 {
		t.Fatalf("history rows = %d, want the capture row and the terminal row", len(ids))
	}
	// A capture row carries no run outcome -- it is a snapshot of one capture,
	// not of an attempt. Exactly one row in the run closes it.
	if terminals != 1 {
		t.Errorf("rows carrying a run outcome = %d, want exactly 1 (the terminal)", terminals)
	}
	if !newestIsT {
		t.Error("the newest history row is a capture, not the terminal -- rerun-last would replay a half-finished snapshot")
	}
}
