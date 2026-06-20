//go:build integration

// Integration-тесты CRUD apply_runs через testcontainers-go. Паттерн
// совпадает с keeper/internal/incarnation/integration_test.go.

package applyrun

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
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
			log.Fatalf("applyrun integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("applyrun integration: skipping, docker unavailable: %v", err)
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

func resetAll(t *testing.T) {
	t.Helper()
	// CASCADE: apply_runs → incarnation → operators (FK chain).
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE apply_runs, state_history, incarnation, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedOperator(t *testing.T, aid string) {
	t.Helper()
	op := &operator.Operator{
		AID:         aid,
		DisplayName: aid,
		AuthMethod:  operator.AuthMethodJWT,
	}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seedOperator(%s): %v", aid, err)
	}
}

func seedIncarnation(t *testing.T, name, aid string) {
	t.Helper()
	creator := aid
	inc := &incarnation.Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: incarnation.StatusReady, CreatedByAID: &creator,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnation(%s): %v", name, err)
	}
}

func TestIntegration_Insert_AndSelect(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	aid := "archon-alice"
	run := &ApplyRun{
		ApplyID:         "01HAPPLY0000000000000000",
		SID:             "host.example.com",
		IncarnationName: "redis-prod",
		Scenario:        "create",
		TaskIdx:         intp(0),
		Status:          StatusRunning,
		StartedByAID:    &aid,
	}
	if err := Insert(ctx, integrationPool, run); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if run.StartedAt.IsZero() {
		t.Errorf("StartedAt zero — RETURNING did not fill")
	}

	got, err := SelectByApplyID(ctx, integrationPool, "01HAPPLY0000000000000000", "host.example.com")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.IncarnationName != "redis-prod" || got.Scenario != "create" {
		t.Errorf("got = %+v", got)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.TaskIdx == nil || *got.TaskIdx != 0 {
		t.Errorf("task_idx = %v, want 0", got.TaskIdx)
	}
	if got.FinishedAt != nil {
		t.Errorf("finished_at = %v, want nil for running", got.FinishedAt)
	}
	if got.StartedByAID == nil || *got.StartedByAID != "archon-alice" {
		t.Errorf("started_by_aid = %v", got.StartedByAID)
	}
}

func TestIntegration_Insert_NullableNils(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	run := &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	}
	if err := Insert(ctx, integrationPool, run); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "a", "s")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.TaskIdx != nil || got.ErrorSummary != nil || got.StartedByAID != nil {
		t.Errorf("nullable not nil: %+v", got)
	}
}

func TestIntegration_Insert_DuplicateKey(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	run := &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	}
	if err := Insert(ctx, integrationPool, run); err != nil {
		t.Fatalf("Insert#1: %v", err)
	}
	err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	})
	if !errors.Is(err, ErrApplyRunAlreadyExists) {
		t.Fatalf("err = %v, want ErrApplyRunAlreadyExists", err)
	}
}

func TestIntegration_Insert_SameApplyDifferentSID(t *testing.T) {
	// apply_id-model A: один apply_id, разный sid → две независимые строки.
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	for _, sid := range []string{"host-a", "host-b"} {
		run := &ApplyRun{
			ApplyID: "01HSAMEAPPLY", SID: sid, IncarnationName: "redis-prod",
			Scenario: "scale", Status: StatusRunning,
		}
		if err := Insert(ctx, integrationPool, run); err != nil {
			t.Fatalf("Insert sid=%s: %v", sid, err)
		}
	}
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM apply_runs WHERE apply_id = '01HSAMEAPPLY'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("rows for shared apply_id = %d, want 2", n)
	}
}

func TestIntegration_Insert_FKViolation(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// incarnation отсутствует → FK violation.
	err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "ghost", Scenario: "create",
		Status: StatusRunning,
	})
	if err == nil {
		t.Fatal("Insert with non-existent incarnation: expected error")
	}
	if errors.Is(err, ErrApplyRunAlreadyExists) {
		t.Errorf("FK-violation should NOT be ErrApplyRunAlreadyExists; err = %v", err)
	}
}

func TestIntegration_Insert_CHECKViolation_BadStatus(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	// Обходим Go-validation прямым Exec-ом, проверяя SQL-side CHECK.
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status)
		 VALUES ('a', 's', 'redis-prod', 'create', 'error_locked')`)
	if err == nil {
		t.Fatal("expected CHECK violation for status='error_locked'")
	}
}

func TestIntegration_UpdateStatus_SetsFinishedAt(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := UpdateStatus(ctx, integrationPool, "a", "s", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "a", "s")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusSuccess {
		t.Errorf("status = %q, want success", got.Status)
	}
	if got.FinishedAt == nil {
		t.Errorf("finished_at nil after terminal status; want set")
	}
}

// TestIntegration_UpdateStatus_ErrorSummaryCoalesce — COALESCE-семантика
// error_summary на ЛЕГИТИМНОМ переходе running→terminal: summary пишется при
// running→failed. Append-only guard (ADR-027(j), S-P2.4) запрещает
// terminal→terminal перезапись — повторный UpdateStatus(failed→failed) теперь
// отвергается [ErrApplyRunAlreadyTerminal] (а не выполняет ещё один COALESCE).
// Тест отражает новый инвариант, а не обходит его: первый коммиттер-терминал
// побеждает, второй — no-op-отказ.
func TestIntegration_UpdateStatus_ErrorSummaryCoalesce(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// running→failed: summary записан (COALESCE на легитимном переходе).
	if err := UpdateStatus(ctx, integrationPool, "a", "s", 0, StatusFailed, strp("boom")); err != nil {
		t.Fatalf("UpdateStatus#1 (running→failed): %v", err)
	}

	// Повторный terminal-write (failed→failed) отвергается append-only guard-ом
	// ДО любой записи — caller трактует как no-op (первый коммиттер победил).
	err := UpdateStatus(ctx, integrationPool, "a", "s", 0, StatusFailed, nil)
	if !errors.Is(err, ErrApplyRunAlreadyTerminal) {
		t.Fatalf("UpdateStatus#2 (failed→failed): err = %v, want ErrApplyRunAlreadyTerminal", err)
	}

	// Отвергнутый второй write не тронул error_summary: остался "boom".
	got, err := SelectByApplyID(ctx, integrationPool, "a", "s")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.ErrorSummary == nil || *got.ErrorSummary != "boom" {
		t.Errorf("error_summary = %v, want preserved \"boom\"", got.ErrorSummary)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want failed (первый терминал зафиксирован)", got.Status)
	}
}

func TestIntegration_UpdateStatus_NotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	err := UpdateStatus(ctx, integrationPool, "ghost", "s", 0, StatusSuccess, nil)
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

func TestIntegration_SelectIncarnationByApplyID(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "scale",
		Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	name, scenario, attempt, err := SelectIncarnationByApplyID(ctx, integrationPool, "a", "s", 0)
	if err != nil {
		t.Fatalf("SelectIncarnationByApplyID: %v", err)
	}
	if name != "redis-prod" || scenario != "scale" {
		t.Errorf("got (%q, %q), want (redis-prod, scale)", name, scenario)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d, want 0 (свежий Insert без claim, DEFAULT 0)", attempt)
	}
}

func TestIntegration_SelectIncarnationByApplyID_NotFound(t *testing.T) {
	resetAll(t)
	_, _, _, err := SelectIncarnationByApplyID(context.Background(), integrationPool, "ghost", "s", 0)
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

func TestIntegration_ApplyRuns_FK_OnIncarnationDelete(t *testing.T) {
	// CASCADE: при удалении incarnation строки apply_runs исчезают.
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM incarnation WHERE name = 'redis-prod'`); err != nil {
		t.Fatalf("DELETE incarnation: %v", err)
	}
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM apply_runs WHERE incarnation_name = 'redis-prod'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("apply_runs rows after CASCADE = %d, want 0", n)
	}
}

