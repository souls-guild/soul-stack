//go:build integration

// Integration-тесты sentinel/cluster-топологий Redis-клиента и cluster-
// блокеров (ADR-006 amendment). В отличие от integration_test.go (standalone
// redis:7 из общего TestMain), эти тесты поднимают СВОИ контейнеры внутри
// тест-функций — стандартный standalone-контейнер им не нужен.
//
// Запуск (как и прочие integration-наборы пакета):
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 \
//	    -run 'Cluster|Sentinel' ./internal/redis/...
//
// Образы:
//   - cluster:  grokzen/redis-cluster:7.0.10 (3 master + 3 replica в одном
//     контейнере; IP=0.0.0.0 заставляет узлы анонсировать 127.0.0.1, иначе
//     клиент с хоста не достучится до внутренних IP узлов — классический
//     testcontainers NAT-pain).
//   - sentinel: bitnami/redis (master) + bitnami/redis-sentinel в общей docker-
//     сети; master анонсируется по имени контейнера, sentinel отдаёт клиенту
//     это имя — поэтому клиент тоже подключается ВНУТРИ сети (sidecar-redis-cli
//     exec), а не с хоста. host-NAT для sentinel-announced-адресов не решается
//     без host-networking, поэтому пароль-резолв/CROSSSLOT-guard-и сосредоточены
//     в cluster-наборе, а sentinel-набор доказывает выбор режима + failover-
//     перенаведение через exec внутри сети.
//
// Если контейнеры/кластер в песочнице не поднимаются — тест self-skip-ается
// (как vault/redis integration), но при SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1
// падает (не маскирует непрогон).

package redis

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	dockercontainer "github.com/moby/moby/api/types/container"
	dockernetwork "github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"
	"github.com/testcontainers/testcontainers-go/wait"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	clusterImage    = "grokzen/redis-cluster:7.0.10"
	vaultClusterTok = "root"
	vaultClusterImg = "hashicorp/vault:1.18"
)

// startCluster поднимает grokzen/redis-cluster и возвращает host:port одного из
// seed-узлов (port 7000). IP=0.0.0.0 → узлы анонсируют 127.0.0.1; host-порты
// мапятся 1:1 на 7000..7005, чтобы анонсированные адреса были достижимы.
func startCluster(ctx context.Context, t *testing.T) (seedAddr string, terminate func()) {
	t.Helper()

	// Фиксируем host-порт = контейнер-порт для каждого узла: grokzen анонсирует
	// 127.0.0.1:<contport>, и go-redis ClusterClient идёт по этим адресам.
	portBindings := func(hc *dockercontainer.HostConfig) {
		hc.PortBindings = nat7000to7005()
	}

	req := testcontainers.ContainerRequest{
		Image:              clusterImage,
		ExposedPorts:       []string{"7000/tcp", "7001/tcp", "7002/tcp", "7003/tcp", "7004/tcp", "7005/tcp"},
		Env:                map[string]string{"IP": "0.0.0.0", "INITIAL_PORT": "7000", "MASTERS": "3", "SLAVES_PER_MASTER": "1"},
		HostConfigModifier: portBindings,
		WaitingFor: wait.ForLog("Ready to accept connections").
			WithStartupTimeout(90 * time.Second),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		if requireDocker() {
			t.Fatalf("cluster integration: container setup failed (REQUIRE_DOCKER): %v", err)
		}
		t.Skipf("cluster integration: skipping, cluster container unavailable: %v", err)
	}
	terminate = func() {
		tctx, tc := context.WithTimeout(context.Background(), 30*time.Second)
		defer tc()
		_ = ctr.Terminate(tctx)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		terminate()
		t.Fatalf("cluster Host: %v", err)
	}
	return fmt.Sprintf("%s:7000", host), terminate
}

