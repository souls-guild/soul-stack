//go:build integration

package scenario

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The linear dispatch loop (ADR-0091, §Decision, decisions 1 and 3): step
// k goes to any host only after every dispatch of the steps before it has finished
// on every host it went to, a step that went to no host included, and a keeper-side
// task runs where it is written.

// stepEvent is one moment the Keeper and a host exchanged: a step sent to a host,
// or that host's terminal for it.
type stepEvent struct {
	seq      int
	terminal bool
	passage  int
	sid      string
	tasks    []string
}

// orderedStepDispatcher answers every ApplyRequest asynchronously, the way a Soul
// does, after a per-host delay, and records sends and terminals on one clock. It
// writes the registers named in stdout per host, and probe, when set, runs at each
// send and terminal so a test can look at the database at that moment.
type orderedStepDispatcher struct {
	t      *testing.T
	delay  map[string]time.Duration
	stdout map[string]map[string]string // register → sid → stdout
	probe  func(stepEvent)

	mu     sync.Mutex
	seq    int
	events []stepEvent
}

func (d *orderedStepDispatcher) record(ev stepEvent) {
	d.mu.Lock()
	d.seq++
	ev.seq = d.seq
	d.events = append(d.events, ev)
	d.mu.Unlock()
	if d.probe != nil {
		d.probe(ev)
	}
}

func (d *orderedStepDispatcher) SendApply(_ context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()
	names := make([]string, 0, len(req.GetTasks()))
	for _, task := range req.GetTasks() {
		names = append(names, task.GetName())
	}
	d.record(stepEvent{passage: passage, sid: sid, tasks: names})

	go func() {
		ctx := context.Background()
		time.Sleep(d.delay[sid])
		for register, bySID := range d.stdout {
			localIdx, planIdx := emitterIndices(req, register)
			if localIdx < 0 {
				continue
			}
			if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
				ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx,
				RegisterData: map[string]any{"stdout": bySID[sid], "changed": false, "failed": false},
				Passage:      passage,
			}); err != nil {
				d.t.Errorf("orderedStepDispatcher: UpsertTaskRegister(%s, %s): %v", sid, register, err)
			}
		}
		d.record(stepEvent{terminal: true, passage: passage, sid: sid})
		if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
			d.t.Errorf("orderedStepDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
		}
	}()
	return nil
}

func (d *orderedStepDispatcher) snapshot() []stepEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]stepEvent(nil), d.events...)
}

// sends is passage → sid → task names of what each host was sent.
func sends(events []stepEvent) map[int]map[string][]string {
	out := map[int]map[string][]string{}
	for _, ev := range events {
		if ev.terminal {
			continue
		}
		if out[ev.passage] == nil {
			out[ev.passage] = map[string][]string{}
		}
		names := append([]string(nil), ev.tasks...)
		sort.Strings(names)
		out[ev.passage][ev.sid] = names
	}
	return out
}

// assertHappensBefore fails for every send of step k that came before the
// terminal of a step < k on any host that step went to.
func assertHappensBefore(t *testing.T, events []stepEvent) {
	t.Helper()
	terminalAt := map[[2]any]int{}
	for _, ev := range events {
		if ev.terminal {
			terminalAt[[2]any{ev.passage, ev.sid}] = ev.seq
		}
	}
	for _, send := range events {
		if send.terminal {
			continue
		}
		for _, earlier := range events {
			if earlier.terminal || earlier.passage >= send.passage {
				continue
			}
			done, ok := terminalAt[[2]any{earlier.passage, earlier.sid}]
			if !ok || done > send.seq {
				t.Errorf("step %d sent to %s before step %d finished on %s (events %+v)",
					send.passage, send.sid, earlier.passage, earlier.sid, events)
			}
		}
	}
}

