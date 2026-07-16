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

// TestL3cToll_DegradedMode - L3c-5 part A: a mass exodus of Soul pods trips
// `cluster:degraded` in Redis, POST scenario returns 503 + Retry-After,
// audit event `cluster.degraded_set` is recorded.
//
// Scenario (~10-15 min):
//  1. kind + bitnami infra + 3 keeper pods + 5 Soul pods (DeployMultiSoul) +
//     Bootstrap-Archon JWT.
//  2. Warmup-wait 70s - Toll warmup_delay=60s (config.DefaultTollWarmup), before
//     that disconnects are not published. +10s buffer.
//  3. Mass delete 3 of 5 Soul pods (GracePeriodSeconds=0 -> SIGKILL ->
//     TCP reset -> eventstream handler registers a non-graceful disconnect ->
//     Toll.NotifyDisconnect -> ZADD `toll:disconnects`).
//  4. WaitForTollDegraded (90s). Rate = 3/5 = 0.60 > threshold 0.20.
//  5. PostScenarioRaw against an arbitrary incarnation/scenario -> 503 + Retry-After
//     header. Toll middleware is outermost in the chain, 503 is returned BEFORE checking
//     incarnation/permission existence (see router.go::POST scenarios chain).
//  6. AssertAuditEvent `cluster.degraded_set`.
func TestL3cToll_DegradedMode(t *testing.T) {
	const (
		soulCount       = 5
		killCount       = 3 // 3/5 = 60% > threshold 20%
		warmupWait      = 70 * time.Second
		degradedTimeout = 90 * time.Second
		incarnationName = "any-name"     // does not exist, Toll returns 503 first
		scenarioName    = "any-scenario" // same invariant
	)

	stack := harness.NewStack(t, harness.Config{})
	certPEM, keyPEM, caPEM := stack.DeployInfra(t)
	_ = stack.DeployKeeper(t, 3, certPEM, keyPEM, caPEM)
	stack.BootstrapArchon(t)

	sids := stack.DeployMultiSoul(t, soulCount)
	if len(sids) != soulCount {
		t.Fatalf("DeployMultiSoul: expected %d SID, got %d", soulCount, len(sids))
	}

	t.Logf("waiting Toll warmup window (%v)…", warmupWait)
	time.Sleep(warmupWait)

	// Mass kill. GracePeriodSeconds=0 -> SIGKILL -> non-graceful (TCP reset)
	// -> eventstream registers disconnect -> Toll publishes.
	killPods := make([]string, 0, killCount)
	for i := 0; i < killCount; i++ {
		killPods = append(killPods, "soul-"+strconv.Itoa(i))
	}
	t.Logf("killing %d Soul pods (grace=0): %v", killCount, killPods)

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

	// Wait for the Toll flag. detection cycle: ZADD on disconnect -> next
	// TickInterval (5s) -> leader Aggregation Tick -> SET cluster:degraded.
	t.Logf("waiting cluster:degraded (max %v)…", degradedTimeout)
	stack.WaitForTollDegraded(t, degradedTimeout)

	// POST scenario -> 503 + Retry-After.
	resp, status, err := stack.PostScenarioRaw(t, incarnationName, scenarioName, nil)
	if err != nil {
		t.Fatalf("PostScenarioRaw: %v", err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("PostScenarioRaw: expected 503, got %d (headers=%v)", status, resp.Header)
	}
	retryAfter := resp.Header.Get("Retry-After")
	if retryAfter == "" {
		t.Fatalf("PostScenarioRaw: Retry-After header is empty (headers=%v)", resp.Header)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("PostScenarioRaw: Content-Type = %q, expected application/problem+json", ct)
	}

	// Audit event cluster.degraded_set. Payload subset is empty - we only check the
	// fact of the write (full payload fields - rate/baseline_connected/threshold/
	// window_seconds - can fluctuate, not asserting exact values).
	stack.AssertAuditEvent(t, "cluster.degraded_set", nil)

	// NB: Clearing cluster:degraded is out-of-scope: requires a grace-window (60s) +
	// pod recovery + waiting + re-assert on 200 OK. It's fine to leave the
	// cluster degraded at test end - the kind cluster is one-shot, t.Cleanup
	// tears down the whole stand.
}
