//go:build integration

package scenario

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/voyageorch"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// A failure held for a rescue unit ahead (ADR-0091 amendment NIM-993, rules 1-4): the
// host waits out the steps between, a unit that succeeds handles the failure and
// returns the host, and a failure no unit handles is judged at its last unit's
// barrier by its own radius.

var (
	skippedRecord = keeperv1.TaskStatus_TASK_STATUS_SKIPPED.String()
	changedRecord = keeperv1.TaskStatus_TASK_STATUS_CHANGED.String()
)

// errorOf is the run error the incarnation was locked with.
func errorOf(inc *incarnation.Incarnation) string {
	s, _ := inc.StatusDetails["error"].(string)
	return s
}

// everyHost is name sent to each of sids.
func everyHost(name string, sids ...string) map[string][]string {
	out := map[string][]string{}
	for _, sid := range sids {
		out[sid] = []string{name}
	}
	return out
}

// assertHandledRows checks that the rows of run applyID carrying the
// `apply_runs.handled` mark are want, (sid, step) each, and that the same rows and no
// other read handled on the wire ([applyrun.RunHostStatus.Handled]).
func assertHandledRows(t *testing.T, applyID string, want ...[2]any) {
	t.Helper()
	marked := map[[2]any]bool{}
	for k, row := range rowsOf(t, applyID) {
		if row.HandledMark {
			marked[k] = true
		}
	}
	detail, err := applyrun.SelectRunDetail(context.Background(), integrationPool, applyID, "redis-prod")
	if err != nil {
		t.Fatalf("SelectRunDetail: %v", err)
	}
	wire := map[[2]any]bool{}
	for _, h := range detail.Hosts {
		if h.Handled {
			wire[[2]any{h.SID, h.Passage}] = true
		}
	}
	wantSet := map[[2]any]bool{}
	for _, k := range want {
		wantSet[k] = true
	}
	if !reflect.DeepEqual(marked, wantSet) {
		t.Errorf("rows marked handled = %v, want %v", marked, wantSet)
	}
	if !reflect.DeepEqual(wire, wantSet) {
		t.Errorf("rows handled on the wire = %v, want %v", wire, wantSet)
	}
}

// destinyRepos lays out one git repository per destiny under one directory and
// returns the template a [DestinySource] finds them by.
func destinyRepos(t *testing.T, destinies map[string]map[string]string) string {
	t.Helper()
	base := t.TempDir()
	for name, files := range destinies {
		dir := filepath.Join(base, name)
		repo, err := git.PlainInit(dir, false)
		if err != nil {
			t.Fatalf("PlainInit: %v", err)
		}
		for rel, content := range files {
			full := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
		}
		commitRepo(t, repo)
	}
	return "file://" + base + "/{name}"
}

// destinyServiceRepo is a service applying the destinies of destinies at master, with
// one scenario.
func destinyServiceRepo(t *testing.T, scenario, body string, destinies ...string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	service := "description: rescue units\nstate_schema: {}\ndestiny:\n"
	for _, d := range destinies {
		service += "  - { name: " + d + ", ref: master }\n"
	}
	for rel, content := range map[string]string{"service.yml": service, "scenario/" + scenario + "/main.yml": body} {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	commitRepo(t, repo)
	return "file://" + dir
}

const adrExample = `name: roll
description: the example of the ADR-0091 amendment of NIM-993, with a step reading what a held host missed
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: /opt/app/deploy.sh }
  - name: reload the balancer
    module: core.exec.run
    register: reload
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
  - name: note the missed reload
    module: core.exec.run
    when: register.reload.skipped
    params: { cmd: note }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rules 2-3: the amendment's example). `deploy`
// fails on host-b, which is held: `reload the balancer` goes to host-a and host-c and
// is recorded SKIPPED for host-b, `roll back` goes to host-b alone, the health check
// passes, `core.task.failed` is skipped by its `when:`, and host-b is back for `warm
// the cache` on all three hosts. A later step reading `register.reload.skipped` reads
// the register synthesized for what host-b missed. The run ends ready, and reads
// success on every read path, its Voyage target succeeded: host-b's failed row of
// `deploy` is marked handled (rule 5), and reads handled on the wire.
//
// Mutations: never hold a failure (decideHost judging every one at once) → the run
// ends error_locked at step 0; return no host (returnHost a no-op) → `warm the cache`
// misses host-b; synthesize no register for a missed task (recordMissed a no-op) →
// the `when:` of the last step fails on host-b, with no `reload` to read; mark no row
// in the commit (commitSuccess without its marks) → the run reads failed on every path
// and its Voyage target fails.
func TestIntegration_LinearADRExampleIsHandled(t *testing.T) {
	hosts := []string{policyHostA, policyHostB, policyHostC}
	seedPolicyRoster(t, "redis-prod", hosts...)
	souls := newRescueSouls(t, map[string][]string{policyHostB: {"deploy"}}, nil)
	souls.extra = map[string]map[string]map[string]any{policyHostB: {"check the service": {"exit_code": 0}}}
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", adrExample), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	want := map[int]map[string][]string{
		0: everyHost("deploy", hosts...),
		1: {policyHostA: {"reload the balancer"}, policyHostC: {"reload the balancer"}},
		2: {policyHostB: {"restore the previous release"}},
		3: {policyHostB: {"check the service"}},
		5: everyHost("warm the cache", hosts...),
		6: {policyHostB: {"note the missed reload"}},
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	names := map[int]string{1: "reload", 2: "restore", 3: "check", 4: "not handled"}
	got := taskRecords(t, applyID, policyHostB, names)
	for name, want := range map[string]string{"reload": skippedRecord, "restore": changedRecord, "check": changedRecord, "not handled": skippedRecord} {
		if got[name] != want {
			t.Errorf("host-b's record of %s = %q, want %q", name, got[name], want)
		}
	}
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusSuccess, true)
	status, err := applyrun.SelectRunStatus(context.Background(), integrationPool, applyID)
	if outcome, done := voyageorch.OutcomeOfRun(status); err != nil || !done || outcome != voyageorch.OutcomeSucceeded {
		t.Errorf("Voyage target outcome = %q (done %v, %v), want %s", outcome, done, err, voyageorch.OutcomeSucceeded)
	}
	if row := rowsOf(t, applyID)[[2]any{policyHostB, 0}]; row.Status != applyrun.StatusFailed {
		t.Errorf("host-b's row of deploy = %s, want failed: a handled failure is not undone", row.Status)
	}
	assertHandledRows(t, applyID, [2]any{policyHostB, 0})
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 4: the amendment's example, the health
// check failing). `core.task.failed` fails the unit on host-b, no unit of `deploy` is
// left, and `deploy` is judged at the unit's barrier by its own radius, the default
// `scenario`: the run ends there with `deploy`'s error and the unit's, and `warm the
// cache` reaches no host.
//
// Mutations: judge no held failure at its last unit's barrier (sweepHost a no-op) →
// the run walks on and ends ready; attach no failed unit to its source
// (attachUnitFailures a no-op) → the run's error lacks the unit's.
func TestIntegration_LinearADRExampleIsNotHandled(t *testing.T) {
	hosts := []string{policyHostA, policyHostB, policyHostC}
	seedPolicyRoster(t, "redis-prod", hosts...)
	souls := newRescueSouls(t, map[string][]string{policyHostB: {"deploy", "not handled while the service is down"}}, nil)
	souls.extra = map[string]map[string]map[string]any{policyHostB: {"check the service": {"exit_code": 1}}}
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", adrExample), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	for _, want := range []string{"step 4 failed", `at task "deploy" of step 0`, "deploy failed on " + policyHostB, "not handled while the service is down failed on " + policyHostB} {
		if !strings.Contains(errorOf(inc), want) {
			t.Errorf("run error lacks %q: %q", want, errorOf(inc))
		}
	}
	sent := souls.sends()
	if got, want := sent[4], map[string][]string{policyHostB: {"not handled while the service is down"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 4 sent = %v, want %v", got, want)
	}
	if got := sent[5]; len(got) != 0 {
		t.Errorf("warm the cache sent to %v after the run ended", got)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 3 and 5). Units are tried in plan order:
// `first` fails on host-a, the next unit gets its chance, `second` handles the
// failure, and host-a is back for `warm`. Both of host-a's failed rows are marked
// handled — the source's, and that of the unit which failed before one succeeded — so
// the run reads success.
//
// Mutations: judge a held failure at its first unit's barrier (lastUnit keeping the
// earliest DecidedAt) → the run ends at step 1 under `scenario`; mark only each
// source's own row (handledRows over s.rows[:1]) → host-a's row of `first` stays a
// failure and the run reads failed.
func TestIntegration_LinearTheFirstUnitThatSucceedsHandles(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy", "first"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: two rescue units of one source
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: first
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: first }
  - name: second
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: second }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	want := map[int]map[string][]string{
		0: everyHost("deploy", policyHostA, policyHostB),
		1: {policyHostA: {"first"}},
		2: {policyHostA: {"second"}},
		3: everyHost("warm", policyHostA, policyHostB),
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	assertHandledRows(t, applyID, [2]any{policyHostA, 0}, [2]any{policyHostA, 1})
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusSuccess, true)
}

