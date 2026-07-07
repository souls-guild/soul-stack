//go:build integration

// Integration-тесты scenario-runner-а (slice .g) через testcontainers PG +
// local-fs git-репо service-noop + mock Outbound. RunResult Soul-а
// симулируется внутри mock-dispatcher-а прямым apply_runs.UpdateStatus —
// тем же путём, что events_runresult.go::correlateRunResult в проде.
//
// Покрываются end-to-end: happy-path (1 task → 1 host → success → state
// commit → ready) и fail-path (RunResult failed → barrier → error_locked).

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

// renderedExecCommand собирает отрендеренную argv-команду core.exec.run из
// wire-params (`cmd` + `args`) в одну строку через пробел — для ассертов тестов,
// которые проверяют итоговую команду целиком (например, "echo hi!").
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
	// Тесты грузят service-репо по file://-URL (local-fs git), который в проде
	// запрещён scheme-allowlist-ом (security review L2). Включаем dev/test-флаг
	// на всё время прогона пакета — тот же приём, что в artifact_test.go.
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

// noopServiceRepo создаёт local-fs git-репо service-noop с одним коммитом:
// service.yml + scenario/create/main.yml (1 задача core.exec.run). Возвращает
// file://-URL для artifact.ServiceLoader.
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

// mockDispatcher симулирует Soul: на SendApply сразу пишет терминальный
// apply_runs-статус (как correlateRunResult в проде), завершая barrier.
type mockDispatcher struct {
	t          *testing.T
	result     applyrun.Status
	summary    *string
	calls      int
	gotApplyID string
	gotTasks   int
	gotAttempt int32 // attempt последнего ApplyRequest (старый dispatch-путь → 0)
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

// waveDispatcher симулирует Soul для serial-волн: фиксирует ПОРЯДОК SendApply
// (по SID) и пишет терминальный статус. failOn — SID, который завершается
// failed (для проверки fail-stop: волна с этим хостом ломает barrier, следующие
// волны не стартуют).
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

// newRunnerAcolyte собирает Runner с AcolyteEnabled=true и реальным Outbound
// (disp симулирует Soul). Используется staged-тестом, доказывающим, что staged-
// прогон идёт INLINE даже в work-queue-режиме (гейт run.go !staged, ADR-056 §S4):
// dispatchPlanned (Acolyte-путь) для staged НЕ вызывается. KID/PollInterval как у
// newAcolyteRunner. gitURL не используется (loader клонирует из ServiceRef.Git).
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
		// Staged-гейт (ADR-056 §S5): тестовые хосты passage-capable (см. newRunnerWithDestiny).
		PassageCap:   stubPassageCap{},
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
	})
}

// newRunnerWithDestiny собирает Runner с опциональным DestinySource (для
// apply:destiny). destinyTemplate пуст → Destiny=nil (apply:destiny не поддержан).
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
		// Staged-гейт (ADR-056 §S5): тестовые хосты «поддерживают passage» (lacking
		// пуст) — иначе fail-closed reject отверг бы все staged-тесты. Forward-compat
		// reject проверяется отдельным stub-ом в TestIntegration_StagedOldSoul_Rejected.
		PassageCap:   stubPassageCap{},
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
	})
}

// newRunnerWithAuditStaged — staged-вариант [newRunnerWithAudit] (реальные
// auditpg.Writer/Reader как production daemon.go) + PassageCap=stubPassageCap{}
// (оба хоста passage-aware, иначе S5-гейт отверг бы staged). Нужен cross-passage-
// гейту (ADR-056 R3): он читает CHANGED/FAILED-факты предыдущих Passage из журнала
// аудита через AuditReader.
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

// stubPassageCap — управляемая [PassageCapabilityChecker] для тестов. lacking —
// список SID-ов, которые НЕ поддерживают passage (по умолчанию nil → все
// поддерживают, как одноверсионный бета-флот). err — симуляция сбоя Redis.
type stubPassageCap struct {
	lacking []string
	err     error
}

func (s stubPassageCap) SoulsLackingPassage(_ context.Context, _ []string) ([]string, error) {
	return s.lacking, s.err
}

// newRunnerWithPassageCap — Runner с явным [PassageCapabilityChecker] (forward-
// compat guard-тест ADR-056 §S5): cap=nil → fail-closed-ветка гейта; cap с
// lacking → reject. Остальное как newRunnerWithDestiny (без Destiny).
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

// waitRunDone ждёт фактического завершения прогона applyID (commit-снапшот в
// state_history появляется только после терминала: и success, и error_locked
// его пишут) и возвращает incarnation. Так тест не путает СИД-овый
// «ready»-снапшот (стартовое значение seed-а) с пост-прогонным «ready».
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

	// state_history snapshot прогона.
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

