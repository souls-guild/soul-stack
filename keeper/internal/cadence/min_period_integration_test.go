//go:build integration

package cadence_test

// Integration tests for the Conductor adaptive min-period resolver (ADR-048
// "Adaptive interval"). Under testcontainers-PG: SelectMinPeriod aggregates the
// enabled registry (MIN(interval_seconds) + bool_or(cron)), and
// DerivedMinPeriod/Clamp derive the polling step. Enabled by the `integration`
// build tag.

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}

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
			log.Fatalf("cadence integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("cadence integration: skipping, docker unavailable: %v", err)
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

	seedOperator(ctx, pool)
	return m.Run()
}

const testAID = "archon-cadence-it"

func seedOperator(ctx context.Context, pool *pgxpool.Pool) {
	_, err := pool.Exec(ctx,
		`INSERT INTO operators (aid, display_name, auth_method, created_by_aid)
		 VALUES ($1, 'Cadence IT', 'jwt', NULL)
		 ON CONFLICT (aid) DO NOTHING`, testAID)
	if err != nil {
		log.Fatalf("cadence integration: seed operator: %v", err)
	}
}

func clearCadences(t *testing.T) {
	t.Helper()
	// DELETE, not TRUNCATE: voyages.cadence_id FK references cadences (TRUNCATE
	// fails on the referenced table). voyages are empty in this test, so DELETE
	// is safe.
	if _, err := integrationPool.Exec(context.Background(), `DELETE FROM cadences`); err != nil {
		t.Fatalf("delete cadences: %v", err)
	}
}

func intptr(i int) *int       { return &i }
func strptr(s string) *string { return &s }

var ulidCounter int

func nextID() string {
	ulidCounter++
	// 26-character ULID-like surrogate (CRUD only checks non-emptiness).
	return "01H000000000000000000CAD" + string(rune('A'+ulidCounter/10)) + string(rune('0'+ulidCounter%10))
}

func insertInterval(t *testing.T, enabled bool, sec int) {
	t.Helper()
	c := &cadence.Cadence{
		ID:              nextID(),
		Name:            "iv",
		Enabled:         enabled,
		ScheduleKind:    cadence.ScheduleKindInterval,
		IntervalSeconds: intptr(sec),
		OverlapPolicy:   cadence.OverlapPolicySkip,
		Kind:            cadence.KindScenario,
		ScenarioName:    strptr("converge"),
		Target:          json.RawMessage(`{"coven":"prod"}`),
		CreatedByAID:    testAID,
	}
	if err := cadence.Insert(context.Background(), integrationPool, c); err != nil {
		t.Fatalf("insert interval cadence: %v", err)
	}
}

func insertCron(t *testing.T, enabled bool, expr string) {
	t.Helper()
	c := &cadence.Cadence{
		ID:            nextID(),
		Name:          "cron",
		Enabled:       enabled,
		ScheduleKind:  cadence.ScheduleKindCron,
		CronExpr:      strptr(expr),
		OverlapPolicy: cadence.OverlapPolicyQueue,
		Kind:          cadence.KindCommand,
		Module:        strptr("core.cmd.shell"),
		Target:        json.RawMessage(`["a.example"]`),
		CreatedByAID:  testAID,
	}
	if err := cadence.Insert(context.Background(), integrationPool, c); err != nil {
		t.Fatalf("insert cron cadence: %v", err)
	}
}

// TestSelectMinPeriod_Integration checks derivedMinPeriod + Clamp against the
// real registry in PG ("Calm" profile 30s/60s/120s).
func TestSelectMinPeriod_Integration(t *testing.T) {
	const (
		floor   = 30 * time.Second
		ceiling = 60 * time.Second
		idle    = 120 * time.Second
	)
	ctx := context.Background()

	type step struct {
		name      string
		seed      func(t *testing.T)
		wantStep  time.Duration // final clamp/idle step
		wantEmpty bool
	}
	steps := []step{
		{
			name:      "empty → idle 120 (empty signal)",
			seed:      func(*testing.T) {},
			wantStep:  idle,
			wantEmpty: true,
		},
		{
			name:     "interval 30 (frequent) → floor-bound 30",
			seed:     func(t *testing.T) { insertInterval(t, true, 30) },
			wantStep: 30 * time.Second,
		},
		{
			name:     "interval 3600 (rare) → ceiling-cap 60",
			seed:     func(t *testing.T) { insertInterval(t, true, 3600) },
			wantStep: 60 * time.Second,
		},
		{
			name:     "cron-only → 60",
			seed:     func(t *testing.T) { insertCron(t, true, "0 */6 * * *") },
			wantStep: 60 * time.Second,
		},
		{
			name: "mixed interval 45 + cron → 45",
			seed: func(t *testing.T) {
				insertInterval(t, true, 45)
				insertCron(t, true, "*/2 * * * *")
			},
			wantStep: 45 * time.Second,
		},
		{
			// Inside the corridor (30 < 31 < 60): neither the floor nor the ceiling
			// clamps it; Clamp must return derived as-is. Sub-floor values
			// (interval < 30) are rejected by DB-CHECK 068 Pass B on INSERT;
			// clamp-floor for derived < floor is covered by pure TestClamp
			// (defence-in-depth: Clamp protects if a row bypassed CHECK).
			name:     "interval 31 (inside corridor) → derived 31 without clamp",
			seed:     func(t *testing.T) { insertInterval(t, true, 31) },
			wantStep: 31 * time.Second,
		},
		{
			name: "disabled interval 30 + enabled cron → cron 60 (disabled ignored)",
			seed: func(t *testing.T) {
				insertInterval(t, false, 30) // disabled, excluded from MIN
				insertCron(t, true, "0 0 * * *")
			},
			wantStep: 60 * time.Second,
		},
		{
			name: "only disabled → empty → idle 120",
			seed: func(t *testing.T) {
				insertInterval(t, false, 30) // disabled (>= floor by DB-CHECK 068)
				insertCron(t, false, "* * * * *")
			},
			wantStep:  idle,
			wantEmpty: true,
		},
	}

	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			clearCadences(t)
			s.seed(t)

			mp, err := cadence.SelectMinPeriod(ctx, integrationPool)
			if err != nil {
				t.Fatalf("SelectMinPeriod: %v", err)
			}
			if got := mp.Empty(); got != s.wantEmpty {
				t.Errorf("Empty() = %v, want %v", got, s.wantEmpty)
			}
			derived, ok := mp.DerivedMinPeriod()
			var step time.Duration
			if !ok {
				step = idle
			} else {
				step = cadence.Clamp(derived, floor, ceiling)
			}
			if step != s.wantStep {
				t.Errorf("adaptive step = %v, want %v (derived=%v ok=%v)", step, s.wantStep, derived, ok)
			}
		})
	}
}
