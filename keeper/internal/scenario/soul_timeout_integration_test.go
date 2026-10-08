//go:build integration

package scenario

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

const lostHost = "host-a.example.com"

// agentFleet is the agents a linear step goes to: a stream per host the test
// turns on and off — SendApply fails while it is off, as the Outbound does — and
// the same answer for the presence the detector reads. A host answers only when
// the test says so.
type agentFleet struct {
	mu      sync.Mutex
	present map[string]bool
	sent    map[string]*keeperv1.ApplyRequest
	sentAt  map[string]time.Time
	refused map[string]int
	// asked counts the presence queries per host: the step's first send asks once,
	// and every poll of a host waiting for its stream asks again.
	asked map[string]int
	// saturated refuses a send although the stream is there — a full queue.
	saturated map[string]bool
}

func newAgentFleet(present ...string) *agentFleet {
	f := &agentFleet{present: map[string]bool{}, sent: map[string]*keeperv1.ApplyRequest{}, sentAt: map[string]time.Time{}, refused: map[string]int{}, asked: map[string]int{}, saturated: map[string]bool{}}
	for _, sid := range present {
		f.present[sid] = true
	}
	return f
}

func (f *agentFleet) SendApply(_ context.Context, sid string, req *keeperv1.ApplyRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.present[sid] || f.saturated[sid] {
		f.refused[sid]++
		return errors.New("grpc: no active EventStream for sid")
	}
	f.sent[sid] = req
	f.sentAt[sid] = time.Now()
	return nil
}

func (f *agentFleet) SoulsStreamAlive(_ context.Context, sids []string) (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]struct{}{}
	for _, sid := range sids {
		f.asked[sid]++
		if f.present[sid] {
			out[sid] = struct{}{}
		}
	}
	return out, nil
}

func (f *agentFleet) setPresent(sid string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.present[sid] = on
}

// waitWaiting returns once sid's step waits for its stream: past the first send,
// at least one poll asked for presence.
func (f *agentFleet) waitWaiting(t *testing.T, sid string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := f.asked[sid]
		f.mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s's step did not start waiting for its stream within 10s", sid)
}

// waitRefused returns once n sends to sid have been refused.
func (f *agentFleet) waitRefused(t *testing.T, sid string, n int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := f.refused[sid]
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fewer than %d sends to %s refused within 4s", n, sid)
}

func (f *agentFleet) setSaturated(sid string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saturated[sid] = on
}

func (f *agentFleet) waitSent(t *testing.T, sid string) *keeperv1.ApplyRequest {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		req := f.sent[sid]
		f.mu.Unlock()
		if req != nil {
			return req
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no ApplyRequest reached %s within 10s", sid)
	return nil
}

// answer is the RunResult arriving, as correlateRunResult writes it.
func (f *agentFleet) answer(t *testing.T, sid string, status applyrun.Status) {
	t.Helper()
	req := f.waitSent(t, sid)
	if err := applyrun.UpdateStatus(context.Background(), integrationPool, req.GetApplyId(), sid, int(req.GetPassage()), status, nil); err != nil {
		t.Fatalf("answer %s: %v", sid, err)
	}
}

func linearRunner(t *testing.T, fleet *agentFleet, runTimeout time.Duration, keepers KeeperModuleRegistry) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	if keepers == nil {
		keepers = stateKeeperRegistry()
	}
	return NewRunner(Deps{
		SoulCap:        stubSoulCap{},
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		ServiceVars:    servicevars.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       fleet,
		SoulPresence:   fleet,
		KeeperModules:  keepers,
		DB:             integrationPool,
		ExecutionModel: ExecutionLinear,
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     runTimeout,
	})
}

func startLostHostRun(t *testing.T, r *Runner, scenario string) string {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, lostHost, []string{"noop-prod"})
	gitURL := writeServiceRepo(t, scenario)
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
	return applyID
}

func lostHostScenario(soulTimeout string) string {
	return `name: create
settings:
  soul_timeout: ` + soulTimeout + `
tasks:
  - name: echo
    module: core.exec.run
    params: { cmd: echo, args: ["x"] }
    changed_when: "false"
`
}

