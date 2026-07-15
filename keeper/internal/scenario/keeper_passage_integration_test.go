//go:build integration

// Guard suite for Slice 2 (keeper-side dispatch per-Passage, ADR-056). Before
// Slice 2, dispatchKeeperTasks was called ONCE before the stage-loop, on the first
// render's tasks (ActivePassage=0), where keeper-tasks for Passage>0 were
// paramless placeholders that never got dispatched. Slice 2 moved the call INSIDE
// the stage-loop, per-Passage, on tasks re-rendered at ActivePassage=p.
//
// Runs go through run()+PG (Start → waitRunDone) with a keeper-Registry stub, real
// auditpg, and stubPassageCap (the staged gate in ADR-056 §S5 requires
// passage-aware hosts; roster is empty for all-keeper, but a nil passageCap makes
// the fail-closed gate reject staged — prod always runs with Redis, so the stub
// mirrors prod).

package scenario

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Slice 2 guards reuse newRunnerKeeperStaged (keeper-Registry + stubPassageCap)
// from midrun_reresolve_integration_test.go — same "keeper-task + staged
// stratification" combo.

// capturingKeeperModule — keeper-side core module that CAPTURES the received
// Params (to prove keeper→keeper register-chaining: a Passage 1 task must see in
// Params a value rendered from register.<prev>.* of a Passage 0 keeper-task) and
// echoes a preset output. failOnState is the state suffix on which the module
// returns failed (for the keeper-fail test).
type capturingKeeperModule struct {
	module.BaseModule
	mu          sync.Mutex
	output      map[string]any            // output echoed in ApplyEvent (on success)
	failOnState string                    // state suffix on which to return failed
	echoParams  []string                  // keys of received Params forwarded into output (transitive chain)
	gotParams   map[string]any            // Params of the last Apply (by state)
	gotStates   []string                  // order of executed states
	gotByState  map[string]map[string]any // Params per state (for multi-passage chains sharing one module)
}

func (m *capturingKeeperModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.mu.Lock()
	m.gotStates = append(m.gotStates, req.GetState())
	var params map[string]any
	if p := req.GetParams(); p != nil {
		params = p.AsMap()
		m.gotParams = params
		if m.gotByState == nil {
			m.gotByState = map[string]map[string]any{}
		}
		m.gotByState[req.GetState()] = params
	}
	fail := req.GetState() == m.failOnState
	// out: static output plus keys forwarded from Params (echoParams). Forwarding
	// is needed for transitive chains (P2 sees the register P0 value that passed
	// through P1 → P1 puts the params key it got from register.P0.* into its own
	// output).
	out := map[string]any{}
	for k, v := range m.output {
		out[k] = v
	}
	for _, k := range m.echoParams {
		if v, ok := params[k]; ok {
			out[k] = v
		}
	}
	m.mu.Unlock()

	if fail {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "keeper task failed at " + req.GetState()})
	}
	ev := &pluginv1.ApplyEvent{Changed: true}
	if len(out) > 0 {
		ev.Output = mustStructAny(out)
	}
	return stream.Send(ev)
}

// paramsForState returns the Params the module received at a specific state (for
// chains where one module executes across multiple Passages under different
// states — e.g. core.cloud.created at P0 and core.cloud.updated at P1).
func (m *capturingKeeperModule) paramsForState(state string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gotByState[state]
}

func (m *capturingKeeperModule) params() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gotParams
}

func (m *capturingKeeperModule) states() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.gotStates...)
}

// keeperChainServiceRepo — 2-Passage all-keeper chain (ADR-056, Slice 2):
//
//	#0 provision (core.cloud.created, register: provision) → Passage 0
//	#1 deliver   (core.bootstrap.delivered, params reads register.provision.ip) → Passage 1
//
// Stratify splits by Passage (deliver reads register provision in params).
// all-keeper → no_hosts bypass. This is an end-to-end proof of keeper→keeper
// register-chaining: deliver sees the ip emitted by provision at Passage 0.
func keeperChainServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: 2-passage all-keeper chain (cloud.created -> bootstrap.delivered)
state_changes: {}
tasks:
  - name: provision vm
    module: core.cloud.created
    on: keeper
    register: provision
    params:
      provider: fake
  - name: deliver bootstrap
    module: core.bootstrap.delivered
    on: keeper
    params:
      target_ip: "${ register.provision.ip }"
