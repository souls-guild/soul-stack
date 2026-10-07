//go:build integration

package scenario

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// Flow control settled on the Keeper under linear dispatch (draft ADR NIM-907,
// decision 4), end to end: what the Keeper decided a host does not run, it records
// itself, and a later step reads that record.
//
// The fixtures put the steps under one default block: each child is its own step,
// and FC-5 — retired by NIM-944 — does not look inside a block, so a cross-step
// register read reaches the Keeper's evaluation here.

const (
	flowHostA = "host-a.example.com"
	flowHostB = "host-b.example.com"
)

// flowDispatcher answers each task's register for a host and closes the host's row
// asynchronously, after delay, recording the order of sends and terminals.
type flowDispatcher struct {
	t        *testing.T
	register func(sid, task string) map[string]any
	delay    time.Duration

	mu     sync.Mutex
	events []string
	wire   map[int]map[string][]*keeperv1.RenderedTask
	wg     sync.WaitGroup
}

func newFlowDispatcher(t *testing.T, register func(sid, task string) map[string]any) *flowDispatcher {
	d := &flowDispatcher{t: t, register: register, delay: 50 * time.Millisecond, wire: map[int]map[string][]*keeperv1.RenderedTask{}}
	// A failed test returns while hosts are still answering; they report into t.
	t.Cleanup(d.wg.Wait)
	return d
}

func (d *flowDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	d.mu.Lock()
	d.events = append(d.events, fmt.Sprintf("send %d %s", passage, sid))
	if d.wire[passage] == nil {
		d.wire[passage] = map[string][]*keeperv1.RenderedTask{}
	}
	d.wire[passage][sid] = req.GetTasks()
	d.mu.Unlock()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		time.Sleep(d.delay)
		for i, task := range req.GetTasks() {
			data := d.register(sid, task.GetName())
			// The CHANGED fact the cross-step requisite gate reads.
			status := keeperv1.TaskStatus_TASK_STATUS_OK
			if data["changed"] == true {
				status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
			}
			if err := auditpg.NewWriter(integrationPool).Write(context.Background(), &audit.Event{
				EventType:     audit.EventTaskExecuted,
				Source:        audit.SourceSoulGRPC,
				CorrelationID: req.GetApplyId(),
				Payload: audit.BuildTaskExecutedPayload(audit.TaskExecutedInput{
					SID: sid, ApplyID: req.GetApplyId(), TaskIdx: i, PlanIndex: int(task.GetPlanIndex()),
					Status: status.String(), Passage: passage,
				}),
				CreatedAt: time.Now().UTC(),
			}); err != nil {
				d.t.Errorf("flowDispatcher: audit Write: %v", err)
			}
			if task.GetRegister() == "" {
				continue
			}
			if err := applyrun.UpsertTaskRegister(context.Background(), integrationPool, &applyrun.TaskRegister{
				ApplyID: req.GetApplyId(), SID: sid, PlanIndex: int(task.GetPlanIndex()), TaskIdx: i,
				RegisterData: data, Passage: passage,
			}); err != nil {
				d.t.Errorf("flowDispatcher: UpsertTaskRegister(%s, %s): %v", sid, task.GetName(), err)
			}
		}
		d.mu.Lock()
		d.events = append(d.events, fmt.Sprintf("done %d %s", passage, sid))
		d.mu.Unlock()
		// A run that failed meanwhile has already closed this row.
		err := applyrun.UpdateStatus(context.Background(), integrationPool, req.GetApplyId(), sid, passage, applyrun.StatusSuccess, nil)
		if err != nil && !errors.Is(err, applyrun.ErrApplyRunAlreadyTerminal) {
			d.t.Errorf("flowDispatcher: UpdateStatus(%s, %d): %v", sid, passage, err)
		}
	}()
	return nil
}

