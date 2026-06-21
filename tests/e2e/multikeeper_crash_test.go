//go:build e2e

// L3a-MK: multi-keeper live-crash recovery (GA-доказательство reclaim_voyages).
//
// Сценарий доказывает END-TO-END recovery на НАСТОЯЩЕМ краше процесса keeper
// (SIGKILL, не SQL-эмуляция TestIntegration_Runner_ReclaimApplyRuns_*):
//
//  1. Кластер из 3 keeper поверх ОБЩИХ PG/Redis/Vault:
//     - keeper-mk-00 — soul-holder (voyage.workers=0, держит soul-стримы);
//     - keeper-mk-01/02 — VoyageWorker-ы (voyage.workers=2), претендуют на Voyage.
//  2. Scenario-Voyage поверх N ready-инкарнаций с batch_size=1 (serial-волны —
//     прогон растянут, окно для kill широкое).
//  3. Дожидаемся voyages.status=running + claimed_by_kid=<owner> (всегда mk-01/02,
//     т.к. soul-holder не претендует).
//  4. SIGKILL ИМЕННО процесса <owner> (живой флот на soul-holder-е не задет).
//  5. ASSERT recovery:
//     (a) reclaim_voyages вернул Voyage в pending → re-claim другим живым KID
//     (attempt вырос, claimed_by_kid != killed);
//     (b) Voyage дошёл до терминала succeeded на живом keeper-е;
//     (c) все voyage_targets succeeded — прогон РЕАЛЬНО доисполнился по каждой
//     инкарнации (не «формально succeeded на пустом scope»);
//     (d) incarnation.state каждой инкарнации консистентен (status=ready, не
//     завис в applying).
package e2e_test

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2E_MultiKeeper_VoyageReclaimAfterCrash(t *testing.T) {
	const (
		keepers     = 3
		incarnCount = 6
		serviceName = "service-noop"
		examplePath = "examples/service/noop"
	)

	stack := harness.NewMultiKeeperStack(t, harness.MultiKeeperConfig{
		Keepers:        keepers,
		Souls:          1,
		VoyageLeaseTTL: 4 * time.Second,
	})
	defer stack.Cleanup()

	// Service registry + один connected soul на soul-holder-е (primary).
	stack.RegisterService(t, serviceName, examplePath)
	soulStub := stack.ConnectSoulStub(t, 0)
	// noop create несёт host-задачу core.exec.run (echo hello) — без default-
	// success режима unscripted-задача дала бы FAILED. Включаем «success на любой
	// ApplyRequest»: проверяем recovery-шов (orphan-lock + re-run), а не fixture
	// per-task-скрипта. Стаб держит стрим до Cleanup.
	soulStub.SetApplyDefaultSuccess(true)

	// Seed N ready-инкарнаций; единственный soul — в coven КАЖДОЙ инкарнации
	// (roster резолвится по incarnation.name ∈ souls.coven[], ADR-008), чтобы
	// per-incarnation scenario-run Voyage-а имел connected-хост.
	incNames := make([]string, incarnCount)
	for i := 0; i < incarnCount; i++ {
		name := incName(i)
		incNames[i] = name
		stack.SeedIncarnationReady(t, name, serviceName, "main", map[string]any{})
		stack.AddSoulToCoven(t, 0, name)
	}

	// Scenario-Voyage поверх всех инкарнаций, batch_size=1 → serial-волны.
	voyageID := stack.CreateScenarioVoyage(t, "create", incNames, 1)

	// Ждём running-владельца. Owner ∈ {keeper-mk-01, keeper-mk-02} (soul-holder
	// mk-00 voyage-pool не поднимает).
	owner := stack.WaitVoyageRunningOwner(t, voyageID, 20*time.Second)
	if owner == "keeper-mk-00" {
		t.Fatalf("неожиданный владелец Voyage %q — soul-holder не должен претендовать на Voyage", owner)
	}
	beforeKill := stack.VoyageState(t, voyageID)
	t.Logf("MK-crash: Voyage %s захвачен %s (attempt=%d, batch=%d/%d) — убиваю процесс",
		voyageID, owner, beforeKill.Attempt, beforeKill.BatchIndex, beforeKill.TotalBatch)

	// ★ НАСТОЯЩИЙ kill процесса keeper-владельца (не SQL-эмуляция).
	stack.KillKeeperByKID(t, owner)

	// (a) reclaim_voyages → re-claim другим живым KID (attempt вырос).
	reclaimedBy := stack.WaitVoyageReclaimed(t, voyageID, owner, beforeKill.Attempt, 30*time.Second)
	t.Logf("MK-crash: Voyage %s перезахвачен %s (был %s) — reclaim сработал", voyageID, reclaimedBy, owner)
	if reclaimedBy == owner {
		t.Fatalf("re-claim вернулся тому же (убитому) KID %q — reclaim не сменил владельца", owner)
	}

	// ★ SEAM-ДЕТЕКТ (закрыт фиксом ADR-027(k)). После reclaim даём живому keeper-у
	// время доисполнить прогон. Если краш владельца оставил per-incarnation
	// scenario-run осиротевшим в `applying`, реклеймнутый VoyageWorker ПЕРЕД
	// повторным спавном детектит МОЙ orphan applying-lock (back-link apply_id из
	// voyage_targets ЭТОГО Voyage от прошлого attempt) и снимает его FENCED
	// (apply_id-match + VerifyOwnership + single-winner CAS applying→ready,
	// voyageorch.reconcileOrphanLock → incarnation.ReleaseApplyingOrphan). После
	// снятия lockRun re-run проходит, voyage_targets доезжает.
	//
	// До фикса reclaim_voyages возвращал Voyage в pending и менял владельца, но НЕ
	// реконсилил dangling `applying` → re-run отвергался («incarnation уже
	// applying»), voyage_targets застревал на batch 0 НАВСЕГДА. Этот ASSERT-блок
	// доказывает, что orphan-lock теперь снимается (инкарнация НЕ остаётся в
	// applying), а Voyage доходит до succeeded.
	time.Sleep(12 * time.Second)
	stack.DumpRecoveryState(t, voyageID)

	// Вариант A (регресс-страж): инкарнация НЕ должна остаться осиротевшей в
	// `applying` — фикс ADR-027(k) снимает lock перед re-run. Если осталась —
	// recovery-шов снова сломан.
	if orphaned := stack.IncarnationsInStatus(t, incNames, "applying"); len(orphaned) > 0 {
		applyRuns := stack.CountApplyRunsForIncarnation(t, orphaned[0])
		t.Fatalf("РЕГРЕСС recovery-шва ADR-027(k) (applying-orphan): после краша владельца %s "+
			"и успешного reclaim_voyages (перезахвачен %s) инкарнация(и) %v ОСТАЛИСЬ в "+
			"`applying` (apply_runs для %s = %d). Реклеймнутый VoyageWorker должен был снять "+
			"осиротевший applying-lock (reconcileOrphanLock → ReleaseApplyingOrphan) перед "+
			"re-run, но не снял — orphan-детект/fencing сломан.",
			owner, reclaimedBy, orphaned, orphaned[0], applyRuns)
	}

	// Вариант B (страж чистоты re-run): после снятия orphan-lock re-dispatch
	// должен доисполниться в ready. error_locked здесь = re-run упал по другой
	// причине (не orphan-шов) — отдельный дефект, не маскируем под succeeded.
	if locked := stack.IncarnationsInStatus(t, incNames, "error_locked"); len(locked) > 0 {
		_, details := stack.IncarnationStatusDetails(t, locked[0])
		t.Fatalf("recovery re-run упал в error_locked (вариант B): после краша владельца %s "+
			"и успешного reclaim_voyages (перезахвачен %s, orphan-lock снят, re-dispatch состоялся) "+
			"инкарнация(и) %v упали в `error_locked` (status_details %s = %s) вместо succeeded — "+
			"per-incarnation re-run не доисполнился чисто (дефект вне orphan-шва).",
			owner, reclaimedBy, locked, locked[0], details)
	}

	// (b) Voyage дошёл до терминала succeeded на живом keeper-е (достижимо ТОЛЬКО
	// если seam-дефекта нет — например, краш пришёлся МЕЖДУ per-incarnation
	// runs, не внутри них).
	final := stack.WaitVoyageSucceeded(t, voyageID, 60*time.Second)
	t.Logf("MK-crash: Voyage %s достиг succeeded (attempt=%d, finished=%v)",
		voyageID, final.Attempt, final.Finished)

	// (c) Все voyage_targets succeeded — прогон РЕАЛЬНО доисполнился.
	got := stack.AssertVoyageTargetsTerminal(t, voyageID)
	if got != incarnCount {
		t.Fatalf("voyage_targets succeeded=%d, ожидалось %d (прогон доисполнился не по всем инкарнациям)", got, incarnCount)
	}

	// (d) incarnation.state консистентен: каждая инкарнация ready (не зависла
	// в applying после краша владельца Voyage).
	for _, name := range incNames {
		stack.WaitIncarnationReady(t, name, 30)
	}

	// Бонус-доказательство: per-row audit `voyage.reclaimed` эмитнут на reclaim.
	if n := stack.CountAuditEvents(t, "voyage.reclaimed", voyageID); n < 1 {
		t.Errorf("audit `voyage.reclaimed` для %s = %d, ожидалось ≥1 (reclaim должен эмитить per-row событие)", voyageID, n)
	}
}

// incName — детерминированное имя i-й инкарнации.
func incName(i int) string {
	return "mk-inc-" + string(rune('a'+i))
}