`)
}

// keeperChain3ServiceRepo — 3-Passage all-keeper chain (ADR-056, Slice 2),
// ★ mirrors the target live flow for creating a redis cluster (provision→deliver→register):
//
//	#0 provision (core.bootstrap.created,   register: provision, output ip)          → Passage 0
//	#1 deliver   (core.bootstrap.delivered, params target_ip=register.provision.ip,
//	             register: deliver, echoParams forwards target_ip into output)        → Passage 1
//	#2 finalize  (core.bootstrap.finalized, params origin=register.deliver.target_ip) → Passage 2
//
// All links are base core.bootstrap (NOT in coremanifest → params aren't validated
// by scenario-load, register expressions pass freely), distinguished by state — one
// capturingKeeperModule serves all three, paramsForState(state) separates
// per-Passage Params. Each link reads the PREVIOUS link's register → Stratify
// splits into 3 Passages. Transitivity: the provision.ip value (P0) is forwarded
// through deliver (P1 puts the target_ip it received into its own register-output)
// and reaches finalize (P2) as origin. all-keeper → no_hosts bypass.
func keeperChain3ServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: 3-passage all-keeper chain (bootstrap.created -> delivered -> finalized)
state_changes: {}
tasks:
  - name: provision vm
    module: core.bootstrap.created
    on: keeper
    register: provision
    params:
      provider: fake
  - name: deliver bootstrap
    module: core.bootstrap.delivered
    on: keeper
    register: deliver
    params:
      target_ip: "${ register.provision.ip }"
  - name: finalize
    module: core.bootstrap.finalized
    on: keeper
    params:
      origin: "${ register.deliver.target_ip }"
`)
}

// TestIntegration_KeeperChain_3Passage_TransitiveRegister — ★ #1 (CRITICAL,
// target live flow for creating a redis cluster). 3-link keeper chain
// bootstrap.created(P0)→bootstrap.delivered(P1)→bootstrap.finalized(P2), each link
// reads the previous one's register. ASSERT:
//   - three apply_runs(apply_id, keeper, passage) rows for passage 0/1/2, ALL success;
//   - Params of the last one (P2) contains a value forwarded TRANSITIVELY from the
//     FIRST register (P0): provision.ip → deliver.target_ip (P1 forward) → finalize.origin (P2);
//   - no host-fan-out happened (all-keeper).
//
// Extends the 2-Passage case to 3 Passages — a guard that keeper→keeper
// register-chaining holds across more than one link (transitive accumulation).
func TestIntegration_KeeperChain_3Passage_TransitiveRegister(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// No hosts seeded: all-keeper → no_hosts bypass (provision-from-zero).

	// One core.bootstrap module serves all three states (created/delivered/finalized):
	// echoes ip in every register-output; echoParams forwards target_ip from params
	// into output (P1 puts the target_ip it got from register.provision.ip into its
	// own register-output → finalize reads register.deliver.target_ip).
	bootstrap := &capturingKeeperModule{
		output:     map[string]any{"ip": "10.0.0.7"},
		echoParams: []string{"target_ip"},
	}
	keepers := fakeKeeperRegistry{"core.bootstrap": bootstrap}
	gitURL := keeperChain3ServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

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

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// All three keeper states executed (once each, in their own Passage), in chain order.
	if got := bootstrap.states(); len(got) != 3 || got[0] != "created" || got[1] != "delivered" || got[2] != "finalized" {
		t.Errorf("bootstrap states = %v, want [created delivered finalized]", got)
	}

	// The middle link (P1, state delivered) got ip from P0 (register.provision.ip).
	if got := bootstrap.paramsForState("delivered"); got == nil || got["target_ip"] != "10.0.0.7" {
		t.Fatalf("P1 (delivered) Params.target_ip = %v, want '10.0.0.7' (register.provision.ip P0 не прокинут)", got["target_ip"])
	}

	// ★ TRANSITIVE forwarding: finalize (P2, state finalized) got origin == ip of
	// the FIRST task (P0), passed through deliver (P1). register-chaining holds
	// across more than one link.
	got := bootstrap.paramsForState("finalized")
	if got == nil || got["origin"] != "10.0.0.7" {
		t.Fatalf("★ P2 (finalized) Params.origin = %v, want '10.0.0.7' — значение register ПЕРВОЙ задачи (P0) НЕ протянуто транзитивно через P1 до P2 (keeper→keeper chaining оборвался на 2-м звене)", got["origin"])
	}

	// No host-fan-out happened (all-keeper).
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (all-keeper)", disp.calls)
	}

	// apply_runs = exactly 3 keeper rows (apply_id, keeper, 0/1/2), all success. No host rows.
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	keeperPassages := map[int]applyrun.Status{}
	for _, st := range statuses {
		if st.SID != render.KeeperTargetSID {
			t.Errorf("неожиданная не-keeper apply_runs-строка sid=%s passage=%d", st.SID, st.Passage)
			continue
		}
		keeperPassages[st.Passage] = st.Status
	}
	if len(keeperPassages) != 3 {
		t.Fatalf("★ keeper apply_runs passages = %v, want ровно {0,1,2} (по строке на Passage с keeper-задачами)", keeperPassages)
	}
	for _, p := range []int{0, 1, 2} {
		if keeperPassages[p] != applyrun.StatusSuccess {
			t.Errorf("keeper apply_runs[passage=%d] = %s, want success", p, keeperPassages[p])
		}
	}
}