// hangDispatcher симулирует Soul, который ПРИНЯЛ ApplyRequest, но ещё не
// прислал RunResult: apply_runs-строка остаётся running, barrier поллит. Так
// run-goroutine «зависает» в barrier-е до отмены — нужно для проверки Cancel.
type hangDispatcher struct {
	t     *testing.T
	mu    sync.Mutex
	calls int
}

func (d *hangDispatcher) SendApply(_ context.Context, _ string, _ *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	// Терминал НЕ пишем — хост остаётся running, barrier ждёт.
	return nil
}

// TestIntegration_CrossKeeperCancel_FlagInPG — cluster-wide Cancel (G1): флаг
// ставит «другой инстанс» (тест пишет его напрямую через RequestCancel, минуя
// Runner — имитация Keeper-B), а run-goroutine на ЭТОМ инстансе (Keeper-A)
// видит его в barrier-поллинге и отменяет прогон → error_locked (то же
// поведение, что локальный ctx-Cancel).
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

	// Ждём, пока хост окажется dispatched (apply_runs-строка running) — иначе
	// RequestCancel опередил бы Insert и не нашёл running-строк.
	waitHostDispatched(t, applyID)

	// «Другой Keeper»: ставит флаг напрямую в PG (без Runner на этом инстансе).
	affected, err := applyrun.RequestCancel(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected == 0 {
		t.Fatal("RequestCancel affected = 0, want >=1 (running-хост прогона)")
	}

	// run-goroutine видит флаг в barrier-поллинге и отменяет прогон.
	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("status_details = %+v, want reason=dispatch_failed (отмена через barrier)", inc.StatusDetails)
	}
}

// TestIntegration_LocalCancel_FastPath — локальный Cancel через
// Runner.RequestCancel: run-goroutine живёт на ЭТОМ инстансе, отмена доходит
// быстрым путём (ctx-Cancel), не дожидаясь барьерного тика. Тот же терминал
// error_locked.
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

// TestIntegration_RequestCancel_TerminalNoOp — Cancel уже завершённого прогона
// (терминальный статус) — no-op: флаг не ставится, incarnation остаётся ready.
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
	// Прогон успешно завершился — incarnation ready, apply_runs success.
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// Cancel завершённого прогона — no-op (found=false, нет running-строк, нет
	// локальной goroutine).
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

// waitHostDispatched ждёт появления хотя бы одной running-строки прогона
// (apply_runs Insert состоялся) — синхронизация перед RequestCancel, чтобы
// флаг застал running-строку.
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
	// Ни одного connected-хоста.
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

	// BAG-1: ранний abort (roster пуст) обязан оставить терминальную apply_runs-
	// строку, иначе Voyage-awaiter висит вечно. Реальных хостов нет → ровно одна
	// sentinel-строка (render.RunSentinelSID), status=failed, terminal.
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

// registerServiceRepo создаёт service-репо со scenario, где probe-задача
// (core.exec.run, register: probe) кормит state_changes.sets через
// ${ register.probe.stdout } (слайс 2 полной грамматики state_changes).
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

// registerMockDispatcher симулирует Soul, выполнивший probe-задачу: на SendApply
// пишет register-данные задачи (как accumulateRegister в проде на TaskEvent),
// затем терминальный apply_runs-статус (как correlateRunResult). registerData —
// payload register.probe для каждого хоста (sid → data).
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

// TestIntegration_RegisterInSets_CommitsToState — полный путь слайса 2:
// probe-задача (register: probe) → register-данные накоплены в
// apply_task_register → после барьера загружены per-host → state_changes.sets
// ${ register.probe.stdout } отрендерен → значение в incarnation.state.
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

// applyDestinyServiceRepo создаёт service-репо с scenario create, делегирующим
// в destiny pilot-flat через apply:destiny. service.yml объявляет destiny[]-ref.
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

// pilotFlatDestinyRepo создаёт плоскую destiny pilot-flat в каталоге
// <base>/pilot-flat (чтобы default_destiny_source-шаблон file://<base>/{name}
// резолвился в этот репо). Возвращает шаблон URL.
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

// TestIntegration_ApplyDestiny — end-to-end слайс A: scenario create с
// apply:destiny → DestinySource грузит destiny pilot-flat (file://) → render
// раскрывает её две задачи → dispatch → success → state commit. Проверяет, что
// диспатчер получил ровно две задачи destiny (apply раскрылся).
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

// TestIntegration_ApplyDestiny_NoSource — apply:destiny при nil-DestinySource →
// render_failed → error_locked (ErrUnsupportedDSL).
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

// inputDefaultsServiceRepo создаёт service-репо, чей scenario create объявляет
// scenario-level `input:` с ОДНИМ обязательным параметром (greeting, required) и
// ОДНИМ с default (suffix). Обе переменные рендерятся в params задачи. Прод
// (и L0) обязаны смёржить default непереданного suffix перед render — иначе
// `${ input.suffix }` падает «no such key» (BUG 1).
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

