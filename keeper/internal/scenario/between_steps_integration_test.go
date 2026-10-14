//go:build integration

package scenario

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
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
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// A run between two steps (NIM-972, ADR-027 amend (r)). After step k's barrier
// and before step k+1's rows every row of the run is terminal, and before its
// first row it has none, so a reader judging the run by its rows took it for
// finished. These guards hold a linear run in such a window, ask each reader
// there, and send the Cancel from an instance that does not own the run.

// windowGate holds the run at the first call it guards until the test lets it go.
type windowGate struct {
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func newWindowGate() *windowGate {
	return &windowGate{reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *windowGate) hold(ctx context.Context) {
	g.once.Do(func() {
		close(g.reached)
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	})
}

func (g *windowGate) await(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the window")
	}
}

// betweenStepsReader is the audit reader a run asks right before it sends step 1
// to the hosts: step 0's barrier has passed and step 1 has written no row.
type betweenStepsReader struct {
	ChangedTaskReader
	gate *windowGate
}

func (r betweenStepsReader) SelectChangedTaskKeys(ctx context.Context, applyID string) (map[auditpg.ChangedTaskKey]struct{}, error) {
	r.gate.hold(ctx)
	return r.ChangedTaskReader.SelectChangedTaskKeys(ctx, applyID)
}

// beforeFirstRowCap is the presence checker a run of several steps asks before
// it renders and writes anything: the lock is held, no row exists.
type beforeFirstRowCap struct {
	stubSoulCap
	gate *windowGate
}

func (c beforeFirstRowCap) SoulsLackingCapability(ctx context.Context, sids []string, capability string) ([]string, error) {
	c.gate.hold(ctx)
	return c.stubSoulCap.SoulsLackingCapability(ctx, sids, capability)
}

func newRunnerInWindow(t *testing.T, disp ApplyDispatcher, reader ChangedTaskReader, soulCap SoulCapabilityChecker, keepers KeeperModuleRegistry) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:        artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:      topology.NewResolver(integrationPool, nil, nil),
		ServiceVars:   servicevars.NewResolver(nil),
		Render:        render.NewPipeline(nil, engine, nil, nil),
		Outbound:      disp,
		DB:            integrationPool,
		Audit:         auditpg.NewWriter(integrationPool),
		AuditReader:   reader,
		KeeperModules: keepers,
		SoulCap:       soulCap,
		PollInterval:  20 * time.Millisecond,
		RunTimeout:    20 * time.Second,
	})
}

const twoHostSteps = `name: create
description: two steps on the hosts, held between them
tasks:
  - name: first
    module: core.exec.run
    changed_when: "false"
    params: { cmd: first }
  - name: second
    module: core.exec.run
    changed_when: "false"
    params: { cmd: second }
`

// holdBetweenSteps starts a linear run of [twoHostSteps] and returns once it sits
// between the steps, after checking it really does: step 0 terminal, step 1
// without a row.
func holdBetweenSteps(t *testing.T) (applyID string, gate *windowGate, disp *orderedStepDispatcher, gitURL string) {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL = writeServiceRepo(t, twoHostSteps)
	gate = newWindowGate()
	disp = &orderedStepDispatcher{t: t}
	reader := betweenStepsReader{ChangedTaskReader: auditpg.NewReader(integrationPool), gate: gate}
	owner := newRunnerInWindow(t, disp, reader, stubSoulCap{}, nil)
	applyID = startModelRun(t, owner, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
	t.Cleanup(func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})
	gate.await(t)

	rows, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(rows) != 1 || rows[0].Passage != 0 || rows[0].Status != applyrun.StatusSuccess {
		t.Fatalf("the run is not between its steps: rows %+v, want one success row of step 0", rows)
	}
	return applyID, gate, disp, gitURL
}

