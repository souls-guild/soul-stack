//go:build e2e

// L3a-MK: presence-gated force-release SID-lease на НАСТОЯЩЕМ краше keeper.
//
// Доказывает END-TO-END находку #2 (eventstream.lease_force_released, ADR-027
// amend (n)) на реальном SIGKILL процесса keeper-холдера стрима — пере-
// доказательство находки #3 WardRoster-теста, теперь БЕЗ 60s-ожидания SID-lease
// TTL и без reconnect-спама:
//
//  1. Кластер из 2 keeper поверх ОБЩИХ PG/Redis/Vault, acolytes>0.
//  2. soul-stub (SID) в режиме hold-apply+reconnect подключён к primary keeper-A
//     (= holder стрима, держит Redis SID-lease soul:<sid>:lock со значением A).
//  3. incarnation.run(create) → dispatched (стаб держит ApplyRequest). Стаб
//     «рестартует» (ClearActiveWard) — после reconnect объявит пустой WardRoster.
//  4. ★ SIGKILL keeper-A. SID-lease в Redis ОСТАЁТСЯ принадлежать A (Release
//     только graceful; TTL 60s, defaultSoulLeaseTTL).
//  5. ★ Стаб reconnect-ит тот же SID к живому keeper-B (fallback-endpoints).
//  6. Дожидаемся истечения Conclave-presence A (~30s DefaultConclaveTTL) — тогда
//     InstanceAlive(A)=false.
//  7. ★ ASSERT: keeper-B делает presence-gated force-release (ForceAcquireSoulLease
//     CAS-by-prev-holder) ВМЕСТО отказа AlreadyExists → стрим открывается (стаб
//     получает ВТОРОЙ HelloReply) → handleWardRoster достигнут → dispatched-orphan
//     реконсилён в `orphaned`. audit eventstream.lease_force_released записан
//     {sid, prev_kid=A, new_kid=B}. Окно невидимости < 60s (по факту ≤ Conclave-
//     TTL ~30s, НЕ полные 60s TTL SID-lease — это и есть устранение находки #2).
package e2e_test

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
	"github.com/souls-guild/soul-stack/tests/e2e/internal/soulstub"
)

// countHelloReplies возвращает число HelloReply-кадров, принятых стабом за всё
// время жизни стрима(ов). Первый — на initial-Open к holder-у; второй+ — на
// успешном reconnect к живому keeper-у после force-release (stream открылся).
func countHelloReplies(s *soulstub.Stub) int {
	n := 0
	for _, m := range s.Messages() {
		if m.Kind == "HelloReply" {
			n++
		}
	}
	return n
}

