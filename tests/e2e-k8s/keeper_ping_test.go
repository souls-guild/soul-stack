//go:build e2e_k8s

package e2e_k8s_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-k8s/harness"
)

// TestL3cKeeperPing_Single — L3c-2 smoke: единичный Keeper-pod (+ PG/Redis/Vault
// через bitnami Helm) отвечает 200 на /readyz через port-forward.
//
// Pre-requisites:
//   - docker + kind CLI в PATH (см. harness.NewCluster pre-flight: иначе t.Skip);
//   - helm CLI в PATH (DeployInfra pre-flight: иначе t.Skip);
//   - kubectl CLI в PATH (PortForward + KubectlApply: иначе t.Skip);
//   - образ `keeper:e2e-k8s` (`make docker-build-keeper`).
//
// Сценарий (~3-5 мин):
//  1. kind spin-up + helm install PG/Redis/Vault (DeployInfra).
//  2. Vault seed: PKI mount + JWT signing-key + PG DSN + keeper-server TLS-cert.
//  3. DeployKeeper: kind load image + Secret/ConfigMap + apply Deployment+Service.
//  4. Wait ReadyReplicas (timeout 5m).
//  5. Port-forward keeper-svc:8080 → host loopback; ping /readyz до 200
//     (deadline 2 мин — keeper run применяет миграции на старте).
func TestL3cKeeperPing_Single(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{})
	certPEM, keyPEM, caPEM := stack.DeployInfra(t)
	pf := stack.DeployKeeper(t, 1, certPEM, keyPEM, caPEM)
	// pf.Close() уже зарегистрирован harness-ом через t.Cleanup.

	readyURL := fmt.Sprintf("http://127.0.0.1:%d/readyz", pf.LocalPort)
	client := &http.Client{Timeout: 5 * time.Second}

	deadline := time.Now().Add(2 * time.Minute)
	var lastStatus int
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(readyURL)
		if err != nil {
			lastErr = err
		} else {
			lastStatus = resp.StatusCode
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return // PASS
			}
		}
		time.Sleep(2 * time.Second)
	}

	t.Fatalf("keeper /readyz did not return 200 within 2 minutes (last status=%d, last err=%v)",
		lastStatus, lastErr)
}
