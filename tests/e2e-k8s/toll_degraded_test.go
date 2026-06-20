//go:build e2e_k8s

package e2e_k8s_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-k8s/harness"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestL3cToll_DegradedMode — L3c-5 part A: массовый отток Soul-pod-ов взводит
// `cluster:degraded` в Redis, POST scenario возвращает 503 + Retry-After,
// audit-event `cluster.degraded_set` записан.
//
// Сценарий (~10-15 мин):
//  1. kind + bitnami infra + 3 keeper-pod + 5 Soul-pod (DeployMultiSoul) +
//     Bootstrap-Archon JWT.
//  2. Warmup-wait 70s — Toll warmup_delay=60s (config.DefaultTollWarmup), до
//     этого disconnect-ы не публикуются. +10s запас.
//  3. Массовый delete 3 из 5 Soul-pod-ов (GracePeriodSeconds=0 → SIGKILL →
//     TCP reset → eventstream-handler регистрирует non-graceful disconnect →
//     Toll.NotifyDisconnect → ZADD `toll:disconnects`).
//  4. WaitForTollDegraded (90s). Rate = 3/5 = 0.60 > threshold 0.20.
//  5. PostScenarioRaw на произвольное incarnation/scenario → 503 + Retry-After
//     header. Toll-middleware — outermost в chain, 503 возвращается ДО проверки
//     existence incarnation/permission (см. router.go::POST scenarios chain).
//  6. AssertAuditEvent `cluster.degraded_set`.
func TestL3cToll_DegradedMode(t *testing.T) {
	const (
		soulCount       = 5
		killCount       = 3 // 3/5 = 60% > threshold 20%
		warmupWait      = 70 * time.Second
		degradedTimeout = 90 * time.Second
		incarnationName = "any-name"     // не существует, Toll отдаёт 503 раньше
		scenarioName    = "any-scenario" // тот же инвариант
	)

	stack := harness.NewStack(t, harness.Config{})
	certPEM, keyPEM, caPEM := stack.DeployInfra(t)
	_ = stack.DeployKeeper(t, 3, certPEM, keyPEM, caPEM)
	stack.BootstrapArchon(t)

	sids := stack.DeployMultiSoul(t, soulCount)
	if len(sids) != soulCount {
		t.Fatalf("DeployMultiSoul: ожидалось %d SID, получено %d", soulCount, len(sids))
	}

	t.Logf("waiting Toll warmup window (%v)…", warmupWait)
	time.Sleep(warmupWait)

	// Массовый kill. GracePeriodSeconds=0 → SIGKILL → non-graceful (TCP reset)
	// → eventstream registers disconnect → Toll publishes.
	killPods := make([]string, 0, killCount)
	for i := 0; i < killCount; i++ {
		killPods = append(killPods, "soul-"+strconv.Itoa(i))
	}
	t.Logf("killing %d Soul-pod-ов (grace=0): %v", killCount, killPods)

	zero := int64(0)
	for _, name := range killPods {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := stack.Clientset.CoreV1().Pods("default").Delete(delCtx, name, metav1.DeleteOptions{
			GracePeriodSeconds: &zero,
		})
		cancel()
		if err != nil {
			t.Fatalf("delete pod %s: %v", name, err)
		}
	}

	// Ожидание Toll-флага. detection cycle: ZADD на disconnect → следующий
	// TickInterval (5s) → leader Aggregation Tick → SET cluster:degraded.
	t.Logf("waiting cluster:degraded (max %v)…", degradedTimeout)
	stack.WaitForTollDegraded(t, degradedTimeout)

	// POST scenario → 503 + Retry-After.
	resp, status, err := stack.PostScenarioRaw(t, incarnationName, scenarioName, nil)
	if err != nil {
		t.Fatalf("PostScenarioRaw: %v", err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("PostScenarioRaw: ожидался 503, получено %d (headers=%v)", status, resp.Header)
	}
	retryAfter := resp.Header.Get("Retry-After")
	if retryAfter == "" {
		t.Fatalf("PostScenarioRaw: Retry-After header пуст (headers=%v)", resp.Header)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("PostScenarioRaw: Content-Type = %q, ожидался application/problem+json", ct)
	}

	// Audit-event cluster.degraded_set. Payload subset пуст — проверяем сам
	// факт записи (полные поля payload — rate/baseline_connected/threshold/
	// window_seconds — могут флуктуировать, не assert-аем точные значения).
	stack.AssertAuditEvent(t, "cluster.degraded_set", nil)

	// NB: Clear cluster:degraded out-of-scope: требует grace-window (60s) +
	// восстановления pod-ов + waiting + re-assert на 200 OK. На тест-end ок
	// держать кластер в degraded — kind-cluster одноразовый, t.Cleanup
	// удалит весь стенд.
}