// ★ GUARD (NIM-972, the Cancel). Another instance cancels the run between its
// steps, when no row of it can carry the flag. The Cancel is found, step 1 never
// reaches the host, and the run reads cancelled once it is over.
//
// Mutations: RequestCancel without the lock flag → found=false, step 1 sent, the
// run ends ready; the owner reading rows only → step 1 sent, ready; no sentinel
// after an abort between steps → the run reads success.
func TestIntegration_BetweenStepsCancelFromAnotherInstance(t *testing.T) {
	applyID, gate, disp, gitURL := holdBetweenSteps(t)

	found, err := newRunner(t, disp, gitURL).RequestCancel(context.Background(), applyID)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if !found {
		t.Error("a Cancel from another instance between the steps found nothing to flag")
	}
	close(gate.release)

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (a cluster-wide Cancel)", inc.StatusDetails["reason"])
	}
	if sent := sends(disp.snapshot())[1]; len(sent) != 0 {
		t.Errorf("step 1 was sent after the Cancel: %v", sent)
	}
	runs, _, err := applyrun.ListRunsByIncarnation(context.Background(), integrationPool, "noop-prod", 0, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRunsByIncarnation = %+v, %v; want one run", runs, err)
	}
	if runs[0].Status != applyrun.RunStatusCancelled || runs[0].FinishedAt == nil {
		t.Errorf("after the Cancel the run reads %q, finished_at %v; want cancelled and finished", runs[0].Status, runs[0].FinishedAt)
	}
}

// ★ GUARD (NIM-972). An abort between steps writes the run's sentinel row before
// it releases the lock. Released first, the run read success — every row it had
// was — until the sentinel landed, and for good if the insert failed; the Voyage
// awaiter polling in between took the run as succeeded.
//
// The test holds apply_runs against writes in an open transaction, so the run's
// sentinel insert waits on it, and looks at the lock meanwhile. A table lock and
// not a row of the sentinel's key: inserting that row would take KEY SHARE on the
// incarnation through the foreign key, and lockIncarnation's SELECT … FOR UPDATE
// would wait on it in either order.
//
// Mutation: release the lock (lockIncarnation) before ensureTerminalApplyRun →
// the incarnation leaves applying while the sentinel is not written.
func TestIntegration_AbortBetweenStepsWritesTheSentinelBeforeTheLockGoes(t *testing.T) {
	applyID, gate, disp, gitURL := holdBetweenSteps(t)
	ctx := context.Background()

	if found, err := newRunner(t, disp, gitURL).RequestCancel(ctx, applyID); err != nil || !found {
		t.Fatalf("RequestCancel = %v, %v; want found", found, err)
	}
	tx, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE apply_runs IN SHARE MODE`); err != nil {
		t.Fatalf("hold apply_runs against writes: %v", err)
	}
	close(gate.release)

	time.Sleep(500 * time.Millisecond)
	inc, err := incarnation.SelectByID(ctx, integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByID: %v", err)
	}
	if inc.Status != incarnation.StatusApplying {
		t.Errorf("the lock was released (status %q) before the run's sentinel was written", inc.Status)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if status, err := applyrun.SelectRunStatus(ctx, integrationPool, applyID); err != nil || status != applyrun.RunStatusCancelled {
		t.Errorf("SelectRunStatus = %q, %v; want cancelled", status, err)
	}
}

// ★ GUARD (NIM-972, GATE-1(f)). A one-step run through the work-queue waits on
// a `dispatched` row — delivered, its Soul applying it — when another instance
// cancels. RequestCancel does not flag a dispatched row, so only the lock
// carries that Cancel, and the barrier defers it while a row is dispatched and
// none is planned or claimed: acting there, the abort would close the row as
// cancelled while the Soul runs on, and drop its result. Once the result is in
// and no row is dispatched, the next poll acts on it — the row keeps its honest
// result, and the run stops instead of committing.
//
// Mutation: the barrier acts on the lock's flag whatever its rows → the run
// stops while the row is dispatched, the row is closed cancelled, the Soul's
// result is a no-op.
func TestIntegration_CancelLeavesADispatchedRowItsResult(t *testing.T) {
	const host = "host-a.example.com"
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, host, []string{"noop-prod"})
	gitURL := writeServiceRepo(t, `name: create
description: one host step through the work-queue
tasks:
  - name: host work
    module: core.exec.run
    changed_when: "false"
    params: { cmd: work }
`)
	ctx := context.Background()
	owner := newQueueRunnerInWindow(t, stubSoulCap{}, nil)
	applyID := startModelRun(t, owner, ExecutionStaged, "noop-prod", "noop", gitURL, "create")
	waitHostRow(t, applyID, host, applyrun.StatusPlanned)
	// An Acolyte claimed the row and handed it to the Soul.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE apply_runs SET status = 'dispatched' WHERE apply_id = $1 AND sid = $2`, applyID, host); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}

	if found, err := newQueueRunnerInWindow(t, stubSoulCap{}, nil).RequestCancel(ctx, applyID); err != nil || !found {
		t.Fatalf("RequestCancel = %v, %v; want found", found, err)
	}
	time.Sleep(300 * time.Millisecond) // many barrier polls at 20ms
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
	if status, err := applyrun.SelectRunStatus(ctx, integrationPool, applyID); err != nil || status != applyrun.RunStatusCancelled {
		t.Errorf("SelectRunStatus = %q, %v; want cancelled", status, err)
	}
}

