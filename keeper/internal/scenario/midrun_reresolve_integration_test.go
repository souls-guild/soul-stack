//go:build integration

// Mid-run re-resolve roster — live proof of S3 (ADR-0061). Target invariant:
// after a `core.soul.registered` step with `refresh_soulprint: true` succeeds
// (Passage 0), scenario-runner RE-resolves the roster BEFORE the next Passage,
// and newly created+onboarded hosts become visible to Passage-1 tasks via
// soulprint.hosts / on:[incarnation.name]. Roster growth is emulated by a
// keeper-module callback: it seeds a new connected soul WHILE keeper-dispatch
// runs (Passage 0), as onboarding would bring up a new VM. Re-resolve before
// Passage 1 reads the grown SQL roster (Topology=NewResolver(pool, nil) →
// SQL presence by status='connected').
//
// ★ S3 invariant: within Passage 0 the roster is unchanged (re-resolve happens
// only at the refresh boundary); Passage 1 sees the grown set. Assert topology
// after refresh also sees the growth (ADR-0061 §determinism).

package scenario

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// seedingKeeperModule — keeper-side core module `core.soul` that, on Apply,
// calls onApply (seeds a new host = emulates onboarding a created VM), then
// returns a success output echoing refreshed (like the real registered.go).
// Simulates the provision→onboarding bridge: by the Passage 0 barrier, the new
// host is already in souls.
type seedingKeeperModule struct {
	module.BaseModule
	onApply func()
}

func (m *seedingKeeperModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if m.onApply != nil {
		m.onApply()
	}
	out := mustStructAny(map[string]any{"refreshed": true, "created": true})
	return stream.Send(&pluginv1.ApplyEvent{Changed: true, Output: out})
}

// rosterTargetDispatcher is a lightweight Soul simulator for re-resolve tests:
// on every SendApply it records (passage → SID) and terminates the apply_runs
// row as success (like correlateRunResult). No register logic — these fixtures'
// Passage 1 doesn't read register (targeting is by roster, not by probe).
type rosterTargetDispatcher struct {
	t  *testing.T
	mu sync.Mutex
	by map[int][]string
}

func newRosterTargetDispatcher(t *testing.T) *rosterTargetDispatcher {
	return &rosterTargetDispatcher{t: t, by: map[int][]string{}}
}

func (d *rosterTargetDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	d.mu.Lock()
	d.by[passage] = append(d.by[passage], sid)
	d.mu.Unlock()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("rosterTargetDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

func (d *rosterTargetDispatcher) targets(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string(nil), d.by[passage]...)
	return out
}

// newRunnerKeeperStaged builds a Runner with a keeper-side Registry AND
// stubPassageCap (the staged gate S5 requires passage-capability). A combination
// not covered by existing constructors: re-resolve tests carry BOTH a keeper
// task (refresh emitter) AND staged stratification (Count=2 at the refresh
// boundary).
func newRunnerKeeperStaged(t *testing.T, disp ApplyDispatcher, keepers KeeperModuleRegistry) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:        artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:      topology.NewResolver(integrationPool, nil, nil),
		Essence:       essence.NewResolver(nil),
		Render:        render.NewPipeline(nil, engine, nil, nil),
		Outbound:      disp,
		KeeperModules: keepers,
		DB:            integrationPool,
		PassageCap:    stubPassageCap{},
		PollInterval:  20 * time.Millisecond,
		RunTimeout:    20 * time.Second,
	})
}

// refreshServiceRepo — a service repo with scenario `grow`: Passage 0 is the
// keeper step core.soul.registered with refresh_soulprint:true (register:
// roster); Passage 1 is a soul task core.exec.run on on:[incarnation.name] (a
// role applied to the whole grown roster). S2 forces the Passage-1 task strictly
// AFTER the refresh step (refresh boundary).
func refreshServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: grow
description: provision-refresh-role single run (ADR-0061 §S3)
state_changes: {}
tasks:
  - name: Register and refresh roster
    module: core.soul.registered
    on: keeper
    register: roster
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply role to grown incarnation roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: "false"
    params:
      cmd: echo
      args: ["role"]