func TestIntegration_ApplyRuns_FK_OnOperatorDelete_SetsNull(t *testing.T) {
	// SET NULL: при удалении оператора started_by_aid обнуляется.
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	aid := "archon-alice"
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "a", SID: "s", IncarnationName: "redis-prod", Scenario: "create",
		Status: StatusRunning, StartedByAID: &aid,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// incarnation создан archon-alice → сначала обнуляем его FK там, чтобы
	// удалить оператора без конфликта (incarnation.created_by_aid тоже SET NULL).
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM operators WHERE aid = 'archon-alice'`); err != nil {
		t.Fatalf("DELETE operator: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "a", "s")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.StartedByAID != nil {
		t.Errorf("started_by_aid = %v after operator delete, want nil (SET NULL)", got.StartedByAID)
	}
}

func TestIntegration_SelectStatusesByApplyID(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// Три хоста одного прогона (apply_id-model A), плюс шумовая строка
	// другого apply_id — она в выборку попасть не должна.
	for _, sid := range []string{"host-c", "host-a", "host-b"} {
		if err := Insert(ctx, integrationPool, &ApplyRun{
			ApplyID: "01HBARRIER", SID: sid, IncarnationName: "redis-prod",
			Scenario: "restart", Status: StatusRunning,
		}); err != nil {
			t.Fatalf("Insert sid=%s: %v", sid, err)
		}
	}
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HOTHER", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "restart", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert noise: %v", err)
	}

	// Переводим один хост в success, другой в failed с summary.
	if err := UpdateStatus(ctx, integrationPool, "01HBARRIER", "host-a", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus host-a: %v", err)
	}
	if err := UpdateStatus(ctx, integrationPool, "01HBARRIER", "host-b", 0, StatusFailed, strp("boom")); err != nil {
		t.Fatalf("UpdateStatus host-b: %v", err)
	}

	got, err := SelectStatusesByApplyID(ctx, integrationPool, "01HBARRIER")
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (noise apply_id excluded)", len(got))
	}
	// ORDER BY sid ASC.
	wantSIDs := []string{"host-a", "host-b", "host-c"}
	for i, hs := range got {
		if hs.SID != wantSIDs[i] {
			t.Errorf("got[%d].SID = %q, want %q (sorted)", i, hs.SID, wantSIDs[i])
		}
	}
	if got[0].Status != StatusSuccess {
		t.Errorf("host-a status = %q, want success", got[0].Status)
	}
	if got[1].Status != StatusFailed {
		t.Errorf("host-b status = %q, want failed", got[1].Status)
	}
	if got[1].ErrorSummary == nil || *got[1].ErrorSummary != "boom" {
		t.Errorf("host-b error_summary = %v, want boom", got[1].ErrorSummary)
	}
	if got[2].Status != StatusRunning {
		t.Errorf("host-c status = %q, want running", got[2].Status)
	}
}

func TestIntegration_SelectStatusesByApplyID_Empty(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	got, err := SelectStatusesByApplyID(ctx, integrationPool, "01HNONE")
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0 for unknown apply_id", len(got))
	}
}

// TestIntegration_RequestCancel_FlagsRunningHosts — cluster-wide Cancel (G1):
// RequestCancel ставит cancel_requested на все running-строки прогона; флаг
// читается обратно через SelectStatusesByApplyID (тот путь, по которому его
// видит barrier-поллинг run-goroutine-а).
func TestIntegration_RequestCancel_FlagsRunningHosts(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	for _, sid := range []string{"host-a", "host-b"} {
		if err := Insert(ctx, integrationPool, &ApplyRun{
			ApplyID: "01HCANCEL", SID: sid, IncarnationName: "redis-prod",
			Scenario: "restart", Status: StatusRunning,
		}); err != nil {
			t.Fatalf("Insert sid=%s: %v", sid, err)
		}
	}
	// Шумовой прогон — его cancel_requested трогать нельзя.
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HKEEP", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "restart", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert noise: %v", err)
	}

	// До отмены флаг нигде не стоит.
	before, err := SelectStatusesByApplyID(ctx, integrationPool, "01HCANCEL")
	if err != nil {
		t.Fatalf("SelectStatuses before: %v", err)
	}
	for _, hs := range before {
		if hs.CancelRequested {
			t.Fatalf("host %s: cancel_requested=true до RequestCancel", hs.SID)
		}
	}

	affected, err := RequestCancel(ctx, integrationPool, "01HCANCEL")
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected != 2 {
		t.Errorf("affected = %d, want 2 (оба running-хоста прогона)", affected)
	}

	after, err := SelectStatusesByApplyID(ctx, integrationPool, "01HCANCEL")
	if err != nil {
		t.Fatalf("SelectStatuses after: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("len = %d, want 2", len(after))
	}
	for _, hs := range after {
		if !hs.CancelRequested {
			t.Errorf("host %s: cancel_requested=false после RequestCancel", hs.SID)
		}
	}

	// Шумовой прогон не затронут.
	noise, err := SelectStatusesByApplyID(ctx, integrationPool, "01HKEEP")
	if err != nil {
		t.Fatalf("SelectStatuses noise: %v", err)
	}
	if len(noise) != 1 || noise[0].CancelRequested {
		t.Errorf("шумовой прогон затронут RequestCancel: %+v", noise)
	}
}

// TestIntegration_RequestCancel_TerminalNoOp — Cancel уже завершённого
// прогона (все строки терминальны) не ставит флаг: affected=0, no-op.
func TestIntegration_RequestCancel_TerminalNoOp(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HDONE", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "restart", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := UpdateStatus(ctx, integrationPool, "01HDONE", "host-a", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	affected, err := RequestCancel(ctx, integrationPool, "01HDONE")
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 (терминальный прогон — no-op)", affected)
	}
	got, err := SelectStatusesByApplyID(ctx, integrationPool, "01HDONE")
	if err != nil {
		t.Fatalf("SelectStatuses: %v", err)
	}
	if len(got) != 1 || got[0].CancelRequested {
		t.Errorf("терминальный прогон получил cancel_requested: %+v", got)
	}
}

// TestIntegration_RequestCancel_Idempotent — повторный RequestCancel на
// running-прогоне идемпотентен (флаг true→true, без ошибки).
func TestIntegration_RequestCancel_Idempotent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedApplyRun(t, "01HTWICE", "host-a")

	first, err := RequestCancel(ctx, integrationPool, "01HTWICE")
	if err != nil {
		t.Fatalf("RequestCancel first: %v", err)
	}
	second, err := RequestCancel(ctx, integrationPool, "01HTWICE")
	if err != nil {
		t.Fatalf("RequestCancel second: %v", err)
	}
	if first != 1 || second != 1 {
		t.Errorf("affected first=%d second=%d, want 1 и 1 (идемпотентно)", first, second)
	}
}

// TestIntegration_RequestCancel_UnknownApplyID — отмена несуществующего
// прогона: affected=0, без ошибки.
func TestIntegration_RequestCancel_UnknownApplyID(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	affected, err := RequestCancel(ctx, integrationPool, "01HGHOST")
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d, want 0 для неизвестного apply_id", affected)
	}
}

// TestIntegration_RequestCancel_PartialRunning — прогон с mixed-хостами: один
// уже success (терминал), второй ещё running. Фильтр status='running' в
// RequestCancel ставит флаг ТОЛЬКО на running-строку (affected=1, не 2);
// barrier-у этого достаточно ([cancelRequested] видит true на любой строке).
// Граница между all-running ([TestIntegration_RequestCancel_FlagsRunningHosts])
// и all-terminal ([TestIntegration_RequestCancel_TerminalNoOp]): отмена прогона,
// у которого часть хостов уже отстрелялась.
func TestIntegration_RequestCancel_PartialRunning(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	for _, sid := range []string{"host-a", "host-b"} {
		if err := Insert(ctx, integrationPool, &ApplyRun{
			ApplyID: "01HMIXED", SID: sid, IncarnationName: "redis-prod",
			Scenario: "restart", Status: StatusRunning,
		}); err != nil {
			t.Fatalf("Insert sid=%s: %v", sid, err)
		}
	}
	// host-a уже завершился success — он не должен получить cancel_requested.
	if err := UpdateStatus(ctx, integrationPool, "01HMIXED", "host-a", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus host-a: %v", err)
	}

	affected, err := RequestCancel(ctx, integrationPool, "01HMIXED")
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if affected != 1 {
		t.Errorf("affected = %d, want 1 (только ещё-running host-b)", affected)
	}

	got, err := SelectStatusesByApplyID(ctx, integrationPool, "01HMIXED")
	if err != nil {
		t.Fatalf("SelectStatuses: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	byID := map[string]HostStatus{got[0].SID: got[0], got[1].SID: got[1]}
	if byID["host-a"].CancelRequested {
		t.Error("host-a (success-терминал) получил cancel_requested — фильтр status='running' нарушен")
	}
	if !byID["host-b"].CancelRequested {
		t.Error("host-b (running) не получил cancel_requested")
	}
	// barrier увидит флаг на любой строке прогона — partial-флага достаточно.
	if !cancelRequestedAny(got) {
		t.Error("ни одна строка не несёт cancel_requested — barrier не отменит прогон")
	}
}

// cancelRequestedAny — тестовое зеркало scenario.cancelRequested: barrier-у
// достаточно флага на любой строке прогона. Дублируем приватный хелпер пакета
// scenario здесь, чтобы applyrun-тест не тянул import scenario.
func cancelRequestedAny(statuses []HostStatus) bool {
	for i := range statuses {
		if statuses[i].CancelRequested {
			return true
		}
	}
	return false
}

func TestIntegration_RecordTaskFailure_FirstFailureWins(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedApplyRun(t, "01HFAIL", "host-a")

	// Первая упавшая задача фиксирует task_idx (локальный 1) + failed_plan_index
	// (глобальный 4 — staged/per-host-where, локальный ≠ глобальный) + summary.
	if err := RecordTaskFailure(ctx, integrationPool, "01HFAIL", "host-a", 0, 1, 4,
		"task 4 core.pkg.installed: E: Version '7.2.4' not found"); err != nil {
		t.Fatalf("RecordTaskFailure first: %v", err)
	}
	// Вторая упавшая задача НЕ затирает (COALESCE first-failure-wins) — ни
	// task_idx, ни failed_plan_index, ни summary.
	if err := RecordTaskFailure(ctx, integrationPool, "01HFAIL", "host-a", 0, 3, 9, "task 9 later boom"); err != nil {
		t.Fatalf("RecordTaskFailure second: %v", err)
	}

	got, err := SelectByApplyID(ctx, integrationPool, "01HFAIL", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.TaskIdx == nil || *got.TaskIdx != 1 {
		t.Errorf("task_idx = %v, want 1 (первая упавшая задача, локальный)", got.TaskIdx)
	}
	if got.ErrorSummary == nil || *got.ErrorSummary != "task 4 core.pkg.installed: E: Version '7.2.4' not found" {
		t.Errorf("error_summary = %v, want первой задачи", got.ErrorSummary)
	}
	// Статус остаётся running до RunResult.
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running (RecordTaskFailure не трогает статус)", got.Status)
	}

	// failed_plan_index читается через HostStatus-проекцию (SelectByApplyID её не
	// несёт). Глобальный индекс первой упавшей задачи = 4 (first-failure-wins).
	statuses, err := SelectStatusesByApplyID(ctx, integrationPool, "01HFAIL")
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses len = %d, want 1", len(statuses))
	}
	hs := statuses[0]
	if hs.FailedPlanIndex == nil || *hs.FailedPlanIndex != 4 {
		t.Errorf("★ failed_plan_index = %v, want 4 (глобальный, первая упавшая задача — не затёрт второй с 9)", hs.FailedPlanIndex)
	}
	if hs.TaskIdx == nil || *hs.TaskIdx != 1 {
		t.Errorf("HostStatus.TaskIdx = %v, want 1 (локальный)", hs.TaskIdx)
	}
}

func TestIntegration_RecordTaskFailure_NotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	err := RecordTaskFailure(ctx, integrationPool, "01HMISSING", "host-x", 0, 0, 0, "boom")
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Fatalf("err = %v, want ErrApplyRunNotFound", err)
	}
}

// TestIntegration_WardClaimColumns_Phase0 — критерий приёмки ADR-027 Phase 0:
// миграция 025 применена к схеме контейнера; существующая apply_runs-строка
// старого пути (Insert) сосуществует с новыми Ward-claim колонками, которые
// получают DEFAULT (attempt=0, claim_* NULL) и никем не пишутся; CHECK status
// принимает planned/claimed и сохраняет running/success/failed/cancelled;
// partial-индекс claim-скана создан. Down/up-обратимость 025 покрывается
// migrate-пакетом (TestIntegration_MigrateApply_DownThenUp прогоняет полный
// down→up) + sanity на содержимое down.sql (migrations_test).
func TestIntegration_WardClaimColumns_Phase0(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// Строка старого пути через обычный Insert — CRUD не знает про Ward-claim.
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HWARD", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert legacy row: %v", err)
	}

	// Ward-claim колонки существуют и несут DEFAULT-значения у строки старого пути.
	var (
		attempt        int
		claimByKID     *string
		claimAt        *time.Time
		claimExpiresAt *time.Time
	)
	if err := integrationPool.QueryRow(ctx, `
		SELECT attempt, claim_by_kid, claim_at, claim_expires_at
		FROM apply_runs WHERE apply_id = '01HWARD' AND sid = 'host-a'
	`).Scan(&attempt, &claimByKID, &claimAt, &claimExpiresAt); err != nil {
		t.Fatalf("select ward-claim columns: %v", err)
	}
	if attempt != 0 {
		t.Errorf("attempt = %d, want 0 (DEFAULT для строки старого пути)", attempt)
	}
	if claimByKID != nil || claimAt != nil || claimExpiresAt != nil {
		t.Errorf("claim_* должны быть NULL у строки старого пути: %v / %v / %v",
			claimByKID, claimAt, claimExpiresAt)
	}

	// CHECK принимает planned/claimed (raw SQL: ValidStatus намеренно их не
	// пропускает в Phase 0, поэтому вставляем мимо CRUD).
	for _, st := range []string{"planned", "claimed"} {
		if _, err := integrationPool.Exec(ctx, `
			INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status)
			VALUES ($1, 'host-x', 'redis-prod', 'create', $2)
		`, "01HWARD-"+st, st); err != nil {
			t.Errorf("status %q отвергнут CHECK-ом после 025: %v", st, err)
		}
	}
	// Сохранены старые значения: cancelled (024-эра) по-прежнему валиден.
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status)
		VALUES ('01HWARD-cancelled', 'host-y', 'redis-prod', 'create', 'cancelled')
	`); err != nil {
		t.Errorf("status 'cancelled' отвергнут после 025 (регрессия): %v", err)
	}
	// Невалидный статус по-прежнему отвергается.
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO apply_runs (apply_id, sid, incarnation_name, scenario, status)
		VALUES ('01HWARD-bad', 'host-z', 'redis-prod', 'create', 'bogus')
	`); err == nil {
		t.Error("status 'bogus' принят, ожидали CHECK violation")
	}

	// Partial-индекс под claim-скан создан.
	var idxExists bool
	if err := integrationPool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = 'apply_runs_claim_scan_idx'
		)
	`).Scan(&idxExists); err != nil {
		t.Fatalf("pg_indexes claim_scan: %v", err)
	}
	if !idxExists {
		t.Error("apply_runs_claim_scan_idx отсутствует после 025")
	}

	// Текущий путь не сломан: Insert/SelectByApplyID работают как раньше,
	// существующая строка не затронута Ward-claim колонками.
	got, err := SelectByApplyID(ctx, integrationPool, "01HWARD", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID после 025: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running (текущий путь не изменился)", got.Status)
	}
}