// newClusterClient подключается к поднятому кластеру; даёт время на gossip-
// сходимость (grokzen иногда репортит Ready до полного формирования слотов).
func newClusterClient(ctx context.Context, t *testing.T, seed string) *Client {
	t.Helper()
	var lastErr error
	for i := 0; i < 20; i++ {
		c, err := NewClient(ctx, Config{Mode: ModeCluster, Nodes: []string{seed}}, nil)
		if err == nil {
			// Дополнительно дождёмся, что slot-карта сформирована (любой SET проходит).
			if perr := c.underlying().Set(ctx, "cluster:warmup", "1", time.Minute).Err(); perr == nil {
				return c
			} else {
				lastErr = perr
				_ = c.Close()
			}
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	if requireDocker() {
		t.Fatalf("cluster integration: client did not converge (REQUIRE_DOCKER): %v", lastErr)
	}
	t.Skipf("cluster integration: cluster did not converge: %v", lastErr)
	return nil
}

// TestIntegration_Cluster_SingleKeyLease — single-key Lua-lease cluster-safe
// (один KEYS → один слот, CROSSSLOT невозможен).
func TestIntegration_Cluster_SingleKeyLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	seed, term := startCluster(ctx, t)
	defer term()

	c := newClusterClient(ctx, t, seed)
	defer c.Close()

	key := "reaper:leader:cluster-test"
	l, err := Acquire(ctx, c, key, "keeper-cl-a", 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire on cluster: %v", err)
	}
	if err := l.Renew(ctx); err != nil {
		t.Errorf("Renew on cluster: %v", err)
	}
	if err := l.Release(ctx); err != nil {
		t.Errorf("Release on cluster: %v", err)
	}
}

// TestIntegration_Cluster_HeraldNoCrossSlot — БЛОКЕР 1 guard: BRPOPLPUSH
// pending→processing не даёт CROSSSLOT, потому что все ключи очереди — под
// общим hash-tag `{q}` (= один слот). Дополнительно сверяем CLUSTER KEYSLOT.
func TestIntegration_Cluster_HeraldNoCrossSlot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	seed, term := startCluster(ctx, t)
	defer term()

	c := newClusterClient(ctx, t, seed)
	defer c.Close()

	// Проверка: pending и processing садятся в ОДИН слот (hash-tag {q}).
	slotPending, err := c.underlying().ClusterKeySlot(ctx, heraldPendingKey).Result()
	if err != nil {
		t.Fatalf("CLUSTER KEYSLOT pending: %v", err)
	}
	slotProcessing, err := c.underlying().ClusterKeySlot(ctx, heraldProcessingKey).Result()
	if err != nil {
		t.Fatalf("CLUSTER KEYSLOT processing: %v", err)
	}
	slotLease, err := c.underlying().ClusterKeySlot(ctx, heraldLeaseKey("job-1")).Result()
	if err != nil {
		t.Fatalf("CLUSTER KEYSLOT lease: %v", err)
	}
	if slotPending != slotProcessing || slotPending != slotLease {
		t.Fatalf("hash-tag {q} не свёл ключи в один слот: pending=%d processing=%d lease=%d",
			slotPending, slotProcessing, slotLease)
	}

	// Реальный BRPOPLPUSH через Claim — без hash-tag это был бы CROSSSLOT.
	q, err := NewHeraldDeliveryQueue(c)
	if err != nil {
		t.Fatalf("NewHeraldDeliveryQueue: %v", err)
	}
	payload := []byte(`{"job_id":"job-1","to":"x"}`)
	if err := q.Enqueue(ctx, payload); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Claim(ctx, 2*time.Second)
	if err != nil {
		// Именно тут проявился бы CROSSSLOT, если бы ключи были в разных слотах.
		if strings.Contains(strings.ToUpper(err.Error()), "CROSSSLOT") {
			t.Fatalf("БЛОКЕР 1 регрессировал: Claim вернул CROSSSLOT: %v", err)
		}
		t.Fatalf("Claim: %v", err)
	}
	if claimed == nil || string(claimed.Payload) != string(payload) {
		t.Fatalf("Claim payload mismatch: %+v", claimed)
	}
	if err := q.Ack(ctx, "job-1", claimed.Payload); err != nil {
		t.Errorf("Ack: %v", err)
	}
}

// TestIntegration_Cluster_CountLiveCrossNode — БЛОКЕР 2 guard: presence-ключи
// `keeper:instance:<kid>` разных KID садятся в РАЗНЫЕ слоты (= разные master-
// узлы); CountLive обязан увидеть ВСЕХ через per-master SCAN (ForEachMaster).
// Без фикса обычный SCAN обошёл бы один узел и недосчитал.
func TestIntegration_Cluster_CountLiveCrossNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	seed, term := startCluster(ctx, t)
	defer term()

	c := newClusterClient(ctx, t, seed)
	defer c.Close()

	// Подбираем KID-ы так, чтобы presence-ключи легли на разные слоты/узлы.
	// 12 разных KID практически гарантируют покрытие всех 3 master-узлов.
	kids := make([]string, 0, 12)
	slots := map[int64]struct{}{}
	for i := 0; i < 12; i++ {
		kid := fmt.Sprintf("keeper-node-%02d", i)
		kids = append(kids, kid)
		if err := RegisterInstance(ctx, c, kid, "meta", 60*time.Second, false); err != nil {
			t.Fatalf("RegisterInstance %s: %v", kid, err)
		}
		slot, err := c.underlying().ClusterKeySlot(ctx, ConclaveKey(kid)).Result()
		if err != nil {
			t.Fatalf("KEYSLOT %s: %v", kid, err)
		}
		slots[slot] = struct{}{}
	}
	if len(slots) < 2 {
		t.Fatalf("presence-ключи не распределились по слотам (slots=%v) — тест не доказателен", slots)
	}

	got, err := CountLive(ctx, c)
	if err != nil {
		t.Fatalf("CountLive: %v", err)
	}
	if got != len(kids) {
		t.Fatalf("БЛОКЕР 2 guard: CountLive=%d, want %d — per-master SCAN недосчитал cross-node presence", got, len(kids))
	}

	live, err := LiveKIDs(ctx, c)
	if err != nil {
		t.Fatalf("LiveKIDs: %v", err)
	}
	seen := map[string]bool{}
	for _, k := range live {
		seen[k] = true
	}
	for _, k := range kids {
		if !seen[k] {
			t.Errorf("LiveKIDs не вернул %q (cross-node недосчёт)", k)
		}
	}
}