`)
}

// TestIntegration_MidRunReResolve_GrownRosterVisibleNextPassage — ★ S3 PROOF
// (ADR-0061). Starting roster is host-a (1 host). The refresh step (Passage 0)
// seeds host-c (emulates onboarding a created VM). ASSERT: Passage 1
// (on:[incarnation.name]) targeted BOTH hosts (host-a + host-c) — re-resolve
// before Passage 1 saw the grown roster. Passage 0 (keeper step) targets no
// hosts (on: keeper). Within Passage 0 the roster was host-a (host-c appeared
// only before Passage 1).
func TestIntegration_MidRunReResolve_GrownRosterVisibleNextPassage(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})

	// keeper module seeds host-c on Apply (= onboarding created a new VM, online
	// by the Passage 0 barrier). Re-resolve before Passage 1 will see it.
	var seedOnce sync.Once
	mod := &seedingKeeperModule{onApply: func() {
		seedOnce.Do(func() {
			seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
		})
	}}
	keepers := fakeKeeperRegistry{"core.soul": mod}

	disp := newRosterTargetDispatcher(t)
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: refreshServiceRepo(t), Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// ★ Passage 1 (the role) targeted BOTH hosts — re-resolve saw host-c.
	p1 := disp.targets(1)
	gotSet := map[string]bool{}
	for _, sid := range p1 {
		gotSet[sid] = true
	}
	if !gotSet["host-a.example.com"] || !gotSet["host-c.example.com"] || len(p1) != 2 {
		t.Fatalf("* Passage 1 targets = %v, want both [host-a host-c] - re-resolve did NOT see the grown roster (host-c onboarded at refresh boundary)", p1)
	}

	// apply_runs: host-a + host-c both have a Passage-1 success row. The keeper
	// row (sid=keeper, passage 0) is the refresh step. host-c has NO Passage-0
	// row (it appeared only before Passage 1 — it wasn't in the Passage 0 roster).
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	if len(got["host-c.example.com"]) != 1 || got["host-c.example.com"][0] != 1 {
		t.Errorf("host-c passages = %v, want [1] (appeared at refresh boundary, was NOT in Passage 0)", got["host-c.example.com"])
	}
	hostAHasP1 := false
	for _, p := range got["host-a.example.com"] {
		if p == 1 {
			hostAHasP1 = true
		}
	}
	if !hostAHasP1 {
		t.Errorf("host-a passages = %v, want containing 1 (role applied to grown roster)", got["host-a.example.com"])
	}
}

// TestIntegration_MidRunReResolve_NoGrowthSameRoster — CONTROL: a refresh step
// exists, but onboarding produced no new hosts (the live snapshot didn't
// change). Re-resolve runs but returns the same set — Passage 1 targets the
// original roster, and the run succeeds. Proves that re-resolve at the refresh
// boundary is safe even without changes (a live snapshot of an unchanged
// online set = the same roster).
func TestIntegration_MidRunReResolve_NoGrowthSameRoster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})

	// the refresh step seeds nothing (onboarding added no hosts).
	mod := &seedingKeeperModule{onApply: func() {}}
	keepers := fakeKeeperRegistry{"core.soul": mod}

	disp := newRosterTargetDispatcher(t)
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: refreshServiceRepo(t), Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	p1 := disp.targets(1)
	if len(p1) != 2 {
		t.Fatalf("Passage 1 targets = %v, want both original hosts (re-resolve with no growth = same roster)", p1)
	}
}

// TestIntegration_MidRunReResolve_AssertSeesGrownRoster — ★ assert topology
// AFTER refresh sees the grown roster (ADR-0061 §determinism). Scenario:
// refresh step (seed host-c) → assert size(soulprint.hosts) == 2. assert is
// evaluated Keeper-side while rendering Passage 1 AFTER re-resolve — it must see
// the grown set (2 hosts). If re-resolve hadn't worked, assert would fail (1
// host) → error_locked.
func TestIntegration_MidRunReResolve_AssertSeesGrownRoster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})

	var seedOnce sync.Once
	mod := &seedingKeeperModule{onApply: func() {
		seedOnce.Do(func() {
			seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
		})
	}}
	keepers := fakeKeeperRegistry{"core.soul": mod}

	gitURL := writeServiceRepo(t, `name: grow_assert
description: refresh then assert grown topology
state_changes: {}
tasks:
  - name: Register and refresh roster
    module: core.soul.registered
    on: keeper
    register: roster
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Assert grown topology
    assert:
      that:
        - "size(soulprint.hosts) == 2"
      message: "expected 2 hosts after onboarding"
