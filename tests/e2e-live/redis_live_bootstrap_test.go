//go:build e2e_live

// Common live-redis-bootstrap helpers for the L3b tests in package e2e_live_test.
//
// Bring up standalone replication (host-0 master, host-1/2 REPLICAOF host-0)
// via a direct Exec in the soul containers - NOT via scenario-apply: only the
// `redis-cli role` master vs slave discriminator is needed for probe->where / when-gating
// tests (fc5_when_gating_test.go). This is standalone replication (REPLICAOF), NOT
// cluster-mode.
//
// Factored into a separate file during the redis consolidation (.pm/tasks/2026-06-22-redis-
// consolidation): the earlier tests that held these helpers (redis_cluster_update_acl /
// redis_cluster_create on the now-removed redis-cluster service) were removed - but
// bootstrapLiveRedis/registerStdout is reused by FC-5. Stop-rule: don't grow a
// second redis-bootstrap.
package e2e_live_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// bootstrapLiveRedis installs redis-server on all three containers and brings up
// standalone replication: host-0 - master, host-1/2 - REPLICAOF host-0. Returns
// host-0's (master) IP. Idempotent per step (a repeated nohup/REPLICAOF is harmless).
func bootstrapLiveRedis(t *testing.T, stack *harness.Stack) (masterIP string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// apt install on all three (redis-server + redis-tools/redis-cli).
	for i := range stack.SoulContainers {
		execOK(t, ctx, stack, i, []string{"sh", "-c",
			"apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq redis-server"},
			"apt install redis-server")
	}

	// Start redis-server in the background on each node (bind 0.0.0.0, protected-mode no
	// for cross-container replication). Idempotent: unless ping PONG.
	for i := range stack.SoulContainers {
		execOK(t, ctx, stack, i, []string{"sh", "-c",
			"redis-cli -p 6379 ping 2>/dev/null | grep -q PONG || " +
				"(mkdir -p /var/lib/redis && nohup redis-server --bind 0.0.0.0 --protected-mode no --dir /var/lib/redis >/var/log/redis.log 2>&1 & sleep 1; true)"},
			"start redis-server")
	}

	// Health-gate: redis responds PONG on every node.
	for i := range stack.SoulContainers {
		waitRedisPong(t, ctx, stack, i)
	}

	// Master (host-0) IP - the REPLICAOF target address for replicas.
	out, code, err := stack.SoulContainers[0].Exec(ctx, []string{"sh", "-c", "hostname -i | awk '{print $1}'"})
	if err != nil || code != 0 {
		t.Fatalf("bootstrapLiveRedis: hostname -i on master: code=%d err=%v out=%s", code, err, out)
	}
	masterIP = strings.TrimSpace(out)
	if masterIP == "" {
		t.Fatalf("bootstrapLiveRedis: empty master IP (hostname -i = %q)", out)
	}

	// host-1/2 -> REPLICAOF host-0. Wait for master_link_status:up.
	for i := 1; i < len(stack.SoulContainers); i++ {
		execOK(t, ctx, stack, i, []string{"sh", "-c",
			fmt.Sprintf("redis-cli -p 6379 replicaof %s 6379", masterIP)},
			"REPLICAOF master")
		waitReplicaLinkUp(t, ctx, stack, i)
	}

	return masterIP
}

// execOK runs a command in the i-th container and fails on exit!=0.
func execOK(t *testing.T, ctx context.Context, stack *harness.Stack, soulIdx int, cmd []string, desc string) {
	t.Helper()
	out, code, err := stack.SoulContainers[soulIdx].Exec(ctx, cmd)
	if err != nil {
		t.Fatalf("bootstrapLiveRedis[%d] %s: exec: %v\nout=%s", soulIdx, desc, err, out)
	}
	if code != 0 {
		t.Fatalf("bootstrapLiveRedis[%d] %s: exit=%d\nout=%s", soulIdx, desc, code, out)
	}
}

// waitRedisPong polls redis-cli ping until PONG (cold-start redis-server ~1-3s).
func waitRedisPong(t *testing.T, ctx context.Context, stack *harness.Stack, soulIdx int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, code, err := stack.SoulContainers[soulIdx].Exec(ctx, []string{"redis-cli", "-p", "6379", "ping"})
		if err == nil && code == 0 && strings.Contains(out, "PONG") {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("waitRedisPong[%d]: redis did not respond PONG within 30s", soulIdx)
}

// waitReplicaLinkUp polls master_link_status:up in `redis-cli info replication`
// on the replica (REPLICAOF handshake + initial sync).
func waitReplicaLinkUp(t *testing.T, ctx context.Context, stack *harness.Stack, soulIdx int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, code, err := stack.SoulContainers[soulIdx].Exec(ctx, []string{"redis-cli", "-p", "6379", "info", "replication"})
		if err == nil && code == 0 {
			last = out
			if strings.Contains(out, "master_link_status:up") {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("waitReplicaLinkUp[%d]: master_link_status:up not reached within 30s\nlast info:\n%s", soulIdx, last)
}

// registerStdout reads register_data->>'stdout' from the probe task (Passage 0,
// plan_index 0) of host sid. Proves that the real soul returned a TaskEvent with
// the probe role's register data, and keeper persisted it into apply_task_register.
func registerStdout(t *testing.T, stack *harness.Stack, applyID, sid string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout string
	err := stack.DB().QueryRow(ctx,
		`SELECT COALESCE(register_data->>'stdout','<null>') FROM apply_task_register
		 WHERE apply_id = $1 AND sid = $2 AND passage = 0
		 ORDER BY plan_index ASC LIMIT 1`, applyID, sid).Scan(&stdout)
	if err != nil {
		t.Fatalf("registerStdout(%s): no register probe task passage=0 (real soul didn't return register?): %v", sid, err)
	}
	return stdout
}
