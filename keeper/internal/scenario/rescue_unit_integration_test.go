//go:build integration

package scenario

import (
	"context"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). A linear run whose group holds
// two rescue units for sources outside it is refused group_multiple_rescue_units
// before anything is sent. A staged run rolls no block as one group, so the same
// file runs.
//
// Mutation: drop the GroupMultipleRescueUnits gate from Runner.run → the linear
// run gets past it and ends on the later cross-step requisite gate instead (this
// harness has no audit reader), with another reason.
func TestIntegration_GroupWithTwoRescueUnitsIsRefused(t *testing.T) {
	const host = "host-a.example.com"
	for _, model := range []ExecutionModel{ExecutionLinear, ExecutionStaged} {
		t.Run(string(model), func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnation(t, "redis-prod")
			seedConnectedSoul(t, host, []string{"redis-prod"})
			gitURL := linearServiceRepo(t, "rescues", `name: rescues
description: one group holding two rescue units for sources outside it
settings:
  serial_scope: block
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: migrate
    module: core.exec.run
    register: migrate
    params: { cmd: migrate }
  - name: rescues
    serial: 1
    block:
      - name: undo deploy
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: rollback }
      - name: undo migrate
        module: core.exec.run
        onfail: [migrate]
        params: { cmd: migrate-down }
`)
			disp := &linearStepDispatcher{t: t, aBySID: map[string]string{}, sent: map[int]map[string][]string{}}
			r := newRunner(t, disp, gitURL)
			r.deps.ExecutionModel = model

			applyID := audit.NewULID()
			if err := r.Start(context.Background(), RunSpec{
				ApplyID:         applyID,
				IncarnationName: "redis-prod",
				ServiceRef:      artifact.ServiceRef{Name: "redis", Git: gitURL, Ref: "master"},
				ScenarioName:    "rescues",
				StartedByAID:    "archon-alice",
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}

			if model == ExecutionStaged {
				waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
				return
			}
			inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
			if inc.StatusDetails["reason"] != config.CodeGroupMultipleRescueUnits {
				t.Errorf("linear reason = %v, want %s", inc.StatusDetails["reason"], config.CodeGroupMultipleRescueUnits)
			}
			if got := disp.messages(); len(got) != 0 {
				t.Errorf("linear run sent %v, want nothing", got)
			}
		})
	}
}

const unitWithNothingToRescue = `name: roll
description: the example of the ADR-0091 amendment of NIM-993, with nothing failing
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: /opt/app/deploy.sh }
  - name: reload the balancer
    module: core.exec.run
    params: { cmd: /opt/app/reload-lb.sh }
  - name: roll back
    onfail: [deploy]
    block:
      - name: restore the previous release
        module: core.exec.run
        params: { cmd: /opt/app/rollback.sh }
      - name: check the service
        module: core.exec.run
        register: health
        failed_when: "false"
        params: { cmd: /opt/app/healthcheck.sh }
      - name: not handled while the service is down
        module: core.task.failed
        when: register.health.exit_code != 0
        params: {}
  - name: warm the cache
    module: core.exec.run
    params: { cmd: /opt/app/warm.sh }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). The amendment's example with nothing
// failing: deploy succeeds on all three hosts, so `roll back` has nothing to rescue.
// Its children are recorded SKIPPED on every host and sent to none, and the `when:`
// of `core.task.failed`, which reads the register of a check that never ran, is never
// evaluated: every host goes on to `warm the cache` and the run ends ready.
//
// Mutation: route a block's unit child as any other (crossStepUnit read false in
// walkBlockChildren) → the Keeper evaluates that `when:` against the skipped
// check's register, fails every host at step 4, and the run ends error_locked.
func TestIntegration_LinearUnitWithNothingToRescueIsSkippedEverywhere(t *testing.T) {
	hosts := []string{policyHostA, policyHostB, policyHostC}
	seedPolicyRoster(t, "redis-prod", hosts...)
	souls := newRescueSouls(t, nil, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", unitWithNothingToRescue), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	want := map[int]map[string][]string{0: {}, 1: {}, 5: {}}
	for _, sid := range hosts {
		want[0][sid] = []string{"deploy"}
		want[1][sid] = []string{"reload the balancer"}
		want[5][sid] = []string{"warm the cache"}
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	names := map[int]string{
		2: "restore the previous release", 3: "check the service", 4: "not handled while the service is down",
	}
	for _, sid := range hosts {
		got := taskRecords(t, applyID, sid, names)
		for _, name := range names {
			if got[name] != keeperv1.TaskStatus_TASK_STATUS_SKIPPED.String() {
				t.Errorf("%s's record of %q = %q, want SKIPPED", sid, name, got[name])
			}
		}
	}
}

const staticFalseUnits = `name: roll
description: static-false tasks and units of an earlier step's source, with nothing failing
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: off
    module: core.exec.run
    register: off
    when: "incarnation.id == 'nope'"
    params: { cmd: off }
  - name: off apply
    register: off_apply
    when: "incarnation.id == 'nope'"
    apply:
      destiny: d
  - name: page
    module: core.exec.run
    onfail: [deploy]
    register: page
    when: "incarnation.id == 'nope'"
    params: { cmd: page }
  - name: undo
    onfail: [deploy]
    register: undo
    when: "incarnation.id == 'nope'"
    apply:
      destiny: d
  - name: warm the cache
    module: core.exec.run
    params: { cmd: warm }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). What a static-false task leaves on a
// live host, where nothing failed: a module task and an applier that are no unit leave
// nothing — no record, no register — and so does an applier unit; a module unit leaves
// the SKIPPED record with the skipped register, as every unit task settled on a live
// host does.
//
// Mutation: give a skipped applier unit its skip targets in unitStaticWhen → undo is
// recorded SKIPPED on every host, its register reading skipped.
func TestIntegration_LinearStaticFalseUnitsOnALiveHost(t *testing.T) {
	hosts := []string{policyHostA, policyHostB}
	seedPolicyRoster(t, "redis-prod", hosts...)
	souls := newRescueSouls(t, nil, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", staticFalseUnits), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	names := map[int]string{1: "off", 2: "off apply", 3: "page", 4: "undo"}
	regs, err := applyrun.SelectTaskRegistersByApplyIDUpToPassage(context.Background(), integrationPool, applyID, 99)
	if err != nil {
		t.Fatalf("registers: %v", err)
	}
	for _, sid := range hosts {
		got := taskRecords(t, applyID, sid, names)
		delete(got, "")
		if want := map[string]string{"page": keeperv1.TaskStatus_TASK_STATUS_SKIPPED.String()}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s's records of the static-false tasks = %v, want %v", sid, got, want)
		}
		left := map[string]any{}
		for _, reg := range regs {
			if name, ok := names[reg.PlanIndex]; ok && reg.SID == sid {
				left[name] = reg.RegisterData["skipped"]
			}
		}
		if want := map[string]any{"page": true}; !reflect.DeepEqual(left, want) {
			t.Errorf("%s's registers of the static-false tasks, by skipped = %v, want %v", sid, left, want)
		}
	}
}
