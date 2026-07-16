//go:build integration

// Integration tests for Herald/Tiding CRUD (ADR-052, S1) through testcontainers-go.
// The pattern matches keeper/internal/augur/integration_test.go.

package herald

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}

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
			log.Fatalf("herald integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("herald integration: skipping, docker unavailable: %v", err)
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
	// CASCADE: tidings -> heralds -> operators (FK chain).
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE tidings, heralds, operators, audit_log CASCADE`)
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

func newWebhookHerald(name, aid string) *Herald {
	return &Herald{
		Name:         name,
		Type:         HeraldWebhook,
		Config:       map[string]any{"url": "https://hooks.example.com/" + name},
		SecretRef:    strptr("vault:secret/keeper/herald/" + name),
		Enabled:      true,
		CreatedByAID: &aid,
	}
}

// --- Herald CRUD round-trip -------------------------------------------

func TestIntegration_Herald_InsertSelectUpdateDelete(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	h := newWebhookHerald("ops-webhook", "archon-alice")
	if err := InsertHerald(ctx, integrationPool, h); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}
	if h.CreatedAt.IsZero() || h.UpdatedAt.IsZero() {
		t.Error("CreatedAt/UpdatedAt zero — RETURNING did not fill")
	}

	got, err := SelectHeraldByName(ctx, integrationPool, "ops-webhook")
	if err != nil {
		t.Fatalf("SelectHeraldByName: %v", err)
	}
	if got.Type != HeraldWebhook || got.Config["url"] != "https://hooks.example.com/ops-webhook" {
		t.Errorf("got = %+v", got)
	}
	if got.SecretRef == nil || *got.SecretRef != "vault:secret/keeper/herald/ops-webhook" {
		t.Errorf("secret_ref round-trip = %v", got.SecretRef)
	}

	// Update: disable and change config.
	got.Enabled = false
	got.Config = map[string]any{"url": "https://new.example.com/x"}
	got.SecretRef = nil
	if err := UpdateHerald(ctx, integrationPool, got); err != nil {
		t.Fatalf("UpdateHerald: %v", err)
	}
	after, err := SelectHeraldByName(ctx, integrationPool, "ops-webhook")
	if err != nil {
		t.Fatalf("SelectHeraldByName after update: %v", err)
	}
	if after.Enabled || after.Config["url"] != "https://new.example.com/x" || after.SecretRef != nil {
		t.Errorf("update not applied: %+v", after)
	}

	if err := DeleteHerald(ctx, integrationPool, "ops-webhook"); err != nil {
		t.Fatalf("DeleteHerald: %v", err)
	}
	if _, err := SelectHeraldByName(ctx, integrationPool, "ops-webhook"); !errors.Is(err, ErrHeraldNotFound) {
		t.Fatalf("after delete err = %v, want ErrHeraldNotFound", err)
	}
}

func TestIntegration_Herald_NullableSecretRef(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	h := newWebhookHerald("no-secret", "archon-alice")
	h.SecretRef = nil // Webhook without a signature is allowed.
	if err := InsertHerald(ctx, integrationPool, h); err != nil {
		t.Fatalf("InsertHerald with nil secret_ref: %v", err)
	}
	got, err := SelectHeraldByName(ctx, integrationPool, "no-secret")
	if err != nil {
		t.Fatalf("SelectHeraldByName: %v", err)
	}
	if got.SecretRef != nil {
		t.Errorf("secret_ref = %v, want nil", got.SecretRef)
	}
}

func TestIntegration_Herald_DuplicateName(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	h := newWebhookHerald("dup", "archon-alice")
	if err := InsertHerald(ctx, integrationPool, h); err != nil {
		t.Fatalf("first InsertHerald: %v", err)
	}
	h2 := newWebhookHerald("dup", "archon-alice")
	if err := InsertHerald(ctx, integrationPool, h2); !errors.Is(err, ErrHeraldExists) {
		t.Fatalf("duplicate err = %v, want ErrHeraldExists", err)
	}
}

func TestIntegration_Herald_TypeCHECK(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	// Bypass ValidHeraldType service validation to reach the DB CHECK
	// heralds_type_enum (defence in depth: the DB rejects foreign types).
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO heralds (name, type, config) VALUES ($1, $2, '{}'::jsonb)`,
		"bad-type", "pagerduty")
	if err == nil {
		t.Fatal("expected CHECK violation on heralds_type_enum")
	}
}

