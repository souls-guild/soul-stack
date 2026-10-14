//go:build integration

package scenario

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/applysink"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Rescue routing across barriers (docs/scenario/settings.md §4, ADR-0091
// fork 1, NIM-951): a host whose task failed still receives every `onfail:` task whose
// source failed on it — dropped under `host`, or after a failure ended the run — and
// nothing else after the failure.

// rescueSouls answers like a Soul, through the sink a real stream feeds: every task of
// a message CHANGED, its register carrying stdout, until one the host is to fail —
// FAILED, no stdout — and every task after it SKIPPED, the fail-stop. A host refused
// at a step has no stream for that step. It records what each host was sent.
type rescueSouls struct {
	t *testing.T
	// fail — sid → the tasks that fail there.
	fail map[string][]string
	// refuse — sid → the step whose send finds no stream.
	refuse map[string]int

	mu   sync.Mutex
	sent map[int]map[string][]string
}

func newRescueSouls(t *testing.T, fail map[string][]string, refuse map[string]int) *rescueSouls {
	return &rescueSouls{t: t, fail: fail, refuse: refuse, sent: map[int]map[string][]string{}}
}

func (s *rescueSouls) SendApply(_ context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	if step, ok := s.refuse[sid]; ok && step == passage {
		return errors.New("stream gone")
	}
	s.mu.Lock()
	if s.sent[passage] == nil {
		s.sent[passage] = map[string][]string{}
	}
	for _, task := range req.GetTasks() {
		s.sent[passage][sid] = append(s.sent[passage][sid], task.GetName())
	}
	s.mu.Unlock()

	go func() {
		ctx := context.Background()
		sink := applysink.New(applysink.Deps{DB: integrationPool, Audit: auditpg.NewWriter(integrationPool), Source: audit.SourceSoulGRPC})
		status := applyrun.StatusSuccess
		for i, task := range req.GetTasks() {
			ev := &keeperv1.TaskEvent{ApplyId: req.GetApplyId(), TaskIdx: int32(i), PlanIndex: task.GetPlanIndex(), Passage: int32(passage)}
			reg := map[string]any{"changed": true, "failed": false, "stdout": "ok"}
			switch {
			case status == applyrun.StatusFailed:
				ev.Status = keeperv1.TaskStatus_TASK_STATUS_SKIPPED
				reg = map[string]any{"changed": false, "failed": false, "skipped": true}
			case slices.Contains(s.fail[sid], task.GetName()):
				ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
				ev.Error = &keeperv1.TaskError{Code: "exec_failed", Module: "core.exec.run", Message: task.GetName() + " failed on " + sid}
				reg = map[string]any{"changed": false, "failed": true}
				status = applyrun.StatusFailed
			default:
				ev.Status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
			}
			rd, err := structpb.NewStruct(reg)
			if err != nil {
				s.t.Errorf("register: %v", err)
			}
			ev.RegisterData = rd
			sink.TaskEvent(ctx, sid, ev)
		}
		if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, passage, status, nil); err != nil {
			s.t.Errorf("UpdateStatus(%s, step %d): %v", sid, passage, err)
		}
	}()
	return nil
}

func (s *rescueSouls) sends() map[int]map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int]map[string][]string{}
	for p, bySID := range s.sent {
		out[p] = map[string][]string{}
		for sid, names := range bySID {
			out[p][sid] = slices.Clone(names)
		}
	}
	return out
}

// taskRecords is every `task.executed` of the run on sid, by task name — the names
// resolved through the run's plan — as status[:error code].
func taskRecords(t *testing.T, applyID, sid string, names map[int]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range auditPayloads(t, applyID, audit.EventTaskExecuted) {
		if p["sid"] != sid {
			continue
		}
		rec := p["status"].(string)
		if e, ok := p["error"].(map[string]any); ok {
			rec += ":" + e["code"].(string)
		}
		out[names[int(p["plan_index"].(float64))]] = rec
	}
	return out
}

const rescueAfterEnd = `name: roll
description: a deploy that fails on one host, ordinary steps, then its rescue
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: report
    module: core.exec.run
    params: { cmd: "${ register.deploy.stdout }" }
  - name: migrate
    module: core.exec.run
    register: migrate
    params: { cmd: migrate }
  - name: rollback_migrate
    module: core.exec.run
    onfail: [migrate]
    params: { cmd: "${ register.migrate.stdout }" }
  - name: gated
    where: "register.deploy.stdout == 'ok'"
    block:
      - name: wipe
        module: core.exec.run
        params: { cmd: wipe }
  - name: recover
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: rollback
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: rollback }
      - name: verify
        module: core.exec.run
        params: { cmd: "${ register.deploy.stdout }" }
`

