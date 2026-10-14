//go:build integration

package scenario

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// A render that fails at a later step (NIM-1000): the run's record says where —
// the step, the task and the line it is written on, the host, the cell, the
// expression — and says it on every surface, without the value a failing lookup
// can quote.

const renderFailsAtStep1 = `name: create
description: the second step reads a register key the first never produced
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    changed_when: "false"
    params: { cmd: probe }

  - name: reads a missing register key
    module: core.exec.run
    params:
      cmd: echo
      args: ["${ register.probe.no_such_key }"]
`

// startRenderFailureRun runs scenario `create` linearly on one host whose probe
// answers, with input, and waits for the run to lock the incarnation.
func startRenderFailureRun(t *testing.T, scenario string, input map[string]any) (applyID string, logs *lockedBuffer) {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, scenario)

	disp := &orderedStepDispatcher{t: t, stdout: map[string]map[string]string{"probe": {"host-a.example.com": "up"}}}
	r := newRunnerInWindow(t, disp, auditpg.NewReader(integrationPool), stubSoulCap{}, nil)
	r.deps.ExecutionModel = ExecutionLinear
	logs = &lockedBuffer{}
	r.logger = slog.New(slog.NewJSONHandler(logs, nil))

	applyID = audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           input,
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	return applyID, logs
}

