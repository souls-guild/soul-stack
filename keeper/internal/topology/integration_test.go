//go:build integration

// Integration tests for topology resolver via testcontainers-go
// (postgres:16-alpine). Pattern matches
// keeper/internal/soul/integration_test.go.

package topology

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
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
			log.Fatalf("topology integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("topology integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("topology integration: ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("topology integration: migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("topology integration: pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

func resetAll(t *testing.T) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, souls, state_history, incarnation, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedOperator(t *testing.T, aid string) {
	t.Helper()
	op := &operator.Operator{AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seedOperator(%s): %v", aid, err)
	}
}

func seedIncarnation(t *testing.T, name string, spec map[string]any) {
	t.Helper()
	inc := &incarnation.Incarnation{
		Name:               name,
		Service:            "redis",
		ServiceVersion:     "v1.0.0",
		StateSchemaVersion: 1,
		Spec:               spec,
		Status:             incarnation.StatusReady,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnation(%s): %v", name, err)
	}
}

func seedSoul(t *testing.T, sid string, coven []string, status soul.Status) {
	t.Helper()
	s := &soul.Soul{SID: sid, Coven: coven, Status: status}
	if err := soul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedSoul(%s): %v", sid, err)
	}
}

// seedMembership binds SIDs to an incarnation (NIM-124: membership is a
// first-class relation `incarnation_membership`, no longer incarnation.name in
// souls.coven[]). The roster now resolves members via this table.
func seedMembership(t *testing.T, incName string, sids ...string) {
	t.Helper()
	if err := incarnation.AddMembers(context.Background(), integrationPool, incName, sids, nil); err != nil {
		t.Fatalf("seedMembership(%s): %v", incName, err)
	}
}

func setSoulprint(t *testing.T, sid string, factsJSON []byte, collectedAt, receivedAt time.Time) {
	t.Helper()
	if err := soul.UpdateSoulprint(context.Background(), integrationPool, sid, factsJSON, collectedAt, receivedAt); err != nil {
		t.Fatalf("setSoulprint(%s): %v", sid, err)
	}
}

func TestIntegration_LoadIncarnationHosts_ByCovenAndStatus(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{
		"hosts": []map[string]any{
			{"sid": "a.example.com", "role": "master"},
			{"sid": "b.example.com", "role": "replica"},
		},
	})

	// nil-lease resolver → SQL-presence fallback: connected — pass through;
	// pending/destroyed cut by SQL phase, disconnected — fallback filter
	// (status='connected'). Lease-aware presence — in TestIntegration_LeaseAware_*.
	seedSoul(t, "a.example.com", []string{"db"}, soul.StatusConnected)
	seedSoul(t, "b.example.com", nil, soul.StatusConnected)
	seedSoul(t, "pending.example.com", nil, soul.StatusPending)
	seedSoul(t, "disc.example.com", nil, soul.StatusDisconnected)
	seedSoul(t, "destroyed.example.com", nil, soul.StatusDestroyed)
	seedMembership(t, "redis-prod", "a.example.com", "b.example.com", "pending.example.com", "disc.example.com", "destroyed.example.com")

	now := time.Now().UTC()
	setSoulprint(t, "a.example.com",
		[]byte(`{"os":{"family":"debian"},"hostname":"a"}`),
		now.Add(-time.Minute), now.Add(-time.Minute))

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}

	if len(hosts) != 2 {
		t.Fatalf("len(hosts) = %d, want 2 (only connected)", len(hosts))
	}
	// ORDER BY sid: a, b.
	if hosts[0].SID != "a.example.com" || hosts[1].SID != "b.example.com" {
		t.Fatalf("order = %v, want [a b] sorted by SID", []string{hosts[0].SID, hosts[1].SID})
	}
	if hosts[0].Role != "master" || hosts[1].Role != "replica" {
		t.Errorf("roles = %q/%q, want master/replica", hosts[0].Role, hosts[1].Role)
	}
	if hosts[0].Soulprint == nil || hosts[0].Soulprint["hostname"] != "a" {
		t.Errorf("host[0] soulprint = %v", hosts[0].Soulprint)
	}
	if hosts[1].Soulprint != nil {
		t.Errorf("host[1] soulprint = %v, want nil (no SoulprintReport)", hosts[1].Soulprint)
	}
}

