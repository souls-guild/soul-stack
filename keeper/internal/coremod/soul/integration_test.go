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
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulforget"
	"github.com/souls-guild/soul-stack/keeper/migrations"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	ctr, err := integrationenv.Start(ctx, "postgres", func(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
		return tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("keeper_test"),
			tcpostgres.WithUsername("keeper"),
			tcpostgres.WithPassword("keeper"),
			tcpostgres.BasicWaitStrategies(),
		)
	})
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

// TestIntegration_Forgotten_MembersOfTheRunOnly — `core.soul.forgotten` over the real
// forget path and real rows: the running incarnation's members go with their seeds
// and memberships; a host of ANOTHER incarnation only and a registered non-member
// stay; the re-run finds no members and changes nothing.
func TestIntegration_Forgotten_MembersOfTheRunOnly(t *testing.T) {
	resetSouls(t)
	ctx := context.Background()
	mustExecIT(t, `TRUNCATE TABLE incarnation_membership, incarnation CASCADE`)
	for _, sid := range []string{"a.example.com", "b.example.com", "other.example.com", "pending.example.com"} {
		if err := keepersoul.Insert(ctx, integrationPool, &keepersoul.Soul{SID: sid, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusConnected}); err != nil {
			t.Fatalf("insert %s: %v", sid, err)
		}
	}
	mustExecIT(t, `INSERT INTO incarnation (id, service, service_version, status)
	               VALUES ('inc-one', 'svc', 'v1', 'destroying'), ('inc-two', 'svc', 'v1', 'ready')`)
	mustExecIT(t, `INSERT INTO incarnation_membership (incarnation_name, sid)
	               VALUES ('inc-one', 'a.example.com'), ('inc-one', 'b.example.com'), ('inc-two', 'other.example.com')`)

	apply := func() *pluginv1.ApplyEvent {
		m := coremodsoul.New(coremodsoul.NewPGStore(integrationPool)).
			WithForget(coremodsoul.NewPGForgetter(integrationPool, nil), nil)
		stream := internaltest.NewApplyStreamCtx(destroyRunCtx(ctx))
		if err := m.Apply(&pluginv1.ApplyRequest{
			State:  coremodsoul.StateForgotten,
			Params: mustStructIT(t, map[string]any{"scope": "incarnation"}),
		}, stream); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		return stream.Last()
	}

	ev := apply()
	if ev.GetFailed() || !ev.GetChanged() {
		t.Fatalf("first run: failed=%v changed=%v msg=%s", ev.GetFailed(), ev.GetChanged(), ev.GetMessage())
	}
	if got := ev.GetOutput().AsMap()["forgotten"]; !reflect.DeepEqual(got, []any{"a.example.com", "b.example.com"}) {
		t.Errorf("forgotten = %v", got)
	}
	for _, sid := range []string{"a.example.com", "b.example.com"} {
		if _, err := keepersoul.SelectBySID(ctx, integrationPool, sid); err == nil {
			t.Errorf("%s is still registered", sid)
		}
	}
	for _, sid := range []string{"other.example.com", "pending.example.com"} {
		if _, err := keepersoul.SelectBySID(ctx, integrationPool, sid); err != nil {
			t.Errorf("%s, not a member of inc-one, was forgotten: %v", sid, err)
		}
	}

	again := apply()
	if again.GetFailed() || again.GetChanged() {
		t.Errorf("re-run: failed=%v changed=%v msg=%s", again.GetFailed(), again.GetChanged(), again.GetMessage())
	}
}

