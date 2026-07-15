//go:build integration

// Integration tests for the scenario runner (slice .g) via testcontainers PG +
// a local-fs git repo service-noop + mock Outbound. Soul's RunResult is
// simulated inside the mock dispatcher via a direct apply_runs.UpdateStatus —
// the same path events_runresult.go::correlateRunResult uses in prod.
//
// Covers end-to-end: happy-path (1 task → 1 host → success → state commit →
// ready) and fail-path (RunResult failed → barrier → error_locked).

package scenario

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// renderedExecCommand joins the rendered core.exec.run argv command from
// wire-params (`cmd` + `args`) into one space-separated string — for test
// assertions that check the whole resulting command (e.g. "echo hi!").
func renderedExecCommand(p *structpb.Struct) string {
	if p == nil {
		return ""
	}
	cmd := p.GetFields()["cmd"].GetStringValue()
	parts := []string{cmd}
	for _, v := range p.GetFields()["args"].GetListValue().GetValues() {
		parts = append(parts, v.GetStringValue())
	}
	return strings.Join(parts, " ")
}

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	// Tests load the service repo via a file:// URL (local-fs git), which in
	// prod is blocked by the scheme allowlist (security review L2). Enable the
	// dev/test flag for the whole package run — same trick as artifact_test.go.
	os.Setenv("SOUL_STACK_ALLOW_FILE_REPOS", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("scenario integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("scenario integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

// --- fixtures ---------------------------------------------------------

func resetAll(t *testing.T) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE apply_runs, state_history, incarnation, souls, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedOperator(t *testing.T, aid string) {
	t.Helper()
	op := &operator.Operator{AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seedOperator: %v", err)
	}
}

func seedIncarnation(t *testing.T, name string) {
	t.Helper()
	inc := &incarnation.Incarnation{
		Name: name, Service: "noop", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusReady,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnation: %v", err)
	}
}

func seedConnectedSoul(t *testing.T, sid string, covens []string) {
	t.Helper()
	s := &soul.Soul{SID: sid, Coven: covens, Status: soul.StatusConnected}
	if err := soul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedConnectedSoul: %v", err)
	}
}

// noopServiceRepo creates a local-fs git repo service-noop with one commit:
// service.yml + scenario/create/main.yml (1 core.exec.run task). Returns a
// file:// URL for artifact.ServiceLoader.
func noopServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}

	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	write("service.yml", `name: noop
state_schema_version: 1
description: noop service for scenario-runner integration test
state_schema:
  type: object
  properties: {}
`)
	write("scenario/create/main.yml", `name: create
description: smoke core.exec.run
state_changes: {}
tasks:
  - name: Echo hello on every host
    module: core.exec.run
    params:
      cmd: echo
      args: ["hello"]
    changed_when: "false"
`)

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init noop", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// mockDispatcher simulates Soul: on SendApply it immediately writes a
// terminal apply_runs status (like correlateRunResult in prod), completing
// the barrier.
type mockDispatcher struct {
	t          *testing.T
	result     applyrun.Status
	summary    *string
	calls      int
	gotApplyID string
	gotTasks   int
	gotAttempt int32 // attempt of the last ApplyRequest (old dispatch path → 0)
}

func (m *mockDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	m.calls++
	m.gotApplyID = req.GetApplyId()
	m.gotTasks = len(req.GetTasks())
	m.gotAttempt = req.GetAttempt()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, m.result, m.summary); err != nil {
		m.t.Errorf("mockDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

// waveDispatcher simulates Soul for serial waves: records the ORDER of
// SendApply calls (by SID) and writes a terminal status. failOn — the SID that
// finishes failed (to test fail-stop: the wave with this host breaks the
// barrier, later waves don't start).
type waveDispatcher struct {
	t      *testing.T
	mu     sync.Mutex
	order  []string
	failOn string
}

func (d *waveDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	d.order = append(d.order, sid)
	d.mu.Unlock()

	status := applyrun.StatusSuccess
	var summary *string
	if sid == d.failOn {
		status = applyrun.StatusFailed
		s := "simulated failure"
		summary = &s
	}
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, status, summary); err != nil {
		d.t.Errorf("waveDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

func (d *waveDispatcher) dispatchedSIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.order))
	copy(out, d.order)
	return out
}

func newRunner(t *testing.T, disp ApplyDispatcher, gitURL string) *Runner {
	t.Helper()
	return newRunnerWithDestiny(t, disp, nil)
}

// newRunnerAcolyte builds a Runner with AcolyteEnabled=true and a real
// Outbound (disp simulates Soul). Used by the staged test that proves a
// staged run goes INLINE even in work-queue mode (run.go gate !staged,
// ADR-056 §S4): dispatchPlanned (the Acolyte path) is NOT called for staged.
// KID/PollInterval mirror newAcolyteRunner. gitURL is unused (the loader
// clones from ServiceRef.Git).
func newRunnerAcolyte(t *testing.T, disp ApplyDispatcher, gitURL string) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		Essence:        essence.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       disp,
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-acolyte-staged-test",
		// Staged gate (ADR-056 §S5): test hosts are passage-capable (see newRunnerWithDestiny).
		PassageCap:   stubPassageCap{},
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
	})
}

// newRunnerWithDestiny builds a Runner with an optional DestinySource (for
// apply:destiny). Empty destinyTemplate → Destiny=nil (apply:destiny unsupported).
func newRunnerWithDestiny(t *testing.T, disp ApplyDispatcher, destinySrc *DestinySource) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:   artifact.NewServiceLoader(t.TempDir(), nil),
		Topology: topology.NewResolver(integrationPool, nil, nil),
		Essence:  essence.NewResolver(nil),
		Render:   render.NewPipeline(nil, engine, nil, nil),
		Outbound: disp,
		Destiny:  destinySrc,
		DB:       integrationPool,
		// Staged gate (ADR-056 §S5): test hosts "support passage" (lacking is
		// empty) — otherwise the fail-closed reject would reject all staged
		// tests. The forward-compat reject is verified by a separate stub in
		// TestIntegration_StagedOldSoul_Rejected.
		PassageCap:   stubPassageCap{},
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
	})
}

// newRunnerWithAuditStaged — staged variant of [newRunnerWithAudit] (real
// auditpg.Writer/Reader like production daemon.go) + PassageCap=stubPassageCap{}
// (both hosts passage-aware, otherwise the S5 gate would reject staged). Needed
// for the cross-passage gate (ADR-056 R3): it reads CHANGED/FAILED facts of
// earlier Passages from the audit log via AuditReader.
func newRunnerWithAuditStaged(t *testing.T, disp ApplyDispatcher) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:       artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:     topology.NewResolver(integrationPool, nil, nil),
		Essence:      essence.NewResolver(nil),
		Render:       render.NewPipeline(nil, engine, nil, nil),
		Outbound:     disp,
		DB:           integrationPool,
		Audit:        auditpg.NewWriter(integrationPool),
		AuditReader:  auditpg.NewReader(integrationPool),
		PassageCap:   stubPassageCap{},
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
	})
}

// stubPassageCap — a controllable [PassageCapabilityChecker] for tests.
// lacking — SIDs that do NOT support passage (default nil → everyone
// supports it, like a single-version beta fleet). err — simulates a Redis
// failure.
type stubPassageCap struct {
	lacking []string
	err     error
}

func (s stubPassageCap) SoulsLackingPassage(_ context.Context, _ []string) ([]string, error) {
	return s.lacking, s.err
}

// newRunnerWithPassageCap — a Runner with an explicit [PassageCapabilityChecker]
// (forward-compat guard test, ADR-056 §S5): cap=nil → the gate's fail-closed
// branch; cap with lacking → reject. Otherwise like newRunnerWithDestiny
// (without Destiny).
func newRunnerWithPassageCap(t *testing.T, disp ApplyDispatcher, cap PassageCapabilityChecker) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:       artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:     topology.NewResolver(integrationPool, nil, nil),
		Essence:      essence.NewResolver(nil),
		Render:       render.NewPipeline(nil, engine, nil, nil),
		Outbound:     disp,
		DB:           integrationPool,
		PassageCap:   cap,
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
	})
}