// sent is passage → sid → task names.
func (d *flowDispatcher) sent() map[int]map[string][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[int]map[string][]string{}
	for p, bySID := range d.wire {
		out[p] = map[string][]string{}
		for sid, tasks := range bySID {
			for _, task := range tasks {
				out[p][sid] = append(out[p][sid], task.GetName())
			}
		}
	}
	return out
}

func (d *flowDispatcher) tasks(passage int, sid string) []*keeperv1.RenderedTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.wire[passage][sid]
}

func (d *flowDispatcher) task(passage int, sid, name string) *keeperv1.RenderedTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, task := range d.wire[passage][sid] {
		if task.GetName() == name {
			return task
		}
	}
	return nil
}

// sendsAfterTerminals reports a send of step k that came before a terminal of a
// step before k: the barrier released early.
func (d *flowDispatcher) sendsAfterTerminals() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	pending := map[int]int{}
	for _, ev := range d.events {
		var kind, sid string
		var p int
		fmt.Sscanf(ev, "%s %d %s", &kind, &p, &sid)
		switch kind {
		case "send":
			for q, n := range pending {
				if q < p && n > 0 {
					return fmt.Errorf("step %d sent to %s while step %d had %d host(s) running: %v", p, sid, q, n, d.events)
				}
			}
			pending[p]++
		case "done":
			pending[p]--
		}
	}
	return nil
}

func startFlowRun(t *testing.T, r *Runner, gitURL, scenarioName string, input map[string]any) string {
	t.Helper()
	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "redis", Git: gitURL, Ref: "master"},
		ScenarioName:    scenarioName,
		Input:           input,
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return applyID
}

func seedFlowHosts(t *testing.T) {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, flowHostA, []string{"redis-prod"})
	seedConnectedSoul(t, flowHostB, []string{"redis-prod"})
}

func newFlowRunner(t *testing.T, disp ApplyDispatcher, logger *slog.Logger) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	r := NewRunner(Deps{
		Loader:       artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:     topology.NewResolver(integrationPool, nil, nil),
		ServiceVars:  servicevars.NewResolver(nil),
		Render:       render.NewPipeline(nil, engine, nil, nil),
		Outbound:     disp,
		DB:           integrationPool,
		Audit:        auditpg.NewWriter(integrationPool),
		AuditReader:  auditpg.NewReader(integrationPool),
		SoulCap:      stubSoulCap{},
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
		Logger:       logger,
	})
	r.deps.ExecutionModel = ExecutionLinear
	return r
}

func hostRow(t *testing.T, applyID, sid string, passage int) applyrun.HostStatus {
	t.Helper()
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	for _, st := range statuses {
		if st.SID == sid && st.Passage == passage {
			return st
		}
	}
	t.Fatalf("no apply_runs row for %s at passage %d", sid, passage)
	return applyrun.HostStatus{}
}