func startModelRun(t *testing.T, r *Runner, model ExecutionModel, incName, service, gitURL, scenario string) string {
	t.Helper()
	r.deps.ExecutionModel = model
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

// ★ GUARD (ADR-0091, §Decision). Three independent steps, none reading a
// register; the middle one targets no host. One host answers slowly. Every
// ApplyRequest carries one step, and the last step goes to neither host until the
// slow host has finished the first — across a step that went to nobody, since the
// ordering is on k waiting, not on k-1 producing a barrier.
//
// Mutations: number a linear run with Stratify (from register references) → the
// three tasks are one Passage, one message per host; return from a step without
// waiting for its barrier → the last step reaches the fast host while the slow one
// still runs the first.
func TestIntegration_LinearStepWaitsForEveryEarlierTerminal(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, hostA, []string{"redis-prod"})
	seedConnectedSoul(t, hostB, []string{"redis-prod"})
	gitURL := linearServiceRepo(t, "steps", `name: steps
description: three independent steps, the middle one for no host
tasks:
  - name: first
    module: core.exec.run
    params: { cmd: first }
  - name: nobody
    module: core.exec.run
    where: "soulprint.self.sid == 'nobody.example.com'"
    params: { cmd: nobody }
  - name: last
    module: core.exec.run
    params: { cmd: last }
`)
	disp := &orderedStepDispatcher{t: t, delay: map[string]time.Duration{hostB: 300 * time.Millisecond}}
	applyID := startModelRun(t, newRunner(t, disp, gitURL), ExecutionLinear, "redis-prod", "redis", gitURL, "steps")
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	events := disp.snapshot()
	want := map[int]map[string][]string{
		0: {hostA: {"first"}, hostB: {"first"}},
		2: {hostA: {"last"}, hostB: {"last"}},
	}
	if got := sends(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent = %v, want %v", got, want)
	}
	assertHappensBefore(t, events)
}

// ★ GUARD (ADR-0091, decision 3; the shape of
// examples/service/long-runner/scenario/serial_waves). A keeper-side capture
// written after a host step rolled in serial waves runs after the last wave's
// terminal: at no send and no terminal of the host step is the field written yet.
// The derivation put it in the host step's Passage, and a Passage ran its
// keeper-side tasks before its host dispatch.
//
// Mutations: number a linear run with Stratify → the capture shares Passage 0 and
// runs before the first wave; give a keeper-side task the first step → the same.
func TestIntegration_LinearKeeperStepRunsAfterHostSteps(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, hostA, []string{"noop-prod"})
	seedConnectedSoul(t, hostB, []string{"noop-prod"})
	gitURL := writeServiceRepo(t, `name: create
description: waves, then state committed once
tasks:
  - name: roll
    module: core.exec.run
    serial: 1
    params: { cmd: roll }
  - name: commit once after all waves
    module: core.state.set
    params:
      field: recorded
      value: after-the-waves
`)

	var early []stepEvent
	var mu sync.Mutex
	disp := &orderedStepDispatcher{t: t, delay: map[string]time.Duration{hostA: 100 * time.Millisecond, hostB: 100 * time.Millisecond}}
	disp.probe = func(ev stepEvent) {
		st, err := incarnation.SelectState(context.Background(), integrationPool, "noop-prod")
		if err != nil {
			t.Errorf("SelectState: %v", err)
			return
		}
		if _, written := st["recorded"]; written {
			mu.Lock()
			early = append(early, ev)
			mu.Unlock()
		}
	}
	applyID := startModelRun(t, newRunner(t, disp, gitURL), ExecutionLinear, "noop-prod", "noop", gitURL, "create")
	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	if got := inc.State["recorded"]; got != "after-the-waves" {
		t.Fatalf("state.recorded = %v, want after-the-waves", got)
	}
	if want := map[int]map[string][]string{0: {hostA: {"roll"}, hostB: {"roll"}}}; !reflect.DeepEqual(sends(disp.snapshot()), want) {
		t.Fatalf("sent = %v, want %v", sends(disp.snapshot()), want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(early) > 0 {
		t.Errorf("the capture was written while the host step ran: %+v", early)
	}
}

// crossStepWhenScenario reads the probe in a later task's when:. The derivation
// put that task a Passage after the probe (through register.marker) and refused it
// as cross_passage_when_unsupported (FC-5); soul-lint lints the same shape
// (testdata/scenario-golden/cross-task-when.yml).
const crossStepWhenScenario = `name: gate
description: a when that reads a register from an earlier step
tasks:
  - name: probe
    module: core.exec.run
    register: role
    changed_when: "false"
    params: { cmd: role }
  - name: mark
    module: core.exec.run
    register: marker
    changed_when: "false"
    params: { cmd: mark }
  - name: act
    module: core.exec.run
    when: "register.role.stdout == 'master'"
    changed_when: "false"
    vars:
      seed: "${ register.marker.stdout }"
    params: { cmd: act }
`

// ★ GUARD, the Keeper half of FC-5's retirement (ADR-0091, S1; the linter
// half is TestGolden_ScenarioCrossTaskWhen). Under linear dispatch the Keeper
// decides `act`'s when: per host, so the run goes through and `act` reaches only
// the master. Staged, the same file is refused before anything is sent.
//
// Mutation: keep FC-5 for a linear run → refused cross_passage_when_unsupported.
func TestIntegration_LinearCrossStepWhenRuns(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	for _, model := range []ExecutionModel{ExecutionLinear, ExecutionStaged} {
		t.Run(string(model), func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnation(t, "redis-prod")
			seedConnectedSoul(t, hostA, []string{"redis-prod"})
			seedConnectedSoul(t, hostB, []string{"redis-prod"})
			gitURL := linearServiceRepo(t, "gate", crossStepWhenScenario)
			disp := &orderedStepDispatcher{t: t, stdout: map[string]map[string]string{
				"role":   {hostA: "master", hostB: "replica"},
				"marker": {hostA: "marked", hostB: "marked"},
			}}
			applyID := startModelRun(t, newRunner(t, disp, gitURL), model, "redis-prod", "redis", gitURL, "gate")

			if model == ExecutionStaged {
				inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
				if inc.StatusDetails["reason"] != config.CodeCrossPassageWhenGating {
					t.Errorf("staged reason = %v, want %s", inc.StatusDetails["reason"], config.CodeCrossPassageWhenGating)
				}
				if got := disp.snapshot(); len(got) != 0 {
					t.Errorf("staged run sent %+v, want nothing", got)
				}
				return
			}
			waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
			want := map[int]map[string][]string{
				0: {hostA: {"probe"}, hostB: {"probe"}},
				1: {hostA: {"mark"}, hostB: {"mark"}},
				2: {hostA: {"act"}},
			}
			if got := sends(disp.snapshot()); !reflect.DeepEqual(got, want) {
				t.Errorf("sent = %v, want %v", got, want)
			}
		})
	}
}