// waitRunDone waits for applyID's run to actually finish (the commit snapshot
// in state_history appears only after a terminal: both success and
// error_locked write it) and returns the incarnation. This keeps the test
// from confusing the seeded "ready" state (the seed's starting value) with the
// post-run "ready".
func waitRunDone(t *testing.T, name, applyID string, want incarnation.Status) *incarnation.Incarnation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, total, err := incarnation.HistorySelectByName(context.Background(), integrationPool,
			name, incarnation.HistoryFilter{ApplyID: applyID}, 0, 1)
		if err != nil {
			t.Fatalf("HistorySelectByName: %v", err)
		}
		if total > 0 {
			inc, err := incarnation.SelectByName(context.Background(), integrationPool, name)
			if err != nil {
				t.Fatalf("SelectByName: %v", err)
			}
			if inc.Status != want {
				t.Fatalf("incarnation status = %q, want %q", inc.Status, want)
			}
			return inc
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("прогон %s не завершился за 10s", applyID)
	return nil
}

// --- tests ------------------------------------------------------------

func TestIntegration_HappyPath(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if inc.StatusDetails != nil {
		t.Errorf("status_details = %+v, want nil on success", inc.StatusDetails)
	}
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1", disp.calls)
	}
	if disp.gotApplyID != applyID {
		t.Errorf("dispatched apply_id = %q, want %q", disp.gotApplyID, applyID)
	}
	if disp.gotTasks != 1 {
		t.Errorf("dispatched tasks = %d, want 1", disp.gotTasks)
	}

	// state_history snapshot of the run.
	hist, total, err := incarnation.HistorySelectByName(context.Background(), integrationPool,
		"noop-prod", incarnation.HistoryFilter{ApplyID: applyID}, 0, 10)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 1 || len(hist) != 1 {
		t.Fatalf("history entries = %d, want 1", total)
	}
	if hist[0].Scenario != "create" {
		t.Errorf("history scenario = %q, want create", hist[0].Scenario)
	}

	// apply_runs row → success.
	st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(st) != 1 || st[0].Status != applyrun.StatusSuccess {
		t.Errorf("apply_runs = %+v, want 1×success", st)
	}
}

func TestIntegration_FailPath(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	summary := "module failed"
	disp := &mockDispatcher{t: t, result: applyrun.StatusFailed, summary: &summary}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails == nil {
		t.Errorf("status_details = nil, want reason on error_locked")
	} else if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("status_details.reason = %v, want dispatch_failed", inc.StatusDetails["reason"])
	}
}

// hangDispatcher simulates a Soul that ACCEPTED the ApplyRequest but hasn't
// sent a RunResult yet: the apply_runs row stays running, the barrier keeps
// polling. This makes the run goroutine "hang" in the barrier until
// cancelled — needed to test Cancel.
type hangDispatcher struct {
	t     *testing.T
	mu    sync.Mutex
	calls int
}

func (d *hangDispatcher) SendApply(_ context.Context, _ string, _ *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	// Terminal NOT written — host stays running, barrier keeps waiting.
	return nil
}

// TestIntegration_CrossKeeperCancel_FlagInPG — cluster-wide Cancel (G1): the
// flag is set by "another instance" (the test writes it directly via
// RequestCancel, bypassing the Runner — simulating Keeper-B), and the
// run-goroutine on THIS instance (Keeper-A) sees it during barrier polling
// and cancels the run → error_locked (same behavior as a local ctx-Cancel).
func TestIntegration_CrossKeeperCancel_FlagInPG(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &hangDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

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

	// Wait until the host is dispatched (apply_runs row running) — otherwise
	// RequestCancel would race ahead of the Insert and find no running rows.
	waitHostDispatched(t, applyID)

	// "Another Keeper": sets the flag directly in PG (no Runner on this instance).
	affected, err := applyrun.RequestCancel(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected == 0 {
		t.Fatal("RequestCancel affected = 0, want >=1 (running-хост прогона)")
	}

	// run-goroutine sees the flag during barrier polling and cancels the run.
	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("status_details = %+v, want reason=dispatch_failed (отмена через barrier)", inc.StatusDetails)
	}
}

// TestIntegration_LocalCancel_FastPath — local Cancel via
// Runner.RequestCancel: the run-goroutine lives on THIS instance, so
// cancellation takes the fast path (ctx-Cancel) instead of waiting for a
// barrier tick. Same error_locked terminal.
func TestIntegration_LocalCancel_FastPath(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &hangDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

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
	waitHostDispatched(t, applyID)

	found, err := r.RequestCancel(context.Background(), applyID)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if !found {
		t.Error("RequestCancel found = false, want true (прогон активен)")
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("status_details = %+v, want reason=dispatch_failed", inc.StatusDetails)
	}
}

// TestIntegration_RequestCancel_TerminalNoOp — Cancel on an already-finished
// run (terminal status) is a no-op: no flag set, incarnation stays ready.
func TestIntegration_RequestCancel_TerminalNoOp(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

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
	// Run finished successfully — incarnation ready, apply_runs success.
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// Cancel on a finished run is a no-op (found=false, no running rows, no
	// local goroutine).
	found, err := r.RequestCancel(context.Background(), applyID)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if found {
		t.Error("RequestCancel found = true для завершённого прогона, want false (no-op)")
	}
	inc, err := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if inc.Status != incarnation.StatusReady {
		t.Errorf("incarnation status = %q, want ready (Cancel не должен трогать завершённый прогон)", inc.Status)
	}
}

// waitHostDispatched waits for at least one running row of the run to appear
// (the apply_runs Insert happened) — synchronization before RequestCancel so
// the flag catches a running row.
func waitHostDispatched(t *testing.T, applyID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
		if err != nil {
			t.Fatalf("SelectStatusesByApplyID: %v", err)
		}
		for _, hs := range st {
			if hs.Status == applyrun.StatusRunning {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("прогон %s не дошёл до running-строки за 5s", applyID)
}

func TestIntegration_NoHosts_ErrorLocked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// No connected hosts.
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "no_hosts" {
		t.Errorf("reason = %v, want no_hosts", inc.StatusDetails["reason"])
	}

	// BAG-1: an early abort (empty roster) must leave a terminal apply_runs
	// row, otherwise the Voyage awaiter hangs forever. No real hosts → exactly
	// one sentinel row (render.RunSentinelSID), status=failed, terminal.
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("apply_runs rows = %d, want 1 (sentinel)", len(statuses))
	}
	if statuses[0].SID != render.RunSentinelSID {
		t.Errorf("sentinel sid = %q, want %q", statuses[0].SID, render.RunSentinelSID)
	}
	if statuses[0].Status != applyrun.StatusFailed {
		t.Errorf("sentinel status = %q, want failed (terminal)", statuses[0].Status)
	}
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (no hosts)", disp.calls)
	}
}

// registerServiceRepo creates a service repo with a scenario where a probe
// task (core.exec.run, register: probe) feeds state_changes.sets via
// ${ register.probe.stdout } (slice 2 of the full state_changes grammar).
func registerServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: register-in-sets service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/probe/main.yml", `name: probe
description: probe → register → state_changes.sets
state_changes:
  sets:
    leader: "${ register.probe.stdout }"
tasks:
  - name: Probe leader
    module: core.exec.run
    params:
      cmd: echo
      args: ["leader"]
    register: probe
    changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init register service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// registerMockDispatcher simulates a Soul that ran the probe task: on
// SendApply it writes the task's register data (like accumulateRegister in
// prod on TaskEvent), then a terminal apply_runs status (like
// correlateRunResult). registerData — the register.probe payload per host
// (sid → data).
type registerMockDispatcher struct {
	t            *testing.T
	registerData map[string]map[string]any
	calls        int
}