func lostRow(t *testing.T, applyID, sid string) applyrun.HostStatus {
	t.Helper()
	st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	for _, hs := range st {
		if hs.SID == sid {
			return hs
		}
	}
	t.Fatalf("no apply_runs row for %s: %+v", sid, st)
	return applyrun.HostStatus{}
}

func summaryOf(hs applyrun.HostStatus) string {
	if hs.ErrorSummary == nil {
		return ""
	}
	return *hs.ErrorSummary
}

// ★ GUARD (NIM-949, acceptance 1). A host whose stream is lost after the step went
// out, and stays lost, fails for unavailability once soul_timeout runs out — not
// at scenario_timeout, which is 24h here.
//
// Mutation: make stepWatch.check a no-op (the run timeout is the only bound again)
// → the run is still applying at the 10s wait → red.
func TestIntegration_SoulTimeout_LostStreamFailsTheHost(t *testing.T) {
	fleet := newAgentFleet(lostHost)
	r := linearRunner(t, fleet, time.Hour, nil)
	applyID := startLostHostRun(t, r, lostHostScenario("1s"))

	fleet.waitSent(t, lostHost)
	if got := lostRow(t, applyID, lostHost).Status; got != applyrun.StatusDispatched {
		t.Fatalf("row after the send = %q, want dispatched (the WardRoster sweep covers only dispatched rows)", got)
	}
	fleet.setPresent(lostHost, false)

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	row := lostRow(t, applyID, lostHost)
	if row.Status != applyrun.StatusFailed || !strings.HasPrefix(summaryOf(row), soulUnavailable+":") {
		t.Errorf("row = %s %q, want failed %s", row.Status, summaryOf(row), soulUnavailable)
	}
}

// ★ GUARD (NIM-949, acceptance 3). A stream that comes back before soul_timeout
// forgives the loss: two drops, each shorter than soul_timeout and longer
// together, then the result — the host succeeds.
//
// Mutation: keep the first sighting of a loss across a reconnect (no
// delete(lostSince)) → the host fails at the second drop → red.
func TestIntegration_SoulTimeout_ShortDropsThenResultSucceed(t *testing.T) {
	fleet := newAgentFleet(lostHost)
	r := linearRunner(t, fleet, time.Hour, nil)
	applyID := startLostHostRun(t, r, lostHostScenario("1500ms"))

	fleet.waitSent(t, lostHost)
	for range 2 {
		fleet.setPresent(lostHost, false)
		time.Sleep(time.Second)
		fleet.setPresent(lostHost, true)
		time.Sleep(200 * time.Millisecond)
	}
	fleet.answer(t, lostHost, applyrun.StatusSuccess)

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
}

// ★ GUARD (NIM-949, acceptance 2). A live host's step is bounded by nothing but
// scenario_timeout: the run ceiling the Runner was built with (300ms here,
// standing in for the old 5 minutes) does not cut a linear run.
//
// Mutation: runDeadline returns r.effectiveRunTimeout for a linear run → the run
// aborts at 300ms → red.
func TestIntegration_SoulTimeout_LiveSlowHostNotCutByRunCeiling(t *testing.T) {
	fleet := newAgentFleet(lostHost)
	r := linearRunner(t, fleet, 300*time.Millisecond, nil)
	applyID := startLostHostRun(t, r, lostHostScenario("1m"))

	fleet.waitSent(t, lostHost)
	time.Sleep(time.Second)
	fleet.answer(t, lostHost, applyrun.StatusSuccess)

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
}