// TestIntegration_Forgotten_SharedHostRefused — a member of the running incarnation
// that is also a member of another one is refused, and nothing is forgotten — not even
// the run's own other member.
func TestIntegration_Forgotten_SharedHostRefused(t *testing.T) {
	resetSouls(t)
	ctx := context.Background()
	mustExecIT(t, `TRUNCATE TABLE incarnation_membership, incarnation CASCADE`)
	for _, sid := range []string{"a.example.com", "shared.example.com"} {
		if err := keepersoul.Insert(ctx, integrationPool, &keepersoul.Soul{SID: sid, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusConnected}); err != nil {
			t.Fatalf("insert %s: %v", sid, err)
		}
	}
	mustExecIT(t, `INSERT INTO incarnation (id, service, service_version, status)
	               VALUES ('inc-one', 'svc', 'v1', 'destroying'), ('inc-two', 'svc', 'v1', 'ready')`)
	mustExecIT(t, `INSERT INTO incarnation_membership (incarnation_name, sid)
	               VALUES ('inc-one', 'a.example.com'), ('inc-one', 'shared.example.com'), ('inc-two', 'shared.example.com')`)

	m := coremodsoul.New(coremodsoul.NewPGStore(integrationPool)).
		WithForget(coremodsoul.NewPGForgetter(integrationPool, nil), nil)
	stream := internaltest.NewApplyStreamCtx(destroyRunCtx(ctx))
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  coremodsoul.StateForgotten,
		Params: mustStructIT(t, map[string]any{"scope": "incarnation"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ev := stream.Last(); !ev.GetFailed() {
		t.Fatalf("a shared host was not refused: %v", ev.GetOutput().AsMap())
	}
	for _, sid := range []string{"a.example.com", "shared.example.com"} {
		if _, err := keepersoul.SelectBySID(ctx, integrationPool, sid); err != nil {
			t.Errorf("%s was forgotten despite the refusal: %v", sid, err)
		}
	}
}

// TestIntegration_Forgotten_VoiceInAnotherIncarnationRefused — a member of the
// run's incarnation only, unbound from inc-two but still holding a Voice in its
// Choir, is refused against real rows, and nothing is forgotten.
func TestIntegration_Forgotten_VoiceInAnotherIncarnationRefused(t *testing.T) {
	resetSouls(t)
	ctx := context.Background()
	mustExecIT(t, `TRUNCATE TABLE incarnation_choir_voices, incarnation_choirs, incarnation_membership, incarnation CASCADE`)
	for _, sid := range []string{"a.example.com", "voice.example.com"} {
		if err := keepersoul.Insert(ctx, integrationPool, &keepersoul.Soul{SID: sid, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusConnected}); err != nil {
			t.Fatalf("insert %s: %v", sid, err)
		}
	}
	mustExecIT(t, `INSERT INTO incarnation (id, service, service_version, status)
	               VALUES ('inc-one', 'svc', 'v1', 'destroying'), ('inc-two', 'svc', 'v1', 'ready')`)
	mustExecIT(t, `INSERT INTO incarnation_membership (incarnation_name, sid)
	               VALUES ('inc-one', 'a.example.com'), ('inc-one', 'voice.example.com')`)
	mustExecIT(t, `INSERT INTO incarnation_choirs (incarnation_name, choir_name) VALUES ('inc-two', 'db')`)
	mustExecIT(t, `INSERT INTO incarnation_choir_voices (incarnation_name, choir_name, sid, role)
	               VALUES ('inc-two', 'db', 'voice.example.com', 'replica')`)

	ev := applyForgottenIT(t, ctx, coremodsoul.NewPGForgetter(integrationPool, nil))
	if !ev.GetFailed() {
		t.Fatalf("a host with a Voice in another incarnation was not refused: %v", ev.GetOutput().AsMap())
	}
	for _, sid := range []string{"a.example.com", "voice.example.com"} {
		if _, err := keepersoul.SelectBySID(ctx, integrationPool, sid); err != nil {
			t.Errorf("%s was forgotten despite the refusal: %v", sid, err)
		}
	}
	var voices int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM incarnation_choir_voices WHERE incarnation_name = 'inc-two'`).Scan(&voices); err != nil || voices != 1 {
		t.Errorf("inc-two's Voices = %d (%v), want 1", voices, err)
	}
}

// bindingForgetter is the real forget path with a concurrent bind in the window
// between the step's pre-check and the delete: before each Forget it makes the host
// a member of inc-two as well.
type bindingForgetter struct {
	*coremodsoul.PGForgetter
	t *testing.T
}

func (b bindingForgetter) Forget(ctx context.Context, sid, incarnation, reason string) (soulforget.Result, error) {
	mustExecIT(b.t, `INSERT INTO incarnation_membership (incarnation_name, sid) VALUES ('inc-two', $1)`, sid)
	return b.PGForgetter.Forget(ctx, sid, incarnation, reason)
}

// TestIntegration_Forgotten_BindInTheWindowIsRefusedInsideTheDelete — the pre-check
// saw a host of inc-one alone; by the delete it is shared. Only the guard inside
// the forget transaction (PGForgetter.Forget → soulforget.EraseOwned) can see that,
// so this is what fails if the forget path ever goes through the unguarded Erase.
func TestIntegration_Forgotten_BindInTheWindowIsRefusedInsideTheDelete(t *testing.T) {
	resetSouls(t)
	ctx := context.Background()
	mustExecIT(t, `TRUNCATE TABLE incarnation_membership, incarnation CASCADE`)
	if err := keepersoul.Insert(ctx, integrationPool, &keepersoul.Soul{SID: "a.example.com", Transport: keepersoul.TransportAgent, Status: keepersoul.StatusConnected}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	mustExecIT(t, `INSERT INTO incarnation (id, service, service_version, status)
	               VALUES ('inc-one', 'svc', 'v1', 'destroying'), ('inc-two', 'svc', 'v1', 'ready')`)
	mustExecIT(t, `INSERT INTO incarnation_membership (incarnation_name, sid) VALUES ('inc-one', 'a.example.com')`)

	ev := applyForgottenIT(t, ctx, bindingForgetter{PGForgetter: coremodsoul.NewPGForgetter(integrationPool, nil), t: t})
	if !ev.GetFailed() {
		t.Fatalf("a host bound to inc-two in the window was forgotten: %v", ev.GetOutput().AsMap())
	}
	if _, err := keepersoul.SelectBySID(ctx, integrationPool, "a.example.com"); err != nil {
		t.Errorf("the host was erased although it became shared before the delete: %v", err)
	}
	var memberships int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM incarnation_membership WHERE sid = 'a.example.com'`).Scan(&memberships); err != nil || memberships != 2 {
		t.Errorf("memberships = %d (%v), want both left in place", memberships, err)
	}
}

func applyForgottenIT(t *testing.T, ctx context.Context, f coremodsoul.Forgetter) *pluginv1.ApplyEvent {
	t.Helper()
	m := coremodsoul.New(coremodsoul.NewPGStore(integrationPool)).WithForget(f, nil)
	stream := internaltest.NewApplyStreamCtx(destroyRunCtx(ctx))
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  coremodsoul.StateForgotten,
		Params: mustStructIT(t, map[string]any{"scope": "incarnation"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return stream.Last()
}

// destroyRunCtx is what keeper_dispatch puts on a keeper task of inc-one's destroy run.
func destroyRunCtx(ctx context.Context) context.Context {
	return util.WithRunScope(util.WithIncarnation(ctx, "inc-one"),
		util.RunScope{Scenario: "destroy", ApplyID: "01JDESTROY", Destroy: true})
}

func mustExecIT(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
