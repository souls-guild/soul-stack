//go:build integration

package scenario

import (
	"context"
	"encoding/json"
	"errors"
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

// The failure policy of a linear run's barrier (docs/scenario/settings.md §4,
// NIM-948): every host a step was sent to is waited for, each failure is judged by
// its task's `on_failure`, a host dropped under `host` stays out of the run, and a
// run that reaches its end without some hosts is `partial_failed`.

// failingStepDispatcher answers like a Soul, after a per-host delay: a host whose
// failing task is in the message reports that task failed — the first failure wins
// its row, as RecordTaskFailure keeps it — and every task before it CHANGED, in the
// audit log the changed-task rollup reads. It records what each host was sent per
// step and the `args` of every task.
type failingStepDispatcher struct {
	t     *testing.T
	delay map[string]time.Duration
	// fail — sid → name of the task that fails on that host.
	fail map[string]string

	mu   sync.Mutex
	sent map[int]map[string][]string
	args map[int]map[string][]any
}

func newFailingStepDispatcher(t *testing.T, fail map[string]string) *failingStepDispatcher {
	return &failingStepDispatcher{t: t, fail: fail, delay: map[string]time.Duration{},
		sent: map[int]map[string][]string{}, args: map[int]map[string][]any{}}
}

func (d *failingStepDispatcher) SendApply(_ context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()
	d.mu.Lock()
	if d.sent[passage] == nil {
		d.sent[passage] = map[string][]string{}
		d.args[passage] = map[string][]any{}
	}
	for _, task := range req.GetTasks() {
		d.sent[passage][sid] = append(d.sent[passage][sid], task.GetName())
		if a, ok := task.GetParams().AsMap()["args"].([]any); ok {
			d.args[passage][sid] = append(d.args[passage][sid], a...)
		}
	}
	d.mu.Unlock()

	go func() {
		ctx := context.Background()
		time.Sleep(d.delay[sid])
		status := applyrun.StatusSuccess
		for i, task := range req.GetTasks() {
			if d.fail[sid] != task.GetName() {
				if err := auditpg.NewWriter(integrationPool).Write(ctx, &audit.Event{
					EventType:     audit.EventTaskExecuted,
					Source:        audit.SourceSoulGRPC,
					CorrelationID: applyID,
					Payload: audit.BuildTaskExecutedPayload(audit.TaskExecutedInput{
						SID: sid, ApplyID: applyID, TaskIdx: i, PlanIndex: int(task.GetPlanIndex()),
						Status: keeperv1.TaskStatus_TASK_STATUS_CHANGED.String(), Passage: passage,
					}),
				}); err != nil {
					d.t.Errorf("audit CHANGED(%s): %v", sid, err)
				}
				continue
			}
			summary := "task " + task.GetName() + " core.exec.run: boom on " + sid
			if err := applyrun.RecordTaskFailure(ctx, integrationPool, applyID, sid, passage, i, int(task.GetPlanIndex()), summary); err != nil {
				d.t.Errorf("RecordTaskFailure(%s): %v", sid, err)
			}
			status = applyrun.StatusFailed
			break
		}
		if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, status, nil); err != nil {
			d.t.Errorf("UpdateStatus(%s, passage=%d): %v", sid, passage, err)
		}
	}()
	return nil
}

func (d *failingStepDispatcher) sends() map[int]map[string][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[int]map[string][]string{}
	for p, bySID := range d.sent {
		out[p] = map[string][]string{}
		for sid, names := range bySID {
			out[p][sid] = append([]string(nil), names...)
		}
	}
	return out
}

func newPolicyRunner(t *testing.T, disp ApplyDispatcher, keepers KeeperModuleRegistry) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	r := NewRunner(Deps{
		Loader:        artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:      topology.NewResolver(integrationPool, nil, nil),
		ServiceVars:   servicevars.NewResolver(nil),
		Render:        render.NewPipeline(nil, engine, nil, nil),
		Outbound:      disp,
		DB:            integrationPool,
		KeeperModules: keepers,
		Audit:         auditpg.NewWriter(integrationPool),
		AuditReader:   auditpg.NewReader(integrationPool),
		SoulCap:       stubSoulCap{},
		PollInterval:  20 * time.Millisecond,
		RunTimeout:    20 * time.Second,
	})
	r.deps.ExecutionModel = ExecutionLinear
	return r
}

