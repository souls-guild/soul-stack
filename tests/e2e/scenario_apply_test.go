//go:build e2e

// L3a E2E: scenario-apply execution-путь (ADR-039) — фундамент per-section
// e2e-покрытия (Tide / push / drift / …). Доказывает, что apply-цепочка
// RegisterService → CreateIncarnation → ConnectSoulStub → apply_runs success →
// incarnation `ready` + state-commit работает end-to-end на реальном стеке
// (PG+Redis+Vault testcontainers + keeper-процесс + connected soul-stub).
//
// Почему он ловит регрессии (зеркало errand_run_test.go для apply-пути):
//   - service-registration отсутствует → CreateIncarnation 422 «not registered»;
//   - acolyte pool disabled → apply_runs навсегда planned → WaitApplySuccess
//     timeout;
//   - dispatch не доходит до Soul-а (нет live-стрима / lease) → orphaned;
//   - state-commit сломан (render state_changes.sets / commitSuccess) → state
//     не совпадает с scenario `state_changes.sets`.
//
// Ограничение (документированное): soul-stub НЕ исполняет реальные модули —
// SetApplyDefaultSuccess делает его отвечающим RunResult{SUCCESS} на любую
// задачу ApplyRequest (L3a-контракт: проверяем keeper-side lifecycle apply_runs +
// state-commit, а не реализм исполнения core-модуля; реальный exec — L3b).
package e2e_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

// TestScenarioApply_NoopCreate_Succeeds — минимальный self-contained
// scenario-apply: noop-service (один core.exec.run-шаг, без required-input).
// CreateIncarnation авто-запускает scenario `create` и возвращает apply_id;
// ждём success + incarnation `ready`.
func TestScenarioApply_NoopCreate_Succeeds(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	// Reusable helper №1: материализация example → file://-git-репо + POST
	// /v1/services. Без неё CreateIncarnation отвечает 422.
	stack.RegisterService(t, "noop", "examples/service/noop")

	// Reusable helper №2: live EventStream-стрим (Redis SID-lease → dispatch
	// маршрутизируется в локальный Outbound). SetApplyDefaultSuccess — SUCCESS
	// на любую задачу без per-task script.
	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)

	// Coven-членство: roster прогона резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Без него scenario видит no_hosts → error_locked.
	stack.AddSoulToCoven(t, 0, "test-noop")

	// CreateIncarnation авто-запускает scenario `create` (incarnation.go) и
	// возвращает apply_id этого прогона. noop-create без required-input.
	// Используем apply_id авто-create — отдельный RunScenario(create) был бы
	// отвергнут («incarnation уже applying»).
	_, applyID := stack.CreateIncarnationWithApply(t, "test-noop", "noop@main", nil)

	// Reusable helper №3: блокирующее ожидание apply_runs.status=success у всех
	// строк прогона (planned→claimed→dispatched→success).
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
}

// TestScenarioApply_SmokeNginx_StateCommit — apply-цепочка с непустым
// state-commit: smoke-nginx-create декларирует state_changes.sets {nginx_package,
// nginx_service}, рендерится keeper-side и коммитится в incarnation.state.
// Доказывает, что state-commit-ветка (RenderStateChanges → mergeStateChanges →
// commitSuccess) работает в реальном прогоне, не только в unit-тестах.
func TestScenarioApply_SmokeNginx_StateCommit(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/smoke-nginx",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "smoke-nginx", "examples/service/smoke-nginx")

	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)
	stack.AddSoulToCoven(t, 0, "test-nginx-state")

	inc, applyID := stack.CreateIncarnationWithApply(t, "test-nginx-state", "smoke-nginx@main", map[string]any{
		"hostname": "web-01",
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.AssertIncarnationState(t, inc, map[string]any{
		"nginx_package": "nginx",
		"nginx_service": "nginx",
	})
}

// TestIncarnationCreate_MissingRequiredInput_422 — regression-guard синхронной
// валидации required-input (фикс 6ce69ce: дыра, где create без обязательного
// input создавал incarnation и падал уже в async-apply). smoke-nginx-create
// объявляет input.hostname required → CreateIncarnation БЕЗ input обязан
// ответить 422 ДО старта прогона (sync-валидация), а не 202.
func TestIncarnationCreate_MissingRequiredInput_422(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/smoke-nginx",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "smoke-nginx", "examples/service/smoke-nginx")

	// Без input.hostname (required) — sync-валидация обязана вернуть 422.
	body, status := stack.CreateIncarnationRaw(t, "test-nginx-missing", "smoke-nginx@main", nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("CreateIncarnation без required-input: status=%d, ожидался 422 (sync-валидация, фикс 6ce69ce); body=%s",
			status, string(body))
	}
	// Sanity: problem-detail упоминает валидацию (а не «not registered» —
	// тот был бы 422 другой природы, ловим подмену причины).
	if !strings.Contains(string(body), "validation") && !strings.Contains(string(body), "hostname") &&
		!strings.Contains(string(body), "required") && !strings.Contains(string(body), "input") {
		t.Fatalf("CreateIncarnation 422 body не похоже на input-валидацию: %s", string(body))
	}
}