const twoFailuresOneRescue = `name: roll
description: a group of two sources, one of which has a rescue
settings:
  on_failure: host
tasks:
  - name: roll
    serial: 3
    settings: { serial_scope: block }
    block:
      - name: deploy
        module: core.exec.run
        register: deploy
        params: { cmd: deploy }
      - name: migrate
        module: core.exec.run
        register: migrate
        params: { cmd: migrate }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
  - name: verify
    module: core.exec.run
    params: { cmd: verify }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 2, "What the run goroutine keeps"). host-a
// fails `deploy` and `migrate` at one step; only `deploy` has a rescue ahead, so host-a
// is judged at once, by both, and dropped under `host`. Its rescue still reaches it, as
// compensation: one that succeeds returns nothing and handles nothing, so no row is
// marked handled, and one that fails is a failure of its unit, not of the host — no
// entry of its own, its row marked dropped beside the source's. The run reads
// partial_failed on every read path.
//
// Mutations: hold a host when its first failure has a unit ahead (decideHost reading
// open[0] alone) → host-a is held and the run ends ready; judge a failed unit's row as
// a failure of its own (an entry for a unit row in decideHost) → a second entry for
// host-a; attach no failed unit to its source (attachUnitFailures a no-op) → the
// rescue's row is not marked dropped.
func TestIntegration_LinearMixedFailuresAreJudgedAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rescueFails bool
	}{{name: "the rescue succeeds"}, {name: "the rescue fails", rescueFails: true}} {
		t.Run(tc.name, func(t *testing.T) {
			hosts := []string{policyHostA, policyHostB, policyHostC}
			seedPolicyRoster(t, "redis-prod", hosts...)
			fail := []string{"deploy", "migrate"}
			if tc.rescueFails {
				fail = append(fail, "rollback")
			}
			souls := newRescueSouls(t, map[string][]string{policyHostA: fail}, nil)
			souls.noStop = map[string][]string{policyHostA: {"deploy"}}
			applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", twoFailuresOneRescue), "roll")

			inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
			if inc.StatusDetails["reason"] != reasonPartialFailed {
				t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
			}
			sent := souls.sends()
			if got, want := sent[1], map[string][]string{policyHostA: {"rollback"}}; !reflect.DeepEqual(got, want) {
				t.Errorf("step 1 sent = %v, want %v: the rescue reaches the judged host as compensation", got, want)
			}
			if got, want := sent[2], everyHost("verify", policyHostB, policyHostC); !reflect.DeepEqual(got, want) {
				t.Errorf("step 2 sent = %v, want %v: a judged host does not return", got, want)
			}
			if n := strings.Count(errorOf(inc), "host "+policyHostA); n != 1 {
				t.Errorf("run error names host-a %d times, want once: %q", n, errorOf(inc))
			}
			var steps []float64
			for _, p := range auditPayloads(t, applyID, audit.EventApplyHostDropped) {
				steps = append(steps, p["step"].(float64))
			}
			if !reflect.DeepEqual(steps, []float64{0}) {
				t.Errorf("apply.host_dropped steps = %v, want [0]", steps)
			}
			rows := rowsOf(t, applyID)
			if row := rows[[2]any{policyHostA, 1}]; tc.rescueFails && (row.Status != applyrun.StatusFailed || !row.Dropped) {
				t.Errorf("host-a's rescue row = %s dropped=%v, want failed and dropped", row.Status, row.Dropped)
			}
			assertHandledRows(t, applyID)
			assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusPartialFailed, true)
		})
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 2, superseding orchestration.md §2.2.1's
// "no later wave starts" for a held failure). `deploy` rolls in `serial: 1`; host-a
// fails it in the first wave, its rescue is ahead, and the later waves go on.
//
// Mutation: stop the waves after one that holds a failure (break out of
// dispatchPassage's wave loop on fails.anyHeld()) → host-b and host-c are never sent
// `deploy`.
func TestIntegration_LinearAHeldFailureStopsNoWave(t *testing.T) {
	hosts := []string{policyHostA, policyHostB, policyHostC}
	seedPolicyRoster(t, "redis-prod", hosts...)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a canary whose first wave fails, with a rescue ahead
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    serial: 1
    params: { cmd: deploy }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	want := map[int]map[string][]string{0: everyHost("deploy", hosts...), 1: {policyHostA: {"rollback"}}, 2: everyHost("warm", hosts...)}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 2; rescues.md). A held host counts as
// still in the run for "no host is left": host-a is held for its rescue, host-b is
// dropped under `host` at the same barrier, and the run walks on — a `core.state.set`
// step between the source and its unit commits — to the rescue, which handles host-a's
// failure. The run ends partial_failed, the captured field kept, host-a's row marked
// handled beside host-b's dropped one, and reads partial_failed on every read path.
//
// Mutations: count a held host out of "no host is left" (settle subtracting the held
// hosts from the live ones) → the run ends at step 0 with no host left, the capture
// never made; write no mark in finishPartial → host-a's row is a failure and the run
// reads failed.
func TestIntegration_LinearAHeldHostIsStillInTheRun(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}, policyHostB: {"smoke"}}, nil)
	gitURL := writeServiceRepo(t, `name: create
