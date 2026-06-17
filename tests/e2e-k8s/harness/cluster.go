//go:build e2e_k8s

// Package harness — L3c kind-cluster lifecycle и kubectl/kind subprocess-обёртки.
//
// В L3c-1 (pilot) реализуется только Cluster (spin-up/teardown kind через
// sigs.k8s.io/kind/pkg/cluster) и базовые subprocess-wrapper-ы. Высокоуровневый
// Stack.DeployInfra / Stack.DeployKeeper наполняются в L3c-2 / L3c-3.
package harness

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cmd"
)

// Cluster — обёртка kind-cluster с автоматическим teardown через t.Cleanup (LIFO).
//
// Имя кластера per-test (soul-stack-e2e-<sanitized-test-name>-<unix-nanos>) —
// PM-decision L3c-cluster-name. Изолирует параллельные прогоны в CI и не
// конфликтует с другими kind-кластерами на хосте.
//
// Kubeconfig пишется в t.TempDir() — автоматически удаляется test-harness-ом.
type Cluster struct {
	Name       string
	Kubeconfig string

	provider *cluster.Provider
}

// NewCluster spins up свежий kind-cluster и регистрирует teardown в t.Cleanup.
// Pre-flight: skip если docker или kind CLI не найдены в PATH.
//
// kind CLI нужен помимо Go-API: load-image (см. Cluster.LoadDockerImage)
// надёжно работает только через CLI (Go-API экспериментальный). Проверяем оба
// требования на старте, чтобы тест скипался единообразно.
func NewCluster(t *testing.T) *Cluster {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("L3c: docker не найден в PATH: %v", err)
	}
	if _, err := exec.LookPath("kind"); err != nil {
		t.Skipf("L3c: kind CLI не найден в PATH (нужен для load-image): %v", err)
	}

	name := fmt.Sprintf("soul-stack-e2e-%s-%d", sanitizeTestName(t.Name()), time.Now().UnixNano())
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig-"+name)

	provider := cluster.NewProvider(cluster.ProviderWithLogger(cmd.NewLogger()))

	// CreateWithWaitForReady=120s — kind по умолчанию НЕ ждёт control-plane;
	// без ожидания List Nodes сразу после Create может вернуть пустой список.
	// 120s — запас для macOS docker-desktop (медленнее Linux dev-машины).
	err := provider.Create(name,
		cluster.CreateWithKubeconfigPath(kubeconfig),
		cluster.CreateWithWaitForReady(120*time.Second),
	)
	if err != nil {
		t.Fatalf("kind create cluster %q: %v", name, err)
	}

	c := &Cluster{Name: name, Kubeconfig: kubeconfig, provider: provider}

	t.Cleanup(func() {
		if err := provider.Delete(name, kubeconfig); err != nil {
			t.Logf("kind delete cluster %s: %v", name, err)
		}
	})

	return c
}

// sanitizeTestName — kind cluster name должен быть DNS-friendly (RFC 1123:
// lowercase alphanumerics + '-'). Go-тесты имеют форму TestName_SubName/Case,
// что не подходит как есть.
//
// Максимальная длина — 30, чтобы остался запас под unix-nanos suffix
// (~19 символов) в пределах 63-char DNS-label лимита (kind ещё префиксует
// container "kind-<name>-control-plane" — итого ~80, контейнер-имя длиннее
// label, но kind терпит до своего внутреннего лимита).
func sanitizeTestName(name string) string {
	result := strings.ToLower(name)
	result = strings.ReplaceAll(result, "_", "-")
	result = strings.ReplaceAll(result, "/", "-")
	if len(result) > 30 {
		result = result[:30]
	}
	return result
}