// ★ GUARD (ADR-0091 item 9). Under linear dispatch a default block's
// children are steps, so `reload onchanges: [cfg]` reads a source from an earlier
// message and only the audit log can answer it. A Keeper without one refuses the
// run before anything is sent; guessing "not changed" would skip the reload in
// silence. Staged, the block is one message and the Soul resolves the link itself.
//
// Mutation: ask the staged detector, which reads the block as one entry, for a
// linear run → the run goes out and `reload` is gated by nothing.
func TestIntegration_LinearCrossStepRequisiteNeedsAuditLog(t *testing.T) {
	const hostA = "host-a.example.com"
	for _, model := range []ExecutionModel{ExecutionLinear, ExecutionStaged} {
		t.Run(string(model), func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnation(t, "redis-prod")
			seedConnectedSoul(t, hostA, []string{"redis-prod"})
			gitURL := linearServiceRepo(t, "pair", `name: pair
description: a default block whose second child reloads on the first child's change
tasks:
  - name: pair
    block:
      - name: write config
        module: core.exec.run
        register: cfg
        params: { cmd: write }
      - name: reload
        module: core.exec.run
        onchanges: [cfg]
        params: { cmd: reload }
`)
			disp := &orderedStepDispatcher{t: t}
			applyID := startModelRun(t, newRunner(t, disp, gitURL), model, "redis-prod", "redis", gitURL, "pair")

			if model == ExecutionStaged {
				waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
				want := map[int]map[string][]string{0: {hostA: {"reload", "write config"}}}
				if got := sends(disp.snapshot()); !reflect.DeepEqual(got, want) {
					t.Errorf("staged sent = %v, want %v", got, want)
				}
				return
			}
			inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
			if inc.StatusDetails["reason"] != "cross_passage_requisite_unsupported" {
				t.Errorf("reason = %v, want cross_passage_requisite_unsupported", inc.StatusDetails["reason"])
			}
			if got := disp.snapshot(); len(got) != 0 {
				t.Errorf("linear run sent %+v, want nothing", got)
			}
		})
	}
}

// ★ GUARD for retiring `state_stale_same_passage_read` (ADR-0091, S1). A
// capture and a later task interpolating the field it writes, with no register
// edge between them: the derivation put both in one Passage and the reader rendered
// the pre-run value — the read soul-lint refused. Under linear dispatch the capture
// is a step of its own and the state is re-read before the reader's step renders.
//
// Mutations: drop the state re-read between steps, or number a linear run with
// Stratify → the reader renders the pre-run value.
func TestIntegration_LinearStateReadAfterCaptureSeesIt(t *testing.T) {
	const scenario = `
name: create
tasks:
  - name: Capture the endpoint
    module: core.state.set
    params:
      field: endpoint
      value: "10.0.0.1"
  - name: Configure against the captured endpoint
    module: core.exec.run
    changed_when: "false"
    params:
      cmd: echo
      args: ["${ incarnation.state.endpoint }"]
`
	want := map[ExecutionModel]string{ExecutionLinear: "10.0.0.1", ExecutionStaged: "pre-run-endpoint"}
	for _, model := range []ExecutionModel{ExecutionLinear, ExecutionStaged} {
		t.Run(string(model), func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnationStateRoster(t, "noop-prod", map[string]any{"endpoint": "pre-run-endpoint"}, "host-a.example.com")
			gitURL := stateServiceRepo(t, scenario)
			disp := &paramsDispatcher{t: t}
			r := newRunnerWithKeeper(t, disp, keeperRegistryWith(nil))
			applyID := startModelRun(t, r, model, "noop-prod", "noop", gitURL, "create")
			waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

			if len(disp.args) != 1 || len(disp.args[0]) != 1 {
				t.Fatalf("dispatched args = %v, want one list of one", disp.args)
			}
			if got := disp.args[0][0]; got != want[model] {
				t.Errorf("rendered endpoint = %v, want %v", got, want[model])
			}
		})
	}
}

