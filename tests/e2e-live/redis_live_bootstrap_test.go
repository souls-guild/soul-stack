//go:build e2e_live

// Общие live-redis-bootstrap-хелперы для L3b-тестов пакета e2e_live_test.
//
// Поднимают standalone-репликацию (host-0 master, host-1/2 REPLICAOF host-0)
// прямым Exec-ом в soul-контейнерах — НЕ через scenario-apply: нужен лишь
// дискриминатор `redis-cli role` master vs slave для probe→where / when-gating
// тестов (fc5_when_gating_test.go). Это standalone replication (REPLICAOF), НЕ
// cluster-mode.
//
// Вынесено в отдельный файл при redis-консолидации (.pm/tasks/2026-06-22-redis-
// consolidation): прежние tests, державшие эти хелперы (redis_cluster_update_acl /
// redis_cluster_create на удалённом сервисе redis-cluster), сняты — но
// bootstrapLiveRedis/registerStdout переиспользует FC-5. Стоп-правило «не плодим
// второй redis-bootstrap».
package e2e_live_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// bootstrapLiveRedis ставит redis-server на все три контейнера и поднимает
// standalone-репликацию: host-0 — master, host-1/2 — REPLICAOF host-0. Возвращает
// IP host-0 (master). Идемпотентно по шагам (повторный nohup/REPLICAOF безвреден).
func bootstrapLiveRedis(t *testing.T, stack *harness.Stack) (masterIP string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// apt install на всех трёх (redis-server + redis-tools/redis-cli).
	for i := range stack.SoulContainers {
		execOK(t, ctx, stack, i, []string{"sh", "-c",
			"apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq redis-server"},
			"apt install redis-server")
	}

	// Запуск redis-server в фоне на каждой ноде (bind 0.0.0.0, protected-mode no
	// для cross-container replication). Idempotent: unless ping PONG.
	for i := range stack.SoulContainers {
		execOK(t, ctx, stack, i, []string{"sh", "-c",
			"redis-cli -p 6379 ping 2>/dev/null | grep -q PONG || " +
				"(mkdir -p /var/lib/redis && nohup redis-server --bind 0.0.0.0 --protected-mode no --dir /var/lib/redis >/var/log/redis.log 2>&1 & sleep 1; true)"},
			"start redis-server")
	}

	// Health-gate: redis отвечает PONG на каждой ноде.
	for i := range stack.SoulContainers {
		waitRedisPong(t, ctx, stack, i)
	}

	// IP мастера (host-0) — REPLICAOF целевой адрес для реплик.
	out, code, err := stack.SoulContainers[0].Exec(ctx, []string{"sh", "-c", "hostname -i | awk '{print $1}'"})
	if err != nil || code != 0 {
		t.Fatalf("bootstrapLiveRedis: hostname -i на master: code=%d err=%v out=%s", code, err, out)
	}
	masterIP = strings.TrimSpace(out)
	if masterIP == "" {
		t.Fatalf("bootstrapLiveRedis: пустой master IP (hostname -i = %q)", out)
	}

	// host-1/2 → REPLICAOF host-0. Дожидаемся master_link_status:up.
	for i := 1; i < len(stack.SoulContainers); i++ {
		execOK(t, ctx, stack, i, []string{"sh", "-c",
			fmt.Sprintf("redis-cli -p 6379 replicaof %s 6379", masterIP)},
			"REPLICAOF master")
		waitReplicaLinkUp(t, ctx, stack, i)
	}

	return masterIP
}

// execOK выполняет команду в i-м контейнере и фейлит при exit!=0.
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

// waitRedisPong поллит redis-cli ping до PONG (cold-start redis-server ~1-3s).
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
	t.Fatalf("waitRedisPong[%d]: redis не ответил PONG за 30s", soulIdx)
}

// waitReplicaLinkUp поллит master_link_status:up в `redis-cli info replication`
// на реплике (REPLICAOF-handshake + initial sync).
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
	t.Fatalf("waitReplicaLinkUp[%d]: master_link_status:up не достигнут за 30s\nlast info:\n%s", soulIdx, last)
}

// registerStdout читает register_data->>'stdout' probe-задачи (Passage 0,
// plan_index 0) хоста sid. Доказывает, что реальный soul вернул TaskEvent с
// register-данными probe-роли, а keeper их персистил в apply_task_register.
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
		t.Fatalf("registerStdout(%s): нет register probe-задачи passage=0 (реальный soul не вернул register?): %v", sid, err)
	}
	return stdout
}