description: a held host, a dropped one, a capture between the source and its rescue
settings:
  on_failure: host
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
  - name: record
    module: core.state.set
    params:
      field: recorded
      value: "between"
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
`)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, keeperRegistryWith(nil)), "redis-prod", "redis", gitURL, "create")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	if got := inc.State["recorded"]; got != "between" {
		t.Errorf("state.recorded = %v, want between: the capture between the source and its rescue", got)
	}
	if got, want := souls.sends()[2], map[string][]string{policyHostA: {"rollback"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v", got, want)
	}
	if row := rowsOf(t, applyID)[[2]any{policyHostB, 0}]; !row.Dropped {
		t.Errorf("host-b's row = %s dropped=%v, want dropped", row.Status, row.Dropped)
	}
	assertHandledRows(t, applyID, [2]any{policyHostA, 0})
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusPartialFailed, true)
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 2). A held host is listed in
// `soulprint.hosts` with `held: true` and `dropped: false`: a step between the source and
// its rescue selects the live hosts by it.
//
// Mutation: list a held host `held: false` (hostFactsToMap's held argument dropped) →
// `report` is sent to no host.
func TestIntegration_LinearAHeldHostIsListedHeld(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a step reading which host is held
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: report
    module: core.exec.run
    where: "soulprint.hosts.exists(h, h.sid == 'host-a.example.com' && h.held && !h.dropped)"
    params: { cmd: report }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
`), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	if got, want := souls.sends()[1], map[string][]string{policyHostB: {"report"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 1 sent = %v, want %v", got, want)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, "What the run goroutine keeps"). A returned host
// stays out of the ordinary steps it missed on every re-render: once host-a is back, the
// pass of `warm` re-renders `report`, whose params read a field host-a's failed deploy
// never produced, with host-a held at report's step, and the run ends ready.
//
// Mutation: never set the Standing (run.go) → the pass of `warm` renders `report` for
// host-a and the run ends render_failed.
func TestIntegration_LinearAReturnedHostStaysOutOfWhatItMissed(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a step a held host misses, read again after it returns
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
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	if got, want := souls.sends()[3], everyHost("warm", policyHostA, policyHostB); !reflect.DeepEqual(got, want) {
		t.Errorf("step 3 sent = %v, want %v", got, want)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 3; fork 3). The default block `phase`
// chose its hosts at its first step, while host-a was held; its rescue inside the block
// handles the failure, and host-a rejoins at the first step after the block, not the
// block's tail: `p2` is recorded SKIPPED for it, `warm` reaches it, and a later step
// reads the register synthesized for the `p2` it missed, though its return was decided
// before `p2`'s step.
//
// Mutations: rejoin at the step after the unit (rejoin returning from) → host-a is live
// at `p2`'s step, not among the block's hosts, and gets no record of it; synthesize a
// missed register only while no return is decided (recordMissed over openPeriod) →
// `register.p2` is missing on host-a, and the `when:` of `note` fails it.
func TestIntegration_LinearAHostReturnsAfterABlockChosenWhileHeld(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a rescue inside a default block chosen while its host was held
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: phase
    block:
      - name: p1
        module: core.exec.run
        params: { cmd: p1 }
      - name: rollback
        module: core.exec.run
        onfail: [deploy]
        params: { cmd: rollback }
      - name: p2
        module: core.exec.run
        register: p2
        params: { cmd: p2 }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
  - name: note
    module: core.exec.run
    when: register.p2.skipped
    params: { cmd: note }
`), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	want := map[int]map[string][]string{
		0: everyHost("deploy", policyHostA, policyHostB),
		1: {policyHostB: {"p1"}},
		2: {policyHostA: {"rollback"}},
		3: {policyHostB: {"p2"}},
		4: everyHost("warm", policyHostA, policyHostB),
		5: {policyHostA: {"note"}},
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	if got := taskRecords(t, applyID, policyHostA, map[int]string{3: "p2"})["p2"]; got != skippedRecord {
		t.Errorf("host-a's record of p2 = %q, want %s", got, skippedRecord)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 6, fail-closed). The destiny `pair` holds a
// source and its own rescue. Its message carries no `rescue_unit` mark, so the Soul runs
// it under fail-stop with rescue: `src` fails on host-a, `fix` runs and succeeds, and
// the row is failed. The replay reads `src` handled; the row says otherwise, so `src`
// is not handled, and host-a is judged by it — dropped under `host`.
//
// Mutation: drop the fail-closed reopen (judgeRow) → `src` reads handled, host-a stays
// in the run, `warm` reaches it and the run ends ready.
func TestIntegration_LinearAFailedRowOfAHandledPairIsNotHandled(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"src"}}, nil)
	r := newPolicyRunner(t, souls, nil)
	r.deps.Destiny = NewDestinySource(artifact.NewDestinyLoader(t.TempDir(), nil), fixedTemplateSource(destinyRepos(t, map[string]map[string]string{
		"pair": {
			"destiny.yml": "name: pair\ndescription: a source and its own rescue\n",
			"tasks/main.yml": `- name: src
  module: core.exec.run
  register: src
  params: { cmd: src }
- name: fix
  module: core.exec.run
  onfail: [src]
  params: { cmd: fix }
`,
		},
	})))
	applyID := startPolicyRun(t, r, "redis-prod", "redis", destinyServiceRepo(t, "roll", `name: roll
description: a destiny holding its own rescue, then an ordinary step
settings:
  on_failure: host
tasks:
  - name: apply the pair
    register: pair
    apply:
      destiny: pair
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`, "pair"), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	if got := taskRecords(t, applyID, policyHostA, map[int]string{1: "fix"})["fix"]; got != changedRecord {
		t.Errorf("host-a's record of fix = %q, want %s: the Soul ran the rescue", got, changedRecord)
	}
	if got, want := souls.sends()[1], map[string][]string{policyHostB: {"warm"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 1 sent = %v, want %v", got, want)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 2). The Keeper fails host-a before sending
// it the group `roll`: `check`'s `when:` cannot be evaluated there. Every source of the
// group has a rescue ahead, yet host-a is not held: it is judged at once by the first
// source among the records — the cause — dropped under `host`, and its rescue still
// reaches it as compensation.
//
// Mutation: hold a host the Keeper failed before sending a group (no keeperFailedGroup
// check in decideHost) → host-a is held, the rescue handles it, and the run ends ready.
func TestIntegration_LinearAGroupTheKeeperFailedIsNotHeld(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, nil, nil)
	souls.extra = map[string]map[string]map[string]any{policyHostB: {"prep": {"code": 0}}}
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a group the Keeper fails on one host, every source of it rescued ahead
settings:
  on_failure: host
tasks:
  - name: prep
    module: core.exec.run
    register: prep
    params: { cmd: prep }
  - name: roll
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: deploy
        module: core.exec.run
        register: deploy
        params: { cmd: deploy }
      - name: check
        module: core.exec.run
        register: check
        when: "register.prep.code == 0"
        params: { cmd: check }
  - name: rollback
    module: core.exec.run
    onfail: [deploy, check]
    params: { cmd: rollback }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	sent := souls.sends()
	if got, want := sent[2], map[string][]string{policyHostA: {"rollback"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v", got, want)
	}
	if got, want := sent[3], map[string][]string{policyHostB: {"warm"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 3 sent = %v, want %v", got, want)
	}
	for _, p := range auditPayloads(t, applyID, audit.EventApplyHostDropped) {
		if p["task"] != "check" || p["step"].(float64) != 1 {
			t.Errorf("apply.host_dropped = %v, want step 1 naming check", p)
		}
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 2). A one-task step the Keeper fails has no
// siblings and is held like any other failure: host-a's `deploy` fails on the Keeper,
// its rescue handles it, and host-a is back for `warm`. The row the Keeper failed is
// marked handled, and the run reads success.
//
// Mutations: judge every host the Keeper failed as a group (keeperFailedGroup true for
// any of them) → the run ends error_locked at step 1; record no failed row
// (judgeRow without its failedRows entry) → the row is not marked and the run reads
// failed.
func TestIntegration_LinearAOneTaskStepTheKeeperFailedIsHeld(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, nil, nil)
	souls.extra = map[string]map[string]map[string]any{policyHostB: {"prep": {"code": 0}}}
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a step the Keeper fails on one host
tasks:
  - name: prep
    module: core.exec.run
    register: prep
    params: { cmd: prep }
  - name: deploy
    module: core.exec.run
    register: deploy
    when: "register.prep.code == 0"
    params: { cmd: deploy }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	want := map[int]map[string][]string{
		0: everyHost("prep", policyHostA, policyHostB),
		1: {policyHostB: {"deploy"}},
		2: {policyHostA: {"rollback"}},
		3: everyHost("warm", policyHostA, policyHostB),
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	assertHandledRows(t, applyID, [2]any{policyHostA, 1})
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusSuccess, true)
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 2, decision on a `loop:`). A `loop:` step's
// iterations share one register and are one source: the Keeper fails both iterations on
// host-a, the step is held as any loop step is, the rescue naming the loop's register
// handles it, and host-a is back for `warm`. The row the Keeper failed is marked
// handled, and the run reads success.
//
// Mutations: judge a host the Keeper failed by several records as a group
// (keeperFailedGroup without the Grouped check) → the run ends error_locked at step 1;
// record no failed row (judgeRow without its failedRows entry) → the row is not marked
// and the run reads failed.
func TestIntegration_LinearALoopTheKeeperFailedIsOneHeldSource(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, nil, nil)
	souls.extra = map[string]map[string]map[string]any{policyHostB: {"prep": {"code": 0}}}
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a loop the Keeper fails on one host
tasks:
  - name: prep
    module: core.exec.run
    register: prep
    params: { cmd: prep }
  - name: each
    module: core.exec.run
    register: each
    when: "register.prep.code == 0"
    loop:
      items: ["x", "y"]
    params: { cmd: "${ item }" }
  - name: rollback
    module: core.exec.run
    onfail: [each]
    params: { cmd: rollback }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	want := map[int]map[string][]string{
		0: everyHost("prep", policyHostA, policyHostB),
		1: {policyHostB: {"each", "each"}},
		2: {policyHostA: {"rollback"}},
		3: everyHost("warm", policyHostA, policyHostB),
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	assertHandledRows(t, applyID, [2]any{policyHostA, 1})
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusSuccess, true)
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 4; rescues.md, settings.md §4). A failure
// tied to no task is held for a rescue of the task its records mark FAILED, and judged,
// when no rescue handles it, by the barrier owner's radius at the step it failed at —
// here `host`, not `stage`'s own `scenario`: host-a is dropped and the run goes on.
//
// Mutation: judge such a source by its task's own radius (newSources without the
// recorded case) → the run ends dispatch_failed at the rescue's barrier.
func TestIntegration_LinearALostStreamIsJudgedByTheOwnersRadius(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"unstage"}}, map[string]int{policyHostA: 0})
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a group whose host is never reached, its rescue failing
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
        settings: { on_failure: scenario }
        params: { cmd: stage }
      - name: swap
        module: core.exec.run
        params: { cmd: swap }
  - name: unstage
    module: core.exec.run
    onfail: [stage]
    params: { cmd: unstage }
  - name: after
    module: core.exec.run
    params: { cmd: after }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonPartialFailed {
		t.Errorf("reason = %v, want %s (details %v)", inc.StatusDetails["reason"], reasonPartialFailed, inc.StatusDetails)
	}
	sent := souls.sends()
	if got, want := sent[1], map[string][]string{policyHostA: {"unstage"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 1 sent = %v, want %v", got, want)
	}
	if got, want := sent[2], map[string][]string{policyHostB: {"after"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v", got, want)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1, `run_once:`). A `run_once:` rescue goes
// to the first host by SID it fires on and decides only that host's failure: host-a is
// handled, and host-b, which the rescue did not reach, is judged at the rescue's
// barrier — under `scenario`, the run ends there.
//
// Mutation: decide no unit on a host its step did not reach (barrier's last loop
// skipped) → host-b is never judged and the run ends ready.
func TestIntegration_LinearARunOnceRescueDecidesOneHost(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}, policyHostB: {"deploy"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a run_once rescue of a failure on two hosts
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    run_once: true
    params: { cmd: rollback }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if !strings.Contains(errorOf(inc), "host "+policyHostB) || strings.Contains(errorOf(inc), "host "+policyHostA) {
		t.Errorf("run error = %q, want host-b's failure alone", errorOf(inc))
	}
	sent := souls.sends()
	if got, want := sent[1], map[string][]string{policyHostA: {"rollback"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 1 sent = %v, want %v", got, want)
	}
	if got := sent[2]; len(got) != 0 {
		t.Errorf("warm sent to %v after the run ended", got)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 2 and 9). A failure ends the run while
// host-a is held: host-b's `report` has no rescue. The run still walks to host-a's
// rescue, which handles its failure, and that failure leaves no entry: the run's error
// names host-b's alone. The run's abort marks host-a's row handled (rule 5); the run
// reads failed for host-b's.
//
// Mutations: append at close every source not judged (close testing state !=
// sourceJudged) → the run's error names host-a's handled deploy; write no mark in an
// aborted run (no markHandled in abort) → host-a's row is not handled.
func TestIntegration_LinearARescueAfterTheEndHandlesWithNoEntry(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}, policyHostB: {"report"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a held host, a failure that ends the run, the rescue after it
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: report
    module: core.exec.run
    params: { cmd: report }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	if !strings.Contains(errorOf(inc), "report failed on "+policyHostB) || strings.Contains(errorOf(inc), "host "+policyHostA) {
		t.Errorf("run error = %q, want host-b's report alone", errorOf(inc))
	}
	if got, want := souls.sends()[2], map[string][]string{policyHostA: {"rollback"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v", got, want)
	}
	assertHandledRows(t, applyID, [2]any{policyHostA, 0})
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusFailed, true)
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 2 and 9). A Cancel stops the run at once
// while host-a is held, and no rescue runs: the failure still held is appended to the
// failure list when it closes, so the run's error names it beside the Cancel.
//
// Mutation: close the list without appending the held sources → the run's error does
// not name host-a's deploy.
func TestIntegration_LinearACancelNamesTheHeldFailure(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	souls.onSend = func(sid string, req *keeperv1.ApplyRequest) {
		if req.GetPassage() == 1 {
			if _, err := incarnation.RequestRunCancel(context.Background(), integrationPool, req.GetApplyId()); err != nil {
				t.Errorf("RequestRunCancel: %v", err)
			}
		}
	}
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a held host, a Cancel before its rescue
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: report
    module: core.exec.run
    params: { cmd: report }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	for _, want := range []string{"cancel", "held for a rescue that did not run", "deploy failed on " + policyHostA} {
		if !strings.Contains(errorOf(inc), want) {
			t.Errorf("run error lacks %q: %q", want, errorOf(inc))
		}
	}
	if got := souls.sends()[2]; len(got) != 0 {
		t.Errorf("rollback sent to %v after a Cancel", got)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 1 and 3, an applier's source). A child of
// the destiny applied as `certs` fails on host-a; `renew`, naming the applier, names
// its children and handles the failure, host-a is back, and the later rescue `page`
// of the same source is recorded SKIPPED on it and sent nowhere. host-a's row of the
// destiny is marked handled, and the run reads success.
//
// Mutations: expand no applier in RescueRule.Task (named as written) → `renew` handles
// nothing, host-a stays held, and `page` is sent to it; record no failed row (judgeRow
// without its failedRows entry) → the row is not marked and the run reads failed.
func TestIntegration_LinearAnApplierSourceIsHandledThroughItsChildren(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"Lay down the marker file"}}, nil)
	r := newPolicyRunner(t, souls, nil)
	r.deps.Destiny = NewDestinySource(artifact.NewDestinyLoader(t.TempDir(), nil), fixedTemplateSource(pilotFlatDestinyRepo(t)))
	applyID := startPolicyRun(t, r, "redis-prod", "redis", destinyServiceRepo(t, "roll", `name: roll
description: an applier whose child fails, two rescues naming the applier
tasks:
  - name: certs
    register: certs
    apply:
      destiny: pilot-flat
      input:
        marker_file: "/etc/marker"
        marker_payload: "ok"
  - name: renew
    module: core.exec.run
    onfail: [certs]
    params: { cmd: renew }
  - name: page
    module: core.exec.run
    onfail: [certs]
    params: { cmd: page }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`, "pilot-flat"), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	sent := souls.sends()
	if got, want := sent[1], map[string][]string{policyHostA: {"renew"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 1 sent = %v, want %v", got, want)
	}
	if got := sent[2]; len(got) != 0 {
		t.Errorf("page sent to %v, want nowhere: the failure is handled", got)
	}
	if got, want := sent[3], everyHost("warm", policyHostA, policyHostB); !reflect.DeepEqual(got, want) {
		t.Errorf("step 3 sent = %v, want %v", got, want)
	}
	if got := taskRecords(t, applyID, policyHostA, map[int]string{4: "page"})["page"]; got != skippedRecord {
		t.Errorf("host-a's record of page = %q, want %s", got, skippedRecord)
	}
	assertHandledRows(t, applyID, [2]any{policyHostA, 0})
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusSuccess, true)
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). A rescue block spans two steps; its
// first child fails on host-a, so the unit can no longer succeed and its second child
// is not sent there — recorded SKIPPED, as the Soul skips it inside one message — nor
// evaluated there: its params read an output `r1` never produced on host-a. `deploy`
// is judged at the block's last step under `scenario`, with the child's error.
//
// Mutation: route a unit's task whatever its unit did (no unitFailed case in
// routeUnit) → `r2`'s params are rendered for host-a and the run ends render_failed.
func TestIntegration_LinearAFailedUnitSendsNoLaterTask(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy", "r1"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a rescue block over two steps whose first child fails
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: roll back
    onfail: [deploy]
    block:
      - name: r1
        module: core.exec.run
        register: r1
        params: { cmd: r1 }
      - name: r2
        module: core.exec.run
        params: { cmd: "${ register.r1.stdout }" }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	for _, want := range []string{"step 2 failed", `at task "deploy" of step 0`, "r1 failed on " + policyHostA} {
		if !strings.Contains(errorOf(inc), want) {
			t.Errorf("run error lacks %q: %q", want, errorOf(inc))
		}
	}
	sent := souls.sends()
	if got, want := sent[1], map[string][]string{policyHostA: {"r1"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 1 sent = %v, want %v", got, want)
	}
	if got := sent[2]; len(got) != 0 {
		t.Errorf("step 2 sent %v, want nothing: the unit failed on host-a", got)
	}
	if got := taskRecords(t, applyID, policyHostA, map[int]string{2: "r2"})["r2"]; got != skippedRecord {
		t.Errorf("host-a's record of r2 = %q, want %s", got, skippedRecord)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). The rescue block also names `verify`, a
// later step's source; it has no source in its own message, so it is a cross-step unit
// all the same. Its first child fails on the held host-a, so its second is not sent
// there — recorded SKIPPED — nor evaluated there: its params read an output `r1` never
// produced on host-a. On the live host-b neither child is sent or evaluated. `deploy`
// is judged at the block's last step under `scenario`, with the child's error.
//
// Mutations: take a unit by its sources at earlier steps only (config.CrossStepUnits) →
// `r2`'s params are rendered for host-b, where `r1` was skipped, and the run ends
// render_failed; route a unit's task whatever its unit did (no unitFailed case in
// routeUnit) → they are rendered for host-a, the same.
func TestIntegration_LinearAFailedMixedSourceUnitSendsNoLaterTask(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy", "r1"}}, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a rescue block over two steps, naming a later source too, whose first child fails
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: roll back
    onfail: [deploy, verify]
    block:
      - name: r1
        module: core.exec.run
        register: r1
        params: { cmd: r1 }
      - name: r2
        module: core.exec.run
        params: { cmd: "${ register.r1.stdout }" }
  - name: verify
    module: core.exec.run
    register: verify
    params: { cmd: verify }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	for _, want := range []string{"step 2 failed", `at task "deploy" of step 0`, "r1 failed on " + policyHostA} {
		if !strings.Contains(errorOf(inc), want) {
			t.Errorf("run error lacks %q: %q", want, errorOf(inc))
		}
	}
	want := map[int]map[string][]string{
		0: everyHost("deploy", policyHostA, policyHostB),
		1: {policyHostA: {"r1"}},
	}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v", got, want)
	}
	if got := taskRecords(t, applyID, policyHostA, map[int]string{2: "r2"})["r2"]; got != skippedRecord {
		t.Errorf("host-a's record of r2 = %q, want %s", got, skippedRecord)
	}
	onB := taskRecords(t, applyID, policyHostB, map[int]string{1: "r1", 2: "r2"})
	for _, name := range []string{"r1", "r2"} {
		if onB[name] != skippedRecord {
			t.Errorf("host-b's record of %s = %q, want %s", name, onB[name], skippedRecord)
		}
	}
}

const mixedSourceUnit = `name: roll
description: a rescue block naming an earlier and a later source, its second child reading its first
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: ./deploy.sh }
  - name: fix
    onfail: [deploy, verify]
    block:
      - name: r1
        module: core.exec.run
        register: r1
        params: { cmd: ./fix.sh }
      - name: r2
        module: core.exec.run
        when: register.r1.stdout == 'ok'
        params: { cmd: ./restart.sh }
  - name: verify
    module: core.exec.run
    register: verify
    params: { cmd: ./verify.sh }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1, "Units with no source before them in
// their own message": the user's example of 2026-10-21). `fix` names `deploy`, at an
// earlier step, and `verify`, at a later one: no source of it travels in its own message,
// so it never fires on a live host. With nothing failed, `r1` and `r2` are settled SKIPPED on
// every host, unsent, and `r2`'s `when:`, which reads an output `r1` never produced, is
// never evaluated: the run ends ready.
//
// Mutation: take a unit by its sources at earlier steps only (config.CrossStepUnits) →
// `r1` goes the ordinary way and is skipped on every host, and `r2`'s `when:` is
// evaluated on every host and fails there; the net judges it under `scenario` and the
// run ends error_locked (without the net too, ready with `r2` FAILED on every host).
func TestIntegration_LinearAMixedSourceUnitIsNotEvaluatedWhereNothingFailed(t *testing.T) {
	hosts := []string{policyHostA, policyHostB, policyHostC}
	seedPolicyRoster(t, "redis-prod", hosts...)
	souls := newRescueSouls(t, nil, nil)
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", mixedSourceUnit), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	want := map[int]map[string][]string{0: everyHost("deploy", hosts...), 3: everyHost("verify", hosts...)}
	if got := souls.sends(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v", got, want)
	}
	for _, sid := range hosts {
		got := taskRecords(t, applyID, sid, map[int]string{1: "r1", 2: "r2"})
		for _, name := range []string{"r1", "r2"} {
			if got[name] != skippedRecord {
				t.Errorf("%s's record of %s = %q, want %s", sid, name, got[name], skippedRecord)
			}
		}
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 1-3, the user's example of 2026-10-21).
// `deploy` fails on host-b, which is held: `fix` goes to host-b alone and is settled
// SKIPPED on host-a and host-c. On host-b `r2`'s `when:` reads `r1`'s output: `r2` runs
// where it reads `ok`, and is skipped by its `when:` where it does not, the unit having
// succeeded either way since `r1` ran. The unit handles `deploy`, host-b is back for
// `verify`, and the run ends ready.
//
// Mutation: hold no host (decideHost judging every failure at once) → the run ends
// error_locked at step 0.
func TestIntegration_LinearAMixedSourceUnitHandlesTheHeldHost(t *testing.T) {
	hosts := []string{policyHostA, policyHostB, policyHostC}
	for _, tc := range []struct {
		name     string
		stdout   string
		r2OnHost bool
	}{{name: "r2 runs", stdout: "ok", r2OnHost: true}, {name: "r2 is skipped by its when", stdout: "bad"}} {
		t.Run(tc.name, func(t *testing.T) {
			seedPolicyRoster(t, "redis-prod", hosts...)
			souls := newRescueSouls(t, map[string][]string{policyHostB: {"deploy"}}, nil)
			souls.extra = map[string]map[string]map[string]any{policyHostB: {"r1": {"stdout": tc.stdout}}}
			applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", mixedSourceUnit), "roll")

			waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
			want := map[int]map[string][]string{
				0: everyHost("deploy", hosts...),
				1: {policyHostB: {"r1"}},
				3: everyHost("verify", hosts...),
			}
			r2 := skippedRecord
			if tc.r2OnHost {
				want[2], r2 = map[string][]string{policyHostB: {"r2"}}, changedRecord
			}
			if got := souls.sends(); !reflect.DeepEqual(got, want) {
				t.Fatalf("sent = %v, want %v", got, want)
			}
			if got := taskRecords(t, applyID, policyHostB, map[int]string{1: "r1", 2: "r2"}); got["r1"] != changedRecord || got["r2"] != r2 {
				t.Errorf("host-b's records of r1 and r2 = %q, %q - want %s, %s", got["r1"], got["r2"], changedRecord, r2)
			}
		})
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, the net of rule 6). The Keeper fails host-a
// before sending it the group `roll`: the `when:` of `fix`, the rescue of its sibling
// `deploy`, cannot be evaluated there. `deploy` never ran, so `fix`'s unit did not
// fire: `fix` is a source like any other, the host is judged at once by it, under its
// own radius `scenario`, and the run ends with an entry naming it — not `deploy`, whose
// FAILED record the Keeper wrote for a task that never ran. `fix`'s unit, its task
// failed, handles nothing at the group's barrier: `deploy` stays judged, so after the
// end `rollback` still reaches host-a as compensation.
//
// Mutations: replay a host the Keeper failed in plan order, its cause not first →
// `deploy`'s record fires the unit, `fix` is its unit's, host-a is judged by `deploy`
// under `host` and dropped, and the run ends partial_failed; make every unit task's
// failure its unit's (RescueRule.Fail without the Fires check) → the same; decide
// `fix`'s unit as one that executed and did not fail (RescueRule.Decide not reading
// lost) → it handles `deploy`, and `rollback` is sent nowhere.
func TestIntegration_LinearAGroupFailedByAUnitThatDidNotFireIsJudgedByIt(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, nil, nil)
	souls.extra = map[string]map[string]map[string]any{policyHostB: {"prep": {"code": 0}}}
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a group whose rescue of its sibling fails on the Keeper before the sibling ran
settings:
  on_failure: host
tasks:
  - name: prep
    module: core.exec.run
    register: prep
    params: { cmd: prep }
  - name: roll
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: deploy
        module: core.exec.run
        register: deploy
        params: { cmd: deploy }
      - name: fix
        module: core.exec.run
        onfail: [deploy]
        when: "register.prep.code == 0"
        settings: { on_failure: scenario }
        params: { cmd: fix }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	if want := `host ` + policyHostA + ` failed at task "fix" [on_failure: scenario]`; !strings.Contains(errorOf(inc), want) {
		t.Errorf("run error lacks %q: %q", want, errorOf(inc))
	}
	sent := souls.sends()
	if got, want := sent[2], map[string][]string{policyHostA: {"rollback"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v: deploy stays judged on host-a", got, want)
	}
	if got := sent[3]; len(got) != 0 {
		t.Errorf("warm sent to %v after the run ended", got)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, the net of rule 6, the applier exception). The
// destiny `own` holds a source and its own rescue; on host-a the fake Soul fails the
// rescue `fix` regardless, although `src` did not fail. `fix`'s unit did not
// fire, so its failure is a source, and the later scenario unit `renew`, naming the
// applier, holds it: host-a is held, not judged at once, `renew` handles the failure,
// and host-a is back for `warm`. Its row of the destiny is marked handled, and the run
// reads success.
//
// Mutations: drop the applier exception (lastUnit matching a source's own register
// alone) → `fix` is judged at once under `scenario` and the run ends error_locked;
// make every unit task's failure its unit's (RescueRule.Fail without the Fires check)
// → the row is a failure tied to no task, judged at once, the same.
func TestIntegration_LinearADestinyRescueThatDidNotFireIsHeldForItsApplier(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, nil, nil)
	souls.failFirst = map[string][]string{policyHostA: {"fix"}}
	r := newPolicyRunner(t, souls, nil)
	r.deps.Destiny = NewDestinySource(artifact.NewDestinyLoader(t.TempDir(), nil), fixedTemplateSource(destinyRepos(t, map[string]map[string]string{
		"own": {
			"destiny.yml": "name: own\ndescription: a source and its own rescue\n",
			"tasks/main.yml": `- name: src
  module: core.exec.run
  register: src
  params: { cmd: src }
- name: fix
  module: core.exec.run
  onfail: [src]
  params: { cmd: fix }
`,
		},
	})))
	applyID := startPolicyRun(t, r, "redis-prod", "redis", destinyServiceRepo(t, "roll", `name: roll
description: a destiny whose own rescue fails where it did not fire, a scenario rescue naming its applier
tasks:
  - name: apply own
    register: own
    apply:
      destiny: own
  - name: renew
    module: core.exec.run
    onfail: [own]
    params: { cmd: renew }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`, "own"), "roll")

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
	sent := souls.sends()
	if got, want := sent[1], map[string][]string{policyHostA: {"renew"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 1 sent = %v, want %v", got, want)
	}
	if got, want := sent[2], everyHost("warm", policyHostA, policyHostB); !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v", got, want)
	}
	assertHandledRows(t, applyID, [2]any{policyHostA, 0})
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusSuccess, true)
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 5). A row is marked handled only when every
// failure it holds was. host-a fails `deploy` and `migrate` in one message, both held
// for their rescues; host-b fails `deploy` alone. `rollback` handles `deploy` on both,
// `unmigrate` fails on host-a, and `migrate` is judged at its barrier: the run ends
// there under `scenario`. host-a's row of the group still holds `migrate` and is not
// marked; host-b's is, by the run's abort.
//
// Mutations: mark a row when any source it holds is handled (handledRows not requiring
// every one) → host-a's row of the group reads handled; write no mark in an aborted run
// (no markHandled in abort) → host-b's row is not marked.
func TestIntegration_LinearARowHoldingAnUnhandledFailureIsNotMarked(t *testing.T) {
	hosts := []string{policyHostA, policyHostB, policyHostC}
	seedPolicyRoster(t, "redis-prod", hosts...)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy", "migrate", "unmigrate"}, policyHostB: {"deploy"}}, nil)
	souls.noStop = map[string][]string{policyHostA: {"deploy"}}
	applyID := startPolicyRun(t, newPolicyRunner(t, souls, nil), "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: two failures in one message, one of them handled
tasks:
  - name: roll
    serial: 3
    settings: { serial_scope: block }
    block:
      - name: deploy
        module: core.exec.run
        register: deploy
        params: { cmd: deploy }
      - name: migrate
        module: core.exec.run
        register: migrate
        params: { cmd: migrate }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
  - name: unmigrate
    module: core.exec.run
    onfail: [migrate]
    params: { cmd: unmigrate }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	for _, want := range []string{"step 2 failed", `at task "migrate" of step 0`, "unmigrate failed on " + policyHostA} {
		if !strings.Contains(errorOf(inc), want) {
			t.Errorf("run error lacks %q: %q", want, errorOf(inc))
		}
	}
	if strings.Contains(errorOf(inc), "host "+policyHostB) {
		t.Errorf("run error names host-b, whose failure was handled: %q", errorOf(inc))
	}
	sent := souls.sends()
	if got, want := sent[1], everyHost("rollback", policyHostA, policyHostB); !reflect.DeepEqual(got, want) {
		t.Errorf("step 1 sent = %v, want %v", got, want)
	}
	if got, want := sent[2], everyHost("unmigrate", policyHostA); !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v", got, want)
	}
	assertHandledRows(t, applyID, [2]any{policyHostB, 0})
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusFailed, true)
}

// queryRefuser fails every statement sent through its pool that refuse matches, and
// counts them.
type queryRefuser struct {
	refuse  func(pgx.TraceQueryStartData) bool
	refused atomic.Int64
}

func (q *queryRefuser) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !q.refuse(data) {
		return ctx
	}
	q.refused.Add(1)
	refused, cancel := context.WithCancel(ctx)
	cancel()
	return refused
}

func (*queryRefuser) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// refusingPool is a pool on the integration database failing the statements refuse
// matches.
func refusingPool(t *testing.T, refuse func(pgx.TraceQueryStartData) bool) (*pgxpool.Pool, *queryRefuser) {
	t.Helper()
	cfg := integrationPool.Config()
	refuser := &queryRefuser{refuse: refuse}
	cfg.ConnConfig.Tracer = refuser
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, refuser
}

func handledMark(data pgx.TraceQueryStartData) bool {
	return strings.Contains(data.SQL, "SET handled = TRUE")
}

func runSentinel(data pgx.TraceQueryStartData) bool {
	return strings.Contains(data.SQL, "INSERT INTO apply_runs") && len(data.Args) > 1 && data.Args[1] == any(render.RunSentinelSID)
}

func teardownArchive(data pgx.TraceQueryStartData) bool {
	return strings.Contains(data.SQL, "INSERT INTO incarnation_archive")
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 5). A run that commits ready marks its
// handled rows in the commit's transaction, so the incarnation is never ready beside a
// run that reads failed: host-a's failure is handled, its row cannot be marked, and the
// commit fails with the mark — the incarnation is locked state_commit_failed and the
// run reads failed.
//
// Mutation: mark after the commit (commitSuccess without its marks, markHandled after
// it) → the incarnation commits ready while the run reads failed.
func TestIntegration_LinearAMarkThatCannotBeWrittenFailsTheCommit(t *testing.T) {
	seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
	r := newPolicyRunner(t, souls, nil)
	pool, refuser := refusingPool(t, handledMark)
	r.deps.DB = pool
	applyID := startPolicyRun(t, r, "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a failure its rescue handles
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
  - name: warm
    module: core.exec.run
    params: { cmd: warm }
`), "roll")

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "state_commit_failed" {
		t.Errorf("reason = %v, want state_commit_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
	}
	if refuser.refused.Load() == 0 {
		t.Error("no handled mark was refused: the run marked nothing")
	}
	if got, want := souls.sends()[2], everyHost("warm", policyHostA, policyHostB); !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v: host-a's failure was handled", got, want)
	}
	assertHandledRows(t, applyID)
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusFailed, true)
}

// ensureTerminalFunc is the frame name of [Runner.ensureTerminalApplyRun], taken from the
// method itself so that renaming it breaks the build instead of disarming
// [abortStatusRead].
var ensureTerminalFunc = runtime.FuncForPC(reflect.ValueOf((*Runner).ensureTerminalApplyRun).Pointer()).Name()

// abortStatusRead matches a read of the run's rows issued from
// [Runner.ensureTerminalApplyRun].
func abortStatusRead(data pgx.TraceQueryStartData) bool {
	if !statusesRead(data) {
		return false
	}
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for more := true; more; {
		var f runtime.Frame
		f, more = frames.Next()
		if f.Function == ensureTerminalFunc {
			return true
		}
	}
	return false
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 5). An aborted run reads failed although
// every failed row it has was handled: host-a's `deploy` is handled, then step 2 fails
// to render. The abort writes the run sentinel first, deciding host-a's row as marked,
// and marks the row only once the sentinel is written; a sentinel it cannot write, or
// rows it cannot read, leave the row unmarked, a failure of the run.
//
// Mutations: mark before the sentinel (markHandled ahead of ensureTerminalApplyRun) →
// with the sentinel refused, the run reads success; decide without the rows to mark
// (carriesAbort ignoring handled) → no sentinel, and the run reads success; true when
// the rows cannot be read → the row is marked, and the run reads success.
func TestIntegration_LinearAnAbortMarksItsHandledRowsAfterItsSentinel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(pgx.TraceQueryStartData) bool
	}{
		{"sentinel written", nil},
		{"sentinel refused", runSentinel},
		{"rows unread", abortStatusRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused := tc.refuse != nil
			seedPolicyRoster(t, "redis-prod", policyHostA, policyHostB)
			souls := newRescueSouls(t, map[string][]string{policyHostA: {"deploy"}}, nil)
			r := newPolicyRunner(t, souls, nil)
			pool, refuser := refusingPool(t, func(data pgx.TraceQueryStartData) bool {
				return refused && tc.refuse(data)
			})
			r.deps.DB = pool
			applyID := startPolicyRun(t, r, "redis-prod", "redis", linearServiceRepo(t, "roll", `name: roll
description: a failure its rescue handles, then a step that fails to render
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
  - name: reads a missing register key
    module: core.exec.run
    params: { cmd: "${ register.deploy.no_such_key }" }
`), "roll")

			inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
			if inc.StatusDetails["reason"] != "render_failed" {
				t.Errorf("reason = %v, want render_failed (details %v)", inc.StatusDetails["reason"], inc.StatusDetails)
			}
			if got, want := souls.sends()[1], map[string][]string{policyHostA: {"rollback"}}; !reflect.DeepEqual(got, want) {
				t.Errorf("step 1 sent = %v, want %v", got, want)
			}
			if refused != (refuser.refused.Load() > 0) {
				t.Errorf("statements refused = %d, want refused %v", refuser.refused.Load(), refused)
			}
			sentinel, written := rowsOf(t, applyID)[[2]any{render.RunSentinelSID, 2}]
			if written == refused || written && sentinel.Status != applyrun.StatusFailed {
				t.Errorf("run sentinel at step 2 = %+v (written %v), want written %v, failed", sentinel, written, !refused)
			}
			want := [][2]any{{policyHostA, 0}}
			if refused {
				want = nil
			}
			assertHandledRows(t, applyID, want...)
			assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusFailed, true)
		})
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 5, a teardown). The teardown's rescue
// handles host-a's failure and the teardown succeeds on every host: its rows are
// marked before its incarnation is dropped with them. The drop is refused here, so the
// rows stay to read — host-a's row marked handled, the run success, the incarnation
// still destroying.
//
// Mutation: mark nothing on the teardown path (no markHandled before
// DeleteAfterTeardown) → host-a's row is not marked and the run reads failed.
func TestIntegration_LinearATeardownMarksTheRowsItsRescueHandled(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedDestroyingIncarnation(t, "redis-prod", nil)
	for _, sid := range []string{policyHostA, policyHostB} {
		seedConnectedSoul(t, sid, []string{"redis-prod"})
	}
	souls := newRescueSouls(t, map[string][]string{policyHostA: {"stop"}}, nil)
	r := newPolicyRunner(t, souls, nil)
	pool, refuser := refusingPool(t, teardownArchive)
	r.deps.DB = pool
	applyID := audit.NewULID()
	if err := r.StartDestroy(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef: artifact.ServiceRef{Name: "redis", Ref: "master", Git: linearServiceRepo(t, DestroyScenarioName, `name: destroy
description: a teardown whose rescue handles a failure
tasks:
  - name: stop
    module: core.exec.run
    register: stop
    params: { cmd: stop }
  - name: kill
    module: core.exec.run
    onfail: [stop]
    params: { cmd: kill }
  - name: wipe
    module: core.exec.run
    params: { cmd: wipe }
`)},
		StartedByAID: "archon-alice",
	}); err != nil {
		t.Fatalf("StartDestroy: %v", err)
	}
	done, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.Shutdown(done); err != nil {
		t.Fatalf("the teardown did not end: %v", err)
	}

	if refuser.refused.Load() == 0 {
		t.Fatal("the teardown never reached the drop of its incarnation")
	}
	if inc, err := incarnation.SelectByID(context.Background(), integrationPool, "redis-prod"); err != nil || inc.Status != incarnation.StatusDestroying {
		t.Fatalf("incarnation = %+v, %v; want it destroying, its drop refused", inc, err)
	}
	sent := souls.sends()
	if got, want := sent[1], map[string][]string{policyHostA: {"kill"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 1 sent = %v, want %v", got, want)
	}
	if got, want := sent[2], everyHost("wipe", policyHostA, policyHostB); !reflect.DeepEqual(got, want) {
		t.Errorf("step 2 sent = %v, want %v", got, want)
	}
	assertHandledRows(t, applyID, [2]any{policyHostA, 0})
	assertRunReads(t, "redis-prod", applyID, applyrun.RunStatusSuccess, true)
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 5). A teardown's rows go with its
// incarnation, so a row a teardown cannot mark for being gone is a warning; for any
// other run it is the error that the run reads the row as a failure.
//
// Mutation: drop the teardown case of markHandled → the teardown logs the error.
func TestIntegration_AGoneRowOfATeardownIsAWarning(t *testing.T) {
	resetAll(t)
	for _, tc := range []struct {
		mode TerminalMode
		want string
	}{
		{TerminalCommitState, "ERROR"},
		{TerminalDestroy, "WARN"},
	} {
		logs := &lockedBuffer{}
		r := &Runner{deps: Deps{DB: integrationPool}}
		spec := RunSpec{ApplyID: audit.NewULID(), TerminalMode: tc.mode}
		r.markHandled(context.Background(), spec, []runRow{{sid: policyHostA, step: 0}}, slog.New(slog.NewJSONHandler(logs, nil)))
		var record struct {
			Level string `json:"level"`
		}
		if err := json.Unmarshal([]byte(logs.String()), &record); err != nil || record.Level != tc.want {
			t.Errorf("mode %d logged %q (%v), want one %s record", tc.mode, logs.String(), err, tc.want)
		}
	}
}
