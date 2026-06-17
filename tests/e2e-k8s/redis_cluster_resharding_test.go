//go:build e2e_k8s

package e2e_k8s_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-k8s/harness"
)

// TestL3cRedisCluster_Resharding — L3c-5 part B: портирование мега-теста
// 2026-05-25 (project_megatest_ha_scale_2026_05_25.md) в kind. 3 Soul-pod →
// scenario `create` из service-redis-cluster-live (3-master cluster,
// --cluster-replicas 0) → assert `cluster_state:ok` через `redis-cli cluster
// info` в одном из pod-ов.
//
// Pre-requisites:
//   - service-catalog c записью `redis-cluster-live` (POST /v1/services).
//   - keeper-pod ДОЛЖЕН видеть git-repo `service-redis-cluster-live`. В L3b
//     это file://-URL на host-side bare-repo (см. tests/e2e-live/harness/
//     git.go); в kind-cluster file:// из pod-а недоступен — host-FS отсутствует.
//     Нужен либо in-cluster git-server pod (sidecar/standalone), либо
//     mount/projected-volume с пред-сделанным bare-repo.
//
// L3c-5 НЕ закрывает git-server infra для kind. Тест в текущем виде —
// каркас с t.Skip и понятной диагностикой; реальный прогон — L3c-future
// (отдельный slice по architect-вердикту, новая сущность `git-server-pod` в
// harness требует propose-and-wait).
//
// Длительность при включении: ~15 мин (apt-get install redis × 3 pod +
// cluster-create).
func TestL3cRedisCluster_Resharding(t *testing.T) {
	t.Skip("L3c-5 part B заблокирован: нет git-server-pod infra для service-loader-а в kind. " +
		"Требуется отдельный slice (propose-and-wait по имени `git-server-pod` + " +
		"alpine/git или nginx-git-http-backend deployment).")

	// Каркас остаётся для будущей разблокировки (L3c-future): когда появится
	// harness.DeployGitServerPod + Stack.RegisterService с in-cluster URL,
	// этот код станет boilerplate.
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/service-redis-cluster-live",
		ServiceName: "redis-cluster-live",
		Souls:       3,
	})
	certPEM, keyPEM, caPEM := stack.DeployInfra(t)
	_ = stack.DeployKeeper(t, 3, certPEM, keyPEM, caPEM)
	stack.BootstrapArchon(t)

	sids := stack.DeployMultiSoul(t, 3)
	if len(sids) != 3 {
		t.Fatalf("DeployMultiSoul: ожидалось 3 SID, получено %d", len(sids))
	}

	incName := stack.CreateIncarnation(t, "test-redis-cluster", "redis-cluster-live@main", map[string]any{
		"cluster_replicas": 0,
	})
	applyID := stack.RunScenario(t, incName, "create", map[string]any{
		"cluster_replicas": 0,
	})
	stack.WaitApplySuccess(t, applyID, 600)

	// Independent verify: cluster_state:ok через exec в soul-0.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deadline := time.Now().Add(30 * time.Second)
	var lastOutput string
	var lastExit int
	ok := false
	for time.Now().Before(deadline) {
		out, code, err := stack.ExecInSoulPod(ctx, "soul-0", []string{
			"redis-cli", "-p", "6379", "cluster", "info",
		})
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