// captureDispatcher симулирует Soul и СОХРАНЯЕТ params первой задачи первого
// ApplyRequest (для проверки отрендеренной команды). Завершает barrier
// success-статусом.
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

// TestIntegration_ScenarioInputDefaultsMerged — регрессия BUG 1: оператор подаёт
// ТОЛЬКО обязательный input (greeting), непереданный suffix берётся из
// scenario `input:`-default. Render проходит, команда — "echo hi!" (default "!"
// смёржен). До фикса `${ input.suffix }` падал «no such key» → render_failed.
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
		// ТОЛЬКО обязательный input: suffix должен подтянуться из default.
		Input: map[string]any{"greeting": "hi"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if disp.gotCommand != "echo hi!" {
		t.Errorf("rendered command = %q, want %q (default suffix смёржен)", disp.gotCommand, "echo hi!")
	}
}

// TestIntegration_ScenarioInputRequiredMissing — обязательный scenario input не
// передан и без default → input_invalid → error_locked (понятная ошибка, не
// «no such key» в глубине CEL).
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
		// greeting (required) НЕ передан.
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
	// Incarnation уже в applying — lockRun должен отказать.
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

	// Прогон отклоняется внутри run-goroutine (lockRun → ErrAlreadyRunning);
	// статус остаётся applying, dispatch не происходит.
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

// TestIntegration_ErrorLocked_Rejected проверяет lock-gate (ADR-009): прогон
// против error_locked-incarnation отклоняется под FOR UPDATE (lockRun →
// ErrLocked), dispatch не происходит, статус остаётся error_locked.
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

	// Прогон отклоняется внутри run-goroutine (lockRun → ErrLocked);
	// статус остаётся error_locked, dispatch не происходит.
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

// TestIntegration_NonRunnableStatus_Rejected проверяет explicit allow-list
// lockRun (fail-closed): прогон против инстанса в destroying ИЛИ
// migration_failed отклоняется (lockRun → ErrNotRunnable), dispatch не
// происходит, статус остаётся неизменным. Раньше эти статусы проваливались в
// default-ветку и молча переводились в applying (латентный баг, вскрытый при
// дизайне destroy).
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

			// Прогон отклоняется внутри run-goroutine (lockRun → ErrNotRunnable);
			// статус остаётся прежним, dispatch не происходит.
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

// serialServiceRepo создаёт service-репо со scenario `roll`, несущим serial:
// заданной формы и непустой state_changes.sets — для проверки волнового
// dispatch + единого barrier.
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

// TestIntegration_Serial_AllWavesCommitOnce — serial: 1 на 3 хостах: все три
// хоста получают ApplyRequest (волны прокатились), state_changes коммитятся
// РОВНО ОДИН раз после ВСЕХ волн (единый barrier, orchestration.md §7) — это
// самый важный инвариант slice D. Проверяем: 3 SendApply, ровно 1
// state_history-snapshot, incarnation.state.rolled закоммичен.
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

	// Все 3 хоста получили ApplyRequest, в порядке SID (волны по 1, последовательно).
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

	// state закоммичен (единый commit после всех волн).
	if inc.State["rolled"] != "yes" {
		t.Errorf("state.rolled = %v, want \"yes\"", inc.State["rolled"])
	}

	// КРИТИЧНО: ровно ОДИН state_history-snapshot — state коммитится единожды
	// после всех волн, не по-волново (§7).
	_, total, err := incarnation.HistorySelectByName(context.Background(), integrationPool,
		"noop-prod", incarnation.HistoryFilter{ApplyID: applyID}, 0, 10)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 1 {
		t.Errorf("state_history snapshots = %d, want 1 (единый commit, НЕ по-волново)", total)
	}

	// Все apply_runs success.
	st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(st) != 3 {
		t.Errorf("apply_runs rows = %d, want 3", len(st))
	}
}

// TestIntegration_Serial_FailStop — serial: 1 на 3 хостах, первый хост (host-a)
// завершается failed: rolling останавливается, последующие волны НЕ стартуют
// (fail-stop, §2.2.1). Проверяем: ровно 1 SendApply (host-a), host-b/host-c НЕ
// получили ApplyRequest, incarnation → error_locked, state НЕ закоммичен.
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

	// Только первая волна (host-a) стартовала; вторая/третья НЕ стартуют.
	got := disp.dispatchedSIDs()
	if len(got) != 1 || got[0] != "host-a.example.com" {
		t.Errorf("dispatched = %v, want [host-a.example.com] (fail-stop: волны 2,3 не стартуют)", got)
	}

	// state НЕ закоммичен (rolled не появился) — §7: частичный коммит запрещён.
	if inc.State["rolled"] == "yes" {
		t.Errorf("state.rolled = yes — state НЕ должен коммититься при fail (§7)")
	}
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed", inc.StatusDetails)
	}
}