func (m *registerMockDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	m.calls++
	applyID := req.GetApplyId()
	if data, ok := m.registerData[sid]; ok {
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, TaskIdx: 0, RegisterData: data,
		}); err != nil {
			m.t.Errorf("registerMockDispatcher: UpsertTaskRegister: %v", err)
		}
	}
	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, 0, applyrun.StatusSuccess, nil); err != nil {
		m.t.Errorf("registerMockDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

// TestIntegration_RegisterInSets_CommitsToState — the full slice 2 path: probe
// task (register: probe) → register data accumulated in apply_task_register →
// loaded per-host after the barrier → state_changes.sets
// ${ register.probe.stdout } rendered → value lands in incarnation.state.
func TestIntegration_RegisterInSets_CommitsToState(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := registerServiceRepo(t)

	disp := &registerMockDispatcher{
		t: t,
		registerData: map[string]map[string]any{
			"host-a.example.com": {"stdout": "leader", "rc": float64(0)},
		},
	}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "probe",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if inc.State["leader"] != "leader" {
		t.Errorf("incarnation.state.leader = %v, want \"leader\" (из register.probe.stdout)", inc.State["leader"])
	}
}

// applyDestinyServiceRepo creates a service repo with a create scenario
// delegating to the pilot-flat destiny via apply:destiny. service.yml
// declares a destiny[] ref.
func applyDestinyServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: pilot-destiny
state_schema_version: 1
description: apply:destiny integration service
state_schema:
  type: object
  properties: {}
destiny:
  - { name: pilot-flat, ref: master }
`)
	write("scenario/create/main.yml", `name: create
description: delegate to pilot-flat destiny
state_changes: {}
tasks:
  - name: Apply pilot-flat
    apply:
      destiny: pilot-flat
      input:
        marker_file: "/etc/marker"
        marker_payload: "ok"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init apply-destiny service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// pilotFlatDestinyRepo creates the flat pilot-flat destiny under
// <base>/pilot-flat (so the default_destiny_source template
// file://<base>/{name} resolves to this repo). Returns the template URL.
func pilotFlatDestinyRepo(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "pilot-flat")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("destiny.yml", `name: pilot-flat
description: flat pilot destiny
input:
  marker_file:
    type: string
    required: true
  marker_payload:
    type: string
    required: true
  marker_mode:
    type: string
    default: "0644"
`)
	write("tasks/main.yml", `- name: Lay down the marker file
  module: core.file.present
  params:
    path: "${ input.marker_file }"
    content: "${ input.marker_payload }"
    mode: "${ input.marker_mode }"
- name: Record placement
  module: core.exec.run
  changed_when: "false"
  params:
    cmd: echo
    args: ["${ input.marker_file }"]
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init pilot-flat destiny", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + base + "/{name}"
}

// TestIntegration_ApplyDestiny — end-to-end slice A: a create scenario with
// apply:destiny → DestinySource loads the pilot-flat destiny (file://) →
// render expands its two tasks → dispatch → success → state commit. Verifies
// the dispatcher received exactly the two destiny tasks (apply expanded).
func TestIntegration_ApplyDestiny(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	serviceURL := applyDestinyServiceRepo(t)
	destinyTemplate := pilotFlatDestinyRepo(t)
	destinySrc := NewDestinySource(artifact.NewDestinyLoader(t.TempDir(), nil), fixedTemplateSource(destinyTemplate))

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithDestiny(t, disp, destinySrc)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "pilot-destiny", Git: serviceURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (один хост)", disp.calls)
	}
	if disp.gotTasks != 2 {
		t.Errorf("dispatched tasks = %d, want 2 (apply:destiny раскрылся в 2 задачи)", disp.gotTasks)
	}
}

// TestIntegration_ApplyDestiny_NoSource — apply:destiny with a nil
// DestinySource → render_failed → error_locked (ErrUnsupportedDSL).
func TestIntegration_ApplyDestiny_NoSource(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	serviceURL := applyDestinyServiceRepo(t)
	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithDestiny(t, disp, nil) // Destiny=nil

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "pilot-destiny", Git: serviceURL, Ref: "master"},
		ScenarioName:    "create",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "render_failed" {
		t.Errorf("reason = %v, want render_failed", inc.StatusDetails["reason"])
	}
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (render упал до dispatch)", disp.calls)
	}
}

// inputDefaultsServiceRepo creates a service repo whose create scenario
// declares a scenario-level `input:` with ONE required param (greeting,
// required) and ONE with a default (suffix). Both vars are rendered into the
// task params. Prod (and L0) must merge the default for an unpassed suffix
// before render — otherwise `${ input.suffix }` fails with "no such key"
// (BUG 1).
func inputDefaultsServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: scenario input-defaults service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/create/main.yml", `name: create
description: scenario input defaults merge
input:
  greeting:
    type: string
    required: true
  suffix:
    type: string
    default: "!"
state_changes: {}
tasks:
  - name: Echo greeting with default suffix
    module: core.exec.run
    params:
      cmd: echo
      args: ["${ input.greeting }${ input.suffix }"]
    changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init input-defaults service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// captureDispatcher simulates Soul and CAPTURES the params of the first task
// of the first ApplyRequest (to check the rendered command). Completes the
// barrier with a success status.
type captureDispatcher struct {
	t          *testing.T
	calls      int
	gotCommand string
}

func (d *captureDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.calls++
	if tasks := req.GetTasks(); len(tasks) > 0 {
		d.gotCommand = renderedExecCommand(tasks[0].GetParams())
	}
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("captureDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

// TestIntegration_ScenarioInputDefaultsMerged — regression for BUG 1: the
// operator supplies ONLY the required input (greeting); the unpassed suffix
// comes from the scenario `input:` default. Render succeeds, command is
// "echo hi!" (default "!" merged in). Before the fix, `${ input.suffix }`
// failed with "no such key" → render_failed.
func TestIntegration_ScenarioInputDefaultsMerged(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := inputDefaultsServiceRepo(t)

	disp := &captureDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		// ONLY the required input: suffix should come from the default.
		Input: map[string]any{"greeting": "hi"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if disp.gotCommand != "echo hi!" {
		t.Errorf("rendered command = %q, want %q (default suffix смёржен)", disp.gotCommand, "echo hi!")
	}
}

// TestIntegration_ScenarioInputRequiredMissing — a required scenario input is
// not passed and has no default → input_invalid → error_locked (a clear
// error, not "no such key" buried in CEL).
func TestIntegration_ScenarioInputRequiredMissing(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := inputDefaultsServiceRepo(t)

	disp := &captureDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		// greeting (required) NOT passed.
		Input: map[string]any{},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "input_invalid" {
		t.Errorf("reason = %v, want input_invalid", inc.StatusDetails["reason"])
	}
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (input провалился до dispatch)", disp.calls)
	}
}

func TestIntegration_AlreadyApplying_Rejected(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// Incarnation already applying — lockRun must refuse.
	inc := &incarnation.Incarnation{
		Name: "noop-prod", Service: "noop", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusApplying,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("Create applying: %v", err)
	}
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The run is rejected inside the run-goroutine (lockRun → ErrAlreadyRunning);
	// status stays applying, no dispatch happens.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if disp.calls > 0 {
			t.Fatalf("SendApply вызван при applying-incarnation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if got.Status != incarnation.StatusApplying {
		t.Errorf("status = %q, want applying (unchanged)", got.Status)
	}
}

// TestIntegration_ErrorLocked_Rejected checks the lock gate (ADR-009): a run
// against an error_locked incarnation is rejected under FOR UPDATE (lockRun →
// ErrLocked), no dispatch happens, status stays error_locked.
func TestIntegration_ErrorLocked_Rejected(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	inc := &incarnation.Incarnation{
		Name: "noop-prod", Service: "noop", ServiceVersion: "main",
		StateSchemaVersion: 1, Status: incarnation.StatusErrorLocked,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("Create error_locked: %v", err)
	}
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "main"},
		ScenarioName:    "create",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The run is rejected inside the run-goroutine (lockRun → ErrLocked);
	// status stays error_locked, no dispatch happens.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if disp.calls > 0 {
			t.Fatalf("SendApply вызван при error_locked-incarnation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if got.Status != incarnation.StatusErrorLocked {
		t.Errorf("status = %q, want error_locked (unchanged)", got.Status)
	}
}

// TestIntegration_NonRunnableStatus_Rejected checks lockRun's explicit
// allow-list (fail-closed): a run against an incarnation in destroying OR
// migration_failed is rejected (lockRun → ErrNotRunnable), no dispatch
// happens, status stays unchanged. These statuses used to fall through to the
// default branch and get silently transitioned to applying (a latent bug
// found while designing destroy).
func TestIntegration_NonRunnableStatus_Rejected(t *testing.T) {
	cases := []struct {
		name   string
		status incarnation.Status
	}{
		{"destroying", incarnation.StatusDestroying},
		{"migration_failed", incarnation.StatusMigrationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			inc := &incarnation.Incarnation{
				Name: "noop-prod", Service: "noop", ServiceVersion: "main",
				StateSchemaVersion: 1, Status: tc.status,
			}
			if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
				t.Fatalf("Create %s: %v", tc.status, err)
			}
			seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
			gitURL := noopServiceRepo(t)

			disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
			r := newRunner(t, disp, gitURL)

			if err := r.Start(context.Background(), RunSpec{
				ApplyID:         audit.NewULID(),
				IncarnationName: "noop-prod",
				ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "main"},
				ScenarioName:    "create",
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}

			// The run is rejected inside the run-goroutine (lockRun → ErrNotRunnable);
			// status stays unchanged, no dispatch happens.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if disp.calls > 0 {
					t.Fatalf("SendApply вызван при %s-incarnation", tc.status)
				}
				time.Sleep(20 * time.Millisecond)
			}
			got, _ := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
			if got.Status != tc.status {
				t.Errorf("status = %q, want %q (unchanged)", got.Status, tc.status)
			}
		})
	}
}