// TestIntegration_KeeperChain_Rerun_NoPKConflict — ★ #5 (operational rerun). Two
// consecutive runs of the same staged keeper chain with DIFFERENT apply_id values.
// ASSERT:
//   - the second run does NOT hit a PK conflict on apply_runs(apply_id, keeper, passage)
//     (migration 078's triple PK distinguishes runs by apply_id) — the chain
//     RE-EXECUTES in full (both Passages again);
//   - run #1's apply_runs rows (apply_id_1) are NOT overwritten by run #2's rows —
//     each run carries its own set of keeper rows under its own apply_id;
//   - register-chaining works on BOTH runs (deliver got ip on #2 too).
func TestIntegration_KeeperChain_Rerun_NoPKConflict(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	cloud := &capturingKeeperModule{output: map[string]any{"ip": "10.0.0.7"}}
	bootstrap := &capturingKeeperModule{output: map[string]any{"delivered": true}}
	keepers := fakeKeeperRegistry{
		"core.cloud":     cloud,
		"core.bootstrap": bootstrap,
	}
	gitURL := keeperChainServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	run := func(applyID string) {
		t.Helper()
		if err := r.Start(context.Background(), RunSpec{
			ApplyID:         applyID,
			IncarnationName: "noop-prod",
			ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
			ScenarioName:    "create",
			StartedByAID:    "archon-alice",
		}); err != nil {
			t.Fatalf("Start(%s): %v", applyID, err)
		}
		waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	}

	// keeperPassages — keeper rows of a specific run (by apply_id), all success.
	keeperPassages := func(applyID string) map[int]applyrun.Status {
		t.Helper()
		statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
		if err != nil {
			t.Fatalf("SelectStatusesByApplyID(%s): %v", applyID, err)
		}
		out := map[int]applyrun.Status{}
		for _, st := range statuses {
			if st.SID != render.KeeperTargetSID {
				t.Errorf("прогон %s: не-keeper строка sid=%s passage=%d", applyID, st.SID, st.Passage)
				continue
			}
			out[st.Passage] = st.Status
		}
		return out
	}

	applyID1 := audit.NewULID()
	run(applyID1)
	if got := keeperPassages(applyID1); len(got) != 2 || got[0] != applyrun.StatusSuccess || got[1] != applyrun.StatusSuccess {
		t.Fatalf("прогон #1 keeper passages = %v, want {0:success,1:success}", got)
	}

	// The second run uses a different apply_id. If Passage weren't part of the PK,
	// or runs weren't distinguished by apply_id, the second Insert(running) on
	// (apply_id, keeper, passage) would fail as a duplicate → keeper_dispatch_failed
	// → error_locked (waitRunDone in run() would never reach Ready).
	applyID2 := audit.NewULID()
	run(applyID2)
	if got := keeperPassages(applyID2); len(got) != 2 || got[0] != applyrun.StatusSuccess || got[1] != applyrun.StatusSuccess {
		t.Fatalf("★ прогон #2 keeper passages = %v, want {0:success,1:success} (rerun staged keeper-цепочки переисполнился без PK-конфликта)", got)
	}

	// ★ Run #1's rows are NOT overwritten by run #2 — each apply_id carries its own set.
	if got := keeperPassages(applyID1); len(got) != 2 {
		t.Fatalf("★ прогон #1 keeper passages ПОСЛЕ rerun = %v, want по-прежнему {0,1} (строки #1 затёрты прогоном #2 — apply_id не изолирует)", got)
	}

	// register-chaining worked on the SECOND run too (deliver sees ip).
	if got := bootstrap.params(); got == nil || got["target_ip"] != "10.0.0.7" {
		t.Errorf("после rerun bootstrap.delivered Params.target_ip = %v, want '10.0.0.7'", got["target_ip"])
	}
	// Each link executed twice (two runs).
	if got := cloud.states(); len(got) != 2 {
		t.Errorf("cloud states = %v, want 2 исполнения (два прогона)", got)
	}
	if got := bootstrap.states(); len(got) != 2 {
		t.Errorf("bootstrap states = %v, want 2 исполнения (два прогона)", got)
	}
}

