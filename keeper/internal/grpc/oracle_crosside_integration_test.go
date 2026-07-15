//go:build integration

// Cross-side integration (qa coverage_gap #1, the main gap in the beacons
// pilot): the SEAM between the Soul producer (PortentEvent) and the Keeper
// consumer (Oracle handler) via the committed proto keeperv1.PortentEvent,
// carried all the way to apply_runs(planned) on a LIVE PG.
//
// Existing tests asserted each link separately: scheduler_test — the
// producer side (scheduler.emit → PortentEvent with
// beacon_name/sid/collected_at/data), events_oracle_test — the consumer
// side against a fake DB + fakeEnqueuer. The seam "Portent actually makes
// it to apply_runs through a real ServiceRef resolve from the
// incarnation" hadn't been asserted end-to-end on live PG. Here, a
// Portent (committed proto, the same shape the Soul scheduler emits)
// goes through the REAL eventStreamHandler.handlePortentEvent + REAL
// oracle CRUD (decrees/souls/oracle_fires) + REAL
// incarnation→ServiceRef resolve + REAL applyrun.InsertPlanned.
//
// SHAPE OF THE SEAM: in-process integration, NOT a full 2-binary e2e.
//   - The producer binary (the soul daemon) can't be imported: the
//     scheduler lives in soul/internal/beacon — an internal package of a
//     DIFFERENT go module (`soul/`), unreachable from the keeper module
//     under Go's internal rules + ADR-011 isolation.
//   - So the Portent is built from the committed proto
//     keeperv1.PortentEvent, in exactly the shape the scheduler's
//     producer tests pin down (beacon_name + data + collected_at + sid),
//     and fed into a real Keeper handler. The seam through the committed
//     contract is asserted here; the full 2-binary e2e (soul daemon +
//     keeper over real gRPC/mTLS) is covered by prod smoke
//     (docs/local-setup.md).

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

