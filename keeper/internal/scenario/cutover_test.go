//go:build integration

// Integration-тесты cutover-а исполнения apply на Acolyte (ADR-027, Phase
// 1.4.2/1.4.3): ветвление dispatch-а (planned+recipe+Summons), RenderForHost
// (per-host рендер по рецепту) и claim-execute (render→SendApply→running, no-op
// success, render-error→failed). Reuse общего harness-а integration_test.go
// (TestMain/seed*/noopServiceRepo/mockDispatcher).

package scenario

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// countingSummons — стаб SummonsPublisher: считает публикации (best-effort путь
// dispatchPlanned шлёт один Summons после всех Insert-ов).
type countingSummons struct{ n atomic.Int64 }

func (s *countingSummons) PublishSummons(context.Context) error {
	s.n.Add(1)
	return nil
}

// newAcolyteRunner собирает Runner с AcolyteEnabled (новый путь dispatch-а) и
// заданным Summons-публикатором. Outbound НЕ должен вызываться на dispatch-е
// нового пути (его дёргает Acolyte при claim) — передаём fakeDispatcher.
func newAcolyteRunner(t *testing.T, summons SummonsPublisher) *Runner {
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
		Outbound:       fakeDispatcher{},
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-acolyte-test",
		Summons:        summons,
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     20 * time.Second,
	})
}

// newClaimRunner собирает ClaimRunner поверх integrationPool с заданным
// Outbound (mockDispatcher симулирует Soul через прямой UpdateStatus).
func newClaimRunner(t *testing.T, disp ApplyDispatcher) *ClaimRunner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewClaimRunner(ClaimDeps{
		Deps: Deps{
			Loader:   artifact.NewServiceLoader(t.TempDir(), nil),
			Topology: topology.NewResolver(integrationPool, nil, nil),
			Essence:  essence.NewResolver(nil),
			Render:   render.NewPipeline(nil, engine, nil, nil),
			Outbound: disp,
			DB:       integrationPool,
		},
		KID:   "keeper-acolyte-test",
		Lease: 30 * time.Second,
		Batch: 10,
	})
}

// TestIntegration_DispatchPlanned_WritesPlannedAndSummons — новый путь: dispatch
// пишет planned+recipe на все roster-хосты (Вариант Б), шлёт Summons, НЕ зовёт
// SendApply. Acolyte (ClaimRunner) затем доводит задания → barrier → ready.
func TestIntegration_DispatchPlanned_WritesPlannedAndSummons(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	summons := &countingSummons{}
	r := newAcolyteRunner(t, summons)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"db_password": "vault:secret/db-creds#password"},
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Дожидаемся появления planned-строк на ОБОИХ хостах (Вариант Б — все roster).
	waitForPlanned(t, applyID, 2)

	// recipe несёт vault-ref КАК ЕСТЬ (инвариант A) на каждой строке.
	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		got, err := applyrun.SelectByApplyID(context.Background(), integrationPool, applyID, sid)
		if err != nil {
			t.Fatalf("SelectByApplyID(%s): %v", sid, err)
		}
		if got.Recipe == nil {
			t.Fatalf("planned %s без recipe", sid)
		}
		if got.Recipe.Input["db_password"] != "vault:secret/db-creds#password" {
			t.Errorf("%s recipe несёт раскрытый секрет вместо vault-ref: %v", sid, got.Recipe.Input["db_password"])
		}
	}

	if summons.n.Load() != 1 {
		t.Errorf("PublishSummons вызван %d раз, want 1", summons.n.Load())
	}

	// Acolyte доводит оба planned → running → success (mockDispatcher симулирует Soul).
	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	cr := newClaimRunner(t, disp)
	driveClaims(t, cr, applyID, 2)

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if inc.StatusDetails != nil {
		t.Errorf("status_details = %+v, want nil on success", inc.StatusDetails)
	}
	if disp.calls != 2 {
		t.Errorf("SendApply (Acolyte) calls = %d, want 2", disp.calls)
	}
}

