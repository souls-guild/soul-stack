//go:build e2e_k8s

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// helm.go — subprocess wrappers over the `helm` CLI for L3c-2 DeployInfra.
//
// PM decision (architect verdict a241beb181086d7a7): the bitnami chart repo
// is added at runtime via `helm repo add` + retry; vendoring charts into the
// repo is deferred. A single retry loop with backoff — the bitnami CDN
// sometimes lags on a cold start in CI or on a local dev machine.

const (
	helmRepoBitnami     = "bitnami"
	helmRepoBitnamiURL  = "https://charts.bitnami.com/bitnami"
	helmRetryAttempts   = 3
	helmRetryBackoff    = 5 * time.Second
	helmInstallTimeout  = "5m"
	helmRedisTimeout    = "3m"
	helmVaultTimeout    = "3m"
	helmInstallDeadline = 7 * time.Minute
)

// requireHelm — pre-flight check for tests that need the helm CLI.
// Symmetric to the kind/docker pre-flight in NewCluster: missing binary ->
// t.Skip, not t.Fatal.
func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("L3c: helm CLI not found in PATH: %v", err)
	}
}

// helmRepoEnsure adds the bitnami repo and refreshes the index. Idempotent:
// `helm repo add` fails with exit code 1 if a repo with that name already
// exists, but the text of the error distinguishes "already added" from a
// network error -- the former is not a failure, the latter is retried.
func (c *Cluster) helmRepoEnsure(t *testing.T) {
	t.Helper()

	var lastErr error
	for attempt := 1; attempt <= helmRetryAttempts; attempt++ {
		cmd := exec.Command("helm", "repo", "add", helmRepoBitnami, helmRepoBitnamiURL, "--force-update")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+c.Kubeconfig)
		out, err := cmd.CombinedOutput()
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("helm repo add bitnami (attempt %d): %v\n%s", attempt, err, out)
		t.Logf("%v", lastErr)
		time.Sleep(helmRetryBackoff)
	}
	if lastErr != nil {
		t.Fatalf("helm repo add bitnami: %v", lastErr)
	}

	updateCmd := exec.Command("helm", "repo", "update", helmRepoBitnami)
	updateCmd.Env = append(os.Environ(), "KUBECONFIG="+c.Kubeconfig)
	if out, err := updateCmd.CombinedOutput(); err != nil {
		t.Fatalf("helm repo update bitnami: %v\n%s", err, out)
	}
}

// helmInstall — `helm install <release> <chart> -n <ns> -f <values> --wait --timeout <t>`.
// `--wait` blocks until all chart resources are Ready (Deployment /
// StatefulSet pods); `--timeout` is the overall watch deadline. KUBECONFIG
// is passed through from Cluster so helm finds the target kind cluster.
func (c *Cluster) helmInstall(t *testing.T, release, chart, valuesPath, timeout string) {
	t.Helper()
	args := []string{
		"install", release, chart,
		"--namespace", "default",
		"--wait", "--timeout", timeout,
	}
	if valuesPath != "" {
		args = append(args, "-f", valuesPath)
	}
	cmd := exec.Command("helm", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+c.Kubeconfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm install %s %s: %v\n%s", release, chart, err, out)
	}
	t.Logf("helm install %s %s: %s", release, chart, firstLine(out))
}

// firstLine — a short summary for t.Log (helm prints a multi-line NOTES
// block on success; only the first line goes into the test log to avoid
// clutter).
func firstLine(out []byte) string {
	for i, b := range out {
		if b == '\n' {
			return string(out[:i])
		}
	}
	return string(out)
}