// resetOracleCrossSide clears the tables used by the cross-side test:
// the onboarding set (souls/operators) + apply/oracle registries. The
// grpc package's resetAll doesn't touch
// apply_runs/incarnation/decrees/oracle_fires.
func resetOracleCrossSide(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE apply_runs, oracle_fires, decrees, vigils, state_history,
		 incarnation, soul_seeds, bootstrap_tokens, souls, operators, audit_log CASCADE`); err != nil {
		t.Fatalf("TRUNCATE oracle-crosside: %v", err)
	}
}

// liveOracleEnqueuer — a [ScenarioEnqueuer] implementation over live PG, an
// exact copy of cmd/keeper/oracle_enqueuer.go (it can't be imported —
// package main): SelectByName(incarnation) → Resolve(service) with a ref
// override = ServiceVersion → InsertPlanned(Recipe). Asserts the REAL
// consumer seam all the way to apply_runs(planned) (unlike fakeEnqueuer,
// which cuts the seam short).
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
	ref.Ref = inc.ServiceVersion // roll out with the deployed version (mirrors the enqueuer)

	applyID := audit.NewULID()
	if err := applyrun.InsertPlanned(ctx, integrationPool, &applyrun.ApplyRun{
		ApplyID:         applyID,
		SID:             req.SubjectSID,
		IncarnationName: inc.Name,
		Scenario:        req.ScenarioName,
		StartedByAID:    nil, // Soul-initiated reaction, no Archon identity
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

// fixedResolver — a stub [incarnation.ServiceResolver]: one service →
// fixed git coordinates. Resolving a ServiceRef from the service registry
// isn't what this seam is about (it's asserted in the incarnation tests);
// what matters here is that the handler carried
// incarnation.Service → Recipe.ServiceRef with the correct ref override.
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

// newCrossSideHandler — a handler over live PG with real oracle deps
// (DB=integrationPool, where-CEL, live enqueuer, recordingAudit) and
// registered keeper_oracle_* metrics.
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

// soulSchedulerPortent builds a PortentEvent in exactly the shape produced
// by the Soul scheduler (soul/internal/beacon/scheduler.go:emit):
// beacon_name + data(Struct) + collected_at + sid. This is the committed
// Soul→Keeper proto contract; the shape is pinned by the scheduler's
// producer tests (TestEdgeTriggeredOnChange).
func soulSchedulerPortent(t *testing.T, beacon, sid string, data map[string]any) *keeperv1.PortentEvent {
	t.Helper()
	ev := &keeperv1.PortentEvent{
		BeaconName:  beacon,
		CollectedAt: timestamppb.New(time.Now().UTC()),
		Sid:         sid, // echo (authority is the mTLS peer cert, the handler reads it separately)
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

// TestIntegration_OracleCrossSide_PortentToPlannedApplyRun — END-TO-END
// Soul→Keeper:
//
//	The Soul producer emits a PortentEvent (committed proto) → the Keeper
//	  consumer's handlePortentEvent on live PG:
//	    SelectDecreesByBeacon (a real decree) → covens from the
//	    souls registry → SubjectMatches → membership → where-CEL →
//	    cooldown → EnqueueScenario (live: SelectByName(incarnation) →
//	    Resolve → InsertPlanned) → RecordFire → audit oracle.fired
//	→ ASSERT: an apply_runs row(planned) with
//	  Recipe.ServiceRef.Ref == inc.ServiceVersion + Recipe.Input carried
//	  through (vault-ref as-is) + audit oracle.fired recorded on live PG +
//	  the oracle_fires cooldown recorded.
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
			Name: svcName, Git: "file:///srv/services/nginx", Ref: "main", // Ref should get overridden to svcVer
		},
	}
	h, aw := newCrossSideHandler(t, resolver)

	// --- act: the Soul producer emits a Portent → the Keeper consumer handles it. ---
	portent := soulSchedulerPortent(t, "svc-down", sid, map[string]any{"severity": "critical"})
	h.handlePortentEvent(ctx, sid, "session-x", portent)

	// --- assert: an apply_runs row(planned) with the correct Recipe. ---
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
	// ServiceRef is resolved FROM the incarnation: Name comes from the
	// resolver, Ref is overridden to inc.ServiceVersion (not the tip of
	// the `main` branch).
	if recipe.ServiceRef.Name != svcName {
		t.Errorf("Recipe.ServiceRef.Name = %q, want %q", recipe.ServiceRef.Name, svcName)
	}
	if recipe.ServiceRef.Ref != svcVer {
		t.Errorf("Recipe.ServiceRef.Ref = %q, want %q (развёрнутая версия incarnation)", recipe.ServiceRef.Ref, svcVer)
	}
	// Input (the Decree's action_input) is carried through as-is.
	if recipe.Input["unit"] != "nginx" || recipe.Input["graceful"] != true {
		t.Errorf("Recipe.Input не проброшен: %+v", recipe.Input)
	}

	// --- assert: audit oracle.fired recorded on live PG (via recordingAudit). ---
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

	// --- assert: the cooldown is recorded (oracle_fires) under correlation = apply_id. ---
	_, hasFired, err := oracle.LastFiredAt(ctx, integrationPool, "restart-web", sid)
	if err != nil {
		t.Fatalf("LastFiredAt: %v", err)
	}
	if !hasFired {
		t.Error("после успешной реакции oracle_fires-cooldown должен быть записан")
	}

	// The audit's correlation_id = apply_id of the run that was queued.
	var applyID string
	if err := integrationPool.QueryRow(ctx,
		`SELECT apply_id FROM apply_runs WHERE sid = $1`, sid).Scan(&applyID); err != nil {
		t.Fatalf("read apply_id: %v", err)
	}
	if firedCorrelation != applyID {
		t.Errorf("oracle.fired CorrelationID = %q, want apply_id %q", firedCorrelation, applyID)
	}
}

// TestIntegration_OracleCrossSide_IncarnationNotFoundNoApplyRun —
// fail-closed on live PG: a Decree targets an incarnation that's not in
// the registry → the live enqueuer returns an error → the handler does
// NOT write fire/audit and no apply_runs row is created. Completes the
// cross-side seam with its negative case (the consumer correctly
// suppresses the reaction when the target is unreachable).
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
	// The host is in coven `ghost-app` (membership will pass), but the
	// incarnation `ghost-app` is NOT created → SelectByName fails →
	// enqueuer error.
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

	// apply_runs is not created.
	var cnt int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM apply_runs`).Scan(&cnt); err != nil {
		t.Fatalf("count apply_runs: %v", err)
	}
	if cnt != 0 {
		t.Errorf("enqueue-fail (incarnation not found): apply_runs должен быть пуст, got %d", cnt)
	}
	// audit oracle.fired is NOT written (the reaction is suppressed before
	// fire).
	for _, e := range aw.snapshot() {
		if e.EventType == audit.EventOracleFired {
			t.Error("enqueue-fail: audit oracle.fired НЕ должен писаться")
		}
	}
	// The cooldown is NOT recorded (a false cooldown would block future
	// reactions).
	if _, hasFired, _ := oracle.LastFiredAt(ctx, integrationPool, "restart-ghost", sid); hasFired {
		t.Error("enqueue-fail: oracle_fires-cooldown НЕ должен писаться")
	}
}