// TestIntegration_SerialGuard_FallsBackToOldPath — scenario с serial-задачей при
// AcolyteEnabled идёт СТАРЫМ путём: dispatch сразу пишет running + SendApply,
// planned-строк не появляется (распределённый serial — Phase 3).
func TestIntegration_SerialGuard_FallsBackToOldPath(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, serialGuardScenario)

	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	// AcolyteEnabled, но serial-guard должен загнать в старый путь → Outbound
	// (mockDispatcher) вызывается напрямую на dispatch-е.
	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := NewRunner(Deps{
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		Essence:        essence.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       disp,
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-acolyte-test",
		Summons:        &countingSummons{},
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     20 * time.Second,
	})

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
	// Старый путь: SendApply дёрнут напрямую (без Acolyte), строка прошла running.
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (старый путь serial-guard)", disp.calls)
	}
	// Старый inline-путь (dispatchWave) НЕ выставляет attempt — fencing-epoch там
	// вырождается (нет Ward-claim/recovery), на проводе attempt=0 (= старый Keeper
	// без fencing, ADR-027(g), S-P2.2). Это осознанно, не баг.
	if disp.gotAttempt != 0 {
		t.Errorf("ApplyRequest.Attempt = %d, want 0 (старый dispatchWave-путь не фенсит)", disp.gotAttempt)
	}
}

// TestIntegration_RenderForHost_SingleHost — RenderForHost рендерит прогон по
// рецепту (load→parse→essence→full-roster render) и фильтрует свой SID. На
// roster-е из одного хоста full-roster == single-host; multi-host parity
// проверяет TestIntegration_TargetingParity_AcolyteVsOldPath.
func TestIntegration_RenderForHost_SingleHost(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	deps := Deps{
		Loader:   artifact.NewServiceLoader(t.TempDir(), nil),
		Topology: topology.NewResolver(integrationPool, nil, nil),
		Essence:  essence.NewResolver(nil),
		Render:   render.NewPipeline(nil, engine, nil, nil),
		Outbound: fakeDispatcher{},
		DB:       integrationPool,
	}
	recipe := &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "create",
	}
	tasks, plans, err := RenderForHost(context.Background(), deps, recipe,
		"noop-prod", audit.NewULID(), "host-a.example.com")
	if err != nil {
		t.Fatalf("RenderForHost: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].Module != "core.exec.run" {
		t.Errorf("module = %q, want core.exec.run", tasks[0].Module)
	}
	// План таргетит именно этот SID.
	host := groupByHost(tasks, plans)["host-a.example.com"]
	if len(host) != 1 {
		t.Errorf("host-a задачи = %d, want 1", len(host))
	}
}

// TestIntegration_RenderForHost_HostNotInRoster — хост вне roster-а (disconnected
// между dispatch и claim) → ошибка (рендерить не на чем).
func TestIntegration_RenderForHost_HostNotInRoster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	gitURL := noopServiceRepo(t)

	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	deps := Deps{
		Loader:   artifact.NewServiceLoader(t.TempDir(), nil),
		Topology: topology.NewResolver(integrationPool, nil, nil),
		Essence:  essence.NewResolver(nil),
		Render:   render.NewPipeline(nil, engine, nil, nil),
		Outbound: fakeDispatcher{},
		DB:       integrationPool,
	}
	recipe := &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "create",
	}
	_, _, err = RenderForHost(context.Background(), deps, recipe,
		"noop-prod", audit.NewULID(), "ghost.example.com")
	if err == nil {
		t.Fatalf("RenderForHost для хоста вне roster-а прошёл, want ошибку")
	}
}

// TestIntegration_Claim_HappyPath — claim одного planned-задания: render →
// MarkDispatched (claimed→dispatched) → SendApply (через applyOnlyDispatcher).
// Строка проходит claimed → dispatched, attempt 0→1. Отметка dispatched теперь
// СТРОГО ПЕРЕД SendApply (ADR-027 amend S3).
func TestIntegration_Claim_HappyPath(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ctx := context.Background()

	// Готовим planned-задание напрямую (InsertPlanned), минуя run-goroutine.
	insertPlannedFixture(t, "01HCLAIMOK", "host-a.example.com", gitURL)

	// applyOnlyDispatcher симулирует Soul, НО только считает SendApply и НЕ
	// терминалит строку — так проверяем именно claimed→dispatched.
	disp := &applyOnlyDispatcher{}
	cr := newClaimRunner(t, disp)
	if err := cr.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.calls.Load() != 1 {
		t.Fatalf("SendApply calls = %d, want 1", disp.calls.Load())
	}
	// Fencing-проброс (ADR-027(g)): claim кладёт run.Attempt в
	// ApplyRequest.Attempt. ClaimNext инкрементил 0→1, поэтому на проводе attempt=1.
	if got := disp.lastAttempt.Load(); got != 1 {
		t.Errorf("ApplyRequest.Attempt = %d, want 1 (claim пробрасывает run.Attempt)", got)
	}
	// SendApply увидел строку уже в dispatched — отметка строго ПЕРЕД send.
	if disp.statusAtSend != string(applyrun.StatusDispatched) {
		t.Errorf("статус на момент SendApply = %q, want dispatched (MarkDispatched строго ДО send)", disp.statusAtSend)
	}

	got, err := applyrun.SelectByApplyID(ctx, integrationPool, "01HCLAIMOK", "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != applyrun.StatusDispatched {
		t.Errorf("status = %q, want dispatched (claimed→dispatched перед SendApply)", got.Status)
	}
	if got.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 (claim инкрементит)", got.Attempt)
	}
}