// ★ GUARD (NIM-949). A send to a host without a stream waits for one instead of
// failing: the stream comes up within soul_timeout, the step goes out — not before
// the stream has been there presenceSettle, so the reconnected Soul's WardRoster
// is swept first — and the host succeeds. With no stream at all the host fails
// for unavailability.
//
// A host known to have no stream is not tried at all until it has one; a send
// refused while the stream is there — a full queue — waits the same way.
//
// Mutations: fail the host on the first refused send (no wait) → "saturated" is
// red; send on the first poll that sees the stream → "comes back" is red; try the
// first send without asking for presence → a refused send → red; never give up
// waiting → "never" is still applying at 10s → red.
func TestIntegration_SoulTimeout_SendWaitsForTheStream(t *testing.T) {
	t.Run("comes back", func(t *testing.T) {
		fleet := newAgentFleet()
		r := linearRunner(t, fleet, time.Hour, nil)
		applyID := startLostHostRun(t, r, lostHostScenario("5s"))

		fleet.waitWaiting(t, lostHost)
		fleet.setPresent(lostHost, true)
		cameBack := time.Now()
		fleet.answer(t, lostHost, applyrun.StatusSuccess)

		waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
		fleet.mu.Lock()
		waited := fleet.sentAt[lostHost].Sub(cameBack)
		refused := fleet.refused[lostHost]
		fleet.mu.Unlock()
		if waited < r.presenceSettle() {
			t.Errorf("sent %s after the stream came back, want at least presenceSettle (%s) — the reconnected Soul's WardRoster sweep must run first", waited, r.presenceSettle())
		}
		if refused != 0 {
			t.Errorf("%d sends tried with no stream, want none — a row inserted to be withdrawn can meet a reconnecting Soul's sweep", refused)
		}
	})
	t.Run("saturated", func(t *testing.T) {
		fleet := newAgentFleet(lostHost)
		fleet.setSaturated(lostHost, true)
		r := linearRunner(t, fleet, time.Hour, nil)
		applyID := startLostHostRun(t, r, lostHostScenario("5s"))

		time.Sleep(300 * time.Millisecond)
		fleet.setSaturated(lostHost, false)
		fleet.answer(t, lostHost, applyrun.StatusSuccess)
		waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
		fleet.mu.Lock()
		defer fleet.mu.Unlock()
		if fleet.refused[lostHost] == 0 {
			t.Error("no send was refused — the case proves nothing")
		}
	})
	t.Run("never", func(t *testing.T) {
		fleet := newAgentFleet()
		r := linearRunner(t, fleet, time.Hour, nil)
		applyID := startLostHostRun(t, r, lostHostScenario("1s"))

		waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
		row := lostRow(t, applyID, lostHost)
		if row.Status != applyrun.StatusFailed || !strings.HasPrefix(summaryOf(row), soulUnavailable+":") {
			t.Errorf("row = %s %q, want failed %s", row.Status, summaryOf(row), soulUnavailable)
		}
	})
}

// ★ GUARD (NIM-949). A linear step's row is one the WardRoster sweep resolves: a
// Soul that reconnects without the step — restarted, or never received it —
// declares nothing, and the host fails at once, the stream being back.
//
// Mutation: insert the linear row `running` → the sweep skips it → still applying
// at 10s → red.
func TestIntegration_SoulTimeout_WardRosterOrphansAStepTheSoulLost(t *testing.T) {
	fleet := newAgentFleet(lostHost)
	r := linearRunner(t, fleet, time.Hour, nil)
	applyID := startLostHostRun(t, r, lostHostScenario("1m"))

	fleet.waitSent(t, lostHost)
	if _, err := applyrun.OrphanDispatched(context.Background(), integrationPool, lostHost, nil); err != nil {
		t.Fatalf("OrphanDispatched: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if got := lostRow(t, applyID, lostHost).Status; got != applyrun.StatusOrphaned {
		t.Errorf("row = %q, want orphaned", got)
	}
}

// blockingKeeperModule holds its step until the step's context ends.
type blockingKeeperModule struct{ module.BaseModule }

func (blockingKeeperModule) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	<-stream.Context().Done()
	return stream.Context().Err()
}

// ★ GUARD (NIM-949, user decision 1). A keeper-side step is bounded by its
// task_timeout as a whole, written as its own timeout:.
//
// Mutations: applyKeeperStep ignores rt.Timeout → the step blocks until the 24h
// scenario_timeout → still applying at 10s → red; the timeout replaces the
// module's message instead of leading it → red.
func TestIntegration_SoulTimeout_KeeperStepBoundedByItsTimeout(t *testing.T) {
	fleet := newAgentFleet(lostHost)
	r := linearRunner(t, fleet, time.Hour, keeperRegistryWith(map[string]module.SoulModule{
		"core.soul": blockingKeeperModule{},
	}))
	applyID := startLostHostRun(t, r, `name: create
tasks:
  - name: Keeper step
    module: core.soul.registered
    timeout: 300ms
    params:
      sid: "host-a.example.com"
      coven: ["tagged"]
      mode: append
  - name: echo
    module: core.exec.run
    params: { cmd: echo, args: ["x"] }
    changed_when: "false"
`)

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	row := lostRow(t, applyID, render.KeeperTargetSID)
	if row.Status != applyrun.StatusFailed || !strings.Contains(summaryOf(row), "timed out after 300ms (task_timeout)") {
		t.Errorf("keeper row = %s %q, want failed by task_timeout", row.Status, summaryOf(row))
	}
	if !strings.Contains(summaryOf(row), context.DeadlineExceeded.Error()) {
		t.Errorf("keeper row summary %q lost the module's own message", summaryOf(row))
	}
}