// TestIntegration_Serial_Percent — serial: "67%" на 3 хостах → ceil(3*0.67)=2 →
// волны [2,1]: все 3 хоста проходят, state коммитится один раз.
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

// serialMultiTaskRepo создаёт service-репо со scenario `roll`, несущим ДВЕ
// module-задачи с РАЗНОЙ serial:-шириной (serialA / serialB). Для проверки
// per-RUN min-width: ширина волны прогона = минимальная положительная среди
// задач (orchestration.md §2.2.1, effectiveSerialWidth), а не per-task.
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

// TestIntegration_Serial_Width2_FiveHosts — serial: 2 на 5 хостах → волны
// [2,2,1] (orchestration.md §2.2.1). Все 5 хостов получают ApplyRequest в
// порядке SID; state коммитится один раз после всех волн. Это end-to-end
// проверка нарезки на волны (не только splitWaves-unit): dispatch + per-wave
// barrier + единый state-commit.
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

	// Все 5 хостов получили ApplyRequest, в порядке SID (волны [2,2,1]
	// последовательны, внутри волны — по SID).
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
	// Единый commit после всех волн (§7): ровно один state_history-snapshot.
	_, total, err := incarnation.HistorySelectByName(context.Background(), integrationPool,
		"noop-prod", incarnation.HistoryFilter{ApplyID: applyID}, 0, 10)
	if err != nil {
		t.Fatalf("HistorySelectByName: %v", err)
	}
	if total != 1 {
		t.Errorf("state_history snapshots = %d, want 1 (единый commit после волн [2,2,1])", total)
	}
}

// TestIntegration_Serial_FailStop_SecondWave — сильнейший тест fail-stop §7:
// serial: 1 на 3 хостах, фейл на host-b (ВТОРАЯ волна). Первая волна (host-a)
// успешна, вторая (host-b) падает → rolling останавливается → ТРЕТЬЯ волна
// (host-c) НЕ стартует. Проверяем: ровно 2 SendApply (host-a, host-b — не 3),
// state НЕ закоммичен, incarnation → error_locked. Это инвариант
// «fail-stop ломает последующие волны» именно НЕ в первой волне.
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

	// Волна 1 (host-a) + волна 2 (host-b) стартовали; волна 3 (host-c) НЕТ.
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

	// state НЕ закоммичен (§7: частичный коммит запрещён).
	if inc.State["rolled"] == "yes" {
		t.Errorf("state.rolled = yes — state НЕ должен коммититься при fail во 2-й волне (§7)")
	}
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed", inc.StatusDetails)
	}
}

// cancelAfterFirstWaveDispatcher симулирует Soul для serial-волн: каждый хост
// завершается success, но СРАЗУ после первого SendApply ставит cluster-wide
// Cancel-флаг (как «другой Keeper»). Так per-wave barrier после первой волны
// видит cancel_requested → cancel-ветка прерывает rolling, последующие волны НЕ
// стартуют (симметрично fail-stop, но через отмену, не через failed-хост).
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
		// «Другой Keeper» ставит флаг во время первой волны, ПОКА строка ещё
		// running (RequestCancel фильтрует status='running'). Порядок строгий:
		// сперва флаг, потом терминал — иначе success опередил бы RequestCancel
		// и тот не нашёл бы running-строк (affected=0, флаг бы не встал).
		if _, err := applyrun.RequestCancel(ctx, integrationPool, req.GetApplyId()); err != nil {
			d.t.Errorf("cancelAfterFirstWaveDispatcher: RequestCancel: %v", err)
		}
	}
	// Хост штатно отстреливается success — прогон должна остановить именно
	// отмена, а не failed-хост (иначе тест дублировал бы fail-stop). Терминал
	// проставляем после флага: barrier увидит cancel_requested на success-строке
	// и прервёт rolling до старта следующей волны.
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

// TestIntegration_Serial_CancelStopsNextWave — cluster-wide Cancel (G1) на
// serial-прогоне: флаг ставится во время первой волны (host-a), per-wave
// barrier видит cancel_requested и прерывает rolling → волны 2,3 (host-b,
// host-c) НЕ стартуют. Это cancel-аналог fail-stop (§2.2.1): отмена ломает
// последующие волны так же, как падение хоста. Наблюдаемо: ровно 1 SendApply,
// incarnation → error_locked, state НЕ закоммичен.
//
// Отличие от [TestIntegration_CrossKeeperCancel_FlagInPG]: там одна волна и
// проверяется сам факт прерывания barrier-а; здесь — что отмена НЕ даёт стартовать
// следующей волне (serial × cancel взаимодействие).
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

	// Только первая волна (host-a) стартовала; волны 2,3 прерваны отменой.
	got := disp.dispatchedSIDs()
	if len(got) != 1 || got[0] != "host-a.example.com" {
		t.Errorf("dispatched = %v, want [host-a.example.com] (cancel останавливает волны 2,3)", got)
	}

	// state НЕ закоммичен — отмена = abort, как и fail-stop (§7).
	if inc.State["rolled"] == "yes" {
		t.Errorf("state.rolled = yes — отменённый прогон НЕ должен коммитить state")
	}
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (cancel через barrier → abort)", inc.StatusDetails)
	}
}