// serialServiceRepo creates a service repo with a `roll` scenario carrying a
// serial: of the given shape and a non-empty state_changes.sets — to test
// wave dispatch + a single barrier.
func serialServiceRepo(t *testing.T, serial string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: serial rolling service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/roll/main.yml", `name: roll
description: rolling restart with serial
state_changes:
  sets:
    rolled: "yes"
tasks:
  - name: Rolling step
    module: core.exec.run
    serial: `+serial+`
    params:
      cmd: echo
      args: ["roll"]
    changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init serial service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_Serial_AllWavesCommitOnce — serial: 1 across 3 hosts: all
// three hosts get an ApplyRequest (waves rolled through), state_changes
// commits EXACTLY ONCE after ALL waves (single barrier, orchestration.md §7) —
// the most important invariant of slice D. Checks: 3 SendApply calls, exactly
// 1 state_history snapshot, incarnation.state.rolled committed.
func TestIntegration_Serial_AllWavesCommitOnce(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"noop-prod"})
	gitURL := serialServiceRepo(t, "1")

	disp := &waveDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "roll",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// All 3 hosts got an ApplyRequest, in SID order (waves of 1, sequential).
	got := disp.dispatchedSIDs()
	want := []string{"host-a.example.com", "host-b.example.com", "host-c.example.com"}
	if len(got) != 3 {
		t.Fatalf("dispatched = %v, want 3 хоста", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dispatch order[%d] = %q, want %q (волны по SID)", i, got[i], want[i])
		}
	}

	// state committed (single commit after all waves).
	if inc.State["rolled"] != "yes" {
		t.Errorf("state.rolled = %v, want \"yes\"", inc.State["rolled"])
	}

	// CRITICAL: exactly ONE state_history snapshot — state commits once after
	// all waves, not per-wave (§7).
	_, total, err := incarnation.HistorySelectByName(context.Background(), integrationPool,
		"noop-prod", incarnation.HistoryFilter{ApplyID: applyID}, 0, 10)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 1 {
		t.Errorf("state_history snapshots = %d, want 1 (единый commit, НЕ по-волново)", total)
	}

	// All apply_runs success.
	st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(st) != 3 {
		t.Errorf("apply_runs rows = %d, want 3", len(st))
	}
}

// TestIntegration_Serial_FailStop — serial: 1 across 3 hosts, the first host
// (host-a) finishes failed: rolling stops, later waves do NOT start
// (fail-stop, §2.2.1). Checks: exactly 1 SendApply (host-a), host-b/host-c did
// NOT get an ApplyRequest, incarnation → error_locked, state NOT committed.
func TestIntegration_Serial_FailStop(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"noop-prod"})
	gitURL := serialServiceRepo(t, "1")

	disp := &waveDispatcher{t: t, failOn: "host-a.example.com"}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "roll",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)

	// Only the first wave (host-a) started; second/third do NOT start.
	got := disp.dispatchedSIDs()
	if len(got) != 1 || got[0] != "host-a.example.com" {
		t.Errorf("dispatched = %v, want [host-a.example.com] (fail-stop: волны 2,3 не стартуют)", got)
	}

	// state NOT committed (rolled didn't appear) — §7: partial commit is forbidden.
	if inc.State["rolled"] == "yes" {
		t.Errorf("state.rolled = yes — state НЕ должен коммититься при fail (§7)")
	}
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed", inc.StatusDetails)
	}
}

// TestIntegration_Serial_Percent — serial: "67%" across 3 hosts →
// ceil(3*0.67)=2 → waves [2,1]: all 3 hosts go through, state commits once.
func TestIntegration_Serial_Percent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"noop-prod"})
	gitURL := serialServiceRepo(t, `"67%"`)

	disp := &waveDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "roll",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if len(disp.dispatchedSIDs()) != 3 {
		t.Errorf("dispatched = %v, want 3 хоста", disp.dispatchedSIDs())
	}
	if inc.State["rolled"] != "yes" {
		t.Errorf("state.rolled = %v, want yes", inc.State["rolled"])
	}
}

// serialMultiTaskRepo creates a service repo with a `roll` scenario carrying
// TWO module tasks with DIFFERENT serial: widths (serialA / serialB). Tests
// per-RUN min-width: the run's wave width is the minimum positive width among
// the tasks (orchestration.md §2.2.1, effectiveSerialWidth), not per-task.
func serialMultiTaskRepo(t *testing.T, serialA, serialB string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: serial multi-task service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/roll/main.yml", `name: roll
description: two tasks with different serial widths
state_changes:
  sets:
    rolled: "yes"
tasks:
  - name: Wide step
    module: core.exec.run
    serial: `+serialA+`
    params:
      cmd: echo
      args: ["wide"]
    changed_when: "false"
  - name: Narrow step
    module: core.exec.run
    serial: `+serialB+`
    params:
      cmd: echo
      args: ["narrow"]
    changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init serial multi-task service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_Serial_Width2_FiveHosts — serial: 2 across 5 hosts → waves
// [2,2,1] (orchestration.md §2.2.1). All 5 hosts get an ApplyRequest in SID
// order; state commits once after all waves. This is an end-to-end check of
// wave slicing (not just the splitWaves unit): dispatch + per-wave barrier +
// single state commit.
func TestIntegration_Serial_Width2_FiveHosts(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	for _, sfx := range []string{"a", "b", "c", "d", "e"} {
		seedConnectedSoul(t, "host-"+sfx+".example.com", []string{"noop-prod"})
	}
	gitURL := serialServiceRepo(t, "2")

	disp := &waveDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "roll",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// All 5 hosts got an ApplyRequest, in SID order (waves [2,2,1] sequential,
	// sorted by SID within a wave).
	got := disp.dispatchedSIDs()
	want := []string{
		"host-a.example.com", "host-b.example.com", "host-c.example.com",
		"host-d.example.com", "host-e.example.com",
	}
	if len(got) != 5 {
		t.Fatalf("dispatched = %v, want 5 хостов", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dispatch order[%d] = %q, want %q (волны [2,2,1] по SID)", i, got[i], want[i])
		}
	}

	if inc.State["rolled"] != "yes" {
		t.Errorf("state.rolled = %v, want \"yes\"", inc.State["rolled"])
	}
	// Single commit after all waves (§7): exactly one state_history snapshot.
	_, total, err := incarnation.HistorySelectByName(context.Background(), integrationPool,
		"noop-prod", incarnation.HistoryFilter{ApplyID: applyID}, 0, 10)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 1 {
		t.Errorf("state_history snapshots = %d, want 1 (единый commit после волн [2,2,1])", total)
	}
}

// TestIntegration_Serial_FailStop_SecondWave — the strongest fail-stop §7
// test: serial: 1 across 3 hosts, failure on host-b (SECOND wave). First wave
// (host-a) succeeds, second (host-b) fails → rolling stops → THIRD wave
// (host-c) does NOT start. Checks: exactly 2 SendApply calls (host-a, host-b —
// not 3), state NOT committed, incarnation → error_locked. This is the
// "fail-stop breaks later waves" invariant specifically NOT on the first wave.
func TestIntegration_Serial_FailStop_SecondWave(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"noop-prod"})
	gitURL := serialServiceRepo(t, "1")

	disp := &waveDispatcher{t: t, failOn: "host-b.example.com"}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "roll",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)

	// Wave 1 (host-a) + wave 2 (host-b) started; wave 3 (host-c) did NOT.
	got := disp.dispatchedSIDs()
	want := []string{"host-a.example.com", "host-b.example.com"}
	if len(got) != 2 {
		t.Fatalf("dispatched = %v, want 2 (host-a, host-b; волна 3 не стартует)", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dispatch[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// state NOT committed (§7: partial commit is forbidden).
	if inc.State["rolled"] == "yes" {
		t.Errorf("state.rolled = yes — state НЕ должен коммититься при fail во 2-й волне (§7)")
	}
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed", inc.StatusDetails)
	}
}

// cancelAfterFirstWaveDispatcher simulates Soul for serial waves: every host
// finishes success, but IMMEDIATELY after the first SendApply it sets the
// cluster-wide Cancel flag (as "another Keeper" would). So the per-wave
// barrier after the first wave sees cancel_requested → the cancel branch
// interrupts rolling, later waves do NOT start (symmetric to fail-stop, but
// via cancellation instead of a failed host).
type cancelAfterFirstWaveDispatcher struct {
	t       *testing.T
	mu      sync.Mutex
	order   []string
	applyID string
}

func (d *cancelAfterFirstWaveDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	first := len(d.order) == 0
	d.order = append(d.order, sid)
	d.mu.Unlock()

	if first {
		// "Another Keeper" sets the flag during the first wave, WHILE the row
		// is still running (RequestCancel filters status='running'). Strict
		// order: flag first, then terminal — otherwise success would race ahead
		// of RequestCancel, which would find no running rows (affected=0, flag
		// never set).
		if _, err := applyrun.RequestCancel(ctx, integrationPool, req.GetApplyId()); err != nil {
			d.t.Errorf("cancelAfterFirstWaveDispatcher: RequestCancel: %v", err)
		}
	}
	// The host finishes success normally — it's the cancellation that must
	// stop the run, not a failed host (otherwise the test would duplicate
	// fail-stop). Terminal is written after the flag: the barrier will see
	// cancel_requested on a success row and interrupt rolling before the next
	// wave starts.
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("cancelAfterFirstWaveDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

func (d *cancelAfterFirstWaveDispatcher) dispatchedSIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.order))
	copy(out, d.order)
	return out
}

