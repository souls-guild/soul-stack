//go:build e2e_k8s

// Package harness — L3c kind cluster lifecycle and kubectl/kind subprocess
// wrappers.
//
// L3c-1 (pilot) implements only Cluster (spin-up/teardown of kind via
// sigs.k8s.io/kind/pkg/cluster) and the basic subprocess wrappers. The
// higher-level Stack.DeployInfra / Stack.DeployKeeper are filled in by
// L3c-2 / L3c-3.
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

// Cluster — kind cluster wrapper with automatic teardown via t.Cleanup (LIFO).
//
// Cluster name is per-test (soul-stack-e2e-<sanitized-test-name>-<unix-nanos>)
// — PM decision L3c-cluster-name. Isolates parallel runs in CI and avoids
// conflicting with other kind clusters on the host.
//
// Kubeconfig is written to t.TempDir() — auto-removed by the test harness.
type Cluster struct {
	Name       string
	Kubeconfig string

	provider *cluster.Provider
}

// NewCluster spins up a fresh kind cluster and registers teardown in
// t.Cleanup. Pre-flight: skips if docker or the kind CLI are not found in
// PATH.
//
// The kind CLI is needed in addition to the Go API: load-image (see
// Cluster.LoadDockerImage) only works reliably via the CLI (the Go API is
// experimental). Both requirements are checked upfront so the test skips
// consistently.
func NewCluster(t *testing.T) *Cluster {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("L3c: docker not found in PATH: %v", err)
	}
	if _, err := exec.LookPath("kind"); err != nil {
		t.Skipf("L3c: kind CLI not found in PATH (needed for load-image): %v", err)
	}

	name := fmt.Sprintf("soul-stack-e2e-%s-%d", sanitizeTestName(t.Name()), time.Now().UnixNano())
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig-"+name)

	provider := cluster.NewProvider(cluster.ProviderWithLogger(cmd.NewLogger()))

	// CreateWithWaitForReady=120s — kind does NOT wait for the control plane
	// by default; without waiting, List Nodes right after Create can return
	// an empty list. 120s gives headroom for macOS docker-desktop (slower
	// than a Linux dev machine).
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

// sanitizeTestName — the kind cluster name must be DNS-friendly (RFC 1123:
// lowercase alphanumerics + '-'). Go test names have the form
// TestName_SubName/Case, which doesn't fit as-is.
//
// Max length is 30, leaving headroom for the unix-nanos suffix (~19 chars)
// within the 63-char DNS-label limit (kind also prefixes the container name
// "kind-<name>-control-plane" -- ~80 total; the container name exceeds the
// label limit, but kind tolerates it up to its own internal limit).
func sanitizeTestName(name string) string {
	result := strings.ToLower(name)
	result = strings.ReplaceAll(result, "_", "-")
	result = strings.ReplaceAll(result, "/", "-")
	if len(result) > 30 {
		result = result[:30]
	}
	return result
}