// TestIntegration_LoadIncarnationHosts_Traits — GUARD (ADR-060): resolver
// pulls operator-set traits (scalar + list) from `souls.traits` into HostFacts.Traits
// via rosterSQL SELECT+scan, symmetric to coven. This is the source of projection
// `soulprint.self.traits` for `where:` targeting.
func TestIntegration_LoadIncarnationHosts_Traits(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{})

	// Seed soul with traits directly via soul.Insert (pilot write path).
	s := &soul.Soul{
		SID:    "a.example.com",
		Status: soul.StatusConnected,
		Traits: map[string]any{
			"namespace": "dba-ns",
			"owners":    []any{"alice", "bob"},
		},
	}
	if err := soul.Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("seedSoul with traits: %v", err)
	}
	// Host without traits — Traits read as empty map (jsonb '{}').
	seedSoul(t, "b.example.com", nil, soul.StatusConnected)
	seedMembership(t, "redis-prod", "a.example.com", "b.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("len(hosts) = %d, want 2", len(hosts))
	}
	// ORDER BY sid: a, b.
	if hosts[0].Traits["namespace"] != "dba-ns" {
		t.Errorf("a.Traits[namespace] = %v, want dba-ns", hosts[0].Traits["namespace"])
	}
	owners, ok := hosts[0].Traits["owners"].([]any)
	if !ok || len(owners) != 2 || owners[0] != "alice" {
		t.Errorf("a.Traits[owners] = %v, want [alice bob]", hosts[0].Traits["owners"])
	}
	if hosts[1].Traits == nil || len(hosts[1].Traits) != 0 {
		t.Errorf("b.Traits = %v, want empty map (no traits)", hosts[1].Traits)
	}
}

func TestIntegration_LoadIncarnationHosts_CrossIncarnationIsolation(t *testing.T) {
	// ADR-008 / PM-decision #4: hosts of another incarnation are NOT read.
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedIncarnation(t, "redis-staging", map[string]any{})

	seedSoul(t, "prod-1.example.com", nil, soul.StatusConnected)
	seedSoul(t, "stg-1.example.com", nil, soul.StatusConnected)
	seedMembership(t, "redis-prod", "prod-1.example.com")
	seedMembership(t, "redis-staging", "stg-1.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "prod-1.example.com" {
		t.Fatalf("got %v, want only [prod-1.example.com]", sids(hosts))
	}
}

// TestIntegration_LoadIncarnationHosts_CovenNameIsNotMembership — GUARD
// (NIM-124): a host carrying a coven literally equal to the incarnation name but
// NOT in incarnation_membership is NOT in the roster; a member with no such coven
// IS. Membership is the relation, not a coven value.
func TestIntegration_LoadIncarnationHosts_CovenNameIsNotMembership(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{})
	// Carries "redis-prod" as an ordinary coven tag, but is NOT a member.
	seedSoul(t, "impostor.example.com", []string{"redis-prod"}, soul.StatusConnected)
	// A real member with no name-coven.
	seedSoul(t, "member.example.com", []string{"db"}, soul.StatusConnected)
	seedMembership(t, "redis-prod", "member.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "member.example.com" {
		t.Fatalf("got %v, want only [member.example.com] (coven==name is NOT membership)", sids(hosts))
	}
}

func TestIntegration_LoadIncarnationHosts_MissingIncarnation_Empty(t *testing.T) {
	// PM-decision #3: nonexistent incarnation → empty slice, not error.
	resetAll(t)
	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("len(hosts) = %d, want 0", len(hosts))
	}
}

func TestIntegration_LoadIncarnationHosts_UndeclaredHostRoleEmpty(t *testing.T) {
	// ADR-008: host outside declared-spec → role "".
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{
		"hosts": []map[string]any{{"sid": "declared.example.com", "role": "master"}},
	})
	seedSoul(t, "declared.example.com", nil, soul.StatusConnected)
	seedSoul(t, "extra.example.com", nil, soul.StatusConnected)
	seedMembership(t, "redis-prod", "declared.example.com", "extra.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	roles := map[string]string{}
	for _, h := range hosts {
		roles[h.SID] = h.Role
	}
	if roles["declared.example.com"] != "master" {
		t.Errorf("declared role = %q, want master", roles["declared.example.com"])
	}
	if roles["extra.example.com"] != "" {
		t.Errorf("undeclared role = %q, want empty", roles["extra.example.com"])
	}
}