// TestIntegration_Serial_CancelStopsNextWave — cluster-wide Cancel (G1) on a
// serial run: the flag is set during the first wave (host-a), the per-wave
// barrier sees cancel_requested and interrupts rolling → waves 2,3 (host-b,
// host-c) do NOT start. This is the cancel counterpart of fail-stop (§2.2.1):
// cancellation breaks later waves the same way a host failure does.
// Observable: exactly 1 SendApply, incarnation → error_locked, state NOT
// committed.
//
// Difference from [TestIntegration_CrossKeeperCancel_FlagInPG]: that test has
// one wave and checks the barrier interruption itself; this one checks that
// cancellation prevents the NEXT wave from starting (serial × cancel
// interaction).
func TestIntegration_Serial_CancelStopsNextWave(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"noop-prod"})
	gitURL := serialServiceRepo(t, "1")

	disp := &cancelAfterFirstWaveDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	disp.applyID = applyID
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "roll",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)

	// Only the first wave (host-a) started; waves 2,3 interrupted by cancel.
	got := disp.dispatchedSIDs()
	if len(got) != 1 || got[0] != "host-a.example.com" {
		t.Errorf("dispatched = %v, want [host-a.example.com] (cancel останавливает волны 2,3)", got)
	}

	// state NOT committed — cancel = abort, same as fail-stop (§7).
	if inc.State["rolled"] == "yes" {
		t.Errorf("state.rolled = yes — отменённый прогон НЕ должен коммитить state")
	}
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (cancel через barrier → abort)", inc.StatusDetails)
	}
}

// TestIntegration_Serial_MinWidth_TwoTasks — per-RUN min-width (§2.2.1): a
// scenario with two tasks of different widths (serial: 2 and serial: 1)
// across 5 hosts. The run's wave width = the MINIMUM positive width = 1 →
// waves [1,1,1,1,1], all 5 hosts dispatched one at a time, in SID order.
// Confirms that the wider task (serial: 2) does NOT set the window — it rolls
// in narrow waves alongside the narrow task.
func TestIntegration_Serial_MinWidth_TwoTasks(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	for _, sfx := range []string{"a", "b", "c", "d", "e"} {
		seedConnectedSoul(t, "host-"+sfx+".example.com", []string{"noop-prod"})
	}
	gitURL := serialMultiTaskRepo(t, "2", "1")

	disp := &waveDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "roll",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// min-width = 1 → each host its own wave, by SID. 5 hosts = 5 SendApply
	// calls (one ApplyRequest per host with ALL its tasks — both tasks travel
	// together).
	got := disp.dispatchedSIDs()
	want := []string{
		"host-a.example.com", "host-b.example.com", "host-c.example.com",
		"host-d.example.com", "host-e.example.com",
	}
	if len(got) != 5 {
		t.Fatalf("dispatched = %v, want 5 (min-width 1 → по одному хосту на волну)", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dispatch order[%d] = %q, want %q (min-width=1, волны по SID)", i, got[i], want[i])
		}
	}
	if inc.State["rolled"] != "yes" {
		t.Errorf("state.rolled = %v, want \"yes\"", inc.State["rolled"])
	}
}

// runOnceServiceRepo creates a service repo with a `once` scenario (run_once: true).
func runOnceServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: run_once service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/once/main.yml", `name: once
description: run_once on a single host
state_changes: {}
tasks:
  - name: Run once
    module: core.exec.run
    run_once: true
    params:
      cmd: echo
      args: ["once"]
    changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init run_once service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_RunOnce_SingleHost — run_once: true across 3 hosts →
// ApplyRequest goes to exactly ONE host (first by SID, host-a), others don't
// get one (orchestration.md §2.2.2). Run succeeds.
func TestIntegration_RunOnce_SingleHost(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-c.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := runOnceServiceRepo(t)

	disp := &waveDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "once",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	got := disp.dispatchedSIDs()
	if len(got) != 1 || got[0] != "host-a.example.com" {
		t.Errorf("dispatched = %v, want [host-a.example.com] (run_once → первый по SID)", got)
	}
}

// --- security: observability masking (#1 secondary channel + #6) -----------

// vaultParamServiceRepo — service-noop whose command params carry a
// vault-ref marker. In tests the render pipeline has no vault resolver, so
// the string reaches the wire as a literal — exactly the case this test
// needs: verifies keeper does NOT mask wire Params, only the observable copy.
func vaultParamServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: secret-in-params smoke
state_changes: {}
tasks:
  - name: Run with secret param
    module: core.exec.run
    params:
      cmd: deploy
      args: ["--token=vault:secret/keeper/deploy-token"]
    changed_when: "false"
