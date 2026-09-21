//go:build e2e_k8s

package e2e_k8s_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-k8s/harness"
)

// TestL3cKeeperPing_Single - L3c-2 smoke: a single Keeper pod (+ PG/Redis/Vault
// via bitnami Helm) responds 200 on /readyz through port-forward.
//
// Pre-requisites:
//   - docker + kind CLI in PATH (see harness.NewCluster pre-flight: otherwise t.Skip);
//   - helm CLI in PATH (DeployInfra pre-flight: otherwise t.Skip);
//   - kubectl CLI in PATH (PortForward + KubectlApply: otherwise t.Skip);
//   - image `keeper:e2e-k8s` (`make docker-build-keeper`).
//
// Scenario (~3-5 min):
//  1. kind spin-up + helm install PG/Redis/Vault (DeployInfra).
//  2. Vault seed: PKI mount + JWT signing key + PG DSN + keeper-server TLS cert.
//  3. DeployKeeper: kind load image + Secret/ConfigMap + apply Deployment+Service.
//  4. Wait ReadyReplicas (timeout 5m).
//  5. Port-forward keeper-svc:8080 -> host loopback; ping /readyz until 200
//     (deadline 2 min - keeper run applies migrations at startup).
func TestL3cKeeperPing_Single(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{})
	certPEM, keyPEM, caPEM := stack.DeployInfra(t)
	pf := stack.DeployKeeper(t, 1, certPEM, keyPEM, caPEM)
	// pf.Close() is already registered by the harness via t.Cleanup.

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
