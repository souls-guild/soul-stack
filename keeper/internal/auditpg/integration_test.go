//go:build integration

// Integration-тесты для pgxWriter через testcontainers-go.
//
// Поднимают postgres:16-alpine на эфемерном порту, применяют миграции из
// keeper/migrations/, гоняют write+read round-trip. Один контейнер
// per-package (TestMain) — поднятие postgres-а ~3-5 сек, иначе суммарно
// дороже.
//
// Запуск:
//
//	make test-integration
//	# или
//	cd keeper && go test -tags=integration -race -count=1 ./internal/auditpg/
//
// Требует docker (testcontainers использует docker-sock). Если docker
// недоступен — TestMain делает t.Skip-эквивалент (os.Exit(0) с лог-ом).
package auditpg

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// integrationPool — общий pool на пакет, инициализируется в TestMain.
// Тесты делают TRUNCATE audit_log перед запуском (см. resetAuditLog) — это
// дешевле, чем поднимать контейнер на каждый Test*.
var integrationPool *pgxpool.Pool

// TestMain делегирует setup/teardown в run(), потому что os.Exit
// обходит defer-ы — context, контейнер и pool остались бы висеть.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run поднимает Postgres-контейнер, применяет миграции, отдаёт m.Run().
// Возвращает exit-code; defer-ы внутри функции корректно отрабатывают,
// потому что os.Exit вызывается уже в TestMain поверх возвращённого кода.
//
// SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true делает testcontainers
// обязательным (CI-режим): любой setup-fail → log.Fatalf. Без флага
// (локальный режим) — тесты skip-ятся при недоступном docker.
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
			log.Fatalf("auditpg integration: setup failed (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER set): %v", err)
		}
		log.Printf("auditpg integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		// Отдельный ctx — основной может быть отменён выходом теста.
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("auditpg integration: ConnectionString: %v", err)
		return 1
	}

	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("auditpg integration: migrate.Apply: %v", err)
		return 1
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("auditpg integration: pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	// Seed bootstrap-Archon-а: с миграцией 004 на audit_log.archon_aid
	// навешан FK на operators(aid). Tests ниже пишут события с
	// `archon_aid: archon-alice` — без seed-а INSERT упадёт по FK.
	// `created_by_aid IS NULL` означает «это bootstrap», что
	// соответствует роли archon-alice в RBAC-примерах (rbac.md).
	if _, err := pool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('archon-alice', 'Alice (test bootstrap)', 'jwt')
	`); err != nil {
		log.Printf("auditpg integration: seed archon-alice: %v", err)
		return 1
	}

	return m.Run()
}

// resetAuditLog — TRUNCATE между тестами, чтобы один тест не видел записи
// другого. Дешевле re-create-контейнера.
func resetAuditLog(t *testing.T) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(), `TRUNCATE TABLE audit_log`)
	if err != nil {
		t.Fatalf("TRUNCATE audit_log: %v", err)
	}
}

func TestIntegration_PGXWriter_RoundTrip(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	auditID := audit.NewULID()
	corrID := audit.NewULID()
	ts := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	ev := &audit.Event{
		AuditID:       auditID,
		EventType:     audit.EventConfigReloadSucceeded,
		Source:        audit.SourceAPI,
		ArchonAID:     "archon-alice",
		CorrelationID: corrID,
		Payload:       map[string]any{"path": "/etc/keeper.yml", "rev": 42},
		CreatedAt:     ts,
	}
	if err := w.Write(ctx, ev); err != nil {
		t.Fatalf("Write: %v", err)
	}

	row := integrationPool.QueryRow(ctx, `
		SELECT audit_id, event_type, source, archon_aid, correlation_id, payload, created_at
		FROM audit_log WHERE audit_id = $1
	`, auditID)
	var (
		gotID        string
		gotType      string
		gotSource    string
		gotArchon    *string
		gotCorr      *string
		payloadBytes []byte
		gotCreated   time.Time
	)
	if err := row.Scan(&gotID, &gotType, &gotSource, &gotArchon, &gotCorr, &payloadBytes, &gotCreated); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	if gotID != auditID {
		t.Errorf("audit_id roundtrip: got %q, want %q", gotID, auditID)
	}
	if gotType != "config.reload_succeeded" {
		t.Errorf("event_type = %q", gotType)
	}
	if gotSource != "api" {
		t.Errorf("source = %q", gotSource)
	}
	if gotArchon == nil || *gotArchon != "archon-alice" {
		t.Errorf("archon_aid = %v", gotArchon)
	}
	if gotCorr == nil || *gotCorr != corrID {
		t.Errorf("correlation_id = %v", gotCorr)
	}
	if !gotCreated.Equal(ts) {
		t.Errorf("created_at = %v, want %v", gotCreated, ts)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, payloadBytes)
	}
	if payload["path"] != "/etc/keeper.yml" {
		t.Errorf("payload.path = %v", payload["path"])
	}
	// JSON-numbers через json.Unmarshal → float64; явный cast.
	if rev, ok := payload["rev"].(float64); !ok || rev != 42 {
		t.Errorf("payload.rev = %v (%T), want 42", payload["rev"], payload["rev"])
	}
}

func TestIntegration_PGXWriter_MaskSecrets(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	auditID := audit.NewULID()
	ev := &audit.Event{
		AuditID:   auditID,
		EventType: audit.EventConfigReloadSucceeded,
		Source:    audit.SourceSignal,
		Payload: map[string]any{
			"password":  "should-be-masked",
			"vault_ref": "vault:secret/keeper/postgres",
			"path":      "/etc/keeper.yml",
		},
	}
	if err := w.Write(ctx, ev); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var payloadBytes []byte
	err := integrationPool.QueryRow(ctx,
		`SELECT payload FROM audit_log WHERE audit_id = $1`, auditID,
	).Scan(&payloadBytes)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload["password"] != "***MASKED***" {
		t.Errorf("payload.password = %v, want masked", payload["password"])
	}
	if payload["vault_ref"] != "***MASKED***" {
		t.Errorf("payload.vault_ref = %v, want masked", payload["vault_ref"])
	}
	if payload["path"] != "/etc/keeper.yml" {
		t.Errorf("payload.path = %v, want passthrough", payload["path"])
	}
}

func TestIntegration_PGXWriter_NullableFields(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	auditID := audit.NewULID()
	ev := &audit.Event{
		AuditID:   auditID,
		EventType: audit.EventConfigReloadSucceeded,
		Source:    audit.SourceSignal,
		// ArchonAID, CorrelationID — пусты; должны лечь NULL.
	}
	if err := w.Write(ctx, ev); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var (
		gotArchon *string
		gotCorr   *string
	)
	err := integrationPool.QueryRow(ctx,
		`SELECT archon_aid, correlation_id FROM audit_log WHERE audit_id = $1`, auditID,
	).Scan(&gotArchon, &gotCorr)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if gotArchon != nil {
		t.Errorf("archon_aid = %q, want NULL", *gotArchon)
	}
	if gotCorr != nil {
		t.Errorf("correlation_id = %q, want NULL", *gotCorr)
	}
}

// TestIntegration_Reader_DeliveryHistory_Filters — guard read-пути доставок
// уведомлений (ADR-052, переработка разовых уведомлений S2). Фронт-секция
// «Уведомления» на странице прогона и история канала читают терминалы доставки
// (herald.delivered/herald.failed) через GET /v1/audit:
//
//   - voyage-секция: correlation_id=<voyage_id> + type=herald.delivered/failed
//     → события доставки ИМЕННО этого прогона;
//   - история канала: payload_herald=<herald-name> → все доставки канала
//     (фильтр по payload->>'herald', добавлен в S2).
//
// Сидим терминалы двух прогонов в два канала, проверяем оба фильтра в изоляции
// и их пересечение.
func TestIntegration_Reader_DeliveryHistory_Filters(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	reader := NewReader(integrationPool)

	voyageA := audit.NewULID()
	voyageB := audit.NewULID()

	// Терминалы доставки: correlation_id = voyage_id (как пишет worker.emitAudit),
	// payload.herald = имя канала. Перекрёстно: оба канала в обоих прогонах +
	// failed-терминал, плюс «посторонние» события (другой type) — фильтр их режет.
	seed := []struct {
		et       audit.EventType
		voyage   string
		herald   string
		statusOK bool
	}{
		{audit.EventHeraldDelivered, voyageA, "ops-slack", true},
		{audit.EventHeraldFailed, voyageA, "ops-pager", false},
		{audit.EventHeraldDelivered, voyageB, "ops-slack", true},
		{audit.EventHeraldDelivered, voyageB, "ops-pager", true},
	}
	for _, s := range seed {
		payload := map[string]any{
			"herald":     s.herald,
			"tiding":     "t-" + s.herald,
			"event_type": "voyage.finalized",
			"attempt":    0,
		}
		if s.statusOK {
			payload["status_code"] = 200
		}
		ev := &audit.Event{
			EventType:     s.et,
			Source:        audit.SourceKeeperInternal,
			CorrelationID: s.voyage,
			Payload:       payload,
		}
		if err := w.Write(ctx, ev); err != nil {
			t.Fatalf("seed write (%s/%s/%s): %v", s.et, s.voyage, s.herald, err)
		}
	}
	// Посторонний шум — не доставка, не должен попадать ни под один фильтр.
	if err := w.Write(ctx, &audit.Event{
		EventType:     audit.EventConfigReloadSucceeded,
		Source:        audit.SourceAPI,
		CorrelationID: voyageA,
		Payload:       map[string]any{"path": "/etc/keeper.yml"},
	}); err != nil {
		t.Fatalf("seed noise: %v", err)
	}

	deliveryTypes := []string{string(audit.EventHeraldDelivered), string(audit.EventHeraldFailed)}

	// (1) voyage-секция: доставки прогона A — оба терминала A (delivered ops-slack
	// + failed ops-pager), но НЕ доставки B и НЕ config-шум прогона A.
	rowsA, totalA, err := reader.List(ctx, ListFilter{
		Types:         deliveryTypes,
		CorrelationID: voyageA,
	}, 0, 50)
	if err != nil {
		t.Fatalf("List voyageA: %v", err)
	}
	if totalA != 2 {
		t.Errorf("voyageA delivery total = %d, want 2 (delivered+failed этого прогона)", totalA)
	}
	for _, r := range rowsA {
		if r.CorrelationID == nil || *r.CorrelationID != voyageA {
			t.Errorf("voyageA filter leaked correlation_id %v", r.CorrelationID)
		}
		if r.EventType != string(audit.EventHeraldDelivered) && r.EventType != string(audit.EventHeraldFailed) {
			t.Errorf("voyageA filter leaked non-delivery type %q", r.EventType)
		}
	}

	// (2) история канала ops-slack: все доставки канала (прогоны A и B), без ops-pager.
	rowsCh, totalCh, err := reader.List(ctx, ListFilter{
		Types:         deliveryTypes,
		PayloadHerald: "ops-slack",
	}, 0, 50)
	if err != nil {
		t.Fatalf("List herald ops-slack: %v", err)
	}
	if totalCh != 2 {
		t.Errorf("ops-slack history total = %d, want 2 (доставки канала в A и B)", totalCh)
	}
	for _, r := range rowsCh {
		if got, _ := r.Payload["herald"].(string); got != "ops-slack" {
			t.Errorf("ops-slack filter leaked payload.herald = %v", r.Payload["herald"])
		}
	}

	// (3) пересечение voyage_id ∩ herald: одна конкретная доставка в B канала ops-pager.
	rowsX, totalX, err := reader.List(ctx, ListFilter{
		Types:         deliveryTypes,
		CorrelationID: voyageB,
		PayloadHerald: "ops-pager",
	}, 0, 50)
	if err != nil {
		t.Fatalf("List intersect: %v", err)
	}
	if totalX != 1 || len(rowsX) != 1 {
		t.Fatalf("voyageB ∩ ops-pager total = %d, want 1", totalX)
	}
	if rowsX[0].EventType != string(audit.EventHeraldDelivered) {
		t.Errorf("intersect type = %q, want herald.delivered", rowsX[0].EventType)
	}
}

// TestIntegration_Reader_ChangedTaskKeys — guard read-пути свёртки changed-задач
// (T3): SelectChangedTaskKeys читает (sid, plan_index) задач прогона, терминал-
// ивших CHANGED, СТРОГО из `task.executed`-событий со `status == TASK_STATUS_CHANGED`.
// Проверяем:
//   - фильтр по correlation_id (apply_id) + event_type + status (другие прогоны /
//     статусы / типы не попадают);
//   - дедуп пары (sid, plan_index) (retry дал две task.executed-строки одной задачи);
//   - backward-compat: строки БЕЗ plan_index (старый Soul / прогон до T3) читаются
//     с fallback на task_idx (COALESCE в SQL);
//   - секрет-гигиена: register_data/error-значения payload НЕ влияют на ключи
//     (берутся только sid + plan_index).
//
// Этот сид специально кладёт ТОЛЬКО task_idx (без plan_index) — проверяет
// fallback-ветку. Приоритет plan_index над task_idx под staged/per-host (plan_idx
// ≠ task_idx) проверяет TestIntegration_Reader_ChangedTaskKeys_PlanIndexPriority.
func TestIntegration_Reader_ChangedTaskKeys(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	reader := NewReader(integrationPool)

	applyA := audit.NewULID()
	applyB := audit.NewULID()

	// task.executed-события прогона A: смесь статусов; CHANGED на (a,0),(b,0),(a,1);
	// retry: (a,0) записан дважды (дедуп). + OK-задача (не changed) + FAILED.
	// register_data — секрет-shaped payload, не должен утечь в ключи.
	type te struct {
		apply  string
		sid    string
		idx    int
		status string
	}
	seed := []te{
		{applyA, "a.local", 0, "TASK_STATUS_CHANGED"},
		{applyA, "a.local", 0, "TASK_STATUS_CHANGED"}, // retry → дубль, дедуп
		{applyA, "b.local", 0, "TASK_STATUS_CHANGED"},
		{applyA, "a.local", 1, "TASK_STATUS_CHANGED"},
		{applyA, "a.local", 2, "TASK_STATUS_OK"},      // не changed
		{applyA, "b.local", 1, "TASK_STATUS_FAILED"},  // не changed
		{applyB, "z.local", 0, "TASK_STATUS_CHANGED"}, // другой прогон
	}
	for _, s := range seed {
		ev := &audit.Event{
			EventType:     audit.EventTaskExecuted,
			Source:        audit.SourceSoulGRPC,
			CorrelationID: s.apply,
			Payload: map[string]any{
				"sid":      s.sid,
				"apply_id": s.apply,
				// БЕЗ plan_index — backward-compat: чтение fallback-ит на task_idx.
				"task_idx":      s.idx,
				"status":        s.status,
				"register_data": map[string]any{"password": "should-not-leak-into-key"},
			},
		}
		if err := w.Write(ctx, ev); err != nil {
			t.Fatalf("seed write: %v", err)
		}
	}
	// Посторонний шум: run.completed того же apply_id — не task.executed, режется.
	if err := w.Write(ctx, &audit.Event{
		EventType:     audit.EventRunCompleted,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: applyA,
		Payload:       map[string]any{"sid": "a.local", "status": "RUN_STATUS_SUCCESS"},
	}); err != nil {
		t.Fatalf("seed noise: %v", err)
	}

	keys, err := reader.SelectChangedTaskKeys(ctx, applyA)
	if err != nil {
		t.Fatalf("SelectChangedTaskKeys: %v", err)
	}

	// Ожидаем РОВНО {(a,0),(b,0),(a,1)} — дедуп схлопнул дубль (a,0); OK/FAILED
	// и прогон B исключены. plan_index взят fallback-ом из task_idx.
	want := map[ChangedTaskKey]struct{}{
		{SID: "a.local", PlanIndex: 0}: {},
		{SID: "b.local", PlanIndex: 0}: {},
		{SID: "a.local", PlanIndex: 1}: {},
	}
	if len(keys) != len(want) {
		t.Fatalf("got %d keys, want %d: %+v", len(keys), len(want), keys)
	}
	for k := range want {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing expected key %+v", k)
		}
	}
	// Прогон B не должен присутствовать.
	if _, ok := keys[ChangedTaskKey{SID: "z.local", PlanIndex: 0}]; ok {
		t.Error("cross-apply leak: applyB key in applyA result")
	}
}

// TestIntegration_Reader_ChangedTaskKeys_PlanIndexPriority — T3 GUARD (read-путь):
// под staged/per-host-where ГЛОБАЛЬНЫЙ plan_index ≠ ЛОКАЛЬНОМУ task_idx; свёртка
// CHANGED-задач ОБЯЗАНА брать plan_index (ключ корреляции с RenderedTask.Index),
// а НЕ task_idx — иначе ключ указал бы на соседнюю задачу (mismatch в
// state_changes-whitelist + audit changed_tasks).
//
// Сид: одна CHANGED-задача с plan_index=7, task_idx=2 (имитация второго Passage,
// где локальная позиция 2 соответствует глобальному плану 7). Ожидаем ключ
// (sid, 7) — глобальный; РЕВЕРС-инвариант: (sid, 2) (локальный task_idx) в
// результате присутствовать НЕ должен.
func TestIntegration_Reader_ChangedTaskKeys_PlanIndexPriority(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	reader := NewReader(integrationPool)

	applyID := audit.NewULID()

	ev := &audit.Event{
		EventType:     audit.EventTaskExecuted,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: applyID,
		Payload: map[string]any{
			"sid":      "h.local",
			"apply_id": applyID,
			// staged/per-host: локальная позиция 2 в своём Passage ≠ глобальному
			// сквозному индексу 7 по всему плану.
			"task_idx":   2,
			"plan_index": 7,
			"status":     "TASK_STATUS_CHANGED",
		},
	}
	if err := w.Write(ctx, ev); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	keys, err := reader.SelectChangedTaskKeys(ctx, applyID)
	if err != nil {
		t.Fatalf("SelectChangedTaskKeys: %v", err)
	}

	// Ключ — ГЛОБАЛЬНЫЙ plan_index (7), а не локальный task_idx (2).
	if _, ok := keys[ChangedTaskKey{SID: "h.local", PlanIndex: 7}]; !ok {
		t.Errorf("ожидался ключ (h.local, plan_index=7) — свёртка ДОЛЖНА брать глобальный plan_index; keys=%+v", keys)
	}
	// РЕВЕРС: локальный task_idx (2) НЕ должен стать ключом — иначе корреляция с
	// планом указала бы на соседнюю задачу (T3-баг).
	if _, ok := keys[ChangedTaskKey{SID: "h.local", PlanIndex: 2}]; ok {
		t.Errorf("ключ (h.local, 2) присутствует — свёртка взяла ЛОКАЛЬНЫЙ task_idx вместо plan_index (T3-регресс); keys=%+v", keys)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1: %+v", len(keys), keys)
	}
}

// TestIntegration_Reader_PayloadVoyage_Filter — guard read-пути visibility Voyage
// detail (ADR-052 amend §k): per-incarnation события incarnation.run_completed
// несут correlation_id=apply_id (РАЗНЫЙ у каждой инкарнации), а voyage_id лежит в
// payload. Voyage detail собирает run-события вояжа фильтром payload_voyage
// (payload->>'voyage_id'). Проверяем:
//   - фильтр возвращает ВСЕ per-incarnation run-события данного voyage_id
//     (несмотря на разные apply_id/correlation_id);
//   - НЕ возвращает run-события чужого вояжа;
//   - НЕ возвращает события без voyage_id (прямой путь create/rerun/destroy);
//   - параметризация: значение фильтра уходит позиционным плейсхолдером, а не
//     конкатенацией (косвенно — поиск по литералу с кавычками не матчит).
func TestIntegration_Reader_PayloadVoyage_Filter(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	reader := NewReader(integrationPool)

	voyageA := audit.NewULID()
	voyageB := audit.NewULID()

	// run-события вояжа A: ДВЕ инкарнации, у каждой свой apply_id (correlation_id),
	// общий voyage_id в payload. + run-событие вояжа B + событие БЕЗ voyage_id
	// (прямой путь, минующий Voyage) — оба должны быть отрезаны фильтром A.
	seed := []struct {
		voyage      string // "" → без voyage_id в payload (прямой путь)
		incarnation string
		status      string
	}{
		{voyageA, "redis-a", "success"},
		{voyageA, "redis-b", "failed"},
		{voyageB, "redis-c", "success"},
		{"", "redis-direct", "success"}, // create-путь: voyage_id нет
	}
	for _, s := range seed {
		payload := map[string]any{
			"incarnation":   s.incarnation,
			"scenario":      "add_user",
			"apply_id":      audit.NewULID(),
			"status":        s.status,
			"changed_tasks": []map[string]any{},
		}
		if s.voyage != "" {
			payload["voyage_id"] = s.voyage
		}
		ev := &audit.Event{
			EventType:     audit.EventIncarnationRunCompleted,
			Source:        audit.SourceKeeperInternal,
			CorrelationID: payload["apply_id"].(string), // per-incarnation apply_id
			Payload:       payload,
		}
		if err := w.Write(ctx, ev); err != nil {
			t.Fatalf("seed write (%s/%s): %v", s.voyage, s.incarnation, err)
		}
	}

	// Фильтр по вояжу A: РОВНО два per-incarnation события (redis-a + redis-b),
	// несмотря на разные correlation_id; ни B, ни прямой путь.
	rowsA, totalA, err := reader.List(ctx, ListFilter{PayloadVoyage: voyageA}, 0, 50)
	if err != nil {
		t.Fatalf("List voyageA: %v", err)
	}
	if totalA != 2 {
		t.Errorf("voyageA run-events total = %d, want 2 (обе инкарнации вояжа A)", totalA)
	}
	if len(rowsA) != 2 {
		t.Fatalf("voyageA rows = %d, want 2", len(rowsA))
	}
	for _, r := range rowsA {
		if got, _ := r.Payload["voyage_id"].(string); got != voyageA {
			t.Errorf("voyageA filter leaked payload.voyage_id = %v", r.Payload["voyage_id"])
		}
		if r.EventType != string(audit.EventIncarnationRunCompleted) {
			t.Errorf("voyageA filter leaked type %q", r.EventType)
		}
	}

	// Фильтр по вояжу B: ровно одно событие (redis-c), не утекает A / прямой путь.
	_, totalB, err := reader.List(ctx, ListFilter{PayloadVoyage: voyageB}, 0, 50)
	if err != nil {
		t.Fatalf("List voyageB: %v", err)
	}
	if totalB != 1 {
		t.Errorf("voyageB run-events total = %d, want 1", totalB)
	}

	// Несуществующий voyage_id → пусто (события без voyage_id под фильтр не падают).
	_, totalNone, err := reader.List(ctx, ListFilter{PayloadVoyage: "voy-does-not-exist"}, 0, 50)
	if err != nil {
		t.Fatalf("List voyage none: %v", err)
	}
	if totalNone != 0 {
		t.Errorf("unknown voyage total = %d, want 0 (no leak, no SQL-injection)", totalNone)
	}
}

func TestIntegration_PGXWriter_ConcurrentWrites(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	const n = 50
	w := NewWriter(integrationPool)

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := &audit.Event{
				EventType: audit.EventConfigReloadSucceeded,
				Source:    audit.SourceAPI,
				Payload:   map[string]any{"seq": i},
			}
			if err := w.Write(ctx, ev); err != nil {
				errCh <- fmt.Errorf("seq=%d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Write: %v", err)
	}

	var count int
	err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&count)
	if err != nil {
		t.Fatalf("COUNT: %v", err)
	}
	if count != n {
		t.Errorf("audit_log rows = %d, want %d", count, n)
	}
}