// seedApplyRun вставляет минимальную apply_runs-строку (FK-родитель для
// apply_task_register).
func seedApplyRun(t *testing.T, applyID, sid string) {
	t.Helper()
	if err := Insert(context.Background(), integrationPool, &ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("seedApplyRun(%s,%s): %v", applyID, sid, err)
	}
}

func TestIntegration_TaskRegister_UpsertAndSelect(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedApplyRun(t, "01HREG", "host-a")
	seedApplyRun(t, "01HREG", "host-b")

	// PlanIndex — ключ корреляции (PK-компонент, миграция 079); N=1 → ==TaskIdx.
	rows := []*TaskRegister{
		{ApplyID: "01HREG", SID: "host-a", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "a0", "rc": float64(0)}},
		{ApplyID: "01HREG", SID: "host-a", PlanIndex: 2, TaskIdx: 2, RegisterData: map[string]any{"stdout": "a2"}},
		{ApplyID: "01HREG", SID: "host-b", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "b0"}},
	}
	for _, r := range rows {
		if err := UpsertTaskRegister(ctx, integrationPool, r); err != nil {
			t.Fatalf("UpsertTaskRegister(%s,%d): %v", r.SID, r.PlanIndex, err)
		}
	}

	got, err := SelectTaskRegistersByApplyID(ctx, integrationPool, "01HREG")
	if err != nil {
		t.Fatalf("SelectTaskRegistersByApplyID: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Сортировка (sid, plan_index): host-a/0, host-a/2, host-b/0.
	if got[0].SID != "host-a" || got[0].PlanIndex != 0 {
		t.Errorf("got[0] = %s/%d, want host-a/0", got[0].SID, got[0].PlanIndex)
	}
	if got[1].SID != "host-a" || got[1].PlanIndex != 2 {
		t.Errorf("got[1] = %s/%d, want host-a/2", got[1].SID, got[1].PlanIndex)
	}
	if got[2].SID != "host-b" || got[2].PlanIndex != 0 {
		t.Errorf("got[2] = %s/%d, want host-b/0", got[2].SID, got[2].PlanIndex)
	}
	if got[0].RegisterData["stdout"] != "a0" {
		t.Errorf("got[0].stdout = %v, want a0", got[0].RegisterData["stdout"])
	}
	// jsonb-число читается как float64.
	if got[0].RegisterData["rc"] != float64(0) {
		t.Errorf("got[0].rc = %T(%v), want float64(0)", got[0].RegisterData["rc"], got[0].RegisterData["rc"])
	}
}

func TestIntegration_TaskRegister_UpsertOverwrites(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	seedApplyRun(t, "01HREG", "host-a")

	if err := UpsertTaskRegister(ctx, integrationPool, &TaskRegister{
		ApplyID: "01HREG", SID: "host-a", TaskIdx: 0,
		RegisterData: map[string]any{"stdout": "first"},
	}); err != nil {
		t.Fatalf("Upsert#1: %v", err)
	}
	if err := UpsertTaskRegister(ctx, integrationPool, &TaskRegister{
		ApplyID: "01HREG", SID: "host-a", TaskIdx: 0,
		RegisterData: map[string]any{"stdout": "second"},
	}); err != nil {
		t.Fatalf("Upsert#2: %v", err)
	}

	got, err := SelectTaskRegistersByApplyID(ctx, integrationPool, "01HREG")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (upsert по PK)", len(got))
	}
	if got[0].RegisterData["stdout"] != "second" {
		t.Errorf("stdout = %v, want second (последний побеждает)", got[0].RegisterData["stdout"])
	}
}

