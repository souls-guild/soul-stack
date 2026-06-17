//go:build e2e_k8s

package harness

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// leader.go — harness-помощник L3c-4 для интроспекции Redis lease-ключей
// в kind-cluster: `reaper:leader` (Reaper-leadership KID) и `soul:<sid>:lock`
// (SID-lease KID-holder). Используется failover-тестом для определения, какой
// keeper-pod держит leadership на момент kill-pod, и для верификации, что
// после kill оставшиеся pod подхватили работу.
//
// Реализация: `kubectl exec -it redis-master-0 -- redis-cli GET <key>`.
// Per-вызов exec — дороже long-lived клиента, но в failover-тесте мы делаем
// ≤10 GET-операций, разница в ms незаметна. Альтернатива (port-forward +
// go-redis в go.mod tests/e2e-k8s/) тянет лишнюю зависимость, redis-cli уже
// есть в bitnami/redis-image.

// redisMasterPod — имя single-replica master pod-а bitnami/redis-чарта
// (standalone-режим). Совпадает с redis.yaml::architecture: standalone.
const redisMasterPod = "redis-master-0"

// redisGet выполняет `redis-cli -t <timeout> GET <key>` внутри redis-master
// pod-а и возвращает строковое значение (без трейлинг \n). Пустая строка —
// `(nil)` в redis-cli, что означает «ключ отсутствует или истёк».
//
// Возвращает (value, err) где err — non-nil только на инфраструктурных
// ошибках kubectl/exec, не на отсутствии ключа.
func (s *Stack) redisGet(ctx context.Context, key string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		"exec",
		"-n", "default",
		redisMasterPod,
		"--",
		"redis-cli", "GET", key,
	)
	cmd.Env = append(cmd.Env, "KUBECONFIG="+s.Cluster.Kubeconfig)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl exec redis-cli GET %s: %w (stderr: %s)",
			key, err, stderr.String())
	}

	// redis-cli печатает значение + \n; на nil возвращает пустую строку.
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// GetReaperLeaderKID читает Redis-ключ `reaper:leader` и возвращает KID
// текущего Reaper-лидера. Пустая строка — нет лидера (lease истёк или
// reaper.enabled=false во всех pod-ах). Ключ зафиксирован константой
// `leaseKey` в keeper/internal/reaper/runner.go.
func (s *Stack) GetReaperLeaderKID(ctx context.Context) (string, error) {
	return s.redisGet(ctx, "reaper:leader")
}

// GetSoulLeaseHolder читает Redis-ключ `soul:<sid>:lock` и возвращает KID
// keeper-pod-а, который держит EventStream к данному Soul-у. Пустая строка —
// lease истёк / нет активного стрима. Ключ зафиксирован в
// keeper/internal/redis/soullease.go::SoulLeaseKey.
//
// Failover-тест использует значение ДО kill-pod для понимания, надо ли
// проверять реконнект (если holder == killed-leader-KID).
func (s *Stack) GetSoulLeaseHolder(ctx context.Context, sid string) (string, error) {
	return s.redisGet(ctx, "soul:"+sid+":lock")
}

// FindKeeperPodByKID ищет pod с label `app=keeper`, имя которого совпадает с
// KID. Convention `KID = pod-name` обеспечена init-container-ом `kid-render`
// (см. manifests/keeper/deployment.yaml): он подставляет `$POD_NAME` в
// литерал `__KID__` в шаблоне keeper.yml.
//
// Возвращает err, если pod с таким именем не найден среди label=app=keeper —
// failover-тест должен фейлить explicitly, а не угадывать неверный pod
// для kill.
func (s *Stack) FindKeeperPodByKID(ctx context.Context, kid string) (string, error) {
	pods, err := s.Clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{
		LabelSelector: "app=keeper",
	})
	if err != nil {
		return "", fmt.Errorf("list keeper pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Name == kid {
			return p.Name, nil
		}
	}
	available := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		available = append(available, p.Name)
	}
	return "", fmt.Errorf("no keeper pod with name=%q (KID convention pod-name); available pods: %v",
		kid, available)
}

// WaitForDeploymentReady блокируется до тех пор, пока Deployment не достигнет
// readyReplicas == spec.replicas. Используется L3c-4 для подтверждения, что
// после kill-pod self-heal восстановил 3 ready replicas (новый pod создан
// ReplicaSet-ом + прошёл readiness probe).
//
// Симметрично waitDeploymentReady (внутренний helper в DeployKeeper-flow),
// но публично-экспортируемый.
func (s *Stack) WaitForDeploymentReady(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		dep, err := s.Clientset.AppsV1().Deployments("default").Get(ctx, name, metav1.GetOptions{})
		cancel()
		if err == nil && dep.Spec.Replicas != nil && dep.Status.ReadyReplicas >= *dep.Spec.Replicas {
			return
		}
		if err == nil {
			ready := dep.Status.ReadyReplicas
			want := int32(0)
			if dep.Spec.Replicas != nil {
				want = *dep.Spec.Replicas
			}
			t.Logf("WaitForDeploymentReady %s: ready=%d/%d", name, ready, want)
		} else {
			t.Logf("WaitForDeploymentReady %s: get: %v", name, err)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("deployment %s did not become Ready within %v", name, timeout)
}

// DeleteKeeperPod удаляет pod kubectl-эквивалентом через client-go.
// Возвращает err — caller (failover-тест) обязан проверить.
//
// graceful: grace-period 0 — нам нужна симуляция жёсткого сбоя (lease должен
// остаться удерживаемым killed-KID-ом до истечения TTL). graceful shutdown
// вызвал бы корректный Release lease через defer-цепочку в Reaper, что
// сделало бы failover-окно нулевым (новый лидер моментально захватил бы) —
// тест валидировал бы happy path, а не реальный crash-сценарий.
func (s *Stack) DeleteKeeperPod(ctx context.Context, podName string) error {
	zero := int64(0)
	return s.Clientset.CoreV1().Pods("default").Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
	})
}

// keeperPodList возвращает имена всех pod с label `app=keeper`. Используется
// для логирования диагностики failover (какой pod удалён, какие остались).
//
// Возвращает только Running-pod-ы — kubectl delete pod создаёт нового
// (Terminating-старый + Pending-новый); caller обычно хочет видеть только
// активные.
func (s *Stack) keeperPodList(ctx context.Context) ([]string, error) {
	pods, err := s.Clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{
		LabelSelector: "app=keeper",
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			out = append(out, p.Name)
		}
	}
	return out, nil
}