func TestIntegration_Herald_NullCreatedByOnOperatorDelete(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-temp")
	ctx := context.Background()

	h := newWebhookHerald("owned", "archon-temp")
	if err := InsertHerald(ctx, integrationPool, h); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `DELETE FROM operators WHERE aid = $1`, "archon-temp"); err != nil {
		t.Fatalf("delete operator: %v", err)
	}
	got, err := SelectHeraldByName(ctx, integrationPool, "owned")
	if err != nil {
		t.Fatalf("SelectHeraldByName: %v", err)
	}
	if got.CreatedByAID != nil {
		t.Errorf("created_by_aid = %v, want NULL after operator delete (ON DELETE SET NULL)", got.CreatedByAID)
	}
}

// --- Tiding CRUD + FK CASCADE -----------------------------------------

func TestIntegration_Tiding_RoundTrip(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ch", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}

	inc := "web-prod"
	aid := "archon-alice"
	tg := &Tiding{
		Name:         "nightly-fail",
		Herald:       "ch",
		EventTypes:   []string{"scenario_run.*", "incarnation.drift_checked"},
		OnlyFailures: true,
		Incarnation:  &inc,
		Enabled:      true,
		CreatedByAID: &aid,
	}
	if err := InsertTiding(ctx, integrationPool, tg); err != nil {
		t.Fatalf("InsertTiding: %v", err)
	}
	if tg.CreatedAt.IsZero() {
		t.Error("CreatedAt zero — RETURNING did not fill")
	}

	got, err := SelectTidingByName(ctx, integrationPool, "nightly-fail")
	if err != nil {
		t.Fatalf("SelectTidingByName: %v", err)
	}
	if len(got.EventTypes) != 2 || !got.OnlyFailures || got.Incarnation == nil || *got.Incarnation != "web-prod" {
		t.Errorf("got = %+v", got)
	}
	if got.Cadence != nil {
		t.Errorf("cadence = %v, want nil", got.Cadence)
	}

	byHerald, err := SelectTidingsByHerald(ctx, integrationPool, "ch")
	if err != nil {
		t.Fatalf("SelectTidingsByHerald: %v", err)
	}
	if len(byHerald) != 1 {
		t.Errorf("SelectTidingsByHerald len = %d, want 1", len(byHerald))
	}
}

func TestIntegration_Tiding_HeraldFKMissing(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	tg := &Tiding{Name: "orphan", Herald: "ghost", EventTypes: []string{"scenario_run.*"}}
	if err := InsertTiding(ctx, integrationPool, tg); !errors.Is(err, ErrHeraldNotFound) {
		t.Fatalf("err = %v, want ErrHeraldNotFound", err)
	}
}

func TestIntegration_Tiding_HeraldCascadeDelete(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ch", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}
	tg := &Tiding{Name: "sub", Herald: "ch", EventTypes: []string{"command_run.*"}, Enabled: true}
	if err := InsertTiding(ctx, integrationPool, tg); err != nil {
		t.Fatalf("InsertTiding: %v", err)
	}

	// Deleting a Herald cascades to its Tiding (ADR-052(a) ON DELETE CASCADE).
	if err := DeleteHerald(ctx, integrationPool, "ch"); err != nil {
		t.Fatalf("DeleteHerald: %v", err)
	}
	if _, err := SelectTidingByName(ctx, integrationPool, "sub"); !errors.Is(err, ErrTidingNotFound) {
		t.Fatalf("after cascade err = %v, want ErrTidingNotFound", err)
	}
}