// ★ GUARD (NIM-972). The lock's Cancel is deferred only while a row is
// dispatched and none is planned or claimed. A Cancel that lands while the
// work-queue path writes its rows leaves them unflagged; once one is
// dispatched, deferring would let an Acolyte claim and send the rest after a
// Cancel already reported found. With a row still planned the barrier acts at
// once, closing the rows as a row flag always did.
//
// Mutation: defer the lock's flag while any row is dispatched → the planned row
// stays open and the run waits on it.
func TestIntegration_LockCancelActsWhileARowIsUndelivered(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, hostA, []string{"noop-prod"})
	seedConnectedSoul(t, hostB, []string{"noop-prod"})
	gitURL := writeServiceRepo(t, `name: create
description: one host step through the work-queue, two hosts
tasks:
  - name: host work
    module: core.exec.run
    changed_when: "false"
    params: { cmd: work }
`)
	ctx := context.Background()
	owner := newQueueRunnerInWindow(t, stubSoulCap{}, nil)
	applyID := startModelRun(t, owner, ExecutionStaged, "noop-prod", "noop", gitURL, "create")
	waitHostRow(t, applyID, hostA, applyrun.StatusPlanned)
	waitHostRow(t, applyID, hostB, applyrun.StatusPlanned)
	if _, err := integrationPool.Exec(ctx,
		`UPDATE apply_runs SET status = 'dispatched' WHERE apply_id = $1 AND sid = $2`, applyID, hostA); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}

	if locked, err := incarnation.RequestRunCancel(ctx, integrationPool, applyID); err != nil || !locked {
		t.Fatalf("RequestRunCancel = %v, %v; want the lock flagged", locked, err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	for _, r := range hostRows(t, applyID) {
		if r.Status != applyrun.StatusCancelled {
			t.Errorf("host %s row = %q, want cancelled", r.SID, r.Status)
		}
	}
}

// ★ GUARD (NIM-972). With no row dispatched the barrier does act on the lock's
// flag. The lock alone carries this Cancel, as it does when the Cancel lands
// before a step's rows are written; the host's row is `running`, inline.
//
// Mutation: the barrier reads the row flag only → the run waits on the held
// host and does not stop within waitRunDone's deadline.
func TestIntegration_BarrierActsOnTheLockCancel(t *testing.T) {
	const host = "host-a.example.com"
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, host, []string{"noop-prod"})
	gitURL := writeServiceRepo(t, `name: create
description: one host step, inline
tasks:
  - name: host work
    module: core.exec.run
    changed_when: "false"
    params: { cmd: work }
`)
	ctx := context.Background()
	disp := newHeldDispatcher(t)
	owner := newRunnerInWindow(t, disp, auditpg.NewReader(integrationPool), stubSoulCap{}, nil)
	applyID := startModelRun(t, owner, ExecutionStaged, "noop-prod", "noop", gitURL, "create")
	waitHostRow(t, applyID, host, applyrun.StatusRunning)

	if locked, err := incarnation.RequestRunCancel(ctx, integrationPool, applyID); err != nil || !locked {
		t.Fatalf("RequestRunCancel = %v, %v; want the lock flagged", locked, err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if rows := hostRows(t, applyID); len(rows) != 1 || rows[0].Status != applyrun.StatusRunning {
		t.Errorf("host rows when the run stopped = %+v, want the live apply left running", rows)
	}
}

// heldDispatcher answers each ApplyRequest with success only when the test is
// done with it — at cleanup, which also runs after a Fatal — and waits for the
// write there, so no result lands after the test has returned and the next
// test's resetAll has emptied apply_runs. A request that arrives after cleanup
// began, from a run the failed test left behind, is not answered at all.
type heldDispatcher struct {
	t       *testing.T
	release chan struct{}
	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
}

func newHeldDispatcher(t *testing.T) *heldDispatcher {
	d := &heldDispatcher{t: t, release: make(chan struct{})}
	t.Cleanup(func() {
		d.mu.Lock()
		d.closed = true
		close(d.release)
		d.mu.Unlock()
		d.wg.Wait()
	})
	return d
}

func (d *heldDispatcher) SendApply(_ context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		<-d.release
		if err := applyrun.UpdateStatus(context.Background(), integrationPool, req.GetApplyId(), sid, int(req.GetPassage()), applyrun.StatusSuccess, nil); err != nil {
			d.t.Errorf("heldDispatcher: UpdateStatus(%s): %v", sid, err)
		}
	}()
	return nil
}

// waitHostRow waits until the run's row on sid has the given status.
func waitHostRow(t *testing.T, applyID, sid string, want applyrun.Status) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, r := range hostRows(t, applyID) {
			if r.SID == sid && r.Status == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run's row on %s never reached %s", sid, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ★ GUARD (NIM-972 over NIM-961). A linear run whose steps go through the
// work-queue: step 0 was claimed and run by another instance's Acolyte, and the
// run sits between its steps when a third instance cancels it. Step 1's rows are
// never written, so no Acolyte can claim and send them.
//
// Mutation: drop the Cancel check before a step's hosts are sent → step 1's
// rows are written after the Cancel (the abort then closes them).
func TestIntegration_BetweenStepsCancelStopsAQueuedLinearStep(t *testing.T) {
	const host = "host-a.example.com"
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, host, []string{"noop-prod"})
	gitURL := writeServiceRepo(t, twoHostSteps)
	gate := newWindowGate()
	t.Cleanup(func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})
	souls := newQueueSouls(t, "", nil)
	owner := newLinearQueueRunner(t, souls)
	owner.deps.AuditReader = betweenStepsReader{ChangedTaskReader: auditpg.NewReader(integrationPool), gate: gate}
	runAcolyte(t, souls, "keeper-2")
	applyID := startModelRun(t, owner, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
	gate.await(t)

	step0 := stepRows(t, applyID)[0][host]
	if step0.kid != "keeper-2" {
		t.Fatalf("step 0 was claimed by %q, want keeper-2 - the run did not go through the queue", step0.kid)
	}
	found, err := newRunner(t, souls, gitURL).RequestCancel(context.Background(), applyID)
	if err != nil || !found {
		t.Fatalf("RequestCancel = %v, %v; want found", found, err)
	}
	close(gate.release)

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	rows := stepRows(t, applyID)[1]
	// The run's sentinel names the step it stopped at (NIM-1000); it is not a row
	// any Acolyte could claim.
	delete(rows, render.RunSentinelSID)
	if len(rows) != 0 {
		t.Errorf("step 1 rows after the Cancel: %+v", rows)
	}
	if sent := souls.messages()[1]; len(sent) != 0 {
		t.Errorf("step 1 was sent after the Cancel: %v", sent)
	}
}

// ★ GUARD (NIM-972, the Cancel before the first row). The run holds its lock and
// has written nothing; its first step is a keeper step. A Cancel from another
// instance there stops the run before the keeper step runs, not after it.
//
// Mutations: RequestCancel without the lock flag → found=false and everything
// runs; drop the Cancel check before a step's keeper tasks → the keeper step runs.
func TestIntegration_BeforeFirstRowCancelStopsTheKeeperStep(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, `name: create
description: a keeper step first, then the hosts
tasks:
  - name: keeper work
    module: fakekeeper.order.recorded
    on: keeper
    params: {}
  - name: host work
    module: core.exec.run
    changed_when: "false"
    params: { cmd: work }
`)
	gate := newWindowGate()
	t.Cleanup(func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})
	keeperStep := &orderedKeeperModule{}
	disp := &orderedStepDispatcher{t: t}
	owner := newRunnerInWindow(t, disp, auditpg.NewReader(integrationPool), beforeFirstRowCap{gate: gate},
		keeperRegistryWith(map[string]module.SoulModule{"fakekeeper.order": keeperStep}))
	applyID := startModelRun(t, owner, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
	gate.await(t)

	rows, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("the run has rows already: %+v", rows)
	}
	found, err := newRunner(t, disp, gitURL).RequestCancel(context.Background(), applyID)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if !found {
		t.Error("a Cancel from another instance before the first row found nothing to flag")
	}
	close(gate.release)

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if n := keeperStep.applyCount(); n != 0 {
		t.Errorf("the keeper step ran %d time(s) after the Cancel", n)
	}
	if got := disp.snapshot(); len(got) != 0 {
		t.Errorf("hosts were sent %+v after the Cancel", got)
	}
}