func TestIntegration_TaskRegister_FKCascadeOnApplyRunDelete(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	seedApplyRun(t, "01HREG", "host-a")

	if err := UpsertTaskRegister(ctx, integrationPool, &TaskRegister{
		ApplyID: "01HREG", SID: "host-a", TaskIdx: 0,
		RegisterData: map[string]any{"stdout": "x"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Удаление родительской apply_runs-строки → CASCADE чистит register.
	if _, err := integrationPool.Exec(ctx,
		`DELETE FROM apply_runs WHERE apply_id = '01HREG' AND sid = 'host-a'`); err != nil {
		t.Fatalf("DELETE apply_runs: %v", err)
	}
	got, err := SelectTaskRegistersByApplyID(ctx, integrationPool, "01HREG")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0 после CASCADE-удаления родителя", len(got))
	}
}

func TestIntegration_TaskRegister_FKViolation_NoApplyRun(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// Нет apply_runs-родителя → FK violation.
	err := UpsertTaskRegister(ctx, integrationPool, &TaskRegister{
		ApplyID: "01HGHOST", SID: "host-a", TaskIdx: 0,
		RegisterData: map[string]any{"stdout": "x"},
	})
	if err == nil {
		t.Fatal("UpsertTaskRegister без apply_runs-родителя: expected FK violation")
	}
}

// seedPlanned вставляет apply_runs-строку в статус planned (work-queue вход,
// ADR-027): scenario-runner на dispatch-е пишет именно planned, Acolyte клеймит
// её через ClaimNext.
func seedPlanned(t *testing.T, applyID, sid string) {
	t.Helper()
	if err := Insert(context.Background(), integrationPool, &ApplyRun{
		ApplyID: applyID, SID: sid, IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusPlanned,
	}); err != nil {
		t.Fatalf("seedPlanned(%s,%s): %v", applyID, sid, err)
	}
}

// TestIntegration_ValidStatus_PlannedClaimedNowValid — Phase 1: planned/claimed
// стали валидными для CRUD-слоя ([Insert]/[UpdateStatus]); при этом старый путь
// (прямой Insert(running)) не сломан.
func TestIntegration_ValidStatus_PlannedClaimedNowValid(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// planned теперь проходит Go-validation Insert-а.
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HVS", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusPlanned,
	}); err != nil {
		t.Fatalf("Insert(planned): %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HVS", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusPlanned {
		t.Errorf("status = %q, want planned", got.Status)
	}

	// Старый путь не тронут: Insert(running) по-прежнему работает.
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HVS", SID: "host-b", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusRunning,
	}); err != nil {
		t.Fatalf("Insert(running) legacy path сломан: %v", err)
	}
	gotB, err := SelectByApplyID(ctx, integrationPool, "01HVS", "host-b")
	if err != nil {
		t.Fatalf("SelectByApplyID host-b: %v", err)
	}
	if gotB.Status != StatusRunning {
		t.Errorf("status = %q, want running (legacy path)", gotB.Status)
	}
}