// keeperForwardAccumServiceRepo — #2 forward-accumulation: P2 reads register from
// BOTH P0 AND P1 at once. All links are base core.bootstrap (free-form params),
// distinguished by state.
//
//	#0 provision (core.bootstrap.created,   register: provision, output ip+token)    → Passage 0
//	#1 deliver   (core.bootstrap.delivered, register: deliver,   output ip+token)    → Passage 1 (reads register.provision.ip)
//	#2 finalize  (core.bootstrap.finalized, params from_p0=register.provision.ip
//	                                              + from_p1=register.deliver.token)   → Passage 2
//
// finalize (P2) reads register of the two previous Passages at once — the
// register bucket in keeperVars at P2 carries accumulated provision (P0) AND
// deliver (P1).
func keeperForwardAccumServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: P2 reads register of both P0 and P1 (forward-accumulation)
state_changes: {}
tasks:
  - name: provision vm
    module: core.bootstrap.created
    on: keeper
    register: provision
    params:
      provider: fake
  - name: deliver bootstrap
    module: core.bootstrap.delivered
    on: keeper
    register: deliver
    params:
      target_ip: "${ register.provision.ip }"
  - name: finalize reads both
    module: core.bootstrap.finalized
    on: keeper
    params:
      from_p0: "${ register.provision.ip }"
      from_p1: "${ register.deliver.token }"
