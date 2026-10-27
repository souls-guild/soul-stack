//go:build integration

package scenario

import (
	"context"
	"errors"
	"maps"
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

// Rescue routing across barriers (docs/scenario/rescues.md, ADR-0091
// fork 1 and amendment NIM-993, NIM-951): a host whose task failed still receives every
// `onfail:` task whose source failed on it — held for it, dropped under `host`, or after
// a failure ended the run — and nothing else while its failure is not handled.

// rescueSouls answers like a Soul that sets no rescue unit marks, through the sink a
// real stream feeds: every task of a message CHANGED, its register carrying stdout,
// until one the host is to fail — FAILED, no stdout — and after it the fail-stop with
// rescue: every task SKIPPED but an `onfail:` task whose source in the message failed,
// which runs. An `onfail:` task whose source did not fail is SKIPPED, and an applier's
// terminal folds its children. A host refused at a step has no stream for that step. It
// records what each host was sent.
type rescueSouls struct {
	t *testing.T
	// fail — sid → the tasks that fail there.
	fail map[string][]string
	// refuse — sid → the step whose send finds no stream.
	refuse map[string]int
	// noStop — sid → the tasks of fail whose failure stops nothing after it: two
	// failures reported in one message.
	noStop map[string][]string
	// failFirst — sid → the tasks that fail there whether or not their requisite fires:
	// a rescue whose source did not fail fails all the same.
	failFirst map[string][]string
	// extra — sid → task → the fields its register carries beside the usual ones.
	extra map[string]map[string]map[string]any
	// onSend — called with every message before it is answered.
	onSend func(sid string, req *keeperv1.ApplyRequest)

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
	if s.onSend != nil {
		s.onSend(sid, req)
	}

	go func() {
		ctx := context.Background()
		sink := applysink.New(applysink.Deps{DB: integrationPool, Audit: auditpg.NewWriter(integrationPool), Source: audit.SourceSoulGRPC})
		status := applyrun.StatusSuccess
		stopped := false
		failedAt, changedAt := map[int32]bool{}, map[int32]bool{}
		for i, task := range req.GetTasks() {
			ev := &keeperv1.TaskEvent{ApplyId: req.GetApplyId(), TaskIdx: int32(i), PlanIndex: task.GetPlanIndex(), Passage: int32(passage)}
			reg := map[string]any{"changed": true, "failed": false, "timed_out": false, "skipped": false, "stdout": "ok"}
			onfail := task.GetOnfailIdx()
			rescues := slices.ContainsFunc(onfail, func(p int32) bool { return failedAt[p] })
			failFirst := slices.Contains(s.failFirst[sid], task.GetName())
			switch {
			case len(task.GetAggregateOf()) > 0:
				ev.Status = keeperv1.TaskStatus_TASK_STATUS_OK
				reg = map[string]any{
					"changed":   slices.ContainsFunc(task.GetAggregateOf(), func(p int32) bool { return changedAt[p] }),
					"failed":    slices.ContainsFunc(task.GetAggregateOf(), func(p int32) bool { return failedAt[p] }),
					"timed_out": false, "skipped": false,
				}
			case !failFirst && (stopped && !rescues || len(onfail) > 0 && !rescues || task.GetWhen() == "false"):
				ev.Status = keeperv1.TaskStatus_TASK_STATUS_SKIPPED
				reg = map[string]any{"changed": false, "failed": false, "timed_out": false, "skipped": true}
			case failFirst, slices.Contains(s.fail[sid], task.GetName()):
				ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
				ev.Error = &keeperv1.TaskError{Code: "exec_failed", Module: "core.exec.run", Message: task.GetName() + " failed on " + sid}
				reg = map[string]any{"changed": false, "failed": true, "timed_out": false, "skipped": false}
				status = applyrun.StatusFailed
				failedAt[int32(i)] = true
				stopped = stopped || !slices.Contains(s.noStop[sid], task.GetName())
			default:
				ev.Status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
				changedAt[int32(i)] = true
			}
			if ev.Status != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
				maps.Copy(reg, s.extra[sid][task.GetName()])
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

const heldUntilItsRescue = `name: roll
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

// ★ GUARD (ADR-0091 amendment NIM-993, rules 2-3, superseding fork 1's end of the run
// at a failure with a rescue ahead; acceptance: radius `scenario`). `deploy` fails on
// host-a under the default radius, and its rescue `rollback` is ahead, so the failure
// is held and the run goes on. host-a gets no ordinary task while held — not `report`
// and the block `gated`, which read a field the failed deploy never produced there,
// nor `migrate`, nor `verify` beside its rescue in the group `recover` — and the
// Keeper records each SKIPPED for it. `rollback_migrate` rescues `migrate`, which
// failed nowhere, so it reaches no host. host-a gets `rollback` alone, which handles
// the failure, and the run ends ready.
//
// Mutations: never hold a failure (decideHost judging every one at once) → the run
// ends error_locked; target a held host like a live one (ForStep listing held hosts
// among the live) → render evaluates `report` for host-a and the run aborts
// render_failed;
// record nothing for a host the run holds (coveredAway over the dropped hosts alone) →
// host-a has no record of `report`.
func TestIntegration_LinearAHeldHostGetsOnlyItsRescue(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", heldUntilItsRescue), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	want := map[int]map[string][]string{
		0: {policyHostA: {"deploy"}, policyHostB: {"deploy"}},
		1: {policyHostB: {"report"}},
		2: {policyHostB: {"migrate"}},
		4: {policyHostB: {"wipe"}},
		5: {policyHostA: {"rollback"}, policyHostB: {"rollback", "verify"}},
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	names := map[int]string{0: "deploy", 1: "report", 2: "migrate", 3: "rollback_migrate", 4: "wipe", 5: "rollback", 6: "verify"}
	skippedRec, changedRec := keeperv1.TaskStatus_TASK_STATUS_SKIPPED.String(), keeperv1.TaskStatus_TASK_STATUS_CHANGED.String()
	wantRecords := map[string]string{
		"deploy": keeperv1.TaskStatus_TASK_STATUS_FAILED.String() + ":exec_failed",
		"report": skippedRec, "migrate": skippedRec, "rollback_migrate": skippedRec, "wipe": skippedRec,
		"rollback": changedRec, "verify": skippedRec,
	}
	if got := taskRecords(t, applyID, policyHostA, names); !reflect.DeepEqual(got, wantRecords) {
		t.Errorf("host-a's records = %v, want %v", got, wantRecords)
	}
	rows := rowsOf(t, applyID)
	for step := 1; step <= 4; step++ {
		if row, ok := rows[[2]any{policyHostA, step}]; ok {
			t.Errorf("held host-a holds a row at step %d: %s", step, row.Status)
		}
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

// ★ GUARD (ADR-0091 amendment NIM-993, rules 2-3; acceptance: radius `host`).
// `deploy` fails on host-a, which is held, not dropped: it gets no ordinary task while
// its rescue is ahead, yet gets `rollback` at its step, and the hosts where deploy
// succeeded do not. `rollback` handles the failure, host-a is back for `verify`, and
// the run ends ready.
//
// Mutations: target a rescue by the live roster only, as before NIM-951 → host-a is
// never sent `rollback`, recorded not reached instead; return no host (returnHost a
// no-op) → `verify` misses host-a.
func TestIntegration_LinearAHeldHostReceivesItsRescueAndReturns(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", rescueUnderHost), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	want := map[int]map[string][]string{
		0: {policyHostA: {"deploy"}, policyHostB: {"deploy"}, policyHostC: {"deploy"}},
		1: {policyHostB: {"report"}, policyHostC: {"report"}},
		2: {policyHostA: {"rollback"}},
		3: {policyHostA: {"verify"}, policyHostB: {"verify"}, policyHostC: {"verify"}},
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	if row := rowsOf(t, applyID)[[2]any{policyHostA, 2}]; row.Status != applyrun.StatusSuccess {
		t.Errorf("host-a's rescue row = %s, want success", row.Status)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 4, superseding fork 1's "a rescue is
// judged by its own radius"). A rescue's own `on_failure` is not read: when it fails,
// no rescue of `deploy` is left, and `deploy`'s failure is judged at the rescue's
// barrier by `deploy`'s radius. Under `host` host-a is dropped there and audited once,
// at that step and naming `deploy`, and both its failed rows — the source's and the
// rescue's — are marked dropped, and no other row, nor any row handled, so the run reads
// partial_failed on every read path. A rescue left at the
// default `scenario` beside a source under `host` ends nothing: host-a is dropped, and
// `verify` reaches host-b.
//
// Mutations: mark the last failed row alone (droppedRows with each source's last row)
// → the source's row is not dropped; judge a rescue's failure by the rescue's own
// radius (an entry for a unit row in decideHost) → the run ends at step 2 under
// `scenario` and `verify` misses host-b.
func TestIntegration_LinearARescuesOwnRadiusIsNotRead(t *testing.T) {
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
		dropped := map[[2]any]bool{{policyHostA, 0}: true, {policyHostA, 2}: true}
		for k, row := range rows {
			if row.Dropped != dropped[k] || dropped[k] && row.Status != applyrun.StatusFailed {
				t.Errorf("row %v = %s dropped=%v, want dropped=%v: only the source's row and the failed rescue's", k, row.Status, row.Dropped, dropped[k])
			}
		}
		assertHandledRows(t, applyID)
		assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusPartialFailed, true)
		var steps []float64
		for _, p := range auditPayloads(t, applyID, audit.EventApplyHostDropped) {
			steps = append(steps, p["step"].(float64))
			if p["task"] != "deploy" {
				t.Errorf("apply.host_dropped names task %v, want deploy", p["task"])
			}
		}
		if !reflect.DeepEqual(steps, []float64{2}) {
			t.Errorf("apply.host_dropped steps = %v, want [2]", steps)
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
		if inc.StatusDetails["reason"] != reasonPartialFailed {
			t.Errorf("reason = %v, want %s: the rescue's own radius is not read (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
		}
		errText, _ := inc.StatusDetails["error"].(string)
		for _, want := range []string{`at task "deploy" of step 0`, "deploy failed on " + policyHostA, "rollback failed on " + policyHostA} {
			if !strings.Contains(errText, want) {
				t.Errorf("run error lacks %q: %q", want, errText)
			}
		}
		if got, want := souls.sends()[3], map[string][]string{policyHostB: {"verify"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("step 3 sent = %v, want %v", got, want)
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

// ★ GUARD (rescues.md; settings.md §4, user 2026-10-08; ADR-0091 amendment NIM-993,
// rule 2). host-a has no stream for the group's step: its row fails soul_unavailable, a
// failure tied to no task. The Keeper records what a Soul's fail-stop would have — the
// first child FAILED with the row's code, the next SKIPPED, not taken — and that record
// is the failure, held for the rescue of the first child, which reaches host-a and
// handles it; the rescue of the second does not reach it, and the run ends ready.
//
// Mutations: write no record for a host that stopped reporting → host-a is judged at
// once and `unstage` misses it; FAILED on every task it did not report → `swap` is
// held too and `unswap` reaches host-a.
func TestIntegration_LinearHostNeverReachedIsRecordedForItsRescue(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, nil, map[string]int{policyHostA: 0})
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", rescueOfAGroup), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
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

// ★ GUARD (a plan-wide stopgap ahead of the per-step gate of ADR-027 amend (v), while
// no instance announces rescue_hold). On a work-queue instance every step of a plan
// holding a cross-step rescue unit goes inline, the steps before the unit and before
// the run's first failure included: an older instance's claim of any of them renders
// the whole plan, the unit too, which the run goroutine does not render for a live
// host. host-a fails `deploy` and is held, `rollback` reaches it and handles the
// failure, and host-a is back for `verify`; keeper-2 polls the queue throughout and
// claims no row. No host's stream is refused here: a refused stream on an inline step
// waits out `soul_timeout` (1m by default, longer than waitRunDone's 10s) and then
// fails `soul_unavailable`. The queue path of a refused stream is covered in
// TestIntegration_LinearQueuedStepsAfterAnInMessageRescue.
//
// Mutations: drop the rescue_hold question from Runner.queuesStep → keeper-2 claims
// the steps' rows, rendering over the recipe's one roster, which holds no held host,
// and the run does not end ready; drop the cross-step half of needsRescueHold →
// keeper-2 claims the rows of step 0, before the run's first failure.
func TestIntegration_LinearUnitPlanGoesInlineWithoutRescueHold(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
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
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	if got, want := souls.sends()[2], map[string][]string{policyHostA: {"rollback"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("step 2 sent = %v, want %v", got, want)
	}
	rows := stepRows(t, applyID)
	all := []string{policyHostA, policyHostB, policyHostC}
	live := []string{policyHostB, policyHostC}
	for step, sids := range map[int][]string{0: all, 1: live, 2: all, 3: all} {
		for _, sid := range sids {
			if row, ok := rows[step][sid]; !ok || row.kid != "" {
				t.Errorf("%s's row at step %d: present %v, claim_by_kid %q - want an inline row", sid, step, ok, row.kid)
			}
		}
	}
}

const inMessageRescueUnderHost = `name: roll
description: a deploy that fails, an ordinary step, then a group whose rescue names a sibling and the deploy
settings:
  on_failure: host
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: report
    module: core.exec.run
    params: { cmd: report }
  - name: pair
    serial: 3
    settings: { serial_scope: block }
    block:
      - name: stage
        module: core.exec.run
        register: stage
        params: { cmd: stage }
      - name: rollback
        module: core.exec.run
        onfail: [stage, deploy]
        params: { cmd: rollback }
  - name: verify
    module: core.exec.run
    register: verify
    params: { cmd: verify }
`

// ★ GUARD (ADR-027 amend (v), rescues.md; ADR-0091 amendment NIM-993, rules 2-3 and
// 7). A rescue with a source before it in its own group is no cross-step unit, so until
// the run's first failure a plan holding no other rescue goes through the work-queue. It
// is still a unit of its earlier source and can hold a host, so from that failure on
// every step goes inline: a claim renders over the recipe's one roster, which holds no
// held host. host-a fails `deploy`, and host-c's claim of `deploy` finds no stream
// (send_apply_failed): both are held, miss `report`, get `rollback` alone from the
// group inline, which handles both failures, and both are back for `verify`.
//
// Mutations: drop the first-failure half of needsRescueHold → keeper-2 claims the rows
// of `report` and `verify`; route a rescue with a source before it in its own group to
// the dropped hosts alone (droppedSIDs for notLiveSIDs in walkBlockChildren) → the held
// hosts never get `rollback`; fail the claim's send with its error instead of
// send_apply_failed → host-c's `deploy` record names no stop code.
func TestIntegration_LinearQueuedStepsAfterAnInMessageRescue(t *testing.T) {
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

	applyID := startLinearRun(t, r, linearServiceRepo(t, "roll", inMessageRescueUnderHost), "roll")
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	all := []string{policyHostA, policyHostB, policyHostC}
	want := map[int]map[string][]string{
		1: {policyHostB: {"report"}},
		2: {policyHostA: {"rollback"}, policyHostB: {"stage", "rollback"}, policyHostC: {"rollback"}},
		3: everyHost("verify", all...),
	}
	sent := souls.sends()
	for step, w := range want {
		if !reflect.DeepEqual(sent[step], w) {
			t.Errorf("step %d sent = %v, want %v", step, sent[step], w)
		}
	}
	if rec := taskRecords(t, applyID, policyHostC, map[int]string{0: "deploy"})["deploy"]; rec != keeperv1.TaskStatus_TASK_STATUS_FAILED.String()+":send_apply_failed" {
		t.Errorf("host-c's deploy record = %q, want FAILED with send_apply_failed", rec)
	}
	rows := stepRows(t, applyID)
	if row := rows[0][policyHostC]; row.kid != "keeper-2" || row.status != applyrun.StatusFailed {
		t.Errorf("host-c's step-0 row: claim_by_kid %q, status %s - want claimed by keeper-2 and failed", row.kid, row.status)
	}
	for step, sids := range map[int][]string{1: {policyHostB}, 2: all, 3: all} {
		for _, sid := range sids {
			row, ok := rows[step][sid]
			if !ok || row.kid != "" || row.status != applyrun.StatusSuccess {
				t.Errorf("%s's step-%d row: present %v, claim_by_kid %q, status %s - want an inline, successful row", sid, step, ok, row.kid, row.status)
			}
		}
	}
}

// ★ GUARD (rescues.md; ADR-0091 amendment NIM-993, rules 2 and 4). A rescue rolled
// in `serial:` waves fails on host-a in the first wave. Its own radius `scenario` is
// not read: `deploy` is judged there by its radius `host`, host-a is dropped, and the
// wave after it sends host-b its rescue — its deploy failed too, and is held for it —
// which handles host-b's failure. The run ends partial_failed.
//
// Mutation: judge a rescue's failure by the rescue's own radius (an entry for a unit
// row in decideHost) → the first wave ends the run, which ends dispatch_failed.
func TestIntegration_LinearARescueFailingInAWaveEndsNothing(t *testing.T) {
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
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	if got, want := souls.sends()[1], map[string][]string{policyHostA: {"rollback"}, policyHostB: {"rollback"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("step 1 sent = %v, want %v", got, want)
	}
	if errText, _ := inc.StatusDetails["error"].(string); strings.Contains(errText, "host "+policyHostB) {
		t.Errorf("run error names host-b, whose rescue handled its failure: %q", errText)
	}
}

const endInAWave = `name: roll
description: a held host's rescue in the wave after the one that ends the run, then a rescue past the end
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: roll
    serial: 1
    settings: { serial_scope: block }
    block:
      - name: smoke
        module: core.exec.run
        params: { cmd: smoke }
      - name: rollback
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: rollback }
  - name: recover
    serial: 3
    settings: { serial_scope: block }
    block:
      - name: undo
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: undo }
      - name: verify
        module: core.exec.run
        params: { cmd: verify }
`

// ★ GUARD (rescues.md; ADR-0091 amendment NIM-993, rules 2-3 and 9). host-b fails
// `deploy` and is held for its rescues. In the group `roll`, rolled one host a wave,
// host-a fails `smoke`, which no rescue names, so the first wave ends the run. The
// waves after it send only rescues: host-b gets `rollback` in the second, which fails,
// and host-c, whose `deploy` succeeded, gets nothing in the third. Past the end the run
// walks to `undo`, the next rescue, and the run's plan at that step names it alone, not
// the placeholder `verify` beside it. `undo` handles host-b's failure, so the run's
// error names host-a's alone.
//
// Mutations: stop the waves at the barrier that ended the run (break on endedHere in
// dispatchPassage) → host-b is never sent `rollback`; send a wave after the end whole
// (no rescueWave) → host-c is sent `smoke`; persist a step's placeholders past the end
// (passageTasks for targetedTasks in run.go) → `verify` is in the run's plan.
func TestIntegration_LinearPastAnEndInAWaveOnlyRescuesGoOut(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB, policyHostC)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"smoke"}, policyHostB: {"deploy", "rollback"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", endInAWave), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	if errText := errorOf(inc); !strings.Contains(errText, "smoke failed on "+policyHostA) || strings.Contains(errText, "host "+policyHostB) {
		t.Errorf("run error = %q, want host-a's smoke alone: undo handled host-b's deploy", errText)
	}
	sent := souls.sends()
	if got, want := sent[1], map[string][]string{policyHostA: {"smoke", "rollback"}, policyHostB: {"rollback"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 1 sent = %v, want %v", got, want)
	}
	if got, want := sent[2], map[string][]string{policyHostB: {"undo"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v", got, want)
	}
	var planned []string
	rows, err := integrationPool.Query(context.Background(), `SELECT name FROM apply_run_plan WHERE apply_id = $1 AND passage = 2 ORDER BY plan_index`, applyID)
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
	if !reflect.DeepEqual(planned, []string{"undo"}) {
		t.Errorf("run plan at step 2 = %v, want [undo]", planned)
	}
}

// ★ GUARD (rescues.md; ADR-0091 amendment NIM-993, rules 2-3). host-b fails `smoke`,
// which no rescue names, so its failure ends the run at step 0; host-a fails `deploy`
// beside it and is held for its rescues. Past the end an applier rescue is sent whole —
// its destiny's tasks and the terminal carrying its register — so when it fails there,
// the next rescue, whose `when:` reads that register, runs and handles host-a's
// failure, which leaves no entry.
//
// Mutation: send no applier terminal past the end (the terminal of a routed applier
// laid out with no targets) → `register.undo` is missing, `page` fails on the Keeper,
// and host-a's deploy is named in the run's error.
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
description: a group whose failures hold one host and end the run, an applier rescue, a rescue reading its register
tasks:
  - name: roll
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: deploy
        module: core.exec.run
        register: deploy
        params: { cmd: deploy }
      - name: smoke
        module: core.exec.run
        params: { cmd: smoke }
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
    when: "register.undo.failed"
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

	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy", "Lay down the marker file"}, policyHostB: {"smoke"}}, nil)
	r := newPolicyRunner(t, souls, nil)
	r.deps.Destiny = NewDestinySource(artifact.NewDestinyLoader(t.TempDir(), nil), fixedTemplateSource(pilotFlatDestinyRepo(t)))
	applyID := startPolicyRun(t, r, "redis-prod", "redis", "file://"+dir, "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	sent := souls.sends()
	if got := sent[1][policyHostA]; !slices.Contains(got, "applier-register undo") {
		t.Errorf("undo sent host-a %v, want the applier's terminal among its tasks", got)
	}
	if got, want := sent[2], map[string][]string{policyHostA: {"page"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v", got, want)
	}
	if errText, _ := inc.StatusDetails["error"].(string); !strings.Contains(errText, "smoke failed on "+policyHostB) || strings.Contains(errText, "host "+policyHostA) {
		t.Errorf("run error = %q, want host-b's smoke alone: page handled host-a's deploy", errText)
	}
}
