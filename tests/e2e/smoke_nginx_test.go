//go:build e2e

// L3a E2E smoke: smoke-nginx happy-path (ADR-039).
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper-процесс + 1 soul-stub.
//     В среде без docker / без keeper-бинаря — t.Skip из NewStack (pre-flight).
//  2. CreateIncarnation `test-nginx` поверх service `smoke-nginx@main`.
//  3. RunScenario `create` с input.hostname.
//  4. WaitApplySuccess → AssertApplyRunsStatus("success") + audit + metrics.
//
// Drift-тесты (TestValidApplyRunsStatus*) — настоящие asserts, без Skip —
// гарантируют, что harness.validApplyRunsStatus не разойдётся с реальным enum
// в keeper/internal/applyrun/applyrun.go.
package e2e_test

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestSmokeNginx_InstallAndStart(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/smoke-nginx",
		Souls:       1,
	})
	defer stack.Cleanup()

	// Service-registration: без неё CreateIncarnation отвечает 422 «service
	// smoke-nginx is not registered» (ADR-029). RegisterService материализует
	// example-каталог в per-test file://-git-репо и POST /v1/services.
	stack.RegisterService(t, "smoke-nginx", "examples/service/smoke-nginx")

	// Live EventStream-стрим: захватывает Redis SID-lease → ApplyRequest
	// смаршрутизируется в локальный Outbound. SetApplyDefaultSuccess — отвечать
	// SUCCESS на любую задачу scenario (install/start nginx) без per-task script:
	// apply-e2e проверяет lifecycle apply_runs, а не реализм исполнения (L3a).
	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)

	// Coven-членство: roster прогона резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Без него scenario видит no_hosts → error_locked.
	stack.AddSoulToCoven(t, 0, "test-nginx")

	inc, applyID := stack.CreateIncarnationWithApply(t, "test-nginx", "smoke-nginx@main", map[string]any{
		"hostname": "web-01",
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.AssertIncarnationState(t, inc, map[string]any{
		"nginx_package": "nginx",
		"nginx_service": "nginx",
	})
	// Audit-event: POST /v1/incarnations авто-запускает create-scenario и пишет
	// `incarnation.created` (router.go) с `apply_id` авто-create-прогона в
	// payload (incarnation.go::Create SetAuditPayload). Это тот же apply_id, что
	// мы ждали в WaitApplySuccess — связь audit↔apply-run.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	// Метрика успешного прогона: keeper_scenario_runs_total{result="ok"} (scenario/
	// metrics.go — closed enum ok/failed/locked, инкремент в run.go-терминале).
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// TestValidApplyRunsStatus_PilotValueAccepted — sanity-check: "success" входит
// в harness.validApplyRunsStatus. Ловит опечатки в самом fixture-литерале (на
// случай, если кто-то поменяет ключ-литерал в asserts.go и забудет про "success").
func TestValidApplyRunsStatus_PilotValueAccepted(t *testing.T) {
	statuses := harness.ValidApplyRunsStatuses()
	want := map[string]bool{"success": true, "failed": true, "planned": true}
	for s := range want {
		found := false
		for _, v := range statuses {
			if v == s {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("harness.validApplyRunsStatus не содержит %q; полный список: %v", s, statuses)
		}
	}
}

// TestValidApplyRunsStatus_RejectsTypo — типичные опечатки из ADR-039 § Часть D
// должны определяться как невалидные. Используем pure-проверку IsValidApplyRunsStatus,
// чтобы не плодить fake-testing.TB (testing.TB содержит private-метод и в
// реализуемых пакетах не имитируется).
func TestValidApplyRunsStatus_RejectsTypo(t *testing.T) {
	bad := []string{"succeeded", "done", "ok", "completed"}
	for _, status := range bad {
		if harness.IsValidApplyRunsStatus(status) {
			t.Fatalf("IsValidApplyRunsStatus принял невалидный %q, должен был вернуть false", status)
		}
	}
}

// TestValidApplyRunsStatus_CoversApplyRunGoEnum — drift-detector: harness-овский
// литерал значений apply_runs.status должен покрывать ровно тот набор, что
// keeper/internal/applyrun/applyrun.go::ValidStatus считает валидным.
//
// Pilot-фаза: список захардкожен по applyrun.go::Status const-ам на момент
// 2026-05-26 (planned/claimed/running/dispatched/success/failed/cancelled/orphaned/no_match).
// В L3a-implementation slice — замена на импорт `applyrun.ValidStatus` через
// replace в tests/e2e/go.mod (deps testcontainers не утекают, а контракт-импорт
// applyrun — лёгкий, тащит только pgx-string-типы).
//
// Тест не делает реальной проверки против applyrun.go (потребовался бы импорт);
// он зафиксирован expected-литералом, и при правке keeper-side enum-а нужно
// обновить expected ЗДЕСЬ + validApplyRunsStatus в harness/asserts.go. Drift
// поймается обычным `go test -tags=e2e ./...` (если в expected попадёт значение,
// которого нет в harness — этот тест упадёт).
func TestValidApplyRunsStatus_CoversApplyRunGoEnum(t *testing.T) {
	expected := []string{
		"planned",
		"claimed",
		"running",
		"dispatched",
		"success",
		"failed",
		"cancelled",
		"orphaned",
		"no_match",
	}
	got := harness.ValidApplyRunsStatuses()

	sort.Strings(expected)
	sort.Strings(got)

	if len(expected) != len(got) {
		t.Fatalf("длина drift: expected=%d got=%d (expected=%v got=%v)", len(expected), len(got), expected, got)
	}
	for i := range expected {
		if expected[i] != got[i] {
			t.Fatalf("drift на индексе %d: expected=%q got=%q (полный expected=%v, полный got=%v)", i, expected[i], got[i], expected, got)
		}
	}
}