`)
}

// TestIntegration_KeeperChain_ForwardAccumulation — #2: a Passage 2 keeper-task
// reads register of BOTH previous Passages (P0 provision AND P1 deliver) in one
// render. ASSERT: finalize (P2) got from_p0 == ip (register P0) AND from_p1 ==
// token (register P1) — the KeeperRegister bucket at P2 carries the accumulated
// register of all past Passages (loadRegisterByHostUpToPassage(P2) = register of
// Passage<2), not just the immediately preceding one.
func TestIntegration_KeeperChain_ForwardAccumulation(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	// Each register-output carries BOTH ip AND token (output is shared per module) —
	// P0's register provision.ip and P1's register deliver.token are both available
	// to the finalizer.
	bootstrap := &capturingKeeperModule{output: map[string]any{"ip": "10.0.0.7", "token": "tok-abc"}}
	keepers := fakeKeeperRegistry{"core.bootstrap": bootstrap}
	gitURL := keeperForwardAccumServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

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

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	got := bootstrap.paramsForState("finalized")
	if got == nil {
		t.Fatal("P2 (finalized) Params == nil")
	}
	if got["from_p0"] != "10.0.0.7" {
		t.Errorf("from_p0 = %v, want '10.0.0.7' (register.provision.ip P0 не виден на P2)", got["from_p0"])
	}
	if got["from_p1"] != "tok-abc" {
		t.Errorf("from_p1 = %v, want 'tok-abc' (register.deliver.token P1 не виден на P2)", got["from_p1"])
	}
}

// TestIntegration_KeeperChain_FailPassage2_EarlyPassagesSucceed — #3: keeper-fail
// on the LAST link of a 3-Passage chain (P2), after two successful Passages.
// ASSERT: incarnation ERROR_LOCKED (reason keeper_dispatch_failed); keeper
// apply_runs: passage 0 = success, passage 1 = success, passage 2 = failed; state
// is NOT committed (commit only after the LAST Passage). Proves that aborting on
// the latest keeper-Passage doesn't lose the terminals of earlier successful
// Passages.
func TestIntegration_KeeperChain_FailPassage2_EarlyPassagesSucceed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	// A single core.bootstrap that fails on the LAST state (finalized = P2).
	bootstrap := &capturingKeeperModule{
		output:      map[string]any{"ip": "10.0.0.7"},
		echoParams:  []string{"target_ip"},
		failOnState: "finalized",
	}
	keepers := fakeKeeperRegistry{"core.bootstrap": bootstrap}
	gitURL := keeperChain3ServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

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

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "keeper_dispatch_failed" {
		t.Errorf("reason = %v, want keeper_dispatch_failed (keeper-fail Passage 2)", inc.StatusDetails)
	}
	if len(inc.State) != 0 {
		t.Errorf("incarnation.state = %v, want пустой (фейл до финального commit)", inc.State)
	}

	// Early Passages completed (P0 created, P1 delivered), P2 finalized failed at the end.
	if got := bootstrap.states(); len(got) != 3 || got[0] != "created" || got[1] != "delivered" || got[2] != "finalized" {
		t.Errorf("bootstrap states = %v, want [created delivered finalized] (ранние Passage успели, P2 дошёл до Apply и упал)", got)
	}

	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	keeperPassages := map[int]applyrun.Status{}
	for _, st := range statuses {
		if st.SID != render.KeeperTargetSID {
			t.Errorf("неожиданная не-keeper apply_runs-строка sid=%s passage=%d", st.SID, st.Passage)
			continue
		}
		keeperPassages[st.Passage] = st.Status
	}
	if keeperPassages[0] != applyrun.StatusSuccess || keeperPassages[1] != applyrun.StatusSuccess {
		t.Errorf("keeper passages 0/1 = %s/%s, want success/success (ранние Passage отработали ДО фейла P2)", keeperPassages[0], keeperPassages[1])
	}
	if keeperPassages[2] != applyrun.StatusFailed {
		t.Fatalf("★ keeper apply_runs[passage=2] = %s, want failed (keeper-fail на последнем звене записал failed-строку именно P2)", keeperPassages[2])
	}
}

// crossChannelServiceRepo — a Passage 1 keeper-task reading a HOST register (NOT
// a keeper register) in params. Structure:
//
//	#0 host probe (core.exec.run, register: hostprobe) → Passage 0 (host task)
//	#1 keeper read (core.bootstrap.read, params data=register.hostprobe.stdout) → Passage 1
//
// The keeper-task reads register.hostprobe.* — a HOST register emitted by the
// Passage 0 host task. It lives in RegisterByHost[<hostSID>], while keeperVars
// sees ONLY KeeperRegister (the keeper bucket). So at Passage 1 render the
// keeper-task gets no-such-key → render_failed → error_locked.
func crossChannelServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: keeper task reads HOST register (cross-channel, must fail-closed)
state_changes: {}
tasks:
  - name: host probe
    module: core.exec.run
    register: hostprobe
    params:
      cmd: echo
      args: ["ok"]
    changed_when: "false"
  - name: keeper reads host register
    module: core.bootstrap.read
    on: keeper
    params:
      data: "${ register.hostprobe.stdout }"
`)
}

