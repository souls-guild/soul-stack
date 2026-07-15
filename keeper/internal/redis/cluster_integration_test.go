//go:build integration

// Integration tests for the Redis client's sentinel/cluster topologies and
// cluster blockers (ADR-006 amendment). Unlike integration_test.go (the
// standalone redis:7 from the shared TestMain), these tests spin up their
// OWN containers inside the test functions — they don't need the standard
// standalone container.
//
// Run (like the package's other integration suites):
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 \
//	    -run 'Cluster|Sentinel' ./internal/redis/...
//
// Images:
//   - cluster: grokzen/redis-cluster:7.0.10 (3 masters + 3 replicas in one
//     container; IP=0.0.0.0 makes nodes announce 127.0.0.1, otherwise a
//     client on the host can't reach the nodes' internal IPs — the classic
//     testcontainers NAT pain).
//   - sentinel: bitnami/redis (master) + bitnami/redis-sentinel on a shared
//     docker network; the master is announced by container name, and
//     sentinel hands that name to the client — so the client also connects
//     FROM INSIDE the network (sidecar redis-cli exec), not from the host.
//     Host-NAT for sentinel-announced addresses isn't solvable without
//     host-networking, so password-resolve/CROSSSLOT guards are concentrated
//     in the cluster suite, while the sentinel suite proves mode selection +
//     failover re-pointing via exec inside the network.
//
// If containers/cluster don't come up in the sandbox, the test self-skips
// (like vault/redis integration), but fails under
// SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 (doesn't mask a non-run).

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

// startCluster brings up grokzen/redis-cluster and returns host:port of one
// of the seed nodes (port 7000). IP=0.0.0.0 → nodes announce 127.0.0.1;
// host ports map 1:1 to 7000..7005 so the announced addresses are reachable.
func startCluster(ctx context.Context, t *testing.T) (seedAddr string, terminate func()) {
	t.Helper()

	// Pin host-port = container-port for each node: grokzen announces
	// 127.0.0.1:<contport>, and the go-redis ClusterClient dials those addresses.
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

// newClusterClient connects to the running cluster; allows time for gossip
// convergence (grokzen sometimes reports Ready before slots fully form).
func newClusterClient(ctx context.Context, t *testing.T, seed string) *Client {
	t.Helper()
	var lastErr error
	for i := 0; i < 20; i++ {
		c, err := NewClient(ctx, Config{Mode: ModeCluster, Nodes: []string{seed}}, nil)
		if err == nil {
			// Additionally wait until the slot map has formed (any SET succeeds).
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

// TestIntegration_Cluster_SingleKeyLease — single-key Lua-lease is
// cluster-safe (one KEYS → one slot, CROSSSLOT impossible).
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

// TestIntegration_Cluster_HeraldNoCrossSlot — BLOCKER 1 guard: BRPOPLPUSH
// pending→processing does not trigger CROSSSLOT, because all queue keys sit
// under a shared hash-tag `{q}` (= one slot). Also cross-checks CLUSTER KEYSLOT.
func TestIntegration_Cluster_HeraldNoCrossSlot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	seed, term := startCluster(ctx, t)
	defer term()

	c := newClusterClient(ctx, t, seed)
	defer c.Close()

	// Check: pending and processing land in ONE slot (hash-tag {q}).
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

	// A real BRPOPLPUSH via Claim — without the hash-tag this would be CROSSSLOT.
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
		// This is exactly where CROSSSLOT would show up if the keys were in different slots.
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

// TestIntegration_Cluster_CountLiveCrossNode — BLOCKER 2 guard: presence keys
// `keeper:instance:<kid>` for different KIDs land in DIFFERENT slots (=
// different master nodes); CountLive must see ALL of them via per-master
// SCAN (ForEachMaster). Without the fix, a plain SCAN would cover only one
// node and undercount.
func TestIntegration_Cluster_CountLiveCrossNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	seed, term := startCluster(ctx, t)
	defer term()

	c := newClusterClient(ctx, t, seed)
	defer c.Close()

	// Pick KIDs so presence keys land on different slots/nodes.
	// 12 distinct KIDs practically guarantee coverage of all 3 master nodes.
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

// TestIntegration_Cluster_PubSubCrossNode — classic broadcast pub/sub is
// delivered on a cluster (ADR-006: sharded SPUBLISH is a separate GA slice;
// here we verify that plain PUBLISH/SUBSCRIBE works on a ClusterClient).
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
	if _, err := sub.Receive(ctx); err != nil { // subscribe confirmation
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

// TestIntegration_Cluster_PasswordFromVault — the cluster password is
// resolved from a REAL Vault (KV v2) through the keeper-vault client. grokzen
// has no password, so the resolve itself is verified separately from the
// connect: keeper-vault.ReadKV returns the password, resolvePassword extracts
// it. (The "resolved password → AUTH" link is covered by the unit test
// TestNewClient_VaultRef_Resolved against miniredis.)
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

	// `#field` override against a real Vault.
	gotF, err := resolvePassword(ctx, vc, "vault:secret/keeper/redis#rotated")
	if err != nil {
		t.Fatalf("resolvePassword #field from real Vault: %v", err)
	}
	if gotF != "cluster-pw-rotated" {
		t.Errorf("resolved #rotated = %q, want cluster-pw-rotated", gotF)
	}
}

// startVaultWithRedisSecret brings up a Vault dev server and seeds
// secret/keeper/redis with password + rotated fields. Returns the
// keeper-vault client (it satisfies passwordResolver).
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
	// The dev Vault mounts `secret/` as KV v2 (like vault/integration_test.go).
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

// nat7000to7005 returns PortBindings that pin host-port = container-port for
// each cluster node (needed so the announced 127.0.0.1:<port> is reachable
// from the host).
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