// ★ GUARD (decision 4, "the skip is still recorded"). act's when: is false on B, so B
// is not sent act; the Keeper writes act's SKIPPED register on B itself, and the next
// step's `!register.act.changed` reads false from it and goes to B — while on A, where
// act changed, the next step is skipped and recorded the same way. No step is sent
// before every host of the step before it finished.
//
// Mutations: do not write the Keeper's records → the next step's when: fails on B
// (no such key) and the run ends error_locked; let a closed host's row count toward
// the barrier without counting it as dispatched → step 2 goes out while A still runs
// step 1.
func TestIntegration_LinearKeeperSkipIsRecordedAndReadByALaterStep(t *testing.T) {
	seedFlowHosts(t)
	gitURL := linearServiceRepo(t, "skip", `name: skip
description: a step skipped on one host, and a later step that reads its register
tasks:
  - name: steps
    block:
      - name: probe
        module: core.exec.run
        register: probe
        params: { cmd: probe }
      - name: act
        module: core.exec.run
        register: act
        when: register.probe.stdout == 'yes'
        params: { cmd: act }
      - name: after
        module: core.exec.run
        when: "!register.act.changed"
        params: { cmd: after }
`)
	disp := newFlowDispatcher(t, func(sid, task string) map[string]any {
		switch {
		case task == "probe" && sid == flowHostA:
			return map[string]any{"stdout": "yes", "changed": false}
		case task == "probe":
			return map[string]any{"stdout": "no", "changed": false}
		default:
			return map[string]any{"changed": true}
		}
	})
	r := newFlowRunner(t, disp, nil)
	applyID := startFlowRun(t, r, gitURL, "skip", nil)
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	disp.wg.Wait()

	want := map[int]map[string][]string{
		0: {flowHostA: {"probe"}, flowHostB: {"probe"}},
		1: {flowHostA: {"act"}},
		2: {flowHostB: {"after"}},
	}
	if got := disp.sent(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v", got, want)
	}
	if err := disp.sendsAfterTerminals(); err != nil {
		t.Error(err)
	}
	if row := hostRow(t, applyID, flowHostB, 1); row.Status != applyrun.StatusSuccess {
		t.Errorf("B's act row = %s, want success", row.Status)
	}
	if row := hostRow(t, applyID, flowHostA, 2); row.Status != applyrun.StatusSuccess {
		t.Errorf("A's after row = %s, want success", row.Status)
	}

	rows, err := applyrun.SelectTaskRegistersByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectTaskRegistersByApplyID: %v", err)
	}
	var skippedAct bool
	for _, row := range rows {
		if row.SID == flowHostB && row.Passage == 1 {
			skippedAct = row.RegisterData["skipped"] == true && row.RegisterData["changed"] == false
		}
	}
	if !skippedAct {
		t.Errorf("no SKIPPED register for act on B: %+v", rows)
	}

	// The audit fact the requisite gate and the changed-task rollup read.
	keys, err := auditpg.NewReader(integrationPool).SelectChangedTaskKeys(context.Background(), applyID)
	if err != nil {
		t.Fatalf("SelectChangedTaskKeys: %v", err)
	}
	act := disp.task(1, flowHostA, "act")
	if act == nil {
		t.Fatalf("A was not sent act")
	}
	if _, ok := keys[auditpg.ChangedTaskKey{SID: flowHostB, PlanIndex: int(act.GetPlanIndex())}]; ok {
		t.Errorf("act has a CHANGED fact on B, where it was skipped")
	}
	if n := countTaskExecuted(t, applyID, flowHostB, "TASK_STATUS_SKIPPED"); n != 1 {
		t.Errorf("B has %d SKIPPED task.executed events, want 1", n)
	}
}

// ★ GUARD (decision 4: "as it must for a host the requisite gate drops"). restart's
// onchanges: source changed on A only, so the gate drops restart on B; the Keeper
// records it SKIPPED there, and the next step reads `register.restart.skipped`.
//
// Mutation: do not record the gate's drops → the next step's when: fails on B and
// the run ends error_locked.
func TestIntegration_LinearRequisiteGateDropIsRecorded(t *testing.T) {
	seedFlowHosts(t)
	gitURL := linearServiceRepo(t, "gated", `name: gated
description: a requisite that fires on one host, and a later step that reads the skip
tasks:
  - name: steps
    block:
      - name: conf
        module: core.exec.run
        register: conf
        params: { cmd: conf }
      - name: restart
        module: core.exec.run
        register: restart
        onchanges: [conf]
        params: { cmd: restart }
      - name: after
        module: core.exec.run
        when: register.restart.skipped
        params: { cmd: after }
`)
	disp := newFlowDispatcher(t, func(sid, task string) map[string]any {
		switch task {
		case "conf":
			return map[string]any{"changed": sid == flowHostA}
		default:
			return map[string]any{"changed": true, "skipped": false}
		}
	})
	r := newFlowRunner(t, disp, nil)
	applyID := startFlowRun(t, r, gitURL, "gated", nil)
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	disp.wg.Wait()

	want := map[int]map[string][]string{
		0: {flowHostA: {"conf"}, flowHostB: {"conf"}},
		1: {flowHostA: {"restart"}},
		2: {flowHostB: {"after"}},
	}
	if got := disp.sent(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v", got, want)
	}
	if n := countTaskExecuted(t, applyID, flowHostB, "TASK_STATUS_SKIPPED"); n != 1 {
		t.Errorf("B has %d SKIPPED task.executed events, want 1 (restart)", n)
	}
}

