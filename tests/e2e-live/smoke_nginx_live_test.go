//go:build e2e_live

// L3b E2E flagship-smoke: smoke-nginx-live happy-path (ADR-039).
//
// Параллель с tests/e2e/smoke_nginx_test.go (L3a, soul-stub отвечает scripted),
// но идущая через РЕАЛЬНЫЙ apt-install nginx внутри Debian-12-soul-container.
// Покрытие, которое L3a не даёт: Keeper render → ApplyRequest на wire → реальный
// soul Apply (core.pkg / core.file.rendered / core.service) → RunResult →
// apply_runs success.
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper-процесс + 1 privileged
//     debian-12 systemd-PID-1 soul-container. Real Bootstrap-flow закрыт
//     L3b-2-slice-ом; здесь полагаемся, что после NewStack souls.status =
//     'connected' уже выставлен.
//  2. CreateIncarnation `test-nginx-live` поверх service `smoke-nginx-live@main`.
//  3. RunScenario `create` с input.hostname=soul-live-a.example.com.
//  4. WaitApplySuccess (timeout 300 s — apt-update + install nginx могут быть
//     медленными на нагруженной CI-машине, см. README example-а).
//  5. AssertApplyRunsStatus / AssertIncarnationState / AssertAuditEvent /
//     AssertMetricGE — те же контракт-проверки, что в L3a.
//
// Container-side asserts — L3b-4: подтверждают, что после apply реально стоит
// nginx-пакет, активен systemd-unit и сгенерирован конфиг с server_name.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bSmokeNginxLive_InstallAndStart(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/smoke-nginx-live",
		ServiceName: "smoke-nginx-live",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("ожидался 1 soul-контейнер, получено %d", got)
	}
	const wantSID = "soul-live-a.example.com"
	if sc := stack.SoulContainers[0]; sc.SID != wantSID {
		t.Errorf("SoulContainers[0].SID = %q, ожидалось %q", sc.SID, wantSID)
	}

	const incName = "test-nginx-live"

	// Coven-членство ДО Create: roster прогона резолвится по `incarnation.name ∈
	// coven[]` (ADR-008, topology/resolver.go::rosterSQL). Bootstrap-flow выставил
	// souls.status='connected', но coven остался пустым — без этого шага scenario
	// видит no_hosts → ноль строк apply_runs → WaitApplySuccess timeout. Ставим
	// метку ДО CreateIncarnationWithApply, т.к. POST /v1/incarnations авто-
	// запускает create-прогон сразу; coven должен существовать к его roster-
	// резолву. Параллель с L3a (tests/e2e/smoke_nginx_test.go::AddSoulToCoven).
	stack.AddSoulToCoven(t, 0, incName)

	// POST /v1/incarnations авто-запускает scenario `create` и возвращает его
	// apply_id. Отдельный RunScenario(create) был бы отвергнут lock-gate-ом
	// («incarnation уже в статусе applying») и его apply_id никогда не получил бы
	// apply_runs-строк. Ждём apply_id именно авто-create-прогона (как L3a).
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "smoke-nginx-live@main", map[string]any{
		"hostname": wantSID,
	})

	// 300 c — apt-get update + apt-get install nginx + systemctl start
	// в свежем Debian-12-контейнере на нагруженной CI-машине. README
	// фиксирует ожидаемое время прогона (~3–5 минут).
	stack.WaitApplySuccess(t, applyID, 300)

	// apply_runs success ≠ incarnation.state закоммичен: state_changes пишутся
	// отдельной транзакцией ПОСЛЕ барьера (run.go §8, status→ready). Без этого
	// ожидания AssertIncarnationState читает пустой state в окне гонки.
	stack.WaitIncarnationReady(t, inc, 30)

	// YAML loader (L3b-5): apply_runs / incarnation_state / audit_events /
	// metrics / host_state — один источник правды (smoke-nginx-live/expectations
	// /after-create.yaml). Симметрично L3a-fixture-формату (см. docs/testing/e2e.md).
	exp := harness.LoadExpectations(t, "smoke-nginx-live/expectations/after-create.yaml")
	stack.AssertExpectations(t, exp, applyID, inc)

	// apply_id в payload audit-event-а — runtime-значение, не выражается через
	// YAML-fixture; проверяется отдельно после AssertExpectations. POST
	// /v1/incarnations авто-запускает create-scenario и пишет `incarnation.created`
	// (huma_incarnation_op.go) с apply_id авто-прогона в payload — тот же apply_id,
	// что и в WaitApplySuccess. `incarnation.scenario_started` пишется только при
	// явном RunScenario, который здесь не вызывается (как L3a smoke_nginx_test.go).
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
}