// TestIntegration_ClaimNext_PlannedToClaimed — базовый claim: planned → claimed,
// выставлены claim_by_kid/claim_at/claim_expires_at, attempt 0→1.
func TestIntegration_ClaimNext_PlannedToClaimed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedPlanned(t, "01HCLAIM", "host-a")

	claimed, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed len = %d, want 1", len(claimed))
	}
	c := claimed[0]
	if c.Status != StatusClaimed {
		t.Errorf("status = %q, want claimed", c.Status)
	}
	if c.ClaimByKID == nil || *c.ClaimByKID != "keeper-1" {
		t.Errorf("claim_by_kid = %v, want keeper-1", c.ClaimByKID)
	}
	if c.ClaimAt == nil {
		t.Errorf("claim_at nil, want set")
	}
	if c.ClaimExpiresAt == nil {
		t.Errorf("claim_expires_at nil, want set")
	}
	if c.ClaimAt != nil && c.ClaimExpiresAt != nil && !c.ClaimExpiresAt.After(*c.ClaimAt) {
		t.Errorf("claim_expires_at %v не позже claim_at %v", c.ClaimExpiresAt, c.ClaimAt)
	}
	if c.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 (0→1)", c.Attempt)
	}

	// Персистентность: повторный SELECT видит claimed.
	got, err := SelectByApplyID(ctx, integrationPool, "01HCLAIM", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusClaimed {
		t.Errorf("persisted status = %q, want claimed", got.Status)
	}
}

// TestIntegration_ClaimNext_NoPlanned — нет planned-заданий → пустой срез, не ошибка.
func TestIntegration_ClaimNext_NoPlanned(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// Шумовая running-строка не должна клеймиться (только planned).
	seedApplyRun(t, "01HRUN", "host-a")

	claimed, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("claimed len = %d, want 0 (нет planned)", len(claimed))
	}
}

// TestIntegration_ClaimNext_BatchLimit — захватывается не больше batch-строк.
func TestIntegration_ClaimNext_BatchLimit(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	for _, sid := range []string{"host-a", "host-b", "host-c", "host-d", "host-e"} {
		seedPlanned(t, "01HBATCH", sid)
	}

	first, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ClaimNext#1: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first batch len = %d, want 2 (limit)", len(first))
	}

	// Остаток (3 planned) — следующим claim-ом, тем же limit-ом 2 → 2, потом 1.
	second, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ClaimNext#2: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("second batch len = %d, want 2", len(second))
	}
	third, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ClaimNext#3: %v", err)
	}
	if len(third) != 1 {
		t.Fatalf("third batch len = %d, want 1 (остаток)", len(third))
	}
}