func TestIntegration_Tiding_EmptyEventTypesCHECK(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ch", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}
	// Bypass ValidateEventTypes service validation to reach the DB CHECK
	// tidings_event_types_nonempty (defence in depth).
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO tidings (name, herald, event_types) VALUES ($1, $2, $3)`,
		"empty-et", "ch", []string{})
	if err == nil {
		t.Fatal("expected CHECK violation on tidings_event_types_nonempty")
	}
}

func TestIntegration_Tiding_DuplicateName(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ch", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}
	tg := &Tiding{Name: "dup", Herald: "ch", EventTypes: []string{"voyage.*"}, Enabled: true}
	if err := InsertTiding(ctx, integrationPool, tg); err != nil {
		t.Fatalf("first InsertTiding: %v", err)
	}
	tg2 := &Tiding{Name: "dup", Herald: "ch", EventTypes: []string{"voyage.*"}, Enabled: true}
	if err := InsertTiding(ctx, integrationPool, tg2); !errors.Is(err, ErrTidingExists) {
		t.Fatalf("duplicate err = %v, want ErrTidingExists", err)
	}
}

// --- N1: ephemeral / voyage_id / annotations / projection round-trip --

// TestIntegration_Tiding_EphemeralPayloadRoundTrip verifies that the new N1
// fields round-trip through insert -> select -> update without loss (ADR-052(g)/(h)):
// ephemeral+voyage_id, annotations (JSONB object), and projection (TEXT[]).
func TestIntegration_Tiding_EphemeralPayloadRoundTrip(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ch", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}

	voyageID := "vy_run_42"
	tg := &Tiding{
		Name:        "ephemeral-run",
		Herald:      "ch",
		EventTypes:  []string{"scenario_run.*"},
		Ephemeral:   true,
		VoyageID:    &voyageID,
		Annotations: map[string]any{"team": "ops", "severity": "high"},
		Projection:  []string{"event_type", "summary.succeeded"},
		Enabled:     true,
	}
	if err := InsertTiding(ctx, integrationPool, tg); err != nil {
		t.Fatalf("InsertTiding ephemeral: %v", err)
	}

	got, err := SelectTidingByName(ctx, integrationPool, "ephemeral-run")
	if err != nil {
		t.Fatalf("SelectTidingByName: %v", err)
	}
	if !got.Ephemeral || got.VoyageID == nil || *got.VoyageID != "vy_run_42" {
		t.Errorf("ephemeral/voyage_id round-trip = %v / %v", got.Ephemeral, got.VoyageID)
	}
	if got.Annotations["team"] != "ops" || got.Annotations["severity"] != "high" {
		t.Errorf("annotations round-trip = %+v", got.Annotations)
	}
	if len(got.Projection) != 2 || got.Projection[0] != "event_type" || got.Projection[1] != "summary.succeeded" {
		t.Errorf("projection round-trip = %+v", got.Projection)
	}

	// Update projection/annotations while staying ephemeral.
	got.Projection = []string{"event_type"}
	got.Annotations = map[string]any{"runbook": "https://wiki/x"}
	if err := UpdateTiding(ctx, integrationPool, got); err != nil {
		t.Fatalf("UpdateTiding: %v", err)
	}
	after, err := SelectTidingByName(ctx, integrationPool, "ephemeral-run")
	if err != nil {
		t.Fatalf("SelectTidingByName after update: %v", err)
	}
	if len(after.Projection) != 1 || after.Annotations["runbook"] != "https://wiki/x" {
		t.Errorf("update not applied: projection=%v annotations=%+v", after.Projection, after.Annotations)
	}
}

// TestIntegration_Tiding_UpdateClearsPayload covers UpdateTiding replace semantics
// (N4): a persistent rule with annotations/projection is updated without these
// fields (nil), so they are cleared (annotations='{}', projection='{}'). This is
// live proof for FE that empty/omitted PUT fields erase previous values.
func TestIntegration_Tiding_UpdateClearsPayload(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ch", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}
	tg := &Tiding{
		Name:        "persistent-payload",
		Herald:      "ch",
		EventTypes:  []string{"scenario_run.*"},
		Annotations: map[string]any{"team": "ops"},
		Projection:  []string{"summary.succeeded"},
		Enabled:     true,
	}
	if err := InsertTiding(ctx, integrationPool, tg); err != nil {
		t.Fatalf("InsertTiding: %v", err)
	}

	// PUT replace without annotations/projection (handler passes nil): clear.
	cleared := &Tiding{
		Name:       "persistent-payload",
		Herald:     "ch",
		EventTypes: []string{"scenario_run.*"},
		Enabled:    true,
	}
	if err := UpdateTiding(ctx, integrationPool, cleared); err != nil {
		t.Fatalf("UpdateTiding clear: %v", err)
	}
	after, err := SelectTidingByName(ctx, integrationPool, "persistent-payload")
	if err != nil {
		t.Fatalf("SelectTidingByName: %v", err)
	}
	if len(after.Annotations) != 0 {
		t.Errorf("annotations not cleared by replace-update: %+v", after.Annotations)
	}
	if len(after.Projection) != 0 {
		t.Errorf("projection not cleared by replace-update: %+v", after.Projection)
	}
}

// TestIntegration_Tiding_TaskSelectorRoundTrip verifies that the task selector
// (ADR-052(l), T4a) round-trips through insert -> select -> update without loss,
// and PUT replace without task clears it (omit==clear, like annotations/projection).
func TestIntegration_Tiding_TaskSelectorRoundTrip(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ch", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}
	tg := &Tiding{
		Name:       "task-sub",
		Herald:     "ch",
		EventTypes: []string{"incarnation.run_completed"},
		Task:       strptr("nginx_pkg"),
		Enabled:    true,
	}
	if err := InsertTiding(ctx, integrationPool, tg); err != nil {
		t.Fatalf("InsertTiding: %v", err)
	}

	got, err := SelectTidingByName(ctx, integrationPool, "task-sub")
	if err != nil {
		t.Fatalf("SelectTidingByName: %v", err)
	}
	if got.Task == nil || *got.Task != "nginx_pkg" {
		t.Errorf("task = %v, want nginx_pkg", got.Task)
	}

	// PUT replace without task (handler passes nil): clear (omit==clear).
	cleared := &Tiding{
		Name:       "task-sub",
		Herald:     "ch",
		EventTypes: []string{"incarnation.run_completed"},
		Enabled:    true,
	}
	if err := UpdateTiding(ctx, integrationPool, cleared); err != nil {
		t.Fatalf("UpdateTiding clear: %v", err)
	}
	after, err := SelectTidingByName(ctx, integrationPool, "task-sub")
	if err != nil {
		t.Fatalf("SelectTidingByName after: %v", err)
	}
	if after.Task != nil {
		t.Errorf("task not cleared by replace-update: %v", after.Task)
	}
}

// TestIntegration_Tiding_PersistentDefaults verifies that a persistent rule (as in
// S1) writes ephemeral=false, voyage_id=NULL, annotations='{}', projection='{}'
// through DEFAULT.
func TestIntegration_Tiding_PersistentDefaults(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ch", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}
	tg := &Tiding{Name: "persistent", Herald: "ch", EventTypes: []string{"voyage.*"}, Enabled: true}
	if err := InsertTiding(ctx, integrationPool, tg); err != nil {
		t.Fatalf("InsertTiding: %v", err)
	}
	got, err := SelectTidingByName(ctx, integrationPool, "persistent")
	if err != nil {
		t.Fatalf("SelectTidingByName: %v", err)
	}
	if got.Ephemeral || got.VoyageID != nil {
		t.Errorf("persistent rule must have ephemeral=false / voyage_id=nil, got %v / %v", got.Ephemeral, got.VoyageID)
	}
	if len(got.Annotations) != 0 || len(got.Projection) != 0 {
		t.Errorf("persistent rule defaults: annotations=%+v projection=%+v", got.Annotations, got.Projection)
	}
}

// TestIntegration_Tiding_EphemeralVoyageCHECK verifies that DB CHECK
// tidings_ephemeral_voyage_consistent catches an ephemeral <-> voyage_id violation
// by bypassing service validation with a direct INSERT (defence in depth).
func TestIntegration_Tiding_EphemeralVoyageCHECK(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ch", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}

	// ephemeral=true without voyage_id: CHECK violation.
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO tidings (name, herald, event_types, ephemeral) VALUES ($1, $2, $3, true)`,
		"bad-eph", "ch", []string{"scenario_run.*"})
	if err == nil {
		t.Fatal("expected CHECK violation: ephemeral=true requires voyage_id")
	}

	// ephemeral=false with voyage_id: CHECK violation.
	_, err = integrationPool.Exec(ctx,
		`INSERT INTO tidings (name, herald, event_types, ephemeral, voyage_id) VALUES ($1, $2, $3, false, $4)`,
		"bad-persist", "ch", []string{"scenario_run.*"}, "vy_1")
	if err == nil {
		t.Fatal("expected CHECK violation: non-ephemeral must not set voyage_id")
	}
}