func TestIntegration_FilterByCovens(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "a.example.com", []string{"db"}, soul.StatusConnected)
	seedSoul(t, "b.example.com", []string{"cache"}, soul.StatusConnected)
	seedMembership(t, "redis-prod", "a.example.com", "b.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}

	filtered := r.FilterByCovens(hosts, []string{"db"})
	if len(filtered) != 1 || filtered[0].SID != "a.example.com" {
		t.Errorf("FilterByCovens([db]) = %v, want [a]", sids(filtered))
	}
}

// TestIntegration_FilterByCovens_MultiLabelAND verifies AND semantics for
// multi-label filtering over a real PG roster query ([ADR-040] amendment
// 2026-05-27; orchestration.md §3). The host must carry ALL listed labels;
// host{prod}+host{eu} with filter [prod, eu] is empty (previously OR would
// return both).
func TestIntegration_FilterByCovens_MultiLabelAND(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "prod-only.example.com", []string{"prod"}, soul.StatusConnected)
	seedSoul(t, "eu-only.example.com", []string{"eu"}, soul.StatusConnected)
	seedSoul(t, "prod-eu.example.com", []string{"prod", "eu"}, soul.StatusConnected)
	seedSoul(t, "prod-us.example.com", []string{"prod", "us"}, soul.StatusConnected)
	seedSoul(t, "neither.example.com", []string{"cache"}, soul.StatusConnected)
	seedMembership(t, "redis-prod", "prod-only.example.com", "eu-only.example.com", "prod-eu.example.com", "prod-us.example.com", "neither.example.com")

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 5 {
		t.Fatalf("roster len = %d, want 5", len(hosts))
	}

	// [prod, eu] is an intersection; only prod-eu.example.com carries both labels.
	filtered := r.FilterByCovens(hosts, []string{"prod", "eu"})
	if len(filtered) != 1 || filtered[0].SID != "prod-eu.example.com" {
		t.Errorf("FilterByCovens([prod, eu]) = %v, want [prod-eu.example.com] (AND)", sids(filtered))
	}

	// [prod] is single-label AND, equivalent to OR on one label: prod-only + prod-eu + prod-us.
	filteredSingle := r.FilterByCovens(hosts, []string{"prod"})
	if len(filteredSingle) != 3 {
		t.Errorf("FilterByCovens([prod]) len = %d, want 3", len(filteredSingle))
	}

	// [prod, missing] has one label absent from all hosts, so the result is empty.
	filteredEmpty := r.FilterByCovens(hosts, []string{"prod", "missing"})
	if len(filteredEmpty) != 0 {
		t.Errorf("FilterByCovens([prod, missing]) = %v, want empty (AND fail-closed)", sids(filteredEmpty))
	}
}

// --- lease-aware presence (PG + Redis) --------------------------------

// integrationLeaseChecker — wrapper over real keeperredis.Client for
// [SoulLeaseChecker] (batch SID-lease EXISTS). Mirror of topologyLeaseChecker from
// cmd/keeper, locally in test — topology doesn't import cmd/keeper.
type integrationLeaseChecker struct{ rc *keeperredis.Client }

func (c integrationLeaseChecker) SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error) {
	return keeperredis.SoulsStreamAlive(ctx, c.rc, sids)
}

