//go:build e2e_k8s

package harness

import (
	"os"
	"os/exec"
	"testing"
)

// KubectlApply — subprocess `kubectl apply -f <path>` with KUBECONFIG from
// Cluster. Used by L3c-2+ to apply raw keeper/soul YAML manifests.
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
// The image must already be built (present in `docker images`) before
// calling -- kind load copies the tarball from the host's docker daemon
// into the node container.
//
// Used by L3c-3+ to load locally built keeper / soul-live images into the
// kind cluster without a registry push.
func (c *Cluster) LoadDockerImage(t *testing.T, image string) {
	t.Helper()
	cmd := exec.Command("kind", "load", "docker-image", image, "--name", c.Name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kind load docker-image %s --name %s: %v\n%s", image, c.Name, err, out)
	}
}