// TestIntegration_KeeperChain_CrossChannel_FailClosed — ★ #6 (DATA SAFETY,
// fail-closed). A keeper-task (Passage 1) tries to read a HOST register
// (register.hostprobe.*, emitted by the Passage 0 host task) in params. The host
// register lives in per-host RegisterByHost[<hostSID>]; keeperVars reads ONLY the
// isolated KeeperRegister channel (keeper bucket). ASSERT: the keeper-task does
// NOT see the host register → CEL no-such-key → render_failed → incarnation
// ERROR_LOCKED; the keeper-task (P1) is NOT executed (bootstrap.Apply is not
// called — fails at render, before dispatch).
//
// Guard on channel isolation: if someone "fixes" the host-fallback so the host
// register leaks into keeperVars (keeperVars would see register.hostprobe), this
// test goes RED — the run reaches Ready instead of error_locked. Mirrors the unit
// test TestKeeperRegisterChannel_Isolated (render package), but through a full
// run()+PG.
func TestIntegration_KeeperChain_CrossChannel_FailClosed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	bootstrap := &capturingKeeperModule{output: map[string]any{"ok": true}}
	keepers := fakeKeeperRegistry{"core.bootstrap": bootstrap}
	gitURL := crossChannelServiceRepo(t)

	// host probe (Passage 0) terminates success — Passage 0 converges, the run
	// reaches the Passage 1 re-render, where the keeper-task fails at render.
	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

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

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	// render_failed — the keeper-task couldn't render params (host-register isn't
	// visible in keeperVars). Reason must be render_failed (not
	// keeper_dispatch_failed: the failure happens at render, before module execution).
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "render_failed" {
		t.Errorf("reason = %v, want render_failed (host-register не виден keeper-задаче на render Passage 1)", inc.StatusDetails)
	}

	// ★ The keeper-task is NOT executed — failure at render, before module dispatch.
	// If the host register had leaked into keeperVars, render would have passed and
	// bootstrap.Apply would have been called.
	if got := bootstrap.states(); len(got) != 0 {
		t.Fatalf("★ keeper-модуль states = %v, want пусто — keeper-задача читает HOST-register, должна упасть на render (no-such-key) ДО исполнения; непустой states ⇒ host-register протёк в keeperVars (канал НЕ изолирован)", got)
	}
}

// TestIntegration_KeeperChain_2Passage_RegisterChained — ★ END-TO-END PROOF OF THE
// EPIC (Slice 2). 2-Passage all-keeper chain: cloud.created (Passage 0) emits
// register provision{ip}, bootstrap.delivered (Passage 1) reads
// register.provision.ip in params. ASSERT: BOTH keeper-Passages executed, deliver
// got Params.target_ip == ip from provision (register forwarded end-to-end),
// apply_runs = 2 keeper rows (apply_id, keeper, 0) and (apply_id, keeper, 1),
// incarnation READY.
func TestIntegration_KeeperChain_2Passage_RegisterChained(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// No hosts seeded: all-keeper → no_hosts bypass (provision-from-zero).

	cloud := &capturingKeeperModule{output: map[string]any{"ip": "10.0.0.7"}}
	bootstrap := &capturingKeeperModule{output: map[string]any{"delivered": true}}
	keepers := fakeKeeperRegistry{
		"core.cloud":     cloud,
		"core.bootstrap": bootstrap,
	}
	gitURL := keeperChainServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

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

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// Both keeper states executed.
	if got := cloud.states(); len(got) != 1 || got[0] != "created" {
		t.Errorf("cloud states = %v, want [created]", got)
	}
	if got := bootstrap.states(); len(got) != 1 || got[0] != "delivered" {
		t.Errorf("bootstrap states = %v, want [delivered]", got)
	}

	// ★ register forwarded end-to-end: deliver got target_ip == ip from provision.
	got := bootstrap.params()
	if got == nil || got["target_ip"] != "10.0.0.7" {
		t.Fatalf("★ bootstrap.delivered Params.target_ip = %v, want '10.0.0.7' (register.provision.ip Passage 0 НЕ прокинут в keeper-задачу Passage 1)", got["target_ip"])
	}

	// No host-fan-out happened (all-keeper).
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (all-keeper, host-fan-out нет)", disp.calls)
	}

	// apply_runs = exactly 2 keeper rows (apply_id, keeper, 0) and (apply_id, keeper, 1),
	// both success. No host rows.
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	keeperPassages := map[int]applyrun.Status{}
	for _, st := range statuses {
		if st.SID != render.KeeperTargetSID {
			t.Errorf("неожиданная не-keeper apply_runs-строка sid=%s passage=%d", st.SID, st.Passage)
			continue
		}
		keeperPassages[st.Passage] = st.Status
	}
	if len(keeperPassages) != 2 {
		t.Fatalf("★ keeper apply_runs passages = %v, want ровно {0,1} (по строке на Passage с keeper-задачами)", keeperPassages)
	}
	for _, p := range []int{0, 1} {
		if keeperPassages[p] != applyrun.StatusSuccess {
			t.Errorf("keeper apply_runs[passage=%d] = %s, want success", p, keeperPassages[p])
		}
	}
}

