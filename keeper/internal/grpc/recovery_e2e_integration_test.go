//go:build integration

// Сквозной integration-тест связки S4 (reclaim-only-claimed / пере-claim) ↔
// S1/S5 (RunResult.attempt epoch-check в correlateRunResult) на ЖИВОМ PG.
//
// Major coverage-gap (qa ae10): существующие тесты утверждают звенья по
// отдельности — TestIntegration_ClaimNext_AttemptIncrements доводит attempt до 2
// и обрывается; TestCorrelateRunResult_StaleAttemptDropped подаёт stale на
// СИНТЕТИЧЕСКИЙ rowAttempt=5 через fake-DB. Шов end-to-end (attempt реально
// доезжает через пере-claim → correlateRunResult читает ту же строку и
// отвергает устаревшую попытку) не был утверждён в одной системе. Этот тест
// проходит всю цепочку на одном PG-пуле через реальные applyrun-CRUD и реальный
// eventStreamHandler.correlateRunResult.

package grpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// resetRecoveryE2E чистит таблицы, задействованные сквозным recovery-тестом.
// grpc-пакетный resetAll TRUNCATE-ит онбординг-таблицы, но не apply_runs /
// incarnation / state_history — здесь нужен именно apply-lifecycle набор.
func resetRecoveryE2E(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE apply_runs, state_history, incarnation, operators, audit_log CASCADE`); err != nil {
		t.Fatalf("TRUNCATE recovery-e2e: %v", err)
	}
}

// seedRecoveryIncarnation создаёт оператора и incarnation с известным state —
// контроль «correlateRunResult не трогает incarnation.state».
func seedRecoveryIncarnation(t *testing.T, name string, state map[string]any) {
	t.Helper()
	ctx := context.Background()
	aid := "archon-alice"
	if err := operator.Insert(ctx, integrationPool, &operator.Operator{
		AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT,
	}); err != nil {
		t.Fatalf("operator.Insert: %v", err)
	}
	creator := aid
	if err := incarnation.Create(ctx, integrationPool, &incarnation.Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, State: state,
		Status: incarnation.StatusReady, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("incarnation.Create: %v", err)
	}
}

// newRecoveryHandler — handler поверх живого PG-пула с зарегистрированными
// keeper_grpc_*-метриками (scrape keeper_runresult_stale_total) и
// recordingAudit-ом. SeedDB не нужен (correlateRunResult ходит только в
// ApplyRunDB), но validate() требует SeedDB+AuditWriter — даём fake/recording.
func newRecoveryHandler(t *testing.T) (*eventStreamHandler, *recordingAudit, *obs.Registry) {
	t.Helper()
	reg := obs.NewRegistry()
	aw := &recordingAudit{}
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		ApplyRunDB:  integrationPool,
		Metrics:     RegisterGRPCMetrics(reg),
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardIntegrationLogger()), aw, reg
}

// readApplyStatus читает status строки `(applyID, sid)` напрямую (без CRUD-guard-ов).
func readApplyStatus(t *testing.T, ctx context.Context, applyID, sid string) string {
	t.Helper()
	var st string
	if err := integrationPool.QueryRow(ctx,
		`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, sid).Scan(&st); err != nil {
		t.Fatalf("read apply_runs status: %v", err)
	}
	return st
}

// readIncarnationState читает incarnation.state как text (для сравнения «не тронут»).
func readIncarnationState(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	var s string
	if err := integrationPool.QueryRow(ctx,
		`SELECT state::text FROM incarnation WHERE name = $1`, name).Scan(&s); err != nil {
		t.Fatalf("read incarnation.state: %v", err)
	}
	return s
}

