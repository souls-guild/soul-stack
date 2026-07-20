//go:build e2e_live

// L3c live verification: remove-node slot migration is LOSSLESS on REAL Redis
// Cluster (NOT L0 fake). Closes trust gap of MAJOR fix from 2026-06-22:
// community.redis remove-node moved slot keys through CLUSTER GETKEYSINSLOT ->
// stringification (join with space) -> strings.Fields. Redis key is arbitrary
// byte string and may contain space/\t/\n; "user 42" was split into two tokens ->
// MIGRATE over nonexistent keys -> key was NOT moved, while SETSLOT NODE still
// handed off the slot -> DATA LOSS. Fix is typed redisConn.GetKeysInSlot ([]string)
// over go-redis ClusterGetKeysInSlot.
//
// L0-fake (cluster_test.go::TestApplyClusterRemoveNode_WhitespaceKeysLossless)
// proves ONLY command sequence: that "user 42" goes into MIGRATE as a single KEYS
// argument. It does NOT prove losslessness of REAL data - that after remove-node,
// the key is physically available on the new owner and DBSIZE matches. That is
// L3c's job (live Redis Cluster, independent verify through redis-cli).
//
// INVARIANT (what unblocked test must check):
//  1. Start REAL Redis Cluster through community.redis scenario `create`
//     (examples/service/redis, redis_type=cluster) on soul containers: >=3 masters
//     with slots + >=1 removable master with slots.
//  2. Write N keys into slots of the REMOVED master, MUST include:
//     - key with SPACE in name ("user 42") - exactly the defective case;
//     - key with TTL (PSETEX) - MIGRATE must carry remaining TTL too;
//     - normal key for contrast.
//     Record cluster-wide DBSIZE (sum over masters) BEFORE.
//  3. Run scenario `remove_node` (remove_node_sid = removed master,
//     seed_sid = any remaining one). WaitApplySuccess.
//  4. ASSERT lossless (independent redis-cli, NOT through plugin):
//     - EVERY written key is available (GET / EXISTS) on NEW slot owner
//     (redis-cli -c follows MOVED automatically), including "user 42";
//     - key with TTL preserved TTL (> 0, in a reasonable window);
//     - cluster total DBSIZE AFTER == BEFORE (no lost keys);
//     - removed master is absent from CLUSTER NODES (FORGET converged).
//
// Pre-requisites (why t.Skip remains):
//   - examples/service/redis scenario `create` for redis_type=cluster in L3b-live
//     is not yet proven end-to-end (see redis_cluster_create_test.go::t.Skip:
//     host-variable flow-control in destiny blocks cluster-create live, and
//     community.redis cluster-bootstrap over soul containers is not wrapped by
//     harness yet: no helper starts cluster-mode redis on N containers and waits
//     for cluster_state:ok through plugin).
//   - harness has no helper for writing whitespace/TTL keys into specific slot
//     and no cluster-aware DBSIZE aggregator (redis-cli -c GET following MOVED).
//
// L3c does NOT close this harness infrastructure gap; this is a skeleton with
// t.Skip and clear diagnostics. Real run is a separate slice once (a)
// cluster-create live is unblocked (per-role scenario steps OR per-host
// destiny-dispatch) and (b) harness gets cluster-aware write/verify helpers.
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// remRedisPassword is requirepass redis for remove-node lossless run
// (seeded into Vault scoped vault:-ref, as in redis_cluster_create_test.go).
const remRedisPassword = "remove-node-redis-secret-32b"

// losslessKeys are keys written into slots of removed master BEFORE remove-node.
// "user 42" is EXACTLY the defective case (space in name). Each key must survive
// slot migration losslessly.
var losslessKeys = []string{
	"user 42",   // space in name - defective key-loss case
	"a\tb",      // tab
	"plain-key", // normal - contrast
}