// ★ GUARD (settings.md §4, fork 1; acceptance: radius `scenario`). `deploy` fails on
// host-a under the default radius: the run ends at its barrier, yet walks on to the
// rescue — host-a gets `rollback`, host-b nothing — and ends failed after it. Nothing
// between them is sent or rendered: `report` and the block `gated` read a field the
// failed deploy never produced on host-a, and `rollback_migrate` rescues `migrate`,
// which never ran. The rescue's group carries it alone, and the run's plan names it
// alone at that step.
//
// Mutations: render every task after the end → the run aborts render_failed on
// `report` and `rollback` is never sent; evaluate a block's `where:` past the end →
// the same on `gated`; render a rescue that reaches no host → the same on
// `rollback_migrate`; persist a step's placeholders past the end → `verify` is in the
// run's plan.
func TestIntegration_LinearRescueRunsAfterTheRunEnded(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", rescueAfterEnd), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	if errText, _ := inc.StatusDetails["error"].(string); !strings.Contains(errText, "step 0 failed") || !strings.Contains(errText, "deploy failed on "+policyHostA) {
		t.Errorf("run error = %q, want the step-0 failure of host-a", errText)
	}
	want := map[int]map[string][]string{
		0: {policyHostA: {"deploy"}, policyHostB: {"deploy"}},
		5: {policyHostA: {"rollback"}},
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	if _, closed := rowsOf(t, applyID)[[2]any{policyHostB, 5}]; closed {
		t.Error("host-b holds a row at the rescue's step: past the end a rescue reaches only the hosts it fires on")
	}
	var planned []string
	rows, err := integrationPool.Query(context.Background(), `SELECT name FROM apply_run_plan WHERE apply_id = $1 AND passage = 5 ORDER BY plan_index`, applyID)
	if err != nil {
		t.Fatalf("apply_run_plan: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("apply_run_plan scan: %v", err)
		}
		planned = append(planned, name)
	}
	rows.Close()
	if !reflect.DeepEqual(planned, []string{"rollback"}) {
		t.Errorf("run plan at the rescue's step = %v, want [rollback]", planned)
	}
	detail, err := applyrun.SelectRunDetail(context.Background(), integrationPool, applyID, "redis-prod")
	if err != nil {
		t.Fatalf("SelectRunDetail: %v", err)
	}
	if detail.Status != applyrun.RunStatusFailed {
		t.Errorf("run status = %q, want failed: a successful rescue does not change it", detail.Status)
	}
}

const rescueUnderHost = `name: roll
description: a deploy that drops its host, an ordinary step, then its rescues
settings:
  on_failure: host
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: report
    module: core.exec.run
    params: { cmd: "${ register.deploy.stdout }" }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
  - name: verify
    module: core.exec.run
    params: { cmd: verify }
`

// ★ GUARD (settings.md §4, fork 1; acceptance: radius `host`). `deploy` fails on
// host-a, which is dropped: it gets no ordinary task after, yet still gets `rollback`
// at its step, and the hosts where deploy succeeded do not. The run goes on and ends
// partial_failed.
//
// Mutation: target a rescue by the live roster only, as before NIM-951 → host-a is
// never sent `rollback`, recorded not reached instead.
func TestIntegration_LinearDroppedHostReceivesItsRescue(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", rescueUnderHost), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	want := map[int]map[string][]string{
		0: {policyHostA: {"deploy"}, policyHostB: {"deploy"}, policyHostC: {"deploy"}},
		1: {policyHostB: {"report"}, policyHostC: {"report"}},
		2: {policyHostA: {"rollback"}},
		3: {policyHostB: {"verify"}, policyHostC: {"verify"}},
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	if row := rowsOf(t, applyID)[[2]any{policyHostA, 2}]; row.Status != applyrun.StatusSuccess {
		t.Errorf("host-a's rescue row = %s, want success", row.Status)
	}
}