// runCompletedPayload is the run's incarnation.run_completed payload as stored.
func runCompletedPayload(t *testing.T, applyID string) string {
	t.Helper()
	var payload string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT payload::text FROM audit_log WHERE correlation_id = $1 AND event_type = $2`,
		applyID, string(audit.EventIncarnationRunCompleted)).Scan(&payload); err != nil {
		t.Fatalf("run_completed of %s: %v", applyID, err)
	}
	return payload
}

// ★ GUARD (NIM-1000). Step 1 fails to render after step 0 ran: the run's
// sentinel row names step 1, and the runs API, the run_completed event and the
// log carry the failure's address.
//
// Mutations: insert the sentinel at passage 0 → the sentinel reads step 0;
// drop the Failure render attaches (taskFailure returns err unchanged) → no task,
// cell or expression; leave failure out of the run_completed payload → red.
func TestIntegration_RenderFailureAtStepIsAddressed(t *testing.T) {
	applyID, logs := startRenderFailureRun(t, renderFailsAtStep1, nil)
	ctx := context.Background()

	rows, err := applyrun.SelectStatusesByApplyID(ctx, integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	var sentinel *applyrun.HostStatus
	for i := range rows {
		if rows[i].SID == render.RunSentinelSID {
			sentinel = &rows[i]
		}
	}
	if sentinel == nil || sentinel.Passage != 1 || sentinel.Status != applyrun.StatusFailed {
		t.Fatalf("rows = %+v, want the run sentinel failed at step 1", rows)
	}

	detail, err := applyrun.SelectRunDetail(ctx, integrationPool, applyID, "noop-prod")
	if err != nil {
		t.Fatalf("SelectRunDetail: %v", err)
	}
	f := detail.Failure
	if f == nil {
		t.Fatal("the run detail carries no failure")
	}
	wantSummary := `render: task "reads a missing register key" (host host-a.example.com): render: cell "args[0]": CEL eval "register.probe.no_such_key": no such key: no_such_key`
	if f.Summary != wantSummary {
		t.Errorf("summary = %q\nwant      %q", f.Summary, wantSummary)
	}
	if f.Code != "render_failed" || f.Step == nil || *f.Step != 1 {
		t.Errorf("code %q, step %v; want render_failed at step 1", f.Code, f.Step)
	}
	if len(f.Tasks) != 1 {
		t.Fatalf("tasks = %+v, want one", f.Tasks)
	}
	task := f.Tasks[0]
	if task.Index == nil || *task.Index != 1 || task.Name != "reads a missing register key" || task.File != "scenario/create/main.yml" || task.Line != 10 {
		t.Errorf("task = %+v, want #1 at scenario/create/main.yml:10", task)
	}
	if f.Host != "host-a.example.com" || f.Cell != "args[0]" || f.Expression != "register.probe.no_such_key" ||
		f.Cause != "no such key: no_such_key" || f.CauseWithheld {
		t.Errorf("failure = %+v", f)
	}

	runs, _, err := applyrun.ListRunsByIncarnation(ctx, integrationPool, "noop-prod", 0, 10)
	if err != nil || len(runs) != 1 || runs[0].Failure == nil || runs[0].Failure.Summary != wantSummary {
		t.Errorf("runs list = %+v, %v; want the run with its failure", runs, err)
	}

	var event struct {
		Failure *struct {
			Code    string `json:"code"`
			Step    *int   `json:"step"`
			Summary string `json:"summary"`
			Cell    string `json:"cell"`
		} `json:"failure"`
	}
	if err := json.Unmarshal([]byte(runCompletedPayload(t, applyID)), &event); err != nil {
		t.Fatalf("decode run_completed: %v", err)
	}
	if event.Failure == nil || event.Failure.Code != "render_failed" || event.Failure.Step == nil || *event.Failure.Step != 1 ||
		event.Failure.Summary != wantSummary || event.Failure.Cell != "args[0]" {
		t.Errorf("run_completed failure = %+v, want the run's failure", event.Failure)
	}

	if out := logs.String(); !strings.Contains(out, applyID) || !strings.Contains(out, `"step":1`) || !strings.Contains(out, "no such key: no_such_key") {
		t.Errorf("the run log does not carry the failure with its step:\n%s", out)
	}
}

// ★ GUARD (NIM-1000). A lookup keyed by a secret input fails, and CEL quotes the
// key — the secret — in its message. The secret reaches none of the places the
// failure is recorded in: status_details, the history row, the runs API, the
// run_completed event, the log, the apply_runs rows. The cause is withheld; the
// address stays.
//
// Mutations: withhold skips the check (no secret source ever read) → red in every
// place; runOutcome back on audit.MaskSecrets → still green (the secret is not a
// vault ref — the withholding is what keeps it out), which is why the check is
// render's and not the masker's.
func TestIntegration_RenderFailureWithholdsASecret(t *testing.T) {
	const secret = "s3cr3t-nim1000-7f2a"
	applyID, logs := startRenderFailureRun(t, `name: create
input:
  pw:
    type: string
    secret: true
tasks:
  - name: lookup
    module: core.exec.run
    vars: { m: { a: "1" } }
    params:
      cmd: echo
      args: ["${ vars.m[input.pw] }"]
`, map[string]any{"pw": secret})
	ctx := context.Background()

	inc, err := incarnation.SelectByID(ctx, integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByID: %v", err)
	}
	details, _ := json.Marshal(inc.StatusDetails)

	var summary, detailJSON string
	if err := integrationPool.QueryRow(ctx,
		`SELECT COALESCE(error_summary, ''), COALESCE(error_detail::text, '') FROM state_history WHERE apply_id = $1 AND run_status IS NOT NULL`,
		applyID).Scan(&summary, &detailJSON); err != nil {
		t.Fatalf("state_history: %v", err)
	}

	detail, err := applyrun.SelectRunDetail(ctx, integrationPool, applyID, "noop-prod")
	if err != nil || detail.Failure == nil {
		t.Fatalf("SelectRunDetail = %+v, %v; want a failure", detail, err)
	}
	// The failure and the host rows, not the run's `input`: the input snapshot is
	// masked by key name only and carries this secret today — NIM-1005, a defect
	// of its own that no failure path reaches.
	api, _ := json.Marshal(struct {
		Failure *applyrun.RunFailure
		Hosts   []applyrun.RunHostStatus
	}{detail.Failure, detail.Hosts})

	rows, err := applyrun.SelectStatusesByApplyID(ctx, integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	var rowSummaries []string
	for _, row := range rows {
		if row.ErrorSummary != nil {
			rowSummaries = append(rowSummaries, *row.ErrorSummary)
		}
	}

	for place, text := range map[string]string{
		"status_details":           string(details),
		"state_history.summary":    summary,
		"state_history.detail":     detailJSON,
		"runs API":                 string(api) + detail.Failure.Summary,
		"run_completed":            runCompletedPayload(t, applyID),
		"log":                      logs.String(),
		"apply_runs.error_summary": strings.Join(rowSummaries, "\n"),
	} {
		if strings.Contains(text, secret) {
			t.Errorf("%s carries the secret: %s", place, text)
		}
	}

	f := detail.Failure
	if !f.CauseWithheld || f.Expression != "vars.m[input.pw]" || f.Cell != "args[0]" || len(f.Tasks) != 1 || f.Tasks[0].Name != "lookup" {
		t.Errorf("failure = %+v, want the address kept and the cause withheld", f)
	}
	if !strings.Contains(summary, `CEL eval "vars.m[input.pw]"`) {
		t.Errorf("summary = %q, want the chain up to the withheld cause", summary)
	}
}