// TestIntegration_KeeperChain_FailPassage1_ErrorLocked — ★ keeper-FAIL on Passage>0
// (Slice 2). cloud.created (Passage 0) succeeds → Passage 0 host-dispatch (none) →
// barrier 0 → bootstrap.delivered (Passage 1) FAILED. ASSERT: incarnation
// ERROR_LOCKED, reason keeper_dispatch_failed, state NOT committed; apply_runs:
// keeper passage 0 = success (ran before the failure), keeper passage 1 = failed;
// no host-dispatch at all (all-keeper). Proves abort on Passage>0 AFTER a
// successful earlier Passage plus observability (error_locked is correct, state is
// last-known-good).
func TestIntegration_KeeperChain_FailPassage1_ErrorLocked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	cloud := &capturingKeeperModule{output: map[string]any{"ip": "10.0.0.7"}}
	// bootstrap.delivered fails (failOnState="delivered").
	bootstrap := &capturingKeeperModule{failOnState: "delivered"}
	keepers := fakeKeeperRegistry{
		"core.cloud":     cloud,
		"core.bootstrap": bootstrap,
	}
	gitURL := keeperChainServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

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

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "keeper_dispatch_failed" {
		t.Errorf("reason = %v, want keeper_dispatch_failed (keeper-fail Passage 1)", inc.StatusDetails)
	}
	// state is NOT committed (commit only after the LAST Passage; Passage 1 fails before that).
	if len(inc.State) != 0 {
		t.Errorf("incarnation.state = %v, want пустой (keeper-fail Passage 1 НЕ коммитит state)", inc.State)
	}

	// Passage 0's keeper-task completed (cloud executed), Passage 1 failed before finishing.
	if got := cloud.states(); len(got) != 1 {
		t.Errorf("cloud states = %v, want [created] (Passage 0 успел до фейла Passage 1)", got)
	}

	// apply_runs: keeper passage 0 = success, keeper passage 1 = failed. No host rows.
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	keeperPassages := map[int]applyrun.Status{}
	for _, st := range statuses {
		if st.SID != render.KeeperTargetSID {
			t.Errorf("неожиданная не-keeper apply_runs-строка sid=%s passage=%d (host-dispatch не должен был стартовать)", st.SID, st.Passage)
			continue
		}
		keeperPassages[st.Passage] = st.Status
	}
	if keeperPassages[0] != applyrun.StatusSuccess {
		t.Errorf("keeper apply_runs[passage=0] = %s, want success (отработал ДО фейла Passage 1)", keeperPassages[0])
	}
	if keeperPassages[1] != applyrun.StatusFailed {
		t.Fatalf("★ keeper apply_runs[passage=1] = %s, want failed (keeper-fail Passage 1 записал failed-строку)", keeperPassages[1])
	}
}

// mixedKeeperHostPassage0Repo — a keeper-task plus a host-task in ONE Passage 0
// (mixed keeper-passage-0 + host-passage-0). No register dependency between them →
// Stratify gives both Passage 0 (N=1). The host-task makes the roster non-empty
// (no_hosts doesn't kick in; the keeper-task executes first, host-fan-out follows).
func mixedKeeperHostPassage0Repo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: mixed keeper + host in Passage 0
state_changes: {}
tasks:
  - name: vault read
    module: core.vault.kv-read
    on: keeper
    register: secret
    params:
      path: secret/data/db
  - name: host echo
    module: core.exec.run
    params:
      cmd: echo
      args: ["hi"]
    changed_when: "false"
