//go:build e2e_k8s

package harness

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// toll.go — read-доступ к Toll cluster-degraded флагу в Redis (ADR-038). Helper
// нужен L3c-5 TestL3cToll_DegradedMode для assert-а, что Toll-leader взвёл
// `cluster:degraded` после массового оттока Soul-pod-ов.
//
// Ключ `cluster:degraded` — единое место (keeper/internal/redis/tolldetector.go
// const tollDegradedKey). Строка дублируется здесь сознательно: harness не
// импортирует keeper/internal/* (Go-internal-rules + L3c module isolation).

// tollDegradedKey — Redis-ключ Toll cluster-флага. Должен совпадать с
// keeper/internal/redis/tolldetector.go::tollDegradedKey.
const tollDegradedKey = "cluster:degraded"

// IsTollDegraded возвращает true, если в Redis выставлен флаг cluster:degraded.
// Использует EXISTS (не GET) — value-холдер для assert-а не нужен, флаг
// бинарный.
//
// Per-call port-forward + redis.Client: тест опрашивает в polling-loop-е, но
// каждый poll создаёт свежий PortForward — это надёжнее, чем shared-handle
// (kubectl port-forward может умереть, t.Cleanup закроет).
func (s *Stack) IsTollDegraded(t *testing.T) bool {
	t.Helper()
	pf := s.Cluster.PortForward(t, "svc/redis-master", 6379, 30*time.Second)

	// Bitnami Redis в helm-values по умолчанию без auth (см.
	// helm-values/redis.yaml: auth.enabled=false). Если поднимут — добавить
	// password из RedisAuthSecret.
	rc := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("127.0.0.1:%d", pf.LocalPort),
	})
	defer rc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	n, err := rc.Exists(ctx, tollDegradedKey).Result()
	if err != nil {
		t.Logf("IsTollDegraded: EXISTS %s: %v", tollDegradedKey, err)
		return false
	}
	return n == 1
}

// WaitForTollDegraded поллит cluster:degraded флаг до true или timeout.
// На fail печатает baseline-snapshot из PG (souls.status='connected') для
// диагностики (false-positive могут быть из-за пустого baseline).
func (s *Stack) WaitForTollDegraded(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.IsTollDegraded(t) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("WaitForTollDegraded: cluster:degraded не взведён за %v", timeout)
}
