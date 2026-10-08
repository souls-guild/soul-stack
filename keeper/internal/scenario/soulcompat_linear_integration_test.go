//go:build integration

package scenario

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The Soul-side capability gates of a linear run (draft ADR NIM-907 items 8, 10;
// NIM-946): the whole plan is checked before the first step, and each step again on
// what each host is sent, once the Keeper settled it.

// linearCapRun starts body as a linear run on the two flow hosts, the Souls lacking
// what lacking says, and waits for want.
func linearCapRun(t *testing.T, name, body string, register func(sid, task string) map[string]any, lacking map[string][]string, want incarnation.Status) (*flowDispatcher, *recordingSoulCap, *incarnation.Incarnation) {
	t.Helper()
	seedFlowHosts(t)
	gitURL := linearServiceRepo(t, name, body)
	disp := newFlowDispatcher(t, register)
	r := newFlowRunner(t, disp, nil)
	cap := &recordingSoulCap{inner: stubSoulCap{lackingByCap: lacking}}
	r.soulCap = cap
	applyID := startFlowRun(t, r, gitURL, name, nil)
	inc := waitRunDone(t, "redis-prod", applyID, want)
	disp.wg.Wait()
	return disp, cap, inc
}

func probeRegister(sid, task string) map[string]any {
	return map[string]any{"stdout": "out-" + sid, "changed": false}
}

func requireRefusal(t *testing.T, inc *incarnation.Incarnation, wants ...string) {
	t.Helper()
	if inc.StatusDetails["reason"] != reasonSoulCapabilityUnsupported {
		t.Fatalf("reason = %v, want %s", inc.StatusDetails["reason"], reasonSoulCapabilityUnsupported)
	}
	errText, _ := inc.StatusDetails["error"].(string)
	for _, want := range wants {
		if !strings.Contains(errText, want) {
			t.Errorf("status_details.error = %q, does not mention %q", errText, want)
		}
	}
}

// ★ GUARD (NIM-946 acceptance). A later step's failed_when reads another register, so
// the Keeper substitutes its value in; host-b's Soul does not announce it reads a
// rendered predicate. The run is refused before its first step — not one
// ApplyRequest, not even the probe — and the refusal names the host, the bit and the
// step that needs it.
//
// Mutation: gate only per step (drop the up-front gate in run.go) → the probe goes
// out on both hosts and the refusal comes at step 1 → red.
func TestIntegration_LinearRenderedPredicateRefusedBeforeTheFirstStep(t *testing.T) {
	disp, _, inc := linearCapRun(t, "rendered", `name: rendered
description: a later step's predicate the Keeper renders
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: check
    module: core.exec.run
    failed_when: register.self.stdout != register.probe.stdout
    params: { cmd: check }
`, probeRegister, map[string][]string{config.CapabilityRenderedPredicate: {flowHostB}}, incarnation.StatusErrorLocked)

	requireRefusal(t, inc, flowHostB, config.CapabilityRenderedPredicate, `step 1 "check"`)
	if errText, _ := inc.StatusDetails["error"].(string); strings.Contains(errText, flowHostA) {
		t.Errorf("status_details.error = %q names host-a, which announced the bit", errText)
	}
	if sent := disp.sent(); len(sent) != 0 {
		t.Fatalf("sent %v, want nothing: the refusal must come before the first step", sent)
	}
}

// ★ GUARD (NIM-946 acceptance, the other half). The same Souls run a plan whose
// predicate reads only register.self: nothing is substituted, the text travels as
// written, and the bit is never asked about.
//
// Mutation: require the bit for any predicate → this run is refused → red.
func TestIntegration_LinearPredicateWithoutSubstitutionNeedsNoBit(t *testing.T) {
	disp, cap, _ := linearCapRun(t, "plain", `name: plain
description: a later step's predicate that reads only its own result
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: check
    module: core.exec.run
    failed_when: register.self.rc != 0
    params: { cmd: check }
`, probeRegister, map[string][]string{config.CapabilityRenderedPredicate: {flowHostB}}, incarnation.StatusReady)

	want := map[int]map[string][]string{
		0: {flowHostA: {"probe"}, flowHostB: {"probe"}},
		1: {flowHostA: {"check"}, flowHostB: {"check"}},
	}
	if got := disp.sent(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v", got, want)
	}
	if asked := cap.askedFor(config.CapabilityRenderedPredicate); len(asked) != 0 {
		t.Errorf("asked about %s for %v, want never", config.CapabilityRenderedPredicate, asked)
	}
	if asked := cap.askedFor(config.CapabilityFlowControl); len(asked) == 0 {
		t.Errorf("never asked about %s, which the failed_when travels on", config.CapabilityFlowControl)
	}
}

// grouped: restart's source changed on host-a only, so the requisite gate drops
// restart on host-b, where the group then carries it as `when: false` — a predicate
// host-b's Soul must evaluate. Host-a is sent restart with no predicate at all. after's
// `when:` is decided on the Keeper and travels blank.
const groupedRequisiteScenario = `name: grouped
description: a group child the requisite gate drops on one host
tasks:
  - name: conf
    module: core.exec.run
    register: conf
    params: { cmd: conf }
  - name: canary
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: restart
        module: core.exec.run
        register: restart
        onchanges: [conf]
        params: { cmd: restart }
      - name: verify
        module: core.exec.run
        params: { cmd: verify }
  - name: after
    module: core.exec.run
    when: register.conf.changed
    params: { cmd: after }
`

func confChangedOnA(sid, task string) map[string]any {
	return map[string]any{"changed": task == "conf" && sid == flowHostA}
}

// ★ GUARD (draft ADR item 10, "that gate must read the per-host value, and so run
// after the requisite gate"). Host-b is sent `when: false` for restart and does not
// announce flow_control: the step is refused there — at the step, since the drop
// depends on step 0's result — and only after step 0 ran.
//
// Mutation: read the shared `When` (empty for restart) as the in-loop gate did → no
// host is asked for flow_control and the run completes → red.
func TestIntegration_LinearGroupChildDroppedOnOneHostNeedsFlowControlThere(t *testing.T) {
	disp, _, inc := linearCapRun(t, "grouped", groupedRequisiteScenario, confChangedOnA,
		map[string][]string{config.CapabilityFlowControl: {flowHostB}}, incarnation.StatusErrorLocked)

	requireRefusal(t, inc, flowHostB, config.CapabilityFlowControl, `step 1 "restart"`, "refused at step 1")
	want := map[int]map[string][]string{0: {flowHostA: {"conf"}, flowHostB: {"conf"}}}
	if got := disp.sent(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v (step 0 only)", got, want)
	}
}

// ★ GUARD, the other half: host-a is sent restart with no predicate and after with its
// `when:` decided, so a Soul there without flow_control runs every step.
//
// Mutations: require flow_control of every host a grouped child targets → host-a is
// refused at step 1; gate a linear run's steps on the author's predicates, as the
// staged gate does → after's `when:` refuses host-a at step 2 → red.
func TestIntegration_LinearGroupChildSentWithoutPredicateNeedsNoFlowControl(t *testing.T) {
	disp, _, _ := linearCapRun(t, "grouped", groupedRequisiteScenario, confChangedOnA,
		map[string][]string{config.CapabilityFlowControl: {flowHostA}}, incarnation.StatusReady)

	want := map[int]map[string][]string{
		0: {flowHostA: {"conf"}, flowHostB: {"conf"}},
		1: {flowHostA: {"restart", "verify"}, flowHostB: {"restart", "verify"}},
		2: {flowHostA: {"after"}},
	}
	if got := disp.sent(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v", got, want)
	}
	if when := disp.task(1, flowHostB, "restart").GetWhen(); when != "false" {
		t.Errorf("host-b's restart when = %q, want \"false\"", when)
	}
	if when := disp.task(1, flowHostA, "restart").GetWhen(); when != "" {
		t.Errorf("host-a's restart when = %q, want none", when)
	}
}

// ★ GUARD (draft ADR item 10 with NIM-948's failure policy). A capability refusal is
// the run's, whatever its radius: under `on_failure: host` and `tolerate: 1` host-b's
// late refusal at act's step ends the run — no host is dropped for it, host-a is not
// sent the next step, and the run reads failed, not partial_failed.
//
// Mutation: judge the step's refusal as host-b's own failure under the host radius →
// host-b is dropped, host-a runs `after`, and the run ends partial_failed → red.
func TestIntegration_LinearCapabilityRefusalEndsTheRunUnderHostRadius(t *testing.T) {
	disp, _, inc := linearCapRun(t, "hostradius", `name: hostradius
description: a late capability refusal under on_failure host
settings:
  on_failure: host
  tolerate: 1
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: act
    module: core.exec.run
    where: register.probe.stdout == 'out-host-b.example.com'
    failed_when: register.self.stdout != register.probe.stdout
    params: { cmd: act }
  - name: after
    module: core.exec.run
    params: { cmd: after }
`, probeRegister, map[string][]string{config.CapabilityRenderedPredicate: {flowHostB}}, incarnation.StatusErrorLocked)

	requireRefusal(t, inc, flowHostB, config.CapabilityRenderedPredicate, `step 1 "act"`, "refused at step 1")
	want := map[int]map[string][]string{0: {flowHostA: {"probe"}, flowHostB: {"probe"}}}
	if got := disp.sent(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v (the probe only)", got, want)
	}
	applyID, _ := inc.StatusDetails["apply_id"].(string)
	detail, err := applyrun.SelectRunDetail(context.Background(), integrationPool, applyID, "redis-prod")
	if err != nil {
		t.Fatalf("SelectRunDetail: %v", err)
	}
	if detail.Status != applyrun.RunStatusFailed {
		t.Errorf("run status = %q, want failed", detail.Status)
	}
	if drops := auditPayloads(t, applyID, audit.EventApplyHostDropped); len(drops) != 0 {
		t.Errorf("apply.host_dropped = %v, want none: a capability refusal drops no host", drops)
	}
}

// ★ GUARD (with NIM-961's work-queue). On an instance with Acolytes a step goes
// through the queue, and its capability gate still comes before its rows: act's hosts
// depend on the probe, so only act's step can ask host-b, and the refusal leaves no
// step-1 row for an Acolyte to claim.
//
// Mutation: skip the step's gate where the queue may take the step → host-b's step-1
// row is queued, claimed and sent the rendered predicate → red.
func TestIntegration_LinearQueuedStepIsGatedBeforeItsRows(t *testing.T) {
	seedTwoHosts(t, flowHostA, flowHostB)
	gitURL := linearServiceRepo(t, "queued", `name: queued
description: a queued step whose hosts depend on the probe
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: act
    module: core.exec.run
    where: register.probe.stdout == 'b'
    failed_when: register.self.stdout != register.probe.stdout
    params: { cmd: act }
`)
	souls := newQueueSouls(t, "probe", map[string]string{flowHostA: "a", flowHostB: "b"})
	r := newLinearQueueRunner(t, souls)
	r.soulCap = stubSoulCap{lackingByCap: map[string][]string{config.CapabilityRenderedPredicate: {flowHostB}}}
	runAcolyte(t, souls, "keeper-2")
	applyID := startLinearRun(t, r, gitURL, "queued")
	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)

	requireRefusal(t, inc, flowHostB, config.CapabilityRenderedPredicate, `step 1 "act"`, "refused at step 1")
	// The run's own row, which the abort closes, is not a host's.
	rows := map[int]map[string]stepRowView{}
	for p, bySID := range stepRows(t, applyID) {
		for sid, row := range bySID {
			if sid == render.RunSentinelSID {
				continue
			}
			if rows[p] == nil {
				rows[p] = map[string]stepRowView{}
			}
			rows[p][sid] = row
		}
	}
	if len(rows[0]) != 2 {
		t.Errorf("step 0 rows = %v, want one per host", rows[0])
	}
	for sid, row := range rows[0] {
		if row.kid != "keeper-2" {
			t.Errorf("step 0 of %s claimed by %q, want keeper-2 - the step did not go through the queue", sid, row.kid)
		}
	}
	if len(rows[1]) != 0 {
		t.Errorf("step 1 rows = %v, want none: the gate comes before the step's rows", rows[1])
	}
	if got := souls.messages()[1]; len(got) != 0 {
		t.Errorf("step 1 sent %v, want nothing", got)
	}
}

// ★ GUARD (draft ADR item 10, a late refusal stated). act's `where:` reads the probe,
// so its hosts are unknown before the run: the up-front gate cannot ask about them,
// and the step's gate refuses host-b at act's step, after the probe ran.
//
// Mutation: drop the step's gate in dispatchPassage → act is sent to host-b with a
// rendered predicate its Soul cannot read → the run completes → red.
func TestIntegration_LinearRegisterTargetedStepIsGatedAtItsStep(t *testing.T) {
	disp, _, inc := linearCapRun(t, "targeted", `name: targeted
description: a step whose hosts depend on the probe
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: act
    module: core.exec.run
    where: register.probe.stdout == 'out-host-b.example.com'
    failed_when: register.self.stdout != register.probe.stdout
    params: { cmd: act }
`, probeRegister, map[string][]string{config.CapabilityRenderedPredicate: {flowHostB}}, incarnation.StatusErrorLocked)

	requireRefusal(t, inc, flowHostB, config.CapabilityRenderedPredicate, `step 1 "act"`, "refused at step 1")
	want := map[int]map[string][]string{0: {flowHostA: {"probe"}, flowHostB: {"probe"}}}
	if got := disp.sent(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v (the probe only)", got, want)
	}
}