// cancellingKeeperModule plays another Keeper instance: while the keeper step runs,
// it flags the run cancelled in PG, as `POST .../cancel` on a non-owner does.
type cancellingKeeperModule struct {
	module.BaseModule
	mu       sync.Mutex
	affected int64
}

func (m *cancellingKeeperModule) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	scope, ok := util.RunScopeFrom(stream.Context())
	if !ok {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "no run scope in the module context"})
	}
	affected, err := applyrun.RequestCancel(stream.Context(), integrationPool, scope.ApplyID)
	if err != nil {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: err.Error()})
	}
	m.mu.Lock()
	m.affected = affected
	m.mu.Unlock()
	return stream.Send(&pluginv1.ApplyEvent{Changed: true})
}

// ★ GUARD (ADR-0091, decision 3). A keeper step written last has no
// barrier after it, so nothing polled a cluster-wide Cancel that flagged its row,
// and the run committed `ready` over the operator's Cancel. Two checks read it
// now: the one after a step's keeper tasks (NIM-972, staged runs too) and the
// one at a linear step's end.
//
// Mutation: drop both checks → the run ends ready.
func TestIntegration_LinearCancelDuringLastKeeperStep(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, `name: create
description: host work, then a keeper step during which another instance cancels
tasks:
  - name: work on the host
    module: core.exec.run
    changed_when: "false"
    params: { cmd: work }
  - name: another instance cancels the run here
    module: fakekeeper.cancel.requested
    on: keeper
    params: {}
`)
	cancel := &cancellingKeeperModule{}
	disp := &orderedStepDispatcher{t: t}
	r := newRunnerWithKeeper(t, disp, keeperRegistryWith(map[string]module.SoulModule{"fakekeeper.cancel": cancel}))
	applyID := startModelRun(t, r, ExecutionLinear, "noop-prod", "noop", gitURL, "create")

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (a cluster-wide Cancel)", inc.StatusDetails["reason"])
	}
	cancel.mu.Lock()
	defer cancel.mu.Unlock()
	if cancel.affected == 0 {
		t.Fatal("RequestCancel flagged no row - the keeper step held no running row, and the test proves nothing")
	}
}

// ★ GUARD (NIM-944). A host task reloading on a keeper step's change: the keeper
// step is a step of its own, so the link is resolved from the audit log, where the
// keeper's fact is recorded under the keeper's SID, not the host's. Read under the
// host's SID it never matched, and the reload was dropped on every host in silence.
//
// Mutation: look a keeper-side source's fact up under the consumer's SID → nothing
// is sent at the reload's step.
func TestIntegration_LinearHostReloadsOnAKeeperStepChange(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, hostA, []string{"noop-prod"})
	seedConnectedSoul(t, hostB, []string{"noop-prod"})
	gitURL := writeServiceRepo(t, `name: create
description: a keeper step records a value, the hosts reload when it changed
tasks:
  - name: record
    module: core.state.set
    register: recorded
    params:
      field: recorded
      value: fresh
  - name: reload
    module: core.exec.run
    onchanges: [recorded]
    params: { cmd: reload }
`)
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	disp := &orderedStepDispatcher{t: t}
	r := NewRunner(Deps{
		Loader:        artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:      topology.NewResolver(integrationPool, nil, nil),
		ServiceVars:   servicevars.NewResolver(nil),
		Render:        render.NewPipeline(nil, engine, nil, nil),
		Outbound:      disp,
		DB:            integrationPool,
		KeeperModules: keeperRegistryWith(nil),
		Audit:         auditpg.NewWriter(integrationPool),
		AuditReader:   auditpg.NewReader(integrationPool),
		SoulCap:       stubSoulCap{},
		PollInterval:  20 * time.Millisecond,
		RunTimeout:    20 * time.Second,
	})
	applyID := startModelRun(t, r, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	want := map[int]map[string][]string{1: {hostA: {"reload"}, hostB: {"reload"}}}
	if got := sends(disp.snapshot()); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %v, want %v", got, want)
	}
}
