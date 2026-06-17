//go:build integration

// Cross-side integration (qa coverage_gap #1, главный пробел beacons-пилота):
// ШОВ Soul-producer (PortentEvent) ↔ Keeper-consumer (Oracle handler) через
// committed proto keeperv1.PortentEvent, доведённый до apply_runs(planned) на
// ЖИВОМ PG.
//
// Существующие тесты утверждали звенья по отдельности: scheduler_test —
// producer-сторону (scheduler.emit → PortentEvent с beacon_name/sid/collected_at/
// data), events_oracle_test — consumer-сторону на fake-DB + fakeEnqueuer.
// Шов «Portent реально доезжает до apply_runs через реальный резолв ServiceRef из
// incarnation» в одной системе на живом PG не был утверждён. Здесь Portent
// (committed proto, та же форма, что эмитит Soul-scheduler) проходит через
// РЕАЛЬНЫЙ eventStreamHandler.handlePortentEvent + РЕАЛЬНЫЙ oracle-CRUD (decrees/
// souls/oracle_fires) + РЕАЛЬНЫЙ резолв incarnation→ServiceRef + РЕАЛЬНЫЙ
// applyrun.InsertPlanned.
//
// ФОРМА ШВА: in-process integration, а НЕ полный 2-бинарный e2e.
//   - Producer-бинарь (soul-демон) импортировать нельзя: scheduler живёт в
//     soul/internal/beacon — internal-пакет ДРУГОГО go-модуля (`soul/`),
//     недоступен из keeper-модуля по правилам Go internal + изоляции ADR-011.
//   - Поэтому Portent строится из committed proto keeperv1.PortentEvent ровно в
//     той форме, что фиксируют producer-тесты scheduler-а (beacon_name + data +
//     collected_at + sid), и подаётся в реальный Keeper-handler. Шов через
//     committed contract утверждён; полный 2-бинарный e2e (soul-демон +
//     keeper по реальному gRPC/mTLS) покрыт прод-smoke (docs/local-setup.md).

package grpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/obs"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// resetOracleCrossSide чистит таблицы, задействованные cross-side-тестом:
// onboarding-набор (souls/operators) + apply/oracle-реестры. grpc-пакетный
// resetAll не трогает apply_runs/incarnation/decrees/oracle_fires.
func resetOracleCrossSide(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE apply_runs, oracle_fires, decrees, vigils, state_history,
		 incarnation, soul_seeds, bootstrap_tokens, souls, operators, audit_log CASCADE`); err != nil {
		t.Fatalf("TRUNCATE oracle-crosside: %v", err)
	}
}

// liveOracleEnqueuer — реализация [ScenarioEnqueuer] поверх живого PG, точная
// калька cmd/keeper/oracle_enqueuer.go (его нельзя импортировать — package main):
// SelectByName(incarnation) → Resolve(service) с ref override = ServiceVersion →
// InsertPlanned(Recipe). Утверждает РЕАЛЬНЫЙ consumer-шов до apply_runs(planned)
// (а не fakeEnqueuer, который шов обрывает).
type liveOracleEnqueuer struct {
	resolver incarnation.ServiceResolver
}

func (e *liveOracleEnqueuer) EnqueueScenario(ctx context.Context, req EnqueueScenarioRequest) (string, error) {
	inc, err := incarnation.SelectByName(ctx, integrationPool, req.IncarnationName)
	if err != nil {
		return "", err
	}
	ref, ok := e.resolver.Resolve(inc.Service)
	if !ok {
		return "", fmt.Errorf("liveOracleEnqueuer: resolver не знает сервис %q", inc.Service)
	}
	ref.Ref = inc.ServiceVersion // катим развёрнутой версией (калька enqueuer-а)

	applyID := audit.NewULID()
	if err := applyrun.InsertPlanned(ctx, integrationPool, &applyrun.ApplyRun{
		ApplyID:         applyID,
		SID:             req.SubjectSID,
		IncarnationName: inc.Name,
		Scenario:        req.ScenarioName,
		StartedByAID:    nil, // Soul-инициированная реакция без identity Архонта
		Recipe: &applyrun.Recipe{
			ServiceRef:   ref,
			ScenarioName: req.ScenarioName,
			Input:        req.ActionInput,
			StartedByAID: nil,
		},
	}); err != nil {
		return "", err
	}
	return applyID, nil
}

// fixedResolver — стаб [incarnation.ServiceResolver]: один сервис → фиксированные
// git-координаты. Резолв ServiceRef из реестра сервисов — не предмет этого шва
// (он утверждён в incarnation-тестах); здесь важно, что handler довёл
// incarnation.Service → Recipe.ServiceRef с правильным ref override.
type fixedResolver struct {
	service string
	ref     artifact.ServiceRef
}

func (r fixedResolver) Resolve(service string) (artifact.ServiceRef, bool) {
	if service != r.service {
		return artifact.ServiceRef{}, false
	}
	return r.ref, true
}

// newCrossSideHandler — handler поверх живого PG с реальным oracle-deps
// (DB=integrationPool, where-CEL, live enqueuer, recordingAudit) и
// зарегистрированными keeper_oracle_*-метриками.
func newCrossSideHandler(t *testing.T, resolver incarnation.ServiceResolver) (*eventStreamHandler, *recordingAudit) {
	t.Helper()
	where, err := oracle.NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}
	aw := &recordingAudit{}
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		Oracle: &OracleDeps{
			DB:          integrationPool,
			Where:       where,
			Enqueuer:    &liveOracleEnqueuer{resolver: resolver},
			AuditWriter: aw,
			Metrics:     oracle.RegisterOracleMetrics(obs.NewRegistry()),
		},
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardIntegrationLogger()), aw
}

// soulSchedulerPortent строит PortentEvent ровно в форме producer-а Soul-scheduler-а
// (soul/internal/beacon/scheduler.go:emit): beacon_name + data(Struct) +
// collected_at + sid. Это committed proto-контракт Soul→Keeper; форма
// зафиксирована producer-тестами scheduler-а (TestEdgeTriggeredOnChange).
func soulSchedulerPortent(t *testing.T, beacon, sid string, data map[string]any) *keeperv1.PortentEvent {
	t.Helper()
	ev := &keeperv1.PortentEvent{
		BeaconName:  beacon,
		CollectedAt: timestamppb.New(time.Now().UTC()),
		Sid:         sid, // echo (авторитет — mTLS peer cert, handler берёт его отдельно)
	}
	if data != nil {
		s, err := structpb.NewStruct(data)
		if err != nil {
			t.Fatalf("structpb.NewStruct: %v", err)
		}
		ev.Data = s
	}
	return ev
}

// TestIntegration_OracleCrossSide_PortentToPlannedApplyRun — СКВОЗНОЙ Soul→Keeper:
//
//	Soul producer эмитит PortentEvent (committed proto) → Keeper consumer
//	  handlePortentEvent на живом PG:
//	    SelectDecreesByBeacon (реальный decree) → covens из souls-registry →
//	    SubjectMatches → membership → where-CEL → cooldown → EnqueueScenario
//	    (live: SelectByName(incarnation) → Resolve → InsertPlanned) →
//	    RecordFire → audit oracle.fired
//	→ ASSERT: apply_runs row(planned) с Recipe.ServiceRef.Ref == inc.ServiceVersion
//	  + Recipe.Input проброшен (vault-ref как есть) + audit oracle.fired записан
//	  на живом PG + oracle_fires-cooldown зафиксирован.
func TestIntegration_OracleCrossSide_PortentToPlannedApplyRun(t *testing.T) {
	resetOracleCrossSide(t)
	ctx := context.Background()
	const (
		aid     = "archon-alice"
		sid     = "host-a.example.com"
		incName = "web-app"
		svcName = "nginx-svc"
		svcVer  = "v2.1.0"
	)

	// --- seed: operator + souls(connected, covens) + incarnation + decree. ---
	if err := operator.Insert(ctx, integrationPool, &operator.Operator{
		AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT,
	}); err != nil {
		t.Fatalf("operator.Insert: %v", err)
	}
	creator := aid
	if err := soul.Insert(ctx, integrationPool, &soul.Soul{
		SID: sid, Transport: soul.TransportAgent, Status: soul.StatusConnected,
		Coven: []string{incName, "web"}, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("soul.Insert: %v", err)
	}
	if err := incarnation.Create(ctx, integrationPool, &incarnation.Incarnation{
		Name: incName, Service: svcName, ServiceVersion: svcVer,
		StateSchemaVersion: 1, State: map[string]any{"replicas": float64(3)},
		Status: incarnation.StatusReady, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("incarnation.Create: %v", err)
	}
	if err := oracle.InsertVigil(ctx, integrationPool, &oracle.Vigil{
		Name: "svc-down", Coven: []string{"web"}, IntervalSpec: "30s",
		CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertVigil: %v", err)
	}
	if err := oracle.InsertDecree(ctx, integrationPool, &oracle.Decree{
		Name: "restart-web", OnBeacon: "svc-down",
		SubjectCoven: []string{"web"}, IncarnationName: incName,
		ActionScenario: "restart_service", ActionInput: []byte(`{"unit":"nginx","graceful":true}`),
		Cooldown: "5m", Enabled: true, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertDecree: %v", err)
	}

	resolver := fixedResolver{
		service: svcName,
		ref: artifact.ServiceRef{
			Name: svcName, Git: "file:///srv/services/nginx", Ref: "main", // Ref должен переопределиться на svcVer
		},
	}
	h, aw := newCrossSideHandler(t, resolver)

	// --- act: Soul-producer эмитит Portent → Keeper-consumer обрабатывает. ---
	portent := soulSchedulerPortent(t, "svc-down", sid, map[string]any{"severity": "critical"})
	h.handlePortentEvent(ctx, sid, "session-x", portent)

	// --- assert: apply_runs row(planned) с правильным Recipe. ---
	var (
		status     string
		recipeJSON []byte
		gotInc     string
		gotScen    string
	)
	if err := integrationPool.QueryRow(ctx,
		`SELECT status, recipe, incarnation_name, scenario FROM apply_runs WHERE sid = $1`, sid).
		Scan(&status, &recipeJSON, &gotInc, &gotScen); err != nil {
		t.Fatalf("apply_runs не создан или не читается: %v", err)
	}
	if status != string(applyrun.StatusPlanned) {
		t.Errorf("apply_runs.status = %q, want planned", status)
	}
	if gotInc != incName {
		t.Errorf("apply_runs.incarnation_name = %q, want %q", gotInc, incName)
	}
	if gotScen != "restart_service" {
		t.Errorf("apply_runs.scenario = %q, want restart_service", gotScen)
	}

	recipe, err := applyrun.UnmarshalRecipe(recipeJSON)
	if err != nil {
		t.Fatalf("UnmarshalRecipe: %v", err)
	}
	if recipe == nil {
		t.Fatal("planned-задание без recipe")
	}
	// ServiceRef резолвится ИЗ incarnation: Name из resolver-а, Ref переопределён
	// на inc.ServiceVersion (а не tip-ом ветки `main`).
	if recipe.ServiceRef.Name != svcName {
		t.Errorf("Recipe.ServiceRef.Name = %q, want %q", recipe.ServiceRef.Name, svcName)
	}
	if recipe.ServiceRef.Ref != svcVer {
		t.Errorf("Recipe.ServiceRef.Ref = %q, want %q (развёрнутая версия incarnation)", recipe.ServiceRef.Ref, svcVer)
	}
	// Input (action_input Decree-а) проброшен как есть.
	if recipe.Input["unit"] != "nginx" || recipe.Input["graceful"] != true {
		t.Errorf("Recipe.Input не проброшен: %+v", recipe.Input)
	}

	// --- assert: audit oracle.fired записан на живом PG (через recordingAudit). ---
	var firedPayloadInc bool
	var firedCorrelation string
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			firedPayloadInc = true
			firedCorrelation = e.CorrelationID
			if e.Payload["decree"] != "restart-web" {
				t.Errorf("oracle.fired payload.decree = %v", e.Payload["decree"])
			}
			if e.Payload["sid"] != sid {
				t.Errorf("oracle.fired payload.sid = %v", e.Payload["sid"])
			}
		}
	}
	if !firedPayloadInc {
		t.Error("ожидали audit oracle.fired")
	}

	// --- assert: cooldown зафиксирован (oracle_fires) под correlation = apply_id. ---
	_, hasFired, err := oracle.LastFiredAt(ctx, integrationPool, "restart-web", sid)
	if err != nil {
		t.Fatalf("LastFiredAt: %v", err)
	}
	if !hasFired {
		t.Error("после успешной реакции oracle_fires-cooldown должен быть записан")
	}

	// correlation_id audit-а = apply_id поставленного прогона.
	var applyID string
	if err := integrationPool.QueryRow(ctx,
		`SELECT apply_id FROM apply_runs WHERE sid = $1`, sid).Scan(&applyID); err != nil {
		t.Fatalf("read apply_id: %v", err)
	}
	if firedCorrelation != applyID {
		t.Errorf("oracle.fired CorrelationID = %q, want apply_id %q", firedCorrelation, applyID)
	}
}

// TestIntegration_OracleCrossSide_IncarnationNotFoundNoApplyRun — fail-closed на
// живом PG: Decree таргетит incarnation, которой нет в реестре → live enqueuer
// возвращает ошибку → handler НЕ пишет fire/audit и apply_runs не создаётся.
// Дополняет cross-side-шов отрицательным концом (consumer корректно гасит
// реакцию при недоставимом таргете).
func TestIntegration_OracleCrossSide_IncarnationNotFoundNoApplyRun(t *testing.T) {
	resetOracleCrossSide(t)
	ctx := context.Background()
	const (
		aid = "archon-alice"
		sid = "host-b.example.com"
	)
	if err := operator.Insert(ctx, integrationPool, &operator.Operator{
		AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT,
	}); err != nil {
		t.Fatalf("operator.Insert: %v", err)
	}
	creator := aid
	// Хост В coven `ghost-app` (membership пройдёт), но incarnation `ghost-app`
	// НЕ создана → SelectByName fail → enqueuer error.
	if err := soul.Insert(ctx, integrationPool, &soul.Soul{
		SID: sid, Transport: soul.TransportAgent, Status: soul.StatusConnected,
		Coven: []string{"ghost-app"}, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("soul.Insert: %v", err)
	}
	if err := oracle.InsertVigil(ctx, integrationPool, &oracle.Vigil{
		Name: "svc-down", Coven: []string{"ghost-app"}, IntervalSpec: "30s",
		CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertVigil: %v", err)
	}
	if err := oracle.InsertDecree(ctx, integrationPool, &oracle.Decree{
		Name: "restart-ghost", OnBeacon: "svc-down",
		SubjectCoven: []string{"ghost-app"}, IncarnationName: "ghost-app",
		ActionScenario: "restart_service", Cooldown: "5m", Enabled: true, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertDecree: %v", err)
	}

	resolver := fixedResolver{service: "any", ref: artifact.ServiceRef{Name: "any"}}
	h, aw := newCrossSideHandler(t, resolver)

	h.handlePortentEvent(ctx, sid, "session-y",
		soulSchedulerPortent(t, "svc-down", sid, nil))

	// apply_runs не создан.
	var cnt int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM apply_runs`).Scan(&cnt); err != nil {
		t.Fatalf("count apply_runs: %v", err)
	}
	if cnt != 0 {
		t.Errorf("enqueue-fail (incarnation not found): apply_runs должен быть пуст, got %d", cnt)
	}
	// audit oracle.fired НЕ пишется (реакция погашена до fire).
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			t.Error("enqueue-fail: audit oracle.fired НЕ должен писаться")
		}
	}
	// cooldown НЕ зафиксирован (ложный cooldown заблокировал бы будущие реакции).
	if _, hasFired, _ := oracle.LastFiredAt(ctx, integrationPool, "restart-ghost", sid); hasFired {
		t.Error("enqueue-fail: oracle_fires-cooldown НЕ должен писаться")
	}
}