// injectFirstFailure makes the first apply_runs row operation op (INSERT or
// DELETE) matching cond fail, as a database error would.
func injectFirstFailure(t *testing.T, op, cond string) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE SEQUENCE nim949_fail_once`,
		`CREATE FUNCTION nim949_fail_once() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF ` + cond + ` AND nextval('nim949_fail_once') = 1 THEN
				RAISE EXCEPTION 'injected: first ` + op + ` refused';
			END IF;
			IF TG_OP = 'DELETE' THEN
				RETURN OLD;
			END IF;
			RETURN NEW;
		END $$`,
		`CREATE TRIGGER nim949_fail_once BEFORE ` + op + ` ON apply_runs FOR EACH ROW EXECUTE FUNCTION nim949_fail_once()`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = integrationPool.Exec(ctx, `DROP TRIGGER IF EXISTS nim949_fail_once ON apply_runs`)
		_, _ = integrationPool.Exec(ctx, `DROP FUNCTION IF EXISTS nim949_fail_once()`)
		_, _ = integrationPool.Exec(ctx, `DROP SEQUENCE IF EXISTS nim949_fail_once`)
	})
}

// ★ GUARD (NIM-949). A row a refused send failed to withdraw is taken back by the
// next try, which then sends the step: left in place, every later insert would
// collide with it and the host, its stream present, would never be sent the step.
// The first refused send leaves the row; a second send is tried only once it has
// been taken back.
//
// Mutation: drop the already-exists branch of trySend → no second send → red.
func TestIntegration_SoulTimeout_UnwithdrawnRowIsTakenBack(t *testing.T) {
	fleet := newAgentFleet()
	r := linearRunner(t, fleet, time.Hour, nil)
	injectFirstFailure(t, "DELETE", "OLD.status = 'dispatched'")
	applyID := startLostHostRun(t, r, lostHostScenario("5s"))

	fleet.waitWaiting(t, lostHost)
	fleet.setSaturated(lostHost, true)
	fleet.setPresent(lostHost, true)
	fleet.waitRefused(t, lostHost, 2)
	fleet.setSaturated(lostHost, false)
	fleet.answer(t, lostHost, applyrun.StatusSuccess)
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
}

// ★ GUARD (NIM-949). A database error sending the step to a host that waited for
// its stream is retried: the stream is there, and failing the host would record
// it as unavailable.
//
// Mutation: fail the host on the error → the run ends error_locked → red.
func TestIntegration_SoulTimeout_DatabaseErrorWhileWaitingIsRetried(t *testing.T) {
	fleet := newAgentFleet()
	r := linearRunner(t, fleet, time.Hour, nil)
	injectFirstFailure(t, "INSERT", "NEW.status = 'dispatched'")
	applyID := startLostHostRun(t, r, lostHostScenario("5s"))

	fleet.waitWaiting(t, lostHost)
	fleet.setPresent(lostHost, true)
	fleet.answer(t, lostHost, applyrun.StatusSuccess)
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
}

// ★ GUARD (NIM-949). The failed row of a host the step never reached is written
// even when the first write fails: without it the barrier waits for the host
// until scenario_timeout. A trigger refuses the first insert of a failed row.
//
// Mutation: give up after the first failed write → no row → still applying at
// 10s → red.
func TestIntegration_SoulTimeout_UnsentFailureWriteIsRetried(t *testing.T) {
	fleet := newAgentFleet()
	r := linearRunner(t, fleet, time.Hour, nil)
	injectFirstFailure(t, "INSERT", "NEW.status = 'failed'")
	applyID := startLostHostRun(t, r, lostHostScenario("300ms"))

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	row := lostRow(t, applyID, lostHost)
	if row.Status != applyrun.StatusFailed || !strings.HasPrefix(summaryOf(row), soulUnavailable+":") {
		t.Errorf("row = %s %q, want failed %s", row.Status, summaryOf(row), soulUnavailable)
	}
}

// ★ GUARD (NIM-949). The lost-stream detector covers a step the work-queue carries
// (NIM-961): the claim marks the row dispatched, and a host whose stream stays gone
// for soul_timeout is failed soul_unavailable by the same barrier.
//
// Mutation: hand dispatchStepPlanned no step watch → the row stays dispatched → red.
func TestIntegration_SoulTimeout_QueuedStepLostStreamFailsTheHost(t *testing.T) {
	const host = "host-a.example.com"
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, host, []string{"noop-prod"})
	gitURL := writeServiceRepo(t, `name: create