// TestIntegration_Claim_MarkDispatchedBeforeSend — порядок инварианта: на момент
// вызова SendApply строка уже dispatched (отметка строго ДО send). Если бы
// отметка шла после, dispatcher увидел бы claimed.
func TestIntegration_Claim_MarkDispatchedBeforeSend(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ctx := context.Background()

	insertPlannedFixture(t, "01HORDER", "host-a.example.com", gitURL)

	disp := &applyOnlyDispatcher{}
	cr := newClaimRunner(t, disp)
	if err := cr.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.calls.Load() != 1 {
		t.Fatalf("SendApply calls = %d, want 1", disp.calls.Load())
	}
	if disp.statusAtSend != string(applyrun.StatusDispatched) {
		t.Errorf("статус на момент SendApply = %q, want dispatched", disp.statusAtSend)
	}
}

// TestIntegration_Claim_SendApplyFails_Failed — SendApply вернул ошибку (Keeper
// жив, знает что доставка не удалась): задание терминалится failed с safe-summary.
func TestIntegration_Claim_SendApplyFails_Failed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ctx := context.Background()

	insertPlannedFixture(t, "01HSENDFAIL", "host-a.example.com", gitURL)

	disp := &failingDispatcher{}
	cr := newClaimRunner(t, disp)
	if err := cr.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.calls.Load() != 1 {
		t.Fatalf("SendApply calls = %d, want 1 (попытка доставки была)", disp.calls.Load())
	}
	got, err := applyrun.SelectByApplyID(ctx, integrationPool, "01HSENDFAIL", "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != applyrun.StatusFailed {
		t.Errorf("status = %q, want failed (SendApply-ошибка терминалит из dispatched)", got.Status)
	}
	if got.ErrorSummary == nil || *got.ErrorSummary != "send_apply_failed" {
		t.Errorf("error_summary = %v, want send_apply_failed", got.ErrorSummary)
	}
}

// TestIntegration_Claim_NoOpHost — where: отфильтровал все задачи на хосте →
// claim закрывает задание `no_match` без ApplyRequest (FINDING-01 (б): no-op
// хост получает отдельный benign-терминал, не `success`).
func TestIntegration_Claim_NoOpHost(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := writeServiceRepo(t, whereFalseScenario)
	ctx := context.Background()

	insertPlannedFixture(t, "01HCLAIMNOOP", "host-a.example.com", gitURL)

	disp := &applyOnlyDispatcher{}
	cr := newClaimRunner(t, disp)
	if err := cr.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.calls.Load() != 0 {
		t.Errorf("SendApply calls = %d, want 0 (no-op хост)", disp.calls.Load())
	}
	got, err := applyrun.SelectByApplyID(ctx, integrationPool, "01HCLAIMNOOP", "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != applyrun.StatusNoMatch {
		t.Errorf("status = %q, want no_match (FINDING-01 (б): no-op хост — отдельный benign-терминал)", got.Status)
	}
}

// TestIntegration_Claim_RenderError_FailedMasked — несуществующий сценарий в
// рецепте → render-ошибка → failed с masked-summary (без раскрытого секрета).
func TestIntegration_Claim_RenderError_FailedMasked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)
	ctx := context.Background()

	// Рецепт с input-секретом + ссылкой на НЕсуществующий сценарий → render
	// упадёт; summary не должен утечь vault-ref.
	insertPlannedFixtureFull(t, "01HCLAIMERR", "host-a.example.com", &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "does_not_exist",
		Input:        map[string]any{"db_password": "vault:secret/db-creds#password"},
	})

	disp := &applyOnlyDispatcher{}
	cr := newClaimRunner(t, disp)
	if err := cr.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if disp.calls.Load() != 0 {
		t.Errorf("SendApply calls = %d, want 0 (render упал до отправки)", disp.calls.Load())
	}
	got, err := applyrun.SelectByApplyID(ctx, integrationPool, "01HCLAIMERR", "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != applyrun.StatusFailed {
		t.Errorf("status = %q, want failed (render-ошибка)", got.Status)
	}
	if got.ErrorSummary == nil {
		t.Fatalf("error_summary nil, want masked-summary")
	}
	if strings.Contains(*got.ErrorSummary, "vault:secret/db-creds") {
		t.Errorf("error_summary несёт голый vault-ref: %q", *got.ErrorSummary)
	}
}