// --- V5-1 typed PortentPayload integration (ADR-030 amendment 2026-05-26) ---

// seedV5TypedFixture сеет минимальный набор (operator + soul + incarnation +
// vigil + decree) под V5-1 typed-payload-тесты. where_cel — параметр (typed
// или legacy access). Возвращает (sid, decree-name).
func seedV5TypedFixture(t *testing.T, ctx context.Context, beaconCheck, whereCEL string) (string, string) {
	t.Helper()
	const (
		aid     = "archon-alice"
		sid     = "host-v5.example.com"
		incName = "web-app"
		svcName = "nginx-svc"
		svcVer  = "v2.1.0"
	)
	if err := operator.Insert(ctx, integrationPool, &operator.Operator{
		AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT,
	}); err != nil {
		t.Fatalf("operator.Insert: %v", err)
	}
	creator := aid
	if err := soul.Insert(ctx, integrationPool, &soul.Soul{
		SID: sid, Transport: soul.TransportAgent, Status: soul.StatusConnected,
		Coven: []string{incName, "web"}, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("soul.Insert: %v", err)
	}
	if err := incarnation.Create(ctx, integrationPool, &incarnation.Incarnation{
		Name: incName, Service: svcName, ServiceVersion: svcVer,
		StateSchemaVersion: 1, State: map[string]any{},
		Status: incarnation.StatusReady, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("incarnation.Create: %v", err)
	}
	if err := oracle.InsertVigil(ctx, integrationPool, &oracle.Vigil{
		Name: "watch-v5", Coven: []string{"web"}, IntervalSpec: "30s",
		CheckAddr: beaconCheck, Enabled: true, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertVigil: %v", err)
	}
	whereRef := whereCEL
	if err := oracle.InsertDecree(ctx, integrationPool, &oracle.Decree{
		Name: "react-v5", OnBeacon: "watch-v5", WhereCEL: &whereRef,
		SubjectCoven: []string{"web"}, IncarnationName: incName,
		ActionScenario: "restart_service",
		Cooldown:       "5m", Enabled: true, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertDecree: %v", err)
	}
	return sid, "react-v5"
}

// resolverV5 — общий фиксированный resolver для V5-тестов (nginx-svc).
func resolverV5() incarnation.ServiceResolver {
	return fixedResolver{
		service: "nginx-svc",
		ref:     artifact.ServiceRef{Name: "nginx-svc", Git: "file:///srv/nginx", Ref: "main"},
	}
}

// assertApplyPlannedV5 — apply_runs с указанным SID создан и в статусе planned.
func assertApplyPlannedV5(t *testing.T, ctx context.Context, sid string) {
	t.Helper()
	var cnt int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM apply_runs WHERE sid = $1 AND status = 'planned'`, sid).Scan(&cnt); err != nil {
		t.Fatalf("count apply_runs: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("ожидали 1 planned apply_run для sid=%s, got %d", sid, cnt)
	}
}

func assertApplyNotPlannedV5(t *testing.T, ctx context.Context, sid string) {
	t.Helper()
	var cnt int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM apply_runs WHERE sid = $1`, sid).Scan(&cnt); err != nil {
		t.Fatalf("count apply_runs: %v", err)
	}
	if cnt != 0 {
		t.Errorf("apply_runs не должен быть создан, got %d", cnt)
	}
}

// TestIntegration_OracleV5_TypedPayloadDualWriteFires — Soul-side dual-write
// (data + typed file_changed) + Decree where-CEL читает typed payload
// `event.file_changed.path.startsWith("/etc/")` → fire+enqueue на живом PG.
func TestIntegration_OracleV5_TypedPayloadDualWriteFires(t *testing.T) {
	resetOracleCrossSide(t)
	ctx := context.Background()
	sid, decree := seedV5TypedFixture(t, ctx,
		"core.beacon.file_changed",
		`event.file_changed.path.startsWith("/etc/")`)

	h, aw := newCrossSideHandler(t, resolverV5())

	// dual-write Portent: legacy data + typed file_changed (Soul-side hand-off).
	dataStruct, err := structpb.NewStruct(map[string]any{
		"path": "/etc/nginx.conf", "sha256": "abc123",
	})
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	portent := &keeperv1.PortentEvent{
		BeaconName:  "watch-v5",
		CollectedAt: timestamppb.New(time.Now().UTC()),
		Sid:         sid,
		Data:        dataStruct,
		Payload: &keeperv1.PortentEvent_FileChanged{FileChanged: &keeperv1.FileChangedPortent{
			Path: "/etc/nginx.conf", Sha256: "abc123",
		}},
	}
	h.handlePortentEvent(ctx, sid, "session-typed", portent)

	assertApplyPlannedV5(t, ctx, sid)

	// audit oracle.fired записан.
	var fired bool
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired && e.Payload["decree"] == decree {
			fired = true
		}
	}
	if !fired {
		t.Error("ожидали audit oracle.fired для typed-payload Decree")
	}
}

// TestIntegration_OracleV5_LegacyOnlyStillFires — обратная совместимость:
// Soul-side шлёт ТОЛЬКО legacy data (без typed payload) → Decree `event.data.*`
// должен матчить (handler собирает legacy-ветку в активацию).
func TestIntegration_OracleV5_LegacyOnlyStillFires(t *testing.T) {
	resetOracleCrossSide(t)
	ctx := context.Background()
	sid, _ := seedV5TypedFixture(t, ctx,
		"core.beacon.file_changed",
		`event.data.path == "/etc/passwd"`)

	h, _ := newCrossSideHandler(t, resolverV5())
	// Legacy-only Portent: только data, без typed payload.
	portent := soulSchedulerPortent(t, "watch-v5", sid, map[string]any{
		"path": "/etc/passwd", "sha256": "deadbeef",
	})
	h.handlePortentEvent(ctx, sid, "session-legacy", portent)

	assertApplyPlannedV5(t, ctx, sid)
}

// TestIntegration_OracleV5_TypedOnlyMatches — Soul-side шлёт ТОЛЬКО typed
// (без legacy data, симуляция S5-final post-hard-cut) → Decree
// `event.service_down.service == "nginx"` должен матчить.
func TestIntegration_OracleV5_TypedOnlyMatches(t *testing.T) {
	resetOracleCrossSide(t)
	ctx := context.Background()
	sid, _ := seedV5TypedFixture(t, ctx,
		"core.beacon.service_down",
		`event.service_down.service == "nginx" && !event.service_down.active`)

	h, _ := newCrossSideHandler(t, resolverV5())

	portent := &keeperv1.PortentEvent{
		BeaconName:  "watch-v5",
		CollectedAt: timestamppb.New(time.Now().UTC()),
		Sid:         sid,
		Payload: &keeperv1.PortentEvent_ServiceDown{ServiceDown: &keeperv1.ServiceDownPortent{
			Service: "nginx", Active: false, InitSystem: "systemd",
		}},
	}
	h.handlePortentEvent(ctx, sid, "session-typed-only", portent)

	assertApplyPlannedV5(t, ctx, sid)
}

// TestIntegration_OracleV5_TypeMismatchNoFire — type-mismatch fail-safe:
// Decree ожидает file_changed, Soul шлёт service_down → where-CEL
// `event.file_changed.path` даёт no-such-key → default-deny → apply_runs пуст.
func TestIntegration_OracleV5_TypeMismatchNoFire(t *testing.T) {
	resetOracleCrossSide(t)
	ctx := context.Background()
	sid, _ := seedV5TypedFixture(t, ctx,
		"core.beacon.service_down", // Vigil наблюдает service_down
		`event.file_changed.path.startsWith("/etc/")`)

	h, _ := newCrossSideHandler(t, resolverV5())
	portent := &keeperv1.PortentEvent{
		BeaconName:  "watch-v5",
		CollectedAt: timestamppb.New(time.Now().UTC()),
		Sid:         sid,
		Payload: &keeperv1.PortentEvent_ServiceDown{ServiceDown: &keeperv1.ServiceDownPortent{
			Service: "nginx",
		}},
	}
	h.handlePortentEvent(ctx, sid, "session-mismatch", portent)

	assertApplyNotPlannedV5(t, ctx, sid)
}