// ★ GUARD (settings.md §4, fork 1). A rescue is judged by its own radius. Under `host`
// it fails on the host already dropped: the host stays out, the drop is audited again,
// and both its failed rows are marked dropped so the run still reads partial_failed.
// Under the default `scenario` its failure ends the run for every host — after which
// only rescues still go out.
//
// Mutations: mark only the first failed row of a dropped host → the run reads failed;
// judge a rescue by its source's radius → the scenario-radius rescue drops host-a and
// `verify` reaches host-b.
func TestIntegration_LinearRescueIsJudgedByItsOwnRadius(t *testing.T) {
	t.Run("host", func(t *testing.T) {
		seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
		souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy", "rollback"}}, nil)
		gitURL := linearServiceRepo(t, "roll", strings.Replace(rescueUnderHost, "    onfail: [deploy]\n", "    onfail: [deploy]\n    settings: { on_failure: host }\n", 1))
		applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", gitURL, "roll")

		inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
		if inc.StatusDetails["reason"] != reasonPartialFailed {
			t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
		}
		rows := rowsOf(t, applyID)
		for _, k := range [][2]any{{policyHostA, 0}, {policyHostA, 2}} {
			if row := rows[k]; row.Status != applyrun.StatusFailed || !row.Dropped {
				t.Errorf("row %v = %s dropped=%v, want a failed, dropped row", k, row.Status, row.Dropped)
			}
		}
		var steps []float64
		for _, p := range auditPayloads(t, applyID, audit.EventApplyHostDropped) {
			steps = append(steps, p["step"].(float64))
		}
		if !reflect.DeepEqual(steps, []float64{0, 2}) {
			t.Errorf("apply.host_dropped steps = %v, want [0 2]", steps)
		}
	})
	t.Run("scenario", func(t *testing.T) {
		seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
		souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy", "rollback"}}, nil)
		sourceUnderHost := strings.Replace(
			strings.Replace(rescueUnderHost, "settings:\n  on_failure: host\n", "", 1),
			"    register: deploy\n", "    register: deploy\n    settings: { on_failure: host }\n", 1)
		applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", sourceUnderHost), "roll")

		inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
		if inc.StatusDetails["reason"] != "dispatch_failed" {
			t.Errorf("reason = %v, want dispatch_failed: the rescue failed under scenario (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
		}
		errText, _ := inc.StatusDetails["error"].(string)
		for _, want := range []string{"deploy failed on " + policyHostA, "rollback failed on " + policyHostA} {
			if !strings.Contains(errText, want) {
				t.Errorf("run error lacks %q: %q", want, errText)
			}
		}
		if got := souls.sends()[3]; len(got) != 0 {
			t.Errorf("verify sent to %v after the rescue's failure ended the run", got)
		}
	})
}

const rescueOfAGroup = `name: roll
description: a group whose host is never reached, then a rescue per child
settings:
  on_failure: host
  soul_timeout: 300ms
tasks:
  - name: canary
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: stage
        module: core.exec.run
        register: stage
        params: { cmd: stage }
      - name: swap
        module: core.exec.run
        register: swap
        params: { cmd: swap }
  - name: unstage
    module: core.exec.run
    onfail: [stage]
    params: { cmd: unstage }
  - name: unswap
    module: core.exec.run
    onfail: [swap]
    params: { cmd: unswap }
`

// ★ GUARD (settings.md §4, fork 1; user 2026-10-08). host-a has no stream for the
// group's step: its row fails soul_unavailable, a failure tied to no task. The Keeper
// records what a Soul's fail-stop would have — the first child FAILED with the row's
// code, the next SKIPPED, not taken — so the rescue of the first child reaches host-a
// and the rescue of the second does not.
//
// Mutations: write no record for a host that stopped reporting → `unstage` misses
// host-a; FAILED on every task it did not report → `unswap` reaches host-a too.
func TestIntegration_LinearHostNeverReachedIsRecordedForItsRescue(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, nil, map[string]int{policyHostA: 0})
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", rescueOfAGroup), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	want := map[int]map[string][]string{
		0: {policyHostB: {"stage", "swap"}},
		1: {policyHostA: {"unstage"}},
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	names := map[int]string{0: "stage", 1: "swap", 2: "unstage", 3: "unswap"}
	got := taskRecords(t, applyID, policyHostA, names)
	wantRecords := map[string]string{
		"stage":   keeperv1.TaskStatus_TASK_STATUS_FAILED.String() + ":" + soulUnavailable,
		"swap":    keeperv1.TaskStatus_TASK_STATUS_SKIPPED.String(),
		"unstage": keeperv1.TaskStatus_TASK_STATUS_CHANGED.String(),
		"unswap":  keeperv1.TaskStatus_TASK_STATUS_SKIPPED.String(),
	}
	if !reflect.DeepEqual(got, wantRecords) {
		t.Errorf("host-a's records = %v, want %v", got, wantRecords)
	}
	if row := rowsOf(t, applyID)[[2]any{policyHostA, 0}]; row.FailedPlanIndex != nil {
		t.Errorf("host-a's step-0 row names failed task %d - the Keeper's records must not become its first failure", *row.FailedPlanIndex)
	}
}