`)
}

// TestIntegration_MixedKeeperHostPassage0 — ★ MIXED keeper+host Passage 0 (Slice 2).
// A keeper-task (core.vault.kv-read, on: keeper) plus a host-task (core.exec.run)
// in ONE Passage 0 (no register dependency → N=1). ASSERT: the keeper-task
// executes BEFORE host-dispatch (host-fan-out started — 1 SendApply), the Passage
// 0 barrier is NOT inflated by the keeper row (classify skips the keeper-target →
// doesn't hang waiting on an "extra" terminal), incarnation READY. apply_runs:
// keeper passage 0 + host passage 0.
func TestIntegration_MixedKeeperHostPassage0(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	vault := &capturingKeeperModule{output: map[string]any{"value": "s3cr3t"}}
	keepers := fakeKeeperRegistry{"core.vault": vault}
	gitURL := mixedKeeperHostPassage0Repo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

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

	// The run does NOT hang (if classify treated the keeper row as a host terminal
	// and inflated the terminal count, or conversely waited on the keeper row as a
	// host — the barrier would hang until RunTimeout and waitRunDone would fail).
	// READY = the barrier converged exactly on Passage 0's host rows.
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// keeper-task executed.
	if got := vault.states(); len(got) != 1 || got[0] != "kv-read" {
		t.Errorf("vault states = %v, want [kv-read]", got)
	}
	// host-fan-out started (the host-task went out to one host).
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (host echo на одном хосте)", disp.calls)
	}

	// apply_runs: keeper passage 0 (success) + host passage 0 (success).
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	var keeperOK, hostOK bool
	for _, st := range statuses {
		if st.Passage != 0 {
			t.Errorf("apply_runs[%s] passage = %d, want 0 (N=1, всё в Passage 0)", st.SID, st.Passage)
		}
		switch st.SID {
		case render.KeeperTargetSID:
			keeperOK = st.Status == applyrun.StatusSuccess
		case "host-a.example.com":
			hostOK = st.Status == applyrun.StatusSuccess
		}
	}
	if !keeperOK {
		t.Errorf("нет keeper apply_runs success passage 0: %+v", statuses)
	}
	if !hostOK {
		t.Errorf("нет host apply_runs success passage 0: %+v", statuses)
	}
}

// --- Slice-2 unit guard over Stratify for keeper chains (no PG) -----------------

// TestStratify_KeeperChain_TwoPassages — a keeper→keeper chain stratifies by the
// register dependency in params: provision (register: provision) → Passage 0,
// deliver (params reads register.provision) → Passage 1. Proves that Stratify sees
// the register edge through a keeper-task's params (keeper-tasks have no where:,
// the edge goes THROUGH params) — the foundation of per-passage keeper-dispatch.
func TestStratify_KeeperChain_TwoPassages(t *testing.T) {
	scn, _, diags, err := config.LoadScenarioManifestFromBytes("main.yml", []byte(`name: create
description: keeper chain stratify
state_changes: {}
tasks:
  - name: provision
    module: core.cloud.created
    on: keeper
    register: provision
    params:
      provider: fake
  - name: deliver
    module: core.bootstrap.delivered
    on: keeper
    params:
      target_ip: "${ register.provision.ip }"
`), config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("parse diags: %v", diags)
	}

	passage, perr := render.Stratify(scn.Tasks)
	if perr != nil {
		t.Fatalf("Stratify: %v", perr)
	}
	if passage.Count != 2 {
		t.Fatalf("passage.Count = %d, want 2 (provision P0, deliver P1)", passage.Count)
	}
	if passage.TaskPassage[0] != 0 || passage.TaskPassage[1] != 1 {
		t.Fatalf("TaskPassage = %v, want [0 1] (deliver читает register.provision в params)", passage.TaskPassage)
	}
}