// newLeaseChecker spins up miniredis + keeperredis.Client and returns
// checker + mr (for direct setting/clearing of lease keys) + cleanup.
func newLeaseChecker(t *testing.T) (integrationLeaseChecker, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc, err := keeperredis.NewClient(context.Background(), keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("redis NewClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	return integrationLeaseChecker{rc: rc}, mr
}

// setLease/clearLease — setting/clearing SID-lease key `soul:<sid>:lock`
// directly in miniredis (emulation of live/dead EventStream).
func setLease(t *testing.T, mr *miniredis.Miniredis, sid string) {
	t.Helper()
	if err := mr.Set(keeperredis.SoulLeaseKey(sid), "kid-test"); err != nil {
		t.Fatalf("setLease(%s): %v", sid, err)
	}
}

func clearLease(mr *miniredis.Miniredis, sid string) {
	mr.Del(keeperredis.SoulLeaseKey(sid))
}

// TestIntegration_LeaseAware_PresenceFromLeaseNotStatus — presence invariant:
// online ⇔ live SID-lease, NOT snapshot `souls.status`. disconnected snapshot with
// live lease (idle Soul, reconnect not reflected in PG) is targeted;
// connected snapshot without lease (stale) — is not.
func TestIntegration_LeaseAware_PresenceFromLeaseNotStatus(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	lease, mr := newLeaseChecker(t)

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "idle.example.com", nil, soul.StatusDisconnected)
	seedSoul(t, "stale.example.com", nil, soul.StatusConnected)
	seedMembership(t, "redis-prod", "idle.example.com", "stale.example.com")
	setLease(t, mr, "idle.example.com") // live stream, but PG snapshot is disconnected

	r := NewResolver(integrationPool, lease, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "idle.example.com" {
		t.Fatalf("got %v, want [idle.example.com] (presence = lease, not status)", sids(hosts))
	}
}

// TestIntegration_LeaseAware_ReconnectRetargets — reconnect: lease dropped
// (Soul offline) → not targeted; lease retaken → targeted again.
func TestIntegration_LeaseAware_ReconnectRetargets(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	lease, mr := newLeaseChecker(t)

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "host.example.com", nil, soul.StatusConnected)
	seedMembership(t, "redis-prod", "host.example.com")
	r := NewResolver(integrationPool, lease, nil)

	// No lease -> offline.
	if hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod"); err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	} else if len(hosts) != 0 {
		t.Fatalf("no-lease: got %v, want [] (offline)", sids(hosts))
	}

	// Lease acquired -> online.
	setLease(t, mr, "host.example.com")
	if hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod"); err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	} else if len(hosts) != 1 {
		t.Fatalf("with-lease: got %v, want [host] (online)", sids(hosts))
	}

	// Lease removed (stream dropped) -> offline again.
	clearLease(mr, "host.example.com")
	if hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod"); err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	} else if len(hosts) != 0 {
		t.Fatalf("lease-dropped: got %v, want [] (offline)", sids(hosts))
	}

	// Reconnect: lease reacquired -> online again.
	setLease(t, mr, "host.example.com")
	if hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod"); err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	} else if len(hosts) != 1 {
		t.Fatalf("reconnect: got %v, want [host] (online again)", sids(hosts))
	}
}

// TestIntegration_LeaseAware_IdleSoulStaysTargetable verifies that an idle Soul
// (lease is extended by renewal, but without app traffic) stays online:
// presence is lease, not PG `last_seen_at`. This emulates exactly the lease
// without any PG update.
func TestIntegration_LeaseAware_IdleSoulStaysTargetable(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	lease, mr := newLeaseChecker(t)

	seedIncarnation(t, "redis-prod", map[string]any{})
	// disconnected snapshot + stale last_seen: neither status nor last_seen makes
	// it online; only a live lease does.
	seedSoul(t, "idle.example.com", nil, soul.StatusDisconnected)
	seedMembership(t, "redis-prod", "idle.example.com")
	setLease(t, mr, "idle.example.com")

	r := NewResolver(integrationPool, lease, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "idle.example.com" {
		t.Fatalf("got %v, want [idle.example.com] (idle on live lease stays online)", sids(hosts))
	}
}

// TestIntegration_LeaseAware_TerminalNotCandidate verifies that terminal status
// (revoked) is excluded by phase-1 SQL even with a live lease (revoked Soul
// cannot be targeted regardless of stream state).
func TestIntegration_LeaseAware_TerminalNotCandidate(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	lease, mr := newLeaseChecker(t)

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "revoked.example.com", nil, soul.StatusRevoked)
	seedSoul(t, "ok.example.com", nil, soul.StatusConnected)
	seedMembership(t, "redis-prod", "revoked.example.com", "ok.example.com")
	setLease(t, mr, "revoked.example.com") // not a candidate even with a live lease
	setLease(t, mr, "ok.example.com")

	r := NewResolver(integrationPool, lease, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "ok.example.com" {
		t.Fatalf("got %v, want [ok.example.com] (revoked excluded by SQL phase)", sids(hosts))
	}
}
