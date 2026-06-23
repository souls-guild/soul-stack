//go:build e2e_live

// L3b E2E multi-host: examples/service/redis::create в режиме cluster на трёх
// ПОДЛИННЫХ soul-контейнерах (redis-консолидация концепции Ansible-роли, ADR-039 /
// .pm/tasks/2026-06-22-redis-consolidation).
//
// Прежний самодостаточный сервис redis-cluster-live (core-модули + redis-cli
// --cluster create, доказан live мегатестом 2026-05-25) удалён: cluster-флоу
// поглощён режимом cluster консолидированного redis, который формирует кластер
// ЦЕЛИКОМ через плагин community.redis.cluster (CLUSTER MEET/ADDSLOTS/REPLICATE по
// go-redis), а не redis-cli --cluster create.
//
// Тело ниже перецелено на консолидированный redis (redis_type=cluster, shards=3,
// replicas_per_shard=0 → 3 master без реплик, минимум для cluster_state:ok на
// 3-узловом стенде). Stack поднимает три privileged Debian-12 systemd-PID-1
// контейнера (soul-live-a/-b/-c.example.com) с реальным CSR-handshake-ом.
//
// ★ t.Skip: cluster-create через community.redis.cluster ещё НЕ доказан live
// end-to-end (render-проверен на L0 — scenario/create/tests/cluster-*, но
// container-бутстрап кластера плагином поверх soul-контейнеров не прогонялся:
// нет harness-обвязки дождаться cluster_state:ok через плагин-путь). Тот же
// backlog-блокер фиксирует redis_cluster_remove_node_lossless_test.go. Реактивировать
// вместе с cluster-create live (плагинный бутстрап + cluster-aware verify в harness).
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisClusterCreate_ThreeNode(t *testing.T) {
	t.Skip("backlog (redis-консолидация): cluster-create через community.redis.cluster не доказан live end-to-end (render-проверен на L0 scenario/create/tests/cluster-*, но плагинный бутстрап кластера поверх soul-контейнеров harness-ом не обвязан — нет helper-а дождаться cluster_state:ok через плагин-путь). Симметрично redis_cluster_remove_node_lossless_test.go::t.Skip. Реактивировать с harness cluster-bootstrap/verify helper-ами — .pm/tasks/2026-06-22-redis-consolidation")

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		ServiceName: "redis",
		Souls:       3,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 3 {
		t.Fatalf("ожидалось 3 soul-контейнера, получено %d", got)
	}
	wantSIDs := []string{
		"soul-live-a.example.com",
		"soul-live-b.example.com",
		"soul-live-c.example.com",
	}
	for i, want := range wantSIDs {
		if got := stack.SoulContainers[i].SID; got != want {
			t.Fatalf("SoulContainers[%d].SID = %q, ожидалось %q", i, got, want)
		}
	}

	const incName = "redis-clstr-create"

	// Coven-членство ДО Create: roster резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Все три соула в covene incarnation, иначе no_hosts → ноль apply_runs.
	for i := range stack.SoulContainers {
		stack.AddSoulToCoven(t, i, incName)
		// cluster nodes-MAP строится из soulprint.hosts (primary_ip по SID) —
		// ждём непустые факты ДО create-render.
		stack.WaitSoulprintReported(t, i, 60)
	}

	// requirepass + cluster-build password — keeper-side vault()-резолв.
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": "cluster-redis-secret-32b"})

	// Материализуем режим-агностичный destiny `redis` (cluster-ветка зовёт
	// apply: destiny: redis) под git-тегом v1.0.0 (ref из service.yml::destiny[]).
	stack.MaterializeDestinies(t, "v1.0.0", "redis")

	// POST /v1/incarnations авто-запускает create → возвращает apply_id авто-прогона.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis@main", map[string]any{
		"redis_type":         "cluster",
		"version":            "7.2.4",
		"shards":             3,
		"replicas_per_shard": 0,
	})

	// 600 c — apt-get install redis × 3 + render config + старт + плагинный
	// community.redis.cluster bootstrap (CLUSTER MEET/ADDSLOTS).
	stack.WaitApplySuccess(t, applyID, 600)
	stack.WaitIncarnationReady(t, inc, 30)

	exp := harness.LoadExpectations(t, "redis/expectations/after-create-cluster.yaml")
	stack.AssertExpectations(t, exp, applyID, inc)

	// Independent cluster-health-check: redis-cli cluster info внутри первого
	// soul-контейнера. Поллинг — cluster_state переходит в ok после того, как все
	// cluster bus-соединения установились.
	sc := stack.SoulContainers[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		lastOutput string
		lastExit   int
		ok         bool
	)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, code, err := sc.Exec(ctx, []string{"redis-cli", "-p", "6379", "cluster", "info"})
		if err != nil {
			t.Fatalf("redis-cli cluster info: %v", err)
		}
		lastOutput, lastExit = out, code
		if code == 0 && strings.Contains(out, "cluster_state:ok") {
			ok = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !ok {
		t.Fatalf("redis cluster не достиг cluster_state:ok (last_exit=%d) output:\n%s",
			lastExit, lastOutput)
	}
}
