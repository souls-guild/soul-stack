//go:build integration

// Integration-тесты topology-резолвера через testcontainers-go
// (postgres:16-alpine). Паттерн совпадает с
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

	// nil-lease резолвер → SQL-presence fallback: connected — попадают;
	// pending/destroyed отсекает SQL-фаза, disconnected — fallback-фильтр
	// (status='connected'). Lease-aware presence — в TestIntegration_LeaseAware_*.
	seedSoul(t, "a.example.com", []string{"redis-prod", "db"}, soul.StatusConnected)
	seedSoul(t, "b.example.com", []string{"redis-prod"}, soul.StatusConnected)
	seedSoul(t, "pending.example.com", []string{"redis-prod"}, soul.StatusPending)
	seedSoul(t, "disc.example.com", []string{"redis-prod"}, soul.StatusDisconnected)
	seedSoul(t, "destroyed.example.com", []string{"redis-prod"}, soul.StatusDestroyed)

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

func TestIntegration_LoadIncarnationHosts_CrossIncarnationIsolation(t *testing.T) {
	// ADR-008 / PM-decision #4: хосты другой incarnation НЕ читаются.
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedIncarnation(t, "redis-staging", map[string]any{})

	seedSoul(t, "prod-1.example.com", []string{"redis-prod"}, soul.StatusConnected)
	seedSoul(t, "stg-1.example.com", []string{"redis-staging"}, soul.StatusConnected)

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "prod-1.example.com" {
		t.Fatalf("got %v, want only [prod-1.example.com]", sids(hosts))
	}
}

func TestIntegration_LoadIncarnationHosts_MissingIncarnation_Empty(t *testing.T) {
	// PM-decision #3: несуществующая incarnation → пустой slice, не ошибка.
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
	// ADR-008: хост вне declared-spec → role "".
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{
		"hosts": []map[string]any{{"sid": "declared.example.com", "role": "master"}},
	})
	seedSoul(t, "declared.example.com", []string{"redis-prod"}, soul.StatusConnected)
	seedSoul(t, "extra.example.com", []string{"redis-prod"}, soul.StatusConnected)

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
	seedSoul(t, "a.example.com", []string{"redis-prod", "db"}, soul.StatusConnected)
	seedSoul(t, "b.example.com", []string{"redis-prod", "cache"}, soul.StatusConnected)

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

// TestIntegration_FilterByCovens_MultiLabelAND проверяет AND-семантику
// multi-label фильтра поверх реальной PG-roster-выборки ([ADR-040] amendment
// 2026-05-27; orchestration.md §3). Хост должен нести ВСЕ перечисленные метки;
// host{prod}+host{eu} с filter [prod, eu] — пусто (раньше OR давал бы оба).
func TestIntegration_FilterByCovens_MultiLabelAND(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "prod-only.example.com", []string{"redis-prod", "prod"}, soul.StatusConnected)
	seedSoul(t, "eu-only.example.com", []string{"redis-prod", "eu"}, soul.StatusConnected)
	seedSoul(t, "prod-eu.example.com", []string{"redis-prod", "prod", "eu"}, soul.StatusConnected)
	seedSoul(t, "prod-us.example.com", []string{"redis-prod", "prod", "us"}, soul.StatusConnected)
	seedSoul(t, "neither.example.com", []string{"redis-prod", "cache"}, soul.StatusConnected)

	r := NewResolver(integrationPool, nil, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 5 {
		t.Fatalf("roster len = %d, want 5", len(hosts))
	}

	// [prod, eu] — пересечение, только prod-eu.example.com несёт обе метки.
	filtered := r.FilterByCovens(hosts, []string{"prod", "eu"})
	if len(filtered) != 1 || filtered[0].SID != "prod-eu.example.com" {
		t.Errorf("FilterByCovens([prod, eu]) = %v, want [prod-eu.example.com] (AND)", sids(filtered))
	}

	// [prod] — single-label AND ≡ OR на single-label: prod-only + prod-eu + prod-us.
	filteredSingle := r.FilterByCovens(hosts, []string{"prod"})
	if len(filteredSingle) != 3 {
		t.Errorf("FilterByCovens([prod]) len = %d, want 3", len(filteredSingle))
	}

	// [prod, missing] — одна метка отсутствует у всех → пусто.
	filteredEmpty := r.FilterByCovens(hosts, []string{"prod", "missing"})
	if len(filteredEmpty) != 0 {
		t.Errorf("FilterByCovens([prod, missing]) = %v, want empty (AND fail-closed)", sids(filteredEmpty))
	}
}

// --- lease-aware presence (PG + Redis) --------------------------------

// integrationLeaseChecker — обёртка над real keeperredis.Client под
// [SoulLeaseChecker] (batch SID-lease EXISTS). Зеркало topologyLeaseChecker из
// cmd/keeper, локально в тесте — топология не импортит cmd/keeper.
type integrationLeaseChecker struct{ rc *keeperredis.Client }

