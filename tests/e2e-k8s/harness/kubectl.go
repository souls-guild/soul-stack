//go:build e2e_k8s

package harness

import (
	"os"
	"os/exec"
	"testing"
)

// KubectlApply — subprocess `kubectl apply -f <path>` с KUBECONFIG из Cluster.
// Используется L3c-2+ для применения raw YAML manifest-ов keeper/soul.
func (c *Cluster) KubectlApply(t *testing.T, manifestPath string) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", manifestPath)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+c.Kubeconfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl apply -f %s: %v\n%s", manifestPath, err, out)
	}
}

// LoadDockerImage — subprocess `kind load docker-image <image> --name <cluster>`.
// Образ должен быть уже built (присутствует в `docker images`) до вызова —
// kind load копирует tarball из docker-daemon хоста внутрь node-контейнера.
//
// Используется L3c-3+ для загрузки локально собранных keeper / soul-live image
// в kind-cluster без push в registry.
func (c *Cluster) LoadDockerImage(t *testing.T, image string) {
	t.Helper()
	cmd := exec.Command("kind", "load", "docker-image", image, "--name", c.Name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kind load docker-image %s --name %s: %v\n%s", image, c.Name, err, out)
	}
}