func TestL3cRedisClusterRemoveNode_SlotMigrationLossless(t *testing.T) {
	t.Skip("L3c blocked (harness infra): community.redis cluster-create live is not yet proven end-to-end (see redis_cluster_create_test.go::t.Skip - host-variable flow-control in cluster-create destiny) + no harness helpers to write whitespace/TTL keys into specific slot and compare cluster-aware DBSIZE. Unblock together with cluster-create live (per-role scenario steps OR per-host destiny-dispatch) + cluster-aware write/verify helpers in harness. L0 fake (TestApplyClusterRemoveNode_WhitespaceKeysLossless) proves command order; this test proves real data losslessness.")

	// Skeleton remains for future unblocking.
	// When cluster-create becomes applicable live and harness gets cluster-aware
	// helpers, body below becomes boilerplate.
	const (
		incName   = "redis-remove-node-lossless"
		rcService = "redis"
		rcExample = "examples/service/redis"
		removeSID = "soul-live-c.example.com" // removed master with slots
		seedSID   = "soul-live-a.example.com" // remaining master (contact seed)
	)

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: rcExample,
		ServiceName: rcService,
		Souls:       3,
	})
	defer stack.Cleanup()

	for i := range stack.SoulContainers {
		stack.AddMember(t, i, incName)
		stack.WaitSoulprintReported(t, i, 60)
	}
	stack.MaterializeDestinies(t, "v1.0.0", "redis")
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": remRedisPassword})

	// (1) bootstrap cluster-mode redis through scenario create (redis_type=cluster).
	//     TODO(L3c-future): need helper guaranteeing cluster_state:ok through
	//     community.redis (plugin cluster bootstrap, not redis-cli --cluster create).
	createID := stack.RunScenario(t, incName, "create", map[string]any{
		"redis_type":     "cluster",
		"redis_password": "vault:secret/redis/" + incName + "#password",
	})
	stack.WaitApplySuccess(t, createID, 600)
	stack.WaitIncarnationReady(t, incName, 60)

	// (2) write keys (including "user 42" + TTL key) into slots of REMOVED master
	//     and record total cluster DBSIZE BEFORE.
	//     TODO(L3c-future): cluster-aware write-helper — redis-cli -c -a <pw> SET,
	//     PSETEX for TTL key; address slots specifically on removeSID.
	for _, k := range losslessKeys {
		writeClusterKey(t, stack, seedSID, k, "v-"+k)
	}
	const ttlKey = "session ttl-key" // space + TTL together
	writeClusterKeyWithTTL(t, stack, seedSID, ttlKey, "v-ttl", 600_000)
	dbsizeBefore := clusterDBSize(t, stack)

	// (3) remove_node of removed master.
	removeID := stack.RunScenario(t, incName, "remove_node", map[string]any{
		"remove_node_sid": removeSID,
		"seed_sid":        seedSID,
	})
	stack.WaitApplySuccess(t, removeID, 300)
	stack.WaitIncarnationReady(t, incName, 60)

	// (4) lossless independent verify (redis-cli -c, follows MOVED):
	//     each key is available on new owner; TTL key preserved TTL; DBSIZE
	//     matches; removed master forgotten.
	for _, k := range append(append([]string(nil), losslessKeys...), ttlKey) {
		if !clusterKeyExists(t, stack, seedSID, k) {
			t.Fatalf("key %q LOST after remove-node (slot migration is not lossless)", k)
		}
	}
	assertClusterKeyTTLPositive(t, stack, seedSID, ttlKey)
	if after := clusterDBSize(t, stack); after != dbsizeBefore {
		t.Fatalf("DBSIZE mismatch: before=%d after=%d (lost/duplicated keys)", dbsizeBefore, after)
	}
	assertNodeForgotten(t, stack, seedSID, removeSID)
}

// --- TODO(L3c-future) harness cluster-aware helpers (skeleton stubs) ----------
// Real implementations will appear when unblocked: redis-cli -c -a <pw> inside
// soul container seedSID (-c follows MOVED across whole cluster).

func writeClusterKey(t *testing.T, _ *harness.Stack, _ /*seedSID*/, key, _ /*val*/ string) {
	t.Helper()
	t.Fatalf("TODO(L3c-future): cluster-aware SET helper for key %q (redis-cli -c -a <pw> SET)", key)
}

func writeClusterKeyWithTTL(t *testing.T, _ *harness.Stack, _ /*seedSID*/, key, _ /*val*/ string, _ /*ttlMs*/ int) {
	t.Helper()
	t.Fatalf("TODO(L3c-future): cluster-aware PSETEX helper for TTL key %q", key)
}

func clusterDBSize(t *testing.T, _ *harness.Stack) int {
	t.Helper()
	t.Fatalf("TODO(L3c-future): cluster-aware DBSIZE aggregator (sum DBSIZE over masters)")
	return 0
}

func clusterKeyExists(t *testing.T, _ *harness.Stack, _ /*seedSID*/, key string) bool {
	t.Helper()
	t.Fatalf("TODO(L3c-future): cluster-aware EXISTS helper for key %q (redis-cli -c)", key)
	return false
}

func assertClusterKeyTTLPositive(t *testing.T, _ *harness.Stack, _ /*seedSID*/, key string) {
	t.Helper()
	t.Fatalf("TODO(L3c-future): cluster-aware PTTL>0 verify for key %q", key)
}

// assertNodeForgotten checks removed SID is absent from seed CLUSTER NODES (FORGET
// converged). Implementable now (redis-cli cluster nodes), but tied to live
// cluster from step (1), so it remains skeleton until create is unblocked.
func assertNodeForgotten(t *testing.T, stack *harness.Stack, seedSID, removeSID string) {
	t.Helper()
	idx := -1
	for i, sc := range stack.SoulContainers {
		if sc.SID == seedSID {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("seed SID %q not found among soul containers", seedSID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := stack.SoulContainers[idx].Exec(ctx,
		[]string{"redis-cli", "-a", remRedisPassword, "cluster", "nodes"})
	if err != nil || code != 0 {
		t.Fatalf("cluster nodes on seed: code=%d err=%v out=%s", code, err, out)
	}
	// removeSID -> ip:port. TODO(L3c-future): resolve SID->ip from soulprint; for
	// now verify that removed address physically disappeared from topology.
	if strings.Contains(out, removeSID) {
		t.Fatalf("removed node %q is still in CLUSTER NODES (FORGET did not converge):\n%s", removeSID, out)
	}
}