// TestIntegration_Serial_MinWidth_TwoTasks — per-RUN min-width (§2.2.1): scenario
// с двумя задачами разной ширины (serial: 2 и serial: 1) на 5 хостах. Ширина
// волны прогона = МИНИМАЛЬНАЯ положительная = 1 → волны [1,1,1,1,1], все 5
// хостов диспатчатся по одному, в порядке SID. Подтверждает, что широкая задача
// (serial: 2) НЕ задаёт окно — катится узкими волнами вместе с узкой.
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

	// min-width = 1 → каждый хост отдельной волной, по SID. 5 хостов = 5 SendApply
	// (один ApplyRequest на хост со ВСЕМИ его задачами — обе задачи едут вместе).
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

// runOnceServiceRepo создаёт service-репо со scenario `once` (run_once: true).
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

// TestIntegration_RunOnce_SingleHost — run_once: true на 3 хостах → ApplyRequest
// уходит ровно на ОДИН хост (первый по SID, host-a), остальные не получают
// (orchestration.md §2.2.2). Прогон успешен.
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

// --- security: observability masking (#1 второй канал + #6) -----------

// vaultParamServiceRepo — service-noop, у которого params команды несут
// vault-ref-маркер. Render-пайплайн в тестах без vault-резолвера, поэтому
// строка доходит до wire литералом — это нужный для теста кейс: проверяем,
// что keeper НЕ маскирует wire-Params, а маскирует только наблюдаемую копию.
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

// writeServiceRepo — общий конструктор local-fs git-репо service-noop с
// заданным scenario/create/main.yml (вынесено из noopServiceRepo для
// переиспользования секрет-фикстурой).
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

// secretWireDispatcher захватывает wire-Params (что реально ушло бы на Soul)
// и возвращает ошибку, ЭХАЮЩУЮ vault-ref из payload — симулирует transport/
// marshal-фейл, выносящий секрет в err.Error().
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
	// Эхо payload в ошибке (как делают некоторые transport/marshal-ошибки).
	return fmt.Errorf("rpc transport: failed to send %s", d.wireParams)
}

// TestIntegration_SecretInParams_MaskedInObservability_NotOnWire — security #1/#6:
//   - wire-ApplyRequest.Params несёт РЕАЛЬНОЕ значение (не сломано маскингом);
//   - error_summary (apply_runs, читается наружу через barrier) НЕ содержит
//     payload-эха — только safe-причина send_apply_failed;
//   - status_details.error (GET incarnation, без маскинга на чтении) замаскирован,
//     vault-ref не светится plaintext.
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

	// 1. Wire НЕ тронут: реальное значение дошло до dispatcher-а.
	wantWire := "deploy --token=vault:secret/keeper/deploy-token"
	if disp.wireParams != wantWire {
		t.Errorf("wire params = %q, want %q (wire-Params не должны маскироваться)", disp.wireParams, wantWire)
	}

	// 2. error_summary без payload-эха.
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

	// 3. status_details.error (читается наружу через GET incarnation без
	//    маскинга на чтении) НЕ светит секрет: ни payload-эхо send-фейла, ни
	//    vault-ref. В этом сценарии error_summary уже safe (send_apply_failed),
	//    поэтому транзитная barrier-ошибка тоже безопасна — главный инвариант
	//    «secret не утёк в наблюдаемый канал».
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

// TestIntegration_StatusDetailsError_VaultRefMasked — security #6, прямой
// канал: cause.Error() c vault-ref, попадающий в status_details минуя
// error_summary-транзит, маскируется в lockIncarnation перед записью и при
// чтении через incarnation не светит vault-путь plaintext.
//
// Путь: render-фаза падает ДО dispatch (apply:destiny без default_destiny_source
// → ErrUnsupportedDSL). Подменяем причину на vault-несущую через scenario с
// заведомо непарсящимся apply: — но детерминированный vault-ref в cause проще
// получить, проверив сам механизм на уровне lockIncarnation: см. unit-тест
// TestMaskSecrets_VaultRefSubstring (shared/audit) — он доказывает, что
// MaskSecrets маскирует строку с vault-маркером в любой позиции, а
// lockIncarnation прогоняет весь details через MaskSecrets перед записью.
func TestIntegration_StatusDetailsError_VaultRefMasked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	// Service-репо со scenario, ссылающимся на apply: destiny без
	// default_destiny_source в Deps → render-фаза вернёт ErrUnsupportedDSL.
	// Сам по себе он vault-ref не несёт; маскинг status_details на
	// vault-содержащей строке доказан unit-тестом TestMaskSecrets_VaultRefSubstring
	// + фактом MaskSecrets-прогона в lockIncarnation. Здесь подтверждаем, что
	// status_details пишется через маскинг-путь (reason присутствует, секрета нет).
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
// Покрытие S1 keeper-dispatch сквозь run()+PG. До этих тестов ни один
// integration-прогон не нёс keeper-задачу (newRunner строит Runner с
// KeeperModules==nil), поэтому keeper-dispatch путь (run.go шаг 5.5 →
// dispatchKeeperTasks) гонялся только unit-ами над applyKeeperTask, минуя
// apply_runs/incarnation-финал. fakeKeeperModule/fakeKeeperRegistry —
// в keeper_dispatch_test.go (без build-тега, компилируются и здесь).