// --- V5-1 typed PortentPayload integration (ADR-030 amendment 2026-05-26) ---

// seedV5TypedFixture seeds a minimal set (operator + soul + incarnation +
// vigil + decree) for the V5-1 typed-payload tests. where_cel is a
// parameter (typed or legacy access). Returns (sid, decree name).
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

// resolverV5 — a shared fixed resolver for the V5 tests (nginx-svc).
func resolverV5() incarnation.ServiceResolver {
	return fixedResolver{
		service: "nginx-svc",
		ref:     artifact.ServiceRef{Name: "nginx-svc", Git: "file:///srv/nginx", Ref: "main"},
	}
}

// assertApplyPlannedV5 — an apply_runs row for the given SID is created
// and in the planned status.
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

// TestIntegration_OracleV5_TypedPayloadDualWriteFires — Soul-side
// dual-write (data + typed file_changed) + a Decree where-CEL reading the
// typed payload `event.file_changed.path.startsWith("/etc/")` →
// fire+enqueue on live PG.
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

	// audit oracle.fired is recorded.
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

// TestIntegration_OracleV5_LegacyOnlyStillFires — backward compatibility:
// the Soul side sends ONLY legacy data (no typed payload) → the Decree
// `event.data.*` must still match (the handler folds the legacy branch
// into the activation).
func TestIntegration_OracleV5_LegacyOnlyStillFires(t *testing.T) {
	resetOracleCrossSide(t)
	ctx := context.Background()
	sid, _ := seedV5TypedFixture(t, ctx,
		"core.beacon.file_changed",
		`event.data.path == "/etc/passwd"`)

	h, _ := newCrossSideHandler(t, resolverV5())
	// Legacy-only Portent: data only, no typed payload.
	portent := soulSchedulerPortent(t, "watch-v5", sid, map[string]any{
		"path": "/etc/passwd", "sha256": "deadbeef",
	})
	h.handlePortentEvent(ctx, sid, "session-legacy", portent)

	assertApplyPlannedV5(t, ctx, sid)
}

// TestIntegration_OracleV5_TypedOnlyMatches — the Soul side sends ONLY
// typed (no legacy data, simulating S5-final post-hard-cut) → the Decree
// `event.service_down.service == "nginx"` must match.
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
// the Decree expects file_changed, the Soul sends service_down →
// where-CEL's `event.file_changed.path` yields a no-such-key →
// default-deny → apply_runs stays empty.
func TestIntegration_OracleV5_TypeMismatchNoFire(t *testing.T) {
	resetOracleCrossSide(t)
	ctx := context.Background()
	sid, _ := seedV5TypedFixture(t, ctx,
		"core.beacon.service_down", // the Vigil watches service_down
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