// ★ GUARD (NIM-972). The flag means something only while the lock names a run, and
// it is reset when the lock is taken, not when it is released: a Keeper older than
// migration 121 releases the lock without knowing the column. A request left
// there must not cancel the next run.
//
// Mutation: drop the reset from lockApplyingWithEpochSQL → the next run is
// cancelled at its first step.
func TestIntegration_StaleRunCancelFlagDoesNotReachTheNextRun(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE incarnation SET applying_cancel_requested = true WHERE id = $1`, "noop-prod"); err != nil {
		t.Fatalf("leave a stale flag: %v", err)
	}
	gitURL := writeServiceRepo(t, twoHostSteps)
	disp := &orderedStepDispatcher{t: t}
	r := newRunnerInWindow(t, disp, auditpg.NewReader(integrationPool), stubSoulCap{}, nil)
	applyID := startModelRun(t, r, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
}

// anotherInstanceCancels is a keeper step during which a Keeper instance that
// does not own the run cancels it.
type anotherInstanceCancels struct {
	module.BaseModule
	other *Runner
	mu    sync.Mutex
	found bool
}

func (m *anotherInstanceCancels) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	scope, ok := util.RunScopeFrom(stream.Context())
	if !ok {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "no run scope in the module context"})
	}
	found, err := m.other.RequestCancel(stream.Context(), scope.ApplyID)
	if err != nil {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: err.Error()})
	}
	m.mu.Lock()
	m.found = found
	m.mu.Unlock()
	return stream.Send(&pluginv1.ApplyEvent{Changed: true})
}

func newQueueRunnerInWindow(t *testing.T, soulCap SoulCapabilityChecker, keepers KeeperModuleRegistry) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		SoulCap:        soulCap,
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		ServiceVars:    servicevars.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       fakeDispatcher{},
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-acolyte-nim972",
		KeeperModules:  keepers,
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     20 * time.Second,
	})
}

const queuedKeeperThenHosts = `name: create
description: one step through the work-queue, keeper side first
tasks:
  - name: keeper work
    module: %s
    on: keeper
    params: {}
  - name: host work
    module: core.exec.run
    changed_when: "false"
    params: { cmd: work }
`

// hostRows is the run's rows on its hosts, without the keeper's and the
// sentinel's.
func hostRows(t *testing.T, applyID string) []applyrun.HostStatus {
	t.Helper()
	rows, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	var out []applyrun.HostStatus
	for _, r := range rows {
		if r.SID != render.KeeperTargetSID && r.SID != render.RunSentinelSID {
			out = append(out, r)
		}
	}
	return out
}

// ★ GUARD (NIM-972, the work-queue path). A one-step run goes through the queue:
// keeper tasks, then a `planned` row per host for an Acolyte. A Cancel from
// another instance before the first row stops the keeper tasks; one made while
// they run stops the `planned` rows from being written, which an Acolyte would
// claim and send before any barrier polled.
//
// Mutations: drop the Cancel check before the keeper tasks → the keeper step runs;
// drop the one before dispatchPlanned → a planned row is written for the host.
func TestIntegration_QueuedRunCancelBeforeItsRows(t *testing.T) {
	seed := func(t *testing.T, keeperModule string) string {
		resetAll(t)
		seedOperator(t, "archon-alice")
		seedIncarnation(t, "noop-prod")
		seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
		return writeServiceRepo(t, fmt.Sprintf(queuedKeeperThenHosts, keeperModule))
	}

	t.Run("before the keeper tasks", func(t *testing.T) {
		gitURL := seed(t, "fakekeeper.order.recorded")
		gate := newWindowGate()
		t.Cleanup(func() {
			select {
			case <-gate.release:
			default:
				close(gate.release)
			}
		})
		keeperStep := &orderedKeeperModule{}
		owner := newQueueRunnerInWindow(t, beforeFirstRowCap{gate: gate},
			keeperRegistryWith(map[string]module.SoulModule{"fakekeeper.order": keeperStep}))
		applyID := startModelRun(t, owner, ExecutionStaged, "noop-prod", "noop", gitURL, "create")
		gate.await(t)
		if rows := hostRows(t, applyID); len(rows) != 0 {
			t.Fatalf("the run has host rows already: %+v", rows)
		}
		found, err := newQueueRunnerInWindow(t, stubSoulCap{}, nil).RequestCancel(context.Background(), applyID)
		if err != nil || !found {
			t.Errorf("RequestCancel from another instance = %v, %v; want found", found, err)
		}
		close(gate.release)

		waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
		if n := keeperStep.applyCount(); n != 0 {
			t.Errorf("the keeper step ran %d time(s) after the Cancel", n)
		}
		if rows := hostRows(t, applyID); len(rows) != 0 {
			t.Errorf("host rows written after the Cancel: %+v", rows)
		}
	})

	t.Run("during the keeper tasks", func(t *testing.T) {
		gitURL := seed(t, "fakekeeper.cancel.requested")
		cancel := &anotherInstanceCancels{other: newQueueRunnerInWindow(t, stubSoulCap{}, nil)}
		owner := newQueueRunnerInWindow(t, stubSoulCap{},
			keeperRegistryWith(map[string]module.SoulModule{"fakekeeper.cancel": cancel}))
		applyID := startModelRun(t, owner, ExecutionStaged, "noop-prod", "noop", gitURL, "create")

		waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
		cancel.mu.Lock()
		found := cancel.found
		cancel.mu.Unlock()
		if !found {
			t.Fatal("the Cancel during the keeper step found nothing to flag, and the test proves nothing")
		}
		if rows := hostRows(t, applyID); len(rows) != 0 {
			t.Errorf("host rows written after the Cancel: %+v", rows)
		}
	})
}

// ★ GUARD (NIM-972, the run status). Between the steps every reader of the run's
// status — the one the Voyage awaiter polls, the incarnation's run list, the run
// detail, the global run list — says applying with no finish time. Once the run
// is over the same readers say success, so the guard does not pass on readers
// that never leave applying.
//
// Mutations: ignore the lock in AggregateRunStatus → three readers say success;
// drop the lock branch of the global SQL fold → the global list says success;
// drop the lock from either finished_at → a finish time while the run applies.
func TestIntegration_BetweenStepsRunReadsApplying(t *testing.T) {
	applyID, gate, _, _ := holdBetweenSteps(t)
	assertRunReads(t, applyID, applyrun.RunStatusApplying, false)

	close(gate.release)
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	assertRunReads(t, applyID, applyrun.RunStatusSuccess, true)
}

func assertRunReads(t *testing.T, applyID string, want applyrun.RunStatus, finished bool) {
	t.Helper()
	ctx := context.Background()

	if got, err := applyrun.SelectRunStatus(ctx, integrationPool, applyID); err != nil || got != want {
		t.Errorf("SelectRunStatus (the Voyage awaiter) = %q, %v; want %q", got, err, want)
	}

	runs, _, err := applyrun.ListRunsByIncarnation(ctx, integrationPool, "noop-prod", 0, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRunsByIncarnation = %+v, %v; want one run", runs, err)
	}
	if runs[0].Status != want || (runs[0].FinishedAt != nil) != finished {
		t.Errorf("ListRunsByIncarnation: status %q finished_at %v; want %q, finished=%v", runs[0].Status, runs[0].FinishedAt, want, finished)
	}

	detail, err := applyrun.SelectRunDetail(ctx, integrationPool, applyID, "noop-prod")
	if err != nil {
		t.Fatalf("SelectRunDetail: %v", err)
	}
	if detail.Status != want || (detail.FinishedAt != nil) != finished {
		t.Errorf("SelectRunDetail: status %q finished_at %v; want %q, finished=%v", detail.Status, detail.FinishedAt, want, finished)
	}

	global, _, err := applyrun.ListRuns(ctx, integrationPool, applyrun.RunsFilter{}, incarnation.ListScope{Unrestricted: true}, 0, 10)
	if err != nil || len(global) != 1 {
		t.Fatalf("ListRuns = %+v, %v; want one run", global, err)
	}
	if global[0].Status != want || (global[0].FinishedAt != nil) != finished {
		t.Errorf("ListRuns: status %q finished_at %v; want %q, finished=%v", global[0].Status, global[0].FinishedAt, want, finished)
	}
}

// ★ GUARD (NIM-972, FENCING-1). A reclaimed Voyage releases the lock of its dead
// attempt's run. The incarnation is held by another run, live and between its
// steps, with no non-terminal row: the release refuses, and the live run commits.
//
// Mutation: drop the lock-holder check from ReleaseApplyingOrphan → the live run's
// lock is released as an orphan's and the incarnation reads ready mid-run.
func TestIntegration_BetweenStepsOrphanReleaseSparesTheLiveRun(t *testing.T) {
	applyID, gate, _, _ := holdBetweenSteps(t)

	err := incarnation.ReleaseApplyingOrphan(context.Background(), integrationPool, "noop-prod", audit.NewULID(), audit.NewULID())
	if !errors.Is(err, incarnation.ErrOrphanLockNotReleased) {
		t.Errorf("ReleaseApplyingOrphan over a live run between its steps = %v, want ErrOrphanLockNotReleased", err)
	}
	inc, err := incarnation.SelectByID(context.Background(), integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByID: %v", err)
	}
	if inc.Status != incarnation.StatusApplying || inc.ApplyingApplyID == nil || *inc.ApplyingApplyID != applyID {
		t.Errorf("after the release: status %q, lock %v; want applying, held by %s", inc.Status, inc.ApplyingApplyID, applyID)
	}

	close(gate.release)
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
}