description: one host step
settings:
  soul_timeout: 300ms
tasks:
  - name: host work
    module: core.exec.run
    changed_when: "false"
    params: { cmd: work }
`)
	fleet := newAgentFleet(host)
	owner := newQueueRunnerInWindow(t, stubSoulCap{}, nil)
	owner.deps.StepQueue = stepQueueAnswer{ok: true}
	owner.deps.SoulPresence = fleet
	applyID := startModelRun(t, owner, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
	waitHostRow(t, applyID, host, applyrun.StatusPlanned)
	// The claim's mark, as an Acolyte makes it before the send; then the stream goes.
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE apply_runs SET status = 'dispatched' WHERE apply_id = $1 AND sid = $2`, applyID, host); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	fleet.setPresent(host, false)

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	rows := hostRows(t, applyID)
	if len(rows) != 1 || rows[0].Status != applyrun.StatusFailed || !strings.HasPrefix(summaryOf(rows[0]), soulUnavailable+":") {
		t.Errorf("host rows = %+v, want one failed %s row", rows, soulUnavailable)
	}
}

// ★ GUARD (NIM-949, user decision 2026-10-08). A step the work-queue carries is
// queued for a host only once its stream is there, as an inline step is sent: the
// claim that takes the row sends at once and would fail a host with no stream. The
// row is written after the stream has been there presenceSettle; a stream that never
// comes fails the host soul_unavailable with nothing queued.
//
// Mutations: queue the row whatever the presence → a planned row while the host has
// no stream → "comes back" red; never give up waiting → "never" still applying → red.
func TestIntegration_SoulTimeout_QueuedStepWaitsForTheStream(t *testing.T) {
	const host = "host-a.example.com"
	start := func(t *testing.T, fleet *agentFleet) string {
		resetAll(t)
		seedOperator(t, "archon-alice")
		seedIncarnation(t, "noop-prod")
		seedConnectedSoul(t, host, []string{"noop-prod"})
		gitURL := writeServiceRepo(t, `name: create
description: one host step
settings:
  soul_timeout: 2s
tasks:
  - name: host work
    module: core.exec.run
    changed_when: "false"
    params: { cmd: work }
`)
		owner := newQueueRunnerInWindow(t, stubSoulCap{}, nil)
		owner.deps.StepQueue = stepQueueAnswer{ok: true}
		owner.deps.SoulPresence = fleet
		return startModelRun(t, owner, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
	}
	t.Run("comes back", func(t *testing.T) {
		fleet := newAgentFleet()
		applyID := start(t, fleet)
		fleet.waitWaiting(t, host)
		if rows := hostRows(t, applyID); len(rows) != 0 {
			t.Fatalf("rows while the host has no stream = %+v, want none", rows)
		}
		fleet.setPresent(host, true)
		waitHostRow(t, applyID, host, applyrun.StatusPlanned)
		// The claim's mark and the Soul's answer.
		if _, err := integrationPool.Exec(context.Background(),
			`UPDATE apply_runs SET status = 'dispatched' WHERE apply_id = $1 AND sid = $2`, applyID, host); err != nil {
			t.Fatalf("mark dispatched: %v", err)
		}
		if err := applyrun.UpdateStatus(context.Background(), integrationPool, applyID, host, 0, applyrun.StatusSuccess, nil); err != nil {
			t.Fatalf("the Soul's result: %v", err)
		}
		waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	})
	t.Run("never", func(t *testing.T) {
		applyID := start(t, newAgentFleet())
		waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
		rows := hostRows(t, applyID)
		if len(rows) != 1 || rows[0].Status != applyrun.StatusFailed || !strings.HasPrefix(summaryOf(rows[0]), soulUnavailable+":") {
			t.Errorf("host rows = %+v, want one failed %s row", rows, soulUnavailable)
		}
	})
}

const secondHost = "host-b.example.com"

func startTwoHostRun(t *testing.T, r *Runner, scenario string) string {
	t.Helper()
	return startHostsRun(t, r, scenario, lostHost, secondHost)
}

func startHostsRun(t *testing.T, r *Runner, scenario string, sids ...string) string {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	for _, sid := range sids {
		seedConnectedSoul(t, sid, []string{"noop-prod"})
	}
	gitURL := writeServiceRepo(t, scenario)
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
	return applyID
}

// ★ GUARD (NIM-949). A cluster-wide Cancel reaches a host still waiting for its
// stream: it is not sent the step, though the barrier defers the Cancel while the
// other host's row is dispatched (ADR-027 amend (r)) — sent meanwhile, it would
// apply the step after the Cancel.
//
// Mutation: drop the Cancel check from await → host-b is sent the step → red.
func TestIntegration_SoulTimeout_CancelReachesAHostWaitingForItsStream(t *testing.T) {
	fleet := newAgentFleet(lostHost)
	r := linearRunner(t, fleet, time.Hour, nil)
	applyID := startTwoHostRun(t, r, lostHostScenario("5s"))
	fleet.waitSent(t, lostHost)
	fleet.waitWaiting(t, secondHost)
	if locked, err := incarnation.RequestRunCancel(context.Background(), integrationPool, applyID); err != nil || !locked {
		t.Fatalf("RequestRunCancel = %v, %v; want the lock flagged", locked, err)
	}
	fleet.setPresent(secondHost, true)
	time.Sleep(10 * r.presenceSettle())
	fleet.mu.Lock()
	sent := fleet.sent[secondHost]
	fleet.mu.Unlock()
	if sent != nil {
		t.Errorf("host-b was sent the step after the run was cancelled")
	}
	fleet.answer(t, lostHost, applyrun.StatusSuccess)
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	for _, row := range hostRows(t, applyID) {
		if row.SID == secondHost {
			t.Errorf("host-b row = %+v, want none", row)
		}
	}
}

// ★ GUARD (NIM-949). A step a dispatch error has ended withdraws a host still
// waiting for its stream: the run does not wait for it, and it is neither sent the
// step nor failed for unavailability.
//
// Mutation: count the waiting host when the dispatch fails (no withdraw) → the run
// waits soul_timeout for host-a, which gets a soul_unavailable row → red.
func TestIntegration_SoulTimeout_DoomedStepWithdrawsAWaitingHost(t *testing.T) {
	fleet := newAgentFleet(secondHost)
	r := linearRunner(t, fleet, time.Hour, nil)
	injectFirstFailure(t, "INSERT", "NEW.sid = '"+secondHost+"'")
	applyID := startTwoHostRun(t, r, lostHostScenario("3s"))

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	for _, row := range hostRows(t, applyID) {
		if row.SID == lostHost {
			t.Errorf("host-a row = %+v, want none — the step it waited for had already failed", row)
		}
	}
}

// ★ GUARD (NIM-949). A late-queued row of a run cancelled between the wait's last
// check and the row's write is flagged: the claim reads only the row's flag, and an
// unflagged row would be sent after the Cancel. A trigger lands the Cancel on the
// applying lock inside the row's own insert.
//
// Mutation: drop the re-check after the late write → the row is never flagged → red.
func TestIntegration_SoulTimeout_LateQueuedRowOfACancelledRunIsFlagged(t *testing.T) {
	const host = "host-a.example.com"
	ctx := context.Background()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, host, []string{"noop-prod"})
	gitURL := writeServiceRepo(t, `name: create
description: one host step
settings:
  soul_timeout: 5s
tasks:
  - name: host work
    module: core.exec.run
    changed_when: "false"
    params: { cmd: work }
`)
	for _, stmt := range []string{
		`CREATE FUNCTION nim949_cancel_on_plan() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			UPDATE incarnation SET applying_cancel_requested = true WHERE applying_apply_id = NEW.apply_id;
			RETURN NEW;
		END $$`,
		`CREATE TRIGGER nim949_cancel_on_plan BEFORE INSERT ON apply_runs FOR EACH ROW
		WHEN (NEW.status = 'planned') EXECUTE FUNCTION nim949_cancel_on_plan()`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = integrationPool.Exec(ctx, `DROP TRIGGER IF EXISTS nim949_cancel_on_plan ON apply_runs`)
		_, _ = integrationPool.Exec(ctx, `DROP FUNCTION IF EXISTS nim949_cancel_on_plan()`)
	})
	fleet := newAgentFleet()
	owner := newQueueRunnerInWindow(t, stubSoulCap{}, nil)
	owner.deps.StepQueue = stepQueueAnswer{ok: true}
	owner.deps.SoulPresence = fleet
	applyID := startModelRun(t, owner, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
	fleet.waitWaiting(t, host)
	fleet.setPresent(host, true)

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	var flagged bool
	if err := integrationPool.QueryRow(ctx,
		`SELECT cancel_requested FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, host).Scan(&flagged); err != nil {
		t.Fatalf("read the late row: %v", err)
	}
	if !flagged {
		t.Error("the late-queued row of a cancelled run is not flagged — a claim would send it")
	}
}

// ★ GUARD (NIM-949). A host that waited for its stream and was sent its step no
// longer counts as waiting: when a later wave's dispatch fails, the step still waits
// for every host already sent, or the abort would close a row a live Soul applies.
// Waves of two: host-a waits and is sent in the first, host-c is sent in the
// second, and host-d's insert fails beside it.
//
// Mutation: do not count a sent host out of the waiting ones → the step ends at
// host-d's failure and host-c's row is closed under it → red.
func TestIntegration_SoulTimeout_SentWaitingHostStaysCounted(t *testing.T) {
	const hostC, hostD = "host-c.example.com", "host-d.example.com"
	fleet := newAgentFleet(secondHost, hostC, hostD)
	r := linearRunner(t, fleet, time.Hour, nil)
	injectFirstFailure(t, "INSERT", "NEW.sid = '"+hostD+"'")
	applyID := startHostsRun(t, r, `name: create
settings:
  soul_timeout: 5s
tasks:
  - name: echo
    module: core.exec.run
    params: { cmd: echo, args: ["x"] }
    changed_when: "false"
    serial: 2
`, lostHost, secondHost, hostC, hostD)

	fleet.waitWaiting(t, lostHost)
	fleet.setPresent(lostHost, true)
	fleet.answer(t, lostHost, applyrun.StatusSuccess)
	fleet.answer(t, secondHost, applyrun.StatusSuccess)
	fleet.waitSent(t, hostC)
	time.Sleep(300 * time.Millisecond)
	inc, err := incarnation.SelectByID(context.Background(), integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByID: %v", err)
	}
	if inc.Status != incarnation.StatusApplying {
		t.Fatalf("the run ended (status %q) while host-c applied its step", inc.Status)
	}
	fleet.answer(t, hostC, applyrun.StatusSuccess)
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if got := lostRow(t, applyID, hostC).Status; got != applyrun.StatusSuccess {
		t.Errorf("host-c row = %s, want its own success", got)
	}
}

// ★ GUARD (NIM-949). A wait that ends on a Cancel takes back a row an earlier try
// failed to withdraw: left dispatched, with nothing on the host to finish it, it
// holds the lock's Cancel deferred while the host's stream stays present, so the run
// would wait out scenario_timeout. A trigger refuses the withdraw until the Cancel,
// and twice after it — at most one of those can be the failed send's own — so the
// wait has to try again.
//
// Mutations: leave the wait on a Cancel without the withdraw → the run never ends →
// red; leave it when the withdraw fails instead of trying again → the same.
func TestIntegration_SoulTimeout_CancelWithdrawsAnUnsentRow(t *testing.T) {
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE SEQUENCE nim949_after_cancel`,
		`CREATE FUNCTION nim949_hold_withdraw() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.status = 'dispatched' THEN
				IF NOT EXISTS (
					SELECT 1 FROM incarnation WHERE applying_apply_id = OLD.apply_id AND applying_cancel_requested
				) THEN
					RAISE EXCEPTION 'injected: withdraw refused';
				END IF;
				IF nextval('nim949_after_cancel') <= 2 THEN
					RAISE EXCEPTION 'injected: first withdraw after the Cancel refused';
				END IF;
			END IF;
			RETURN OLD;
		END $$`,
		`CREATE TRIGGER nim949_hold_withdraw BEFORE DELETE ON apply_runs FOR EACH ROW EXECUTE FUNCTION nim949_hold_withdraw()`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = integrationPool.Exec(ctx, `DROP TRIGGER IF EXISTS nim949_hold_withdraw ON apply_runs`)
		_, _ = integrationPool.Exec(ctx, `DROP FUNCTION IF EXISTS nim949_hold_withdraw()`)
		_, _ = integrationPool.Exec(ctx, `DROP SEQUENCE IF EXISTS nim949_after_cancel`)
	})
	fleet := newAgentFleet()
	r := linearRunner(t, fleet, time.Hour, nil)
	applyID := startLostHostRun(t, r, lostHostScenario("5s"))
	fleet.waitWaiting(t, lostHost)
	fleet.setSaturated(lostHost, true)
	fleet.setPresent(lostHost, true)
	fleet.waitRefused(t, lostHost, 1)
	if locked, err := incarnation.RequestRunCancel(ctx, integrationPool, applyID); err != nil || !locked {
		t.Fatalf("RequestRunCancel = %v, %v; want the lock flagged", locked, err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if rows := hostRows(t, applyID); len(rows) != 0 {
		t.Errorf("host rows = %+v, want none — the step never reached the host", rows)
	}
}

// ★ GUARD (NIM-949). A waiting host reads a Cancel from the rows as well as from
// the applying lock: a run that holds no lock (`destroying`) carries its Cancel on
// its rows only, and a host with its stream present must not be sent the step then.
//
// Mutation: read only the lock → the host is attempted → red.
func TestIntegration_SoulTimeout_WaitReadsARowCancel(t *testing.T) {
	ctx := context.Background()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	applyID := audit.NewULID()
	aid := "archon-alice"
	other := &applyrun.ApplyRun{ApplyID: applyID, SID: secondHost, IncarnationName: "noop-prod", Scenario: "destroy", Status: applyrun.StatusPlanned, StartedByAID: &aid}
	if err := applyrun.Insert(ctx, integrationPool, other); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n, err := applyrun.RequestCancel(ctx, integrationPool, applyID); err != nil || n != 1 {
		t.Fatalf("RequestCancel = %d, %v; want the planned row flagged", n, err)
	}
	r := linearRunner(t, newAgentFleet(lostHost), time.Hour, nil)
	w := r.newStepWatch(RunSpec{ApplyID: applyID, executionModel: ExecutionLinear}, 0, slog.Default())
	attempted := false
	left := w.await(ctx, &applyrun.ApplyRun{ApplyID: applyID, SID: lostHost}, func() (bool, error) {
		attempted = true
		return true, nil
	}, nil)
	if attempted || left {
		t.Errorf("await attempted=%v left=%v; want no attempt on a run whose row carries the Cancel", attempted, left)
	}
}

// ★ GUARD (NIM-949). A waiting host whose run's Cancel cannot be read is not sent the
// step: unknown is not "not cancelled". The Runner's database is closed, so every
// read fails; the host's stream is present.
//
// Mutation: treat a read error as "not cancelled" → the host is attempted → red.
func TestIntegration_SoulTimeout_WaitFailsClosedOnAnUnreadableCancel(t *testing.T) {
	ctx := context.Background()
	closed, err := pgxpool.New(ctx, integrationPool.Config().ConnString())
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	closed.Close()
	r := linearRunner(t, newAgentFleet(lostHost), time.Hour, nil)
	r.deps.DB = closed
	w := r.newStepWatch(RunSpec{ApplyID: audit.NewULID(), executionModel: ExecutionLinear}, 0, slog.Default())
	waitCtx, cancel := context.WithTimeout(ctx, 10*r.presenceSettle())
	defer cancel()
	attempted := false
	w.await(waitCtx, &applyrun.ApplyRun{SID: lostHost}, func() (bool, error) {
		attempted = true
		return true, nil
	}, nil)
	if attempted {
		t.Error("the host was attempted while its run's Cancel could not be read")
	}
}