// ★ GUARD. A loop's iterations are each skipped on one host: each host is sent the
// iteration that runs there, and the Keeper records the other one SKIPPED once the
// host's row exists, numbered after what the host was sent, where the Soul's own event
// for the sent iteration cannot collide with it.
//
// Mutations: do not write the records of a host that is still sent a message → no
// SKIPPED events; number them by their would-be position → B's record collides with
// the Soul's task_idx 0.
func TestIntegration_LinearLoopIterationSkippedBesideOneThatRuns(t *testing.T) {
	seedFlowHosts(t)
	gitURL := linearServiceRepo(t, "each", `name: each
description: a loop whose iterations each run on one host
tasks:
  - name: each
    module: core.exec.run
    loop:
      items: ["`+flowHostA+`", "`+flowHostB+`"]
    vars: { target: "${ item }" }
    when: vars.target == soulprint.self.sid
    params: { cmd: "${ item }" }
`)
	disp := newFlowDispatcher(t, func(sid, task string) map[string]any { return map[string]any{"changed": false} })
	r := newFlowRunner(t, disp, nil)
	applyID := startFlowRun(t, r, gitURL, "each", nil)
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	disp.wg.Wait()

	for _, sid := range []string{flowHostA, flowHostB} {
		sent := disp.tasks(0, sid)
		if len(sent) != 1 || sent[0].GetParams().GetFields()["cmd"].GetStringValue() != sid {
			t.Errorf("%s was sent %v, want its own iteration only", sid, sent)
		}
		var idx []int
		rows, err := integrationPool.Query(context.Background(),
			`SELECT (payload->>'task_idx')::int FROM audit_log WHERE event_type = $1 AND correlation_id = $2 AND payload->>'sid' = $3 AND payload->>'status' = 'TASK_STATUS_SKIPPED'`,
			string(audit.EventTaskExecuted), applyID, sid)
		if err != nil {
			t.Fatalf("audit_log: %v", err)
		}
		for rows.Next() {
			var i int
			if err := rows.Scan(&i); err != nil {
				t.Fatalf("scan: %v", err)
			}
			idx = append(idx, i)
		}
		rows.Close()
		if !reflect.DeepEqual(idx, []int{1}) {
			t.Errorf("%s SKIPPED task_idx = %v, want [1], after the one task it was sent", sid, idx)
		}
	}
}