// newRunnerWithKeeper собирает Runner с keeper-side core-Registry (для задач
// `on: keeper`). Остальное — как newRunner (mock-dispatcher, PG-пул, real
// render). keepers==nil → задача `on: keeper` отвергается
// (ErrKeeperModulesNotConfigured) — это путь QA-пробела (f).
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

// keeperServiceRepo создаёт service-репо со scenario create, несущим
// keeperTasks keeper-side задач (`on: keeper`, модуль keeperModule) и ОДНУ
// Soul-side echo-задачу в конце — Soul-side нужна, чтобы roster был непустым
// (run.go шаг 3, иначе no_hosts отсекает прогон до keeper-dispatch). Каждая
// keeper-задача несёт register: keeperN — для проверки accumulateKeeperRegister.
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

// mustStructAny — structpb из map без *testing.T (для конструкторов модулей).
func mustStructAny(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}

// TestIntegration_KeeperDispatch_Failed_ErrorLocked — QA-пробел (a), КРИТИЧНО:
// keeper-side задача провалена (final.Failed=true) → incarnation error_locked;
// apply_runs(sid="keeper") = failed с error_summary; host-fan-out НЕ стартовал
// (SendApply не вызван); incarnation.state не закоммичен. Сквозь run()+PG —
// первый integration-тест, гоняющий keeper-dispatch через реальную БД.
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

	// host-fan-out НЕ стартовал — keeper-задача упала до dispatch-а хостам.
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (keeper-задача упала до host-fan-out)", disp.calls)
	}

	// state НЕ закоммичен (остался state_before = пустой seed-state).
	if len(inc.State) != 0 {
		t.Errorf("incarnation.state = %v, want пустой (state_before, keeper-fail НЕ коммитит)", inc.State)
	}

	// apply_runs(sid="keeper") = failed с error_summary.
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

// TestIntegration_KeeperDispatch_TwoTasks_OrderAndRegister — QA-пробел (c):
// ДВЕ keeper-задачи в одном сценарии. Обе исполнены в порядке Index, обе
// register-записи под sid="keeper" с разными task_idx (0 и 1). Прогон успешен,
// host-fan-out стартовал (Soul-задача → 1 SendApply).
func TestIntegration_KeeperDispatch_TwoTasks_OrderAndRegister(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	// Модуль фиксирует порядок вызовов через общий счётчик: каждая задача шлёт
	// свой output (call_order), чтобы register-записи различались наблюдаемо.
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

	// Обе keeper-задачи исполнены (модуль вызван дважды).
	if got := mod.applyCount(); got != 2 {
		t.Errorf("keeper module Apply вызван %d раз, want 2", got)
	}

	// host-fan-out стартовал: Soul-задача ушла одному хосту.
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (Soul echo на одном хосте)", disp.calls)
	}

	// Обе register-записи под sid="keeper" с разными task_idx (0,1).
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

// orderedKeeperModule — keeper-side модуль, считающий вызовы Apply (для проверки
// «обе keeper-задачи исполнены»). Финал всегда success c output.call_order.
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

// TestIntegration_KeeperDispatch_FirstFailedAbortsSecond — QA-пробел (c),
// abort-инвариант: ПЕРВАЯ из двух keeper-задач провалена → ВТОРАЯ не исполняется
// (dispatchKeeperTasks возвращает ошибку на первой failed). Наблюдаемо: модуль
// вызван ровно 1 раз, incarnation error_locked, host-fan-out не стартовал.
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

// countingFailModule — keeper-side модуль, отдающий failed-финал и считающий
// вызовы Apply (для abort-инварианта «вторая задача не исполняется»).
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

// TestIntegration_KeeperDispatch_NilRegistry_ErrorLocked — QA-пробел (f):
// scenario несёт `on: keeper`-задачу, но Runner собран с KeeperModules==nil →
// dispatchKeeperTasks возвращает ErrKeeperModulesNotConfigured →
// keeper_dispatch_failed → error_locked. host-fan-out не стартовал.
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