`)

	disp := newRosterTargetDispatcher(t)
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// assert passed (2 hosts after re-resolve) → run succeeded. If re-resolve
	// hadn't worked, assert would have seen 1 host → false → error_locked.
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
}

// TestIntegration_MidRunReResolve_OfflineHostExcludedNextPassage — ★ live-snapshot
// CONTRACT (ADR-0061 §S3): re-resolve at the refresh boundary is NOT a
// monotonic growth, but a FRESH live snapshot of the current online set. A
// P0-roster host that goes OFFLINE by the refresh boundary is EXCLUDED from the
// P1 roster — targeting follows the actually-online set (no point rolling a
// role onto an offline host). Documents the CORRECT semantics (not a
// regression): mirrors GrownRosterVisibleNextPassage, but in reverse (a host
// leaves rather than arrives).
//
// Seed: host-a + host-b online in P0. The refresh step (Passage 0) moves host-b
// to status=disconnected (emulating: host-b went down by the refresh boundary —
// lost its EventStream/lease). Re-resolve before Passage 1 reads a live snapshot
// (filterAlive → status snapshot, lease==nil in the unit resolver) → sees only
// host-a. ASSERT: Passage 1 targeted ONLY host-a; host-b did NOT make it into P1.
func TestIntegration_MidRunReResolve_OfflineHostExcludedNextPassage(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})

	// the refresh step drops host-b offline WHILE keeper-dispatch Passage 0 runs
	// (before re-resolve at the Passage 1 boundary). re-resolve reads a live
	// snapshot → host-b is no longer online → excluded from the P1 roster.
	var dropOnce sync.Once
	mod := &seedingKeeperModule{onApply: func() {
		dropOnce.Do(func() {
			if err := soul.UpdateStatus(context.Background(), integrationPool,
				"host-b.example.com", soul.StatusDisconnected, nil); err != nil {
				t.Errorf("UpdateStatus(host-b → disconnected): %v", err)
			}
		})
	}}
	keepers := fakeKeeperRegistry{"core.soul": mod}

	disp := newRosterTargetDispatcher(t)
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: refreshServiceRepo(t), Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// ★ Passage 1 (the role) targeted ONLY host-a — the re-resolve live snapshot
	// excluded the departed offline host-b (NOT a monotonic growth: the set can
	// SHRINK).
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("* Passage 1 targets = %v, want [host-a] - live-snapshot re-resolve must EXCLUDE the offline host-b (ADR-0061 SS3: re-resolve = live-snapshot, not monotonic growth)", p1)
	}
}

// newRunnerKeeperStagedTimeout — like [newRunnerKeeperStaged], but with a
// controllable RunTimeout (micro-base) and MaxAwaitTimeoutFn (controllable
// ceiling). Needed by the timeout guard below: the micro-base makes the abort
// deterministic within seconds, and ceilingFn sets provision-eff so it reliably
// exceeds the keeper-mock's blocking time (without the prod ceiling's 30m
// default, the test would be either slow or nondeterministic).
func newRunnerKeeperStagedTimeout(t *testing.T, disp ApplyDispatcher, keepers KeeperModuleRegistry, base time.Duration, ceiling time.Duration) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:            artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:          topology.NewResolver(integrationPool, nil, nil),
		Essence:           essence.NewResolver(nil),
		Render:            render.NewPipeline(nil, engine, nil, nil),
		Outbound:          disp,
		KeeperModules:     keepers,
		DB:                integrationPool,
		PassageCap:        stubPassageCap{},
		PollInterval:      20 * time.Millisecond,
		RunTimeout:        base,
		MaxAwaitTimeoutFn: func() time.Duration { return ceiling },
	})
}

// TestIntegration_ProvisionRun_NotCutByBaseTimeout — ★ MAIN GUARD for this bug
// (provision-aware run-timeout, ADR-0061). REGRESSION TRAP: a refresh-emitter run
// (provision-from-zero) with a MICRO-base run-timeout (100ms) does NOT abort when
// the keeper step blocks longer than the base (250ms). Proves that run() applies
// a provision-aware effectiveRunTimeout (ceiling+deployBudget) to the run, NOT the
// raw runTimeout. BEFORE the fix, defaultRunTimeout was applied in Start to the
// whole run → this keeper step would exceed the base → abort (error_locked)
// mid-onboarding — exactly the live bug (joinWait/await_online unreachable under
// a 5m runCtx).
//
// ceilingFn=()→1s → eff = 1s + deployBudget(10m) ≫ the 250ms block: the run
// survives and reaches Ready. The negative half ("non-provision is cut by base")
// is covered by the existing TestIntegration_NoClaim_BarrierTimeout /
// TestIntegration_FromLocked_* (short RunTimeout, plan without a refresh emitter
// → eff=base → honest timeout).
func TestIntegration_ProvisionRun_NotCutByBaseTimeout(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})

	// the keeper step (refresh emitter) blocks 250ms — longer than the 100ms
	// micro-base. Under the raw runTimeout the run would abort here; under
	// provision-eff (1s+10m) it survives.
	mod := &seedingKeeperModule{onApply: func() {
		time.Sleep(250 * time.Millisecond)
	}}
	keepers := fakeKeeperRegistry{"core.soul": mod}

	disp := newRosterTargetDispatcher(t)
	r := newRunnerKeeperStagedTimeout(t, disp, keepers, 100*time.Millisecond, 1*time.Second)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: refreshServiceRepo(t), Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Reaches Ready (didn't abort on the micro-base) — provision-eff applied.
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
}