func TestE2E_MultiKeeper_PresenceGatedLeaseForceReleaseAfterCrash(t *testing.T) {
	const (
		keepers     = 2
		serviceName = "service-noop"
		examplePath = "examples/service/noop"
		incarnation = "lfr-orphan-inc"
		scenario    = "create"
	)

	stack := harness.NewMultiKeeperStack(t, harness.MultiKeeperConfig{
		Keepers:        keepers,
		Souls:          1,
		VoyageLeaseTTL: 4 * time.Second,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, serviceName, examplePath)

	// Стаб в режиме hold-apply + reconnect+WardRoster. Подключён к primary
	// (keeper-A) — holder стрима (держит SID-lease).
	soulStub := stack.ConnectSoulStubReconnect(t, 0, true)
	sid := stack.SoulSID(0)
	holderKID := stack.StreamHolderKID(t)
	if helloN := countHelloReplies(soulStub); helloN != 1 {
		t.Fatalf("LFR: после initial-Open ожидался ровно 1 HelloReply, получено %d", helloN)
	}
	t.Logf("LFR: soul-stub %s держит стрим к %s (holder/lease-owner), endpoints=%v", sid, holderKID, stack.AllKeeperGRPCAddrs())

	// Ready-инкарнация с единственным connected-хостом в её coven.
	stack.SeedIncarnationReady(t, incarnation, serviceName, "main", map[string]any{})
	stack.AddSoulToCoven(t, 0, incarnation)

	// incarnation.run(create): Acolyte клеймит planned→dispatched→SendApply в стрим
	// holder-а. Стаб держит ApplyRequest → строка зависает dispatched.
	applyID := stack.RunScenario(t, incarnation, scenario, nil)
	t.Logf("LFR: incarnation.run(%s) apply_id=%s", scenario, applyID)

	got := stack.WaitApplyRunStatusForSID(t, applyID, sid, []string{"dispatched"}, 30*time.Second)
	t.Logf("LFR: apply_runs(%s,%s).status=%q — задание отдано holder-у, RunResult удержан", applyID, sid, got)

	// «Рестарт» Soul-процесса: in-flight физически нет → WardRoster на reconnect
	// объявит пустой набор, keeper-B осиротит dispatched-строку.
	soulStub.ClearActiveWard()

	// ★ Засекаем момент краша holder-а: окно невидимости меряется от него до
	// успешного force-release reconnect-а (доказательство «< 60s, ≤ ~Conclave-TTL»).
	killAt := time.Now()

	// ★ НАСТОЯЩИЙ SIGKILL keeper-холдера стрима. SID-lease soul:<sid>:lock в Redis
	// остаётся принадлежать holder-у (Release только graceful) до TTL ~60s.
	stack.KillKeeperByKID(t, holderKID)
	t.Logf("LFR: SIGKILL %s (holder/lease-owner) отправлен в t0 — стаб должен reconnect к живому keeper-у", holderKID)

	live := stack.LiveKeeperKIDs()
	if len(live) != 1 {
		t.Fatalf("LFR: после kill ожидался ровно 1 живой keeper, осталось %d (%v)", len(live), live)
	}
	newKeeperKID := live[0]
	t.Logf("LFR: живой keeper для force-release reconnect: %s", newKeeperKID)

	// ★ ASSERT-1: audit eventstream.lease_force_released записан с new_kid=живой
	// keeper. Это происходит ТОЛЬКО после истечения Conclave-presence убитого
	// holder-а (~30s) — пока он жив в Conclave, force-release fail-safe-отказывается
	// (AlreadyExists). Ждём до 55s: < 60s TTL SID-lease — доказательство, что
	// reconnect НЕ ждал полного протухания lease (находка #2 устранена). Если бы
	// force-release НЕ работал, событие не появилось бы вовсе, а reconnect встал бы
	// до 60s-истечения lease (поведение исходной находки #3).
	gotNewKID := stack.WaitLeaseForceReleased(t, sid, []string{newKeeperKID}, 55*time.Second)
	elapsed := time.Since(killAt)
	t.Logf("LFR: ★ audit eventstream.lease_force_released{sid=%s, new_kid=%s} за %s после краша holder-а", sid, gotNewKID, elapsed.Round(time.Second))

	// Окно невидимости < 60s (TTL SID-lease) — устранение находки #2. Conclave-TTL
	// (~30s) — by-design нижняя граница (presence обязан истечь); запас на reaper-
	// тик/reconnect-backoff/sweep. Жёсткая верхняя граница 60s = TTL lease: если
	// reconnect занял бы ≥60s, это поведение исходной находки (ждали полного TTL).
	if elapsed >= 60*time.Second {
		t.Fatalf("LFR: окно невидимости %s ≥ 60s (TTL SID-lease) — force-release НЕ устранил 60s-ожидание (находка #2 НЕ закрыта)", elapsed)
	}

	// ASSERT-2: prev_kid в audit = убитый holder (перехват приписан мёртвому
	// владельцу, не случайному KID).
	prevKID := stack.AuditPayloadField(t, "eventstream.lease_force_released", "sid", sid, "prev_kid")
	if prevKID != holderKID {
		t.Fatalf("LFR: audit prev_kid=%q, ожидался убитый holder %q (force-release приписан не тому владельцу)", prevKID, holderKID)
	}

	// ASSERT-3: стрим РЕАЛЬНО открылся на живом keeper-е — стаб получил ВТОРОЙ
	// HelloReply (handshake reconnect-а завершён, handleWardRoster достижим).
	stack.Eventually(t, 10*time.Second, func() bool {
		return countHelloReplies(soulStub) >= 2
	}, "стаб не получил второй HelloReply после force-release reconnect-а")
	t.Logf("LFR: ★ стаб получил второй HelloReply — стрим открыт на %s через force-release (handleWardRoster достигнут)", newKeeperKID)

	// ASSERT-4: handleWardRoster реконсилил dispatched-orphan в `orphaned`. Это —
	// конечное доказательство сквозного пути крах→force-release→reconnect→roster→
	// terminal, теперь БЕЗ 60s-ожидания. Окно короткое (стрим уже открыт): WardRoster
	// шлётся СРАЗУ после Hello, keeper терминалит dispatched немедленно.
	final := stack.WaitApplyRunStatusForSID(t, applyID, sid, []string{"orphaned"}, 15*time.Second)
	t.Logf("LFR: ★ apply_runs(%s,%s).status=%q — dispatched-orphan реконсилён WardRoster-ом после force-release (END-TO-END)", applyID, sid, final)

	// Дурабельность терминала (single-winner append-only, ADR-027(j)).
	time.Sleep(2 * time.Second)
	if cur := stack.ApplyRunStatusForSID(t, applyID, sid); cur != "orphaned" {
		t.Fatalf("LFR: статус строки откатился в %q после orphaned — нарушен single-winner append-only терминал", cur)
	}
}
