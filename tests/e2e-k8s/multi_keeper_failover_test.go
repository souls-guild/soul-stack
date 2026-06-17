//go:build e2e_k8s

package e2e_k8s_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-k8s/harness"
)

// TestL3cMultiKeeper_KillLeader — L3c-4 HA-failover-сценарий: 3 Keeper-pod в
// Deployment (replicas=3) + 1 Soul-pod. Один из keeper-pod-ов (выбранный
// Reaper-лидер) убивается через `kubectl delete pod --grace-period=0`;
// валидируется, что:
//
//  1. оставшиеся 2 keeper подбирают Reaper-leadership через истечение Redis
//     lease (lock_ttl=15s, см. harness/keeperyml.go::reaperBlock) — новый
//     лидер появляется в `GET reaper:leader` с KID, отличным от killed;
//  2. Deployment self-healing восстанавливает 3 ready replica (ReplicaSet
//     создаёт новый pod + он проходит /readyz);
//  3. Soul-pod остаётся в `souls.status='connected'` (если он держался не
//     на killed-Keeper) или реконнектится через failback-цепочку endpoints
//     (если держался — fallback-list keeper:9095 ClusterIP направит на
//     живой pod через kube-proxy round-robin).
//
// Pre-requisites: docker + kind + kubectl + helm + образы `keeper:e2e-k8s` и
// `soul:e2e-k8s`. Без любого — t.Skip.
//
// ReaperEnabled=true в Config — иначе lease `reaper:leader` не пишется,
// failover-механика не валидируется. Дефолт false для L3c-2/3 сохранён.
//
// Per-pod KID — за счёт init-container `kid-render` (см.
// manifests/keeper/deployment.yaml): подставляет $POD_NAME в литерал
// `__KID__` шаблона keeper.yml. Без этого все 3 pod имели бы одинаковый
// KID и failover-механика была бы не различима (lease holder всегда тот же
// KID, новый pod захватил бы тот же ключ моментально без видимой смены).
func TestL3cMultiKeeper_KillLeader(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ReaperEnabled: true,
	})
	certPEM, keyPEM, caPEM := stack.DeployInfra(t)
	pf := stack.DeployKeeper(t, 3, certPEM, keyPEM, caPEM)
	_ = pf

	sid := stack.DeploySoul(t)

	// 1. Дожидаемся выбора первого Reaper-лидера. lock_ttl=15s — лидер
	//    появляется на первом успешном Acquire. Запас 60s покрывает
	//    cold-start race (3 pod стартуют почти одновременно).
	leaderKID := waitForReaperLeader(t, stack, "", 60*time.Second)
	t.Logf("L3c-4 initial Reaper leader: %s", leaderKID)

	// 2. Находим pod по KID — convention KID=pod-name держится init-
	//    container-ом.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	leaderPod, err := stack.FindKeeperPodByKID(ctx, leaderKID)
	cancel()
	if err != nil {
		t.Fatalf("FindKeeperPodByKID(%s): %v", leaderKID, err)
	}

	// 3. Snapshot SID-lease holder до failover. Если соул держится на
	//    killed-leader — после kill будет реконнект на другой keeper.
	ctxLease, cancelLease := context.WithTimeout(context.Background(), 10*time.Second)
	soulHolderBefore, err := stack.GetSoulLeaseHolder(ctxLease, sid)
	cancelLease()
	if err != nil {
		t.Fatalf("GetSoulLeaseHolder(%s): %v", sid, err)
	}
	t.Logf("L3c-4 SID-lease holder before kill: %q (killing leader %q)",
		soulHolderBefore, leaderKID)

	// 4. Kill leader pod (grace-period=0 — симулируем crash, не graceful
	//    Release-lease через defer; failover должен полагаться на TTL).
	ctxKill, cancelKill := context.WithTimeout(context.Background(), 30*time.Second)
	if err := stack.DeleteKeeperPod(ctxKill, leaderPod); err != nil {
		cancelKill()
		t.Fatalf("DeleteKeeperPod(%s): %v", leaderPod, err)
	}
	cancelKill()

	// 5. Дожидаемся НОВОГО лидера (KID != убитый). Lease TTL=15s + acquire
	//    backoff 5s → typical 15-20s; ставим 90s запас на CI-нагрузку.
	newLeaderKID := waitForReaperLeader(t, stack, leaderKID, 90*time.Second)
	t.Logf("L3c-4 new Reaper leader: %s (was %s)", newLeaderKID, leaderKID)

	// 6. Deployment self-heals — ReplicaSet поднимает новый pod взамен
	//    удалённого, к Ready=3 возвращается в течение 60s (image-load уже
	//    был, остаётся только init-container + readiness).
	stack.WaitForDeploymentReady(t, "keeper", 120*time.Second)

	// 7. Soul-pod должен оставаться connected (либо реконнект прошёл через
	//    fallback ClusterIP к живому keeper-у). 120s — щедрый запас на
	//    timeout reconnect-flow в Soul.
	stack.WaitForSoulConnected(t, sid, 2*time.Minute)

	if soulHolderBefore == leaderKID {
		t.Logf("L3c-4 SID-lease was held by killed leader %s; soul reconnect verified by WaitForSoulConnected",
			leaderKID)
	}
}

// waitForReaperLeader поллит `reaper:leader` до появления KID, отличного от
// excludeKID (для пустой строки — любого KID). Возвращает наблюдаемый KID
// или фейлит t при timeout.
//
// Используется два раза в failover-тесте: (a) до kill — `excludeKID=""`,
// нам нужен любой лидер; (b) после kill — `excludeKID=<old>`, нам нужен
// именно НОВЫЙ.
func waitForReaperLeader(t *testing.T, stack *harness.Stack, excludeKID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		kid, err := stack.GetReaperLeaderKID(ctx)
		cancel()
		if err != nil {
			t.Logf("waitForReaperLeader: GET reaper:leader: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		last = kid
		if kid != "" && kid != excludeKID {
			return kid
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("waitForReaperLeader: no leader satisfying exclude=%q within %v (last=%q)",
		excludeKID, timeout, last)
	return ""
}