// ★ GUARD (settings.md §4, fork 1; user 2026-10-08: rescue steps go through the
// work-queue). On the queue path host-a fails `deploy` and host-c's claim finds no
// stream (send_apply_failed): both are dropped, and the Acolyte that claims each one's
// `rollback` row renders the rescue for a dropped host as the run did.
//
// Mutation: render the claim without the recipe's rescue routing → the claim arrives
// at no task for a dropped host, refuses it, and `rollback` reaches neither.
func TestIntegration_LinearQueuedRescueReachesTheDroppedHosts(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, map[string]int{policyHostC: 0})
	r := newLinearQueueRunner(t, souls)
	r.deps.Audit = auditpg.NewWriter(integrationPool)
	r.deps.AuditReader = auditpg.NewReader(integrationPool)
	cr := newStepClaimRunner(t, souls, "keeper-2")
	cr.deps.Deps.Audit = auditpg.NewWriter(integrationPool)
	cr.deps.Deps.AuditReader = auditpg.NewReader(integrationPool)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_ = cr.Claim(ctx)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	applyID := startLinearRun(t, r, linearServiceRepo(t, "roll", rescueUnderHost), "roll")
	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	if got, want := souls.sends()[2], map[string][]string{policyHostA: {"rollback"}, policyHostC: {"rollback"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("step 2 sent = %v, want %v", got, want)
	}
	for _, sid := range []string{policyHostA, policyHostC} {
		if row := stepRows(t, applyID)[2][sid]; row.kid != "keeper-2" {
			t.Errorf("%s's rescue was not claimed from the queue (claim_by_kid %q)", sid, row.kid)
		}
	}
	names := map[int]string{0: "deploy"}
	if rec := taskRecords(t, applyID, policyHostC, names)["deploy"]; rec != keeperv1.TaskStatus_TASK_STATUS_FAILED.String()+":send_apply_failed" {
		t.Errorf("host-c's deploy record = %q, want FAILED with send_apply_failed", rec)
	}
}

// ★ GUARD (settings.md §4, fork 1). A rescue rolled in `serial:` waves fails on host-a
// in the first wave under its own radius `scenario`, which ends the run. The wave after
// it still sends host-b its rescue: its deploy failed too, and a failure that ends the
// run stops ordinary tasks, not rescues.
//
// Mutation: stop the waves at the barrier that ended the run, as before → host-b is
// never sent `rollback`.
func TestIntegration_LinearRescueWavesGoOnAfterTheEnd(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy", "rollback"}, policyHostB: {"deploy"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a serial rescue whose first wave ends the run
settings:
  on_failure: host
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    serial: 1
    settings: { on_failure: scenario }
    params: { cmd: rollback }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	if got, want := souls.sends()[1], map[string][]string{policyHostA: {"rollback"}, policyHostB: {"rollback"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("step 1 sent = %v, want %v", got, want)
	}
}

// ★ GUARD (settings.md §4, fork 1). Past the end an applier rescue is sent whole — its
// destiny's tasks and the terminal carrying its register — so a later rescue whose
// `when:` reads that register runs.
//
// Mutation: cut every step past the end down to the tasks that carry `onfail:` → the
// terminal is not sent, `register.undo` is missing, and `page` fails on the Keeper.
func TestIntegration_LinearApplierRescuePastTheEndKeepsItsRegister(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	for rel, content := range map[string]string{
		"service.yml": "description: applier rescue\nstate_schema: {}\ndestiny:\n  - { name: pilot-flat, ref: master }\n",
		"scenario/roll/main.yml": `name: roll
description: a deploy that ends the run, an applier rescue, a rescue reading its register
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: undo
    register: undo
    onfail: [deploy]
    apply:
      destiny: pilot-flat
      input:
        marker_file: "/etc/marker"
        marker_payload: "ok"
  - name: page
    module: core.exec.run
    onfail: [deploy]
    when: "!register.undo.failed"
    params: { cmd: page }
`,
	} {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	commitRepo(t, repo)

	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	r := newPolicyRunner(t, souls, nil)
	r.deps.Destiny = NewDestinySource(artifact.NewDestinyLoader(t.TempDir(), nil), fixedTemplateSource(pilotFlatDestinyRepo(t)))
	applyID := startPolicyRun(t, r, "redis-prod", "redis", "file://"+dir, "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	sent := souls.sends()
	if got := sent[1][policyHostA]; !slices.Contains(got, "applier-register undo") {
		t.Errorf("undo sent host-a %v, want the applier's terminal among its tasks", got)
	}
	if got, want := sent[2], map[string][]string{policyHostA: {"page"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v", got, want)
	}
}