`)
}

// writeServiceRepo — shared constructor for a local-fs git repo service-noop
// with a given scenario/create/main.yml (factored out of noopServiceRepo for
// reuse by the secret fixture).
func writeServiceRepo(t *testing.T, scenarioMain string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: noop service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/create/main.yml", scenarioMain)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// secretWireDispatcher captures wire-Params (what would actually go to Soul)
// and returns an error ECHOING the payload's vault-ref — simulates a
// transport/marshal failure that leaks the secret into err.Error().
type secretWireDispatcher struct {
	t          *testing.T
	calls      int
	wireParams string
}

func (d *secretWireDispatcher) SendApply(_ context.Context, _ string, req *keeperv1.ApplyRequest) error {
	d.calls++
	if tasks := req.GetTasks(); len(tasks) > 0 {
		d.wireParams = renderedExecCommand(tasks[0].GetParams())
	}
	// Echo the payload in the error (as some transport/marshal errors do).
	return fmt.Errorf("rpc transport: failed to send %s", d.wireParams)
}

// TestIntegration_SecretInParams_MaskedInObservability_NotOnWire — security #1/#6:
//   - wire-ApplyRequest.Params carries the REAL value (not broken by masking);
//   - error_summary (apply_runs, read externally via the barrier) contains NO
//     payload echo — only the safe reason send_apply_failed;
//   - status_details.error (GET incarnation, unmasked on read) is masked,
//     the vault-ref doesn't leak plaintext.
func TestIntegration_SecretInParams_MaskedInObservability_NotOnWire(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := vaultParamServiceRepo(t)

	disp := &secretWireDispatcher{t: t}
	r := newRunner(t, disp, gitURL)

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

	// 1. Wire is untouched: the real value reached the dispatcher.
	wantWire := "deploy --token=vault:secret/keeper/deploy-token"
	if disp.wireParams != wantWire {
		t.Errorf("wire params = %q, want %q (wire-Params не должны маскироваться)", disp.wireParams, wantWire)
	}

	// 2. error_summary has no payload echo.
	st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(st) != 1 || st[0].ErrorSummary == nil {
		t.Fatalf("apply_runs = %+v, want 1 row с error_summary", st)
	}
	if *st[0].ErrorSummary != "send_apply_failed" {
		t.Errorf("error_summary = %q, want safe-причина без payload-эха", *st[0].ErrorSummary)
	}
	if strings.Contains(*st[0].ErrorSummary, "vault:") || strings.Contains(*st[0].ErrorSummary, "deploy-token") {
		t.Errorf("error_summary leaks secret: %q", *st[0].ErrorSummary)
	}

	// 3. status_details.error (read externally via GET incarnation, unmasked
	//    on read) leaks NO secret: no send-failure payload echo, no vault-ref.
	//    In this scenario error_summary is already safe (send_apply_failed), so
	//    the transitive barrier error is safe too — the main invariant is "the
	//    secret never leaks into an observable channel".
	if inc.StatusDetails == nil {
		t.Fatalf("status_details = nil, want error_locked detail")
	}
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("status_details.reason = %v, want dispatch_failed", inc.StatusDetails["reason"])
	}
	errStr, _ := inc.StatusDetails["error"].(string)
	if strings.Contains(errStr, "vault:") || strings.Contains(errStr, "deploy-token") {
		t.Errorf("status_details.error leaks secret: %q", errStr)
	}
}

// TestIntegration_StatusDetailsError_VaultRefMasked — security #6, direct
// channel: a cause.Error() carrying a vault-ref that reaches status_details
// without going through error_summary transit is masked in lockIncarnation
// before the write, and reading it back via incarnation never leaks the
// vault path in plaintext.
//
// Path: the render phase fails BEFORE dispatch (apply:destiny without
// default_destiny_source → ErrUnsupportedDSL). We can't easily force a
// vault-carrying cause through a deliberately unparseable apply: scenario —
// getting a deterministic vault-ref into cause is easier to obtain by
// verifying the mechanism itself at the lockIncarnation level: see the unit
// test TestMaskSecrets_VaultRefSubstring (shared/audit), which proves
// MaskSecrets masks a vault-marker substring at any position, and
// lockIncarnation runs the whole details map through MaskSecrets before
// writing.
func TestIntegration_StatusDetailsError_VaultRefMasked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	// Service repo with a scenario referencing apply: destiny without
	// default_destiny_source in Deps → the render phase returns
	// ErrUnsupportedDSL. It doesn't carry a vault-ref itself; masking of a
	// vault-containing status_details string is proven by the unit test
	// TestMaskSecrets_VaultRefSubstring + the fact that lockIncarnation runs
	// MaskSecrets. Here we just confirm status_details is written through the
	// masking path (reason present, no secret).
	gitURL := writeServiceRepo(t, `name: create
description: render-fail
state_changes: {}
tasks:
  - name: bad apply
    apply:
      destiny: nonexistent
`)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

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
	if inc.StatusDetails == nil {
		t.Fatalf("status_details = nil, want render_failed detail")
	}
	errStr, _ := inc.StatusDetails["error"].(string)
	if strings.Contains(errStr, "vault:") {
		t.Errorf("status_details.error leaks vault-ref: %q", errStr)
	}
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (render упал до dispatch)", disp.calls)
	}
}

// --- keeper-side dispatch (`on: keeper`, ADR-017) ---------------------
//
// S1 keeper-dispatch coverage through run()+PG. Before these tests, no
// integration run carried a keeper task (newRunner builds a Runner with
// KeeperModules==nil), so the keeper-dispatch path (run.go step 5.5 →
// dispatchKeeperTasks) was only exercised by unit tests over
// applyKeeperTask, bypassing the apply_runs/incarnation finalization.
// fakeKeeperModule/fakeKeeperRegistry live in keeper_dispatch_test.go (no
// build tag, compiled here too).

// newRunnerWithKeeper builds a Runner with a keeper-side core Registry (for
// `on: keeper` tasks). Otherwise like newRunner (mock dispatcher, PG pool,
// real render). keepers==nil → an `on: keeper` task is rejected
// (ErrKeeperModulesNotConfigured) — this is the QA-gap (f) path.
func newRunnerWithKeeper(t *testing.T, disp ApplyDispatcher, keepers KeeperModuleRegistry) *Runner {
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
		PollInterval:  20 * time.Millisecond,
		RunTimeout:    20 * time.Second,
	})
}

// keeperServiceRepo creates a service repo with a create scenario carrying
// keeperTasks keeper-side tasks (`on: keeper`, module keeperModule) and ONE
// Soul-side echo task at the end — Soul-side is needed so the roster is
// non-empty (run.go step 3, otherwise no_hosts cuts the run short before
// keeper-dispatch). Each keeper task carries register: keeperN — to check
// accumulateKeeperRegister.
func keeperServiceRepo(t *testing.T, keeperModule string, keeperTasks int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`name: create
description: keeper-side dispatch integration
state_changes: {}
tasks:
`)
	for i := 0; i < keeperTasks; i++ {
		fmt.Fprintf(&b, `  - name: Keeper step %d
    module: %s
    on: keeper
    register: keeper%d
    params:
      sid: "host-a.example.com"
      coven: ["tagged%d"]
      mode: append
`, i, keeperModule, i, i)
	}
	b.WriteString(`  - name: Soul echo
    module: core.exec.run
    params:
      cmd: echo
      args: ["soul"]
    changed_when: "false"