// ★ GUARD. A host the Keeper fails before sending takes its place among the hosts
// the step may change, by SID: the waves before it run, the hosts sent beside it in
// its wave finish, the step fails, and the hosts of later waves are never sent it.
// A host whose every task the Keeper skipped takes no place in a wave.
//
// Mutations: close a failed host before the first wave → "first" sends B, "last" A
// only, "shares a wave" B and C; give a skipped host a wave's place → "skipped host"
// sends nothing.
func TestIntegration_LinearHostFailedEarlyFailsAtItsWave(t *testing.T) {
	const flowHostC = "host-c.example.com"
	noRC := map[string]any{"stdout": "no rc", "changed": false}
	rc := func(n int) map[string]any { return map[string]any{"rc": n, "changed": false} }
	for name, tc := range map[string]struct {
		serial   int
		probe    map[string]map[string]any
		broken   string
		wantSent map[string][]string
	}{
		"first": {
			serial: 1, broken: flowHostA,
			probe: map[string]map[string]any{flowHostA: noRC, flowHostB: rc(1), flowHostC: rc(1)},
		},
		"last": {
			serial: 1, broken: flowHostC,
			probe:    map[string]map[string]any{flowHostA: rc(1), flowHostB: rc(1), flowHostC: noRC},
			wantSent: map[string][]string{flowHostA: {"act"}, flowHostB: {"act"}},
		},
		"shares a wave": {
			serial: 2, broken: flowHostA,
			probe:    map[string]map[string]any{flowHostA: noRC, flowHostB: rc(1), flowHostC: rc(1)},
			wantSent: map[string][]string{flowHostB: {"act"}},
		},
		"skipped host takes no place": {
			serial: 2, broken: flowHostA,
			probe:    map[string]map[string]any{flowHostA: noRC, flowHostB: rc(0), flowHostC: rc(1)},
			wantSent: map[string][]string{flowHostC: {"act"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			seedFlowHosts(t)
			seedConnectedSoul(t, flowHostC, []string{"redis-prod"})
			gitURL := linearServiceRepo(t, "rolling", fmt.Sprintf(`name: rolling
description: a rolling step whose when cannot be evaluated on one host
tasks:
  - name: steps
    block:
      - name: probe
        module: core.exec.run
        register: probe
        params: { cmd: probe }
      - name: act
        module: core.exec.run
        serial: %d
        when: register.probe.rc > 0
        params: { cmd: act }
`, tc.serial))
			disp := newFlowDispatcher(t, func(sid, task string) map[string]any { return tc.probe[sid] })
			r := newFlowRunner(t, disp, nil)
			applyID := startFlowRun(t, r, gitURL, "rolling", nil)
			waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
			disp.wg.Wait()

			if got := disp.sent()[1]; !reflect.DeepEqual(got, tc.wantSent) && !(len(got) == 0 && len(tc.wantSent) == 0) {
				t.Errorf("step 1 sent %v, want %v", got, tc.wantSent)
			}
			if row := hostRow(t, applyID, tc.broken, 1); row.Status != applyrun.StatusFailed {
				t.Errorf("%s's row = %s, want failed", tc.broken, row.Status)
			}
			for sid := range tc.wantSent {
				if row := hostRow(t, applyID, sid, 1); row.Status != applyrun.StatusSuccess {
					t.Errorf("%s, sent the step, = %s, want success: it finished before the step failed", sid, row.Status)
				}
			}
		})
	}
}

func countTaskExecuted(t *testing.T, applyID, sid, status string) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE event_type = $1 AND correlation_id = $2 AND payload->>'sid' = $3 AND payload->>'status' = $4`,
		string(audit.EventTaskExecuted), applyID, sid, status).Scan(&n); err != nil {
		t.Fatalf("count task.executed: %v", err)
	}
	return n
}

// ★ GUARD (decision 4, "an evaluation error fails that host's task"). act's when:
// fails to evaluate on B: B's step fails with a Keeper-written FAILED record and a
// failed row, A is still sent act and finishes it, and then the step fails.
//
// Mutations: abort the run on a host's when: error (render_failed) → A is never sent
// act; let B's early row break the barrier at once → A's row is failed while A runs.
func TestIntegration_LinearWhenErrorFailsOnlyThatHost(t *testing.T) {
	seedFlowHosts(t)
	gitURL := linearServiceRepo(t, "broken", `name: broken
description: a when that cannot be evaluated on one host
tasks:
  - name: steps
    block:
      - name: probe
        module: core.exec.run
        register: probe
        params: { cmd: probe }
      - name: act
        module: core.exec.run
        when: register.probe.rc > 0
        params: { cmd: act }
`)
	disp := newFlowDispatcher(t, func(sid, task string) map[string]any {
		if sid == flowHostA {
			return map[string]any{"rc": 1, "changed": false}
		}
		return map[string]any{"stdout": "no rc", "changed": false}
	})
	r := newFlowRunner(t, disp, nil)
	applyID := startFlowRun(t, r, gitURL, "broken", nil)
	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	disp.wg.Wait()

	if got := disp.sent()[1]; !reflect.DeepEqual(got, map[string][]string{flowHostA: {"act"}}) {
		t.Errorf("step 1 sent %v, want act to A only", got)
	}
	// A finished what it was sent before the step failed, instead of being failed
	// while it ran.
	if row := hostRow(t, applyID, flowHostA, 1); row.Status != applyrun.StatusSuccess {
		t.Errorf("A's act row = %s, want success", row.Status)
	}
	row := hostRow(t, applyID, flowHostB, 1)
	if row.Status != applyrun.StatusFailed || row.ErrorSummary == nil || !strings.Contains(*row.ErrorSummary, `when "register.probe.rc > 0"`) {
		t.Errorf("B's row = {%s %v}, want failed naming the when:", row.Status, row.ErrorSummary)
	}
	if n := countTaskExecuted(t, applyID, flowHostB, "TASK_STATUS_FAILED"); n != 1 {
		t.Errorf("B has %d FAILED task.executed events, want 1", n)
	}
	if reason := inc.StatusDetails["reason"]; reason != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (the barrier's), not render_failed", reason)
	}
}

// ★ GUARD (decision 4, masking). A secret substituted into a rendered predicate
// reaches the host and nothing else: not the Keeper's log, not the audit log, not
// the failure reason of the host whose when: over the same secret failed.
//
// Mutations: log the rendered predicate at dispatch → the log holds the secret;
// keep CEL's error text for a when: reading a secret → B's error_summary and audit
// event quote it (no such key: <secret>).
func TestIntegration_LinearRenderedSecretStaysOnTheWire(t *testing.T) {
	const secret = "s3cr3t-token-7f2a"
	seedFlowHosts(t)
	gitURL := linearServiceRepo(t, "guarded", `name: guarded
description: a secret input substituted into a rendered predicate
input:
  token:
    type: string
    secret: true
tasks:
  - name: steps
    block:
      - name: probe
        module: core.exec.run
        register: probe
        params: { cmd: probe }
      - name: check
        module: core.exec.run
        when: register.probe.known[input.token] == 1
        failed_when: register.self.stdout != input.token
        params: { cmd: check }
`)
	disp := newFlowDispatcher(t, func(sid, task string) map[string]any {
		if sid == flowHostA {
			return map[string]any{"known": map[string]any{secret: 1}, "changed": false}
		}
		return map[string]any{"known": map[string]any{}, "changed": false}
	})
	var logs bytes.Buffer
	r := newFlowRunner(t, disp, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	applyID := startFlowRun(t, r, gitURL, "guarded", map[string]any{"token": secret})
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	disp.wg.Wait()

	check := disp.task(1, flowHostA, "check")
	if check == nil || !strings.Contains(check.GetFailedWhen(), secret) || check.GetWhen() != "" || check.GetFlowContext() != nil {
		t.Fatalf("A was sent %v, want the rendered failed_when, no when:, no flow_context", check)
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("the Keeper's log holds the secret")
	}
	rows, err := integrationPool.Query(context.Background(), `SELECT event_type, payload::text FROM audit_log WHERE correlation_id = $1`, applyID)
	if err != nil {
		t.Fatalf("audit_log: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var eventType, payload string
		if err := rows.Scan(&eventType, &payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		types = append(types, eventType)
		if strings.Contains(payload, secret) {
			t.Errorf("audit event %s holds the secret: %s", eventType, payload)
		}
	}
	sort.Strings(types)
	if len(types) == 0 {
		t.Errorf("no audit event for the run")
	}
	row := hostRow(t, applyID, flowHostB, 1)
	if row.Status != applyrun.StatusFailed || row.ErrorSummary == nil {
		t.Fatalf("B's row = {%s %v}, want failed", row.Status, row.ErrorSummary)
	}
	if strings.Contains(*row.ErrorSummary, secret) {
		t.Errorf("B's error_summary holds the secret: %s", *row.ErrorSummary)
	}
}