func startPolicyRun(t *testing.T, r *Runner, incName, service, gitURL, scenario string) string {
	t.Helper()
	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: incName,
		ServiceRef:      artifact.ServiceRef{Name: service, Git: gitURL, Ref: "master"},
		ScenarioName:    scenario,
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return applyID
}

// rowsOf is the run's rows, (sid, step) → row.
func rowsOf(t *testing.T, applyID string) map[[2]any]applyrun.HostStatus {
	t.Helper()
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	out := map[[2]any]applyrun.HostStatus{}
	for _, hs := range statuses {
		out[[2]any{hs.SID, hs.Passage}] = hs
	}
	return out
}

// auditPayloads is the payload of every audit event of the run of type eventType.
func auditPayloads(t *testing.T, applyID string, eventType audit.EventType) []map[string]any {
	t.Helper()
	rows, err := integrationPool.Query(context.Background(),
		`SELECT payload FROM audit_log WHERE correlation_id = $1 AND event_type = $2 ORDER BY audit_id`, applyID, string(eventType))
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("audit scan: %v", err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("audit payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

const policyHostA, policyHostB, policyHostC, policyHostD = "host-a.example.com", "host-b.example.com", "host-c.example.com", "host-d.example.com"

func seedPolicyRoster(t *testing.T, inc string, sids ...string) {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, inc)
	for _, sid := range sids {
		seedConnectedSoul(t, sid, []string{inc})
	}
}

const threeStepsUnderHost = `name: roll
description: three steps, every failure drops its host
settings:
  on_failure: host
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: first }
  - name: second
    module: core.exec.run
    params: { cmd: second }
  - name: third
    module: core.exec.run
    params: { cmd: third }
`

// ★ GUARD (settings.md §4, acceptance 1). Under `on_failure: host` two hosts fail at
// different steps: each is dropped where it failed and sent nothing after, the third
// host runs every step, and the run ends partial_failed with its incarnation
// error_locked. Each drop is audited, and each later task that would have reached a
// dropped host is recorded SKIPPED on it.
//
// Mutations: keep a dropped host in the next step's roster → host-a is sent
// `second`; skip MarkDropped → the run reads failed; end the run on a host-radius
// failure → host-c never runs `third`; let a later step's re-render replace the
// plan of a step that ran → `first` counts host-c alone in run_completed.
func TestIntegration_LinearHostRadiusDropsAndFinishesPartial(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	gitURL := linearServiceRepo(t, "roll", threeStepsUnderHost)
	disp := newFailingStepDispatcher(t, map[string]string{policyHostA: "first", policyHostB: "second"})
	applyID := startPolicyRun(t, newPolicyRunner(t, disp, nil), "redis-prod", "redis", gitURL, "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	want := map[int]map[string][]string{
		0: {policyHostA: {"first"}, policyHostB: {"first"}, policyHostC: {"first"}},
		1: {policyHostB: {"second"}, policyHostC: {"second"}},
		2: {policyHostC: {"third"}},
	}
	if got := disp.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}

	detail, err := applyrun.SelectRunDetail(context.Background(), integrationPool, applyID, "redis-prod")
	if err != nil {
		t.Fatalf("SelectRunDetail: %v", err)
	}
	if detail.Status != applyrun.RunStatusPartialFailed {
		t.Errorf("run status = %q, want partial_failed", detail.Status)
	}
	rows := rowsOf(t, applyID)
	for _, k := range [][2]any{{policyHostA, 0}, {policyHostB, 1}} {
		if row := rows[k]; row.Status != applyrun.StatusFailed || !row.Dropped {
			t.Errorf("row %v = %s dropped=%v, want a failed, dropped row", k, row.Status, row.Dropped)
		}
	}

	drops := auditPayloads(t, applyID, audit.EventApplyHostDropped)
	got := map[string][2]any{}
	for _, p := range drops {
		got[p["sid"].(string)] = [2]any{p["step"], p["task"]}
	}
	wantDrops := map[string][2]any{policyHostA: {float64(0), "first"}, policyHostB: {float64(1), "second"}}
	if !reflect.DeepEqual(got, wantDrops) {
		t.Errorf("apply.host_dropped = %v, want %v", got, wantDrops)
	}

	var notReached []string
	for _, p := range auditPayloads(t, applyID, audit.EventTaskExecuted) {
		if p["status"] == keeperv1.TaskStatus_TASK_STATUS_SKIPPED.String() {
			notReached = append(notReached, p["sid"].(string)+"@"+jsonNum(p["passage"]))
		}
	}
	sort.Strings(notReached)
	if wantNR := []string{policyHostA + "@1", policyHostA + "@2", policyHostB + "@2"}; !reflect.DeepEqual(notReached, wantNR) {
		t.Errorf("not-reached records = %v, want %v", notReached, wantNR)
	}

	var completed []string
	changed := map[string][2]any{}
	for _, p := range auditPayloads(t, applyID, audit.EventIncarnationRunCompleted) {
		completed = append(completed, p["status"].(string))
		for _, raw := range p["changed_tasks"].([]any) {
			ct := raw.(map[string]any)
			changed[ct["name"].(string)] = [2]any{ct["changed_hosts"], ct["total_hosts"]}
		}
	}
	if !reflect.DeepEqual(completed, []string{"partial_failed"}) {
		t.Errorf("incarnation.run_completed statuses = %v, want [partial_failed]", completed)
	}
	// host-b changed `first` before it was dropped at `second`: the rollup counts the
	// hosts each task ran on, not the roster of the last step.
	wantChanged := map[string][2]any{"first": {float64(2), float64(3)}, "second": {float64(1), float64(2)}, "third": {float64(1), float64(1)}}
	if !reflect.DeepEqual(changed, wantChanged) {
		t.Errorf("run_completed changed_tasks (changed, total) = %v, want %v", changed, wantChanged)
	}
}

func jsonNum(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ★ GUARD (settings.md §4, acceptance 2). Two hosts fail at the same step under the
// default radius, one of them slowly, while a third works on. The barrier waits for
// all three: both failures are in the run's error, the slow host's row carries its
// own failure rather than a forced one, the working host finishes, and no later step
// is sent.
//
// Mutation: return from the barrier at the first failed row → host-b is force-failed
// `dispatch_failed` and its failure is missing from the error.
func TestIntegration_LinearScenarioRadiusWaitsForEveryHost(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	gitURL := linearServiceRepo(t, "roll", `name: roll
description: two steps under the default radius
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: first }
  - name: second
    module: core.exec.run
    params: { cmd: second }
`)
	disp := newFailingStepDispatcher(t, map[string]string{policyHostA: "first", policyHostB: "first"})
	disp.delay[policyHostB] = 400 * time.Millisecond
	disp.delay[policyHostC] = 400 * time.Millisecond
	applyID := startPolicyRun(t, newPolicyRunner(t, disp, nil), "redis-prod", "redis", gitURL, "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	errText, _ := inc.StatusDetails["error"].(string)
	for _, sid := range []string{policyHostA, policyHostB} {
		if !strings.Contains(errText, "boom on "+sid) {
			t.Errorf("run error lacks %s's failure: %q", sid, errText)
		}
	}
	rows := rowsOf(t, applyID)
	if row := rows[[2]any{policyHostB, 0}]; row.Status != applyrun.StatusFailed || row.ErrorSummary == nil || !strings.Contains(*row.ErrorSummary, "boom on "+policyHostB) {
		t.Errorf("host-b row = %s %v, want its own failure", row.Status, row.ErrorSummary)
	}
	if row := rows[[2]any{policyHostC, 0}]; row.Status != applyrun.StatusSuccess {
		t.Errorf("host-c row = %s, want success: a host still working is not failed for another's failure", row.Status)
	}
	if got := disp.sends(); len(got[1]) != 0 {
		t.Errorf("step 1 sent to %v after the run ended", got[1])
	}
}

// ★ GUARD (settings.md §6, ADR item 5; acceptance 3). A host dropped at the first step
// is still online when a refresh boundary re-reads the roster. It stays out: the step
// after the boundary goes to the host that stayed and the one that joined, and
// soulprint.hosts lists the dropped host with `dropped: true`.
//
// Mutation: apply the drop set only on steps without a refresh boundary → host-b is
// re-admitted and sent `configure`.
func TestIntegration_LinearDroppedHostStaysOutAcrossRefresh(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	gitURL := writeServiceRepo(t, `name: create
description: a drop, a refresh boundary, a step after it
settings:
  on_failure: host
tasks:
  - name: install
    module: core.exec.run
    params: { cmd: install }
  - name: Register and refresh roster
    module: core.soul.registered
    register: roster
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
  - name: configure
    module: core.exec.run
    params:
      cmd: configure
      args:
        - "${ string(size(soulprint.hosts)) }"
        - "${ string(size(soulprint.hosts.filter(h, !h.dropped))) }"
        - "${ soulprint.hosts.filter(h, h.dropped)[0].sid }"
`)
	var seedOnce sync.Once
	keepers := keeperRegistryWith(nil)
	keepers["core.soul"] = &seedingKeeperModule{onApply: func() {
		seedOnce.Do(func() { seedConnectedSoul(t, policyHostC, []string{"redis-prod"}) })
	}}
	disp := newFailingStepDispatcher(t, map[string]string{policyHostB: "install"})
	applyID := startPolicyRun(t, newPolicyRunner(t, disp, keepers), "redis-prod", "noop", gitURL, "create")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s", inc.StatusDetails["reason"], reasonPartialFailed)
	}
	sent := disp.sends()
	if want := map[string][]string{policyHostA: {"configure"}, policyHostC: {"configure"}}; !reflect.DeepEqual(sent[2], want) {
		t.Fatalf("step 2 sent = %v, want %v - the dropped host must not come back at the refresh boundary", sent[2], want)
	}
	disp.mu.Lock()
	args := disp.args[2][policyHostA]
	disp.mu.Unlock()
	if want := []any{"3", "2", policyHostB}; !reflect.DeepEqual(args, want) {
		t.Errorf("soulprint.hosts at step 2 = %v, want %v (all three, two in the run, host-b dropped)", args, want)
	}
}

// ★ GUARD (settings.md §2.1, acceptance 4). `tolerate: 1` goes on after one dropped
// host and ends the run at the second.
//
// Mutation: `>=` for `>` in Tolerate.Exceeded → one drop ends the run.
func TestIntegration_LinearTolerateOneGoesOnAfterOneEndsAtTwo(t *testing.T) {
	const scenario = `name: roll
description: two steps, one dropped host tolerated
settings:
  on_failure: host
  tolerate: 1
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: first }
  - name: second
    module: core.exec.run
    params: { cmd: second }
`
	t.Run("one", func(t *testing.T) {
		seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC, policyHostD)
		disp := newFailingStepDispatcher(t, map[string]string{policyHostA: "first"})
		applyID := startPolicyRun(t, newPolicyRunner(t, disp, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", scenario), "roll")
		inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
		if inc.StatusDetails["reason"] != reasonPartialFailed {
			t.Errorf("reason = %v, want %s: one drop is within tolerate: 1 (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
		}
		if got := len(disp.sends()[1]); got != 3 {
			t.Errorf("step 1 sent to %d hosts, want 3", got)
		}
	})
	t.Run("two", func(t *testing.T) {
		seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC, policyHostD)
		disp := newFailingStepDispatcher(t, map[string]string{policyHostA: "first", policyHostB: "first"})
		applyID := startPolicyRun(t, newPolicyRunner(t, disp, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", scenario), "roll")
		inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
		if inc.StatusDetails["reason"] != "dispatch_failed" {
			t.Errorf("reason = %v, want dispatch_failed: two drops exceed tolerate: 1", inc.StatusDetails["reason"])
		}
		if got := disp.sends()[1]; len(got) != 0 {
			t.Errorf("step 1 sent to %v after the run ended", got)
		}
		detail, err := applyrun.SelectRunDetail(context.Background(), integrationPool, applyID, "redis-prod")
		if err != nil {
			t.Fatalf("SelectRunDetail: %v", err)
		}
		if detail.Status != applyrun.RunStatusFailed {
			t.Errorf("run status = %q, want failed", detail.Status)
		}
	})
}

// ★ GUARD (settings.md §2.1). A step that tightens `tolerate` ends the run at its own
// barrier, where nothing failed, over a drop the scenario's threshold allowed.
//
// Mutation: check tolerate only at a barrier that judged a failure → the run goes on
// to `third` and ends partial_failed.
func TestIntegration_LinearTightenedTolerateEndsAtAQuietBarrier(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	disp := newFailingStepDispatcher(t, map[string]string{policyHostA: "first"})
	applyID := startPolicyRun(t, newPolicyRunner(t, disp, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a drop the scenario tolerates, then a step that tolerates none
settings:
  on_failure: host
  tolerate: 1
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: first }
  - name: second
    module: core.exec.run
    settings: { tolerate: 0 }
    params: { cmd: second }
  - name: third
    module: core.exec.run
    params: { cmd: third }
`), "roll")
	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	if got := disp.sends(); len(got[1]) != 2 || len(got[2]) != 0 {
		t.Errorf("sent = %v, want `second` on the two hosts left and `third` on none", got)
	}
}

// ★ GUARD (settings.md §2.1). A step that targets no host is still a barrier: one that
// tightens `tolerate` ends the run there.
//
// Mutation: return from a step with nothing to send before judging it → the run goes
// on to `third` and ends partial_failed.
func TestIntegration_LinearEmptyStepStillChecksTolerate(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	disp := newFailingStepDispatcher(t, map[string]string{policyHostA: "first"})
	applyID := startPolicyRun(t, newPolicyRunner(t, disp, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a tolerated drop, then a step for nobody that tolerates none
settings:
  on_failure: host
  tolerate: 1
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: first }
  - name: nobody
    module: core.exec.run
    where: "soulprint.self.sid == 'nobody.example.com'"
    settings: { tolerate: 0 }
    params: { cmd: nobody }
  - name: third
    module: core.exec.run
    params: { cmd: third }
`), "roll")
	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	if got := disp.sends(); len(got[2]) != 0 {
		t.Errorf("third sent to %v after a barrier that tolerates no drop", got[2])
	}
}

// ★ GUARD (settings.md §6). Every host dropped is not a partial run: the run ends
// failed at the barrier that emptied it, and no row is marked dropped.
//
// Mutation: drop the "no host left" check → the run walks its remaining steps with
// nobody and ends partial_failed.
func TestIntegration_LinearEveryHostDroppedFails(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	disp := newFailingStepDispatcher(t, map[string]string{policyHostA: "first", policyHostB: "first"})
	applyID := startPolicyRun(t, newPolicyRunner(t, disp, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", threeStepsUnderHost), "roll")
	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed", inc.StatusDetails["reason"])
	}
	for k, row := range rowsOf(t, applyID) {
		if row.Dropped {
			t.Errorf("row %v marked dropped in a run that ended failed", k)
		}
	}
}

// sendFailingDispatcher refuses the send to the hosts in refuse, as a Soul whose
// stream is gone does, and answers the rest like failingStepDispatcher.
type sendFailingDispatcher struct {
	*failingStepDispatcher
	refuse map[string]bool
}

func (d *sendFailingDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	if d.refuse[sid] {
		return errors.New("stream gone")
	}
	return d.failingStepDispatcher.SendApply(ctx, sid, req)
}

// ★ GUARD (settings.md §4). A send that fails is a failure tied to no task: the row is
// failed `send_apply_failed`, the step goes to the rest of the wave, and the barrier
// owner's radius judges it — under `host` the host is dropped and the run finishes
// without it.
//
// Mutation: return from the wave at a failed send, as a staged run does → the run
// ends dispatch_failed and host-c is never sent the step.
func TestIntegration_LinearFailedSendIsJudgedByTheStep(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	disp := &sendFailingDispatcher{failingStepDispatcher: newFailingStepDispatcher(t, nil), refuse: map[string]bool{policyHostB: true}}
	applyID := startPolicyRun(t, newPolicyRunner(t, disp, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", threeStepsUnderHost), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	want := map[int]map[string][]string{
		0: {policyHostA: {"first"}, policyHostC: {"first"}},
		1: {policyHostA: {"second"}, policyHostC: {"second"}},
		2: {policyHostA: {"third"}, policyHostC: {"third"}},
	}
	if got := disp.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	row := rowsOf(t, applyID)[[2]any{policyHostB, 0}]
	if row.Status != applyrun.StatusFailed || !row.Dropped || row.ErrorSummary == nil || *row.ErrorSummary != "send_apply_failed" {
		t.Errorf("host-b row = %s dropped=%v summary=%v, want a dropped send_apply_failed row", row.Status, row.Dropped, row.ErrorSummary)
	}
	drops := auditPayloads(t, applyID, audit.EventApplyHostDropped)
	if len(drops) != 1 || drops[0]["sid"] != policyHostB {
		t.Errorf("apply.host_dropped = %v, want host-b alone", drops)
	} else if _, named := drops[0]["task"]; named {
		t.Errorf("a failure tied to no task names one: %v", drops[0])
	}
}

// ★ GUARD (settings.md §4, NIM-961). A step that goes through the work-queue is
// judged by the same barrier as an inline one: under `on_failure: host` the hosts
// that fail are dropped and the run finishes partial_failed. The Acolyte's render
// of a later step lists the dropped hosts in soulprint.hosts, as the run's own did.
//
// Mutations: keep the old fail-fast barrier for a queued step → the run ends
// dispatch_failed at host-a's failure; render the claim without the recipe's
// dropped hosts → host-c is sent soulprint.hosts of one host.
func TestIntegration_LinearQueuedStepJudgesFailures(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	gitURL := linearServiceRepo(t, "roll", `name: roll
description: three steps through the queue, every failure drops its host
settings:
  on_failure: host
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: first }
  - name: second
    module: core.exec.run
    params: { cmd: second }
  - name: third
    module: core.exec.run
    params:
      cmd: third
      args:
        - "${ string(size(soulprint.hosts)) }"
        - "${ string(size(soulprint.hosts.filter(h, !h.dropped))) }"
`)
	disp := newFailingStepDispatcher(t, map[string]string{policyHostA: "first", policyHostB: "second"})
	r := newLinearQueueRunner(t, disp)
	runAcolyte(t, disp, "keeper-2")

	applyID := startLinearRun(t, r, gitURL, "roll")
	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	want := map[int]map[string][]string{
		0: {policyHostA: {"first"}, policyHostB: {"first"}, policyHostC: {"first"}},
		1: {policyHostB: {"second"}, policyHostC: {"second"}},
		2: {policyHostC: {"third"}},
	}
	if got := disp.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	for step, bySID := range stepRows(t, applyID) {
		for sid, row := range bySID {
			if row.kid != "keeper-2" {
				t.Errorf("step %d of %s was not claimed from the queue (claim_by_kid %q)", step, sid, row.kid)
			}
		}
	}
	disp.mu.Lock()
	args := disp.args[2][policyHostC]
	disp.mu.Unlock()
	if want := []any{"3", "1"}; !reflect.DeepEqual(args, want) {
		t.Errorf("soulprint.hosts as the claim rendered it = %v, want %v", args, want)
	}
}

// ★ GUARD (settings.md §4). A step that failed to dispatch part-way ends the run only
// once the hosts already sent have finished — the abort would close a row a live
// host is applying — and a Cancel that arrives meanwhile still reaches the abort.
//
// Mutations: return the dispatch error without waiting → host-a's row is still
// running when the step gives up; log a Cancel seen while waiting instead of joining
// it → the run ends failed and its live rows are force-failed.
func TestIntegration_FailAfterSentWaitsAndKeepsTheCancel(t *testing.T) {
	ctx := context.Background()
	cause := errors.New("insert failed")
	insertRunning := func(t *testing.T, applyID, sid string) {
		t.Helper()
		if err := applyrun.Insert(ctx, integrationPool, &applyrun.ApplyRun{
			ApplyID: applyID, SID: sid, IncarnationName: "redis-prod", Scenario: "roll",
			Status: applyrun.StatusRunning, Passage: 0,
		}); err != nil {
			t.Fatalf("Insert(%s): %v", sid, err)
		}
	}

	t.Run("waits", func(t *testing.T) {
		seedPolicyRoster(t, "redis-prod", policyHostA)
		r := newPolicyRunner(t, newFailingStepDispatcher(t, nil), nil)
		applyID := audit.NewULID()
		insertRunning(t, applyID, policyHostA)
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = applyrun.UpdateStatus(ctx, integrationPool, applyID, policyHostA, 0, applyrun.StatusSuccess, nil)
		}()
		err := r.failAfterSent(ctx, RunSpec{ApplyID: applyID}, discardLog(), 0, 1, cause)
		if !errors.Is(err, cause) || errors.Is(err, errCancelRequested) {
			t.Fatalf("failAfterSent = %v, want the dispatch error alone", err)
		}
		if row := rowsOf(t, applyID)[[2]any{policyHostA, 0}]; row.Status != applyrun.StatusSuccess {
			t.Errorf("host-a's row is %s when the step gave up, want its own success", row.Status)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		seedPolicyRoster(t, "redis-prod", policyHostA)
		r := newPolicyRunner(t, newFailingStepDispatcher(t, nil), nil)
		applyID := audit.NewULID()
		insertRunning(t, applyID, policyHostA)
		if _, err := applyrun.RequestCancel(ctx, integrationPool, applyID); err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		err := r.failAfterSent(ctx, RunSpec{ApplyID: applyID}, discardLog(), 0, 1, cause)
		if !errors.Is(err, cause) || !errors.Is(err, errCancelRequested) {
			t.Errorf("failAfterSent = %v, want the dispatch error joined with the Cancel", err)
		}
	})
}

// ★ GUARD (NIM-972 over NIM-948). A linear run's barrier is waitStep, so it reads
// the applying lock's Cancel as the staged barrier does: it acts on it while the
// host's row is running inline, and defers it while a queued row is dispatched — the
// abort would close a delivered row while its Soul applies it.
//
// Mutations: read the row flags only in waitStep → "acts" waits on the held host
// past waitRunDone's deadline; act on the lock whatever the rows → "defers" stops
// the run while the row is dispatched.
func TestIntegration_LinearBarrierReadsTheLockCancel(t *testing.T) {
	const host = "host-a.example.com"
	body := `name: create
description: one host step
tasks:
  - name: host work
    module: core.exec.run
    changed_when: "false"
    params: { cmd: work }
`
	setup := func(t *testing.T) string {
		resetAll(t)
		seedOperator(t, "archon-alice")
		seedIncarnation(t, "noop-prod")
		seedConnectedSoul(t, host, []string{"noop-prod"})
		return writeServiceRepo(t, body)
	}
	ctx := context.Background()

	t.Run("acts", func(t *testing.T) {
		gitURL := setup(t)
		owner := newRunnerInWindow(t, newHeldDispatcher(t), auditpg.NewReader(integrationPool), stubSoulCap{}, nil)
		applyID := startModelRun(t, owner, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
		waitHostRow(t, applyID, host, applyrun.StatusRunning)
		if locked, err := incarnation.RequestRunCancel(ctx, integrationPool, applyID); err != nil || !locked {
			t.Fatalf("RequestRunCancel = %v, %v; want the lock flagged", locked, err)
		}
		waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
		if rows := hostRows(t, applyID); len(rows) != 1 || rows[0].Status != applyrun.StatusRunning {
			t.Errorf("host rows when the run stopped = %+v, want the live apply left running", rows)
		}
	})

	t.Run("defers", func(t *testing.T) {
		gitURL := setup(t)
		owner := newQueueRunnerInWindow(t, stubSoulCap{}, nil)
		owner.deps.StepQueue = stepQueueAnswer{ok: true}
		applyID := startModelRun(t, owner, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
		waitHostRow(t, applyID, host, applyrun.StatusPlanned)
		if _, err := integrationPool.Exec(ctx,
			`UPDATE apply_runs SET status = 'dispatched' WHERE apply_id = $1 AND sid = $2`, applyID, host); err != nil {
			t.Fatalf("mark dispatched: %v", err)
		}
		if found, err := newQueueRunnerInWindow(t, stubSoulCap{}, nil).RequestCancel(ctx, applyID); err != nil || !found {
			t.Fatalf("RequestCancel = %v, %v; want found", found, err)
		}
		time.Sleep(300 * time.Millisecond)
		inc, err := incarnation.SelectByID(ctx, integrationPool, "noop-prod")
		if err != nil {
			t.Fatalf("SelectByID: %v", err)
		}
		if inc.Status != incarnation.StatusApplying {
			t.Fatalf("the run stopped (status %q) while its row was dispatched", inc.Status)
		}
		if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, host, 0, applyrun.StatusSuccess, nil); err != nil {
			t.Fatalf("the Soul's result: %v", err)
		}
		waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
		if rows := hostRows(t, applyID); len(rows) != 1 || rows[0].Status != applyrun.StatusSuccess {
			t.Errorf("host rows = %+v, want the Soul's success", rows)
		}
	})
}
