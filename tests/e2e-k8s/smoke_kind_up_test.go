//go:build e2e_k8s

package e2e_k8s_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-k8s/harness"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestL3cKindUp_Empty — pilot smoke L3c-1: kind-cluster поднимается, в нём
// есть как минимум один node со статусом Ready. Ничего не деплоит (PG/Redis/
// Vault → L3c-2, Keeper/Soul → L3c-3).
//
// При отсутствии docker/kind в PATH — t.Skip (см. harness.NewCluster).
func TestL3cKindUp_Empty(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{})

	cfg, err := clientcmd.BuildConfigFromFlags("", stack.Cluster.Kubeconfig)
	if err != nil {
		t.Fatalf("build kubeconfig %q: %v", stack.Cluster.Kubeconfig, err)
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("client-go NewForConfig: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Fatalf("ожидался минимум 1 node в kind-кластере, получено 0")
	}

	for _, node := range nodes.Items {
		var ready bool
		var lastMsg string
		for _, cond := range node.Status.Conditions {
			if cond.Type != corev1.NodeReady {
				continue
			}
			lastMsg = cond.Message
			if cond.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			t.Errorf("node %s не Ready: %s", node.Name, lastMsg)
		}
	}
}
