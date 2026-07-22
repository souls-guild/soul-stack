//go:build e2e_k8s

package e2e_k8s_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-k8s/harness"
)

// TestL3cMultiKeeper_BootstrapAndConnect - L3c-3 flagship: 3 Keeper pods
// (Deployment replicas=3) + 1 Soul pod (StatefulSet privileged systemd-PID-1,
// parity with L3b debian-12) goes through the real CSR Bootstrap flow and reaches
// `souls.status='connected'` via a k8s Service ClusterIP to a keeper replica.
//
// What's validated:
//  1. multi-keeper HA mode is alive (3 Keeper pods Ready);
//  2. ClusterIP `keeper:9094` (bootstrap) and `keeper:9095` (event_stream)
//     round-robin traffic - Soul hits the DNS name, kube-proxy
//     routes to any of the 3 replicas;
//  3. the real CSR flow works (not a prePush SQL shortcut):
//     `soul init` does the Bootstrap RPC, gets a leaf cert, `soul run` via
//     systemd-service establishes the EventStream stream;
//  4. audit event `soul.bootstrapped` is recorded in `audit_log` by the keeper side.
//
// Pre-requisites: docker + kind + kubectl + helm CLI, images
// `keeper:e2e-k8s` and `soul:e2e-k8s` (Makefile: docker-build-keeper +
// docker-build-soul). Missing any of these - t.Skip.
//
// Scenario (~5-7 min):
//  1. kind spin-up + helm install PG/Redis/Vault (DeployInfra).
//  2. DeployKeeper(3): kind load image + Secret/ConfigMap + apply Deployment
//     (replicas=3) + wait ReadyReplicas=3 (5m).
//  3. DeploySoul: kind load image + IssueBootstrapToken (port-forward to PG) +
//     Secret/ConfigMap + apply StatefulSet + wait Ready (3m) + execInPod
//     `soul init` (real CSR) + execInPod `systemctl start soul.service`
//     + WaitForSoulConnected (2m).
//  4. Assert audit event `soul.bootstrapped` payload.sid=soul-0.example.com.
func TestL3cMultiKeeper_BootstrapAndConnect(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{})
	certPEM, keyPEM, caPEM := stack.DeployInfra(t)
	pf := stack.DeployKeeper(t, 3, certPEM, keyPEM, caPEM)
	// pf is not used directly - DeploySoul hits the in-cluster DNS
	// `keeper.default.svc.cluster.local`, port-forward here is only for
	// out-of-band diagnostics via /readyz. pf.Close is registered in Cleanup.
	_ = pf

	sid := stack.DeploySoul(t)

	stack.AssertAuditEvent(t, "soul.bootstrapped", map[string]any{
		"sid": sid,
	})
}
