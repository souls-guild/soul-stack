//go:build integration

// Integration tests of Voyage CRUD via testcontainers-go (ADR-043).
// Pattern matches keeper/internal/choir/integration_test.go.

package voyage

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
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
			log.Fatalf("voyage integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("voyage integration: skipping, docker unavailable: %v", err)
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
	// CASCADE: voyage_targets → voyages → operators (FK chain).
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE voyage_targets, voyages, operators CASCADE`); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedOperator(t *testing.T, aid string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ($1, $2, 'jwt')`, aid, aid); err != nil {
		t.Fatalf("seedOperator(%s): %v", aid, err)
	}
}

// TestInsertTargets_Integration runs CopyFrom insert of voyage_targets against
// REAL Postgres (testcontainers + migration 059) and reads rows back. Catches
// column mismatch of voyage_targets that mock test cannot catch in principle
// (S-med-3 BLOCKER: CopyFrom declared nonexistent target_sid / target_incarnation
// / attempt — on live DB would fail «column does not exist», but mock gave
// false-green). Covers both target types: scenario (target_id = incarnation name)
// and command (target_id = SID).
func TestInsertTargets_Integration(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedOperator(t, "archon-test")

	// Parent Voyage (kind=scenario) via real CRUD: InsertTargets references it
	// via FK voyage_targets.voyage_id → voyages.voyage_id.
	v := scenarioVoyage()
	v.VoyageID = "01H000000000000000000VOYIN"
	v.StartedByAID = "archon-test"
	if err := Insert(ctx, integrationPool, v); err != nil {
		t.Fatalf("Insert voyage: %v", err)
	}

	// Single CopyFrom via real pool: mixed scenario/command targets.
	targets := []VoyageTarget{
		{TargetKind: TargetKindIncarnation, TargetID: "service-web", BatchIndex: 0},
		{TargetKind: TargetKindSID, TargetID: "host-a.example.com", BatchIndex: 1},
	}
	if err := InsertTargets(ctx, integrationPool, v.VoyageID, targets); err != nil {
		t.Fatalf("InsertTargets (real PG): %v", err)
	}

	// Reverse SELECT: check target_id (incarnation/SID per kind), kind,
	// batch_index, status (DEFAULT awaiting). If CopyFrom columns did not match
	// schema 059 — InsertTargets above would fail on «column does not exist»,
	// asserts would not reach here.
	rows, err := integrationPool.Query(ctx, `
		SELECT target_kind, target_id, batch_index, status
		FROM voyage_targets
		WHERE voyage_id = $1
		ORDER BY batch_index`, v.VoyageID)
	if err != nil {
		t.Fatalf("select targets: %v", err)
	}
	defer rows.Close()

	type row struct {
		kind     string
		targetID string
		batch    int
		status   string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.targetID, &r.batch, &r.status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	want := []row{
		{kind: string(TargetKindIncarnation), targetID: "service-web", batch: 0, status: string(TargetStatusAwaiting)},
		{kind: string(TargetKindSID), targetID: "host-a.example.com", batch: 1, status: string(TargetStatusAwaiting)},
	}
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