// TestIntegration_ClaimNext_Concurrent — ключевой тест корректности: два
// параллельных ClaimNext (разные Acolyte-ы / KID) над общим набором planned НЕ
// получают одну и ту же строку. Гарантирует это FOR UPDATE SKIP LOCKED.
// Объединение захваченных множеств == полный набор, пересечение пусто.
func TestIntegration_ClaimNext_Concurrent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	const n = 40
	for i := 0; i < n; i++ {
		seedPlanned(t, "01HCONC", "host-"+strconv.Itoa(i))
	}

	type res struct {
		runs []*ApplyRun
		err  error
	}
	var (
		wg      sync.WaitGroup
		results [2]res
	)
	kids := [2]string{"keeper-1", "keeper-2"}
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Каждый воркер клеймит пачками, пока planned не кончатся.
			for {
				batch, err := ClaimNext(ctx, integrationPool, kids[idx], 60*time.Second, 5)
				if err != nil {
					results[idx].err = err
					return
				}
				if len(batch) == 0 {
					return
				}
				results[idx].runs = append(results[idx].runs, batch...)
			}
		}(w)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("worker %d: %v", i, r.err)
		}
	}

	// Ни одна строка не захвачена дважды; объединение покрывает весь набор.
	seen := make(map[string]string) // sid → kid, захвативший её
	total := 0
	for i, r := range results {
		for _, run := range r.runs {
			total++
			if prev, dup := seen[run.SID]; dup {
				t.Fatalf("строка sid=%s захвачена дважды: kid=%s и kid=%s (FOR UPDATE SKIP LOCKED нарушен)",
					run.SID, prev, kids[i])
			}
			seen[run.SID] = kids[i]
			// Захватившая строку — её KID и attempt=1.
			if run.ClaimByKID == nil || *run.ClaimByKID != kids[i] {
				t.Errorf("sid=%s claim_by_kid=%v, want %s", run.SID, run.ClaimByKID, kids[i])
			}
			if run.Attempt != 1 {
				t.Errorf("sid=%s attempt=%d, want 1", run.SID, run.Attempt)
			}
		}
	}
	if total != n {
		t.Errorf("суммарно захвачено %d, want %d (часть planned потеряна)", total, n)
	}
	if len(seen) != n {
		t.Errorf("уникальных захваченных строк %d, want %d", len(seen), n)
	}
}

// TestIntegration_ClaimNext_AttemptIncrements — attempt инкрементится при
// повторном claim. Эмулируем recovery вручную: claimed → planned reset, затем
// claim снова → attempt 1→2.
func TestIntegration_ClaimNext_AttemptIncrements(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedPlanned(t, "01HEPOCH", "host-a")

	first, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext#1: %v", err)
	}
	if len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("first claim attempt = %v, want 1", first)
	}

	// Recovery-эмуляция: протухший Ward возвращён в planned (claim_* сброшены,
	// attempt СОХРАНЁН — recovery-скан Reaper attempt не трогает, ADR-027(i)).
	if _, err := integrationPool.Exec(ctx, `
		UPDATE apply_runs
		SET status='planned', claim_by_kid=NULL, claim_at=NULL, claim_expires_at=NULL
		WHERE apply_id='01HEPOCH' AND sid='host-a'`); err != nil {
		t.Fatalf("recovery reset: %v", err)
	}

	second, err := ClaimNext(ctx, integrationPool, "keeper-2", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext#2: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second claim len = %d, want 1", len(second))
	}
	if second[0].Attempt != 2 {
		t.Errorf("attempt = %d, want 2 (1→2 при повторном claim)", second[0].Attempt)
	}
	if second[0].ClaimByKID == nil || *second[0].ClaimByKID != "keeper-2" {
		t.Errorf("claim_by_kid = %v, want keeper-2 (новый владелец после recovery)", second[0].ClaimByKID)
	}
}

// TestIntegration_ClaimNext_Validation — пустой kid / неположительные lease/batch
// отвергаются до похода в БД.
func TestIntegration_ClaimNext_Validation(t *testing.T) {
	ctx := context.Background()
	if _, err := ClaimNext(ctx, integrationPool, "", time.Second, 1); err == nil {
		t.Error("empty kid: expected error")
	}
	if _, err := ClaimNext(ctx, integrationPool, "keeper-1", 0, 1); err == nil {
		t.Error("zero lease: expected error")
	}
	if _, err := ClaimNext(ctx, integrationPool, "keeper-1", time.Second, 0); err == nil {
		t.Error("zero batch: expected error")
	}
}

// TestIntegration_MarkDispatched_Ok — claimed → dispatched проходит.
func TestIntegration_MarkDispatched_Ok(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedPlanned(t, "01HMARK", "host-a")
	if _, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10); err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if err := MarkDispatched(ctx, integrationPool, "01HMARK", "host-a"); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HMARK", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusDispatched {
		t.Errorf("status = %q, want dispatched", got.Status)
	}
}

// TestIntegration_MarkDispatched_GuardRejectsNonClaimed — guard: переход
// возможен ТОЛЬКО из claimed. running→dispatched и planned→dispatched отвергаются
// (ErrApplyRunNotClaimed), отсутствующая строка → ErrApplyRunNotFound.
func TestIntegration_MarkDispatched_GuardRejectsNonClaimed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// running → dispatched: guard (running вне claimed).
	seedApplyRun(t, "01HG", "host-running") // вставляет StatusRunning
	if err := MarkDispatched(ctx, integrationPool, "01HG", "host-running"); !errors.Is(err, ErrApplyRunNotClaimed) {
		t.Errorf("running→dispatched: err = %v, want ErrApplyRunNotClaimed", err)
	}

	// planned → dispatched: guard.
	seedPlanned(t, "01HG", "host-planned")
	if err := MarkDispatched(ctx, integrationPool, "01HG", "host-planned"); !errors.Is(err, ErrApplyRunNotClaimed) {
		t.Errorf("planned→dispatched: err = %v, want ErrApplyRunNotClaimed", err)
	}

	// Отсутствующая строка → NotFound.
	if err := MarkDispatched(ctx, integrationPool, "01HG", "ghost"); !errors.Is(err, ErrApplyRunNotFound) {
		t.Errorf("ghost: err = %v, want ErrApplyRunNotFound", err)
	}

	// Повторный MarkDispatched после успешного перехода (dispatched уже) —
	// тоже guard (idempotency: второй вызов не «переподтверждает»).
	seedPlanned(t, "01HG", "host-twice")
	if _, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10); err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if err := MarkDispatched(ctx, integrationPool, "01HG", "host-twice"); err != nil {
		t.Fatalf("MarkDispatched#1: %v", err)
	}
	if err := MarkDispatched(ctx, integrationPool, "01HG", "host-twice"); !errors.Is(err, ErrApplyRunNotClaimed) {
		t.Errorf("повторный MarkDispatched: err = %v, want ErrApplyRunNotClaimed", err)
	}
}