// keeperOnlyServiceRepo создаёт service-репо со scenario create ТОЛЬКО из
// keeper-задач (ни одной Soul-side задачи). Roster прогона будет резолвиться
// run.go-шагом 3 независимо от состава задач.
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

// TestIntegration_KeeperOnly_NoHosts_RunsKeeperTasks — bypass no_hosts для
// all-keeper provision-from-zero (ADR-0061 §контекст amend): keeper-only сценарий
// (0 Soul-side задач, ВСЕ `on: keeper`) против incarnation БЕЗ connected-хостов
// теперь ИСПОЛНЯЕТ keeper-задачу (chicken-egg: create-сценарий создаёт хосты С
// НУЛЯ, поэтому пустой roster на старте законен). Гейт пропускается по СОСТАВУ
// (allKeeperTasks), без флага. Инвертирует прежнюю фиксацию S1-ограничения
// (...AbortsBeforeKeeper): раньше no_hosts отсекал прогон до keeper-dispatch.
//
// Наблюдаемо: keeper-модуль вызван (applyCount==1), прогон НЕ error_locked по
// no_hosts, доходит до keeper-dispatch (шаг 5.5) и финализируется ready —
// host-fan-out на пустом roster = no-op-success. Essence резолвится в keeper-
// контексте (keeperEssenceInput, без host-представителя) — без паники на hosts[0].
func TestIntegration_KeeperOnly_NoHosts_RunsKeeperTasks(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// Ни одного connected-хоста — provision-from-zero стартует на пустом roster.

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

	// Прогон НЕ падает в no_hosts — доходит до keeper-dispatch и завершается
	// успешно (host-fan-out на пустом roster = no-op).
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// Keeper-задача ИСПОЛНЕНА — гейт пропущен по составу (all-keeper).
	if got := mod.applyCount(); got != 1 {
		t.Errorf("keeper module Apply вызван %d раз, want 1 (all-keeper bypass no_hosts на пустом roster)", got)
	}
	// host-fan-out не стартовал — Soul-задач нет, roster пуст.
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (нет Soul-задач, пустой roster)", disp.calls)
	}

	// keeper apply_runs(sid="keeper") = success (НЕ sentinel no_hosts).
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

// mixedKeeperHostServiceRepo создаёт service-репо со scenario create из ОДНОЙ
// keeper-задачи (`on: keeper`, БЕЗ refresh_soulprint) И ОДНОЙ Soul-side задачи
// (core.exec.run). Состав смешанный → allKeeperTasks(false); refresh-эмиттера НЕТ
// → HasRefreshEmitter(false). Ни один класс bypass не применяется → no_hosts-гейт
// держится на пустом roster (РЕВЕРС-фикстура: ловит расширение bypass на mixed-
// без-refresh).
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

// TestIntegration_HostScenario_NoHosts_StillAborts — GUARD bypass-границы:
// host-сценарий (несёт Soul-side задачу) против пустого roster по-прежнему падает
// в no_hosts. allKeeperTasks(false) → bypass НЕ применяется. Защищает прежнее
// поведение host-прогонов от регрессии (bypass не должен «протечь» на host-
// сценарии).
func TestIntegration_HostScenario_NoHosts_StillAborts(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// Ни одного connected-хоста.
	gitURL := noopServiceRepo(t) // несёт Soul-side core.exec.run

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

// TestIntegration_MixedKeeperAndHost_NoHosts_StillAborts — ★ РЕВЕРС-GUARD (security,
// нерасширенное поведение СОХРАНЕНО): смешанный сценарий (keeper-задача + Soul-задача)
// БЕЗ refresh-эмиттера против пустого roster падает в no_hosts. allKeeperTasks(false)
// (есть host-задача) И HasRefreshEmitter(false) (нет refresh_soulprint) → НИ ОДИН
// класс bypass не применяется → гейт держится. Keeper-модуль НЕ исполнен (abort на
// шаге 3, до keeper-dispatch). Защищает границу: bypass mixed-плана требует ИМЕННО
// refresh-эмиттера; mixed без него остаётся за no_hosts (host-задача на пустом P0).
func TestIntegration_MixedKeeperAndHost_NoHosts_StillAborts(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// Ни одного connected-хоста.

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
	// Keeper-модуль НЕ исполнен — no_hosts отсёк прогон до keeper-dispatch (шаг 5.5).
	if got := mod.applyCount(); got != 0 {
		t.Errorf("keeper module Apply вызван %d раз, want 0 (no_hosts до keeper-dispatch)", got)
	}
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0", disp.calls)
	}
}

// mixedKeeperHostRefreshServiceRepo — mixed-план provision→роль с REFRESH-эмиттером:
// keeper-задача core.soul.registered c `refresh_soulprint: true` (provision-passage)
// + Soul-side host-задача (deploy-passage). HasRefreshEmitter(true) → план провиженит
// roster mid-run → стартует на ПУСТОМ roster законно (host-задача стратифицируется в
// Passage ПОСЛЕ refresh-границы, §S2/§S3). Целевая фикстура bypass-класса (б).
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

