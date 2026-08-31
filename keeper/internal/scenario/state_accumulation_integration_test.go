//go:build integration

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod"
	coremodstate "github.com/souls-guild/soul-stack/keeper/internal/coremod/state"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// State accumulation across Passages ([ADR-0084]).
//
// A `core.state.<verb>` step commits at the step, so `incarnation.state` read by a
// LATER Passage must be what that step wrote — not the pre-run snapshot renderIn was
// built with. The runner re-reads the row at the boundary
// (incarnation.SelectState, gated on config.HasStateCapture) and re-renders against it.
//
// PG-backed on purpose: the value has to travel through a real UPDATE + a real SELECT
// in two different transactions. A fake store would make both halves agree by
// construction and prove nothing about the commit point.

// stateCaptureScenario — Passage 0 captures `endpoint` on the keeper side, Passage 1
// reads it back on a host. The Passage split is the register edge (`register: cap`
// consumed by the host task, ADR-056 §b) — the DSL's own way of ordering a reader
// after a capture, and the form ADR-0084's ordering guard leaves legal.
const stateCaptureScenario = `
name: create
tasks:
  - name: Capture the endpoint
    module: core.state.set
    on: keeper
    register: cap
    params:
      field: endpoint
      value: "10.0.0.1"
  - name: Configure against the captured endpoint
    module: core.exec.run
    changed_when: "false"
    params:
      cmd: echo
      args: ["${ register.cap.field }", "${ incarnation.state.endpoint }"]
`

// stateServiceRepo is [writeServiceRepo] with a NON-empty state_schema: `core.state.*`
// checks the field against the schema at Apply, so a service declaring `properties: {}`
// cannot capture anything.
func stateServiceRepo(t *testing.T, scenarioMain string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `state_schema_version: 1
description: noop service with a capturable field
state_schema:
  type: object
  properties:
    endpoint:
      type: string
`)
	write("scenario/create/main.yml", scenarioMain)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// paramsDispatcher records the rendered params of every task it is handed, so a test
// can assert what a Passage actually resolved `${ … }` to.
type paramsDispatcher struct {
	t    *testing.T
	mu   sync.Mutex
	args [][]any
}

func (d *paramsDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	d.mu.Lock()
	for _, task := range req.GetTasks() {
		p := task.GetParams().AsMap()
		if a, ok := p["args"].([]any); ok {
			d.args = append(d.args, a)
		}
	}
	d.mu.Unlock()
	// Terminate the row of THIS Passage: a staged run writes one apply_runs row
	// per (sid, passage), so passage 0 would be "not found" for a Passage-1 apply.
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("paramsDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

// seedIncarnationStateRoster seeds an incarnation whose state is ALREADY populated,
// plus its roster. The pre-run value is what a run without the boundary re-read would
// render — it is the mutation detector, not decoration.
func seedIncarnationStateRoster(t *testing.T, name string, state map[string]any, sids ...string) {
	t.Helper()
	seedIncarnationRoster(t, name, sids...)
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE incarnation SET state = $2 WHERE name = $1`, name, state); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

// TestIntegration_StateAccumulatesAcrossPassages — ★ GUARD for the Passage-boundary
// state re-read ([ADR-0084] "Reading state while writing it").
//
// Passage 0 runs `core.state.set field: endpoint` on the keeper side and COMMITS it
// mid-run. Passage 1's host task interpolates `${ incarnation.state.endpoint }`.
// Observable: the dispatched params carry the CAPTURED value, and the incarnation row
// holds it too.
//
// The pre-run state deliberately carries a DIFFERENT value for the same field, so the
// frozen-snapshot behavior this ADR reverses is distinguishable from the accumulating
// one: drop the `capturesState` re-read from the Passage loop and this asserts
// "pre-run-endpoint" instead of "10.0.0.1". Without the differing seed the test would
// pass either way.
func TestIntegration_StateAccumulatesAcrossPassages(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationStateRoster(t, "noop-prod", map[string]any{"endpoint": "pre-run-endpoint"}, "host-a.example.com")

	gitURL := stateServiceRepo(t, stateCaptureScenario)
	disp := &paramsDispatcher{t: t}
	keepers := coremod.NewRegistry(map[string]module.SoulModule{
		coremodstate.Name: coremodstate.New(nil, nil, "secret").
			WithStore(coremodstate.NewPGStore(integrationPool)),
	})
	r := newRunnerWithKeeper(t, disp, keepers)

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

	if got := inc.State["endpoint"]; got != "10.0.0.1" {
		t.Errorf("incarnation.state.endpoint = %v, want 10.0.0.1 (the capture did not commit)", got)
	}
	if len(disp.args) != 1 {
		t.Fatalf("dispatched arg lists = %d, want 1", len(disp.args))
	}
	got := disp.args[0]
	if len(got) != 2 {
		t.Fatalf("args = %v, want 2 elements", got)
	}
	if got[0] != "endpoint" {
		t.Errorf("args[0] = %v, want \"endpoint\" (register.cap.field)", got[0])
	}
	if got[1] != "10.0.0.1" {
		t.Errorf("args[1] = %v, want \"10.0.0.1\" — Passage 1 rendered the PRE-RUN state, "+
			"so the boundary re-read of incarnation.state did not happen", got[1])
	}
}