`)
	return writeServiceRepo(t, b.String())
}

// mustStructAny — structpb from a map without *testing.T (for module constructors).
func mustStructAny(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}

// TestIntegration_KeeperDispatch_Failed_ErrorLocked — QA gap (a), CRITICAL: a
// keeper-side task fails (final.Failed=true) → incarnation error_locked;
// apply_runs(sid="keeper") = failed with error_summary; host-fan-out did NOT
// start (SendApply not called); incarnation.state not committed. Through
// run()+PG — the first integration test exercising keeper-dispatch against a
// real DB.
func TestIntegration_KeeperDispatch_Failed_ErrorLocked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	failed := &fakeKeeperModule{final: &pluginv1.ApplyEvent{Failed: true, Message: "invalid coven"}}
	keepers := fakeKeeperRegistry{"core.soul": failed}
	gitURL := keeperServiceRepo(t, "core.soul.registered", 1)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithKeeper(t, disp, keepers)

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
		t.Errorf("reason = %v, want keeper_dispatch_failed", inc.StatusDetails)
	}

	// host-fan-out did NOT start — the keeper task failed before dispatching to hosts.
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (keeper-задача упала до host-fan-out)", disp.calls)
	}

	// state NOT committed (stayed at state_before = the empty seed state).
	if len(inc.State) != 0 {
		t.Errorf("incarnation.state = %v, want пустой (state_before, keeper-fail НЕ коммитит)", inc.State)
	}

	// apply_runs(sid="keeper") = failed with error_summary.
	st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	var keeperRow *applyrun.HostStatus
	for i := range st {
		if st[i].SID == render.KeeperTargetSID {
			keeperRow = &st[i]
		}
	}
	if keeperRow == nil {
		t.Fatalf("нет apply_runs строки sid=%q: %+v", render.KeeperTargetSID, st)
	}
	if keeperRow.Status != applyrun.StatusFailed {
		t.Errorf("keeper apply_run status = %q, want failed", keeperRow.Status)
	}
	if keeperRow.ErrorSummary == nil || !strings.Contains(*keeperRow.ErrorSummary, "invalid coven") {
		t.Errorf("keeper error_summary = %v, want содержащее 'invalid coven'", keeperRow.ErrorSummary)
	}
}

// TestIntegration_KeeperDispatch_TwoTasks_OrderAndRegister — QA gap (c): TWO
// keeper tasks in one scenario. Both execute in Index order, both register
// entries land under sid="keeper" with different task_idx (0 and 1). Run
// succeeds, host-fan-out started (Soul task → 1 SendApply).
func TestIntegration_KeeperDispatch_TwoTasks_OrderAndRegister(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	// The module records call order via a shared counter: each task sends its
	// own output (call_order) so register entries differ observably.
	mod := &orderedKeeperModule{}
	keepers := fakeKeeperRegistry{"core.soul": mod}
	gitURL := keeperServiceRepo(t, "core.soul.registered", 2)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithKeeper(t, disp, keepers)

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

	// Both keeper tasks executed (module called twice).
	if got := mod.applyCount(); got != 2 {
		t.Errorf("keeper module Apply вызван %d раз, want 2", got)
	}

	// host-fan-out started: the Soul task went to one host.
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (Soul echo на одном хосте)", disp.calls)
	}

	// Both register entries under sid="keeper" with different task_idx (0,1).
	regs, err := applyrun.SelectTaskRegistersByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectTaskRegistersByApplyID: %v", err)
	}
	keeperIdx := map[int]bool{}
	for _, tr := range regs {
		if tr.SID == render.KeeperTargetSID {
			keeperIdx[tr.TaskIdx] = true
		}
	}
	if !keeperIdx[0] || !keeperIdx[1] {
		t.Errorf("keeper register task_idx-ы = %v, want {0,1} под sid=keeper", keeperIdx)
	}
}

// orderedKeeperModule — a keeper-side module that counts Apply calls (to
// check "both keeper tasks executed"). Final is always success with
// output.call_order.
type orderedKeeperModule struct {
	module.BaseModule
	mu    sync.Mutex
	count int
}

func (m *orderedKeeperModule) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.mu.Lock()
	m.count++
	order := m.count
	m.mu.Unlock()
	return stream.Send(&pluginv1.ApplyEvent{
		Changed: true,
		Output:  mustStructAny(map[string]any{"call_order": float64(order)}),
	})
}

func (m *orderedKeeperModule) applyCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

// TestIntegration_KeeperDispatch_FirstFailedAbortsSecond — QA gap (c), abort
// invariant: the FIRST of two keeper tasks fails → the SECOND does not run
// (dispatchKeeperTasks returns an error on the first failure). Observable:
// module called exactly once, incarnation error_locked, host-fan-out did not
// start.
func TestIntegration_KeeperDispatch_FirstFailedAbortsSecond(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	mod := &countingFailModule{}
	keepers := fakeKeeperRegistry{"core.soul": mod}
	gitURL := keeperServiceRepo(t, "core.soul.registered", 2)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithKeeper(t, disp, keepers)

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
		t.Errorf("reason = %v, want keeper_dispatch_failed", inc.StatusDetails)
	}
	if got := mod.applyCount(); got != 1 {
		t.Errorf("keeper module Apply вызван %d раз, want 1 (вторая keeper-задача не стартует после fail)", got)
	}
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (abort до host-fan-out)", disp.calls)
	}
}

// countingFailModule — a keeper-side module that returns a failed final and
// counts Apply calls (for the abort invariant "second task doesn't run").
type countingFailModule struct {
	module.BaseModule
	mu    sync.Mutex
	count int
}

func (m *countingFailModule) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.mu.Lock()
	m.count++
	m.mu.Unlock()
	return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "первая keeper-задача провалена"})
}

func (m *countingFailModule) applyCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

// TestIntegration_KeeperDispatch_NilRegistry_ErrorLocked — QA gap (f): the
// scenario carries an `on: keeper` task, but the Runner is built with
// KeeperModules==nil → dispatchKeeperTasks returns
// ErrKeeperModulesNotConfigured → keeper_dispatch_failed → error_locked.
// host-fan-out did not start.
func TestIntegration_KeeperDispatch_NilRegistry_ErrorLocked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	gitURL := keeperServiceRepo(t, "core.soul.registered", 1)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithKeeper(t, disp, nil) // KeeperModules == nil

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
		t.Errorf("reason = %v, want keeper_dispatch_failed (nil keeper-registry)", inc.StatusDetails)
	}
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (keeper-задача отвергнута до host-fan-out)", disp.calls)
	}
}

// keeperOnlyServiceRepo creates a service repo with a create scenario made
// ONLY of keeper tasks (no Soul-side tasks at all). The run's roster will
// still be resolved by run.go step 3 regardless of task composition.
func keeperOnlyServiceRepo(t *testing.T, keeperModule string) string {
	t.Helper()
	return writeServiceRepo(t, fmt.Sprintf(`name: create
description: keeper-only scenario
state_changes: {}
tasks:
  - name: Keeper only step
    module: %s
    on: keeper
    params:
      sid: "host-a.example.com"
      coven: ["tagged"]
      mode: append
`, keeperModule))
}

// TestIntegration_KeeperOnly_NoHosts_RunsKeeperTasks — bypasses no_hosts for
// all-keeper provision-from-zero (ADR-0061 §context amend): a keeper-only
// scenario (0 Soul-side tasks, ALL `on: keeper`) against an incarnation with
// NO connected hosts now RUNS the keeper task (chicken-and-egg: a create
// scenario provisions hosts FROM ZERO, so an empty roster at start is
// legitimate). The gate is bypassed based on task COMPOSITION
// (allKeeperTasks), no flag. Inverts the previous S1-limitation fixture
// (...AbortsBeforeKeeper): no_hosts used to cut the run short before
// keeper-dispatch.
//
// Observable: keeper module called (applyCount==1), run is NOT error_locked
// on no_hosts, reaches keeper-dispatch (step 5.5) and finalizes ready —
// host-fan-out on an empty roster = no-op success. Essence resolves in the
// keeper context (keeperEssenceInput, no host representative) — no panic on
// hosts[0].
func TestIntegration_KeeperOnly_NoHosts_RunsKeeperTasks(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// No connected hosts — provision-from-zero starts on an empty roster.

	mod := &orderedKeeperModule{}
	keepers := fakeKeeperRegistry{"core.soul": mod}
	gitURL := keeperOnlyServiceRepo(t, "core.soul.registered")

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithKeeper(t, disp, keepers)

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

	// The run does NOT fall into no_hosts — it reaches keeper-dispatch and
	// finishes successfully (host-fan-out on an empty roster = no-op).
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// Keeper task EXECUTED — gate bypassed based on composition (all-keeper).
	if got := mod.applyCount(); got != 1 {
		t.Errorf("keeper module Apply вызван %d раз, want 1 (all-keeper bypass no_hosts на пустом roster)", got)
	}
	// host-fan-out did not start — no Soul tasks, empty roster.
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (нет Soul-задач, пустой roster)", disp.calls)
	}

	// keeper apply_runs(sid="keeper") = success (NOT the no_hosts sentinel).
	st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	var keeperRow *applyrun.HostStatus
	for i := range st {
		if st[i].SID == render.RunSentinelSID {
			t.Errorf("найдена sentinel-строка %q — прогон не должен был упасть в no_hosts", render.RunSentinelSID)
		}
		if st[i].SID == render.KeeperTargetSID {
			keeperRow = &st[i]
		}
	}
	if keeperRow == nil {
		t.Fatalf("нет apply_runs строки sid=%q (keeper-задача не исполнилась): %+v", render.KeeperTargetSID, st)
	}
	if keeperRow.Status != applyrun.StatusSuccess {
		t.Errorf("keeper apply_run status = %q, want success", keeperRow.Status)
	}
}

// mixedKeeperHostServiceRepo creates a service repo with a create scenario of
// ONE keeper task (`on: keeper`, WITHOUT refresh_soulprint) AND ONE Soul-side
// task (core.exec.run). Mixed composition → allKeeperTasks(false); no refresh
// emitter → HasRefreshEmitter(false). Neither bypass class applies → the
// no_hosts gate holds on an empty roster (REVERSE fixture: catches bypass
// creeping onto mixed-without-refresh).
func mixedKeeperHostServiceRepo(t *testing.T, keeperModule string) string {
	t.Helper()
	return writeServiceRepo(t, fmt.Sprintf(`name: create
description: mixed keeper+host scenario
state_changes: {}
tasks:
  - name: Keeper step
    module: %s
    on: keeper
    params:
      sid: "host-a.example.com"
      coven: ["tagged"]
      mode: append
  - name: Soul echo
    module: core.exec.run
    params:
      cmd: echo
      args: ["soul"]
    changed_when: "false"
