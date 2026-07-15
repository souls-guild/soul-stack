//go:build integration

// Integration tests for `core.soul.registered` via testcontainers-go
// (postgres:16-alpine). End-to-end: the module is called with a PGStore over
// a real pgxpool, verifying the side effect in souls (coven and status).
//
// Mirrors the pattern in keeper/internal/soul/integration_test.go.

package soul_test

import (
	"context"
	"log"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodsoul "github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/migrations"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
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
			log.Fatalf("coremod/soul integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("coremod/soul integration: skipping, docker unavailable: %v", err)
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

func resetSouls(t *testing.T) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, souls, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func mustStructIT(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func TestIntegration_Apply_CreatesSoul_Append(t *testing.T) {
	resetSouls(t)
	ctx := context.Background()

	m := coremodsoul.New(coremodsoul.NewPGStore(integrationPool))
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStructIT(t, map[string]any{
			"sid":   "host-create.example.com",
			"coven": []any{"prod", "db"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("unexpected: %+v", ev)
	}
	got, err := keepersoul.SelectBySID(ctx, integrationPool, "host-create.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.Status != keepersoul.StatusPending {
		t.Errorf("status=%q, want pending", got.Status)
	}
	covenGot := append([]string(nil), got.Coven...)
	sort.Strings(covenGot)
	if !reflect.DeepEqual(covenGot, []string{"db", "prod"}) {
		t.Errorf("coven=%v, want [db prod]", covenGot)
	}
}

func TestIntegration_Apply_Append_UnionWithExisting(t *testing.T) {
	resetSouls(t)
	ctx := context.Background()

	// Seed an existing Soul with one tag.
	if err := keepersoul.Insert(ctx, integrationPool, &keepersoul.Soul{
		SID:    "host-append.example.com",
		Status: keepersoul.StatusConnected,
		Coven:  []string{"prod"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := coremodsoul.New(coremodsoul.NewPGStore(integrationPool))
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStructIT(t, map[string]any{
			"sid":   "host-append.example.com",
			"coven": []any{"db", "replica"},
			"mode":  "append",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("apply failed: %+v", stream.Last())
	}
	got, err := keepersoul.SelectBySID(ctx, integrationPool, "host-append.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	c := append([]string(nil), got.Coven...)
	sort.Strings(c)
	if !reflect.DeepEqual(c, []string{"db", "prod", "replica"}) {
		t.Errorf("coven=%v, want [db prod replica]", c)
	}
}

func TestIntegration_Apply_Replace(t *testing.T) {
	resetSouls(t)
	ctx := context.Background()

	if err := keepersoul.Insert(ctx, integrationPool, &keepersoul.Soul{
		SID:    "host-replace.example.com",
		Status: keepersoul.StatusConnected,
		Coven:  []string{"prod", "old"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := coremodsoul.New(coremodsoul.NewPGStore(integrationPool))
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStructIT(t, map[string]any{
			"sid":   "host-replace.example.com",
			"coven": []any{"prod", "new"},
			"mode":  "replace",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("apply failed: %+v", stream.Last())
	}
	got, err := keepersoul.SelectBySID(ctx, integrationPool, "host-replace.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	c := append([]string(nil), got.Coven...)
	sort.Strings(c)
	if !reflect.DeepEqual(c, []string{"new", "prod"}) {
		t.Errorf("coven=%v, want [new prod]", c)
	}
}

func TestIntegration_Apply_Remove(t *testing.T) {
	resetSouls(t)
	ctx := context.Background()

	if err := keepersoul.Insert(ctx, integrationPool, &keepersoul.Soul{
		SID:    "host-remove.example.com",
		Status: keepersoul.StatusConnected,
		Coven:  []string{"prod", "legacy", "db"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := coremodsoul.New(coremodsoul.NewPGStore(integrationPool))
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStructIT(t, map[string]any{
			"sid":   "host-remove.example.com",
			"coven": []any{"legacy"},
			"mode":  "remove",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("apply failed: %+v", stream.Last())
	}
	got, _ := keepersoul.SelectBySID(ctx, integrationPool, "host-remove.example.com")
	c := append([]string(nil), got.Coven...)
	sort.Strings(c)
	if !reflect.DeepEqual(c, []string{"db", "prod"}) {
		t.Errorf("coven=%v, want [db prod]", c)
	}
}

func TestIntegration_Apply_Idempotent(t *testing.T) {
	resetSouls(t)
	ctx := context.Background()

	if err := keepersoul.Insert(ctx, integrationPool, &keepersoul.Soul{
		SID:    "host-idem.example.com",
		Status: keepersoul.StatusConnected,
		Coven:  []string{"prod", "db"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := coremodsoul.New(coremodsoul.NewPGStore(integrationPool))

	// First run: changed=false (set already matches).
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStructIT(t, map[string]any{
			"sid":   "host-idem.example.com",
			"coven": []any{"prod", "db"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || ev.Changed {
		t.Fatalf("expected no-op, got %+v", ev)
	}
}
