//go:build e2e

// L3a-MK: standalone-orphan applying-reconcile на НАСТОЯЩЕМ краше keeper.
//
// Доказывает END-TO-END закрытие находки #1 (reconcile_orphan_applying, ADR-027
// amend (m)) на реальном SIGKILL процесса keeper-владельца ПРЯМОГО (standalone,
// НЕ через Voyage) incarnation.run — не unit-моке reconciler-а:
//
//  1. Кластер из 2 keeper поверх ОБЩИХ PG/Redis/Vault, acolytes>0 (apply пишется
//     Acolyte-путём claimed→dispatched→SendApply), reconcile_orphan_applying с
//     КОРОТКИМ stale_after (3s по умолчанию harness-а, НЕ prod-90s).
//  2. soul-stub в режиме hold-apply подключён к primary keeper-A (= владелец
//     standalone-прогона: HTTP incarnation.run приходит к нему, lockRun пишет
//     applying_by_kid=A; SendApply маршрутизируется в его EventStream).
//  3. incarnation.run(create) → дожидаемся apply_runs.status='dispatched' для SID
//     (стаб держит ApplyRequest, RunResult не шлёт). incarnation зависает
//     `applying` с epoch (applying_by_kid=A, applying_apply_id=run, applying_since).
//  4. ★ SIGKILL keeper-A (владелец applying-lock + run-barrier). Барьер умирает,
//     standalone-run НЕ имеет reclaim (reclaim_voyages только для Voyage) — без
//     находки #1 incarnation осталась бы `applying` НАВСЕГДА.
//  5. Дожидаемся истечения Conclave-presence A (~30s DefaultConclaveTTL) — тогда
//     InstanceAlive(applying_by_kid=A)=false.
//  6. ★ ASSERT: reconcile_orphan_applying на живом keeper-B снимает осиротевший
//     applying-lock (applying→ready), epoch-колонки очищены в NULL, audit-event
//     reaper.reconcile_orphan_applying.executed записан {incarnation, prev_kid=A,
//     apply_id}. incarnation перестаёт быть «навсегда applying».
//
// FENCING-1 (presence-мёртвый владелец, но live-rival apply_run с другим apply_id
// → правило НЕ снимает) ЗДЕСЬ не дублируется: он уже доказан integration-тестом
// orphan_applying_reconcile_integration_test.go (ReleaseApplyingOrphan FENCING-1
// внутри). Live-rival на crash-стенде потребовал бы второй конкурирующий прогон
// той же инкарнации — несоразмерно сложно для аддитивной ценности над уже
// существующим integration-покрытием. См. отчёт slice S3.
package e2e_test

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2E_MultiKeeper_StandaloneOrphanReconcileAfterCrash(t *testing.T) {
	const (
		keepers     = 2
		serviceName = "service-noop"
		examplePath = "examples/service/noop"
		incarnation = "so-orphan-inc"
		scenario    = "create"
		// staleAfter КОРОТКИЙ — правило должно сработать вскоре после истечения
		// presence убитого владельца, а не ждать by-design 90s prod-дефолта.
		staleAfter = 3 * time.Second
	)

	stack := harness.NewMultiKeeperStack(t, harness.MultiKeeperConfig{
		Keepers:                   keepers,
		Souls:                     1,
		VoyageLeaseTTL:            4 * time.Second,
		ReconcileOrphanStaleAfter: staleAfter,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, serviceName, examplePath)

	// Стаб в режиме hold-apply (RunResult не шлёт → строка зависает dispatched,
	// incarnation остаётся applying). reconnect здесь НЕ нужен: цель — реконсиляция
	// applying-lock Reaper-ом, а не reconnect-стрима стаба. Стаб подключён к primary
	// (keeper-A) — он и есть владелец standalone-прогона.
	soulStub := stack.ConnectSoulStub(t, 0)
	soulStub.SetHoldApply(true)
	sid := stack.SoulSID(0)
	ownerKID := stack.StreamHolderKID(t)
	t.Logf("SO-orphan: soul-stub %s держит стрим к %s (= владелец standalone-run-а)", sid, ownerKID)

	// Ready-инкарнация с единственным connected-хостом в её coven.
	stack.SeedIncarnationReady(t, incarnation, serviceName, "main", map[string]any{})
	stack.AddSoulToCoven(t, 0, incarnation)

	// ★ ПРЯМОЙ incarnation.run(create) — standalone-путь (НЕ Voyage; back-link
	// voyage_targets отсутствует). lockRun на keeper-A пишет applying + epoch
	// (applying_by_kid=A). Acolyte клеймит planned→dispatched→SendApply в стрим A.
	// Стаб держит ApplyRequest → строка зависает dispatched, incarnation — applying.
	applyID := stack.RunScenario(t, incarnation, scenario, nil)
	t.Logf("SO-orphan: standalone incarnation.run(%s) apply_id=%s", scenario, applyID)

	// (1) Дожидаемся dispatched — задание физически отдано стабу, RunResult удержан.
	got := stack.WaitApplyRunStatusForSID(t, applyID, sid, []string{"dispatched"}, 30*time.Second)
	t.Logf("SO-orphan: apply_runs(%s,%s).status=%q — задание отдано, RunResult удержан", applyID, sid, got)

	// (2) incarnation в applying с заполненным epoch владельца A — точка, из которой
	// краш владельца оставит осиротевший lock. Проверяем epoch ДО краша: правило
	// детектит orphan именно по applying_by_kid (presence-свидетель смерти).
	incStatus, _ := stack.IncarnationStatusDetails(t, incarnation)
	if incStatus != "applying" {
		t.Fatalf("SO-orphan: incarnation %s status=%q до краша, ожидался applying (lockRun не взял lock?)", incarnation, incStatus)
	}
	epoch := stack.IncarnationApplyingEpochSnapshot(t, incarnation)
	if epoch.ByKID == nil || *epoch.ByKID != ownerKID {
		gotKID := "<nil>"
		if epoch.ByKID != nil {
			gotKID = *epoch.ByKID
		}
		t.Fatalf("SO-orphan: applying_by_kid=%q до краша, ожидался %q (epoch не выставлен lockRun-ом)", gotKID, ownerKID)
	}
	if epoch.ApplyID == nil || *epoch.ApplyID == "" || !epoch.SinceSet {
		t.Fatalf("SO-orphan: неполный epoch до краша (apply_id=%v since_set=%v) — reconcile-предикат не сработает",
			epoch.ApplyID, epoch.SinceSet)
	}
	t.Logf("SO-orphan: incarnation %s applying с epoch{by_kid=%s, apply_id=%s, since_set=%v} — owner-lock заряжен",
		incarnation, *epoch.ByKID, *epoch.ApplyID, epoch.SinceSet)

	// (3) ★ НАСТОЯЩИЙ SIGKILL владельца standalone-прогона. Барьер run-goroutine на A
	// умирает; applying-lock остаётся в PG с applying_by_kid=A. reclaim_voyages его
	// НЕ трогает (нет voyage_targets back-link — это standalone). Без находки #1
	// incarnation висела бы applying навсегда.
	stack.KillKeeperByKID(t, ownerKID)
	t.Logf("SO-orphan: SIGKILL %s (владелец applying-lock) отправлен", ownerKID)

	live := stack.LiveKeeperKIDs()
	if len(live) == 0 {
		t.Fatalf("SO-orphan: после kill не осталось живых keeper-ов (нужен ≥1 для reconcile-Reaper-а)")
	}
	t.Logf("SO-orphan: живые keeper-ы (исполняют reconcile_orphan_applying): %v", live)

	// (4) ★ ASSERT END-TO-END: после истечения Conclave-presence убитого A
	// (~30s DefaultConclaveTTL — by-design, не конфигурируемо) reconcile_orphan_
	// applying на живом keeper-B детектит presence-мёртвого владельца и снимает
	// осиротевший lock applying→ready. Окно: presence-TTL (~30s) + stale_after (3s)
	// + reaper-interval (500ms) + запас. Ждём до 70s — presence-TTL обязателен,
	// stale_after КОРОТКИЙ (правило сработает почти сразу после presence-истечения).
	finalStatus := stack.WaitIncarnationStatus(t, incarnation, []string{"ready"}, 70*time.Second)
	t.Logf("SO-orphan: ★ incarnation %s.status=%q — осиротевший applying-lock снят reconcile_orphan_applying (END-TO-END)", incarnation, finalStatus)

	// (5) epoch-колонки очищены в NULL — ReleaseApplyingOrphan чистит их той же tx,
	// что status→ready (иначе протухший applying_by_kid дал бы повторный orphan-
	// детект на следующем тике).
	epochAfter := stack.IncarnationApplyingEpochSnapshot(t, incarnation)
	if !epochAfter.EpochCleared() {
		t.Fatalf("SO-orphan: epoch НЕ очищен после снятия orphan-lock: by_kid=%v apply_id=%v attempt=%v since_set=%v",
			epochAfter.ByKID, epochAfter.ApplyID, epochAfter.Attempt, epochAfter.SinceSet)
	}
	t.Logf("SO-orphan: epoch-колонки обнулены в NULL — lock снят чисто")

	// (6) audit-event reaper.reconcile_orphan_applying.executed записан с правильным
	// payload {incarnation, prev_kid=убитый владелец}. Доказывает, что снятие
	// прошло именно reconcile-правилом (а не побочным путём), и несёт security-trail.
	if !stack.WaitAuditEventByPayload(t, "reaper.reconcile_orphan_applying.executed",
		"incarnation", incarnation, 10*time.Second) {
		t.Fatalf("SO-orphan: audit reaper.reconcile_orphan_applying.executed для %s НЕ записан", incarnation)
	}
	prevKIDinAudit := stack.AuditPayloadField(t, "reaper.reconcile_orphan_applying.executed", "incarnation", incarnation, "prev_kid")
	if prevKIDinAudit != ownerKID {
		t.Fatalf("SO-orphan: audit prev_kid=%q, ожидался убитый владелец %q (reconcile приписан не тому KID)", prevKIDinAudit, ownerKID)
	}
	t.Logf("SO-orphan: ★ audit reaper.reconcile_orphan_applying.executed{incarnation=%s, prev_kid=%s} записан — находка #1 ДОКАЗАНА live",
		incarnation, prevKIDinAudit)

	// (7) Дурабельность: incarnation НЕ откатывается обратно в applying (single-
	// winner CAS applying→ready). Добираем с паузой, чтобы поймать гипотетический
	// повторный orphan-детект (был бы багом: epoch уже NULL → кандидатом не станет).
	time.Sleep(2 * time.Second)
	if st, _ := stack.IncarnationStatusDetails(t, incarnation); st != "ready" {
		t.Fatalf("SO-orphan: incarnation откатилась в %q после ready — нарушена дурабельность снятия orphan-lock", st)
	}
}