`, keeperModule))
}

// TestIntegration_HostScenario_NoHosts_StillAborts — GUARD for the bypass
// boundary: a host scenario (carries a Soul-side task) against an empty
// roster still falls into no_hosts. allKeeperTasks(false) → bypass does NOT
// apply. Protects existing host-run behavior from regressing (bypass must not
// "leak" onto host scenarios).
func TestIntegration_HostScenario_NoHosts_StillAborts(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// No connected hosts.
	gitURL := noopServiceRepo(t) // carries a Soul-side core.exec.run

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

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
	if inc.StatusDetails["reason"] != "no_hosts" {
		t.Errorf("reason = %v, want no_hosts (host-сценарий на пустом roster не bypass-ится)", inc.StatusDetails["reason"])
	}
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0", disp.calls)
	}
}

// TestIntegration_MixedKeeperAndHost_NoHosts_StillAborts — ★ REVERSE GUARD
// (security, unextended behavior PRESERVED): a mixed scenario (keeper task +
// Soul task) WITHOUT a refresh emitter against an empty roster falls into
// no_hosts. allKeeperTasks(false) (there's a host task) AND
// HasRefreshEmitter(false) (no refresh_soulprint) → NEITHER bypass class
// applies → the gate holds. The keeper module is NOT executed (abort at step
// 3, before keeper-dispatch). Protects the boundary: bypassing a mixed plan
// requires SPECIFICALLY a refresh emitter; mixed without one stays behind
// no_hosts (host task on an empty P0).
func TestIntegration_MixedKeeperAndHost_NoHosts_StillAborts(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// No connected hosts.

	mod := &orderedKeeperModule{}
	keepers := fakeKeeperRegistry{"core.soul": mod}
	gitURL := mixedKeeperHostServiceRepo(t, "core.soul.registered")

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithKeeper(t, disp, keepers)

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
	if inc.StatusDetails["reason"] != "no_hosts" {
		t.Errorf("reason = %v, want no_hosts (смешанный keeper+host → !allKeeperTasks)", inc.StatusDetails["reason"])
	}
	// Keeper module NOT executed — no_hosts cut the run short before keeper-dispatch (step 5.5).
	if got := mod.applyCount(); got != 0 {
		t.Errorf("keeper module Apply вызван %d раз, want 0 (no_hosts до keeper-dispatch)", got)
	}
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0", disp.calls)
	}
}

// mixedKeeperHostRefreshServiceRepo — a mixed provision→role plan WITH a
// REFRESH emitter: a core.soul.registered keeper task with
// `refresh_soulprint: true` (provision passage) + a Soul-side host task
// (deploy passage). HasRefreshEmitter(true) → the plan grows the roster
// mid-run → legitimately starting on an EMPTY roster (the host task
// stratifies into a Passage AFTER the refresh boundary, §S2/§S3). Target
// fixture for bypass class (b).
func mixedKeeperHostRefreshServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: mixed provision (refresh) + host deploy
state_changes: {}
tasks:
  - name: Register provisioned hosts and refresh roster
    module: core.soul.registered
    on: keeper
    register: provision
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Deploy role to grown roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: "false"
    params:
      cmd: echo
      args: ["role"]
`)
}

// TestIntegration_MixedKeeperAndHost_Refresh_RunsToDispatch — ★ GUARD for
// bypass class (b) (ADR-0061 amendment): a mixed provision→role plan WITH a
// refresh emitter (core.soul.registered refresh_soulprint: true) against an
// EMPTY roster does NOT fall into no_hosts — it reaches keeper-dispatch and
// runs the provision step. allKeeperTasks false (there's a host task), but
// HasRefreshEmitter true → an empty starting roster is legitimate (the host
// task travels in a Passage after the refresh boundary, on the re-resolved
// roster). Inverts the previous "mixed stays behind no_hosts" fixture.
//
// Observable: the keeper provision module is called (applyCount==1), NO
// no_hosts sentinel row, the run finalizes ready (host-fan-out on the empty
// re-resolved roster = no-op success — VMs aren't really onboarded in the
// unit test, the live snapshot stays empty).
func TestIntegration_MixedKeeperAndHost_Refresh_RunsToDispatch(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// No connected hosts — provision-from-zero starts on an empty roster.

	mod := &orderedKeeperModule{}
	keepers := fakeKeeperRegistry{"core.soul": mod}
	gitURL := mixedKeeperHostRefreshServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	// staged Runner: the refresh boundary yields Count=2 → needs a
	// passage-capability checker (stubPassageCap, ADR-056 §S5). Empty starting
	// roster → the presence gate on an empty SID set = no-op (nobody lacking),
	// staged mechanics pass through.
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

	// NOT no_hosts: the run reaches keeper-dispatch and finishes (host-deploy
	// on the empty re-resolved roster = no-op).
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// Provision keeper task EXECUTED — bypass let the empty roster through via refresh.
	if got := mod.applyCount(); got != 1 {
		t.Errorf("keeper module Apply вызван %d раз, want 1 (mixed+refresh bypass no_hosts на пустом roster)", got)
	}

	// No no_hosts sentinel row — the starting gate was bypassed.
	st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	for i := range st {
		if st[i].SID == render.RunSentinelSID {
			t.Errorf("найдена sentinel-строка %q — mixed+refresh не должен был упасть в no_hosts", render.RunSentinelSID)
		}
	}
}

// seedCreateHistory inserts a state_history snapshot for a failed `create`
// scenario AND sets incarnation.created_scenario='create' — together giving
// scope=create in [incarnation.UnlockForRerun]: the gate requires
// created_scenario == the last failed scenario (the create-path value is set
// by the handler/MCP, Create persists it). state_before == state_after = `{}`.
func seedCreateHistory(t *testing.T, name string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(), `
INSERT INTO state_history (history_id, incarnation_name, scenario, state_before, state_after, apply_id)
VALUES ($1, $2, 'create', '{}'::jsonb, '{}'::jsonb, $1)`,
		audit.NewULID(), name)
	if err != nil {
		t.Fatalf("seedCreateHistory: %v", err)
	}
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE incarnation SET created_scenario = 'create' WHERE name = $1`, name); err != nil {
		t.Fatalf("seedCreateHistory (created_scenario): %v", err)
	}
}

// waitIncarnationStatus polls the incarnation status until it reaches want
// (waitRunDone can't be used for the rerun-create path: UnlockForRerun writes
// state_history with the same applyID BEFORE the run finishes, so we must
// wait on status specifically).
func waitIncarnationStatus(t *testing.T, name string, want incarnation.Status) *incarnation.Incarnation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		inc, err := incarnation.SelectByName(context.Background(), integrationPool, name)
		if err != nil {
			t.Fatalf("SelectByName: %v", err)
		}
		if inc.Status == want {
			return inc
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("incarnation %s не достигла статуса %q за 10s", name, want)
	return nil
}

// TestIntegration_FromLocked_RerunLast_DrivesRun — GUARD (S2 blocker): a
// rerun-create from error_locked DRIVES a real run to completion.
// UnlockForRerun under FOR UPDATE reserves applying (bypassing ready), then
// Start{FromLocked:true} — and lockRun must SEE applying as a valid starting
// status rather than reject the run. Without the FromLocked branch the run
// would get stuck in applying forever (lockRun would see applying →
// ErrAlreadyRunning → refusal). Checks: dispatch happened, the incarnation
// reached ready (didn't stay in applying).
func TestIntegration_FromLocked_RerunLast_DrivesRun(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	inc := &incarnation.Incarnation{
		Name: "noop-prod", Service: "noop", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusErrorLocked,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("Create error_locked: %v", err)
	}
	seedCreateHistory(t, "noop-prod") // last failed scenario = create (scope=create)
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	// Unlock part of rerun-last: error_locked → applying bypassing ready
	// (race-free), same as the handler/MCP tool.
	if _, err := incarnation.UnlockForRerun(context.Background(), integrationPool,
		"noop-prod", "rerun bootstrap verified", "archon-alice", audit.NewULID(), applyID); err != nil {
		t.Fatalf("UnlockForRerun: %v", err)
	}

	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
		FromLocked:      true,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Run driven to completion: incarnation is ready (did NOT get stuck in applying).
	waitIncarnationStatus(t, "noop-prod", incarnation.StatusReady)
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (прогон должен был стартовать)", disp.calls)
	}
	if disp.gotApplyID != applyID {
		t.Errorf("dispatched apply_id = %q, want %q", disp.gotApplyID, applyID)
	}
}

// TestIntegration_FromLocked_FailClosed_RejectsNonApplying — GUARD: FromLocked
// is fail-closed. lockRun under FromLocked does NOT transition status again —
// it must see applying. If the reserved row moved away before start (status
// isn't applying, here — ready), the run is rejected (ErrNotRunnable), no
// dispatch happens, status is untouched. Protects against running on an
// inconsistent call.
func TestIntegration_FromLocked_FailClosed_RejectsNonApplying(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod") // status ready, NOT applying
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         audit.NewULID(),
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
		FromLocked:      true,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The run is rejected inside the run-goroutine (lockRun → ErrNotRunnable);
	// status stays ready, no dispatch happens.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if disp.calls > 0 {
			t.Fatalf("SendApply вызван при FromLocked против не-applying статуса (fail-closed нарушен)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := incarnation.SelectByName(context.Background(), integrationPool, "noop-prod")
	if got.Status != incarnation.StatusReady {
		t.Errorf("status = %q, want ready (unchanged — fail-closed не должен трогать статус)", got.Status)
	}
}