// TestIntegration_MixedKeeperAndHost_Refresh_RunsToDispatch — ★ GUARD bypass-класса
// (б) (ADR-0061 amendment): смешанный provision→роль план С refresh-эмиттером
// (core.soul.registered refresh_soulprint: true) против ПУСТОГО roster НЕ падает в
// no_hosts — доходит до keeper-dispatch и исполняет provision-шаг. allKeeperTasks
// false (есть host-задача), но HasRefreshEmitter true → пустой стартовый roster
// законен (host-задача уезжает в Passage после refresh-границы, на re-resolved
// roster). Инвертирует прежнюю фиксацию «mixed остаётся за no_hosts».
//
// Наблюдаемо: keeper provision-модуль вызван (applyCount==1), НЕТ sentinel-строки
// no_hosts, прогон финализируется ready (host-fan-out на пустом re-resolved roster
// = no-op-success — VM в unit реально не онбордятся, live-снимок остаётся пустым).
func TestIntegration_MixedKeeperAndHost_Refresh_RunsToDispatch(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// Ни одного connected-хоста — provision-from-zero стартует на пустом roster.

	mod := &orderedKeeperModule{}
	keepers := fakeKeeperRegistry{"core.soul": mod}
	gitURL := mixedKeeperHostRefreshServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	// staged-Runner: refresh-граница даёт Count=2 → нужен passage-capability-чекер
	// (stubPassageCap, ADR-056 §S5). Пустой стартовый roster → presence-гейт на
	// пустом наборе SID = no-op (никто не lacking), staged-механика проходит.
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

	// НЕ no_hosts: прогон доходит до keeper-dispatch и завершается (host-deploy на
	// пустом re-resolved roster = no-op).
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// Provision keeper-задача ИСПОЛНЕНА — bypass пропустил пустой roster по refresh.
	if got := mod.applyCount(); got != 1 {
		t.Errorf("keeper module Apply вызван %d раз, want 1 (mixed+refresh bypass no_hosts на пустом roster)", got)
	}

	// Нет sentinel-строки no_hosts — стартовый гейт пропущен.
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

// seedCreateHistory вставляет state_history-snapshot упавшего `create`-сценария И
// проставляет incarnation.created_scenario='create' — вместе дают scope=create в
// [incarnation.UnlockForRerun]: gate требует created_scenario == последний упавший
// сценарий (значение create-пути проставляет handler/MCP, Create персистит). state_before == state_after = `{}`.
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

// waitIncarnationStatus поллит статус incarnation до достижения want (для rerun-
// create-пути нельзя использовать waitRunDone: UnlockForRerun пишет state_history
// с тем же applyID ДО завершения прогона, поэтому ждать надо именно по статусу).
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

// TestIntegration_FromLocked_RerunLast_DrivesRun — GUARD (блокер S2): rerun-
// create из error_locked ДОВОДИТ до реального прогона. UnlockForRerun под FOR
// UPDATE резервирует applying (минуя ready), затем Start{FromLocked:true} — и
// lockRun обязан УВИДЕТЬ applying как валидный стартовый статус, а не отвергнуть
// прогон. Без FromLocked-ветки прогон застревал бы в applying навсегда (lockRun
// видел applying → ErrAlreadyRunning → отказ). Проверяем: dispatch состоялся,
// инкарнация дошла до ready (не осталась в applying).
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
	seedCreateHistory(t, "noop-prod") // последний упавший сценарий = create (scope=create)
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	// Unlock-часть rerun-last: error_locked → applying минуя ready (race-free),
	// как в handler-е/MCP-tool-е.
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

	// Прогон доведён до конца: инкарнация в ready (НЕ застряла в applying).
	waitIncarnationStatus(t, "noop-prod", incarnation.StatusReady)
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (прогон должен был стартовать)", disp.calls)
	}
	if disp.gotApplyID != applyID {
		t.Errorf("dispatched apply_id = %q, want %q", disp.gotApplyID, applyID)
	}
}

// TestIntegration_FromLocked_FailClosed_RejectsNonApplying — GUARD: FromLocked
// fail-closed. lockRun при FromLocked НЕ транзитит статус повторно — обязан
// увидеть applying. Если зарезервированная строка УШЛА из-под старта (статус не
// applying, здесь — ready), прогон отклоняется (ErrNotRunnable), dispatch не
// происходит, статус не трогается. Защита от исполнения по неконсистентному вызову.
func TestIntegration_FromLocked_FailClosed_RejectsNonApplying(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod") // статус ready, НЕ applying
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

	// Прогон отклоняется внутри run-goroutine (lockRun → ErrNotRunnable);
	// статус остаётся ready, dispatch не происходит.
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