// TestIntegration_Cluster_PubSubCrossNode — классический broadcast pub/sub
// доставляется в cluster (ADR-006: sharded SPUBLISH — отдельный GA-slice, тут
// проверяем, что обычный PUBLISH/SUBSCRIBE работает на ClusterClient).
func TestIntegration_Cluster_PubSubCrossNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	seed, term := startCluster(ctx, t)
	defer term()

	c := newClusterClient(ctx, t, seed)
	defer c.Close()

	const channel = "events:shard:test"
	sub := c.underlying().Subscribe(ctx, channel)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil { // подтверждение подписки
		t.Fatalf("subscribe confirm: %v", err)
	}
	ch := sub.Channel()

	if err := c.underlying().Publish(ctx, channel, "hello-cluster").Err(); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case m := <-ch:
		if m.Payload != "hello-cluster" {
			t.Errorf("payload = %q", m.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pub/sub message not delivered on cluster")
	}
}

// TestIntegration_Cluster_PasswordFromVault — пароль кластера резолвится из
// РЕАЛЬНОГО Vault (KV v2) через keeper-vault-клиент. grokzen без пароля, поэтому
// сам резолв проверяется отдельно от коннекта: keeper-vault.ReadKV отдаёт
// password, resolvePassword извлекает его. (Связку «resolved password → AUTH»
// покрывает unit TestNewClient_VaultRef_Resolved на miniredis.)
func TestIntegration_Cluster_PasswordFromVault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	vc, term := startVaultWithRedisSecret(ctx, t, "cluster-pw")
	defer term()

	got, err := resolvePassword(ctx, vc, "vault:secret/keeper/redis")
	if err != nil {
		t.Fatalf("resolvePassword from real Vault: %v", err)
	}
	if got != "cluster-pw" {
		t.Errorf("resolved password = %q, want cluster-pw", got)
	}

	// `#field`-override против реального Vault.
	gotF, err := resolvePassword(ctx, vc, "vault:secret/keeper/redis#rotated")
	if err != nil {
		t.Fatalf("resolvePassword #field from real Vault: %v", err)
	}
	if gotF != "cluster-pw-rotated" {
		t.Errorf("resolved #rotated = %q, want cluster-pw-rotated", gotF)
	}
}

// startVaultWithRedisSecret поднимает Vault dev и кладёт secret/keeper/redis с
// полями password + rotated. Возвращает keeper-vault-клиент (он удовлетворяет
// passwordResolver).
func startVaultWithRedisSecret(ctx context.Context, t *testing.T, pw string) (*keepervault.Client, func()) {
	t.Helper()
	ctr, err := tcvault.Run(ctx, vaultClusterImg, tcvault.WithToken(vaultClusterTok))
	if err != nil {
		if requireDocker() {
			t.Fatalf("vault setup failed (REQUIRE_DOCKER): %v", err)
		}
		t.Skipf("vault unavailable: %v", err)
	}
	term := func() {
		tctx, tc := context.WithTimeout(context.Background(), 30*time.Second)
		defer tc()
		_ = ctr.Terminate(tctx)
	}
	addr, err := ctr.HttpHostAddress(ctx)
	if err != nil {
		term()
		t.Fatalf("vault HttpHostAddress: %v", err)
	}

	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = addr
	api, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		term()
		t.Fatalf("vaultapi.NewClient: %v", err)
	}
	api.SetToken(vaultClusterTok)
	// dev-Vault поднимает `secret/` как KV v2 (как vault/integration_test.go).
	if _, err := api.KVv2("secret").Put(ctx, "keeper/redis", map[string]any{
		"password": pw,
		"rotated":  pw + "-rotated",
	}); err != nil {
		term()
		t.Fatalf("seed vault secret: KVv2.Put: %v", err)
	}

	vc, err := keepervault.NewClient(ctx, config.KeeperVault{
		Addr:    addr,
		Token:   vaultClusterTok,
		KVMount: "secret",
	})
	if err != nil {
		term()
		t.Fatalf("keepervault.NewClient: %v", err)
	}
	return vc, term
}

// --- helpers ---

// nat7000to7005 возвращает PortBindings, фиксирующие host-порт = контейнер-порт
// для каждого cluster-узла (нужно, чтобы анонсированный 127.0.0.1:<port> был
// достижим с хоста).
func nat7000to7005() dockernetwork.PortMap {
	m := dockernetwork.PortMap{}
	for p := 7000; p <= 7005; p++ {
		cp := dockernetwork.MustParsePort(fmt.Sprintf("%d/tcp", p))
		m[cp] = []dockernetwork.PortBinding{{
			HostIP:   netip.MustParseAddr("127.0.0.1"),
			HostPort: fmt.Sprintf("%d", p),
		}}
	}
	return m
}
