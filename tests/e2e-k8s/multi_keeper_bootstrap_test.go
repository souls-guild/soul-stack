//go:build e2e_k8s

package e2e_k8s_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-k8s/harness"
)

// TestL3cMultiKeeper_BootstrapAndConnect — L3c-3 flagship: 3 Keeper-pod
// (Deployment replicas=3) + 1 Soul-pod (StatefulSet privileged systemd-PID-1,
// parity с L3b debian-12) проходит реальный CSR Bootstrap-flow и достигает
// `souls.status='connected'` через k8s-Service ClusterIP к keeper-replica.
//
// Что валидируется:
//  1. multi-keeper HA-режим живой (3 Keeper-pod в Ready);
//  2. ClusterIP `keeper:9094` (bootstrap) и `keeper:9095` (event_stream)
//     round-robin распределяют трафик — Soul бьёт по DNS-имени, kube-proxy
//     направляет к любому из 3 replica;
//  3. реальный CSR-flow проходит (не shortcut prePush через SQL):
//     `soul init` делает Bootstrap RPC, получает leaf-cert, `soul run` через
//     systemd-service устанавливает EventStream-стрим;
//  4. audit-event `soul.bootstrapped` записан в `audit_log` keeper-стороной.
//
// Pre-requisites: docker + kind + kubectl + helm CLI, образы
// `keeper:e2e-k8s` и `soul:e2e-k8s` (Makefile: docker-build-keeper +
// docker-build-soul). Без любого — t.Skip.
//
// Сценарий (~5-7 мин):
//  1. kind spin-up + helm install PG/Redis/Vault (DeployInfra).
//  2. DeployKeeper(3): kind load image + Secret/ConfigMap + apply Deployment
//     (replicas=3) + wait ReadyReplicas=3 (5m).
//  3. DeploySoul: kind load image + IssueBootstrapToken (port-forward к PG) +
//     Secret/ConfigMap + apply StatefulSet + wait Ready (3m) + execInPod
//     `soul init` (реальный CSR) + execInPod `systemctl start soul.service`
//     + WaitForSoulConnected (2m).
//  4. Assert audit-event `soul.bootstrapped` payload.sid=soul-0.example.com.
func TestL3cMultiKeeper_BootstrapAndConnect(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{})
	certPEM, keyPEM, caPEM := stack.DeployInfra(t)
	pf := stack.DeployKeeper(t, 3, certPEM, keyPEM, caPEM)
	// pf не используется напрямую — DeploySoul бьёт по in-cluster DNS
	// `keeper.default.svc.cluster.local`, port-forward здесь только для
	// сторонней диагностики через /readyz. pf.Close зарегистрирован в Cleanup.
	_ = pf

	sid := stack.DeploySoul(t)

	stack.AssertAuditEvent(t, "soul.bootstrapped", map[string]any{
		"sid": sid,
	})
}
