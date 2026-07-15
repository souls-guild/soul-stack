//go:build integration

// Integration tests for pgxWriter via testcontainers-go.
//
// Spins up postgres:16-alpine on an ephemeral port, applies migrations from
// keeper/migrations/, runs a write+read round-trip. One container per package
// (TestMain) — starting postgres takes ~3-5s, otherwise it would add up to
// more overall.
//
// Run:
//
//	make test-integration
//	# or
//	cd keeper && go test -tags=integration -race -count=1 ./internal/auditpg/
//
// Requires docker (testcontainers uses the docker socket). If docker is
// unavailable — TestMain does a t.Skip-equivalent (os.Exit(0) with a log).
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

// integrationPool — shared pool for the package, initialized in TestMain.
// Tests TRUNCATE audit_log before running (see resetAuditLog) — cheaper than
// spinning up a container per Test*.
var integrationPool *pgxpool.Pool

// TestMain delegates setup/teardown to run(), because os.Exit bypasses
// defers — the context, container, and pool would otherwise leak.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run starts a Postgres container, applies migrations, and hands off to
// m.Run(). Returns the exit code; defers inside the function run correctly
// because os.Exit is called only in TestMain, on top of the returned code.
//
// SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1|true makes testcontainers mandatory
// (CI mode): any setup failure → log.Fatalf. Without the flag (local mode) —
// tests are skipped when docker is unavailable.
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
		// Separate ctx — the main one may be canceled by the test exiting.
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

	// Seed the bootstrap Archon: migration 004 adds an FK from
	// audit_log.archon_aid to operators(aid). Tests below write events with
	// `archon_aid: archon-alice` — without the seed the INSERT would fail the
	// FK. `created_by_aid IS NULL` means "this is bootstrap", matching the
	// archon-alice role in the RBAC examples (rbac.md).
	if _, err := pool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('archon-alice', 'Alice (test bootstrap)', 'jwt')
	`); err != nil {
		log.Printf("auditpg integration: seed archon-alice: %v", err)
		return 1
	}

	return m.Run()
}

// resetAuditLog — TRUNCATE between tests so one test doesn't see another's
// rows. Cheaper than re-creating the container.
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
	// JSON numbers via json.Unmarshal → float64; explicit cast.
	if rev, ok := payload["rev"].(float64); !ok || rev != 42 {
		t.Errorf("payload.rev = %v (%T), want 42", payload["rev"], payload["rev"])
	}
}

// TestIntegration_Reader_ArchonAID_ILIKE — runtime proof of ILIKE search
// semantics on archon_aid: a partial substring in a DIFFERENT case ("ALIC")
// finds the `archon-alice` record. Exact `=` (the old behavior) would not have
// found it. Also checks that a non-matching substring ("bob") produces no
// false positives.
func TestIntegration_Reader_ArchonAID_ILIKE(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	reader := NewReader(integrationPool)

	// archon-alice — the only seeded FK-valid AID (see run()).
	if err := w.Write(ctx, &audit.Event{
		EventType:     audit.EventConfigReloadSucceeded,
		Source:        audit.SourceAPI,
		ArchonAID:     "archon-alice",
		CorrelationID: audit.NewULID(),
		Payload:       map[string]any{"path": "/etc/keeper.yml"},
	}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// Substring in a different case — should still match (case-insensitive
	// substring).
	for _, q := range []string{"ALIC", "alice", "archon-", "ALICE"} {
		rows, total, err := reader.List(ctx, ListFilter{ArchonAID: q}, 0, 50)
		if err != nil {
			t.Fatalf("List(ArchonAID=%q): %v", q, err)
		}
		if total != 1 || len(rows) != 1 {
			t.Fatalf("ArchonAID=%q total = %d, want 1 (case-insensitive substring)", q, total)
		}
		if rows[0].ArchonAID == nil || *rows[0].ArchonAID != "archon-alice" {
			t.Errorf("ArchonAID=%q matched wrong row: %v", q, rows[0].ArchonAID)
		}
	}

	// Non-matching substring — empty (no false positives).
	_, total, err := reader.List(ctx, ListFilter{ArchonAID: "bob"}, 0, 50)
	if err != nil {
		t.Fatalf("List(ArchonAID=bob): %v", err)
	}
	if total != 0 {
		t.Errorf("ArchonAID=bob total = %d, want 0", total)
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
		// ArchonAID, CorrelationID — empty; should land as NULL.
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

// TestIntegration_Reader_DeliveryHistory_Filters — guard for the read path of
// notification deliveries (ADR-052, one-off notification rework S2). The
// "Notifications" section on the run page and the channel history read
// delivery terminals (herald.delivered/herald.failed) via GET /v1/audit:
//
//   - voyage section: correlation_id=<voyage_id> + type=herald.delivered/failed
//     → delivery events for EXACTLY this run;
//   - channel history: payload_herald=<herald-name> → all deliveries of the
//     channel (filter on payload->>'herald', added in S2).
//
// Seeds terminals of two runs into two channels, checks both filters in
// isolation and their intersection.
func TestIntegration_Reader_DeliveryHistory_Filters(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	reader := NewReader(integrationPool)

	voyageA := audit.NewULID()
	voyageB := audit.NewULID()

	// Delivery terminals: correlation_id = voyage_id (as written by
	// worker.emitAudit), payload.herald = channel name. Cross-matrixed: both
	// channels in both runs + a failed terminal, plus "unrelated" events
	// (different type) — the filter cuts those out.
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
	// Unrelated noise — not a delivery, must not match either filter.
	if err := w.Write(ctx, &audit.Event{
		EventType:     audit.EventConfigReloadSucceeded,
		Source:        audit.SourceAPI,
		CorrelationID: voyageA,
		Payload:       map[string]any{"path": "/etc/keeper.yml"},
	}); err != nil {
		t.Fatalf("seed noise: %v", err)
	}

	deliveryTypes := []string{string(audit.EventHeraldDelivered), string(audit.EventHeraldFailed)}

	// (1) voyage section: deliveries of run A — both A terminals (delivered
	// ops-slack + failed ops-pager), but NOT B's deliveries and NOT run A's
	// config noise.
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

	// (2) ops-slack channel history: all deliveries of the channel (runs A and
	// B), excluding ops-pager.
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

	// (3) intersection of voyage_id ∩ herald: one specific delivery in B on
	// channel ops-pager.
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

// TestIntegration_Reader_ChangedTaskKeys — guard for the read path of the
// changed-task aggregation (T3): SelectChangedTaskKeys reads (sid, plan_index)
// for a run's tasks that terminated CHANGED, STRICTLY from `task.executed`
// events with `status == TASK_STATUS_CHANGED`. Checks:
//   - filter by correlation_id (apply_id) + event_type + status (other runs /
//     statuses / types are excluded);
//   - dedup of the (sid, plan_index) pair (retry produced two task.executed
//     rows for one task);
//   - backward-compat: rows WITHOUT plan_index (old Soul / pre-T3 run) are read
//     with a fallback to task_idx (COALESCE in SQL);
//   - secret hygiene: register_data/error payload values do NOT affect the
//     keys (only sid + plan_index are read).
//
// This seed deliberately sets ONLY task_idx (no plan_index) — exercises the
// fallback branch. Priority of plan_index over task_idx under
// staged/per-host (plan_idx ≠ task_idx) is checked by
// TestIntegration_Reader_ChangedTaskKeys_PlanIndexPriority.
func TestIntegration_Reader_ChangedTaskKeys(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	reader := NewReader(integrationPool)

	applyA := audit.NewULID()
	applyB := audit.NewULID()

	// task.executed events for run A: mixed statuses; CHANGED on (a,0),(b,0),(a,1);
	// retry: (a,0) is written twice (dedup). + an OK task (not changed) + FAILED.
	// register_data is a secret-shaped payload and must not leak into the keys.
	type te struct {
		apply  string
		sid    string
		idx    int
		status string
	}
	seed := []te{
		{applyA, "a.local", 0, "TASK_STATUS_CHANGED"},
		{applyA, "a.local", 0, "TASK_STATUS_CHANGED"}, // retry → duplicate, dedup
		{applyA, "b.local", 0, "TASK_STATUS_CHANGED"},
		{applyA, "a.local", 1, "TASK_STATUS_CHANGED"},
		{applyA, "a.local", 2, "TASK_STATUS_OK"},      // not changed
		{applyA, "b.local", 1, "TASK_STATUS_FAILED"},  // not changed
		{applyB, "z.local", 0, "TASK_STATUS_CHANGED"}, // a different run
	}
	for _, s := range seed {
		ev := &audit.Event{
			EventType:     audit.EventTaskExecuted,
			Source:        audit.SourceSoulGRPC,
			CorrelationID: s.apply,
			Payload: map[string]any{
				"sid":      s.sid,
				"apply_id": s.apply,
				// WITHOUT plan_index — backward-compat: read falls back to task_idx.
				"task_idx":      s.idx,
				"status":        s.status,
				"register_data": map[string]any{"password": "should-not-leak-into-key"},
			},
		}
		if err := w.Write(ctx, ev); err != nil {
			t.Fatalf("seed write: %v", err)
		}
	}
	// Unrelated noise: run.completed with the same apply_id — not task.executed, filtered out.
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

	// Expect EXACTLY {(a,0),(b,0),(a,1)} — dedup collapsed the (a,0) duplicate;
	// OK/FAILED and run B are excluded. plan_index comes from the task_idx
	// fallback.
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
	// Run B must not be present.
	if _, ok := keys[ChangedTaskKey{SID: "z.local", PlanIndex: 0}]; ok {
		t.Error("cross-apply leak: applyB key in applyA result")
	}
}

// TestIntegration_Reader_ChangedTaskKeys_PlanIndexPriority — T3 GUARD (read
// path): under staged/per-host-where the GLOBAL plan_index ≠ the LOCAL
// task_idx; the CHANGED-task aggregation MUST use plan_index (the correlation
// key with RenderedTask.Index), NOT task_idx — otherwise the key would point
// at a neighboring task (mismatch in the state_changes whitelist + audit
// changed_tasks).
//
// Seed: one CHANGED task with plan_index=7, task_idx=2 (simulating a second
// Passage, where local position 2 corresponds to global plan position 7).
// Expect key (sid, 7) — the global one; reverse invariant: (sid, 2) (the local
// task_idx) must NOT be present in the result.
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
			// staged/per-host: local position 2 within its Passage ≠ the global
			// cross-run index 7 across the whole plan.
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

	// The key is the GLOBAL plan_index (7), not the local task_idx (2).
	if _, ok := keys[ChangedTaskKey{SID: "h.local", PlanIndex: 7}]; !ok {
		t.Errorf("ожидался ключ (h.local, plan_index=7) — свёртка ДОЛЖНА брать глобальный plan_index; keys=%+v", keys)
	}
	// REVERSE: the local task_idx (2) must NOT become a key — otherwise the
	// correlation with the plan would point at a neighboring task (T3 bug).
	if _, ok := keys[ChangedTaskKey{SID: "h.local", PlanIndex: 2}]; ok {
		t.Errorf("ключ (h.local, 2) присутствует — свёртка взяла ЛОКАЛЬНЫЙ task_idx вместо plan_index (T3-регресс); keys=%+v", keys)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1: %+v", len(keys), keys)
	}
}

// TestIntegration_Reader_PayloadVoyage_Filter — guard for the read path of
// Voyage detail visibility (ADR-052 amend §k): per-incarnation
// incarnation.run_completed events carry correlation_id=apply_id (DIFFERENT
// for each incarnation), while voyage_id lives in payload. Voyage detail
// collects a voyage's run events via the payload_voyage filter
// (payload->>'voyage_id'). Checks:
//   - the filter returns ALL per-incarnation run events for a given voyage_id
//     (despite different apply_id/correlation_id);
//   - does NOT return run events of a different voyage;
//   - does NOT return events without voyage_id (direct create/rerun/destroy
//     path);
//   - parameterization: the filter value goes through a positional
//     placeholder, not concatenation (indirectly — a search with a quoted
//     literal doesn't match).
func TestIntegration_Reader_PayloadVoyage_Filter(t *testing.T) {
	resetAuditLog(t)
	ctx := context.Background()

	w := NewWriter(integrationPool)
	reader := NewReader(integrationPool)

	voyageA := audit.NewULID()
	voyageB := audit.NewULID()

	// Run events for voyage A: TWO incarnations, each with its own apply_id
	// (correlation_id), sharing voyage_id in payload. + a run event for voyage B
	// + an event WITHOUT voyage_id (direct path bypassing Voyage) — both must be
	// cut off by filter A.
	seed := []struct {
		voyage      string // "" → no voyage_id in payload (direct path)
		incarnation string
		status      string
	}{
		{voyageA, "redis-a", "success"},
		{voyageA, "redis-b", "failed"},
		{voyageB, "redis-c", "success"},
		{"", "redis-direct", "success"}, // create path: no voyage_id
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

	// Filter by voyage A: EXACTLY two per-incarnation events (redis-a +
	// redis-b), despite different correlation_id; neither B nor the direct
	// path.
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

	// Filter by voyage B: exactly one event (redis-c), no leak from A / direct path.
	_, totalB, err := reader.List(ctx, ListFilter{PayloadVoyage: voyageB}, 0, 50)
	if err != nil {
		t.Fatalf("List voyageB: %v", err)
	}
	if totalB != 1 {
		t.Errorf("voyageB run-events total = %d, want 1", totalB)
	}

	// Nonexistent voyage_id → empty (events without voyage_id don't match the filter).
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