// TestIntegration_InsertPlanned_WithRecipe — Phase 1.4.2: InsertPlanned пишет
// строку status=planned с persisted recipe (колонка 029), attempt=0 DEFAULT,
// Ward-колонки NULL. Инвариант A: recipe.Input несёт vault-ref СТРОКОЙ.
func TestIntegration_InsertPlanned_WithRecipe(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	aid := "archon-alice"
	recipe := &Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "redis", Git: "https://example.test/redis.git", Ref: "main"},
		ScenarioName: "create",
		Input:        map[string]any{"db_password": "vault:secret/db-creds#password"},
		StartedByAID: &aid,
	}
	run := &ApplyRun{
		ApplyID: "01HPLAN", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", StartedByAID: &aid, Recipe: recipe,
	}
	if err := InsertPlanned(ctx, integrationPool, run); err != nil {
		t.Fatalf("InsertPlanned: %v", err)
	}
	if run.Status != StatusPlanned {
		t.Errorf("run.Status = %q, want planned", run.Status)
	}
	if run.StartedAt.IsZero() {
		t.Errorf("StartedAt не заполнен RETURNING-ом")
	}

	got, err := SelectByApplyID(ctx, integrationPool, "01HPLAN", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusPlanned {
		t.Errorf("persisted status = %q, want planned", got.Status)
	}
	if got.Attempt != 0 {
		t.Errorf("attempt = %d, want 0 (DEFAULT, инкрементит claim)", got.Attempt)
	}
	if got.ClaimByKID != nil {
		t.Errorf("claim_by_kid = %v, want NULL до claim", got.ClaimByKID)
	}

	// recipe доезжает через claim: ClaimNext.RETURNING несёт recipe → run.Recipe.
	claimed, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed len = %d, want 1", len(claimed))
	}
	c := claimed[0]
	if c.Recipe == nil {
		t.Fatalf("claimed.Recipe nil — recipe не доехал через ClaimNext")
	}
	if c.Recipe.ScenarioName != "create" {
		t.Errorf("recipe.ScenarioName = %q, want create", c.Recipe.ScenarioName)
	}
	// Инвариант A: vault-ref в персисте — СТРОКОЙ, секрет не раскрыт.
	if c.Recipe.Input["db_password"] != "vault:secret/db-creds#password" {
		t.Errorf("recipe.Input db_password = %v, want vault-ref как есть", c.Recipe.Input["db_password"])
	}
	if c.Recipe.StartedByAID == nil || *c.Recipe.StartedByAID != aid {
		t.Errorf("recipe.StartedByAID = %v, want %q", c.Recipe.StartedByAID, aid)
	}
}

// TestIntegration_InsertPlanned_RejectsNilRecipe — planned-задание без рецепта
// Acolyte отрендерить не может → InsertPlanned отвергает nil-recipe.
func TestIntegration_InsertPlanned_RejectsNilRecipe(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	err := InsertPlanned(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HNIL", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", // Recipe не задан
	})
	if err == nil {
		t.Fatalf("InsertPlanned с nil-recipe прошёл, want ошибку")
	}
}

// dispatchRow помощник: planned → claimed → dispatched для (applyID, sid).
// Возвращает attempt захваченной строки (fencing-epoch после ClaimNext).
func dispatchRow(t *testing.T, applyID, sid string) int {
	t.Helper()
	ctx := context.Background()
	seedPlanned(t, applyID, sid)
	if _, err := ClaimNext(ctx, integrationPool, "keeper-1", 30*time.Second, 10); err != nil {
		t.Fatalf("ClaimNext(%s/%s): %v", applyID, sid, err)
	}
	if err := MarkDispatched(ctx, integrationPool, applyID, sid); err != nil {
		t.Fatalf("MarkDispatched(%s/%s): %v", applyID, sid, err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, applyID, sid)
	if err != nil {
		t.Fatalf("SelectByApplyID(%s/%s): %v", applyID, sid, err)
	}
	return got.Attempt
}

// TestIntegration_OrphanDispatched_SweepsAbsent — Soul-reconcile (ADR-027(g),
// S6) на реальной PG: dispatched-строка SID-а, чей apply_id НЕ в наборе WardRoster,
// терминалится в orphaned (с finished_at + error_summary); строка из набора — НЕ
// тронута; строка ДРУГОГО SID-а — не тронута (sweep per-SID).
func TestIntegration_OrphanDispatched_SweepsAbsent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	dispatchRow(t, "01HLIVE", "host-a")  // объявим живым → не трогать
	dispatchRow(t, "01HDEAD", "host-a")  // вне набора → orphaned
	dispatchRow(t, "01HOTHER", "host-b") // другой SID → не трогать

	n, err := OrphanDispatched(ctx, integrationPool, "host-a", []*ActiveApply{{ApplyID: "01HLIVE"}})
	if err != nil {
		t.Fatalf("OrphanDispatched: %v", err)
	}
	if n != 1 {
		t.Fatalf("orphaned count = %d, want 1 (только 01HDEAD/host-a)", n)
	}

	dead, err := SelectByApplyID(ctx, integrationPool, "01HDEAD", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID dead: %v", err)
	}
	if dead.Status != StatusOrphaned {
		t.Errorf("01HDEAD status = %q, want orphaned", dead.Status)
	}
	if dead.FinishedAt == nil {
		t.Error("01HDEAD finished_at не проставлен")
	}
	if dead.ErrorSummary == nil || *dead.ErrorSummary != orphanDispatchedErrorSummary {
		t.Errorf("01HDEAD error_summary = %v, want фиксированный orphaned-маркер", dead.ErrorSummary)
	}

	// Объявленный живым — остался dispatched.
	live, err := SelectByApplyID(ctx, integrationPool, "01HLIVE", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID live: %v", err)
	}
	if live.Status != StatusDispatched {
		t.Errorf("01HLIVE status = %q, want dispatched (в наборе)", live.Status)
	}

	// Другой SID — не затронут (sweep per-SID).
	other, err := SelectByApplyID(ctx, integrationPool, "01HOTHER", "host-b")
	if err != nil {
		t.Fatalf("SelectByApplyID other: %v", err)
	}
	if other.Status != StatusDispatched {
		t.Errorf("01HOTHER status = %q, want dispatched (другой SID)", other.Status)
	}
}