// TestIntegration_RecoveryReclaim_StaleRunResultDropped_LiveAttempt2Commits —
// СКВОЗНОЙ S4↔S1/S5 на живом PG:
//
//	InsertPlanned → ClaimNext(attempt=1) → MarkDispatched(claimed→dispatched)
//	  → эмуляция смерти владельца + протухание lease + reclaim (dispatched
//	    нельзя реклеймить, поэтому сначала «откатываем» к claimed как
//	    недо-доставленный рендер, затем ReclaimApplyRuns по истёкшему lease
//	    возвращает в planned, attempt сохраняется)
//	  → ClaimNext(attempt=2)
//	  → correlateRunResult(RunResult{attempt=1}) = устаревшая попытка:
//	    stale-drop + keeper_runresult_stale_total++ + строка НЕ-терминальна +
//	    incarnation.state не тронут
//	  → correlateRunResult(RunResult{attempt=2}) = актуальная попытка:
//	    commit (строка терминалится success).
//
// Утверждает, что attempt реально доезжает через пере-claim, а
// correlateRunResult читает ту же строку и применяет epoch-check к ней.
func TestIntegration_RecoveryReclaim_StaleRunResultDropped_LiveAttempt2Commits(t *testing.T) {
	resetRecoveryE2E(t)
	const (
		incName = "redis-prod"
		applyID = "01HRECOVERYE2E0000000000"
		sid     = "host.example.com"
	)
	knownState := map[string]any{"replicas": float64(3), "version": "7.2"}
	seedRecoveryIncarnation(t, incName, knownState)
	ctx := context.Background()
	aid := "archon-alice"

	// 1) InsertPlanned — planned-задание под Acolyte-claim.
	if err := applyrun.InsertPlanned(ctx, integrationPool, &applyrun.ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: incName, Scenario: "scale",
		StartedByAID: &aid,
		Recipe: &applyrun.Recipe{
			ScenarioName: "scale",
			Input:        map[string]any{"replicas": float64(5)},
			StartedByAID: &aid,
		},
	}); err != nil {
		t.Fatalf("InsertPlanned: %v", err)
	}

	// 2) ClaimNext → attempt 0→1; MarkDispatched → claimed→dispatched.
	claimed, err := applyrun.ClaimNext(ctx, integrationPool, "keeper-dead", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext#1: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Attempt != 1 {
		t.Fatalf("первый claim: len=%d attempt=%v, want 1/1", len(claimed), claimed)
	}
	if err := applyrun.MarkDispatched(ctx, integrationPool, applyID, sid); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	if got := readApplyStatus(t, ctx, applyID, sid); got != string(applyrun.StatusDispatched) {
		t.Fatalf("после MarkDispatched status=%q, want dispatched", got)
	}

	// 3) Эмуляция смерти владельца ДО отдачи + протухание lease + пере-claim.
	// В Acolyte-флоу reclaim сужен до status='claimed' (S4): dispatched НЕ
	// реклеймится by design. Здесь моделируем «владелец умер, не успев
	// дорендерить/доотдать» — строка трактуется как недо-доставленный claimed с
	// истёкшим lease. Сначала переводим строку в claimed с уже истёкшим
	// claim_expires_at (как если бы dispatched не успел проставиться), затем
	// reclaim-предикатом recovery (status='claimed' AND claim_expires_at < NOW()
	// → planned, attempt СОХРАНЯЕТСЯ) возвращаем её в очередь. SQL зеркалит
	// reaper.reclaimApplyRunsSQL — тянуть пакет reaper ради одного UPDATE-а
	// нецелесообразно, предикат воспроизведён буквально.
	if _, err := integrationPool.Exec(ctx, `
		UPDATE apply_runs
		SET status='claimed', claim_by_kid='keeper-dead', claim_at=NOW() - INTERVAL '2 hours',
		    claim_expires_at=NOW() - INTERVAL '1 hour'
		WHERE apply_id=$1 AND sid=$2`, applyID, sid); err != nil {
		t.Fatalf("эмуляция истёкшего claimed: %v", err)
	}
	tag, err := integrationPool.Exec(ctx, `
		UPDATE apply_runs
		SET status='planned', claim_by_kid=NULL, claim_at=NULL, claim_expires_at=NULL
		WHERE status='claimed' AND claim_expires_at < NOW()`)
	if err != nil {
		t.Fatalf("reclaim протухшего claimed: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("reclaim затронул %d строк, want 1 (протухший claimed → planned)", tag.RowsAffected())
	}
	if got := readApplyStatus(t, ctx, applyID, sid); got != string(applyrun.StatusPlanned) {
		t.Fatalf("после reclaim status=%q, want planned", got)
	}

	// 4) ClaimNext снова → attempt 1→2 (fencing-epoch вырос: новый владелец).
	reclaim, err := applyrun.ClaimNext(ctx, integrationPool, "keeper-live", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext#2: %v", err)
	}
	if len(reclaim) != 1 {
		t.Fatalf("повторный claim len=%d, want 1", len(reclaim))
	}
	if reclaim[0].Attempt != 2 {
		t.Fatalf("повторный claim attempt=%d, want 2 (1→2 через пере-claim)", reclaim[0].Attempt)
	}
	// Доводим до dispatched, как сделал бы живой Acolyte перед SendApply.
	if err := applyrun.MarkDispatched(ctx, integrationPool, applyID, sid); err != nil {
		t.Fatalf("MarkDispatched#2: %v", err)
	}

	h, aw, reg := newRecoveryHandler(t)
	stateBefore := readIncarnationState(t, ctx, incName)

	// 5) RunResult от ПЕРВОЙ (устаревшей) попытки attempt=1 — через реальный handler.
	h.handleRunResult(ctx, sid, "session-stale", &keeperv1.RunResult{
		ApplyId: applyID, Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS, Attempt: 1,
	})

	// ASSERT: метрика stale +1.
	if body := obstest.Scrape(t, reg.Gatherer()); !strings.Contains(body, "keeper_runresult_stale_total 1") {
		t.Errorf("keeper_runresult_stale_total != 1 после stale RunResult; got=\n%s", body)
	}
	// ASSERT: строка осталась НЕ-терминальной (dispatched от 2-го claim).
	if got := readApplyStatus(t, ctx, applyID, sid); got != string(applyrun.StatusDispatched) {
		t.Errorf("после stale RunResult status=%q, want dispatched (не терминал)", got)
	}
	// ASSERT: incarnation.state не изменён (correlateRunResult его не трогает).
	if got := readIncarnationState(t, ctx, incName); got != stateBefore {
		t.Errorf("incarnation.state изменён stale-результатом: %q → %q", stateBefore, got)
	}
	// audit run.completed пишется ДО correlate — факт приёма зафиксирован даже на stale.
	if len(aw.snapshot()) != 1 {
		t.Errorf("audit events = %d, want 1 (run.completed до correlate)", len(aw.snapshot()))
	}

	// 6) Контраст: RunResult от АКТУАЛЬНОЙ попытки attempt=2 → commit (терминал).
	h.handleRunResult(ctx, sid, "session-live", &keeperv1.RunResult{
		ApplyId: applyID, Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS, Attempt: 2,
	})
	if got := readApplyStatus(t, ctx, applyID, sid); got != string(applyrun.StatusSuccess) {
		t.Errorf("после актуального RunResult status=%q, want success (commit)", got)
	}
	// Метрика stale НЕ выросла повторно: актуальная попытка не stale.
	if body := obstest.Scrape(t, reg.Gatherer()); strings.Contains(body, "keeper_runresult_stale_total 2") {
		t.Errorf("keeper_runresult_stale_total вырос на актуальной попытке; got=\n%s", body)
	}
}
