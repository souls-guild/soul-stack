//go:build e2e_k8s

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// helm.go — subprocess-обёртки над `helm` CLI для L3c-2 DeployInfra.
//
// PM-decision (architect-вердикт a241beb181086d7a7): bitnami chart repo
// подключается runtime через `helm repo add` + retry, вендоринг чартов в репо
// отложен. Один retry-loop с backoff — bitnami CDN иногда лагает на cold start
// CI/локальной dev-машины.

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

// requireHelm — pre-flight для тестов, которым нужен helm CLI. Симметрично
// pre-flight kind/docker в NewCluster: без бинаря — t.Skip, не t.Fatal.
func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("L3c: helm CLI не найден в PATH: %v", err)
	}
}

// helmRepoEnsure добавляет bitnami-репо и обновляет индекс. Идемпотентен:
// `helm repo add` падает с exit-code 1 если репо с этим именем уже есть, но
// текстовая ошибка отличает «уже добавлен» от сетевой — в первом случае не
// фейлим, во втором — retry.
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
// `--wait` блокирует до Ready всех ресурсов чарта (Deployment / StatefulSet
// pods); `--timeout` — общий watch-deadline. KUBECONFIG прокидывается из
// Cluster, чтобы helm нашёл целевой kind-кластер.
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

// firstLine — короткий summary для t.Log (helm на success печатает многострочный
// NOTES; в лог теста кладём только первую строку, чтобы не загромождать).
func firstLine(out []byte) string {
	for i, b := range out {
		if b == '\n' {
			return string(out[:i])
		}
	}
	return string(out)
}