// TestIntegration_OrphanDispatched_EpochDivergence_NotOrphaned — attempt-разъезд:
// набор несёт тот же apply_id, что и dispatched-строка, но с ДРУГИМ attempt (идёт
// пере-claim). Строка НЕ терминалится — присутствие apply_id в наборе (с любым
// attempt) защищает её от orphan (epoch-fenced: orphan безопаснее НЕ делать).
func TestIntegration_OrphanDispatched_EpochDivergence_NotOrphaned(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	attempt := dispatchRow(t, "01HEPOCH", "host-a")

	// Soul объявил тот же apply_id, но с бОльшим attempt (пере-claim в полёте).
	n, err := OrphanDispatched(ctx, integrationPool, "host-a", []*ActiveApply{
		{ApplyID: "01HEPOCH", Attempt: int32(attempt + 5)},
	})
	if err != nil {
		t.Fatalf("OrphanDispatched: %v", err)
	}
	if n != 0 {
		t.Fatalf("orphaned count = %d, want 0 (apply_id в наборе защищает от orphan)", n)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HEPOCH", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusDispatched {
		t.Errorf("status = %q, want dispatched (attempt-разъезд не терминалит)", got.Status)
	}
}

// TestIntegration_OrphanDispatched_EmptySet_OrphansAll — пустой WardRoster
// (рестарт Soul): все dispatched-строки SID-а терминалятся.
func TestIntegration_OrphanDispatched_EmptySet_OrphansAll(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	dispatchRow(t, "01HA", "host-a")
	dispatchRow(t, "01HB", "host-a")

	n, err := OrphanDispatched(ctx, integrationPool, "host-a", nil)
	if err != nil {
		t.Fatalf("OrphanDispatched(nil): %v", err)
	}
	if n != 2 {
		t.Fatalf("orphaned count = %d, want 2 (пустой набор → все dispatched)", n)
	}
	for _, id := range []string{"01HA", "01HB"} {
		got, err := SelectByApplyID(ctx, integrationPool, id, "host-a")
		if err != nil {
			t.Fatalf("SelectByApplyID %s: %v", id, err)
		}
		if got.Status != StatusOrphaned {
			t.Errorf("%s status = %q, want orphaned", id, got.Status)
		}
	}
}

// TestIntegration_OrphanDispatched_SingleWinnerVsRunResult — гонка sweep ↔ RunResult
// через single-winner: терминал из RunResult (UpdateStatus dispatched→success)
// победил → последующий sweep той же строки даёт 0 (она уже не dispatched).
func TestIntegration_OrphanDispatched_SingleWinnerVsRunResult(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	dispatchRow(t, "01HRACE", "host-a")

	// RunResult пришёл первым: dispatched → success.
	if err := UpdateStatus(ctx, integrationPool, "01HRACE", "host-a", 0, StatusSuccess, nil); err != nil {
		t.Fatalf("UpdateStatus(success): %v", err)
	}

	// sweep с пустым набором НЕ перезаписывает уже терминальную строку (фильтр
	// status='dispatched' отсёк её) — 0 затронутых.
	n, err := OrphanDispatched(ctx, integrationPool, "host-a", nil)
	if err != nil {
		t.Fatalf("OrphanDispatched: %v", err)
	}
	if n != 0 {
		t.Fatalf("orphaned count = %d, want 0 (RunResult-терминал победил, single-winner)", n)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HRACE", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusSuccess {
		t.Errorf("status = %q, want success (не перезаписан orphaned-ом)", got.Status)
	}
}

// TestIntegration_OrphanedStatus_CHECKAllows — миграция 044: CHECK-constraint
// apply_runs_status_valid допускает orphaned (прямой Insert проходит). Симметрично
// TestIntegration_ValidStatus_PlannedClaimedNowValid для 040.
func TestIntegration_OrphanedStatus_CHECKAllows(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HORPH", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusOrphaned,
	}); err != nil {
		t.Fatalf("Insert(orphaned) — CHECK не допускает orphaned после 044: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HORPH", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusOrphaned {
		t.Errorf("status = %q, want orphaned", got.Status)
	}
}

// TestIntegration_NoMatchStatus_CHECKAllows — миграция 045 (FINDING-01 вариант
// (б)): apply_runs_status_valid допускает no_match (прямой Insert проходит).
// Симметрично TestIntegration_OrphanedStatus_CHECKAllows для 044.
func TestIntegration_NoMatchStatus_CHECKAllows(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HNOMATCH", SID: "host-a", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusNoMatch,
	}); err != nil {
		t.Fatalf("Insert(no_match) — CHECK не допускает no_match после 045: %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HNOMATCH", "host-a")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusNoMatch {
		t.Errorf("status = %q, want no_match", got.Status)
	}
}

// TestIntegration_UpdateStatus_NoMatchSetsFinishedAt — claim no-op путь FINDING-01
// (вариант (б)): нецелевой roster-хост переводится planned/claimed → no_match
// через UpdateStatus, который проставляет finished_at (no_match — терминал, не
// running). Без finished_at no_match-строки не попадали бы под purge_apply_runs
// и копились бы вечно.
func TestIntegration_UpdateStatus_NoMatchSetsFinishedAt(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	// planned-строка (как её пишет dispatchPlanned на КАЖДЫЙ roster-хост).
	if err := Insert(ctx, integrationPool, &ApplyRun{
		ApplyID: "01HNM", SID: "host-x", IncarnationName: "redis-prod",
		Scenario: "create", Status: StatusPlanned,
	}); err != nil {
		t.Fatalf("Insert(planned): %v", err)
	}
	// claim no-op: on:/where: отфильтровал всё → no_match (НЕ success).
	if err := UpdateStatus(ctx, integrationPool, "01HNM", "host-x", 0, StatusNoMatch, nil); err != nil {
		t.Fatalf("UpdateStatus(no_match): %v", err)
	}
	got, err := SelectByApplyID(ctx, integrationPool, "01HNM", "host-x")
	if err != nil {
		t.Fatalf("SelectByApplyID: %v", err)
	}
	if got.Status != StatusNoMatch {
		t.Errorf("status = %q, want no_match", got.Status)
	}
	if got.FinishedAt == nil {
		t.Error("finished_at nil после перехода в no_match; want set (иначе строка не purge-ится)")
	}
}