// TestIntegration_Tiding_EphemeralPartialIndex verifies that the partial index
// tidings_ephemeral_voyage_idx covers only ephemeral rows. It checks pg_indexes:
// the index definition carries voyage_id plus WHERE ephemeral.
func TestIntegration_Tiding_EphemeralPartialIndex(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	var indexdef string
	err := integrationPool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'tidings_ephemeral_voyage_idx'`).Scan(&indexdef)
	if err != nil {
		t.Fatalf("index tidings_ephemeral_voyage_idx not found: %v", err)
	}
	if !strings.Contains(indexdef, "voyage_id") {
		t.Errorf("index must be on voyage_id; def = %q", indexdef)
	}
	if !strings.Contains(strings.ToLower(indexdef), "where ephemeral") {
		t.Errorf("index must be partial WHERE ephemeral; def = %q", indexdef)
	}
}

// TestIntegration_Tiding_ListHidesEphemeral guards the listing read path (S2,
// ADR-042 "dumb frontend"): SelectAllTidings without include_ephemeral returns
// only persistent rules, and total uses the same predicate. include_ephemeral=true
// returns everything. Seed one persistent rule and two one-shot rules.
func TestIntegration_Tiding_ListHidesEphemeral(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	if err := InsertHerald(ctx, integrationPool, newWebhookHerald("ch", "archon-alice")); err != nil {
		t.Fatalf("InsertHerald: %v", err)
	}

	// Persistent rule.
	persistent := &Tiding{Name: "persistent", Herald: "ch", EventTypes: []string{"voyage.*"}, Enabled: true}
	if err := InsertTiding(ctx, integrationPool, persistent); err != nil {
		t.Fatalf("InsertTiding persistent: %v", err)
	}
	// Two one-shot rules, bound to runs. The Tiding name must match NamePattern
	// (^[a-z0-9-]{1,63}$), so a voyage_id with underscores cannot be used as name.
	for i, vy := range []string{"vy_1", "vy_2"} {
		eph := &Tiding{
			Name:       fmt.Sprintf("eph-%d", i),
			Herald:     "ch",
			EventTypes: []string{"voyage.*"},
			Ephemeral:  true,
			VoyageID:   strptr(vy),
			Enabled:    true,
		}
		if err := InsertTiding(ctx, integrationPool, eph); err != nil {
			t.Fatalf("InsertTiding %s: %v", eph.Name, err)
		}
	}

	// Default (includeEphemeral=false): persistent only.
	items, total, err := SelectAllTidings(ctx, integrationPool, false, 0, 50)
	if err != nil {
		t.Fatalf("SelectAllTidings(false): %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("default listing total = %d, items = %d, want 1/1 (ephemeral hidden)", total, len(items))
	}
	if items[0].Name != "persistent" || items[0].Ephemeral {
		t.Errorf("default listing leaked ephemeral: %+v", items[0])
	}

	// includeEphemeral=true: all three.
	all, totalAll, err := SelectAllTidings(ctx, integrationPool, true, 0, 50)
	if err != nil {
		t.Fatalf("SelectAllTidings(true): %v", err)
	}
	if totalAll != 3 || len(all) != 3 {
		t.Fatalf("include_ephemeral listing total = %d, items = %d, want 3/3", totalAll, len(all))
	}
	var ephCount int
	for _, tg := range all {
		if tg.Ephemeral {
			ephCount++
		}
	}
	if ephCount != 2 {
		t.Errorf("include_ephemeral listing must contain 2 ephemeral rules, got %d", ephCount)
	}
}