// --- helpers ----------------------------------------------------------

// applyOnlyDispatcher — Outbound, который только считает SendApply и НЕ пишет
// терминальный статус (в отличие от mockDispatcher): для проверки именно
// claimed→dispatched, оставляя строку dispatched. Дополнительно фиксирует статус
// строки на момент send (statusAtSend) — для проверки порядка «отметка ДО send».
type applyOnlyDispatcher struct {
	calls        atomic.Int64
	lastAttempt  atomic.Int32 // attempt последнего отправленного ApplyRequest (fencing-проброс)
	statusAtSend string       // статус apply_runs-строки в момент входа в SendApply
}

func (d *applyOnlyDispatcher) SendApply(ctx context.Context, _ string, req *keeperv1.ApplyRequest) error {
	d.calls.Add(1)
	d.lastAttempt.Store(req.GetAttempt())
	// Снимок статуса строки на входе в send: если MarkDispatched отработал ДО
	// SendApply (инвариант S3), здесь увидим 'dispatched'.
	var status string
	if err := integrationPool.QueryRow(ctx,
		`SELECT status FROM apply_runs WHERE apply_id = $1`, req.GetApplyId()).Scan(&status); err == nil {
		d.statusAtSend = status
	}
	return nil
}

// failingDispatcher — Outbound, чей SendApply всегда возвращает ошибку: для
// проверки терминала failed на провале доставки (claim после MarkDispatched).
type failingDispatcher struct {
	calls atomic.Int64
}

func (d *failingDispatcher) SendApply(_ context.Context, _ string, _ *keeperv1.ApplyRequest) error {
	d.calls.Add(1)
	return errSendApply
}

var errSendApply = errSendApplyT("send apply boom")

type errSendApplyT string

func (e errSendApplyT) Error() string { return string(e) }

// waitForPlanned ждёт появления n planned-строк прогона applyID.
func waitForPlanned(t *testing.T, applyID string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
		if err != nil {
			t.Fatalf("SelectStatusesByApplyID: %v", err)
		}
		planned := 0
		for _, s := range st {
			if s.Status == applyrun.StatusPlanned {
				planned++
			}
		}
		if planned >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("planned-строк %d не появилось за 10s для %s", n, applyID)
}

// driveClaims прокручивает ClaimRunner.Claim, пока не доведёт все n заданий до
// running/терминала (mockDispatcher терминалит на SendApply).
func driveClaims(t *testing.T, cr *ClaimRunner, applyID string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := cr.Claim(context.Background()); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
		if err != nil {
			t.Fatalf("SelectStatusesByApplyID: %v", err)
		}
		terminal := 0
		for _, s := range st {
			switch s.Status {
			case applyrun.StatusSuccess, applyrun.StatusFailed, applyrun.StatusCancelled, applyrun.StatusNoMatch:
				terminal++
			}
		}
		if terminal >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("claims не доведены до терминала за 10s для %s", applyID)
}

// insertPlannedFixture пишет одно planned-задание с recipe на noop-сценарий.
func insertPlannedFixture(t *testing.T, applyID, sid, gitURL string) {
	t.Helper()
	insertPlannedFixtureFull(t, applyID, sid, &applyrun.Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName: "create",
	})
}

func insertPlannedFixtureFull(t *testing.T, applyID, sid string, recipe *applyrun.Recipe) {
	t.Helper()
	if err := applyrun.InsertPlanned(context.Background(), integrationPool, &applyrun.ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: "noop-prod",
		Scenario: recipe.ScenarioName, Recipe: recipe,
	}); err != nil {
		t.Fatalf("InsertPlanned(%s,%s): %v", applyID, sid, err)
	}
}

// serialGuardScenario — scenario/create с задачей, несущей serial: (serial-guard
// гонит её в старый путь даже при AcolyteEnabled). Имя сценария — create
// (writeServiceRepo пишет именно его).
const serialGuardScenario = `name: create
description: serial-guard fixture
state_changes: {}
tasks:
  - name: Echo with serial
    module: core.exec.run
    serial: 1
    params:
      cmd: echo
      args: ["hello"]
    changed_when: "false"
`

// whereFalseScenario — scenario/create с задачей where: false → ни один хост не
// таргетится (claim делает no-op success).
const whereFalseScenario = `name: create
description: where-false fixture
state_changes: {}
tasks:
  - name: Never targets any host
    module: core.exec.run
    where: "false"
    params:
      cmd: echo
      args: ["never"]
    changed_when: "false"
`