func (c integrationLeaseChecker) SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error) {
	return keeperredis.SoulsStreamAlive(ctx, c.rc, sids)
}

// newLeaseChecker поднимает miniredis + keeperredis.Client и возвращает
// checker + mr (для прямой постановки/снятия lease-ключей) + cleanup.
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

// setLease/clearLease — постановка/снятие SID-lease-ключа `soul:<sid>:lock`
// напрямую в miniredis (эмуляция живого/мёртвого EventStream-а).
func setLease(t *testing.T, mr *miniredis.Miniredis, sid string) {
	t.Helper()
	if err := mr.Set(keeperredis.SoulLeaseKey(sid), "kid-test"); err != nil {
		t.Fatalf("setLease(%s): %v", sid, err)
	}
}

func clearLease(mr *miniredis.Miniredis, sid string) {
	mr.Del(keeperredis.SoulLeaseKey(sid))
}

// TestIntegration_LeaseAware_PresenceFromLeaseNotStatus — presence-инвариант:
// online ⇔ живой SID-lease, НЕ снимок `souls.status`. disconnected-снимок с
// живым lease (idle-Soul, reconnect не отразился в PG) таргетируется;
// connected-снимок без lease (stale) — нет.
func TestIntegration_LeaseAware_PresenceFromLeaseNotStatus(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	lease, mr := newLeaseChecker(t)

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "idle.example.com", []string{"redis-prod"}, soul.StatusDisconnected)
	seedSoul(t, "stale.example.com", []string{"redis-prod"}, soul.StatusConnected)
	setLease(t, mr, "idle.example.com") // живой стрим, но PG-снимок disconnected

	r := NewResolver(integrationPool, lease, nil)
	hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "idle.example.com" {
		t.Fatalf("got %v, want [idle.example.com] (presence = lease, not status)", sids(hosts))
	}
}

// TestIntegration_LeaseAware_ReconnectRetargets — reconnect: lease снят
// (Soul offline) → не таргетируется; lease перевзят → снова таргетируется.
func TestIntegration_LeaseAware_ReconnectRetargets(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	lease, mr := newLeaseChecker(t)

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "host.example.com", []string{"redis-prod"}, soul.StatusConnected)
	r := NewResolver(integrationPool, lease, nil)

	// Нет lease → offline.
	if hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod"); err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	} else if len(hosts) != 0 {
		t.Fatalf("no-lease: got %v, want [] (offline)", sids(hosts))
	}

	// Lease взят → online.
	setLease(t, mr, "host.example.com")
	if hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod"); err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	} else if len(hosts) != 1 {
		t.Fatalf("with-lease: got %v, want [host] (online)", sids(hosts))
	}

	// Lease снят (стрим упал) → снова offline.
	clearLease(mr, "host.example.com")
	if hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod"); err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	} else if len(hosts) != 0 {
		t.Fatalf("lease-dropped: got %v, want [] (offline)", sids(hosts))
	}

	// Reconnect — lease перевзят → снова online.
	setLease(t, mr, "host.example.com")
	if hosts, err := r.LoadIncarnationHosts(ctx, "redis-prod"); err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	} else if len(hosts) != 1 {
		t.Fatalf("reconnect: got %v, want [host] (online again)", sids(hosts))
	}
}

// TestIntegration_LeaseAware_IdleSoulStaysTargetable — idle-Soul (lease
// продлевается renewal-ом, но без app-трафика) остаётся online: presence — lease,
// не PG `last_seen_at`. Эмулируем именно lease без какого-либо PG-обновления.
func TestIntegration_LeaseAware_IdleSoulStaysTargetable(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	lease, mr := newLeaseChecker(t)

	seedIncarnation(t, "redis-prod", map[string]any{})
	// disconnected-снимок + stale last_seen: ни status, ни last_seen не делают
	// его online; только живой lease.
	seedSoul(t, "idle.example.com", []string{"redis-prod"}, soul.StatusDisconnected)
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

// TestIntegration_LeaseAware_TerminalNotCandidate — terminal-status (revoked)
// исключается ещё фазой-1 SQL, даже при живом lease (нельзя таргетить
// отозванный Soul независимо от стрима).
func TestIntegration_LeaseAware_TerminalNotCandidate(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	lease, mr := newLeaseChecker(t)

	seedIncarnation(t, "redis-prod", map[string]any{})
	seedSoul(t, "revoked.example.com", []string{"redis-prod"}, soul.StatusRevoked)
	seedSoul(t, "ok.example.com", []string{"redis-prod"}, soul.StatusConnected)
	setLease(t, mr, "revoked.example.com") // даже с живым lease — не кандидат
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
