//go:build e2e_live

// L3b E2E multi-host: redis-cluster-live happy-path (ADR-039, slice L3b-5).
//
// Поверх L3b-1..L3b-4 закрывает оставшийся пробел покрытия — multi-host прогон
// с N>1 soul-контейнерами. Stack поднимает три privileged Debian-12 systemd-PID-1
// контейнера (soul-live-a/-b/-c.example.com), каждый со своим SID, своим
// bootstrap-token-ом и реальным CSR-handshake-ом. Service —
// examples/service/service-redis-cluster-live (committed, scenario create
// устанавливает redis, рендерит cluster-config, запускает redis-server и
// формирует Redis Cluster через redis-cli --cluster create на одной ноде).
//
// --cluster-replicas передаётся как 0 (3 master без реплик, минимум для
// cluster_state:ok на 3-узловом стенде). default по scenario — 2 (9 нод),
// который на L3b-CI 3-нодовой раскладке не сходится: --cluster-replicas 2
// требует 1+2=3 ноды на slot-shard, всего >= 9.
//
// Покрытие: контракт apply_runs (3 строки success), incarnation.state
// (cluster_replicas=0), audit-event, metrics — всё через YAML loader
// (harness.LoadExpectations + Stack.AssertExpectations, L3b-5 deliverable).
// После — independent verify: redis-cli cluster info внутри первого
// контейнера должен показать cluster_state:ok.
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisClusterLive_ThreeNode(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/service-redis-cluster-live",
		ServiceName: "redis-cluster-live",
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

	inc := stack.CreateIncarnation(t, "test-redis-cluster", "redis-cluster-live@main", map[string]any{
		"cluster_replicas": 0,
	})

	applyID := stack.RunScenario(t, inc, "create", map[string]any{
		"cluster_replicas": 0,
	})

	// 600 c — apt-get update + apt-get install redis (с retry на флапе mirror-а)
	// на трёх контейнерах + render config + nohup redis-server + redis-cli
	// --cluster create. README example фиксирует ожидаемое время (~5-8 минут на
	// холодном CI).
	stack.WaitApplySuccess(t, applyID, 600)

	exp := harness.LoadExpectations(t, "redis-cluster-live/expectations/after-create.yaml")
	stack.AssertExpectations(t, exp, applyID, inc)

	// Independent cluster-health-check: redis-cli cluster info внутри первого
	// soul-контейнера. Поллинг на 30 секунд — cluster_state переходит в ok после
	// того, как все cluster bus-соединения установились (redis-cli --cluster
	// create возвращается раньше, чем gossip сойдётся на всех нодах).
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
